package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// TestAPIEndpoints walks the major endpoints with a fresh store and a
// running API server. Auth is verified along the way.
func TestAPIEndpoints(t *testing.T) {
	_ = log.Init(t.TempDir(), "error")
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed two studies with one series each.
	const tokenValue = "test-token-abc"
	studies := []struct {
		studyUID, seriesUID, sopUID string
		modality                    string
	}{
		{"1.2.A.study", "1.2.A.series", "1.2.A.inst", "CT"},
		{"1.2.B.study", "1.2.B.series", "1.2.B.inst", "MR"},
	}
	for _, s := range studies {
		rec := store.InstanceRecord{
			SOPInstanceUID:    s.sopUID,
			SeriesInstanceUID: s.seriesUID,
			StudyInstanceUID:  s.studyUID,
			FileSize:          1024,
			ReceivedAt:        time.Now(),
		}
		if err := st.PutInstance(rec,
			store.SeriesMetaUpdate{Modality: s.modality},
			store.StudyMetaUpdate{PatientID: "P-" + s.studyUID, PatientName: "TEST"},
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	_ = st.SetStudyStatus("1.2.B.study", store.StatusFailed)

	// Start API server on ephemeral port.
	srv := &Server{
		Addr:      "127.0.0.1:0",
		APIToken:  tokenValue,
		Store:     st,
		DataDir:   t.TempDir(),
		Version:   "test",
		LabID:     "UJJ1",
		OrgID:     "UJJ",
		DICOMPort: 1007,
		HTTPPort:  9042,
		PeerName:  "BHARATPACS",
		PeerURL:   "http://example.com:8042",
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Stop(ctx)
	})

	addr := srv.listener.Addr().String()
	base := "http://" + addr

	// /api/health: unauthenticated, always 200.
	{
		resp, err := http.Get(base + "/api/health")
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("health status = %d, want 200", resp.StatusCode)
		}
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got["status"] != "ok" {
			t.Errorf("health body status = %v, want ok", got["status"])
		}
	}

	// /api/status with no token: 401.
	{
		resp, err := http.Get(base + "/api/status")
		if err != nil {
			t.Fatalf("status no-auth: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status no-auth = %d, want 401", resp.StatusCode)
		}
	}

	// /api/status with bad token: 401.
	{
		resp := authReq(t, "GET", base+"/api/status", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status bad-auth = %d, want 401", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// /api/status with good token: 200, expected fields.
	{
		resp := authReq(t, "GET", base+"/api/status", tokenValue)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status auth = %d body=%s", resp.StatusCode, body)
		}
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got["lab_id"] != "UJJ1" {
			t.Errorf("status.lab_id = %v, want UJJ1", got["lab_id"])
		}
		if got["peer_name"] != "BHARATPACS" {
			t.Errorf("status.peer_name = %v, want BHARATPACS", got["peer_name"])
		}
	}

	// /api/worklist (no filter): both studies.
	{
		resp := authReq(t, "GET", base+"/api/worklist", tokenValue)
		defer resp.Body.Close()
		var got struct {
			Studies []store.StudyRecord `json:"studies"`
			Count   int                 `json:"count"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got.Count != 2 {
			t.Errorf("worklist count = %d, want 2", got.Count)
		}
	}

	// /api/worklist?status=failed: only the B study.
	{
		resp := authReq(t, "GET", base+"/api/worklist?status=failed", tokenValue)
		defer resp.Body.Close()
		var got struct {
			Studies []store.StudyRecord `json:"studies"`
			Count   int                 `json:"count"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got.Count != 1 || got.Studies[0].StudyInstanceUID != "1.2.B.study" {
			t.Errorf("worklist failed = %+v, want only 1.2.B.study", got)
		}
	}

	// /api/study/:uid: returns study + series.
	{
		resp := authReq(t, "GET", base+"/api/study/1.2.A.study", tokenValue)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("study detail status = %d", resp.StatusCode)
		}
		var got struct {
			Study  store.StudyRecord    `json:"study"`
			Series []store.SeriesRecord `json:"series"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got.Study.StudyInstanceUID != "1.2.A.study" {
			t.Errorf("study uid = %q", got.Study.StudyInstanceUID)
		}
		if len(got.Series) != 1 {
			t.Errorf("series count = %d, want 1", len(got.Series))
		}
	}

	// /api/study/:uid not found: 404.
	{
		resp := authReq(t, "GET", base+"/api/study/does.not.exist", tokenValue)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("missing study = %d, want 404", resp.StatusCode)
		}
	}

	// /api/study/:uid/retry: re-enqueues failed series.
	{
		resp := authReq(t, "POST", base+"/api/study/1.2.B.study/retry", tokenValue)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("retry status = %d body=%s", resp.StatusCode, body)
		}
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got["series_requeued"] != float64(1) {
			t.Errorf("series_requeued = %v, want 1", got["series_requeued"])
		}
		// Queue depth should now be 1.
		depth, _ := st.QueueDepth()
		if depth != 1 {
			t.Errorf("queue depth after retry = %d, want 1", depth)
		}
	}

	// /api/queue/depth: 1.
	{
		resp := authReq(t, "GET", base+"/api/queue/depth", tokenValue)
		defer resp.Body.Close()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		if got["depth"] != float64(1) {
			t.Errorf("queue depth endpoint = %v, want 1", got["depth"])
		}
	}

	// /api/study/:uid DELETE: force-delete now wired (M7).
	{
		resp := authReq(t, "DELETE", base+"/api/study/1.2.A.study", tokenValue)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("delete = %d, want 200; body=%s", resp.StatusCode, body)
		}
		// Verify the study is actually gone.
		if _, err := st.GetStudy("1.2.A.study"); err == nil {
			t.Errorf("study should be deleted from store after DELETE")
		}
	}
}

func authReq(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(""))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	return resp
}
