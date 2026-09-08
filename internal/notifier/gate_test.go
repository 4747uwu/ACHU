package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// recorder is a stand-in backend that records every payload it receives.
type recorder struct {
	mu   sync.Mutex
	got  []map[string]any
	srv  *httptest.Server
	fail bool
}

func newRecorder() *recorder {
	r := &recorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)

		r.mu.Lock()
		failing := r.fail
		if !failing {
			r.got = append(r.got, m)
		}
		r.mu.Unlock()

		if failing {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"success":true}`)
	}))
	return r
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *recorder) setFailing(b bool) {
	r.mu.Lock()
	r.fail = b
	r.mu.Unlock()
}

// waitFor polls cond for up to a second. The notifier posts in the background,
// so tests observe it rather than sequencing it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestGateParksAndFlushes is the behaviour the whole activation feature exists
// to provide: a study received while the account is inactive is not lost, it is
// announced once the account is restored.
func TestGateParksAndFlushes(t *testing.T) {
	rec := newRecorder()
	defer rec.srv.Close()

	var mu sync.Mutex
	active := false
	gate := func() (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		if active {
			return true, ""
		}
		return false, "Your account is inactive. Please contact Achyu."
	}

	n := New(rec.srv.URL+"/api/orthanc2/instance-exe-received", "", "ARX1", "ARX")
	n.Gate = gate

	n.MaybeNotify("1.2.3", StudyPayload{StudyInstanceUID: "1.2.3", PatientID: "P1"})

	waitFor(t, "the study to be parked", func() bool { return n.PendingCount() == 1 })
	if rec.count() != 0 {
		t.Fatal("a notification was sent while the lab was inactive")
	}

	// Reactivate; the flusher should drain the backlog.
	mu.Lock()
	active = true
	mu.Unlock()

	n.flushPending()

	if n.PendingCount() != 0 {
		t.Fatalf("backlog still holds %d notifications after reactivation", n.PendingCount())
	}
	if rec.count() != 1 {
		t.Fatalf("backend received %d notifications, want 1", rec.count())
	}
	if got := rec.got[0]["StudyInstanceUID"]; got != "1.2.3" {
		t.Fatalf("flushed the wrong study: %v", got)
	}
}

// TestDeliveredIsGatedToo: the delivered event carries the same commercial
// signal as the study announcement, so it is held on the same terms.
func TestDeliveredIsGatedToo(t *testing.T) {
	rec := newRecorder()
	defer rec.srv.Close()

	blocked := true
	n := New(rec.srv.URL+"/api/orthanc2/instance-exe-received", "", "ARX1", "ARX")
	n.Gate = func() (bool, string) { return !blocked, "inactive" }

	n.NotifyDelivered(DeliveredPayload{StudyInstanceUID: "1.2.3", PatientID: "P1"})

	waitFor(t, "the delivered event to be parked", func() bool { return n.PendingCount() == 1 })
	if rec.count() != 0 {
		t.Fatal("a delivered event was sent while the lab was inactive")
	}

	blocked = false
	n.flushPending()

	if rec.count() != 1 {
		t.Fatalf("backend received %d delivered events, want 1", rec.count())
	}
	if got := rec.got[0]["status"]; got != "delivered" {
		t.Fatalf("flushed payload status = %v, want delivered", got)
	}
	// The OrthancID must survive parking — it is how the server correlates the
	// study without a lookup.
	if got, _ := rec.got[0]["OrthancID"].(string); got != OrthancStudyID("P1", "1.2.3") {
		t.Fatalf("OrthancID lost in parking: %q", got)
	}
}

// TestPendingBacklogIsCapped keeps memory flat on a workstation left running
// for weeks behind a suspended account.
func TestPendingBacklogIsCapped(t *testing.T) {
	n := New("http://127.0.0.1:1/notify", "", "ARX1", "ARX")
	n.Gate = func() (bool, string) { return false, "inactive" }

	for i := 0; i < maxPending+50; i++ {
		n.park(pendingPost{kind: "study", label: fmt.Sprintf("uid-%d", i)})
	}

	if got := n.PendingCount(); got != maxPending {
		t.Fatalf("backlog held %d, want the cap of %d", got, maxPending)
	}

	// The cap must drop the OLDEST, so the newest study is still there.
	n.mu.Lock()
	newest := n.pending[len(n.pending)-1].label
	oldest := n.pending[0].label
	n.mu.Unlock()

	if want := fmt.Sprintf("uid-%d", maxPending+49); newest != want {
		t.Fatalf("newest held notification = %q, want %q", newest, want)
	}
	if oldest == "uid-0" {
		t.Fatal("the cap dropped the newest notifications instead of the oldest")
	}
}

// TestFlushStopsAtFirstFailure: a backend that starts failing mid-drain must
// not cost us the rest of the backlog, and must not reorder it.
func TestFlushStopsAtFirstFailure(t *testing.T) {
	rec := newRecorder()
	defer rec.srv.Close()

	n := New(rec.srv.URL+"/api/orthanc2/instance-exe-received", "", "ARX1", "ARX")
	n.Gate = func() (bool, string) { return true, "" }

	for i := 0; i < 3; i++ {
		n.park(pendingPost{
			kind:    "study",
			label:   fmt.Sprintf("uid-%d", i),
			url:     n.BackendURL,
			payload: StudyPayload{StudyInstanceUID: fmt.Sprintf("uid-%d", i)},
		})
	}

	rec.setFailing(true)
	n.flushPending()

	if got := n.PendingCount(); got != 3 {
		t.Fatalf("a failing backend cost us notifications: %d held, want 3", got)
	}
	n.mu.Lock()
	first := n.pending[0].label
	n.mu.Unlock()
	if first != "uid-0" {
		t.Fatalf("backlog order broken after a failed flush: front is %q, want uid-0", first)
	}

	rec.setFailing(false)
	n.flushPending()

	if got := n.PendingCount(); got != 0 {
		t.Fatalf("%d notifications still held after a successful flush", got)
	}
	if rec.count() != 3 {
		t.Fatalf("backend received %d notifications, want 3", rec.count())
	}
	for i, m := range rec.got {
		want := fmt.Sprintf("uid-%d", i)
		if m["StudyInstanceUID"] != want {
			t.Fatalf("notification %d was %v, want %s — arrival order not preserved",
				i, m["StudyInstanceUID"], want)
		}
	}
}

// TestNilGateIsAlwaysOpen: the gate is optional, and a sender built without one
// must behave exactly as it did before the feature existed.
func TestNilGateIsAlwaysOpen(t *testing.T) {
	rec := newRecorder()
	defer rec.srv.Close()

	n := New(rec.srv.URL+"/api/orthanc2/instance-exe-received", "", "ARX1", "ARX")

	n.MaybeNotify("1.2.3", StudyPayload{StudyInstanceUID: "1.2.3"})

	waitFor(t, "the notification to be sent", func() bool { return rec.count() == 1 })
	if n.PendingCount() != 0 {
		t.Fatal("a notification was parked despite there being no gate")
	}
}

// TestFlusherStopsWithContext: the flusher must not outlive shutdown.
func TestFlusherStopsWithContext(t *testing.T) {
	n := New("http://127.0.0.1:1/notify", "", "ARX1", "ARX")
	ctx, cancel := context.WithCancel(context.Background())
	n.StartPendingFlusher(ctx, 5*time.Millisecond)
	cancel()
	// Nothing to assert beyond not panicking or leaking; give it a tick to exit.
	time.Sleep(20 * time.Millisecond)
}
