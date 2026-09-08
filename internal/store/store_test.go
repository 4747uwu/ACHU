package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestPutInstanceAndList exercises the core ingest path: put two instances
// across two series of one study, then read it all back. This is the
// behavior the SCP handler depends on.
func TestPutInstanceAndList(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const (
		studyUID  = "1.2.840.10008.5.1.test.study.1"
		seriesUID1 = "1.2.840.10008.5.1.test.series.1"
		seriesUID2 = "1.2.840.10008.5.1.test.series.2"
	)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	in1 := InstanceRecord{
		SOPInstanceUID:    "1.2.840.10008.5.1.test.inst.1",
		SeriesInstanceUID: seriesUID1,
		StudyInstanceUID:  studyUID,
		FileSize:          1000,
		ReceivedAt:        now,
	}
	in2 := InstanceRecord{
		SOPInstanceUID:    "1.2.840.10008.5.1.test.inst.2",
		SeriesInstanceUID: seriesUID1,
		StudyInstanceUID:  studyUID,
		FileSize:          2000,
		ReceivedAt:        now.Add(time.Second),
	}
	in3 := InstanceRecord{
		SOPInstanceUID:    "1.2.840.10008.5.1.test.inst.3",
		SeriesInstanceUID: seriesUID2,
		StudyInstanceUID:  studyUID,
		FileSize:          500,
		ReceivedAt:        now.Add(2 * time.Second),
	}

	studyMeta := StudyMetaUpdate{
		PatientID:        "P-001",
		PatientName:      "Test^Patient",
		StudyDate:        "20260426",
		StudyDescription: "Unit test study",
	}
	if err := s.PutInstance(in1, SeriesMetaUpdate{Modality: "CT"}, studyMeta); err != nil {
		t.Fatalf("put in1: %v", err)
	}
	if err := s.PutInstance(in2, SeriesMetaUpdate{Modality: "CT"}, studyMeta); err != nil {
		t.Fatalf("put in2: %v", err)
	}
	if err := s.PutInstance(in3, SeriesMetaUpdate{Modality: "SR"}, studyMeta); err != nil {
		t.Fatalf("put in3: %v", err)
	}

	// Idempotent re-receive — should NOT double-count.
	if err := s.PutInstance(in1, SeriesMetaUpdate{Modality: "CT"}, studyMeta); err != nil {
		t.Fatalf("re-put in1: %v", err)
	}

	study, err := s.GetStudy(studyUID)
	if err != nil {
		t.Fatalf("get study: %v", err)
	}
	if study.PatientName != "Test^Patient" {
		t.Errorf("PatientName = %q, want Test^Patient", study.PatientName)
	}
	if study.SeriesCount != 2 {
		t.Errorf("SeriesCount = %d, want 2", study.SeriesCount)
	}
	if study.InstanceCount != 3 {
		t.Errorf("InstanceCount = %d, want 3 (re-put should be idempotent)", study.InstanceCount)
	}
	if study.TotalSize != 3500 {
		t.Errorf("TotalSize = %d, want 3500", study.TotalSize)
	}
	if len(study.Modalities) != 2 {
		t.Errorf("Modalities = %v, want 2 distinct", study.Modalities)
	}

	seriesList, err := s.ListSeriesByStudy(studyUID)
	if err != nil {
		t.Fatalf("list series: %v", err)
	}
	if len(seriesList) != 2 {
		t.Errorf("ListSeriesByStudy len = %d, want 2", len(seriesList))
	}

	studies, err := s.ListStudies("", 0)
	if err != nil {
		t.Fatalf("list studies: %v", err)
	}
	if len(studies) != 1 {
		t.Errorf("ListStudies len = %d, want 1", len(studies))
	}

	// Status index round-trip.
	if err := s.SetStudyStatus(studyUID, StatusQueued); err != nil {
		t.Fatalf("set status: %v", err)
	}
	queued, err := s.ListStudies(StatusQueued, 0)
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(queued) != 1 || queued[0].StudyInstanceUID != studyUID {
		t.Errorf("ListStudies(queued) = %v, want [%s]", queued, studyUID)
	}
	received, err := s.ListStudies(StatusReceived, 0)
	if err != nil {
		t.Fatalf("list received: %v", err)
	}
	if len(received) != 0 {
		t.Errorf("ListStudies(received) len = %d, want 0 after status change", len(received))
	}
}

// TestQueueFIFO verifies queue entries pop in NotBefore order.
func TestQueueFIFO(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now()
	entries := []QueueEntry{
		{ID: "c", ResourceLevel: "series", ResourceUID: "s.c", CreatedAt: now, NotBefore: now.Add(2 * time.Second)},
		{ID: "a", ResourceLevel: "series", ResourceUID: "s.a", CreatedAt: now, NotBefore: now},
		{ID: "b", ResourceLevel: "series", ResourceUID: "s.b", CreatedAt: now, NotBefore: now.Add(1 * time.Second)},
	}
	for _, e := range entries {
		if err := s.Enqueue(e); err != nil {
			t.Fatalf("enqueue %s: %v", e.ID, err)
		}
	}

	// Pop in order: a (now), b (+1s), c (+2s)
	want := []string{"a", "b", "c"}
	for i, w := range want {
		// Advance "now" past the entry's NotBefore.
		got, err := s.PopReadyQueueEntry(now.Add(3 * time.Second))
		if err != nil {
			t.Fatalf("pop[%d]: %v", i, err)
		}
		if got.ID != w {
			t.Errorf("pop[%d] = %q, want %q", i, got.ID, w)
		}
	}

	// Queue should be empty now.
	if _, err := s.PopReadyQueueEntry(now.Add(time.Hour)); err != ErrNotFound {
		t.Errorf("expected ErrNotFound on empty queue, got %v", err)
	}
}
