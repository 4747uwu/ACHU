package connectivity

import "testing"

// A single failed probe used to pause every upload. On a small clinic uplink
// the uploads saturate the link, the probe's own TCP connect then times out,
// and the pipeline halted itself on a ~90 second cycle while the peer was
// still accepting series. These tests pin the hysteresis that stops it.

func newWatcher() *Watcher {
	w := &Watcher{PeerURL: "https://peer.example"}
	w.wakeCh = make(chan struct{})
	w.online.Store(true) // start healthy, as it is after the first good probe
	return w
}

func TestOneFailedProbeDoesNotPauseUploads(t *testing.T) {
	w := newWatcher()
	w.setOnline(false)
	if !w.IsOnline() {
		t.Fatal("a single failed probe paused uploads — the oscillation is back")
	}
}

func TestTwoFailedProbesStillDoNotPause(t *testing.T) {
	w := newWatcher()
	w.setOnline(false)
	w.setOnline(false)
	if !w.IsOnline() {
		t.Fatalf("paused after 2 failures, want %d", unreachableAfter)
	}
}

func TestGenuineOutageStillPauses(t *testing.T) {
	w := newWatcher()
	for i := 0; i < unreachableAfter; i++ {
		w.setOnline(false)
	}
	if w.IsOnline() {
		t.Fatalf("still online after %d consecutive failures — a real outage would never pause", unreachableAfter)
	}
}

// The counter must reset on success, or scattered failures over hours would
// eventually add up to a pause that was never warranted.
func TestSuccessResetsTheFailureCount(t *testing.T) {
	w := newWatcher()
	for i := 0; i < unreachableAfter-1; i++ {
		w.setOnline(false)
	}
	w.setOnline(true) // one good probe
	for i := 0; i < unreachableAfter-1; i++ {
		w.setOnline(false)
	}
	if !w.IsOnline() {
		t.Fatal("failures accumulated across a success — the counter did not reset")
	}
}

// Recovery stays immediate: waiting three good probes to resume would add 90
// seconds to every recovery for no benefit.
func TestRecoveryIsImmediate(t *testing.T) {
	w := newWatcher()
	for i := 0; i < unreachableAfter; i++ {
		w.setOnline(false)
	}
	if w.IsOnline() {
		t.Fatal("setup failed: expected offline")
	}
	w.setOnline(true)
	if !w.IsOnline() {
		t.Fatal("one good probe did not resume uploads")
	}
}
