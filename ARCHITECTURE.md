# TARANG-SENDER v2.0 ARCHITECTURE

## System Overview

TARANG-Sender is a standalone DICOM bridge application that:
- Receives DICOM instances from clinic modalities (DICOM C-STORE SCP server on port 1007)
- Injects custom private tags for tracking
- Forwards to central PACS via STOW-RS (DICOMweb)
- Provides web UI for management and monitoring
- Runs as single Windows EXE (Wails framework, Windows 7-11)

## Component Stack

### Backend (Go)
- **cmd/tarang-sender/main.go**: App lifecycle, config loading, service orchestration
- **internal/scp/**: DICOM C-STORE listener (custom PS3.8/3.7 implementation)
- **internal/transfer/**: STOW-RS HTTP client with progress tracking
- **internal/dicom/**: Tag injection, dataset parsing
- **internal/api/**: HTTP REST API server with config & progress endpoints
- **internal/config/**: Configuration file I/O with validation
- **internal/store/**: BoltDB persistence (studies, series, instances, queue)
- **internal/queue/**: Background worker for study forwarding
- **internal/retention/**: Automatic cleanup of delivered studies

### Frontend (Pure JavaScript)
- **electron/renderer/login.html**: Login form (username/password)
- **electron/renderer/index.html**: Main dashboard
- **electron/renderer/app.js**: Tab controller, status polling, worklist management
- **electron/renderer/api.js**: HTTP client wrapper with auth headers
- **electron/renderer/app.css**: Responsive styling
- **electron/renderer/settings-form.js**: Editable settings with validation
- **electron/renderer/progress-modal.js**: Real-time transfer progress display

### Wails Desktop Wrapper
- **go.mod**: Go module with Wails dependency
- **wails.json**: Wails build configuration
- **electron/main.go**: Wails app lifecycle (replaces Electron main process)
- **electron/preload.js**: IPC bridge (minimal, mostly handled by Wails)

## Data Flow

### 1. Login Flow
```
User enters credentials
    ↓
POST /auth/lab-login (external API)
    ↓
Response: {token, user.labId, user.orgId, user.settings}
    ↓
Electron Store: Save token, labId, orgId
    ↓
Config JSON: Update with labId, orgId
    ↓
Load Dashboard (index.html)
    ↓
Auto-start DICOM SCP + HTTP API
```

### 2. DICOM Ingest → Forward Flow
```
Clinic modality sends DICOM via C-STORE
    ↓
SCP handler receives (internal/scp/handler.go)
    ↓
Parse metadata, inject private tags (labId, orgId)
    ↓
Write to filesystem + BoltDB
    ↓
Wait for stable_age_seconds (15s default)
    ↓
Queue entry created for transfer
    ↓
Background worker picks up (internal/queue/worker.go)
    ↓
Push via STOW-RS to peer URL with progress callback
    ↓
Update study status: "received" → "transferred" → "delivered"
```

### 3. Settings Edit Flow
```
User edits Settings form in UI
    ↓
Frontend validates fields (port ranges, URL format)
    ↓
POST /api/config with updated config
    ↓
Backend validates again
    ↓
Write to config.json (UTF-8 no-BOM)
    ↓
If critical fields changed: restart DICOM SCP + API
    ↓
Return 200 OK with new config
    ↓
Frontend refetches /api/config to confirm
```

### 4. Progress Tracking Flow
```
Series starts transfer (internal/transfer/client.go)
    ↓
Callback reports: bytes_sent, total_bytes, instances_sent, total_instances
    ↓
Progress manager stores in-memory: map[series_uid]ProgressState
    ↓
Frontend polls every 500ms: GET /api/transfer/{series_uid}/progress
    ↓
Response: {status, percent, eta_s, elapsed_s}
    ↓
Progress bar updates in real-time
    ↓
Transfer complete → Status changes to "delivered"
    ↓
Poll clears, UI shows final status
```

## Configuration Schema

```json
{
  "lab_id": "DEV1",
  "org_id": "DEV",
  "log_level": "info",
  
  "dicom": {
    "port": 1007,
    "aet": "TARANG",
    "stable_age_seconds": 15
  },
  
  "http": {
    "port": 9042,
    "api_token": "dev-token"
  },
  
  "storage": {
    "data_dir": "D:\\tarang-test\\data",
    "retention_hours": 24,
    "max_disk_gb": 50
  },
  
  "peer": {
    "name": "DEVPEER",
    "url": "http://206.189.133.52:8042",
    "username": "alice",
    "password": "alicePassword",
    "compression": "gzip",
    "ca_cert_path": "",
    "protocol": "stow-rs"
  },
  
  "tag_injection": {
    "enabled": true,
    "private_creator": "DEV1",
    "private_organisation": "DEV"
  },
  
  "transfer": {
    "concurrent_workers": 6,
    "bucket_size_mb": 4,
    "max_http_retries": 3,
    "http_timeout_seconds": 120
  }
}
```

## API Endpoints

### Authentication & Status
- `GET /api/health` - Health check (no auth)
- `GET /api/status` - System status, config summary (requires token)

### Configuration
- `GET /api/config` - Retrieve full current config (requires token)
- `PATCH /api/config` - Update config fields (requires token)

### Worklist & Studies
- `GET /api/worklist` - List studies with filters (requires token)
- `GET /api/study/{uid}` - Study detail with series list (requires token)
- `POST /api/study/{uid}/retry` - Retry failed study transfer (requires token)

### Progress Tracking
- `GET /api/transfer/{series_uid}/progress` - Real-time transfer progress (requires token)

### Queue Management
- `GET /api/queue/depth` - Current queue depth (requires token)

## Authentication

All endpoints except `/api/health` require `Authorization: Bearer {api_token}` header.
Token is stored in config.json and generated during login.

## File Structure

```
tarang-sender/
├── cmd/
│   └── tarang-sender/
│       └── main.go                    # App entry point
├── internal/
│   ├── api/
│   │   ├── server.go                  # HTTP API + CORS
│   │   ├── progress.go                # Progress state manager
│   │   └── handlers.go                # Endpoint handlers
│   ├── config/
│   │   └── config.go                  # Config struct + I/O
│   ├── dicom/
│   │   ├── dicom.go
│   │   └── inject.go                  # Tag injection logic
│   ├── log/
│   │   └── log.go
│   ├── queue/
│   │   └── worker.go                  # Background worker
│   ├── retention/
│   │   └── sweeper.go
│   ├── scp/
│   │   ├── server.go
│   │   ├── handler.go                 # C-STORE receiver
│   │   └── dimse.go
│   ├── store/
│   │   ├── store.go
│   │   └── types.go
│   └── transfer/
│       └── client.go                  # STOW-RS + progress
├── electron/
│   ├── renderer/
│   │   ├── login.html                 # Login page (NEW)
│   │   ├── login.js                   # Login logic (NEW)
│   │   ├── index.html                 # Dashboard
│   │   ├── app.js                     # Main controller
│   │   ├── app.css                    # Styles
│   │   ├── api.js                     # HTTP client
│   │   ├── settings-form.js           # Settings editor (NEW)
│   │   └── progress-modal.js          # Progress display (NEW)
│   └── main.go                        # Wails app lifecycle (NEW)
├── go.mod
├── go.sum
├── wails.json                         # Wails build config (NEW)
└── README.md
```

## Build & Deployment

### Development
```bash
go run ./cmd/tarang-sender --config D:\tarang-test\config.json
# OR with Wails dev server:
wails dev
```

### Production Build
```bash
wails build -platform windows/amd64 -skipversioncheck
# Output: bin/tarang-sender.exe (standalone, ~40MB)
```

### Windows 7-11 Compatibility
- Go 1.16+ supports Windows 7 natively
- No external dependencies bundled
- Single EXE file, no installation required

## Security Considerations

1. **API Token**: Generated during login, stored in config.json (local file)
2. **Peer Credentials**: Stored plaintext in config.json (secure against network sniffing via HTTPS to Orthanc)
3. **DICOM AE Title**: Validated on connection (C-STORE associations must match configured AET)
4. **Tag Injection**: Only lab_id and org_id injected (non-sensitive tracking)
5. **File Permissions**: BoltDB automatically restricts access to process owner

## Error Handling

### DICOM Errors
- Association rejected: Log "scp called-ae mismatch" if AE title doesn't match
- Transfer syntax unsupported: Log warning, skip tag injection (dataset passed through unchanged)
- Instance write failure: Retry with backoff (1s, 5s, exponential)

### API Errors
- Invalid config: Return 400 with validation errors
- Config write failure: Return 500 with error message
- Progress query on non-existent series: Return 404

### Graceful Degradation
- If peer unreachable: Queue entries retry indefinitely (respects max_http_retries setting)
- If storage full: Stop accepting new instances, alert via API /status endpoint
- If BoltDB corrupted: Auto-recover or require manual reset

## Testing Strategy

### Unit Tests
- Tag injection with various transfer syntaxes
- Config validation (port ranges, URL formats)
- Progress calculation (ETA, percent complete)

### Integration Tests
- Full DICOM ingest → forward flow
- Settings update with SCP restart
- Progress tracking on multi-instance series
- Retry mechanism with exponential backoff

### System Tests
- Windows 7 SP1, 10, 11 launch
- Real modality C-STORE connection (test via DCMTK storescp simulator)
- Network failover (disable Orthanc, queue, re-enable, resume)

## Future Enhancements

1. **Multi-peer failover**: Store multiple peer configs, auto-switch on timeout
2. **Audit logging**: Track all user actions and DICOM transfers to syslog
3. **Auto-update**: Check for new version on startup, prompt user
4. **System tray**: Minimize to tray, show notification on study delivery
5. **Analytics dashboard**: Charts for throughput, success rate, transfer times
6. **DICOM C-FIND/C-MOVE**: Query peer PACS directly from UI (advanced search)
