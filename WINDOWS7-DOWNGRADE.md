# Windows 7 Compatibility — Downgrade & Replication Guide

Single-file context for reproducing the Windows 7 support work on this project
(`uevolveai-sender` — Electron shell + Go `tarang-sender.exe` DICOM bridge).

Read this top-to-bottom; it is ordered so another agent can replicate every step
without re-discovering anything.

---

## 0. TL;DR

Windows 7 support was blocked by **two independent things**, both fixed by
downgrading to their last Win7-capable versions:

| Component | Was | Now (Win7-capable) | Why the old one broke Win7 |
|-----------|-----|--------------------|----------------------------|
| Electron  | `^31.0.0` (31.7.7) | **`22.3.27`** | Electron ≥23 needs Win10 (Chromium 109) |
| Go toolchain | 1.22 / installed 1.26.2 | **`go1.20.14`** | Go ≥1.21 dropped Win7 (needs Win10/Server 2016) |
| Target arch | host amd64 | **`386` (32-bit)** | ia32 build runs on 32- *and* 64-bit Win7 |

Two Go 1.21+ language features had to be removed because they don't exist in 1.20:
`log/slog` (stdlib) and the builtin `max()`.

**Key gotcha:** editing `go.mod` to `go 1.20` does NOT make a Win7 binary. The
`go` directive is only a *language floor*. What decides Win7 compatibility is the
**toolchain that actually compiles** — forced via `GOTOOLCHAIN=go1.20.14` in the
build scripts. A plain `go build` with Go 1.26 installed silently produces a
Win7-incompatible binary.

There is **no** Go version with both Win7 support *and* stdlib `slog` — they are
mutually exclusive (`slog` = 1.21+, Win7 = ≤1.20). So `slog` must be backported.

---

## 1. Environment this was done on

- Host OS: Windows 11 (build/dev machine — NOT Win7)
- Installed Go: 1.26.2 (used only to *drive* the download of the 1.20.14 toolchain)
- Node: 24.x, npm bundled
- Shell: PowerShell primary; Git Bash available
- Project root: `d:\website\devops\tarang-sender\dicomelearning`
- Build entry points: `build.bat` (primary, used by team) and `build.ps1`

---

## 2. Exact code changes (the downgrade)

### 2.1 `go.mod`
```diff
- go 1.22
+ go 1.20
```
Plus an added dependency (see 2.5). Do NOT add a `toolchain` directive — Go 1.20
predates it and would misbehave; the toolchain is forced via env var instead.

### 2.2 `package.json`
```diff
   "devDependencies": {
-    "electron": "^31.0.0",
+    "electron": "22.3.27",
```
`electron-builder ^24.0.0` stays — it packages Electron 22 fine. Build target is
already `nsis` / `ia32`, which is correct for Win7.

### 2.3 `internal/log/log.go` — replace stdlib slog with the x/exp backport
`log/slog` is Go 1.21+. `golang.org/x/exp/slog` is the pre-1.21 backport with a
**byte-for-byte identical API**. Only this one file changes; all ~105 call sites
across 12 files call `log.L()...` and are untouched (no other file imports slog).

Import change (alias keeps the rest of the file identical):
```diff
 import (
 	"context"
 	"io"
-	"log/slog"
 	"os"
 	"path/filepath"
 	"sync"
 	"sync/atomic"
 	"time"
+
+	slog "golang.org/x/exp/slog"
 )
```
Also update the package doc comment (was "Uses Go 1.21+ slog").

### 2.4 `cmd/tarang-sender/main.go` — remove builtin `max()`
Builtin `min`/`max` are Go 1.21+. There was exactly one use (~line 223):
```diff
-	transcodeSlots := make(chan struct{}, max(1, runtime.NumCPU()/2))
+	transcodeWorkers := runtime.NumCPU() / 2
+	if transcodeWorkers < 1 {
+		transcodeWorkers = 1
+	}
+	transcodeSlots := make(chan struct{}, transcodeWorkers)
```
To find any others: `rg '\b(max|min|clear)\s*\(' --type go`.

### 2.5 Pin `golang.org/x/exp` to a Go-1.20-compatible version
`@latest` x/exp requires Go 1.25 (its `slices` imports stdlib `cmp`/`slices`).
Use a **mid-2023** version, before x/exp/slices started importing stdlib `cmp`:

```
golang.org/x/exp v0.0.0-20230626212559-97b1e661b5df
```
Commands (run with the 1.20 toolchain forced, see §3):
```
go get golang.org/x/exp@v0.0.0-20230626212559-97b1e661b5df
go mod tidy
```
Symptom if you pick a too-new x/exp: `package cmp is not in GOROOT ... note:
imported by a module that requires go 1.25`.

---

## 3. Build mechanics — the part that actually matters

Both `build.bat` and `build.ps1` build the Go binary FIRST, then `npm install`,
then `electron-builder --win --ia32`. The Go step was changed to force the
toolchain and 32-bit arch.

### `build.bat` (before the `go build` line)
```bat
set GOTOOLCHAIN=go1.20.14
set GOARCH=386
set CGO_ENABLED=0
go build -ldflags "-X main.version=0.1.0 -s -w" -o tarang-sender.exe ./cmd/tarang-sender/
```

### `build.ps1` (before the `go build` call)
```powershell
$env:GOTOOLCHAIN  = "go1.20.14"
$env:GOARCH       = "386"
$env:CGO_ENABLED  = "0"
```

Notes:
- `GOTOOLCHAIN=go1.20.14`: the installed Go (1.26) auto-downloads and delegates to
  go1.20.14 on first build (needs internet once). This is the ONLY thing that makes
  the binary Win7-compatible.
- `GOARCH=386`: 32-bit, matches the `ia32` Electron installer, runs on all Win7.
- `CGO_ENABLED=0`: deps are pure Go (no cgo), so cross-compiling 386 is clean and
  needs no C compiler.

Manual one-off build (equivalent, PowerShell):
```powershell
$env:GOTOOLCHAIN="go1.20.14"; $env:GOARCH="386"; $env:CGO_ENABLED="0"
go build -ldflags "-X main.version=0.1.1 -s -w" -o tarang-sender.exe ./cmd/tarang-sender/
```

Swap node_modules to Electron 22 (after editing package.json):
```
npm install        # electron 22.3.27 binary replaces 31.x in node_modules
```
`npm audit` will show vulns — expected; Electron 22 is EOL, unavoidable for Win7.

---

## 4. Verification (all doable on Win11, no VM)

### 4.1 Confirm the binary's toolchain + arch
```powershell
go version .\tarang-sender.exe          # must say: go1.20.14   (NOT go1.2x)
# PE machine: 0x014C = 386 (good), 0x8664 = amd64 (wrong for universal Win7)
```

### 4.2 Static "will it launch on Win7?" check (import table)
A Go binary is statically linked; Win7 only needs the functions it *hard-imports*.
If it imports a Win8+ function, Win7 refuses to launch it ("entry point X could not
be located in KERNEL32.dll"). Verify with Go's `debug/pe`:

- A correct go1.20.14/386 build hard-imports **only `kernel32.dll`** (~39 funcs),
  all present on Win7, and stamps **MinOS subsystem 6.1** (= Windows 7).
- Red flags: `GetSystemTimePreciseAsFileTime`, `WaitOnAddress`,
  `WakeByAddressSingle`, `SetThreadDescription` → means a ≥1.21 build slipped in.

(A ready-made analyzer using `debug/pe` + `File.ImportedSymbols()` was written to
list imports and flag Win8+ APIs; recreate if needed — it's ~120 lines.)

### 4.3 Run it and hit the local API
```powershell
# minimal config needs: lab_id, org_id, dicom.port != http.port, retention_hours>=1,
# peer.url, peer.name, transfer.concurrent_workers>=1
.\tarang-sender.exe --config config.json
# then:
Invoke-WebRequest http://127.0.0.1:9044/api/health   # -> 200 {"status":"ok",...}
```
The sender writes its own log to `<data_dir>/logs/tarang-YYYY-MM-DD.log`. On Win7,
if that log is missing/empty, the exe never started (wrong build or crash).

### 4.4 Package + confirm what's inside the installer
```
npx electron-builder --win --ia32
# expect: electron=22.3.27 arch=ia32 -> dist\...Setup <ver>.exe
# verify the packed binary:
go version dist\win-ia32-unpacked\resources\tarang-sender.exe   # go1.20.14
```

---

## 5. Test status after downgrade

`go test ./...` under go1.20.14/386: no regressions introduced by the downgrade.
Pre-existing failures (confirmed to also fail on the ORIGINAL go1.26.2 build, so
NOT caused by this work):
- `internal/dicom` `TestInjectUndefinedLengthEarlyStillFails` — pre-existing.
- Many `TempDir RemoveAll ... used by another process` — Windows-only test-cleanup
  noise; the logger holds the daily log file open for the process lifetime.
- `internal/transfer` STOW manifest assertion — flaky, passes on isolated re-run.

---

## 6. Runtime issues discovered on Win7 (NOT build/downgrade problems)

The downgrade makes the binary *launch and serve* on Win7 (verified statically).
Two separate runtime issues cause "failed to fetch" and are tracked here:

### 6.1 ★ Login "failed to fetch" = missing root certificate (PRIMARY live issue)
The login page (`electron/renderer/login.js`) does:
```js
fetch('https://uevolveai.xcentic.com/api/auth/lab-login', { method:'POST', ... })
```
This is an outbound HTTPS call from Chromium (Electron 22 = Chromium 108) to the
remote auth server — nothing to do with the Go binary. `fetch` throws
"Failed to fetch" only when the connection can't be established (TLS/DNS/network);
an HTTP error status would be caught as `!res.ok`.

The server's cert chains to **`ISRG Root X2`** (Let's Encrypt's 2020 ECDSA root):
```
uevolveai.xcentic.com  ->  Let's Encrypt YE1  ->  Root YE (ISRG)  ->  ISRG Root X2
```
`ISRG Root X2` is **not in the default Windows 7 root store** (it's pushed via
Microsoft's auto root-update program). Chromium 108 on Windows verifies against the
**OS root store** (Chrome's own root store didn't ship on Windows until Chrome 114).
So: Win10/11 trust it → login works; un-updated Win7 lacks it → TLS fails →
"Failed to fetch". The chain has NO cross-sign to an older root, so the machine
must have ISRG Root X2.

Confirm on the Win7 box: open `https://uevolveai.xcentic.com/api/auth/lab-login`
in IE — a cert warning confirms it. (DevTools would show
`net::ERR_CERT_AUTHORITY_INVALID`.)

Fixes (ranked):
1. **Server-side (best):** reissue the auth API cert under a CA already trusted by
   Win7 (DigiCert / Sectigo, or front with Cloudflare). Fixes all clients at once.
2. **Installer:** bundle `ISRG Root X1`+`X2` and add to Trusted Root during NSIS
   setup: `certutil -addstore -f root isrgrootx2.crt`.
3. **In-app:** Electron `session.setCertificateVerifyProc` to trust the pinned ISRG
   root for the API host (no OS/server change).
4. **Manual:** Windows Update, or import the root by hand per machine.

Inspect any server's chain from the dev box (PowerShell): open a `TcpClient` +
`SslStream` to host:443, `AuthenticateAsClient`, build an `X509Chain`, print each
element's Subject/Issuer.

### 6.2 Renderer hardcodes `localhost:9042` (wrong port + IPv6 on Win7)
`api.js` correctly uses `http://127.0.0.1:${boot.httpPort}` (9044). But
`settings-form.js` and `progress-modal.js` hardcode `http://localhost:9042`:
- wrong port (server listens on 9044), and
- `localhost` can resolve to IPv6 `::1` on Win7, where the Go server (IPv4
  `127.0.0.1` only) isn't listening → "failed to fetch".

Fix (partially applied): `api.js` now exposes `window.tarang.baseUrl = BASE`.
**Still TODO:** update `settings-form.js` (3 fetches: config GET/PATCH, and a
`/api/config/reset` call that also has no server route) and `progress-modal.js`
(2 fetches: transfer progress + cancel) to use `window.tarang.baseUrl` /
`window.tarang.transferProgress()` instead of the hardcoded `localhost:9042`.

---

## 7. Current state / what's left

Done: §2 all edits, §3 build-script changes, `npm install` (Electron 22 in
node_modules), §4 verified (binary is go1.20.14/386, launches, serves, packages),
`api.js` `baseUrl` added.

Not done (needs a decision or follow-up):
- §6.1 cert fix — awaiting choice of server vs installer vs in-app.
- §6.2 finish the `settings-form.js` / `progress-modal.js` URL fix.
- Real-world test-install `dist\...Setup.exe` on an actual Win7 box (the one thing
  that can't be done from Win11).

---

## 8. Quick command reference

```powershell
# Full manual build, Win7 target
$env:GOTOOLCHAIN="go1.20.14"; $env:GOARCH="386"; $env:CGO_ENABLED="0"
go build -ldflags "-X main.version=0.1.1 -s -w" -o tarang-sender.exe ./cmd/tarang-sender/
npm install
npx electron-builder --win --ia32

# Verify
go version .\tarang-sender.exe                 # go1.20.14
go version .\dist\win-ia32-unpacked\resources\tarang-sender.exe

# Or just run the team script (has GOTOOLCHAIN/GOARCH baked in)
.\build.bat
```
