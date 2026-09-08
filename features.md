# Achyu PACS — Backend API Contract

Everything the router (`tarang-sender.exe`, shipped as **Achyu PACS**) sends to
your server, and exactly what it accepts back. Written so you can implement the
controllers without reading the Go source.

**Two hosts, two jobs:**

| Host | Role |
|---|---|
| `https://pacs.achyutrs.com` | Application API — login, study notifications, delivered, **lab activation**, error logs |
| `https://router.achyutrs.com` | DICOM receiver (Orthanc peer) — studies are pushed here via STOW-RS |

All application endpoints hang off one base, derived from the login URL up to and
including `/api`. Log in at `https://pacs.achyutrs.com/api/auth/lab-login` and the
router uses `https://pacs.achyutrs.com/api` for everything else. Point the login
URL at staging and every other call follows automatically.

| Endpoint | Method | When |
|---|---|---|
| `/api/auth/lab-login` | POST | Operator signs in |
| **`/api/sender/lab-status`** | **POST** | **Before every notification and every upload; also polled every 60s** |
| `/api/orthanc2/instance-exe-received` | POST | First instance of a study arrives |
| `/api/orthanc2/delivered` | POST | Every series of a study is on the PACS |
| `/api/sender/error-logs` | POST | Any error-level log record |

---

## 1. Lab activation gate — `POST /api/sender/lab-status`

**This is the new endpoint.** The router asks "is this lab allowed to transmit?"
before it announces a study and before it uploads one. An inactive answer holds
transmission and raises the *"Your account is inactive — contact Achyu"* banner
in the app.

### Request

```
POST https://pacs.achyutrs.com/api/sender/lab-status
Content-Type: application/json
Accept: application/json
Authorization: Bearer <lab_status.api_key>   ← only if an api_key is configured
```

```json
{
  "lab_id":        "ARX1",
  "org_id":        "ARX",
  "hostname":      "RECEPTION-PC",
  "agent_version": "0.1.1",
  "reason":        "notify"
}
```

| Field | Type | Notes |
|---|---|---|
| `lab_id` | string | Lab identifier from login (`user.lab.identifier`). **This is the key to look up.** |
| `org_id` | string | Organisation identifier (`user.organizationIdentifier`) |
| `hostname` | string | Workstation name — for support, may be empty |
| `agent_version` | string | Router build version |
| `reason` | string | Why we asked: `startup`, `poll`, `notify`, `delivered`, `upload`, `manual`. Informational — log it or ignore it. |

### Response — the shape to implement

Return **HTTP 200** with:

```json
{
  "success": true,
  "active":  true,
  "status":  "active",
  "message": ""
}
```

and when the lab is not allowed to transmit:

```json
{
  "success": true,
  "active":  false,
  "status":  "suspended",
  "message": "Your subscription has expired. Please contact Achyu."
}
```

`active` is the only field that has to be right. `message` is shown to the
operator **verbatim** in the banner — write it for a receptionist, not a
developer. If you omit it, the router shows
*"Your account is inactive. Please contact Achyu."*

### How the response is interpreted

Rules are applied in this order; the first that matches decides.

| # | Condition | Verdict |
|---|---|---|
| 1 | HTTP `402`, `403`, `423`, `451` | **inactive** (deliberate refusal; body's `message` still used if present) |
| 2 | HTTP `5xx`, `404`, `429`, timeout, DNS failure, non-JSON body | **unknown** → see *Failure policy* |
| 3 | Body has `active`, `is_active`, `lab_active`, or `data.active` (boolean) | that boolean, **wins over everything else** |
| 4 | Body has `status` (string) | mapped through the tables below |
| 5 | Body has only `success` | `true` → active, `false` → inactive |
| 6 | 200 with nothing recognisable | **unknown** |

Accepted `status` strings (case-insensitive):

- **active:** `active`, `enabled`, `live`, `ok`, `valid`, `approved`, `running`
- **inactive:** `inactive`, `disabled`, `suspended`, `expired`, `blocked`, `banned`,
  `terminated`, `cancelled`, `canceled`, `unpaid`, `overdue`, `deleted`,
  `pending`, `not_found`, `notfound`, `unauthorized`

A `data: { … }` envelope is unwrapped, so `{"success":true,"data":{"active":true}}`
works unchanged. Aliases exist so an existing controller can be reused as-is —
for new work, just return `active`.

> Every row above is pinned by a test: `TestInterpretResponseMatrix` in
> `internal/labstatus/checker_test.go`. If you want another shape accepted, add
> it there.

### Failure policy — read this before you deploy

The gate is deliberately **asymmetric**, because the two mistakes are not equally
bad. Wrongly blocking a working lab loses studies; wrongly allowing a lapsed one
costs a few uploads.

- **"inactive" is only ever concluded from an answer you actually gave.** It then
  persists until you say otherwise.
- **An unreachable backend is `unknown`, not `inactive`.** Unknown keeps the last
  known verdict. With no verdict yet, `lab_status.fail_open` (default `true`)
  decides — so a backend outage, an expired TLS cert, or a clinic's flaky link
  can never strand studies on a workstation whose retention sweeper will delete
  them in 24 hours.
- **Do not return `403` for an unknown lab_id** unless you intend to stop it
  transmitting. Return `200 {"active": false, …}` if that is what you mean, or a
  `5xx` if you genuinely could not answer.
- **Never return 404 for a lab you do not recognise** if you want it blocked —
  404 reads as "route missing" and falls through to the failure policy.

Set `"fail_open": false` in the router's `lab_status` config for a
deny-by-default posture: nothing transmits until you confirm the lab is active.

### Cadence and caching

- Polled every **60s** in the background (`lab_status.poll_seconds`).
- A verdict is cached for **60s**; the ingest path reuses it rather than calling
  you on every instance. Expect roughly **1 request/minute per workstation**,
  plus one on startup and one whenever the operator presses "Check again".
- Concurrent callers collapse onto a single in-flight request.
- Deactivation takes effect within ~60s. Reactivation resumes within ~60s, or
  immediately if the operator presses "Check again".

### What "inactive" actually does

| Behaviour | While inactive |
|---|---|
| Receive DICOM from modalities | **Continues** — nothing is refused or lost |
| Store studies on the workstation | **Continues** |
| `POST /orthanc2/instance-exe-received` | **Held** — parked in memory (max 500), sent after reactivation |
| `POST /orthanc2/delivered` | **Held** — same |
| STOW-RS upload to `router.achyutrs.com` | **Held** — the queue entry is re-enqueued, not failed, so retry budget is untouched |
| App banner | *"Your account is inactive — contact Achyu"* + counts on hold |

Nothing is marked failed and nothing is dropped. When you flip the lab back to
active, held notifications flush and the upload queue drains on its own.

### Minimal controller (Express)

```js
// POST /api/sender/lab-status
router.post('/sender/lab-status', async (req, res) => {
  const { lab_id, org_id, hostname, agent_version, reason } = req.body || {};

  const lab = await Lab.findOne({ identifier: lab_id });

  // Unknown lab: 200 + active:false is an explicit block.
  // Return 500 instead if you genuinely could not answer — never 404.
  if (!lab) {
    return res.json({
      success: true,
      active: false,
      status: 'not_found',
      message: 'This workstation is not registered. Please contact Achyu.',
    });
  }

  const active = lab.isActive && !lab.suspended && lab.subscriptionValidTill > new Date();

  return res.json({
    success: true,
    active,
    status: active ? 'active' : (lab.suspended ? 'suspended' : 'expired'),
    // Shown to the operator verbatim — keep it plain.
    message: active ? '' : 'Your account is inactive. Please contact Achyu.',
  });
});
```

---

## 2. Login — `POST /api/auth/lab-login`

Unchanged from the previous build except for the host.

### Request

```json
{
  "email": "operator@lab.com",
  "password": "secret",
  "peer": { "url": "https://router.achyutrs.com", "username": "" }
}
```

### Response

```json
{
  "success": true,
  "token": "<jwt>",
  "user": {
    "name": "Reception Desk",
    "organizationIdentifier": "ARX",
    "lab": {
      "identifier": "ARX1",
      "settings": {
        "peer": {
          "name": "ACHYU",
          "url": "https://router.achyutrs.com",
          "username": "…",
          "password": "…",
          "protocol": "stow-rs",
          "compression": "gzip"
        }
      }
    }
  }
}
```

- `token` is required; anything else falls back to a default.
- `user.lab.identifier` → `lab_id`, `user.organizationIdentifier` → `org_id`.
  **These are the values sent back to `/sender/lab-status`**, so they must match
  what that controller looks up.
- `user.lab.settings.peer` is optional and overrides the built-in receiver
  defaults. Returning `username`/`password` here is preferable to baking receiver
  credentials into the installer.
- `success: false` or a non-2xx is rejected, showing `message` to the operator.

---

## 3. Study notification — `POST /api/orthanc2/instance-exe-received`

Fired once per study, the moment its first instance arrives, so the study appears
on the PACS as `upload_pending` before the upload finishes. **Gated on lab
activation.**

```json
{
  "StudyInstanceUID": "1.2.840.113619.2.55.3.…",
  "PatientID": "P12345",
  "PatientName": "DOE^JANE",
  "StudyDate": "20260808",
  "StudyTime": "141530",
  "StudyDescription": "CT CHEST",
  "AccessionNumber": "ACC001",
  "Modality": "CT",
  "status": "upload_pending",
  "PrivateCreator_0013": "ARX1",
  "PrivateCreator_0015": "ARX1",
  "PrivateOrganisation_0021": "ARX",
  "PrivateOrganisation_0043": "ARX"
}
```

The `PrivateCreator_*` / `PrivateOrganisation_*` fields carry `lab_id` and
`org_id` (matching the private DICOM tags injected into the instances), which is
how the handler resolves org and lab. Field names match the legacy Lua
`OnStoredInstance` payload, so an existing handler needs no changes.

Any 2xx is success. De-duplicated per study for the process lifetime.

---

## 4. Delivered — `POST /api/orthanc2/delivered`

Fired ~15s after every series of a study has been pushed to the receiver. The URL
is derived by replacing the last path segment of the notifier URL with
`delivered`. **Gated on lab activation.**

```json
{
  "OrthancID": "a1b2c3d4-e5f6a7b8-…",
  "StudyInstanceUID": "1.2.840.113619.2.55.3.…",
  "PatientID": "P12345",
  "status": "delivered",
  "lab_id": "ARX1",
  "org_id": "ARX",
  "delivered_at": "2026-08-08T14:20:31Z"
}
```

`OrthancID` is Orthanc's public study ID, computed the same way Orthanc does —
`SHA1("PatientID|StudyInstanceUID")` as lower-case hex in five dash-separated
8-char groups — so it matches the resource ID on the receiving Orthanc without a
lookup.

Not de-duplicated: a re-delivered study notifies again, by design.

---

## 5. Error logs — `POST /api/sender/error-logs`

Every error-level log record, with device and lab identity, spooled to disk first
so nothing is lost across restarts. **Not** gated — you want to hear about a
broken workstation whether or not its account is paid up. See
`PACS_ERROR_LOG_API.md` for the payload.

---

## 6. DICOM receiver — `https://router.achyutrs.com`

The peer studies are pushed to, via **STOW-RS** (`POST /dicom-web/studies`) with
HTTP basic auth, falling back to Orthanc's native `POST /instances` when
`peer.protocol` is `instances`. Reachability is probed with `GET /system` every
30s; uploads pause while it is down and resume automatically.

---

## Quick reference

```
Login      POST https://pacs.achyutrs.com/api/auth/lab-login
Activation POST https://pacs.achyutrs.com/api/sender/lab-status          ← NEW
Notify     POST https://pacs.achyutrs.com/api/orthanc2/instance-exe-received
Delivered  POST https://pacs.achyutrs.com/api/orthanc2/delivered
Errors     POST https://pacs.achyutrs.com/api/sender/error-logs
Studies   STOW https://router.achyutrs.com/dicom-web/studies
```

The only endpoint you need to add is **`POST /api/sender/lab-status`**. Return
`{"success":true,"active":true|false,"status":"…","message":"…"}` and the router
does the rest.

Achyu PACS — Complete Implementation Context
Written from conversation context on 2026-08-18, NOT read back from the codebase — the working tree was truncated (see §0). Treat this as the authoritative record of what exists and how it works, and as a reconstruction guide if source has to be rewritten.
Deliberately stored outside the project folder (d:\website\devops\tarang-sender\, one level up from updated Achyu\) so it is not sitting in the damaged tree. Copy it somewhere durable.
________________________________________
0. Status: the working tree is damaged
At the end of the session, every large Go source file in updated Achyu\ was truncated to a few hundred bytes, all with the same 18-08-2026 01:3x timestamp. Observed sizes:
File	Size after
internal/transfer/client.go	80 B
internal/scp/server.go	239 B
internal/config/config.go	261 B
internal/api/server.go	295 B
internal/transcode/lossless_roundtrip_test.go	377 B
internal/labstatus/checker_test.go	433 B
internal/scp/pdu.go	465 B
internal/notifier/notifier.go	518 B
internal/scp/dicomtls.go	606 B
internal/config/config_test.go	867 B
cmd/tarang-sender/main.go	917 B
Each surviving fragment looked like the tail of the last edit applied to that file. C: was at 0 bytes free when this surfaced, which is the most likely trigger (temp/atomic-write failures). The project lives on D:, which had ~127 GB free, so it is not a simple "disk full on the target volume".
This is not a git repository, so there is no git restore. Recovery avenues, best first:
1.	dist\ + app.asar — if a packaged installer from an earlier build survives, the entire Electron half (main process, preload, renderer HTML/JS/CSS) can be extracted from resources\app.asar with npx asar extract. This is a complete recovery path for the JavaScript side.
2.	IDE local history — VS Code keeps per-file timelines (%APPDATA%\Code\User\History). Since C: is full, check whether it kept writing; if so, this is the best Go-source recovery path.
3.	Windows Previous Versions / VSS on D: — right-click the folder → Restore previous versions. Worth trying before anything else writes there.
4.	tarang-sender.exe (8,484,864 bytes, built 2026-08-15 14:49, go1.20.14/386) — a working binary with every feature in this document compiled in. It proves behaviour and can keep sites running, but Go binaries are not decompilable back to this source.
5.	Regeneration from this conversation — the files I authored this session exist verbatim in my context and can be rewritten exactly: internal/dpapi/*, internal/oscrypt/*, internal/labstatus/*, internal/scp/dicomtls.go (+ test), internal/transcode additions (+ tests), internal/config crypto + tests, internal/notifier gate additions (+ tests), generate-ico.js, ACHYU_BACKEND_API.md, and every Electron/renderer edit. Files I only edited (pre-existing bodies like transfer/client.go, scp/pdu.go, store/*, dicom/*, queue/worker.go) I can only partially reconstruct — those need path 1–3.
Free space on C: before doing anything else. The Go linker writes to %TEMP% on C:, so build.bat cannot work until then. Every build in this session had to be run with GOTMPDIR/TMP/TEMP redirected to D:.
________________________________________
1. What this product is
Achyu PACS — a Windows desktop DICOM router installed on a clinic workstation. Modalities (CT, X-ray, ultrasound…) send studies to it over DICOM; it stores them locally, then forwards them to a central PACS over HTTP. It exists because the sites have slow, unreliable uplinks: the router accepts a study at LAN speed, then drains it upstream with retries and resume, so a scan is never lost to a dropped connection.
It replaced a client-side Orthanc install. Lineage of names (all the same product line): Bharat PACS → Tarang → SureScan / UEvolveAI / vrinda → QuickLine Router / QUICKON → ArihantX PACS → QuickOn PACS → Achyu PACS. The installer purges the old %APPDATA% folders of every previous name, because leftovers caused reinstalls to come back "out of sync".
Two halves, one product
Half	Artifact	Role
Go engine	tarang-sender.exe	DICOM listener, local store, queue, transcode, upload, local HTTP API
Electron app	installer / Achyu PACS.exe	Login, UI, owns config.json, spawns and supervises the engine
The Electron main process is the only writer of config at install/login time; the Go engine reads it, and may re-save it (normalising encryption, or via PATCH /api/config). They must therefore agree byte-for-byte on the config file format — see §14.
Names deliberately NOT changed in the rebrand
These are internal and renaming them buys nothing while risking breakage:
•	Go module path github.com/bharatpacs/tarang-sender
•	Binary name tarang-sender.exe
•	IPC channels tarang:* and the preload bridge window.tarangAPI
•	The tarang-ready DOM event
•	Log filenames tarang-YYYY-MM-DD.log
•	BoltDB file tarang.db
Everything user-visible is Achyu: window title, tray, installer, shortcuts, appId (com.achyu.pacs), productName (Achyu PACS), AE title, endpoints, log banner.
Versions & pins
•	package.json version 0.1.1 — single source of truth; build.bat reads it and passes it to the Go build via -ldflags -X main.version=.
•	Go 1.20.14, GOARCH=386, CGO_ENABLED=0 — pinned via GOTOOLCHAIN because Go 1.21+ dropped Windows 7 support and the Electron installer is ia32.
•	go.mod must stay go 1.20. Never run go mod tidy — it rewrites the directive to 1.21 (bbolt declares 1.21), which breaks the Win7 build.
•	Electron 22.3.27 (Chromium 108) — relevant to the OSCrypt format in §14.
________________________________________
2. Hosts and endpoints
Host	Role
https://pacs.achyutrs.com	Application API — login, study notifier, delivered, lab activation, error logs
https://router.achyutrs.com	DICOM receiver (Orthanc peer) — studies are pushed here via STOW-RS
Every application endpoint is derived from the login URL up to and including /api. Log in at https://pacs.achyutrs.com/api/auth/lab-login and everything else follows automatically — point login at staging and the whole app follows.
Login      POST {base}/auth/lab-login
Activation POST {base}/sender/lab-status              ← the only NEW endpoint
Notify     POST {base}/orthanc2/instance-exe-received
Delivered  POST {base}/orthanc2/delivered
Errors     POST {base}/sender/error-logs
Studies   STOW https://router.achyutrs.com/dicom-web/studies
Full request/response contract: ACHYU_BACKEND_API.md (also truncated — regenerate from §11 here plus my context).
________________________________________
3. The data path, end to end
modality
   │  DICOM C-STORE (TLS or plaintext, same port 8899)
   ▼
internal/scp ─ sniff first byte ─ 0x16 → TLS wrap, else plaintext
   │  A-ASSOCIATE (any called AE title accepted) → C-STORE-RQ
   ▼
scp.IngestHandler
   ├─ write instance to <data_dir>/…
   ├─ inject private tags (lab/org identity)
   ├─ upsert study/series/instance in BoltDB
   ├─ OnStudyFirstSeen ──▶ notifier.MaybeNotify  ◀─ LAB GATE
   └─ OnInstanceStored ──▶ stability.TouchWithHint
                                │  quiet for stable_age_seconds
                                ▼
                        queue.EnqueueTranscode  (if any compression mode on)
                                │      else EnqueueSeries
                                ▼
                        queue.Worker (6 goroutines, BoltDB-backed)
                           ├─ ConnCheck: LAB GATE, then peer reachability
                           ├─ "transcode" job → transcode.PlanFor → gdcmconv
                           │                     → EnqueueSeries
                           └─ "series" job    → transfer.PushSeries (STOW-RS)
                                                    │
                                    OnStudyDelivered│ (+15s settle)
                                                    ▼
                                        notifier.NotifyDelivered ◀─ LAB GATE
                                                    │
                        retention.Sweeper deletes delivered studies after N hours
Alongside: internal/api serves the renderer on 127.0.0.1:9044; internal/connectivity probes the peer every 30s; internal/errorreport ships error-level logs; internal/labstatus polls the activation gate every 60s.
________________________________________
4. Repo layout
updated Achyu/
├─ cmd/tarang-sender/
│  ├─ main.go                  lifecycle, wiring, startup/shutdown order
│  ├─ autotune_windows.go      totalSystemRAMGB() via Win32
│  └─ autotune_other.go        non-Windows stub
├─ internal/
│  ├─ api/          local HTTP API for the renderer (+ progress.go)
│  ├─ config/       schema, defaults, Normalize, Validate, whole-file crypto
│  ├─ connectivity/ peer reachability watchdog
│  ├─ dicom/        Part-10 parse + private-tag injection
│  ├─ dpapi/        Windows DPAPI wrapper                        [NEW]
│  ├─ errorreport/  spool + POST error-level logs
│  ├─ labstatus/    lab activation gate                          [NEW]
│  ├─ log/          slog-based logger, file + stdout, error sink
│  ├─ notifier/     study + delivered notifications, gate, parking
│  ├─ oscrypt/      Chromium "v10" AES-GCM + Local State key     [NEW]
│  ├─ queue/        durable queue worker, retries, backoff
│  ├─ retention/    delete delivered studies after a window
│  ├─ scp/          DICOM SCP: PDU, DIMSE, handler, TLS          [dicomtls.go NEW]
│  ├─ stability/    per-series quiet-period timers
│  ├─ store/        BoltDB persistence
│  ├─ transcode/    gdcmconv wrapper: lossy .91 + lossless .90
│  └─ transfer/     STOW-RS / Orthanc-instances push client
├─ electron/
│  ├─ main/main.js      config owner, process supervisor, IPC, tray, window
│  ├─ main/preload.js   contextBridge → window.tarangAPI
│  ├─ renderer/         index.html, login.html, app.js, login.js, api.js,
│  │                    app.css, progress-modal.js, settings-form.js (unused)
│  ├─ logo.ico          app icon (generated from AXP-mark.jpeg)
│  └─ logo.png          512×512 transparent master
├─ build/installer.nsh  NSIS hooks: kill sender, purge legacy appdata
├─ build.bat            THE build script (Go + Electron together)
├─ generate-ico.js      brand mark → logo.png + logo.ico
├─ AXP-mark.jpeg        source brand lockup
├─ gdcmconv.exe + gdcm*.dll + msvc*.dll  bundled transcoder (x86 — 32- and 64-bit Windows)
└─ examples/config.example.json
________________________________________
5. Complete config schema
Written by Electron at login, read by Go. Encrypted at rest (§14) — this is the decrypted content.
{
  "lab_id": "ARX1",              // from login: user.lab.identifier
  "org_id": "ARX",               // from login: user.organizationIdentifier
  "log_level": "info",

  "dicom": {
    "port": 8899,
    "aet": "ACHYU",           // display/logging only — never gates a connection
    "stable_age_seconds": 15,    // Electron default; Go default 5
    "tls": {                     // Secure DICOM, same port as plaintext
      "enabled": true,
      "cert_file": "",           // empty → self-signed, generated + cached
      "key_file": "",
      "allow_legacy_rc4": false
    }
  },

  "http": { "port": 9044, "api_token": "<random 24 bytes hex>" },

  "storage": {
    "data_dir": "%APPDATA%/<app>/achyu/data",
    "retention_hours": 24,
    "max_disk_gb": 50
  },

  "peer": {
    "name": "ACHYU",
    "url": "https://router.achyutrs.com",
    "username": "Xcentic",              // carried over; ideally served at login
    "password": "XcenticIngestion123",
    "compression": "gzip",              // HTTP transport compression
    "ca_cert_path": "",
    "protocol": "stow-rs"               // or "instances"
  },

  "tag_injection": {
    "enabled": true,
    "private_creator": "ARX1",          // = lab_id
    "private_organisation": "ARX"       // = org_id
  },

  "transfer": {
    "concurrent_workers": 6,
    "bucket_size_mb": 32,               // 0 → auto-tuned from RAM
    "max_http_retries": 5,
    "http_timeout_seconds": 600
  },

  "compression": {
    "enabled": false,                   // LOSSY .91 — opt-in
    "skip_already_compressed": true,
    "gdcmconv_path": "<resources>/gdcmconv.exe",
    "default_rate": 0,                  // N:1; 0 = skip unlisted modalities
    "modality_rate": { "CT": 10 },
    "lossless": {                       // LOSSLESS .90 — OFF by default
      "enabled": true,
      "skip_already_compressed": true,
      "modality_enabled": {}            // exceptions only; absent = enabled
    }
  },

  "notifier": {
    "backend_url": "https://pacs.achyutrs.com/api/orthanc2/instance-exe-received",
    "api_key": ""
  },

  "error_reporting": {
    "enabled": true,
    "backend_url": "https://pacs.achyutrs.com/api/sender/error-logs",
    "api_key": ""
  },

  "lab_status": {                       // activation gate
    "enabled": true,
    "backend_url": "https://pacs.achyutrs.com/api/sender/lab-status",
    "api_key": "",
    "poll_seconds": 60,
    "fail_open": true
  },

  "resilience": {
    "network_retry": true,
    "instance_checkpoint": true,
    "connectivity_watchdog": true
  }
}
Plus legacy, read-only per-field ciphertext (superseded by whole-file encryption; Load still decrypts them, Save blanks them): http.encrypted_api_token, peer.encrypted_url, peer.encrypted_username, peer.encrypted_password, notifier.encrypted_api_key, error_reporting.encrypted_api_key.
config package API
func Default() Config                       // shipped defaults
func Load(path string) (Config, error)      // read → decrypt → unmarshal on defaults
                                            // → legacy per-field → Normalize → Validate
func (c Config) Save(path string) error     // value receiver! blanks legacy, marshal,
                                            // encrypt, atomic .tmp + rename
func (c *Config) Normalize()                // derived defaults (lab_status.poll_seconds)
func (c Config) Validate() error            // hard errors
func DetectFormat(raw []byte) Format        // plaintext-json | aes-gcm-v10 | dpapi | unknown
func DescribeFile(path string) Format       // same, from a path; never errors
Validate rejects: empty lab_id/org_id; ports out of range; dicom.port == http.port; retention_hours < 1; empty peer.url/peer.name; unknown peer.protocol; concurrent_workers < 1; lab_status.enabled with empty backend_url; exactly one of dicom.tls.cert_file/key_file set.
Auto-tune (autoTuneConfig, only fills zero values, so explicit settings always win): RAM ≥12 GB → bucket_size_mb 32; ≥6 GB → 16; else 8. Workers capped to 4 when RAM < 6 GB.
________________________________________
6. DICOM receive — internal/scp
Hand-rolled DICOM Upper Layer (PS3.8) + DIMSE (PS3.7). Hand-rolled because the Go DICOM networking ecosystem is unmaintained (grailbio/go-netdicom archived, forks have broken transitive deps) and the needed surface is small.
Supported: A-ASSOCIATE with multiple presentation contexts, C-STORE-RQ for any proposed storage SOP class, C-ECHO, all transfer syntaxes in SupportedTransferSyntaxes (store-and-forward, no transcode at this layer), A-RELEASE, A-ABORT. Not supported: async ops, role/extended negotiation, C-FIND/C-MOVE/C-GET as SCP.
Files: server.go (accept loop, association lifecycle, dispatch), pdu.go (PDU framing, item encode/decode, padAE/trimAE), dimse.go (command sets), handler.go (IngestHandler), uids.go (SOP class + transfer syntax UIDs), dicomtls.go (TLS).
6.1 TLS + plaintext on ONE port ★ built this session
The problem: a site's X-ray unit was configured for "Secure DICOM" and could not associate at all, while a CT on the same switch sent plaintext. Giving the X-ray its own port would have meant reconfiguring the CT.
Solution — sniff the first byte of every connection:
const tlsHandshakeByte = 0x16   // TLS record type "handshake"
const sniffTimeout = 15 * time.Second

// A DICOM A-ASSOCIATE-RQ PDU starts 0x01, a TLS ClientHello starts 0x16,
// so one byte is an unambiguous discriminator.
func (s *Server) maybeWrapTLS(conn net.Conn) (net.Conn, bool, error) {
    if s.cfg.TLSConfig == nil { return conn, false, nil }
    br := bufio.NewReader(conn)
    conn.SetReadDeadline(time.Now().Add(sniffTimeout))
    first, err := br.Peek(1)
    conn.SetReadDeadline(time.Time{})
    if err != nil { return nil, false, err }
    buffered := &peekedConn{Conn: conn, r: br}   // re-serves the peeked byte
    if first[0] != tlsHandshakeByte { return buffered, false, nil }
    return tls.Server(buffered, s.cfg.TLSConfig), true, nil
}

type peekedConn struct { net.Conn; r *bufio.Reader }
func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }
handleConnection then calls tlsConn.Handshake() explicitly before reading DICOM, so a handshake failure is logged as such instead of surfacing as a garbled PDU, and logs tls_version + cipher_suite.
BuildServerTLSConfig(TLSOptions{Enabled, CertFile, KeyFile, CacheDir, AllowLegacyRC4}):
•	MinVersion: tls.VersionTLS10 — set explicitly on purpose. Go 1.22+ servers reject TLS 1.0/1.1 by default; an explicit MinVersion overrides that. The field X-ray sends a TLS 1.0 ClientHello.
•	Legacy cipher suites re-enabled, roughly strongest → weakest: ECDHE/RSA AES-GCM, CHACHA20, TLS 1.2 CBC-SHA256, then the TLS 1.0-era TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA, TLS_RSA_WITH_AES_128_CBC_SHA, TLS_RSA_WITH_3DES_EDE_CBC_SHA. RC4 only when allow_legacy_rc4 is true.
•	PreferServerCipherSuites: false — old stacks often implement exactly one suite correctly; let the client choose.
•	ClientAuth: tls.NoClientCert — modalities rarely present one and asking confuses old stacks.
•	Self-signed cert generated and cached under <data_dir>/tls/ (dicom-tls-cert.pem / dicom-tls-key.pem, key written 0600 first). Caching matters: a modality that pins the server cert keeps working after an upgrade. RSA-2048, 10-year validity, notBefore −1h for clock skew, IsCA: true (some old stacks insist), SANs = hostname + localhost + every local IP.
•	A broken TLS config does not take the listener down — it logs and continues plaintext-only, because plaintext modalities are the common case.
Enabled by default: accepting TLS costs nothing when unused (the sniff falls through), so a site enabling Secure DICOM on a modality needs no sender change.
Do not copy these settings to an internet-facing listener. It is defensible only because this listener is on the clinic LAN receiving from hardware on the same switch; the onward hop to the central PACS is a separate connection with modern TLS and certificate verification.
6.2 AE title policy — fully non-restrictive ★ hardened this session
The called AE title is never checked. Any destination AE title associates successfully and is echoed back verbatim in the A-ASSOCIATE-AC. Rationale: the AE title is caller-supplied and unauthenticated, so enforcing it buys no security — it only ever produced called-AE-title-not-recognized rejections when a site's engineer typed the destination name differently from the config.
Verified to associate: exact match, a completely different name, lower case, mixed case, empty called title, empty calling title, both empty, a full 16 characters, punctuation (QUICK-ON_1.2, X-RAY_#2), digits only, internal spaces, leading space. The calling AET (the modality's own name) is equally unchecked.
padAE enforces only the PS3.8 16-byte field width (copy truncates); a dead if len(s) > 16 branch was removed. The configured dicom.aet is display and logging only — the two things that must actually be right on the modality are IP and port.
Editable from Settings (max 16 chars, no character/case validation; whitespace trimmed only because the wire field is space-padded). Saving mirrors it into config.json and re-renders the DICOM chip.
6.3 Tag injection — internal/dicom
Matches the semantics of the legacy tagwrite.lua. On every received instance, when tag_injection.enabled: write four private creator tags at groups 0x0013 / 0x0015 / 0x0021 / 0x0043, element 0x0010, and four corresponding values at element 0x1060 in the same private blocks. Values are lab_id and org_id. This is how the central PACS resolves which lab and organisation a study came from — the same identifiers the notifier sends in its PrivateCreator_* / PrivateOrganisation_* fields.
________________________________________
7. Stability watcher — internal/stability
A modality sends a series as N separate C-STORE operations with no "done" signal. The watcher decides when a series is complete:
•	Touch(seriesUID) / TouchWithHint(seriesUID, instanceCount) per instance.
•	A series is stable once quiet for stable_age_seconds.
•	The hint is the modality-reported expected instance count — when it matches, the quiet period is short-circuited, so a series doesn't idle for the full window.
•	Per-series independent timers.
•	onStable(seriesUID) fires once → enqueue.
Imports only sync and time.
________________________________________
8. Queue — internal/queue
Durable, BoltDB-backed. Producer: stability watcher. Consumer: N goroutines (transfer.concurrent_workers, default 6, poll 500 ms).
QueueEntry{ ID, ResourceLevel, ResourceUID, CreatedAt, NotBefore, Retries, NetworkRetries, LastError }. ResourceLevel is "series", "transcode", or "study".
processOne:
1.	PopReadyQueueEntry(now) — removes it from the store.
2.	For series jobs, ConnCheck(ctx); on error the entry is re-enqueued and the worker backs off to the ticker. (This hook is where the lab gate and the peer watchdog both live.)
3.	Mark resource sending.
4.	Process(ctx, entry).
5.	On success the Processor owns setting the final status (only it knows when every related transfer finished).
Two distinct retry budgets — the important design point:
•	Network errors (EOF, connection reset, timeout — neterror.go) increment NetworkRetries only, do not consume the capped budget, and re-queue with a gentle backoff. A study therefore never lands in Failed because of a temporary outage. Gated by resilience.network_retry.
•	Data/server errors increment Retries; backoff 1s, 5s, 30s, 5m, 30m+; at max_http_retries the resource is marked Failed and the entry dropped.
Statuses: received, stable, queued, pending_transcode/transcoding, sending, delivered, failed. markStatus propagates sending/failed to the parent study so the worklist progress bars key off study status.
Helpers: EnqueueSeries(store, uid), EnqueueTranscode(store, uid).
________________________________________
8.9 One connection can no longer kill the engine
A connection that opened and closed WITHOUT WRITING A BYTE used to take the whole process down: sixteen starts, zero clean exits, thirteen restarts landing 1-2s after "sniff first byte: EOF" (Electron's 1000ms/2000ms backoff). maybeWrapTLS returned (nil, false, err) on the failed sniff; the caller's `conn, isTLS, err := s.maybeWrapTLS(conn)` REASSIGNS conn rather than shadowing it (a parameter shares the function's outermost scope), so the deferred Close() ran on a nil interface — a panic on a per-connection goroutine with nothing to recover it, taking the listener, the queue and every in-flight upload with it. Connections that sent even 8 bytes got past the sniff and failed harmlessly deeper in; only the sniff path was fatal. It fires for any peer that closes without writing — one field X-ray abandoned a socket at the start of every study, but a telnet would have done it too.
Three layers, all pinned by zerobyte_test.go:
1.	maybeWrapTLS never returns a nil conn — on error it hands back the original socket.
2.	handleConnection captures the accepted socket in its own variable before the reassignment, and the defer closes that (plus the wrapper, when there is one, so TLS still gets its close_notify).
3.	The accept goroutine recovers panics, logs at ERROR with a stack (so the error reporter ships it), and drops that one connection. Whatever a single peer provokes, it costs that peer its connection and nothing more.
________________________________________
9. Compression — internal/transcode
Wraps the bundled gdcmconv.exe (GDCM 3.2.7, 32-bit/x86 build — it runs on 32-bit Windows and, via WOW64, on 64-bit Windows too, matching the ia32 Electron app and the GOARCH=386 sender). Two independent modes, mutually exclusive per file:
Mode	Transfer syntax	Default	Notes
Lossless	1.2.840.10008.1.2.4.90 (".90")	ON	Pixel-for-pixel identical, ~40–60% smaller
Lossy	1.2.840.10008.1.2.4.91 (".91")	OFF	Rate-based N:1; discards image data
9.1 The one-flag distinction
gdcmconv --help: -Y --lossy Use the lossy (if possible) compressor. Omitting it selects the reversible wavelet. --irreversible and -r are lossy-only options.
func gdcmconvArgs(plan Plan, in, out string) []string {
    args := []string{"--j2k"}
    if plan.Mode == ModeLossy {
        args = append(args, "--lossy", "--irreversible", "-r", fmt.Sprint(plan.Rate))
    }
    return append(args, "-i", in, "-o", out)
}
A test fails the build if --lossy, --irreversible, or -r ever leak into the lossless path — that single slip would make "lossless" a lie while the UI still said lossless.
9.2 Mode selection
type Settings struct {
    LossyEnabled               bool
    LossyDefaultRate           int
    LossyModalityRate          map[string]int
    LossySkipAlreadyCompressed bool

    LosslessEnabled               bool
    LosslessModalityEnabled       map[string]bool
    LosslessSkipAlreadyCompressed bool
}
type Plan struct { Mode Mode; Rate int; SkipAlreadyCompressed bool }

func PlanFor(s Settings, modality string) Plan
Precedence:
1.	Lossy wins if enabled and the modality resolves to a non-zero rate. An operator who set "CT 10:1" meant it; applying lossless instead would silently ignore that.
2.	Else lossless, if enabled for that modality.
3.	Else ModeNone — sent in original encoding.
lossyRateFor: explicit modality_rate entry wins (including an explicit 0 = disabled), else default_rate. losslessEnabledFor: absent means enabled. Lossless is safe for every modality, so "all of them" is the useful default and the map carves out exceptions only — an empty map must not mean "none". The map is consulted only once compression.lossless.enabled is true, which ships false: both transcode modes are opt-in, so a default install forwards pixel data byte-for-byte.
A stored CT: 10 stays inert while the lossy toggle is off, so a leftover rate cannot quietly activate lossy compression.
9.3 Skip sets differ per mode — deliberately
lossyTransferSyntaxes      // .50 .51 .81 .91 .93   (already LOSSY)
compressedTransferSyntaxes // .50 .51 .57 .70 .80 .81 .90 .91 .92 .93 and 1.2.840.10008.1.2.5 (RLE)
•	Lossy skips only already-lossy files (re-encoding causes a second generation of loss). An uncompressed or losslessly-compressed file is still worth shrinking.
•	Lossless skips anything already compressed: .90 is already the target; another lossless codec gains almost nothing for the CPU; and an already-lossy file cannot be repaired by storing it losslessly — the result is usually larger.
9.4 Integrity guards
Output goes to a sibling .j2k.tmp and is renamed over the original only after all guards pass, so a bad encode can never reach the PACS.
1.	Transfer syntax check — output must be exactly .90/.91 per mode.
2.	Signedness check — (0028,0103) Pixel Representation must not flip signed → unsigned. A known gdcmconv hazard that corrupts CT Hounsfield units.
3.	Lossless-only size guard — entropy coding is not guaranteed to shrink; on noisy or pre-packed images JPEG 2000 can come out larger. Silent inflation would be a bandwidth regression at any site that opts in, so the original is kept whenever the encoded file is not actually smaller.
4.	"could not derive" in gdcmconv output → treated as a skip, not an error. gdcmconv cannot compress non-image objects (protocol docs, raw data, some vendor private encodings) and exits 1.
Non-image modalities are skipped before gdcmconv is even invoked: SR PR KO AU SEG REG RTSTRUCT RTPLAN RTRECORD RTDOSE DOC FID RWV PLAN PMAP.
9.5 The availability guard that makes ON-by-default safe
func Available(path string) bool   // stat (or exec.LookPath for a bare name), THEN actually launch it
Existence is not runnability: the binary is launched once at startup ("gdcmconv --version", 10s timeout) and only a failure to start counts as unavailable — a non-zero exit still proves the image loaded. This catches an architecture mismatch or a missing MSVC runtime DLL, both of which stat fine and then die on every invocation. Without it, an install where gdcmconv can't run would route every series into the transcode queue, fail, and eventually mark studies Failed. main.go therefore builds Settings{} (both modes off) when Available() is false, degrading to "send as-is", and logs one loud startup warning. Electron mirrors this: it writes enabled: false for both modes when gdcmconv.exe is absent.
Concurrency: transcode jobs take a slot from runtime.NumCPU()/2 (min 1) so CPU-bound work doesn't starve the network-bound upload workers sharing the pool.
ResolveGdcmconv(configPath): explicit path → directory of the running executable → bare gdcmconv[.exe] for PATH.
9.6 Per-modality lossless policy — CT, MR, CR and DX all on
config.DefaultLosslessModalityPolicy() is the single Go-side definition; LOSSLESS_MODALITY_POLICY in electron/main/main.js is its twin; examples/config.example.json is the third copy. lossless_policy_test.go fails the build if any of them drifts.
CR and DX were switched OFF on 2026-08-29, after every transcoded DX series was refused by the receiving Orthanc — HTTP 400, empty ReferencedSOPSequence, nothing stored — while an untranscoded series from the same unit delivered normally. They are ON again, to be retested: the bundled encoder has since gone from GDCM 3.2.6 (x64) to 3.2.7 (x86), so if the rejection was a 3.2.6 encoder bug it is no longer in play. Transcoding was never confirmed as the cause anyway — the same unit was separately sending one SOP Instance UID under two different Study UIDs, which Orthanc refuses outright and which nothing on the sender side can fix.
To revert: set CR and/or DX to false in all three places above.
Landing a policy on existing installs: the map records exceptions only, so a `?? {}` default in the Electron bootstrap would never reach an install that already had a stored map — every install that has opened Settings. The bootstrap uses Object.assign({}, stored, POLICY), which forces the policy entries and leaves any other per-modality choice intact. On the Go side, Load decodes the file on top of Default() and encoding/json MERGES into the existing map, so the policy survives for every modality the stored file does not name, while an operator's explicit stored value still wins for the ones it does.
9.7 Instances with no pixel data — internal/transcode/pixeldata.go
Siemens raw-data objects (Raw Data Storage / CSA non-image) ride inside an ordinary CT study carrying Modality=CT, so the modality-level NoPixelModality guard never sees them. They have no pixmap, gdcmconv exits 1 with "Could not read (pixmap)", and transcoder.go only tolerated the string "could not derive" — so it fell through to a returned error, a RETRYABLE failure. One such instance parked its whole study at FAILED forever, retrying two files that can never succeed, while every real image series in the study had already delivered.
Three layers:
1.	hasPixelData(path) walks the dataset for (7FE0,0010) BEFORE gdcmconv is launched. Absent → skip, file passes through untouched, no process started.
2.	isNonImageFailure(out) is the backstop for whatever the probe could not decide: matches "could not read (pixmap)", "could not find pixmap", "no pixel data" and the original "could not derive", case-insensitively. Everything else — permission denied, no space, a killed process — stays retryable.
3.	The existing NoPixelModality modality check, unchanged.
The probe's contract is deliberately ASYMMETRIC — do not "simplify" it. A definite false is returned only after a clean walk all the way to EOF. Anything ambiguous (an encoding it does not read, a truncated file, an implausible VR, a length that overruns) returns errUncertain and the caller falls through to gdcmconv, which is the authority on what it can encode. A wrong false would silently stop compressing real images fleet-wide with nothing going red; a wrong "uncertain" costs one process launch.
Two details worth keeping:
•	Every seek is bounded against file size. Seeking past EOF is legal and silently succeeds, so an element whose declared length overruns the file walked straight off the end and returned a clean "no pixel data" — the exact dangerous case. It has its own test.
•	Group 0xFFFE (item and delimiter tags) is ALWAYS implicit-style — bare 4-byte length, no VR — even inside an Explicit VR dataset. This is the detail that desynchronises a naive walker the moment it steps into a sequence. Defined-length items are skipped whole rather than walked into, because the items inside encapsulated pixel data hold raw fragments, not elements.
A SOP-class allowlist was considered and REJECTED: the .66 family is a trap — 1.2.840.10008.5.1.4.1.1.66 is Raw Data (no pixels) but .66.4 Segmentation does carry pixel data, so a prefix match would silently disable compression on valid images.
Tested in pixeldata_test.go: Explicit/Implicit VR, undefined-length sequences before and after Pixel Data, nesting, defined-length items and sequences, encapsulated JPEG 2000, malformed input — plus TestRawDataInstanceBypassesGdcmconv, which passes a deliberately bogus binary path to prove no process is launched and the file is byte-for-byte unchanged, and its keeper TestImageInstanceStillReachesGdcmconv, which proves the same bogus path DOES fail when there is pixel data.
________________________________________
10. Transfer — internal/transfer
Pushes a series at a time to the peer.
&transfer.Client{
    PeerURL, Username, Password, CACertPath string
    Timeout      time.Duration   // http_timeout_seconds (600)
    Concurrency  int             // transfer.concurrent_workers
    BucketSizeMB int             // chunk size, auto-tuned
    Protocol     PushProtocol    // "stow-rs" (default) | "instances"
    CheckpointEnabled func() bool
    OnStudyDelivered  func(studyUID string)
    OnSeriesProgress  func(SeriesProgress)
}
•	stow-rs — POST {peer}/dicom-web/studies, multipart, HTTP basic auth. Requires the dicom-web plugin on the receiver. Instances are grouped into chunks of ~bucket_size_mb; the log reports chunks, max_concurrency, bucket_mb, then per-chunk size_mb, elapsed, mbps, workers. The STOW response manifest is parsed and a non-empty FailedSOPSequence is treated as an error.
•	instances — Orthanc's native POST /instances, works against any Orthanc.
•	Instance checkpointing (resilience.instance_checkpoint) — records which instances were already delivered, so a retry after a network failure re-sends only the remainder, not the whole series.
•	Simultaneous series uploads capped at 2 (seriesUploadSlots) — deliberate. More than 2 splits bandwidth too thin on asymmetric links (e.g. 10 Mbps upload in India) and caused server-side timeouts (EOF). Two saturate the uplink while keeping the pipe full.
•	OnStudyDelivered fires when every series of a study is delivered; main.go captures the study snapshot immediately and posts the delivered notification 15 s later via time.AfterFunc, so the transfer path is never blocked.
•	OnSeriesProgress feeds api.ProgressManager (sending → Start/Update, complete → Complete, failed → Fail).
Connectivity watchdog — internal/connectivity
GET {peer}/system every 30 s with basic auth; online = status < 500. Probes immediately at startup so the first upload doesn't wait 30 s. WaitUntilOnline(ctx) blocks the queue worker while offline (wake channel + 5 s re-poll) rather than burning retry budget. Gated by resilience.connectivity_watchdog.
________________________________________
11. Lab activation gate — internal/labstatus ★ built this session
The requirement: before sending the notifier and the study, ask the backend whether the lab is active. If active, continue normally; otherwise flag a banner saying the account is inactive and to contact Achyu.
11.1 Request
POST https://pacs.achyutrs.com/api/sender/lab-status
Content-Type: application/json
Accept: application/json
Authorization: Bearer <lab_status.api_key>     ← only when configured
{ "lab_id": "ARX1", "org_id": "ARX", "hostname": "RECEPTION-PC",
  "agent_version": "0.1.1", "reason": "notify" }
reason ∈ startup | poll | notify | delivered | upload | manual — informational.
11.2 Response the backend should return
{ "success": true, "active": false, "status": "suspended",
  "message": "Your subscription has expired. Please contact Achyu." }
active is the only field that must be right. message is shown to the operator verbatim in the banner — write it for a receptionist. Default when omitted: "Your account is inactive. Please contact Achyu."
11.3 Interpretation ladder (first match wins)
#	Condition	Verdict
1	HTTP 402, 403, 423, 451	inactive (deliberate refusal; body message still used)
2	HTTP 5xx, 404, 429, timeout, DNS failure, non-JSON body	unknown
3	active / is_active / lab_active / data.active boolean	that boolean — wins over everything
4	status string	mapped through the tables below
5	only success	true → active, false → inactive
6	200 with nothing recognisable	unknown
•	active labels: active enabled live ok valid approved running
•	inactive labels: inactive disabled suspended expired blocked banned terminated cancelled canceled unpaid overdue deleted pending not_found notfound unauthorized
•	case-insensitive; a data: {…} envelope is unwrapped.
Aliases exist so an existing controller can be reused unchanged; new work should just return active.
11.4 Failure policy — deliberately asymmetric
Wrongly blocking a working lab loses studies (the retention sweeper deletes them in 24 h); wrongly allowing a lapsed one costs a few uploads. Therefore:
•	"inactive" is only ever concluded from an answer the backend actually gave. It then persists until the backend says otherwise.
•	Unreachable backend = unknown, not inactive. Unknown keeps the last known verdict. With no verdict yet, fail_open (default true) decides.
•	Do not return 404 for an unknown lab if you mean "block it" — 404 reads as "route missing" and falls through to the failure policy. Return 200 {"active": false} for an explicit block, or 5xx if you genuinely couldn't answer.
•	"fail_open": false gives a deny-by-default posture.
11.5 API
func New(backendURL, apiKey, labID, orgID, hostname, agentVersion string) *Checker
func (c *Checker) Enabled() bool
func (c *Checker) Snapshot() Verdict
func (c *Checker) Start(ctx)                                  // immediate check + poll loop
func (c *Checker) Allowed(ctx, reason) (bool, string)          // refreshes if stale
func (c *Checker) Refresh(ctx, reason) Verdict                 // FORCES a check
func (c *Checker) GateUpload(ctx) error                        // nil, or ErrInactive
Verdict{ Active, Known, Status, Message, CheckedAt, HTTPStatus, LastError, Enabled }
•	Empty BackendURL = disabled checker; Allowed always true, zero HTTP traffic.
•	Verdict cached 60 s (StaleAfter); the ingest path reuses it rather than calling the backend per instance. ~1 request/minute/workstation.
•	Concurrent refreshes collapse onto one HTTP request via an inflight WaitGroup. The request context is rooted at context.Background() (not the caller's) so whichever caller won the race can't cancel the check everyone is waiting on.
•	Refresh vs Allowed matters: Allowed may legitimately answer from cache. The app's "Check again" button must call Refresh, or it appears to do nothing right after an account is reactivated. (Windows' coarse monotonic clock made age > stale false for same-tick calls, which is how this surfaced.)
•	GateUpload waits up to InactiveHold (30 s), re-checking every 5 s clamped to the remaining hold, then returns ErrInactive. It does not block forever, because the queue entry has already been popped — holding it in memory indefinitely would lose it if the process died.
•	Blocked-transmission warnings are rate-limited to one per minute (the log file is itself shipped to the backend).
11.6 What "inactive" actually does
Behaviour	While inactive
Receive DICOM from modalities	continues — nothing refused or lost
Store studies locally	continues
Study notification	held — parked in memory (max 500), sent after reactivation
Delivered notification	held — same
STOW-RS upload	held — queue entry re-enqueued, retry budget untouched
App banner	"Your account is inactive — contact Achyu" + counts on hold
Nothing is marked failed. Deactivation takes effect within ~60 s; reactivation resumes within ~60 s or immediately via "Check again".
11.7 Wiring
studyNotifier.Gate = func() (bool, string) {
    return labChecker.Allowed(context.Background(), "notify")
}

ConnCheck: func(ctx context.Context) error {
    if err := labChecker.GateUpload(ctx); err != nil { return err }  // gate FIRST
    if !cfg.Resilience.ConnectivityWatchdog { return nil }
    return connWatcher.WaitUntilOnline(ctx)
}
The gate goes first: an inactive lab must not upload whether or not the peer happens to be reachable.
11.8 Observability (added after the gate looked "not running")
A healthy gate used to log only at Debug, making it indistinguishable from one that never ran. Now:
•	lab activation gate ENABLED url=… poll_seconds=… fail_open=… lab_id=… at startup — and a WARN if disabled, since that's the state where studies transmit unchecked.
•	Heartbeat: an unchanged verdict is restated at Info every 10 minutes with a checks=N counter, so any log tail proves it's polling.
•	lab_gate=passed on NOTIFIER firing / NOTIFIER delivered firing, so the check is visible per study, not only when it blocks.
•	msg=ready carries dicom_tls, lossy_compression, lossless_compression, lab_gate.
________________________________________
12. Notifications — internal/notifier
12.1 Study notification
Fired once per study, the moment its first instance arrives, so the study appears on the PACS as upload_pending before the upload finishes.
{ "StudyInstanceUID": "…", "PatientID": "…", "PatientName": "DOE^JANE",
  "StudyDate": "20260808", "StudyTime": "141530",
  "StudyDescription": "CT CHEST", "AccessionNumber": "ACC001",
  "Modality": "CT", "status": "upload_pending",
  "PrivateCreator_0013": "ARX1", "PrivateCreator_0015": "ARX1",
  "PrivateOrganisation_0021": "ARX", "PrivateOrganisation_0043": "ARX" }
Field names match the legacy Lua OnStoredInstance payload so the server handler needs no changes; the private-tag fields carry lab_id/org_id and are how the handler resolves org and lab. De-duplicated per study for the process lifetime.
12.2 Delivered notification
URL derived from the notifier URL by replacing its last path segment with delivered. Fired ~15 s after every series of a study is on the peer. Not de-duplicated — a re-delivered study notifies again, by design.
{ "OrthancID": "029a3eb9-9aa2daa7-…", "StudyInstanceUID": "…",
  "PatientID": "…", "status": "delivered",
  "lab_id": "RNMUZ", "org_id": "XXZQ", "delivered_at": "2026-08-08T22:32:47Z" }
OrthancID reproduces Orthanc's public study ID exactly: SHA1("PatientID|StudyInstanceUID") → lower-case hex → five dash-separated 8-char groups. So it matches the resource ID on the receiving Orthanc with no lookup.
Observed live response: {"success":true,"studyId":"…","bharatPacsId":"…","deliveryCount":1,"promoted":true}.
12.3 Gate + parking ★ built this session
type LabGate func() (bool, string)   // nil gate = always open
A refused notification is parked, not dropped — otherwise a lab suspended for an afternoon would lose every study it received in that window (the retention sweeper would eventually delete them and the PACS would never have heard of them).
•	pending []pendingPost in arrival order; maxPending = 500, oldest dropped when full (keeps memory flat on a workstation left running for weeks).
•	StartPendingFlusher(ctx, 30*time.Second) retries; flushPending stops at the first refusal so ordering is preserved, and re-parks at the front on a transport failure rather than losing the notification.
•	PendingCount() feeds /api/status → the banner's "on hold" counts.
Both POSTs run in a goroutine so ingest is never blocked. Non-2xx is an error. Every payload is logged before sending (NOTIFIER sending payload).
________________________________________
13. Local HTTP API — internal/api
Binds 127.0.0.1:9044 only. Bearer-token auth against http.api_token (random 24 bytes hex, generated by Electron, written into config, passed to the renderer via preload — the renderer never touches disk). Stdlib net/http, no router framework. CORS enabled for dev browser access.
Route	Methods	Purpose
/api/health	GET	unauthenticated liveness: status, uptime, version
/api/status	GET	header pill + footer + banner
/api/worklist?status=&limit=	GET	study list (limit ≤ 1000, default 100)
/api/study/{uid}	GET / DELETE	detail (study + series) / force-delete locally
/api/study/{uid}/retry	POST	re-enqueue every non-delivered series
/api/queue/depth	GET	footer "in flight"
/api/config	GET / PATCH	full config; PATCH deep-merges, Normalize, Validate, Save
/api/transfer/{seriesUID}/progress	GET	live progress
/api/lab-status	GET / POST	gate verdict / force refresh
/api/status returns: running, version, started_at, uptime_s, lab_id, org_id, dicom_port, http_port, peer_name, peer_url, queue_depth, hostname, lab_active, lab_status, lab_status_message, lab_status_known, lab_gate_enabled, pending_notifications. The lab_* fields ride along here on purpose — the renderer already polls this every 5 s, so the banner needs no extra request.
PATCH /api/config deep-merges maps rather than replacing them. Consequence: an omitted map key keeps its previous value, so a "record only the exceptions" payload can never clear an exception. This is why the renderer writes all eight modalities explicitly for both modality_rate and lossless.modality_enabled. It also re-applies Normalize() so a patch can't route around the invariants.
ProgressManager (progress.go) tracks per-series total_bytes, bytes_sent, total_instances, instances_sent, percent, elapsed_s, eta_s, status.
________________________________________
14. Encryption & secrets ★ built this session
Windows-only crypto (DPAPI + Chromium OSCrypt). Every layer falls back to plaintext on non-Windows dev builds so the app still runs.
File	Written by	Read by	Format
session.json	Electron only	Electron only	safeStorage raw binary v10 blob
config.json (whole file)	Electron and Go	Electron and Go	base64 text of a v10 blob (or DPAPI, or plaintext)
legacy encrypted_* fields	Go (old)	Go (old)	base64 raw-DPAPI, superseded, read-only
Paths: %APPDATA%\<electron-app-name>\achyu\config.json; Local State (holds the OSCrypt AES key) is at %APPDATA%\<electron-app-name>\Local State — i.e. two directories up from config.json, then Local State.
14.1 DPAPI — internal/dpapi
CryptProtectData / CryptUnprotectData, CurrentUser scope, no entropy, NULL prompt struct (so it can never show UI on a headless workstation).
func Available() bool                              // real round-trip probe
func Encrypt(plain string) (string, error)         // → base64(blob)
func EncryptBytes(plain []byte) ([]byte, error)    // → raw blob
func Decrypt(encryptedBase64 string) (string, error)
func DecryptBytes(encrypted []byte) ([]byte, error)
A DPAPI blob always begins 01 00 00 00 D0 8C 9D DF, so its base64 begins AQAAANCMnd8 — that signature identifies a Go-written blob.
runtime.KeepAlive is required around the syscalls so the GC can't move or collect the buffer while Win32 holds a raw pointer; output blobs are LocalAlloc and must be copied then LocalFreed.
Security property, with a consequence: ciphertext is bound to the logged-in Windows user on this machine. A config.json/session.json cannot be copied between users or machines — decryption failing after such a copy is correct behaviour, not a bug. Operators log in once after an upgrade.
14.2 Chromium OSCrypt "v10" — internal/oscrypt
This is what safeStorage actually emits on Windows. It is NOT raw DPAPI.
"v10" || 12-byte GCM nonce || AES-256-GCM ciphertext (16-byte tag appended)
The key is not in the blob. It lives in Local State JSON under os_crypt.encrypted_key: base64 → "DPAPI" (5 ASCII bytes) prefix → a raw DPAPI blob wrapping the 32-byte key.
const V10Prefix = "v10"; const KeyLen = 32   // nonce 12, tag 16
func HasV10Prefix(raw []byte) bool
func LoadKey(localStatePath string) ([]byte, error)
func DecryptV10(raw, key []byte) (string, error)
func EncryptV10(plain string, key []byte) ([]byte, error)
crypto/cipher's NewGCM defaults to a 12-byte nonce and 16-byte tag — exactly Chromium's parameters — so Go's Seal/Open are wire-compatible with safeStorage given the same key. Only LoadKey needs Windows; the AES-GCM half is pure stdlib and tests everywhere.
14.3 ⚠ The interop trap — do not repeat
An earlier release assumed safeStorage output was raw DPAPI and had Go call CryptUnprotectData on the base64-decoded bytes → "DPAPI decrypt failed: The data is invalid." crash loop. A .NET ProtectedData round-trip test looked like it proved interop, but .NET ProtectedData is raw DPAPI — a false proxy for safeStorage. The fix was implementing the real v10 scheme in Go.
Once v10 exists on both sides:
Producer	Blob	Go reads?	Electron reads?
Electron safeStorage	v10	✅ DecryptV10	✅ native
Go EncryptV10 (Local State present)	v10	✅	✅ native
Go dpapi.Encrypt (no Local State)	raw DPAPI	✅ DecryptBytes	✅ safeStorage's DPAPI fallback
safeStorage.decryptString transparently reads both, because Chromium's DecryptString falls back to DPAPI when the input lacks the "v10" prefix.
14.4 Format detection ladder (both sides implement it identically)
first non-space byte == '{'   → plaintext JSON
else base64-decode, then:
    starts with "v10"         → AES-256-GCM via the Local State key
    else                      → raw DPAPI
JSON always starts { and base64 never can, so one byte disambiguates. trimStart / bytes.TrimLeft also strips a UTF-8 BOM, so a hand-edited config isn't mistaken for ciphertext.
14.5 Go Load / Save
Load: read → decryptConfig → unmarshal on Default() → legacy per-field decrypt → Normalize → Validate.
Save (value receiver, so blanking is local to the serialised copy): blank legacy encrypted_* → MarshalIndent → encryptConfig → atomic .tmp + rename, mode 0600.
encryptConfig preference order — v10 first, because Electron reads it natively; then raw DPAPI; then plaintext as a last resort (so a non-Windows dev build still persists rather than failing outright).
func localStatePath(configPath string) string {
    // <userData>/achyu/config.json → <userData>/Local State
    return filepath.Join(filepath.Dir(filepath.Dir(configPath)), "Local State")
}
If the sender is launched with a --config outside the Electron userData layout, LoadKey won't find the key and Save falls back to raw DPAPI — still readable by both sides.
main.go re-saves immediately after Load, which normalises a plaintext or foreign-format config to ciphertext on first run. Deliberately placed before autoTuneConfig and the --data-dir override, so RAM-derived tuning and a one-off CLI flag aren't baked into the file. It logs config re-encrypted at rest from=… to=…, then config encrypted at rest format=… — or a WARN if the result is plaintext (on Windows that means both OSCrypt and DPAPI failed and secrets are readable).
14.6 Electron side
const { safeStorage } = require('electron');

function readConfig()        // '{' → JSON; else safeStorage.decryptString(Buffer.from(t,'base64'))
function writeConfig(cfg)    // safeStorage.encryptString(json).toString('base64'), atomic, 0600
function readSession()       // plaintext-tolerant; else safeStorage.decryptString(raw buffer)
function writeSession(s)     // raw Buffer (NOT base64), atomic, 0600
config.json is base64 text (Go reads it, and the leading byte is the format discriminator); session.json is the raw Buffer (only Electron reads it). All touchpoints route through these: bootstrapSender read+write, tarang:update-config read+write, the tarang:login existing-config read, and session read/write.
The generic plaintext readJSON/writeJSONAtomic helpers were deleted — both files this process persists hold secrets, and leaving a general-purpose plaintext writer beside them is how cleartext credentials quietly return in a later edit.
session.json holds the login token, lab/org IDs, and peer credentials.
14.7 UI access gate — not encryption
A hard-coded passcode gates Settings, Connection, Logs, and the login-screen advanced-settings panel. Current value 18967 (was 1432).
•	electron/renderer/app.js: const TAB_PASS = '18967'; (RESTRICTED_TABS = {settings, connection, logs})
•	electron/renderer/login.js: const SETTINGS_PASSWORD = '18967';
Keep the two in sync. This is UI access control, not cryptography — it ships in plaintext inside the renderer bundle.
________________________________________
15. Retention & error reporting
internal/retention — sweeps hourly, deletes delivered studies older than retention_hours (default 24) from this workstation: DB records + files. Failed studies are never auto-deleted. DeleteStudy(store, dataDir, uid) also backs the API's force-delete. Already-delivered instances are not removed from the central PACS.
internal/errorreport — installed as the log sink before any other subsystem starts, so early failures are reported too. Captures every error-level record, spools it under <data_dir>/error-reports (survives restarts), and POSTs with device + lab identity: New(url, apiKey, version, dataDir, CollectDeviceInfo(), LabInfo{LabID, OrgID, PeerName, PeerURL}). Not gated on lab activation — you want to hear about a broken workstation whether or not its account is paid up. See PACS_ERROR_LOG_API.md for the payload.
internal/log — golang.org/x/exp/slog, stdout + daily-rotated <data_dir>/logs/tarang-YYYY-MM-DD.log, format t=HH:MM:SS.mmm level=INFO msg="…" key=value. SetErrorSink hooks the reporter.
________________________________________
16. Electron main process
Bootstrap (bootstrapSender({labId, orgId, peer, apiUrl}))
Called after login, and again at startup when a session exists. Creates dirs, reads existing config (existing user choices win), writes the merged config, then restarts the sender.
deriveApiBase(apiUrl) — everything follows the login URL up to /api; fallback https://pacs.achyutrs.com/api.
Default peer when the login response doesn't supply one: { name: 'ACHYU', url: 'https://router.achyutrs.com', username: 'Xcentic', password: 'XcenticIngestion123', compression: 'gzip', protocol: 'stow-rs' }. Credentials carried over from the previous deployment — if Achyu issues new receiver credentials they belong in the login response, not baked into the installer. normalizePeer strips empty strings so stored blanks don't mask defaults. Peer username/password already in config are preserved, because the auth API returns none and taking them blindly would blank them on every login.
Process supervision
•	Spawns tarang-sender.exe --config <path>, pipes stdout/stderr.
•	taskkill /F /IM tarang-sender.exe before spawning — kills orphans from an unclean previous session that would otherwise hold the HTTP port with a stale token and hang the frontend on "Connecting…" forever.
•	Crash restart with exponential backoff capped at 30 s; attempt counter resets after 30 s of uptime; suppressRestart for intentional stops.
•	Stop = SIGTERM (lets it drain associations), SIGKILL after 10 s.
IPC (preload.js → window.tarangAPI)
ready (bootstrap promise: token, httpPort, dicomPort, aet, dataDir, stableAgeSeconds, retentionHours, localIPs), updateConfig, login, logout, isAuthenticated, getSession, readRecentLogs.
tarang:update-config accepts stable_age_seconds, dicom_port, aet, retention_hours, updates config.json and the live bootstrap object. tarang:read-recent-logs tails the last ~1 MB of today's log, ≤2000 lines.
localIPs = every non-loopback IPv4, shown in the worklist DICOM chip so an engineer can configure the modality.
Window & tray
•	Single-instance lock; second instance restores + focuses the window.
•	Auto-start at Windows login (setLoginItemSettings, name Achyu PACS).
•	1400×900, no application menu, icon electron/logo.ico.
•	X minimizes to the taskbar — event.preventDefault() + mainWindow.minimize(). Minimize, not hide: hiding removes it from the taskbar, which reads as "the app quit" and sends operators hunting the tray or relaunching, while the sender keeps running invisibly. (Before this change the handler only called preventDefault(), so X did nothing at all.)
•	Tray: Open / Quit. Quit sets app.isQuitting so the close handler steps aside. All three restore paths do isMinimized() → restore().
________________________________________
17. Renderer UI
index.html + app.css + api.js + progress-modal.js + app.js. (settings-form.js exists but is not loaded by index.html — dead code.)
Four tabs: Worklist (open), Settings / Connection / Logs (passcode-gated).
•	Worklist — DICOM chip (AET, port, click-to-copy IPs), search, status filter, refresh. Table: Patient (monogram + name + patient_id · modalities), Accession, Study Date/Time, Series/Instances, Status, Study UID (truncated). Auto-refresh every 3 s while visible; paused when hidden. Rows with status sending/transcoding get an inline progress bar (indeterminate shimmer for transcoding); aggregated per study from per-series polling every 1.5 s, updated in place to avoid re-render flicker. Click a row → detail drawer (metadata, per-series list, Retry, Force delete).
•	Settings — Identity (lab/org/hostname + account status), PACS Receiver (URL/user/pass; blank password keeps current), DICOM (AE title, port, stable age), Storage & retention, Secure DICOM (read-only), JPEG 2000 Compression (lossy toggle + skip + default ratio + 8-modality ratio grid), JPEG 2000 Lossless (.90) (toggle + skip + 8-modality grid), Network Resilience (3 toggles). Save / Revert. Ratio controls grey out while their mode is off, so nobody tunes a slider that does nothing.
•	Connection — KPIs (receiver, queue depth, uptime, studies/24h), ping.
•	Logs — level filter, refresh, pause auto-refresh (2 s).
Account-inactive banner — full width between top bar and content, so it can't be scrolled past or mistaken for a toast. Driven by lab_active === false on /api/status; shows lab_status_message verbatim plus "On hold: N notifications, M transfers"; "Check again" calls POST /api/lab-status. Deliberately not cleared when the local sender is unreachable — that says nothing about the account, and flipping the banner on every hiccup trains operators to ignore it. lab_active !== false (i.e. absent, from an older engine) is treated as fine rather than showing a scary banner with no evidence.
api.js wraps the local API, re-fetches bootstrap on 401, and has a dev fallback (?token=&port=) for iterating on the UI in a plain browser.
Branding
•	Palette: indigo carries the UI (--accent #4b3bd8, active tab, primary button, focus); amber (--brand-x) is reserved for the "On" of the Achyu wordmark and the inactive banner, so both read as "look here". Full dark-mode variable set.
•	Login: split screen — dark indigo panel (--panel #0a0e27) with an animated QUICKO + amber N wordmark, cycling taglines ("Every study reaches the central PACS." / "Encrypted or plain, one port." / "Your modalities, bridged.") — and a cream auth panel. Fonts: Schibsted Grotesk, Spectral italic, JetBrains Mono.
•	The logo image sits in the navbar (.brand-logo-img, 24px) and at the top of the login brand panel (.logo-img, 48px), both sourced from electron/logo.png. logo.ico is the window / tray / installer icon.
App icon — generate-ico.js
logo.png in the repo root is the single source for every icon asset; nothing is derived from anything else. The pipeline is lossless end to end — PNG is a lossless format and stays one: full 8-bit RGBA (colorType 6), no palette quantisation, no dithering, no JPEG anywhere. deflateLevel 9 is zlib's lossless maximum, which only shrinks the file.
1.	electron/logo.png and electron/app.png are byte-for-byte copies of the source — copied, never re-encoded.
2.	.ico entries must be square, and the source is 1334x1067 with transparent margins. So the artwork is cropped to its opaque bounding box (alpha > 8; 795x941 on the current file) and centred on a transparent square of side max(w,h) x 1.08. Cropping to content rather than padding the raw canvas is what keeps the mark from drifting off-centre, and the 4% margin per side stops a tray icon looking cramped.
3.	Seven sizes are rendered from that one square canvas — 256/128/64/48/32/24/16, each a bicubic resize of the full-res canvas rather than a resize-of-a-resize. 256 is required by electron-builder.
4.	png-to-ico packs them into electron/logo.ico and electron/app.ico (identical files, kept in sync).
Uses jimp 1.6.1 + png-to-ico 3.0.1 (already in devDependencies). Run with npm run icons.
Windows caches shortcut/taskbar icons aggressively — after reinstalling you may still see the old icon until ie4uinit.exe -show or a re-login.
________________________________________
18. Startup & shutdown order (main.go)
Load config → re-save (normalise encryption) → auto-tune → log.Init
→ error reporter (installed as log sink FIRST)
→ lab status checker → study notifier (+ gate)
→ BoltDB store → progress manager → transfer client
→ resolve gdcmconv + availability → connectivity watcher
→ queue worker → stability watcher → SCP handler
→ build DICOM TLS config → SCP server → API server → retention sweeper
→ errReporter.Start → connWatcher.Start → labChecker.Start
→ notifier.StartPendingFlusher → queue worker goroutine
→ scpServer.Start → apiServer.Start → sweeper.Start → log "ready"
→ wait SIGINT/SIGTERM
Shutdown drains in reverse so each layer finishes in-flight work before its dependency closes:
API stop (5s) → sweeper stop → SCP stop (20s) → watcher stop
→ cancel root ctx (queue worker) → wait → store close
________________________________________
19. Build & release
build.bat        :: THE build script — ALWAYS builds both halves together
1.	Check Go + Node; read version from package.json.
2.	Clean dist\ and tarang-sender.exe (guarantees no stale artifact ships).
3.	set GOTOOLCHAIN=go1.20.14 / GOARCH=386 / CGO_ENABLED=0 go build -ldflags "-X main.version=%APP_VER% -s -w" -o tarang-sender.exe ./cmd/tarang-sender/
4.	Verify go version tarang-sender.exe contains go1.20.14 — fails loudly otherwise, because losing the toolchain override silently produces a Win7-incompatible binary.
5.	npm install
6.	npx electron-builder --win --ia32
build/installer.nsh:
•	killSender — kills the UI FIRST ("Achyu PACS.exe", plus the previous name), then tarang-sender.exe, then gdcmconv.exe, then sleeps 2s. Electron holds Cache, Local State, Local Storage and Network open inside the very profile folder purgeAppData deletes, and the Go sender — a child of Electron, not always reaped — holds tarang.db, the log file and its own binary. Either one makes RMDir /r fail SILENTLY (NSIS does not fail an uninstall for a directory it could not remove), which is why an upgrade kept the stale exe and an uninstall kept the profile.
•	purgeAppData on genuine uninstall only (preserved on ${isUpdated}) — removes current + every legacy %APPDATA% name (Achyu PACS, achyu-pacs, QuickOn PACS, quickon-pacs, ArihantX PACS, arihantx-pacs, QuickLine Router, QUICKON, tarang-sender, Tarang, vrindapacs, BhratPacsExe, BharatPACS Sender, SureScan Sender, UEvolveAI Sender) and the updater caches.
•	…and it runs that whole list under BOTH shell contexts. perMachine:true means the uninstaller runs with SetShellVarContext all, under which NSIS resolves $APPDATA to C:\ProgramData — NOT Roaming. Every RMDir /r used to target a path that had never existed, report success, and leave the real profile (…\AppData\Roaming\achyu-pacs\achyu\data — the data_dir the engine's own log prints) completely intact. Both folder conventions are purged per name too: Electron derives userData from package.json "name" (the kebab form, which is what is actually observed on disk), while "productName" is the documented behaviour.
•	Caveat that cannot be fixed from the uninstaller: an elevated uninstall runs as the ADMIN account, so "current" is the admin's profile. If a different account installed and used the app, its Roaming folder is unreachable from the uninstaller and must be deleted by hand.
•	RENAME TRAP: a project-wide find-and-replace eats the legacy name list, silently dropping uninstall cleanup for every install already in the field. Preserve the old names and add the outgoing one; the same applies to the taskkill list in killSender.
extraResources ships tarang-sender.exe, gdcmconv.exe, all gdcm*.dll, msvcp120.dll, msvcr120.dll, socketxx.dll.
Build gotchas (all hit this session)
•	C: disk space. The Go linker writes to %TEMP% on C:. At ~70 MB free it failed with resize output file failed: … not enough space on the disk; later it hit 0 bytes and broke tooling entirely (and probably caused the source truncation). Workaround used: GOTMPDIR/TMP/TEMP → a dir on D:. build.bat does not do this — free C: instead.
•	Never go mod tidy (see §1).
•	Local Go is 1.26.2, which refuses go 1.20 in go.mod because bbolt declares 1.21. Run tests with GOTOOLCHAIN=go1.20.14 — that works with the file unchanged and matches the shipping toolchain. (Earlier the directive was temporarily bumped and restored; the GOTOOLCHAIN approach is strictly better.)
•	go build ./cmd/tarang-sender/ without -o writes into the CWD and clobbers the shipped exe (and with default GOARCH it's an amd64 build). Always -o somewhere disposable for check builds.
•	go build ./... also trips over stray root scratch .go files.
•	A literal BOM mid-file is illegal in Go source — write "\uFEFF" in the TrimLeft cutset, never the character.
•	Crypto tests must run on Windows for DPAPI / Local State to be real.
________________________________________
20. Tests
Package	Test	Pins
scp	TestAssociateAndCEcho	full A-ASSOCIATE + C-ECHO + release round trip
scp	TestAssociateAcceptsAnyCalledAET	a mismatched called AE title is accepted and echoed
scp	TestAETitleIsFullyUnrestricted	12 cases (empty, lowercase, punctuation, 16-char, spaces…)
scp	TestPadAETruncatesAtFieldWidth	an over-long title can't overflow the 16-byte field
scp	TestDualTransportOnOnePort	TLS and plaintext both associate on one port
scp	TestLegacyTLS10ClientIsAccepted	TLS 1.0 + AES_128_CBC_SHA client accepted
scp	TestSelfSignedCertIsCachedAndReused	cert is stable across restarts
scp	TestTLSMinVersionAndLegacyCiphers	MinVersion TLS1.0, CBC/3DES present, RC4 absent unless opted in
transcode	TestPlanForPrecedence	11 cases of lossy-vs-lossless selection
transcode	TestGdcmconvArgsPerMode	--lossy/--irreversible/-r never leak into lossless
transcode	TestSkipSetsDifferPerMode	7 transfer syntaxes × both modes
transcode	TestLosslessRoundTripIsBitExact	encode → --raw decode → pixel bytes identical; .90; smaller
transcode	TestLossyRoundTripProducesLossy91	lossy lands on .91
transcode	TestAvailable	the missing/unrunnable-gdcmconv guard (incl. a present-but-not-executable file)
labstatus	TestInterpretResponseMatrix	~26 response shapes — the executable backend contract
labstatus	TestUnreachableBackendFailsOpen / FailClosed	the outage policy both ways
labstatus	TestKnownVerdictSurvivesOutage	a 5xx can't upgrade known-inactive to active
notifier	TestGateParksAndFlushes	a study received while inactive is announced after reactivation
notifier	TestDeliveredIsGatedToo, TestPendingBacklogIsCapped	delivered parking; 500 cap
config	TestElectronWrittenConfigRoundTrips	the JS↔Go seam — every key binds
config	TestCompressionDefaultsOffButIsOperatorControlled	off by default, explicit true honoured, survives save/reload
config	TestSaveEncryptsAndLoadRecovers	no secret readable on disk; every field recovered
config	TestGoReadsWhatElectronWrites	the §14.3 interop trap
config	TestElectronCanReadWhatGoWrites	reverse direction
config	TestPlaintextConfigStillLoadsAndIsUpgraded	upgrade path
config	TestDPAPIFallbackWhenNoLocalState, TestLegacyPerFieldDecryption, TestBOMAndWhitespace…, TestDetectFormatLadder, TestCorruptConfigFailsLoudly	
oscrypt	TestEncryptDecryptRoundTrip, TestNonceIsFresh, TestDecryptRejectsTamperingAndWrongKey, TestLoadKeyAgainstSynthesizedLocalState	real DPAPI-wrapped key
TestElectronWrittenConfigRoundTrips is worth keeping deliberately: nothing in the compiler checks that the JSON keys Electron writes match the Go struct tags, so a renamed key fails silently — the field keeps its zero value and a feature quietly stops working.
________________________________________
21. Known issues at end of session
Pre-existing, in packages never touched:
1.	internal/dicom — TestInjectUndefinedLengthEarlyStillFails (inject_test.go:193): "expected error for early undefined-length element, got nil".
2.	internal/transfer — TestPushSeriesViaSTOWFailedManifest (client_test.go:324): "expected error from non-empty FailedSOPSequence".
3.	internal/stability — TestWatcherIndependent: "b fired prematurely". Not a code bug: watcher.go is dated May 17 and imports only sync/time. The test allows ~10 ms of margin, and on this machine a 50 ms sleep actually takes 63 ms (Windows ~15.6 ms timer granularity). It had been passing from the build cache. Fix = widen the margins.
Environmental: internal/api, internal/queue, internal/retention, internal/transfer report TempDir RemoveAll cleanup: … used by another process — the logger holds today's log file open when t.TempDir() cleans up. Assertions pass; Windows-only artifact.
Operational:
•	Lab RNMUZ is suspended on the live backend ({"success":true,"active":false,"status":"suspended","message":"Your account is inactive. Please contact Achyu."}). The deployed engine that produced the session's logs predated the gate (its delivered POST succeeded for a suspended lab), so on deploying the new exe that lab immediately stops transmitting and shows the banner. Activate it first or expect the banner. The backend controller already exists and returns the canonical shape — nothing to build.
•	The rebrand changes the Electron userData path, and DPAPI is user/machine scoped, so operators log in once after upgrading.
•	Receiver credentials for router.achyutrs.com are carried over from the previous deployment — confirm, and prefer serving them at login.
•	The exe's --version reports the package.json version; to identify a build reliably, check go version <exe> and the feature log lines at startup.
________________________________________
22. Everything built in this session, in order
1.	Achyu rebrand — all layers: productName/appId/shortcut, window, tray, login-item, AE title, palette, wordmark, copy, taglines, installer legacy-purge list, docs, example config. Internal names kept (§1).
2.	Endpoints → pacs.achyutrs.com (login/notifier/delivered/errors/gate) and router.achyutrs.com (peer).
3.	Compression auto-off — first implemented as a hard clamp in Normalize() + Electron + UI removal; then reverted on request to a visible toggle that defaults off, with TestCompressionDefaultsOffBut… guarding the save/reload path the clamp had broken.
4.	Lab activation gate — internal/labstatus, notifier gating + parking + flusher, ConnCheck upload gate, /api/lab-status + /api/status fields, the UI banner with "Check again", ACHYU_BACKEND_API.md.
5.	DICOM TLS dual-mode — dicomtls.go, maybeWrapTLS, dicom.tls config, cached self-signed cert, TLS 1.0 + legacy ciphers, tests.
6.	AE title — confirmed non-restrictive, 12-case test, padAE dead-branch removal, editable in Settings, mirrored into config + DICOM chip.
7.	Logo removal from navbar and login; app icon generated from AXP-mark.jpeg via a rewritten generate-ico.js.
8.	JPEG 2000 Lossless (.90) — Mode, Settings, PlanFor, per-mode gdcmconv args, per-mode skip sets, size guard, Available() guard, compression.lossless config (default OFF), UI section + per-modality grid, bit-exactness test.
9.	Observability — labstatus startup line + 10-min heartbeat + checks=N, lab_gate=passed on notifier lines, feature flags on msg=ready, gdcmconv-missing warning.
10.	Encryption at rest — internal/dpapi, internal/oscrypt, whole-file config crypto with the 3-format detection ladder, legacy per-field read, startup re-save + logging, Electron safeStorage for config and session, plaintext helper removal, passcode → 18967.
11.	Window X → minimize to the taskbar (was a no-op).
12.	Two exe builds at go1.20.14/386, each smoke-tested against the live backend; final one at 8,484,864 bytes (2026-08-15 14:49).
________________________________________
That's the whole document. It's also saved at d:\website\devops\tarang-sender\ACHYU_COMPLETE_CONTEXT.md — outside the damaged tree.
Two things still worth acting on: free space on C: (it's at zero, which is likely what caused the truncation and definitely blocks build.bat), and try Previous Versions / VS Code local history on updated Achyu\ before anything else writes there. Say the word and I'll rewrite the files I authored verbatim.

