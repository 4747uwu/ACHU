package queue

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// TestSuccessfulProcess verifies that successful processing dequeues entries
// without re-enqueueing.
func TestSuccessfulProcess(t *testing.T) {
	st := newTestStore(t)
	primeSeries(t, st, "series-A")

	if err := EnqueueSeries(st, "series-A"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var processed int32
	w := &Worker{
		Store: st,
		Process: func(_ context.Context, entry store.QueueEntry) error {
			atomic.AddInt32(&processed, 1)
			// Mimic real processor: mark resource Delivered on success.
			return st.SetSeriesStatus(entry.ResourceUID, store.StatusDelivered)
		},
		Workers:      1,
		PollInterval: 10 * time.Millisecond,
		MaxRetries:   3,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)

	waitFor(t, 500*time.Millisecond, func() bool {
		return atomic.LoadInt32(&processed) == 1
	})
	cancel()

	depth, _ := st.QueueDepth()
	if depth != 0 {
		t.Errorf("queue depth = %d after success, want 0", depth)
	}
	ser, _ := st.GetSeries("series-A")
	if ser.Status != store.StatusDelivered {
		t.Errorf("series status = %q, want delivered", ser.Status)
	}
}

// TestRetriesThenFails verifies that a Processor returning errors triggers
// retries with backoff, and after MaxRetries the resource is marked Failed.
func TestRetriesThenFails(t *testing.T) {
	st := newTestStore(t)
	primeSeries(t, st, "series-X")

	if err := EnqueueSeries(st, "series-X"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var attempts int32
	w := &Worker{
		Store: st,
		Process: func(_ context.Context, _ store.QueueEntry) error {
			atomic.AddInt32(&attempts, 1)
			return errors.New("simulated")
		},
		Workers:      1,
		PollInterval: 5 * time.Millisecond,
		MaxRetries:   3,
		Backoff:      func(int) time.Duration { return 5 * time.Millisecond },
	}

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)

	// MaxRetries=3 with backoffs 1s, 5s — we'd wait too long for full
	// integration. Override backoffs by using a tiny MaxRetries and
	// counting attempts up to that.
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&attempts) >= 3
	})
	cancel()

	// Wait briefly for the final markStatus.
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&attempts) < 3 {
		t.Errorf("attempts = %d, want >= 3", atomic.LoadInt32(&attempts))
	}
	ser, _ := st.GetSeries("series-X")
	if ser.Status != store.StatusFailed {
		t.Errorf("series status = %q, want failed", ser.Status)
	}
}

// ------------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------------

var initLogOnce sync.Once

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	initLogOnce.Do(func() {
		_ = log.Init(t.TempDir(), "error")
	})
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// primeSeries creates a parent study + series so SetSeriesStatus can find them.
func primeSeries(t *testing.T, st *store.Store, seriesUID string) {
	t.Helper()
	rec := store.InstanceRecord{
		SOPInstanceUID:    seriesUID + ".inst.1",
		SeriesInstanceUID: seriesUID,
		StudyInstanceUID:  seriesUID + ".study",
		FileSize:          1,
		ReceivedAt:        time.Now(),
	}
	if err := st.PutInstance(rec, store.SeriesMetaUpdate{Modality: "CT"}, store.StudyMetaUpdate{}); err != nil {
		t.Fatalf("prime: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for condition after %s", timeout)
}

// computeBackoff exposed for testability (used implicitly in the worker).
var _ = computeBackoff
