package stability

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatcherFires verifies that a series fires once after the quiet period.
func TestWatcherFires(t *testing.T) {
	var fired int32
	var mu sync.Mutex
	var firedUIDs []string

	w := New(50*time.Millisecond, func(uid string) {
		atomic.AddInt32(&fired, 1)
		mu.Lock()
		firedUIDs = append(firedUIDs, uid)
		mu.Unlock()
	})
	t.Cleanup(w.Stop)

	w.Touch("series-a")

	// Wait for the timer to fire.
	time.Sleep(150 * time.Millisecond)

	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("fired = %d, want 1", got)
	}
	mu.Lock()
	if len(firedUIDs) != 1 || firedUIDs[0] != "series-a" {
		t.Errorf("firedUIDs = %v, want [series-a]", firedUIDs)
	}
	mu.Unlock()
}

// TestWatcherResets verifies that Touch resets the timer rather than firing
// twice or fragmenting.
func TestWatcherResets(t *testing.T) {
	var fired int32
	w := New(80*time.Millisecond, func(string) {
		atomic.AddInt32(&fired, 1)
	})
	t.Cleanup(w.Stop)

	// Touch every 30ms for 200ms — well under the 80ms threshold.
	w.Touch("hot-series")
	for i := 0; i < 6; i++ {
		time.Sleep(30 * time.Millisecond)
		w.Touch("hot-series")
		if atomic.LoadInt32(&fired) != 0 {
			t.Fatalf("fired prematurely after %dms", (i+1)*30)
		}
	}

	// Now stop touching and wait for stability.
	time.Sleep(150 * time.Millisecond)

	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("fired = %d, want 1 after stability", got)
	}
}

// TestWatcherIndependent verifies multiple series have independent timers.
func TestWatcherIndependent(t *testing.T) {
	var aFired, bFired int32
	w := New(60*time.Millisecond, func(uid string) {
		switch uid {
		case "a":
			atomic.AddInt32(&aFired, 1)
		case "b":
			atomic.AddInt32(&bFired, 1)
		}
	})
	t.Cleanup(w.Stop)

	w.Touch("a")
	time.Sleep(30 * time.Millisecond)
	w.Touch("b")
	// At t=80ms: a is past stability (started at 0, fires ~60ms),
	// b is still active (started at 30ms, fires ~90ms)
	time.Sleep(50 * time.Millisecond) // total t=80ms
	if atomic.LoadInt32(&aFired) != 1 {
		t.Errorf("a fired = %d at t=80ms, want 1", atomic.LoadInt32(&aFired))
	}
	// b shouldn't have fired yet (~10ms remaining)
	if atomic.LoadInt32(&bFired) != 0 {
		t.Errorf("b fired prematurely")
	}

	time.Sleep(50 * time.Millisecond) // total t=130ms — b should fire by now

	if atomic.LoadInt32(&bFired) != 1 {
		t.Errorf("b fired = %d, want 1", atomic.LoadInt32(&bFired))
	}
}

// TestWatcherStop ensures Stop prevents further fires.
func TestWatcherStop(t *testing.T) {
	var fired int32
	w := New(40*time.Millisecond, func(string) {
		atomic.AddInt32(&fired, 1)
	})

	w.Touch("series-a")
	w.Touch("series-b")
	w.Stop()

	time.Sleep(100 * time.Millisecond)

	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("fired = %d after Stop, want 0", got)
	}
}
