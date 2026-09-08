package transfer

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// TestPushSeriesAgainstStubOrthanc spins up an httptest server that
// emulates the Orthanc /instances endpoint, pushes a series, and verifies
// the receiver got each instance with correct auth.
func TestPushSeriesAgainstStubOrthanc(t *testing.T) {
	_ = log.Init(t.TempDir(), "error")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Stage three on-disk instance files.
	const (
		studyUID  = "1.2.3.study.A"
		seriesUID = "1.2.3.series.A"
	)
	seriesDir := filepath.Join(dir, "instances", studyUID, seriesUID)
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := 0; i < 3; i++ {
		sopUID := fmt.Sprintf("%s.inst.%d", seriesUID, i+1)
		path := filepath.Join(seriesDir, sopUID+".dcm")
		body := []byte(fmt.Sprintf("DUMMY-DICOM-INSTANCE-%d", i+1))
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		rec := store.InstanceRecord{
			SOPInstanceUID:    sopUID,
			SeriesInstanceUID: seriesUID,
			StudyInstanceUID:  studyUID,
			FilePath:          path,
			FileSize:          int64(len(body)),
			ReceivedAt:        time.Now(),
		}
		if err := st.PutInstance(rec, store.SeriesMetaUpdate{Modality: "CT"}, store.StudyMetaUpdate{PatientName: "TEST^USER"}); err != nil {
			t.Fatalf("put instance: %v", err)
		}
	}

	// Stub Orthanc.
	var hits int32
	var lastAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instances" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		lastAuth = r.Header.Get("Authorization")
		atomic.AddInt32(&hits, 1)
		// Drain the body so the connection can be reused.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ID":"x","Status":"Success"}`))
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		PeerURL:  srv.URL,
		Username: "alice",
		Password: "secret",
		Timeout:  5 * time.Second,
		Protocol: ProtocolInstances,
	}

	if err := c.PushSeries(context.Background(), st, seriesUID); err != nil {
		t.Fatalf("PushSeries: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("POST count = %d, want 3", got)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if lastAuth != wantAuth {
		t.Errorf("auth header = %q, want %q", lastAuth, wantAuth)
	}

	// Series should be Delivered.
	ser, _ := st.GetSeries(seriesUID)
	if ser.Status != store.StatusDelivered {
		t.Errorf("series status = %q, want delivered", ser.Status)
	}
	stu, _ := st.GetStudy(studyUID)
	if stu.Status != store.StatusDelivered {
		t.Errorf("study status = %q, want delivered (single-series study)", stu.Status)
	}
}

// TestPushSeriesPropagatesPeerError verifies a 5xx from the peer surfaces
// as an error so the queue worker can retry.
func TestPushSeriesPropagatesPeerError(t *testing.T) {
	_ = log.Init(t.TempDir(), "error")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const seriesUID = "1.2.3.series.B"
	const studyUID = "1.2.3.study.B"
	seriesDir := filepath.Join(dir, "instances", studyUID, seriesUID)
	_ = os.MkdirAll(seriesDir, 0o755)
	path := filepath.Join(seriesDir, "i.dcm")
	_ = os.WriteFile(path, []byte("X"), 0o644)
	if err := st.PutInstance(store.InstanceRecord{
		SOPInstanceUID:    "i",
		SeriesInstanceUID: seriesUID,
		StudyInstanceUID:  studyUID,
		FilePath:          path,
		FileSize:          1,
		ReceivedAt:        time.Now(),
	}, store.SeriesMetaUpdate{}, store.StudyMetaUpdate{}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := &Client{PeerURL: srv.URL, Timeout: 2 * time.Second, Protocol: ProtocolInstances}
	err = c.PushSeries(context.Background(), st, seriesUID)
	if err == nil {
		t.Fatal("expected error from 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want containing 500", err)
	}
}

// TestPushSeriesViaSTOW verifies the STOW-RS multipart path against a stub
// DICOMweb endpoint. This is the new default protocol (M4.5).
func TestPushSeriesViaSTOW(t *testing.T) {
	_ = log.Init(t.TempDir(), "error")
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		studyUID  = "1.2.3.stow.study.A"
		seriesUID = "1.2.3.stow.series.A"
	)
	seriesDir := filepath.Join(dir, "instances", studyUID, seriesUID)
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const numInstances = 4
	for i := 0; i < numInstances; i++ {
		sopUID := fmt.Sprintf("%s.inst.%d", seriesUID, i+1)
		path := filepath.Join(seriesDir, sopUID+".dcm")
		body := []byte(fmt.Sprintf("STOW-DICOM-INSTANCE-%d-payload-bytes-here", i+1))
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		rec := store.InstanceRecord{
			SOPInstanceUID:    sopUID,
			SeriesInstanceUID: seriesUID,
			StudyInstanceUID:  studyUID,
			FilePath:          path,
			FileSize:          int64(len(body)),
			ReceivedAt:        time.Now(),
		}
		if err := st.PutInstance(rec, store.SeriesMetaUpdate{Modality: "CT"}, store.StudyMetaUpdate{}); err != nil {
			t.Fatal(err)
		}
	}

	// Stub DICOMweb STOW-RS endpoint.
	var hits int32
	var partsReceived int32
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dicom-web/studies" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		receivedAuth = r.Header.Get("Authorization")

		// The request must be multipart/related with our application/dicom parts.
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/related") {
			http.Error(w, "wrong content-type: "+ct, http.StatusBadRequest)
			return
		}
		mr, err := multipartReader(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if got := part.Header.Get("Content-Type"); got != "application/dicom" {
				http.Error(w, "wrong part content-type: "+got, http.StatusBadRequest)
				return
			}
			if _, err := io.Copy(io.Discard, part); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			atomic.AddInt32(&partsReceived, 1)
		}

		// 200 OK with empty manifest = full success.
		w.Header().Set("Content-Type", "application/dicom+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		PeerURL:  srv.URL,
		Username: "alice",
		Password: "secret",
		Timeout:  5 * time.Second,
		Protocol: ProtocolSTOWRS,
	}
	if err := c.PushSeries(context.Background(), st, seriesUID); err != nil {
		t.Fatalf("PushSeries: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("STOW request count = %d, want 1 (whole series in one request)", got)
	}
	if got := atomic.LoadInt32(&partsReceived); got != numInstances {
		t.Errorf("parts received = %d, want %d", got, numInstances)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if receivedAuth != wantAuth {
		t.Errorf("auth = %q, want %q", receivedAuth, wantAuth)
	}

	ser, _ := st.GetSeries(seriesUID)
	if ser.Status != store.StatusDelivered {
		t.Errorf("series status = %q, want delivered", ser.Status)
	}
	stu, _ := st.GetStudy(studyUID)
	if stu.Status != store.StatusDelivered {
		t.Errorf("study status = %q, want delivered", stu.Status)
	}
}

// TestPushSeriesViaSTOWFailedManifest verifies that a 202 with a non-empty
// FailedSOPSequence triggers an error (so the queue worker can retry).
func TestPushSeriesViaSTOWFailedManifest(t *testing.T) {
	_ = log.Init(t.TempDir(), "error")
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "test.db"))
	t.Cleanup(func() { _ = st.Close() })

	const seriesUID = "1.2.3.stow.fail.series"
	const studyUID = "1.2.3.stow.fail.study"
	seriesDir := filepath.Join(dir, "instances", studyUID, seriesUID)
	_ = os.MkdirAll(seriesDir, 0o755)
	path := filepath.Join(seriesDir, "i.dcm")
	_ = os.WriteFile(path, []byte("payload"), 0o644)
	_ = st.PutInstance(store.InstanceRecord{
		SOPInstanceUID:    "i",
		SeriesInstanceUID: seriesUID,
		StudyInstanceUID:  studyUID,
		FilePath:          path,
		FileSize:          7,
		ReceivedAt:        time.Now(),
	}, store.SeriesMetaUpdate{}, store.StudyMetaUpdate{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so we can return.
		mr, _ := multipartReader(r)
		if mr != nil {
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				_, _ = io.Copy(io.Discard, p)
			}
		}
		w.Header().Set("Content-Type", "application/dicom+json")
		w.WriteHeader(http.StatusAccepted)
		// FailedSOPSequence with one item = 1 instance failed.
		_, _ = w.Write([]byte(`{"00081198":{"vr":"SQ","Value":[{"00081150":{"vr":"UI","Value":["1.2.3"]}}]}}`))
	}))
	t.Cleanup(srv.Close)

	c := &Client{PeerURL: srv.URL, Timeout: 2 * time.Second, Protocol: ProtocolSTOWRS}
	err := c.PushSeries(context.Background(), st, seriesUID)
	if err == nil {
		t.Fatal("expected error from non-empty FailedSOPSequence")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("err = %v, want containing 'failed'", err)
	}
}

// multipartReader builds a multipart.Reader from the request, parsing the
// boundary out of the Content-Type header.
func multipartReader(r *http.Request) (*mpReader, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("parse content-type: %w", err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("no boundary in Content-Type")
	}
	return &mpReader{mr: multipart.NewReader(r.Body, boundary)}, nil
}

type mpReader struct{ mr *multipart.Reader }

func (m *mpReader) NextPart() (*multipart.Part, error) {
	return m.mr.NextPart()
}
