// Package queue implements the durable transfer queue worker.
//
// Queue entries are produced by the stability watcher (M3) and consumed by
// transfer workers (M4). Persistence is handled by the BoltDB store: this
// package is just the loop that pulls work, dispatches it to a Processor,
// and handles retries with exponential backoff.
//
// On success: the entry is done. On failure: re-enqueue with NotBefore set
// to now + backoff, increment Retries. After MaxRetries the resource is
// marked Failed in the store and the entry is dropped.
package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// Processor is invoked per dequeued entry. It runs the actual work — for
// the sender, this is the Transfer Accelerator push (M4).
//
// Returning nil = success. Returning a non-nil error triggers retry with
// backoff up to MaxRetries.
type Processor func(ctx context.Context, entry store.QueueEntry) error

// Worker pulls queue entries and dispatches them to a Processor.
type Worker struct {
	Store        *store.Store
	Process      Processor
	Workers      int           // number of concurrent goroutines
	PollInterval time.Duration // how often to check for ready work
	MaxRetries   int           // data/server-error retries before marking Failed

	// Backoff returns the delay before the n'th data-error retry. If nil,
	// computeBackoff is used (1s/5s/30s/5m/30m).
	Backoff func(retries int) time.Duration

	// ConnCheck, if set, is called before processing each upload job.
	// If it returns a non-nil error the worker blocks until it returns nil
	// (or ctx is cancelled). This prevents burning retry budget while the
	// network is down.
	ConnCheck func(ctx context.Context) error

	// NetworkRetryEnabled, if set, is called to check whether the network-error
	// retry feature is active. If nil, it defaults to enabled.
	NetworkRetryEnabled func() bool
}

// Defaults applied if fields are zero.
const (
	defaultWorkers      = 6
	defaultPollInterval = 500 * time.Millisecond
	defaultMaxRetries   = 5
)

// Run starts Workers goroutines and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w.Workers <= 0 {
		w.Workers = defaultWorkers
	}
	if w.PollInterval <= 0 {
		w.PollInterval = defaultPollInterval
	}
	if w.MaxRetries <= 0 {
		w.MaxRetries = defaultMaxRetries
	}
	if w.Store == nil || w.Process == nil {
		log.L().Error("queue worker not properly configured", "store_nil", w.Store == nil, "process_nil", w.Process == nil)
		return
	}

	log.L().Info("queue worker starting", "concurrency", w.Workers, "poll_interval", w.PollInterval)

	var wg sync.WaitGroup
	for i := 0; i < w.Workers; i++ {
		wg.Add(1)
		go w.workerLoop(ctx, &wg, i+1)
	}
	wg.Wait()
	log.L().Info("queue worker stopped")
}

func (w *Worker) workerLoop(ctx context.Context, wg *sync.WaitGroup, id int) {
	defer wg.Done()
	logger := log.L().With("worker", id)

	t := time.NewTicker(w.PollInterval)
	defer t.Stop()

	for {
		// Drain the queue as fast as we can while there's ready work.
		// Only fall back to the ticker once we hit "nothing ready".
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			handled := w.processOne(ctx, logger)
			if !handled {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// processOne pops one ready entry and processes it. Returns true if
// something was handled (caller should immediately check for more work),
// false if the queue had nothing ready.
func (w *Worker) processOne(ctx context.Context, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) bool {
	entry, err := w.Store.PopReadyQueueEntry(time.Now())
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		logger.Warn("queue pop failed", "err", err)
		return false
	}

	// For upload jobs, block until the peer is reachable. This avoids burning
	// the retry budget while the network is down.
	if w.ConnCheck != nil && (entry.ResourceLevel == "series") {
		if err := w.ConnCheck(ctx); err != nil {
			// ctx cancelled while waiting for connectivity — put the entry back.
			if enqErr := w.Store.Enqueue(entry); enqErr != nil {
				logger.Warn("re-enqueue on shutdown", "id", entry.ID, "err", enqErr)
			}
			return false
		}
	}

	logger.Info("processing queue entry",
		"id", entry.ID,
		"resource", entry.ResourceUID,
		"level", entry.ResourceLevel,
		"retries", entry.Retries,
		"net_retries", entry.NetworkRetries,
	)

	// Reflect "in progress" status on the resource if we know its level.
	w.markStatus(entry, store.StatusSending)

	procErr := w.Process(ctx, entry)
	if procErr == nil {
		// Success — the Processor itself is responsible for setting the
		// resource's final status (Delivered) because only it knows when
		// every related transfer is complete.
		logger.Info("queue entry done", "id", entry.ID)
		return true
	}

	entry.LastError = procErr.Error()

	// Network failures (EOF, connection reset, timeout) do NOT consume the
	// retry budget — they are transient and will resolve when the network
	// returns. We re-queue with a gentle backoff but keep entry.Retries
	// unchanged so the study never lands in "Failed" just because of
	// a temporary outage.
	netRetryOn := w.NetworkRetryEnabled == nil || w.NetworkRetryEnabled()
	if netRetryOn && isNetworkError(procErr) {
		entry.NetworkRetries++
		backoffSec := networkBackoff(entry.NetworkRetries)
		entry.NotBefore = time.Now().Add(time.Duration(backoffSec) * time.Second)
		logger.Warn("network error, will retry (budget preserved)",
			"id", entry.ID,
			"net_retries", entry.NetworkRetries,
			"backoff_s", backoffSec,
			"err", procErr,
		)
		if err := w.Store.Enqueue(entry); err != nil {
			logger.Error("re-enqueue (net) failed", "id", entry.ID, "err", err)
			w.markStatus(entry, store.StatusFailed)
		}
		return true
	}

	// Data/server error — count against the capped budget.
	entry.Retries++

	if entry.Retries >= w.MaxRetries {
		logger.Error("queue entry exhausted retries",
			"id", entry.ID,
			"retries", entry.Retries,
			"err", procErr,
		)
		w.markStatus(entry, store.StatusFailed)
		return true
	}

	backoff := w.backoffFor(entry.Retries)
	entry.NotBefore = time.Now().Add(backoff)

	logger.Warn("queue entry failed, will retry",
		"id", entry.ID,
		"retries", entry.Retries,
		"backoff", backoff,
		"err", procErr,
	)

	if err := w.Store.Enqueue(entry); err != nil {
		logger.Error("re-enqueue failed", "id", entry.ID, "err", err)
		w.markStatus(entry, store.StatusFailed)
	}
	return true
}

// markStatus updates the resource's status, swallowing errors. Best-effort.
func (w *Worker) markStatus(entry store.QueueEntry, status string) {
	switch entry.ResourceLevel {
	case "series":
		_ = w.Store.SetSeriesStatus(entry.ResourceUID, status)
		// Propagate sending/failed to the parent study so the worklist
		// reflects the live state (progress bars key off study status).
		if status == store.StatusSending || status == store.StatusFailed {
			if ser, err := w.Store.GetSeries(entry.ResourceUID); err == nil {
				_ = w.Store.SetStudyStatus(ser.StudyInstanceUID, status)
			}
		}
	case "study":
		_ = w.Store.SetStudyStatus(entry.ResourceUID, status)
	case "transcode":
		// Propagate transcoding/failed state to both series and study so the
		// worklist can show the shimmer bar during transcoding.
		if status == store.StatusFailed {
			_ = w.Store.SetSeriesStatus(entry.ResourceUID, status)
			if ser, err := w.Store.GetSeries(entry.ResourceUID); err == nil {
				_ = w.Store.SetStudyStatus(ser.StudyInstanceUID, status)
			}
		} else if status == store.StatusSending {
			// "sending" is used as the in-progress marker for transcode too.
			_ = w.Store.SetSeriesStatus(entry.ResourceUID, store.StatusPendingTranscode)
			if ser, err := w.Store.GetSeries(entry.ResourceUID); err == nil {
				_ = w.Store.SetStudyStatus(ser.StudyInstanceUID, store.StatusPendingTranscode)
			}
		}
	}
}

// backoffFor returns the delay before retrying after the given retry count.
// Uses Worker.Backoff if set, otherwise computeBackoff.
func (w *Worker) backoffFor(retries int) time.Duration {
	if w.Backoff != nil {
		return w.Backoff(retries)
	}
	return computeBackoff(retries)
}

// computeBackoff returns the delay before the n'th retry (1-indexed):
//
//	1: 1s, 2: 5s, 3: 30s, 4: 5min, 5+: 30min
func computeBackoff(retries int) time.Duration {
	switch retries {
	case 1:
		return 1 * time.Second
	case 2:
		return 5 * time.Second
	case 3:
		return 30 * time.Second
	case 4:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// EnqueueSeries is a convenience for stability-watcher callbacks: marks the
// series queued and pushes a transfer entry for it.
func EnqueueSeries(s *store.Store, seriesUID string) error {
	if err := s.SetSeriesStatus(seriesUID, store.StatusQueued); err != nil {
		return fmt.Errorf("mark series queued: %w", err)
	}
	now := time.Now()
	return s.Enqueue(store.QueueEntry{
		ID:            fmt.Sprintf("series:%s:%d", seriesUID, now.UnixNano()),
		ResourceLevel: "series",
		ResourceUID:   seriesUID,
		CreatedAt:     now,
		NotBefore:     now,
	})
}

// EnqueueTranscode marks a series as pending transcoding and pushes a
// transcode entry into the queue. The queue worker will transcode all
// instances, then call EnqueueSeries to queue the series for transfer.
func EnqueueTranscode(s *store.Store, seriesUID string) error {
	if err := s.SetSeriesStatus(seriesUID, store.StatusPendingTranscode); err != nil {
		return fmt.Errorf("mark series transcoding: %w", err)
	}
	now := time.Now()
	return s.Enqueue(store.QueueEntry{
		ID:            fmt.Sprintf("transcode:%s:%d", seriesUID, now.UnixNano()),
		ResourceLevel: "transcode",
		ResourceUID:   seriesUID,
		CreatedAt:     now,
		NotBefore:     now,
	})
}
