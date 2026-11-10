package transfer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/store"
)

// Receiver selection and failover, end to end: a real store, a real chooser,
// and two receivers that can be broken independently.
//
// These deliberately do NOT call log.Init: on Windows the open log file makes
// t.TempDir cleanup fail, which is why several older tests in this package
// report FAIL with nothing but a cleanup message.

// --- a receiver that records what it was given ------------------------------

type fakeReceiver struct {
	*httptest.Server
	mu     sync.Mutex
	got    map[string]int // SOP-ish body -> times seen
	broken atomic.Bool
}

func newFakeReceiver(t *testing.T) *fakeReceiver {
	t.Helper()
	f := &fakeReceiver{got: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.broken.Load() {
			http.Error(w, "receiver down", http.StatusBadGateway)
			return
		}
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		f.mu.Lock()
		f.got[string(body[:n])]++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ID":"x"}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeReceiver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// --- a scriptable PeerChooser ----------------------------------------------

type scriptedChooser struct {
	mu       sync.Mutex
	pick     []string // successive answers; last one repeats
	n        int
	manual   bool
	cooling  map[string]bool
	failed   []string
	cands    []string
}

func (c *scriptedChooser) ChoosePeer(context.Context) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pick) == 0 {
		return "", false
	}
	i := c.n
	if i >= len(c.pick) {
		i = len(c.pick) - 1
	}
	c.n++
	u := c.pick[i]
	if u == "" {
		return c.cands[0], false // nothing reachable
	}
	return u, true
}

func (c *scriptedChooser) MarkFailed(url string, _ error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cooling == nil {
		c.cooling = map[string]bool{}
	}
	c.cooling[url] = true
	c.failed = append(c.failed, url)
}
func (c *scriptedChooser) IsCoolingDown(url string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cooling[url]
}
func (c *scriptedChooser) IsCandidate(url string) bool {
	for _, u := range c.cands {
		if u == url {
			return true
		}
	}
	return false
}
func (c *scriptedChooser) IsManual() bool { return c.manual }
func (c *scriptedChooser) failures() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.failed...)
}

// --- helpers ----------------------------------------------------------------

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

// seedSeries stages n instance files for one series of a study.
func seedSeries(t *testing.T, st *store.Store, dir, studyUID, seriesUID string, n int) {
	t.Helper()
	seriesDir := filepath.Join(dir, "instances", studyUID, seriesUID)
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < n; i++ {
		sopUID := fmt.Sprintf("%s.inst.%d", seriesUID, i+1)
		path := filepath.Join(seriesDir, sopUID+".dcm")
		body := []byte("DICOM-" + sopUID)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		rec := store.InstanceRecord{
			SOPInstanceUID:    sopUID,
			SeriesInstanceUID: seriesUID,
			StudyInstanceUID:  studyUID,
			FilePath:          path,
			FileSize:          int64(len(body)),
			ReceivedAt:        time.Now(),
		}
		if err := st.PutInstance(rec, store.SeriesMetaUpdate{Modality: "CT"}, store.StudyMetaUpdate{PatientName: "T^U"}); err != nil {
			t.Fatalf("put instance: %v", err)
		}
	}
}

func newClient(peers PeerChooser, fallback string) *Client {
	return &Client{
		PeerURL:  fallback,
		Peers:    peers,
		Protocol: ProtocolInstances,
		Timeout:  10 * time.Second,
	}
}

// --- the tests --------------------------------------------------------------

// A study stays on the receiver it was assigned, even once a more-preferred
// receiver becomes available again. Splitting it would leave it incomplete on
// both, because the receivers do not share a destination.
func TestStudyStaysOnItsReceiver(t *testing.T) {
	st, dir := newStore(t)
	r1, r2 := newFakeReceiver(t), newFakeReceiver(t)

	const study = "1.2.study.stay"
	seedSeries(t, st, dir, study, "1.2.series.a", 2)
	seedSeries(t, st, dir, study, "1.2.series.b", 2)

	ch := &scriptedChooser{cands: []string{r1.URL, r2.URL}, pick: []string{r2.URL, r1.URL}}
	c := newClient(ch, r1.URL)

	if err := c.PushSeries(context.Background(), st, "1.2.series.a"); err != nil {
		t.Fatalf("push a: %v", err)
	}
	// Second series: the chooser now prefers r1, but the study is on r2.
	if err := c.PushSeries(context.Background(), st, "1.2.series.b"); err != nil {
		t.Fatalf("push b: %v", err)
	}

	if r1.count() != 0 {
		t.Fatalf("router1 received %d instances; the study was assigned to router2", r1.count())
	}
	if r2.count() != 4 {
		t.Fatalf("router2 got %d instances, want 4 (the whole study)", r2.count())
	}
	sr, _ := st.GetStudy(study)
	if sr.AssignedPeerURL != r2.URL {
		t.Fatalf("assignment = %q, want %q", sr.AssignedPeerURL, r2.URL)
	}
}

// A manual pin sends everything to the pinned receiver and never cools it off.
func TestManualPinSendsEverythingToThePinnedReceiver(t *testing.T) {
	st, dir := newStore(t)
	r1, r2 := newFakeReceiver(t), newFakeReceiver(t)

	const study = "1.2.study.pin"
	seedSeries(t, st, dir, study, "1.2.series.p", 2)

	ch := &scriptedChooser{cands: []string{r2.URL}, pick: []string{r2.URL}, manual: true}
	c := newClient(ch, r2.URL)

	if err := c.PushSeries(context.Background(), st, "1.2.series.p"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if r1.count() != 0 || r2.count() != 2 {
		t.Fatalf("r1=%d r2=%d, want 0 and 2", r1.count(), r2.count())
	}
}

// A failing receiver is passed over, the study moves, and the WHOLE study is
// re-sent — including the series already delivered to the old receiver.
func TestStudyFailsOverAndIsResentInFull(t *testing.T) {
	st, dir := newStore(t)
	r1, r2 := newFakeReceiver(t), newFakeReceiver(t)

	const study = "1.2.study.move"
	seedSeries(t, st, dir, study, "1.2.series.1", 2)
	seedSeries(t, st, dir, study, "1.2.series.2", 2)

	ch := &scriptedChooser{cands: []string{r1.URL, r2.URL}, pick: []string{r1.URL, r2.URL}}
	c := newClient(ch, r1.URL)

	// Series 1 lands on router 1.
	if err := c.PushSeries(context.Background(), st, "1.2.series.1"); err != nil {
		t.Fatalf("push 1: %v", err)
	}
	if r1.count() != 2 {
		t.Fatalf("router1 got %d, want 2", r1.count())
	}

	// Router 1 breaks. Series 2 fails and marks it failed.
	r1.broken.Store(true)
	if err := c.PushSeries(context.Background(), st, "1.2.series.2"); err == nil {
		t.Fatal("expected series 2 to fail against the broken receiver")
	}
	if got := ch.failures(); len(got) != 1 || got[0] != r1.URL {
		t.Fatalf("MarkFailed = %v, want [%s]", got, r1.URL)
	}

	// The queue retries. The study moves to router 2 and is re-sent in full.
	if err := c.PushSeries(context.Background(), st, "1.2.series.2"); err != nil {
		t.Fatalf("retry 2: %v", err)
	}
	sr, _ := st.GetStudy(study)
	if sr.AssignedPeerURL != r2.URL {
		t.Fatalf("assignment = %q, want %q", sr.AssignedPeerURL, r2.URL)
	}

	// Series 1 must have been re-queued by the move; push it and confirm the
	// new receiver ends up with the whole study.
	if err := c.PushSeries(context.Background(), st, "1.2.series.1"); err != nil {
		t.Fatalf("re-push 1: %v", err)
	}
	if r2.count() != 4 {
		t.Fatalf("router2 got %d instances, want all 4 of the study", r2.count())
	}
}

// §10.2/1 — two series of one UNASSIGNED study pushed concurrently, with a
// chooser that hands the two callers different receivers. Exactly the race the
// compare-and-swap exists for: without it the study is split across both,
// neither caller sees a "move", and it is later marked delivered while
// incomplete on each.
func TestConcurrentSeriesOfOneStudyAgreeOnOneReceiver(t *testing.T) {
	st, dir := newStore(t)
	r1, r2 := newFakeReceiver(t), newFakeReceiver(t)

	const study = "1.2.study.race"
	seedSeries(t, st, dir, study, "1.2.series.x", 2)
	seedSeries(t, st, dir, study, "1.2.series.y", 2)

	// Disagreeing answers: first caller gets r1, second gets r2.
	ch := &scriptedChooser{cands: []string{r1.URL, r2.URL}, pick: []string{r1.URL, r2.URL}}
	c := newClient(ch, r1.URL)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, s := range []string{"1.2.series.x", "1.2.series.y"} {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			if err := c.PushSeries(context.Background(), st, uid); err != nil {
				errs <- err
			}
		}(s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent push failed: %v", err)
	}

	sr, _ := st.GetStudy(study)
	if sr.AssignedPeerURL == "" {
		t.Fatal("study ended with no receiver assigned")
	}

	// Everything must be on the assigned receiver, and nothing on the other.
	assigned, other := r1, r2
	if sr.AssignedPeerURL == r2.URL {
		assigned, other = r2, r1
	}
	if other.count() != 0 {
		t.Fatalf("study SPLIT: %d instances went to the unassigned receiver %s",
			other.count(), other.URL)
	}
	if assigned.count() != 4 {
		t.Fatalf("assigned receiver got %d instances, want all 4", assigned.count())
	}
}

// §10.2/3 — nothing reachable must not move an already-assigned study. Moving
// on no information would re-send it in full to a receiver we cannot reach.
func TestNothingReachableKeepsTheAssignment(t *testing.T) {
	st, dir := newStore(t)
	r1, r2 := newFakeReceiver(t), newFakeReceiver(t)

	const study = "1.2.study.dark"
	seedSeries(t, st, dir, study, "1.2.series.d1", 2)
	seedSeries(t, st, dir, study, "1.2.series.d2", 2)

	ch := &scriptedChooser{cands: []string{r1.URL, r2.URL}, pick: []string{r2.URL}}
	c := newClient(ch, r1.URL)
	if err := c.PushSeries(context.Background(), st, "1.2.series.d1"); err != nil {
		t.Fatalf("push d1: %v", err)
	}

	// Now the chooser reports nothing usable, and r2 is cooling.
	ch.mu.Lock()
	ch.pick = []string{""}
	ch.n = 0
	ch.cooling = map[string]bool{r2.URL: true}
	ch.mu.Unlock()

	if err := c.PushSeries(context.Background(), st, "1.2.series.d2"); err != nil {
		t.Fatalf("push d2: %v", err)
	}
	sr, _ := st.GetStudy(study)
	if sr.AssignedPeerURL != r2.URL {
		t.Fatalf("assignment moved to %q with nothing reachable; want %q", sr.AssignedPeerURL, r2.URL)
	}
	if r1.count() != 0 {
		t.Fatalf("router1 received %d instances despite the study being assigned elsewhere", r1.count())
	}
}

// §10.2/5 — a cancelled push is our shutdown, not the receiver's fault.
// Cooling a healthy receiver off would send the next study somewhere worse.
func TestCancellationDoesNotCoolDownTheReceiver(t *testing.T) {
	st, dir := newStore(t)
	r1 := newFakeReceiver(t)

	const study = "1.2.study.cancel"
	seedSeries(t, st, dir, study, "1.2.series.c", 2)

	ch := &scriptedChooser{cands: []string{r1.URL}, pick: []string{r1.URL}}
	c := newClient(ch, r1.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the push starts
	_ = c.PushSeries(ctx, st, "1.2.series.c")

	if got := ch.failures(); len(got) != 0 {
		t.Fatalf("MarkFailed called on cancellation: %v", got)
	}
}
