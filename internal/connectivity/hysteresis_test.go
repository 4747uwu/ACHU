package connectivity

import "testing"

// A single failed probe used to pause every upload. On a small clinic uplink
// the uploads saturate the link, the probe's own TCP connect then times out,
// and the pipeline halted itself on a ~90 second cycle while the peer was
// still accepting series. These tests pin the hysteresis that stops it.
//
// The hysteresis is now per RECEIVER — one receiver stumbling must not pause
// uploads that another could carry — so these drive record() directly.

const testPeer = "https://r1.example"

// newWatcher returns a watcher with one receiver already known-good, which is
// the state hysteresis exists to protect. A receiver that has NEVER answered
// is marked down on its first failure: there is no working state to defend.
func newWatcher() *Watcher {
	w := &Watcher{URLs: []string{testPeer}}
	w.record(testPeer, true, "") // establishes lastOK
	return w
}

func fail(w *Watcher, n int) {
	for i := 0; i < n; i++ {
		w.record(testPeer, false, "simulated")
	}
}

func TestOneFailedProbeDoesNotPauseUploads(t *testing.T) {
	w := newWatcher()
	fail(w, 1)
	if !w.IsOnline() {
		t.Fatal("a single failed probe paused uploads — the oscillation is back")
	}
}

func TestTwoFailedProbesStillDoNotPause(t *testing.T) {
	w := newWatcher()
	fail(w, 2)
	if !w.IsOnline() {
		t.Fatalf("paused after 2 failures, want %d", unreachableAfter)
	}
}

func TestGenuineOutageStillPauses(t *testing.T) {
	w := newWatcher()
	fail(w, unreachableAfter)
	if w.IsOnline() {
		t.Fatalf("still online after %d consecutive failures — a real outage would never pause", unreachableAfter)
	}
}

// The counter must reset on success, or scattered failures over hours would
// eventually add up to a pause that was never warranted.
func TestSuccessResetsTheFailureCount(t *testing.T) {
	w := newWatcher()
	fail(w, unreachableAfter-1)
	w.record(testPeer, true, "") // one good probe
	fail(w, unreachableAfter-1)
	if !w.IsOnline() {
		t.Fatal("failures accumulated across a success — the counter did not reset")
	}
}

// Recovery stays immediate: waiting three good probes to resume would add 90
// seconds to every recovery for no benefit.
func TestRecoveryIsImmediate(t *testing.T) {
	w := newWatcher()
	fail(w, unreachableAfter)
	if w.IsOnline() {
		t.Fatal("setup failed: expected offline")
	}
	w.record(testPeer, true, "")
	if !w.IsOnline() {
		t.Fatal("one good probe did not resume uploads")
	}
}

// A receiver that has never answered must not get the grace period — an
// install pointed at a wrong URL should report unreachable immediately.
func TestNeverSeenReceiverIsDownOnFirstFailure(t *testing.T) {
	w := &Watcher{URLs: []string{testPeer}}
	w.record(testPeer, false, "dial error")
	if w.IsOnline() {
		t.Fatal("a receiver that never answered was treated as online")
	}
}

// One receiver failing must not pause uploads while another can carry them.
func TestOneReceiverDownDoesNotPauseWhenAnotherIsUp(t *testing.T) {
	const r2 = "https://r2.example"
	w := &Watcher{URLs: []string{testPeer, r2}}
	w.record(testPeer, true, "")
	w.record(r2, true, "")

	for i := 0; i < unreachableAfter; i++ {
		w.record(testPeer, false, "simulated")
	}
	if !w.IsOnline() {
		t.Fatal("uploads paused while a second receiver was still reachable")
	}
}
