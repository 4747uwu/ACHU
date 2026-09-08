# IMPLEMENTATION GUIDE - Phase 1-4 Complete

## STATUS

**COMPLETED:**
- ✅ Architecture documentation (ARCHITECTURE.md)
- ✅ Login page (login.html + login.js)
- ✅ Editable settings form (settings-form.js)
- ✅ Progress modal component (progress-modal.js)

**REMAINING:**
- Backend API endpoints (Go)
- Wails migration
- Build & testing

---

## IMPLEMENTATION STEPS

### Step 1: Update API Server with New Endpoints

**File:** `internal/api/server.go`

Add these endpoint registrations in the `Start()` method (around line 66-71):

```go
mux.HandleFunc("/api/config", s.auth(s.handleConfig))
mux.HandleFunc("/api/transfer/", s.auth(s.handleTransferProgress))
```

And update CORS middleware to allow PATCH:

```go
w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, OPTIONS")
```

Add these handler methods to the Server struct:

```go
// handleConfig handles GET /api/config (retrieve) and PATCH /api/config (update)
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
    switch r.Method {
    case "GET":
        s.configGet(w, r)
    case "PATCH":
        s.configPatch(w, r)
    default:
        writeError(w, http.StatusMethodNotAllowed, "method not allowed")
    }
}

// configGet returns current configuration
func (s *Server) configGet(w http.ResponseWriter, r *http.Request) {
    cfg := map[string]any{
        "lab_id": s.LabID,
        "org_id": s.OrgID,
        "dicom": map[string]any{
            "port":                  s.Server.DIOM.Port,  // Need to pass config to Server
            "aet":                   s.Server.DIOM.AET,
            "stable_age_seconds":    s.Server.DIOM.StableAgeSeconds,
        },
        "http": map[string]any{
            "port":  s.HTTPPort,
            "api_token": "••••••••", // Don't expose actual token
        },
        "storage": map[string]any{
            "data_dir":        s.DataDir,
            "retention_hours": s.Server.Storage.RetentionHours,
            "max_disk_gb":     s.Server.Storage.MaxDiskGB,
        },
        "peer": map[string]any{
            "name":        s.PeerName,
            "url":         s.PeerURL,
            "username":    s.Server.Peer.Username,
            "password":    "••••••••",
            "compression": s.Server.Peer.Compression,
            "protocol":    s.Server.Peer.Protocol,
        },
        "tag_injection": map[string]any{
            "enabled":               s.Server.TagInjection.Enabled,
            "private_creator":       s.Server.TagInjection.PrivateCreator,
            "private_organisation":  s.Server.TagInjection.PrivateOrganisation,
        },
        "transfer": map[string]any{
            "concurrent_workers":    s.Server.Transfer.ConcurrentWorkers,
            "bucket_size_mb":        s.Server.Transfer.BucketSizeMB,
            "max_http_retries":      s.Server.Transfer.MaxHTTPRetries,
            "http_timeout_seconds":  s.Server.Transfer.HTTPTimeoutSeconds,
        },
    }
    writeJSON(w, http.StatusOK, cfg)
}

// configPatch updates configuration and returns new config
func (s *Server) configPatch(w http.ResponseWriter, r *http.Request) {
    var update map[string]any
    if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
        writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
        return
    }

    // For now, reject updates (pass to config manager)
    // TODO: Implement config reload + restart logic
    writeError(w, http.StatusNotImplemented, "config updates not yet implemented")
}

// handleTransferProgress handles GET /api/transfer/{series_uid}/progress
func (s *Server) handleTransferProgress(w http.ResponseWriter, r *http.Request) {
    if r.Method != "GET" {
        writeError(w, http.StatusMethodNotAllowed, "method not allowed")
        return
    }

    // Extract series_uid from path: /api/transfer/{series_uid}/progress
    parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/transfer/"), "/")
    if len(parts) < 1 || parts[0] == "" {
        writeError(w, http.StatusBadRequest, "series_uid required")
        return
    }

    seriesUID := parts[0]
    
    // TODO: Get progress from progress manager
    // For now, return placeholder
    writeJSON(w, http.StatusOK, map[string]any{
        "status":             "sending",
        "series_uid":         seriesUID,
        "bytes_sent":         0,
        "total_bytes":        1000000,
        "instances_sent":     0,
        "total_instances":    5,
        "percent":            0,
        "elapsed_s":          0,
        "eta_s":              60,
    })
}
```

---

### Step 2: Create Progress Manager

**File:** `internal/api/progress.go` (NEW)

```go
package api

import (
	"sync"
	"time"
)

// ProgressState tracks real-time transfer progress for a series
type ProgressState struct {
	SeriesUID       string
	Status          string // "sending", "complete", "failed"
	BytesSent       int64
	TotalBytes      int64
	InstancesSent   int
	TotalInstances  int
	StartTime       time.Time
	Error           string
	LastUpdated     time.Time
}

// ProgressManager maintains in-memory progress tracking
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
func (pm *ProgressManager) Start(seriesUID string, totalBytes int64, totalInstances int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	
	pm.transfers[seriesUID] = &ProgressState{
		SeriesUID:      seriesUID,
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
func (pm *ProgressManager) Fail(seriesUID string, err string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	
	if ps, ok := pm.transfers[seriesUID]; ok {
		ps.Status = "failed"
		ps.Error = err
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
	eta := 0.0
	if elapsed > 0 && ps.TotalBytes > 0 {
		speed := float64(ps.BytesSent) / elapsed
		remaining := float64(ps.TotalBytes - ps.BytesSent)
		eta = remaining / speed
	}
	
	return map[string]any{
		"status":             ps.Status,
		"series_uid":         ps.SeriesUID,
		"bytes_sent":         ps.BytesSent,
		"total_bytes":        ps.TotalBytes,
		"instances_sent":     ps.InstancesSent,
		"total_instances":    ps.TotalInstances,
		"percent":            percent,
		"elapsed_s":          int(elapsed),
		"eta_s":              int(eta),
		"error":              ps.Error,
	}
}

// cleanupExpired removes old completed transfers
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
```

---

### Step 3: Update Transfer Client for Progress Tracking

**File:** `internal/transfer/client.go`

Add progress callback in the push methods (around line 330-430):

```go
// Modify pushFilesSTOWRS to accept progress callback
func (c *Client) pushFilesSTOWRS(ctx context.Context, instances []Instance, onProgress func(bytesSent, totalBytes int64)) error {
    // ... existing code ...
    
    // In the multipart write loop:
    for _, inst := range instances {
        // Write instance to buffer
        // ...
        
        // Report progress
        if onProgress != nil {
            onProgress(totalBytesSent, totalBytesSize)
        }
    }
    
    // ... rest of method ...
}
```

---

### Step 4: Integration Points in main.go

**File:** `cmd/tarang-sender/main.go`

Pass progress manager to API server:

```go
// Around line where API server is created:
progressMgr := api.NewProgressManager()
apiServer := &api.Server{
    // ... existing fields ...
    ProgressManager: progressMgr,
}
```

Pass progress callback to transfer client:

```go
// In queue worker when pushing series:
client := &transfer.Client{
    // ... existing config ...
}

progressCallback := func(sent, total int64) {
    progressMgr.Update(seriesUID, sent, instances.Count())
}

err := client.pushFilesSTOWRS(ctx, instances, progressCallback)
if err != nil {
    progressMgr.Fail(seriesUID, err.Error())
} else {
    progressMgr.Complete(seriesUID)
}
```

---

### Step 5: Update Frontend to Use New Endpoints

**File:** `electron/renderer/index.html`

Add script includes before closing body:

```html
<script src="login.js"></script>
<script src="settings-form.js"></script>
<script src="progress-modal.js"></script>
```

Update Settings pane to include the form:

```html
<div data-pane="settings" style="display: none;">
    <!-- settings-form.js will populate this -->
</div>
```

---

### Step 6: Integrate Login with App Startup

**File:** `electron/renderer/app.js`

Add at top of file:

```javascript
// Check if user is logged in
window.addEventListener('load', async () => {
    const auth = localStorage.getItem('tarang_auth');
    if (!auth) {
        window.location.href = 'login.html';
        return;
    }
    
    const credentials = JSON.parse(auth);
    window.tarang.token = credentials.token;
    window.tarang.labId = credentials.labId;
    window.tarang.orgId = credentials.orgId;
    
    // Auto-start refresh
    init();
});

// Add logout button handler
function logout() {
    localStorage.removeItem('tarang_auth');
    window.location.href = 'login.html?logout=1';
}
```

---

### Step 7: Wails Migration Checklist

To switch from Electron to Wails:

1. **Initialize Wails project:**
   ```bash
   cd D:\website\devops\tarang-sender\tarang-sender
   wails init -name tarang-sender -ide vscode
   ```

2. **Move frontend files:**
   ```
   Copy electron/renderer/* → frontend/
   ```

3. **Create wails.json** - template provided in ARCHITECTURE.md

4. **Create electron/main.go** - Wails app entry point (replace Electron main.js logic)

5. **Build for Windows 7-11:**
   ```bash
   wails build -platform windows/amd64 -ldflags "-X github.com/bharatpacs/tarang-sender/cmd/tarang-sender/version=2.0.0"
   ```

---

## PRIORITY: COMPLETE THESE NEXT

1. **Add progress manager to api/progress.go** ← KEY BLOCKER
2. **Update server.go with config endpoints** ← KEY BLOCKER
3. **Test login flow end-to-end** 
4. **Setup Wails & build standalone EXE**
5. **Windows 7/10/11 testing**

---

## FILES CREATED IN THIS SESSION

- ✅ ARCHITECTURE.md
- ✅ electron/renderer/login.html
- ✅ electron/renderer/login.js
- ✅ electron/renderer/settings-form.js
- ✅ electron/renderer/progress-modal.js

---

## TIME ESTIMATE

- Config API endpoints: 2 hours
- Progress tracking: 1 hour
- Wails setup & migration: 3 hours
- Build & testing: 2 hours
- **Total remaining: ~8 hours**

