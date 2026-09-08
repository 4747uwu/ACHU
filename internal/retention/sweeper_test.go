package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// TestSweepDeletesOnlyAgedDelivered verifies that the sweeper removes
// Delivered studies past the retention horizon and leaves everything else
// alone (other statuses, recently-delivered).
func TestSweepDeletesOnlyAgedDelivered(t *testing.T) {
	st, dataDir := newTestStore(t)

	// Three studies in different states + ages.
	stOld := mkStudyWithFile(t, st, dataDir, "study-old", "ser-1", "inst-1", store.StatusDelivered, 48*time.Hour)
	stRecent := mkStudyWithFile(t, st, dataDir, "study-new", "ser-2", "inst-2", store.StatusDelivered, 1*time.Hour)
	stFailed := mkStudyWithFile(t, st, dataDir, "study-failed", "ser-3", "inst-3", store.StatusFailed, 48*time.Hour)

	sw := New(st, dataDir, 24*time.Hour, time.Hour)
	sw.sweepOnce()

	// Old delivered: should be GONE.
	if _, err := st.GetStudy("study-old"); err == nil {
		t.Errorf("study-old should be deleted, but is still in DB")
	}
	if _, err := os.Stat(stOld); err == nil {
		t.Errorf("study-old file should be removed: %s", stOld)
	}

	// Recently delivered: should remain.
	if _, err := st.GetStudy("study-new"); err != nil {
		t.Errorf("study-new should be retained (under 24h), got err=%v", err)
	}
	if _, err := os.Stat(stRecent); err != nil {
		t.Errorf("study-new file should remain: %v", err)
	}

	// Failed: should never be touched, regardless of age.
	if _, err := st.GetStudy("study-failed"); err != nil {
		t.Errorf("study-failed should never be auto-deleted, got err=%v", err)
	}
	if _, err := os.Stat(stFailed); err != nil {
		t.Errorf("study-failed file should remain: %v", err)
	}
}

// TestForceDeleteRemovesEverything verifies a single DeleteStudy call wipes
// the on-disk file, the instance/series/study rows, and the index entries.
func TestForceDeleteRemovesEverything(t *testing.T) {
	st, dataDir := newTestStore(t)
	filePath := mkStudyWithFile(t, st, dataDir, "study-X", "ser-X", "inst-X", store.StatusReceived, 0)

	if err := DeleteStudy(st, dataDir, "study-X"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// File: gone.
	if _, err := os.Stat(filePath); err == nil {
		t.Errorf("file should be gone: %s", filePath)
	}

	// Study: gone.
	if _, err := st.GetStudy("study-X"); err == nil {
		t.Errorf("study should be gone")
	}

	// Series: gone.
	if _, err := st.GetSeries("ser-X"); err == nil {
		t.Errorf("series should be gone")
	}

	// Instance: gone.
	if _, err := st.GetInstance("inst-X"); err == nil {
		t.Errorf("instance should be gone")
	}

	// ListSeriesByStudy and ListInstancesBySeries should both return empty
	// — i.e. the index buckets were also cleaned up.
	sers, _ := st.ListSeriesByStudy("study-X")
	if len(sers) != 0 {
		t.Errorf("series index for study-X = %d entries, want 0", len(sers))
	}
	insts, _ := st.ListInstancesBySeries("ser-X")
	if len(insts) != 0 {
		t.Errorf("instance index for ser-X = %d entries, want 0", len(insts))
	}

	// Status index should also have nothing for study-X.
	all, _ := st.ListStudies(store.StatusReceived, 0)
	for _, s := range all {
		if s.StudyInstanceUID == "study-X" {
			t.Errorf("study-X still in status index")
		}
	}
}

// TestDeleteStudyIdempotent: deleting a non-existent study is a no-op,
// not an error.
func TestDeleteStudyIdempotent(t *testing.T) {
	st, dataDir := newTestStore(t)

	// First call against an absent study — must error (study not found at
	// the file/dir level — we use GetStudy).
	if err := DeleteStudy(st, dataDir, "no-such-study"); err == nil {
		t.Errorf("DeleteStudy on missing study should error (caller can decide)")
	}

	// But the underlying cascade is idempotent — calling it directly on
	// a non-existent UID returns nil.
	if err := st.DeleteStudyCascade("no-such-study"); err != nil {
		t.Errorf("DeleteStudyCascade on missing UID should be a no-op, got %v", err)
	}
}

// TestSweeperStartStop runs the goroutine path briefly to make sure
// Start/Stop lifecycle is clean under -race.
func TestSweeperStartStop(t *testing.T) {
	st, dataDir := newTestStore(t)
	sw := New(st, dataDir, 24*time.Hour, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sw.Start(ctx)

	// Let a couple of ticks fire so the loop iterates.
	time.Sleep(150 * time.Millisecond)
	sw.Stop()
}

// ------------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------------

func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	_ = log.Init(t.TempDir(), "error")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

// mkStudyWithFile creates a study/series/instance triple and writes a small
// file at the conventional path. Sets the study status, and if ageBefore
// is non-zero, dates DeliveredAt that far in the past.
//
// Returns the file path so tests can stat() it later.
func mkStudyWithFile(t *testing.T, st *store.Store, dataDir, studyUID, seriesUID, sopUID, status string, ageBefore time.Duration) string {
	t.Helper()
	now := time.Now()
	filePath := filepath.Join(dataDir, "instances", studyUID, seriesUID, sopUID+".dcm")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	rec := store.InstanceRecord{
		SOPInstanceUID:    sopUID,
		SeriesInstanceUID: seriesUID,
		StudyInstanceUID:  studyUID,
		FilePath:          filePath,
		FileSize:          5,
		ReceivedAt:        now.Add(-ageBefore),
	}
	if err := st.PutInstance(rec, store.SeriesMetaUpdate{Modality: "CT"}, store.StudyMetaUpdate{}); err != nil {
		t.Fatalf("put instance: %v", err)
	}

	// Move into the requested status. SetStudyStatus stamps DeliveredAt
	// for Delivered transitions, but we want to backdate it for the
	// "old" study so the sweeper sees it as past the horizon.
	if err := st.SetStudyStatus(studyUID, status); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if status == store.StatusDelivered && ageBefore > 0 {
		s, _ := st.GetStudy(studyUID)
		backdated := now.Add(-ageBefore)
		s.DeliveredAt = &backdated
		s.LastInstanceAt = backdated
		if err := st.UpdateStudy(s); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}

	return filePath
}
