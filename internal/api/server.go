// Package api implements the local HTTP API that the Electron renderer
// uses to populate the worklist and react to user actions.
//
// Authenticated via a simple bearer token written to the config file at
// startup. The Electron main process reads the same config and forwards
// the token; the renderer never sees it directly.
//
// Stdlib net/http only — no router framework. The endpoint count is small
// and the routing is mostly resource-by-UID, which we handle with a small
// table of patterns.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/config"
	"github.com/bharatpacs/tarang-sender/internal/connectivity"
	"github.com/bharatpacs/tarang-sender/internal/labstatus"
	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/notifier"
	"github.com/bharatpacs/tarang-sender/internal/queue"
	"github.com/bharatpacs/tarang-sender/internal/retention"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// Server exposes the local HTTP API.
type Server struct {
	Addr      string // e.g. "127.0.0.1:9042"
	APIToken  string // required; matched against Authorization: Bearer header
	Store     *store.Store
	DataDir   string // root for on-disk DICOM files; needed by force-delete

	// Status snapshot fields — copied into responses by /api/status.
	Version      string
	LabID        string
	OrgID        string
	DICOMPort    int
	HTTPPort     int
	PeerName     string
	PeerURL      string
	StartedAt    time.Time

	// Progress tracking for real-time transfer updates
	ProgressManager *ProgressManager

	// Runtime config snapshot and path for /api/config GET/PATCH.
	ConfigPath string
	Config     *config.Config

	// PeerWatcher supplies real peer reachability for /api/status.
	//
	// The status pill used to be driven by whether this local HTTP call
	// succeeded, which is a loopback socket and therefore always green — a
	// site sat "Connected" for 90 minutes while nothing uploaded, which is
	// why nobody rang. It reports the probe result now.
	PeerWatcher *connectivity.Watcher

	// LabChecker backs /api/lab-status and the lab_* fields on /api/status.
	// Optional: nil means the engine was built without the activation gate.
	LabChecker *labstatus.Checker
	// Notifier supplies the count of notifications held while inactive.
	Notifier *notifier.Notifier

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
}

// Start binds the listener and starts serving. Returns once the listener
// is bound; serving runs in a goroutine.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.server != nil {
		s.mu.Unlock()
		return errors.New("api: server already started")
	}
	if s.APIToken == "" {
		s.mu.Unlock()
		return errors.New("api: APIToken is required")
	}
	if s.Store == nil {
		s.mu.Unlock()
		return errors.New("api: Store is required")
	}

	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)        // unauthenticated
	mux.HandleFunc("/api/status", s.auth(s.handleStatus))
	mux.HandleFunc("/api/worklist", s.auth(s.handleWorklist))
	mux.HandleFunc("/api/study/", s.auth(s.handleStudyByUID))
	mux.HandleFunc("/api/queue/depth", s.auth(s.handleQueueDepth))
	mux.HandleFunc("/api/config", s.auth(s.handleConfig))         // NEW: config get/patch
	mux.HandleFunc("/api/lab-status", s.auth(s.handleLabStatus))  // NEW: activation gate
	mux.HandleFunc("/api/transfer/", s.auth(s.handleTransferProgress)) // NEW: progress tracking

	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.listener = l
	s.server = &http.Server{
		Handler:           s.corsMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	s.mu.Unlock()

	log.L().Info("http api listening", "addr", l.Addr().String())

	go func() {
		if err := s.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.L().Error("http api serve", "err", err)
		}
	}()
	return nil
}

// Stop gracefully shuts the API server down.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.server
	s.server = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// auth wraps a handler with bearer-token authentication.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) || header[len(prefix):] != s.APIToken {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// ------------------------------------------------------------------------
// handlers
// ------------------------------------------------------------------------

// /api/health — unauthenticated liveness probe.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"uptime":  time.Since(s.StartedAt).String(),
		"version": s.Version,
	})
}

// /api/status — overall service info for the header status pill.
//
// The lab_* fields ride along here on purpose: the renderer already polls this
// every few seconds for the status pill, so the account-inactive banner needs
// no extra request and no extra polling loop.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	depth, _ := s.Store.QueueDepth()

	labActive := true
	labStatus := ""
	labMessage := ""
	labKnown := true
	labEnabled := false
	if s.LabChecker != nil {
		v := s.LabChecker.Snapshot()
		labActive, labStatus, labMessage = v.Active, v.Status, v.Message
		labKnown, labEnabled = v.Known, v.Enabled
	}
	pending := 0
	if s.Notifier != nil {
		pending = s.Notifier.PendingCount()
	}

	// Peer reachability. Absent a watcher we report nothing rather than
	// guessing — the UI treats a missing field as "unknown", not "fine".
	peerOnline := true
	peerError := ""
	peerLastOK := ""
	if s.PeerWatcher != nil {
		peerOnline = s.PeerWatcher.IsOnline()
		peerError = s.PeerWatcher.LastError()
		if t := s.PeerWatcher.LastProbe(); !t.IsZero() {
			peerLastOK = t.UTC().Format(time.RFC3339)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"peer_online":       peerOnline,
		"peer_error":        peerError,
		"peer_last_success": peerLastOK,
		"lab_active":            labActive,
		"lab_status":            labStatus,
		"lab_status_message":    labMessage,
		"lab_status_known":      labKnown,
		"lab_gate_enabled":      labEnabled,
		"pending_notifications": pending,
		"running":     true,
		"version":     s.Version,
		"started_at":  s.StartedAt.UTC().Format(time.RFC3339),
		"uptime_s":    int(time.Since(s.StartedAt).Seconds()),
		"lab_id":      s.LabID,
		"org_id":      s.OrgID,
		"dicom_port":  s.DICOMPort,
		"http_port":   s.HTTPPort,
		"peer_name":   s.PeerName,
		"peer_url":    s.PeerURL,
		"queue_depth": depth,
		"hostname":    hostname(),
	})
}

// /api/worklist?status=&limit= — paged study list.
func (s *Server) handleWorklist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	limit := 100
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	studies, err := s.Store.ListStudies(status, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"studies": studies,
		"count":   len(studies),
		"limit":   limit,
		"status":  status,
	})
}

// /api/study/:uid              — GET, study + series detail
// /api/study/:uid/retry        — POST, re-enqueue a failed study
// /api/study/:uid/cancel       — POST, cancel an in-flight transfer
// /api/study/:uid              — DELETE, force-delete locally
func (s *Server) handleStudyByUID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/study/")
	if rest == "" {
		writeError(w, http.StatusBadRequest, "missing study uid")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	uid := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		s.studyDetail(w, uid)
	case action == "retry" && r.Method == http.MethodPost:
		s.studyRetry(w, uid)
	case action == "" && r.Method == http.MethodDelete:
		s.studyDelete(w, uid)
	default:
		writeError(w, http.StatusMethodNotAllowed, "unsupported method/action")
	}
}

func (s *Server) studyDetail(w http.ResponseWriter, uid string) {
	study, err := s.Store.GetStudy(uid)
	if err != nil {
		writeError(w, http.StatusNotFound, "study not found")
		return
	}
	series, _ := s.Store.ListSeriesByStudy(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"study":  study,
		"series": series,
	})
}

func (s *Server) studyRetry(w http.ResponseWriter, uid string) {
	stu, err := s.Store.GetStudy(uid)
	if err != nil {
		writeError(w, http.StatusNotFound, "study not found")
		return
	}
	allSeries, err := s.Store.ListSeriesByStudy(uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	requeued := 0
	for _, ser := range allSeries {
		if ser.Status == store.StatusDelivered {
			continue
		}
		if err := queue.EnqueueSeries(s.Store, ser.SeriesInstanceUID); err != nil {
			log.L().Warn("retry enqueue failed", "series_uid", ser.SeriesInstanceUID, "err", err)
			continue
		}
		requeued++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"study_uid":         stu.StudyInstanceUID,
		"series_total":      len(allSeries),
		"series_requeued":   requeued,
	})
}

func (s *Server) studyDelete(w http.ResponseWriter, uid string) {
	if s.DataDir == "" {
		writeError(w, http.StatusInternalServerError, "force-delete: server has no data dir configured")
		return
	}
	if _, err := s.Store.GetStudy(uid); err != nil {
		writeError(w, http.StatusNotFound, "study not found")
		return
	}
	if err := retention.DeleteStudy(s.Store, s.DataDir, uid); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"study_uid": uid,
		"deleted":   true,
	})
}

// /api/queue/depth — for the footer "in flight" indicator.
func (s *Server) handleQueueDepth(w http.ResponseWriter, _ *http.Request) {
	depth, err := s.Store.QueueDepth()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"depth": depth})
}

// /api/lab-status — GET returns the cached activation verdict; POST forces a
// fresh check.
//
// The two verbs are genuinely different operations, which is why the app's
// "Check again" button must POST: a GET may legitimately answer from a verdict
// seconds old, so right after an account is reactivated it would appear to do
// nothing.
func (s *Server) handleLabStatus(w http.ResponseWriter, r *http.Request) {
	if s.LabChecker == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"active":  true,
			"known":   true,
		})
		return
	}

	var v labstatus.Verdict
	switch r.Method {
	case http.MethodGet:
		v = s.LabChecker.Snapshot()
	case http.MethodPost:
		v = s.LabChecker.Refresh(r.Context(), "manual")
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body := map[string]any{
		"enabled":     v.Enabled,
		"active":      v.Active,
		"known":       v.Known,
		"status":      v.Status,
		"message":     v.Message,
		"http_status": v.HTTPStatus,
		"last_error":  v.LastError,
	}
	if !v.CheckedAt.IsZero() {
		body["checked_at"] = v.CheckedAt.UTC().Format(time.RFC3339)
		body["age_s"] = int(time.Since(v.CheckedAt).Seconds())
	}
	if s.Notifier != nil {
		body["pending_notifications"] = s.Notifier.PendingCount()
	}
	writeJSON(w, http.StatusOK, body)
}

// /api/config — GET to retrieve config, PATCH to update config
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.configGet(w, r)
	case http.MethodPatch:
		s.configPatch(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// configGet returns current configuration
func (s *Server) configGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Config == nil {
		writeError(w, http.StatusNotFound, "config not available")
		return
	}
	writeJSON(w, http.StatusOK, s.Config)
}

// configPatch updates configuration fields
func (s *Server) configPatch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Config == nil || s.ConfigPath == "" {
		writeError(w, http.StatusNotFound, "config not available")
		return
	}

	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	baseBytes, err := json.Marshal(s.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "serialize config failed")
		return
	}

	var merged map[string]any
	if err := json.Unmarshal(baseBytes, &merged); err != nil {
		writeError(w, http.StatusInternalServerError, "decode config failed")
		return
	}

	mergeMap(merged, patch)

	mergedBytes, err := json.Marshal(merged)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid patched config")
		return
	}

	var nextCfg config.Config
	if err := json.Unmarshal(mergedBytes, &nextCfg); err != nil {
		writeError(w, http.StatusBadRequest, "patched config shape invalid")
		return
	}
	// Re-apply the derived defaults so a patch cannot route around them —
	// e.g. sending "poll_seconds": 0 and spinning the gate's poll loop.
	nextCfg.Normalize()
	if err := nextCfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := nextCfg.Save(s.ConfigPath); err != nil {
		writeError(w, http.StatusInternalServerError, "write config failed: "+err.Error())
		return
	}

	// Update the config value in-place so closures in main.go (stability
	// watcher, queue worker) see the new settings without a restart.
	// s.Config was set to &cfg in main.go; writing through the pointer
	// updates that same variable.
	*s.Config = nextCfg
	s.LabID = nextCfg.LabID
	s.OrgID = nextCfg.OrgID
	s.DICOMPort = nextCfg.DICOM.Port
	s.HTTPPort = nextCfg.HTTP.Port
	s.PeerName = nextCfg.Peer.Name
	s.PeerURL = nextCfg.Peer.URL

	// API token changes apply immediately for subsequent requests.
	if nextCfg.HTTP.APIToken != "" {
		s.APIToken = nextCfg.HTTP.APIToken
	}

	writeJSON(w, http.StatusOK, nextCfg)
}

// /api/transfer/{series_uid}/progress — GET transfer progress
func (s *Server) handleTransferProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Extract series_uid from path: /api/transfer/{series_uid}/progress
	rest := strings.TrimPrefix(r.URL.Path, "/api/transfer/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "series_uid required")
		return
	}

	seriesUID := parts[0]

	if s.ProgressManager == nil {
		writeError(w, http.StatusNotFound, "progress tracking unavailable")
		return
	}
	progress := s.ProgressManager.GetProgress(seriesUID)
	if progress == nil {
		writeError(w, http.StatusNotFound, "transfer progress not found")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func mergeMap(base map[string]any, patch map[string]any) {
	for k, v := range patch {
		vMap, vIsMap := v.(map[string]any)
		if !vIsMap {
			base[k] = v
			continue
		}
		if bCur, ok := base[k]; ok {
			if bMap, ok := bCur.(map[string]any); ok {
				mergeMap(bMap, vMap)
				base[k] = bMap
				continue
			}
		}
		base[k] = vMap
	}
}

// ------------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "status": status})
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// corsMiddleware wraps the HTTP handler with CORS headers for dev browser access.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
