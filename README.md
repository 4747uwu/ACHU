# tarang-sender

Native Go DICOM receive + forward service that replaces client-side
Orthanc in the Bharat PACS Electron app, plus the Electron renderer
(Worklist / Settings / Connection / Logs) that drives it.

## Status: Phase 1 milestones M1–M7 complete

| | What it does | Where it lives |
|---|---|---|
| **M1** | DICOM C-STORE / C-ECHO SCP from scratch (PS3.8 + PS3.7), BoltDB store, structured logging, graceful shutdown | `internal/scp`, `internal/store`, `internal/dicom`, `cmd/tarang-sender` |
| **M2** | Tag injection — surgical byte-level insertion of 8 private tags matching `tagwrite.lua` semantics. Idempotent, never re-encodes pixel data. | `internal/dicom/inject.go` |
| **M3** | Stability watcher (per-series timer, replaces Orthanc's `OnStableSeries`) + durable queue worker with exponential backoff and retry caps | `internal/stability`, `internal/queue` |
| **M4** | HTTP push transfer client → Orthanc REST `/instances`, basic auth, optional CA pinning, automatic study-Delivered roll-up | `internal/transfer` |
| **M4.5** | DICOMweb STOW-RS push (default): single multipart POST per series instead of per-instance — typically 20–60× fewer round trips | `internal/transfer/client.go` |
| **M5** | Local HTTP API: `/api/health`, `/api/status`, `/api/worklist`, `/api/study/:uid`, `/api/study/:uid/retry`, `/api/study/:uid` (DELETE), `/api/queue/depth` | `internal/api` |
| **M6** | Electron UI — four tabs (Worklist, Settings, Connection, Logs) + main-process integration that bootstraps config, spawns the binary, and bridges IPC | `electron/` |
| **M7** | Retention sweeper (auto-deletes Delivered studies past horizon) + force-delete cascade powering `DELETE /api/study/:uid` | `internal/retention` |

Coming next:

- **M8** — Windows installer + migration from the existing client-side Orthanc data
- **M9** — pilot rollout
- **M10** — full rollout

See `TARANG-PHASE-1-PLAN.md` for the full plan.

---

## Quick start (server side)

```bash
make build         # build for host platform
make run           # run with example config (data → ./local-data)

# In another terminal:
storescu -aec TARANG -aet TESTSCU localhost 1007 path/to/study.dcm
curl -H "Authorization: Bearer $TOKEN" http://localhost:9042/api/worklist | jq
```

The receiver writes files to `local-data/instances/<study>/<series>/<sop>.dcm`
and indexes them in `local-data/tarang.db`. After `stable_age_seconds` of
quiet, each series is automatically queued and pushed to the configured peer.
After `retention_hours` of being marked Delivered, the local copy is swept.

## Cross-compile for the production target (Windows)

```bash
make build-windows
# → bin/tarang-sender.exe (~8 MB, fully static, no DLL deps)
```

## Architecture

```
modality
  │ C-STORE
  ▼
[SCP listener] ── M1 ──► parse + write file ──► BoltDB
  │                                                │
  │ M2: inject private tags                        │
  │                                                │
  └─► OnInstanceStored ──► [stability watcher] ───┘
                                  │
                                  │ stable_age elapsed
                                  ▼
                          [queue: enqueue series]
                                  │
                                  ▼
                            [queue worker]
                                  │
                                  │ Process(entry)
                                  ▼
                          [STOW-RS client] ──► receiver Orthanc
                                  │
                                  └─► mark Delivered
                                          │
                                          ▼
                              [retention sweeper, hourly]
                                          │
                                          └──► age > 24h ──► delete on disk + DB
```

In parallel:

```
[HTTP API] ─── /api/worklist, /status, /retry, /delete ──► Electron renderer
```

### Subsystem responsibilities

- **`internal/scp`** — DICOM Upper Layer Protocol (PS3.8) and DIMSE
  (PS3.7) hand-rolled. ~1,000 lines, zero external network deps.
  Handles A-ASSOCIATE, multi-context negotiation, P-DATA-TF
  fragmentation, C-STORE, C-ECHO, A-RELEASE, A-ABORT.

- **`internal/store`** — BoltDB layer. Five primary buckets (instances,
  series, studies, queue, transfers) plus three secondary index buckets.
  Generic `putJSON[T]/getJSON[T]`. Idempotent `PutInstance` that
  atomically updates instance + series + study aggregates.
  `DeleteStudyCascade` for full teardown.

- **`internal/dicom`** — `suyashkumar/dicom` for parsing only. Surgical
  tag injection (no full re-encode) preserves modality bytes verbatim
  except the targeted private elements. Works with all uncompressed and
  encapsulated/compressed transfer syntaxes.

- **`internal/stability`** — per-series `time.AfterFunc` timers under a
  mutex-guarded map. `Touch(seriesUID)` resets/starts the timer; when
  it fires, the OnStable callback enqueues the series.

- **`internal/queue`** — pulls `store.QueueEntry` items, runs the
  user-supplied `Processor` function. On error: re-enqueue with
  configurable backoff, increment retries, mark Failed after MaxRetries.

- **`internal/transfer`** — HTTP client. STOW-RS (default) streams a
  multipart `POST /dicom-web/studies` per series with `io.Pipe` so
  whole-body PET-CT payloads never materialize in memory. Falls back to
  per-instance `POST /instances` when `peer.protocol = "instances"`.

- **`internal/retention`** — periodic sweep of Delivered studies past
  the horizon + the `DeleteStudy` helper used by both the sweeper and
  the force-delete API endpoint.

- **`internal/api`** — `net/http` mux, bearer-token auth on everything
  except `/api/health`. Endpoints map directly onto store reads + the
  queue helper + retention.DeleteStudy.

- **`electron/`** — main process spawns the Go binary, manages config
  (writes `config.json` with a fresh API token at login), restarts on
  unexpected exit; preload exposes `window.tarangAPI` to the renderer;
  renderer is a four-tab single-page app that polls the local HTTP API.

## Configuration

A single JSON file. See `examples/config.example.json`. The Electron
main process owns this file: it writes it at login (using the `lab_id`
and `org_id` returned by the auth API) and the sender reads it at
startup.

Required: `lab_id`, `org_id`, `peer.name`, `peer.url`, `http.api_token`.
Optional `peer.protocol` selects `stow-rs` (default) or `instances`.

## API surface

All endpoints other than `/api/health` require `Authorization: Bearer <token>`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Liveness; no auth. |
| GET | `/api/status` | Uptime, lab/org ID, peer info, queue depth. |
| GET | `/api/worklist?status=&limit=` | Study list, optional status filter. |
| GET | `/api/study/:uid` | Study record + nested series list. |
| POST | `/api/study/:uid/retry` | Re-enqueues every non-delivered series. |
| DELETE | `/api/study/:uid` | Force-delete: removes files + DB rows for the study. |
| GET | `/api/queue/depth` | In-flight indicator. |

## Electron UI (M6)

Four tabs, all calling the local HTTP API:

- **Worklist** — filterable table of every study the sender has seen.
  Click a row to open a side drawer with study + series detail and
  Retry / Force-delete actions. Auto-refreshes every 3 s.
- **Settings** — read-only identity + ports, editable Stable Age and
  Retention. Edits go through `window.tarangAPI.updateConfig` IPC →
  main process patches `config.json` atomically. The sender must be
  restarted to pick up changes.
- **Connection** — KPI tiles (peer, queue depth, uptime, throughput) +
  receiver-ping action.
- **Logs** — tails `<data_dir>/logs/tarang-YYYY-MM-DD.log` via IPC,
  with level filtering (Errors / Warnings & above / All) and pause
  toggle. Updates every 2 s while visible.

The renderer follows a minimalist clinical aesthetic: IBM Plex Sans
prose, IBM Plex Mono for IDs/UIDs, status pills with low-saturation
fills, no shadows, dark-mode-ready CSS variables.

### Running the renderer outside Electron (dev mode)

`renderer/api.js` honors `?token=…&port=…` query params when
`window.tarangAPI` is missing, so you can iterate on the UI by serving
the renderer with any static-file server pointed at a running sender.

```bash
# Start the sender locally (uses config from examples/...)
./bin/tarang-sender --config /tmp/test-config.json &

# Serve the renderer
cd electron/renderer && python3 -m http.server 8000

# Open in browser:
# http://localhost:8000/index.html?token=YOUR_TOKEN&port=9042
```

## Testing manually

```bash
echoscu -aec TARANG localhost 1007                                           # ping
storescu -aec TARANG -aet TESTSCU localhost 1007 file.dcm                    # send
curl -H "Authorization: Bearer $TOKEN" http://localhost:9042/api/worklist    # see it
```

## Test coverage

```
internal/api         TestAPIEndpoints                                       ✓
internal/dicom       TestRoundTrip, TestInjectExplicit, TestInjectImplicit,
                     TestInjectReplaces, TestInjectUnsupportedSyntax        ✓
internal/queue       TestSuccessfulProcess, TestRetriesThenFails            ✓
internal/retention   TestSweepDeletesOnlyAgedDelivered,
                     TestForceDeleteRemovesEverything,
                     TestDeleteStudyIdempotent, TestSweeperStartStop        ✓
internal/scp         TestAssociateAndCEcho                                  ✓
internal/stability   TestWatcherFires, TestWatcherResets,
                     TestWatcherIndependent, TestWatcherStop                ✓
internal/store       TestPutInstanceAndList, TestQueueFIFO                  ✓
internal/transfer    TestPushSeriesAgainstStubOrthanc,
                     TestPushSeriesPropagatesPeerError,
                     TestPushSeriesViaSTOW,
                     TestPushSeriesViaSTOWFailedManifest                    ✓
```

21 Go tests, all passing under `go test -race`. JS files are syntax-checked
with `node --check`. End-to-end smoke test against a live sender plus
dcmtk's `storescu` confirms the full SCP → tag injection → stability →
queue → STOW-RS → API → retry → force-delete pipeline works.

## Sandbox build note

The committed `go.mod` contains three `replace` directives that route
`go.etcd.io/bbolt`, `golang.org/x/sys`, and `golang.org/x/text` through
their GitHub mirrors. **These exist only because the build sandbox used
during initial development can't reach `proxy.golang.org` or
`golang.org/x/...` directly.** In a normal dev environment with internet
access, **delete those three lines** from `go.mod` — `go mod tidy` will
fetch the canonical paths cleanly.

## License

Proprietary. © Bharat PACS.
