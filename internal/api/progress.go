package api

import (
	"sync"
	"time"
)

// ProgressState tracks real-time transfer progress for a series
type ProgressState struct {
	SeriesUID       string
	StudyUID        string
	Status          string // "sending", "complete", "failed"
	BytesSent       int64
	TotalBytes      int64
	InstancesSent   int
	TotalInstances  int
	StartTime       time.Time
	Error           string
	LastUpdated     time.Time
}

// ProgressManager maintains in-memory progress tracking for transfers
type ProgressManager struct {
	mu          sync.RWMutex
	transfers   map[string]*ProgressState
	cleanupTime time.Duration
}

// NewProgressManager creates a new progress manager
func NewProgressManager() *ProgressManager {
	pm := &ProgressManager{
		transfers:   make(map[string]*ProgressState),
		cleanupTime: 10 * time.Minute, // Clean up completed transfers after 10 min
	}
	go pm.cleanupExpired()
	return pm
}

// Start initializes progress tracking for a series
func (pm *ProgressManager) Start(seriesUID, studyUID string, totalBytes int64, totalInstances int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.transfers[seriesUID] = &ProgressState{
		SeriesUID:      seriesUID,
		StudyUID:       studyUID,
		Status:         "sending",
		TotalBytes:     totalBytes,
		TotalInstances: totalInstances,
		StartTime:      time.Now(),
		LastUpdated:    time.Now(),
	}
}

// Update reports progress
func (pm *ProgressManager) Update(seriesUID string, bytesSent int64, instancesSent int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if ps, ok := pm.transfers[seriesUID]; ok {
		ps.BytesSent = bytesSent
		ps.InstancesSent = instancesSent
		ps.LastUpdated = time.Now()
	}
}

// Complete marks a transfer as complete
func (pm *ProgressManager) Complete(seriesUID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if ps, ok := pm.transfers[seriesUID]; ok {
		ps.Status = "complete"
		ps.BytesSent = ps.TotalBytes
		ps.InstancesSent = ps.TotalInstances
		ps.LastUpdated = time.Now()
	}
}

// Fail marks a transfer as failed
func (pm *ProgressManager) Fail(seriesUID string, errMsg string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if ps, ok := pm.transfers[seriesUID]; ok {
		ps.Status = "failed"
		ps.Error = errMsg
		ps.LastUpdated = time.Now()
	}
}

// Get retrieves current progress
func (pm *ProgressManager) Get(seriesUID string) *ProgressState {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	if ps, ok := pm.transfers[seriesUID]; ok {
		// Return a copy
		cpy := *ps
		return &cpy
	}
	return nil
}

// GetProgress returns progress as API response format
func (pm *ProgressManager) GetProgress(seriesUID string) map[string]any {
	ps := pm.Get(seriesUID)
	if ps == nil {
		return nil
	}

	percent := 0
	if ps.TotalBytes > 0 {
		percent = int((ps.BytesSent * 100) / ps.TotalBytes)
	}

	elapsed := time.Since(ps.StartTime).Seconds()
	var eta int
	if elapsed > 0 && ps.TotalBytes > 0 {
		speed := float64(ps.BytesSent) / elapsed
		if speed > 0 {
			remaining := float64(ps.TotalBytes - ps.BytesSent)
			eta = int(remaining / speed)
		}
	}

	return map[string]any{
		"status":             ps.Status,
		"series_uid":         ps.SeriesUID,
		"study_uid":          ps.StudyUID,
		"bytes_sent":         ps.BytesSent,
		"total_bytes":        ps.TotalBytes,
		"instances_sent":     ps.InstancesSent,
		"total_instances":    ps.TotalInstances,
		"percent":            percent,
		"elapsed_s":          int(elapsed),
		"eta_s":              eta,
		"error":              ps.Error,
	}
}

// cleanupExpired removes old completed transfers from memory
func (pm *ProgressManager) cleanupExpired() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		pm.mu.Lock()
		now := time.Now()
		for uid, ps := range pm.transfers {
			if (ps.Status == "complete" || ps.Status == "failed") &&
				now.Sub(ps.LastUpdated) > pm.cleanupTime {
				delete(pm.transfers, uid)
			}
		}
		pm.mu.Unlock()
	}
}
