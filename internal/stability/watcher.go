// Package stability implements a per-series stability watcher.
//
// The legacy Orthanc Lua scripts use the OnStableSeries callback: Orthanc
// fires it when a series has received no new instances for a configurable
// period (default 60s, but Bharat PACS uses 15s). At that point the series
// is considered "complete" and ready to transfer.
//
// We replicate the same behavior natively. Each Touch resets a per-series
// timer; when the timer fires (no Touch for stable_age) we mark the series
// stable and enqueue it for transfer.
//
// Implementation is a single mutex-guarded map of timers. At 50k studies
// per month with average ~60 instances per study, peak concurrent series
// is in the low hundreds — entirely fine for in-memory state.
package stability

import (
	"sync"
	"time"
)

// Watcher tracks per-series staleness.
type Watcher struct {
	stableAge time.Duration
	onStable  func(seriesUID string)

	mu      sync.Mutex
	timers  map[string]*time.Timer
	counts  map[string]int
	stopped bool
}

// New constructs a Watcher. stableAge is the quiet period before a series
// is declared stable. onStable is invoked from the timer goroutine when
// stability fires; it must not block the timer for long (kick off async
// work if needed).
func New(stableAge time.Duration, onStable func(string)) *Watcher {
	return &Watcher{
		stableAge: stableAge,
		onStable:  onStable,
		timers:    make(map[string]*time.Timer),
		counts:    make(map[string]int),
	}
}

// TouchWithHint records that a new instance arrived for the given series.
// imagesInAcquisition, when > 0, is the value from DICOM tag (0020,1002)
// telling us how many images to expect. If we've received all of them, the
// series is fired immediately without waiting for the stable-age timer.
//
// If a timer is already pending for the series, it is reset to the full
// stableAge. Otherwise a new timer is started.
func (w *Watcher) TouchWithHint(seriesUID string, imagesInAcquisition int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}

	w.counts[seriesUID]++
	received := w.counts[seriesUID]

	// If the modality told us the total, and we've received all instances,
	// fire immediately without waiting for the stable-age timer.
	if imagesInAcquisition > 0 && received >= imagesInAcquisition {
		if t, ok := w.timers[seriesUID]; ok {
			t.Stop()
			delete(w.timers, seriesUID)
		}
		delete(w.counts, seriesUID)
		uid := seriesUID
		go w.onStable(uid) // run in goroutine so we don't block under the lock
		return
	}

	if t, ok := w.timers[seriesUID]; ok {
		// Reset the existing timer. time.Timer.Reset on a fired or stopped
		// timer is fine because we delete the map entry inside the timer
		// callback below — if we're here, the timer hasn't fired yet.
		t.Reset(w.stableAge)
		return
	}

	uid := seriesUID
	w.timers[uid] = time.AfterFunc(w.stableAge, func() {
		w.mu.Lock()
		// Re-check stopped under lock — Stop may have been called.
		if w.stopped {
			delete(w.timers, uid)
			w.mu.Unlock()
			return
		}
		delete(w.timers, uid)
		delete(w.counts, uid)
		w.mu.Unlock()
		w.onStable(uid)
	})
}

// Touch is a backward-compatible shim that calls TouchWithHint with no hint.
func (w *Watcher) Touch(seriesUID string) { w.TouchWithHint(seriesUID, 0) }

// Pending returns the number of series currently being watched.
func (w *Watcher) Pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.timers)
}

// Stop cancels all pending timers. Pending callbacks already running will
// finish; future Touch calls become no-ops.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	for _, t := range w.timers {
		t.Stop()
	}
	w.timers = make(map[string]*time.Timer)
	w.counts = make(map[string]int)
}
