package connectivity

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Receiver selection, driven through the probeFn hook so none of these touch
// the network. The two at the bottom deliberately do use real HTTP, because
// they are about what the transport does with a redirect and a 401.

const (
	r1 = "https://router1.example"
	r2 = "https://router2.example"
)

// fakeProbe returns a probeFn backed by a map that tests can mutate, plus a
// counter of how many probes actually ran.
func fakeProbe(up map[string]bool) (func(context.Context, string) (bool, string), *int32, *sync.Mutex) {
	var mu sync.Mutex
	var calls int32
	fn := func(_ context.Context, url string) (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if up[url] {
			return true, ""
		}
		return false, "simulated down"
	}
	return fn, &calls, &mu
}

func autoWatcher(up map[string]bool) *Watcher {
	fn, _, _ := fakeProbe(up)
	return &Watcher{URLs: []string{r1, r2}, probeFn: fn}
}

func TestAutoPrefersRouter1WhenBothReachable(t *testing.T) {
	w := autoWatcher(map[string]bool{r1: true, r2: true})
	got, ok := w.ChoosePeer(context.Background())
	if !ok || got != r1 {
		t.Fatalf("got %q ok=%v, want %q true", got, ok, r1)
	}
}

func TestAutoUsesRouter2WhenRouter1Down(t *testing.T) {
	up := map[string]bool{r1: false, r2: true}
	w := autoWatcher(up)

	got, ok := w.ChoosePeer(context.Background())
	if !ok || got != r2 {
		t.Fatalf("router1 down: got %q ok=%v, want %q true", got, ok, r2)
	}

	// Router 1 recovers. The freshness window must expire before the next
	// choice, or we would just re-read the stale "down".
	up[r1] = true
	expireFreshness(w)
	got, ok = w.ChoosePeer(context.Background())
	if !ok || got != r1 {
		t.Fatalf("after recovery: got %q ok=%v, want %q true", got, ok, r1)
	}
}

func TestAutoFailedReceiverIsPassedOver(t *testing.T) {
	w := autoWatcher(map[string]bool{r1: true, r2: true})
	w.MarkFailed(r1, context.DeadlineExceeded)

	got, ok := w.ChoosePeer(context.Background())
	if !ok || got != r2 {
		t.Fatalf("got %q ok=%v, want %q true (router1 cooling)", got, ok, r2)
	}
}

// A cooling receiver still beats no receiver at all.
func TestAutoFallsBackToACoolingReceiverRatherThanNone(t *testing.T) {
	w := autoWatcher(map[string]bool{r1: true, r2: false})
	w.MarkFailed(r1, context.DeadlineExceeded)

	got, ok := w.ChoosePeer(context.Background())
	if !ok || got != r1 {
		t.Fatalf("got %q ok=%v, want %q true (only reachable, though cooling)", got, ok, r1)
	}
}

func TestAutoCooldownExpires(t *testing.T) {
	w := autoWatcher(map[string]bool{r1: true, r2: true})
	w.FailureCooldown = 30 * time.Millisecond
	w.MarkFailed(r1, context.DeadlineExceeded)

	if got, _ := w.ChoosePeer(context.Background()); got != r2 {
		t.Fatalf("while cooling: got %q, want %q", got, r2)
	}
	time.Sleep(60 * time.Millisecond)
	expireFreshness(w)
	if got, _ := w.ChoosePeer(context.Background()); got != r1 {
		t.Fatalf("after cooldown: got %q, want %q", got, r1)
	}
}

func TestNoneReachableReturnsPreferredNotOK(t *testing.T) {
	w := autoWatcher(map[string]bool{r1: false, r2: false})
	got, ok := w.ChoosePeer(context.Background())
	if ok {
		t.Fatal("reported a usable receiver with everything down")
	}
	if got != r1 {
		t.Fatalf("got %q, want the preferred %q so the caller has somewhere to go", got, r1)
	}
}

// A pin is the operator's decision. Neither a failure nor an outage moves it.
func TestManualPinHoldsEvenWhenOtherIsPreferred(t *testing.T) {
	fn, _, _ := fakeProbe(map[string]bool{r1: true, r2: true})
	w := &Watcher{URLs: []string{r2, r1}, Manual: true, probeFn: fn}

	if got, ok := w.ChoosePeer(context.Background()); got != r2 || !ok {
		t.Fatalf("got %q ok=%v, want %q true", got, ok, r2)
	}

	w.MarkFailed(r2, context.DeadlineExceeded)
	if w.IsCoolingDown(r2) {
		t.Fatal("a manual pin was put on cooldown — failover would follow")
	}
	expireFreshness(w)
	if got, _ := w.ChoosePeer(context.Background()); got != r2 {
		t.Fatalf("after failure: got %q, want the pinned %q", got, r2)
	}

	// Even while down, a pin does not move — it reports unusable instead.
	fn2, _, _ := fakeProbe(map[string]bool{r1: true, r2: false})
	w.probeFn = fn2
	expireFreshness(w)
	got, ok := w.ChoosePeer(context.Background())
	if got != r2 {
		t.Fatalf("pinned receiver down: got %q, want %q", got, r2)
	}
	if ok {
		t.Fatal("reported the pinned receiver usable while it was down")
	}
}

func TestOnlineMeansAnyReceiver(t *testing.T) {
	w := autoWatcher(map[string]bool{r1: false, r2: true})
	w.probeAll(context.Background())
	if !w.IsOnline() {
		t.Fatal("offline while a fallback receiver was reachable")
	}
}

func TestWaitUntilOnlineWakesWhenAReceiverReturns(t *testing.T) {
	up := map[string]bool{r1: false, r2: false}
	w := autoWatcher(up)
	w.probeAll(context.Background())
	if w.IsOnline() {
		t.Fatal("setup: expected offline")
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- w.WaitUntilOnline(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	up[r2] = true
	w.probeAll(context.Background())

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitUntilOnline returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitUntilOnline did not wake when a receiver returned")
	}
}

func TestPeersStatusReportsActiveAndCooldown(t *testing.T) {
	w := autoWatcher(map[string]bool{r1: true, r2: true})
	w.MarkFailed(r1, context.DeadlineExceeded)
	if _, ok := w.ChoosePeer(context.Background()); !ok {
		t.Fatal("expected a usable receiver")
	}

	peers := w.Peers()
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}
	if peers[0].URL != r1 || !peers[0].CoolingDown {
		t.Fatalf("peer[0] = %+v, want %s cooling", peers[0], r1)
	}
	if peers[1].URL != r2 || !peers[1].Active {
		t.Fatalf("peer[1] = %+v, want %s active", peers[1], r2)
	}
	if w.ActivePeer() != r2 {
		t.Fatalf("ActivePeer = %q, want %q", w.ActivePeer(), r2)
	}
}

// --- real HTTP: the two things that made a broken receiver look healthy -----

// A GET follows a redirect and succeeds; a streamed upload POST cannot. The
// probe must call that unusable and name the working URL.
func TestRedirectingReceiverIsNotUsable(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsSrv.Close()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tlsSrv.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer plain.Close()

	w := &Watcher{URLs: []string{plain.URL}}
	// Trust the test server's cert so the ONLY objection can be the redirect.
	w.client = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	ok, msg := w.probe(context.Background(), plain.URL)
	if ok {
		t.Fatal("a receiver that redirects http->https was reported usable")
	}
	if !strings.Contains(msg, "redirects to https://") {
		t.Fatalf("message %q does not name the working URL", msg)
	}
	t.Logf("correctly rejected: %s", msg)
}

// Wrong credentials give an empty-bodied 401. Counting that as reachable made
// a site look online while every study failed.
func TestCredentialRejectionIsNotUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	w := &Watcher{URLs: []string{srv.URL}}
	ok, msg := w.probe(context.Background(), srv.URL)
	if ok {
		t.Fatal("a receiver rejecting the credentials was reported usable")
	}
	if !strings.Contains(msg, "401") {
		t.Fatalf("message %q does not mention the status", msg)
	}
	t.Logf("correctly rejected: %s", msg)
}

// expireFreshness backdates every probe so the next choice re-probes rather
// than reusing a cached verdict.
func expireFreshness(w *Watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, st := range w.state {
		st.lastProbe = time.Now().Add(-2 * probeFreshness)
	}
}
