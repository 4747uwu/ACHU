# TARANG Sender — Run Book

Copy-paste commands to get `tarang-sender` running on your machine, push
real DICOM into it, watch it deliver to the receiver via STOW-RS, then
bundle it into the Electron app.

> All commands assume you've extracted `tarang-sender.zip` and your shell
> is sitting **inside the `tarang-sender/` directory** unless noted.

---

## 1. Prerequisites

| Tool | Version | Why |
|---|---|---|
| Go | 1.22+ | builds the sender |
| Node.js | 18+ | syntax-checks the renderer JS; required if you build Electron |
| dcmtk | any recent | `storescu` / `echoscu` for testing |
| `curl`, `jq` | any | poking the HTTP API |

```bash
# Ubuntu / Debian / WSL
sudo apt install -y golang-go nodejs dcmtk jq curl

# macOS
brew install go node dcmtk jq

# Windows (PowerShell, with Chocolatey)
choco install golang nodejs dcmtk curl jq
```

Verify:

```bash
go version       # need 1.22+
node --version   # need 18+
storescu --version | head -1
```

---

## 2. One-time setup — fix the sandbox `go.mod`

The shipped `go.mod` has three `replace` directives that exist **only**
because the build sandbox can't reach `proxy.golang.org`. **Delete them
in your dev environment**, then `go mod tidy` will fetch the canonical
paths cleanly.

```bash
cd tarang-sender

# Strip the three replace lines (or open go.mod in your editor and delete them)
sed -i.bak '/^replace go.etcd.io\/bbolt /d;       /^replace golang.org\/x\/sys /d;       /^replace golang.org\/x\/text /d' go.mod
rm go.mod.bak

go mod tidy
```

Sanity check:

```bash
grep -c "^replace" go.mod   # should print 0
```

---

## 3. Build

### For your host (Linux / macOS dev box)

```bash
make build
ls -lh bin/tarang-sender   # ~11 MB
```

### Cross-compile the production Windows binary

```bash
make build-windows
ls -lh bin/tarang-sender.exe   # ~8 MB, fully static, no DLL deps
```

The Makefile sets `CGO_ENABLED=0` and `-ldflags="-s -w"` for the smallest
possible static binary.

---

## 4. Run the tests

```bash
go test -race ./...
```

You should see 8 packages with tests, all `ok`. Total ~21 tests.

```bash
go vet ./...   # should print nothing
```

---

## 5. Configure

Copy the example config and edit the bits that matter for your environment:

```bash
mkdir -p ~/tarang-test
cp examples/config.example.json ~/tarang-test/config.json

# Edit these fields:
#   storage.data_dir   →  somewhere writable, e.g. "/home/you/tarang-test/data"
#   http.api_token     →  any random string — the Electron app generates one per session
#   peer.url           →  your real receiver Orthanc, e.g. "http://206.189.133.52:8042"
#   peer.username/password
#   lab_id, org_id     →  whatever the auth API hands back at login
$EDITOR ~/tarang-test/config.json
```

Quick one-liner for a local-only test config (no real receiver — push will
fail and retry, but the SCP/store/API path works fine):

```bash
cat > ~/tarang-test/config.json <<'EOF'
{
  "lab_id": "DEV1",
  "org_id": "DEV",
  "log_level": "info",
  "dicom":   { "port": 1007, "aet": "TARANG", "stable_age_seconds": 15 },
  "http":    { "port": 9042, "api_token": "dev-token" },
  "storage": { "data_dir": "/tmp/tarang-test/data", "retention_hours": 24, "max_disk_gb": 50 },
  "peer":    { "name": "DEVPEER", "url": "http://127.0.0.1:8042",
               "username": "alice", "password": "alicePassword",
               "compression": "gzip", "ca_cert_path": "", "protocol": "stow-rs" },
  "tag_injection": { "enabled": true, "private_creator": "DEV1", "private_organisation": "DEV" },
  "transfer": { "concurrent_workers": 6, "bucket_size_mb": 4, "max_http_retries": 3, "http_timeout_seconds": 120 }
}
EOF
```

---

## 6. Run the sender

```bash
./bin/tarang-sender --config ~/tarang-test/config.json
```

You should see something like:

```
t=11:50:50.147 level=INFO msg="tarang-sender starting" version=0.7.0 lab_id=DEV1 ...
t=11:50:50.182 level=INFO msg="store opened" path=/tmp/tarang-test/data/tarang.db
t=11:50:50.183 level=INFO msg="scp listening" addr=[::]:1007 aet=TARANG
t=11:50:50.183 level=INFO msg="http api listening" addr=127.0.0.1:9042
t=11:50:50.183 level=INFO msg="retention sweeper starting" retention=24h0m0s interval=1h0m0s
t=11:50:50.183 level=INFO msg=ready dicom=:1007 http=127.0.0.1:9042 aet=TARANG stable_age=15s
```

Leave it running. Open a second terminal for the next steps.

> **Port 1007 below 1024** needs `sudo` on Linux/macOS, or change to `11007`
> in the config. Windows doesn't care.

---

## 7. Send a real DICOM and watch it flow

In the second terminal:

```bash
# Ping
echoscu -aec TARANG localhost 1007

# Send any DICOM file you have lying around
storescu -aec TARANG -aet TESTSCU localhost 1007 /path/to/study.dcm

# Or generate a tiny synthetic one if you don't have one handy:
cat > /tmp/test.dump <<'EOF'
(0008,0016) UI =CTImageStorage
(0008,0018) UI [1.2.3.4.test.inst.001]
(0008,0060) CS [CT]
(0010,0010) PN [DOE^JOHN^TEST]
(0010,0020) LO [P-TEST-001]
(0020,000d) UI [1.2.3.4.test.study.001]
(0020,000e) UI [1.2.3.4.test.series.001]
EOF
dump2dcm /tmp/test.dump /tmp/test.dcm
storescu -aec TARANG -aet TESTSCU localhost 1007 /tmp/test.dcm
```

In the sender's log you should immediately see:

```
level=INFO msg="scp association accepted"  calling_aet=TESTSCU contexts=128
level=INFO msg="instance received"         injected=true ...
```

…then **exactly 15 seconds later** (the stable-age window):

```
level=INFO msg="series stable, queued for transfer"
level=INFO msg="processing queue entry"
level=INFO msg="pushing series" protocol=stow-rs ...
```

If the receiver is unreachable (e.g. you used `127.0.0.1:8042` with nothing
listening), the queue will retry with exponential backoff (1s → 5s → 30s
→ 5m → 30m, capped at `max_http_retries`). That's working as designed.

---

## 8. Hit the HTTP API

```bash
TOKEN=dev-token   # whatever you set in config.json

# Health (no auth)
curl -s http://127.0.0.1:9042/api/health | jq

# Status (auth required)
curl -s -H "Authorization: Bearer $TOKEN" \
     http://127.0.0.1:9042/api/status | jq

# Worklist
curl -s -H "Authorization: Bearer $TOKEN" \
     http://127.0.0.1:9042/api/worklist | jq

# Single study detail (paste the UID from worklist output)
UID=1.2.3.4.test.study.001
curl -s -H "Authorization: Bearer $TOKEN" \
     http://127.0.0.1:9042/api/study/$UID | jq

# Force-retry a study
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
     http://127.0.0.1:9042/api/study/$UID/retry | jq

# Force-delete a study (removes files + DB rows)
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" \
     http://127.0.0.1:9042/api/study/$UID | jq
```

---

## 9. Try the renderer in dev mode (no Electron)

The renderer JS has a built-in fallback so you can iterate on the UI
against the running sender from any browser:

```bash
cd electron/renderer
python3 -m http.server 8000
```

Then open in your browser:

```
http://localhost:8000/index.html?token=dev-token&port=9042
```

You'll get the full four-tab UI talking to the local sender. Refresh to
pick up CSS/JS changes — no rebuild needed.

---

## 10. Bundle into the existing Bharat PACS Electron app

Three steps in your existing Electron project:

**(a) Drop the binary into `resources/`** so it ships with the installer:

```bash
# Windows production target
cp tarang-sender/bin/tarang-sender.exe   /path/to/electron-app/resources/

# (or Linux/macOS for development)
cp tarang-sender/bin/tarang-sender       /path/to/electron-app/resources/
```

**(b) Wire the IPC handlers into your existing `main.js`.** Open
`tarang-sender/electron/main/main.js` and copy these into your existing
main process:

- `bootstrapSender({ labId, orgId, peer })` — call this from your
  post-login handler with the values your auth API returned
- `startSender()` / `stopSender()` — process lifecycle (already called
  by `bootstrapSender` and the `before-quit` hook)
- The three `ipcMain.handle(...)` blocks (`tarang:get-bootstrap`,
  `tarang:update-config`, `tarang:read-recent-logs`)

**(c) Drop the renderer files in.** Copy `electron/renderer/*` into your
existing renderer source tree, and wire `preload.js` into your
`BrowserWindow` config:

```js
new BrowserWindow({
  webPreferences: {
    preload: path.join(__dirname, 'preload.js'),
    contextIsolation: true,
    nodeIntegration: false,
  },
});
```

The renderer fully self-bootstraps from `window.tarangAPI.ready` —
nothing else to wire up.

---

## 11. Common troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `bind: address already in use` on port 1007 | Old Orthanc still running | Stop it, or change `dicom.port` |
| `bind: permission denied` on port 1007 | Linux/macOS unprivileged port | `sudo` or use a port ≥ 1024 |
| `cannot find package go.etcd.io/bbolt` | Forgot to remove the sandbox `replace` directives | See Section 2 |
| Worklist stays empty after `storescu` | DICOM association failed silently | Check sender log; verify AET = `TARANG` |
| Series stays `Received`, never goes `Stable` | Nothing wrong — wait `stable_age_seconds` (default 15s) | Or lower it in config |
| Series stays `Queued`/`Failed`, retry log shows network errors | Receiver unreachable | Check `peer.url`, VPN, firewall |
| HTTP API returns 401 | Wrong / missing bearer token | `Authorization: Bearer <http.api_token from config>` |
| Renderer in browser shows "sender unreachable" | Sender not running, or wrong port | Check `?port=` query param matches `http.port` |

---

## 12. File layout reference

```
tarang-sender/
├── cmd/tarang-sender/main.go      entrypoint — wires every subsystem
├── internal/
│   ├── api/        HTTP API for the renderer
│   ├── config/     JSON config loader + validation
│   ├── dicom/      parsing + surgical tag injection
│   ├── log/        slog wrapper, daily file rotation
│   ├── queue/      durable retry/backoff worker
│   ├── retention/  periodic sweeper + force-delete cascade
│   ├── scp/        DICOM Upper Layer + DIMSE (PS3.7/3.8) — hand-rolled
│   ├── stability/  per-series stable-age timer
│   ├── store/      BoltDB layer + indexes
│   └── transfer/   STOW-RS + legacy /instances clients
├── electron/
│   ├── main/       Electron main process: spawn binary, IPC, restart
│   └── renderer/   Four-tab UI: Worklist, Settings, Connection, Logs
├── examples/config.example.json
├── go.mod / go.sum
├── Makefile
└── README.md
```
