package transfer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/notifier"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// TestStudyDeliveredFiresNotifier proves the exact runtime path used by the
// exe: PushSeries -> study marked Delivered -> OnStudyDelivered hook ->
// notifier.NotifyDelivered -> POST to the derived /delivered endpoint, with
// the correct deterministic Orthanc study ID in the body.
func TestStudyDeliveredFiresNotifier(t *testing.T) {
	// Use a non-managed temp dir for logs: the logger keeps the file open,
	// which would otherwise trip t.TempDir's RemoveAll cleanup on Windows.
	logDir, _ := os.MkdirTemp("", "deliv-log")
	_ = log.Init(logDir, "error")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		studyUID  = "1.2.3.study.DELIVERED"
		seriesUID = "1.2.3.series.DELIVERED"
		patientID = "PID-42"
	)
	seriesDir := filepath.Join(dir, "instances", studyUID, seriesUID)
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sopUID := seriesUID + ".inst.1"
	path := filepath.Join(seriesDir, sopUID+".dcm")
	if err := os.WriteFile(path, []byte("DUMMY"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	rec := store.InstanceRecord{
		SOPInstanceUID:    sopUID,
		SeriesInstanceUID: seriesUID,
		StudyInstanceUID:  studyUID,
		FilePath:          path,
		FileSize:          5,
		ReceivedAt:        time.Now(),
	}
	if err := st.PutInstance(rec,
		store.SeriesMetaUpdate{Modality: "CT"},
		store.StudyMetaUpdate{PatientID: patientID, PatientName: "TEST^USER"},
	); err != nil {
		t.Fatalf("put instance: %v", err)
	}

	// Stub Orthanc peer (receives the push).
	orthanc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ID":"x","Status":"Success"}`))
	}))
	t.Cleanup(orthanc.Close)

	// Stub backend that must receive the delivered POST on /delivered.
	type result struct {
		path    string
		payload notifier.DeliveredPayload
	}
	got := make(chan result, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p notifier.DeliveredPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		w.WriteHeader(http.StatusOK)
		select {
		case got <- result{path: r.URL.Path, payload: p}:
		default:
		}
	}))
	t.Cleanup(backend.Close)

	// Notifier wired exactly as main.go does: backend_url ends in the
	// instance-exe-received segment; DeliveredURL is derived from it.
	n := notifier.New(backend.URL+"/api/orthanc2/instance-exe-received", "", "LAB1", "ORG1")
	if n.DeliveredURL != backend.URL+"/api/orthanc2/delivered" {
		t.Fatalf("DeliveredURL = %q, want %q", n.DeliveredURL, backend.URL+"/api/orthanc2/delivered")
	}

	c := &Client{
		PeerURL:  orthanc.URL,
		Timeout:  5 * time.Second,
		Protocol: ProtocolInstances,
		// Mirror main.go's hook (minus the 15s production delay).
		OnStudyDelivered: func(uid string) {
			stu, err := st.GetStudy(uid)
			if err != nil {
				t.Errorf("hook GetStudy: %v", err)
				return
			}
			n.NotifyDelivered(notifier.DeliveredPayload{
				StudyInstanceUID: stu.StudyInstanceUID,
				PatientID:        stu.PatientID,
			})
		},
	}

	if err := c.PushSeries(context.Background(), st, seriesUID); err != nil {
		t.Fatalf("PushSeries: %v", err)
	}

	select {
	case res := <-got:
		if res.path != "/api/orthanc2/delivered" {
			t.Errorf("POST path = %q, want /api/orthanc2/delivered", res.path)
		}
		if res.payload.Status != "delivered" {
			t.Errorf("status = %q, want delivered", res.payload.Status)
		}
		if res.payload.StudyInstanceUID != studyUID {
			t.Errorf("StudyInstanceUID = %q, want %q", res.payload.StudyInstanceUID, studyUID)
		}
		wantID := notifier.OrthancStudyID(patientID, studyUID)
		if res.payload.OrthancID != wantID {
			t.Errorf("OrthancID = %q, want %q", res.payload.OrthancID, wantID)
		}
		t.Logf("delivered POST received: path=%s orthanc_id=%s study=%s",
			res.path, res.payload.OrthancID, res.payload.StudyInstanceUID)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out: delivered notification never POSTed — hook did not fire")
	}
}
