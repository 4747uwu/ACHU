// Package retention implements the periodic sweep that deletes locally-
// stored DICOM data after it has been delivered to the receiver.
//
// Why a sweeper:
//
//   - Disk on a clinic workstation is small and shared. The sender must
//     not hoard data the way client-side Orthanc currently does.
//   - Once a study reaches StatusDelivered, the receiver-side Orthanc is
//     authoritative and we don't need a local copy. Keeping data around
//     for a configurable window (default 24h) is purely for the operator's
//     benefit: review what just shipped, retry on demand, etc.
//
// What gets deleted:
//
//   - Every instance file on disk under <data_dir>/instances/<study>/...
//   - The InstanceRecord, SeriesRecord, StudyRecord rows in BoltDB
//   - The status index entry for the study
//
// What does NOT get deleted:
//
//   - Studies still Incoming/Received/Stable/Queued/Sending/Failed
//     (only Delivered studies past their retention window are eligible)
//   - Transfer history rows (kept for the operator's audit trail —
//     they're tiny relative to instance files)
//   - The on-disk study directory itself if it isn't fully empty after
//     instance deletion (defensive — only removes empty dirs)
//
// Force-delete (used by DELETE /api/study/:uid) reuses the same teardown
// helper, applied to a specific study regardless of status or age.
package retention

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// Sweeper deletes Delivered studies past the retention horizon.
type Sweeper struct {
	Store         *store.Store
	DataDir       string
	Retention     time.Duration
	Interval      time.Duration

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	done    chan struct{}
}

// New constructs a Sweeper with sensible defaults.
func New(st *store.Store, dataDir string, retention, interval time.Duration) *Sweeper {
	if interval <= 0 {
		// Default: sweep once an hour. This is plenty for a 24h retention
		// — even at one sweep per day we'd never drift the eviction
		// window by more than a single sweep interval.
		interval = time.Hour
	}
	return &Sweeper{
		Store:     st,
		DataDir:   dataDir,
		Retention: retention,
		Interval:  interval,
	}
}

// Start kicks off the background sweep loop. Returns immediately.
func (s *Sweeper) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.mu.Unlock()

	go func() {
		defer close(s.done)
		log.L().Info("retention sweeper starting",
			"retention", s.Retention,
			"interval", s.Interval,
		)
		t := time.NewTicker(s.Interval)
		defer t.Stop()

		// Run one immediate sweep at startup so a long-running clinic
		// workstation that just rebooted starts cleaning up right away
		// rather than waiting an hour.
		s.sweepOnce()

		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case <-t.C:
				s.sweepOnce()
			}
		}
	}()
}

// Stop halts the sweep loop and waits for the in-flight sweep to finish.
func (s *Sweeper) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stop)
	done := s.done
	s.mu.Unlock()
	<-done
	log.L().Info("retention sweeper stopped")
}

// sweepOnce walks delivered studies and deletes any past the horizon.
//
// We scan only the Delivered status index — no full table scan — so the
// cost is proportional to "delivered studies awaiting eviction", not
// total studies the sender has ever seen.
func (s *Sweeper) sweepOnce() {
	if s.Retention <= 0 {
		return // retention disabled
	}
	cutoff := time.Now().Add(-s.Retention)

	studies, err := s.Store.ListStudies(store.StatusDelivered, 0)
	if err != nil {
		log.L().Warn("retention list", "err", err)
		return
	}

	deleted := 0
	for _, stu := range studies {
		// DeliveredAt is set by store.SetStudyStatus when the status
		// transitions to Delivered; fall back to LastInstanceAt for
		// studies migrated from older versions.
		var ref time.Time
		if stu.DeliveredAt != nil {
			ref = *stu.DeliveredAt
		} else {
			ref = stu.LastInstanceAt
		}
		if ref.IsZero() || ref.After(cutoff) {
			continue
		}

		if err := DeleteStudy(s.Store, s.DataDir, stu.StudyInstanceUID); err != nil {
			log.L().Warn("retention delete", "study_uid", stu.StudyInstanceUID, "err", err)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		log.L().Info("retention sweep done",
			"studies_deleted", deleted,
			"cutoff", cutoff.Format(time.RFC3339),
			"candidates", len(studies),
		)
	}
}

// DeleteStudy tears down all on-disk and in-DB state for one study.
//
// Used by the retention sweeper for delivered+aged studies, and by the
// HTTP API's DELETE /api/study/:uid for force-delete on operator request.
//
// Order matters: we delete on-disk files BEFORE DB rows so that, if the
// process crashes mid-way, the study is still discoverable via the
// remaining DB rows on restart and a subsequent sweep can complete the
// cleanup. The reverse order would orphan files we can no longer index.
func DeleteStudy(st *store.Store, dataDir, studyUID string) error {
	if studyUID == "" {
		return fmt.Errorf("delete study: empty UID")
	}

	// Read the study + series + instances first so we know what to clean.
	study, err := st.GetStudy(studyUID)
	if err != nil {
		return fmt.Errorf("get study: %w", err)
	}

	seriesList, err := st.ListSeriesByStudy(studyUID)
	if err != nil {
		return fmt.Errorf("list series: %w", err)
	}

	allInstances := make([]store.InstanceRecord, 0, study.InstanceCount)
	for _, ser := range seriesList {
		insts, err := st.ListInstancesBySeries(ser.SeriesInstanceUID)
		if err != nil {
			return fmt.Errorf("list instances of %s: %w", ser.SeriesInstanceUID, err)
		}
		allInstances = append(allInstances, insts...)
	}

	// 1. Delete every instance file from disk.
	var fileErrs int
	for _, inst := range allInstances {
		if inst.FilePath == "" {
			continue
		}
		if err := os.Remove(inst.FilePath); err != nil && !os.IsNotExist(err) {
			fileErrs++
			log.L().Warn("delete instance file", "path", inst.FilePath, "err", err)
		}
	}

	// 2. Try to remove the now-empty series and study directories. We use
	// Remove (not RemoveAll) on purpose: if anything unexpected lingers
	// in those dirs we'd rather leave it than wipe data we don't index.
	studyDir := filepath.Join(dataDir, "instances", studyUID)
	for _, ser := range seriesList {
		seriesDir := filepath.Join(studyDir, ser.SeriesInstanceUID)
		_ = os.Remove(seriesDir) // ignores ENOTEMPTY by design
	}
	_ = os.Remove(studyDir)

	// 3. Delete the DB rows. We delete instances first, then series, then
	// the study and its status-index entry — bottom-up so the aggregates
	// remain consistent if the txn is interrupted.
	if err := st.DeleteStudyCascade(studyUID); err != nil {
		return fmt.Errorf("cascade delete: %w", err)
	}

	log.L().Info("study deleted",
		"study_uid", studyUID,
		"series", len(seriesList),
		"instances", len(allInstances),
		"file_errors", fileErrs,
	)
	return nil
}
