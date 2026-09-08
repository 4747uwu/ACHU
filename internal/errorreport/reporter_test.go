package errorreport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
)

// drainSpool waits until the spool directory has no .json files, or the
// deadline passes. Returns the number of files remaining.
func drainSpool(dir string, deadline time.Duration) int {
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		n := countSpool(dir)
		if n == 0 {
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	return countSpool(dir)
}

func countSpool(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			n++
		}
	}
	return n
}

func TestReportSpoolsAndSends(t *testing.T) {
	var (
		mu       sync.Mutex
		received []Report
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		received = append(received, rep)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	dir := t.TempDir()
	r := New(srv.URL, "", "9.9.9", dir,
		DeviceInfo{Hostname: "TEST-PC", OS: "windows"},
		LabInfo{LabID: "LAB1", OrgID: "ORG1", PeerName: "BHARATPACS"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	r.Report(log.ErrorEvent{
		Time:    time.Now(),
		Level:   "ERROR",
		Message: "disk full",
		Attrs:   map[string]any{"path": "C:/data", "code": 28},
	})

	if n := drainSpool(filepath.Join(dir, "error-reports"), 2*time.Second); n != 0 {
		t.Fatalf("spool not drained, %d files remain", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("want 1 report received, got %d", len(received))
	}
	got := received[0]
	if got.Message != "disk full" || got.Lab.LabID != "LAB1" || got.Device.Hostname != "TEST-PC" {
		t.Fatalf("payload not enriched correctly: %+v", got)
	}
	if got.Attrs["code"].(float64) != 28 {
		t.Fatalf("attrs not preserved: %+v", got.Attrs)
	}
	if got.ID == "" || got.Source != "tarang-sender" || got.AppVersion != "9.9.9" {
		t.Fatalf("metadata missing: %+v", got)
	}
}

func TestReportDedupesWithinCooldown(t *testing.T) {
	dir := t.TempDir()
	// Empty backend URL → never sends, so spooled files stay put and we can
	// count exactly how many distinct reports were written.
	r := New("", "", "1.0.0", dir, DeviceInfo{}, LabInfo{})

	ev := log.ErrorEvent{Time: time.Now(), Level: "ERROR", Message: "same error"}
	for i := 0; i < 5; i++ {
		r.Report(ev)
	}
	// A distinct message should not be collapsed.
	r.Report(log.ErrorEvent{Time: time.Now(), Level: "ERROR", Message: "other error"})

	if n := countSpool(filepath.Join(dir, "error-reports")); n != 2 {
		t.Fatalf("want 2 spooled reports after dedupe, got %d", n)
	}
}

func TestStartDrainsPreexistingSpool(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	spool := filepath.Join(dir, "error-reports")
	if err := os.MkdirAll(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a report left behind by a previous run.
	leftover := Report{ID: "00000000000000000001-000001", Message: "old", Source: "tarang-sender"}
	b, _ := json.Marshal(leftover)
	if err := os.WriteFile(filepath.Join(spool, leftover.ID+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(srv.URL, "", "1.0.0", dir, DeviceInfo{}, LabInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	if n := drainSpool(spool, 2*time.Second); n != 0 {
		t.Fatalf("preexisting spool not drained, %d remain", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("want 1 send of leftover report, got %d", hits)
	}
}
