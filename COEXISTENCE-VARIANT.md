# Two Routers on One Machine — the "Achyu PACS 2" Variant

Single-file context for building a second, independently installable copy of
this router so it can run beside another router on the same Windows machine
without the two destroying each other.

Read top-to-bottom; it is ordered so another agent can replicate the work
without re-discovering anything. Section 6 records a fix that **looked right,
passed its own test, and was still wrong** — read it before improving on this.

---

## 0. TL;DR

A clinic machine ran **Achyu PACS 2** beside **BharatPACS Sender**, an older
build of this same codebase. Every build shipped its engine as
`tarang-sender.exe`, and older builds reap orphans with:

```
taskkill /F /IM tarang-sender.exe
```

That kills **every** engine on the machine. Each app killed the other's engine
on startup, each restart triggered the other, forever.

| Layer | What it stops | Needs the other product to change? |
|-------|---------------|------------------------------------|
| **Per-product engine filename** | a legacy blanket kill reaching ours | **No** — the decisive fix |
| Ownership read from the command line | *us* reaching anyone else's engine | No |
| Per-product installer kill | an install/uninstall reaching another product | No |
| Separate userData / ports / AET / appId | config, queue, lock, port collisions | No |
| `tools/check-engine-name.js` | the rename silently breaking a bundle | — |

**Key gotcha:** a binary already installed at a site cannot be patched. The
fix that works is to stop answering to the name it hunts for. Do not try to
solve this by making the two products *detect* each other — see §6.

---

## 1. The site and the symptom

- Machine `DESKTOP-RLTH3D9`, user `hp`, lab `BEFLI`
- **Achyu PACS 2** — DICOM 8899, AET `ACHYU2`, profile `%APPDATA%\achyu-pacs2`
- **BharatPACS Sender** — DICOM 8432, AET `bharatpacs`, an older build of this codebase

The UI showed one router **Connected** and the other **Sender offline**. That
screenshot was misleading: neither was working. It caught one instant of a
loop, and a minute later the colours would have been reversed.

---

## 2. Proving it from the log

From `tarang-2026-09-05.log` (the Achyu PACS 2 engine, profile
`C:\Users\hp\AppData\Roaming\achyu-pacs2`, 820 lines / 119,781 bytes):

- **52 engine starts in 23 minutes**, 11:12:57 → 11:35:40
- **Zero `WARN` or `ERROR` lines in the entire file**

> **Provenance:** those figures were taken from the original file on
> 2026-09-05. Both copies now on disk (`ACHU/` and `MasterExe/`) have since been
> overwritten with 160 KB of null bytes, so they **cannot be re-derived from
> that file.** Reproduce the pattern instead: run two builds that ship the engine
> under the same filename, and count `tarang-sender starting` lines.

A crashing process logs something. An engine that starts, reaches `msg=ready`,
and then vanishes without a word was killed from outside.

The restart gaps confirm it:

```
11:29:00.3 → 02.9   2.5s
11:29:02.9 → 07.3   4.4s
11:29:07.3 → 15.8   8.5s
11:29:15.8 → 32.3  16.4s
11:29:32.3 → 34.8   2.5s   ← reset
```

2 → 4 → 8 → 16s then reset is the exponential backoff in
`electron/main/main.js` `startSender`'s exit handler, which resets
`restartAttempts` once a process survives long enough. That is Electron
restarting an engine something else keeps killing.

Confirming the aggressor, from the committed (old) source:

```
git show HEAD:electron/main/main.js | grep taskkill
  spawnSync('taskkill', ['/F', '/IM', 'tarang-sender.exe'], ...)
git show HEAD:build/installer.nsh | grep taskkill
  nsExec::Exec 'taskkill /F /IM tarang-sender.exe /T'
```

---

## 3. What a variant is, and why not `--instance`

The codebase has **two** ways to run a second router.

**`--instance=<id>`** (or `ACHYU_INSTANCE`) — runtime only. Appends `-<id>` to
`userData`, so one installed product gets a second profile. Needs a shortcut
carrying the flag; both profiles share one Start Menu entry and one uninstaller.

**A variant** — a separately *built* product: its own installer, shortcut,
uninstall entry, ports and engine name. No flags to remember.

For two permanent routers on a clinic machine, **use a variant**. `--instance`
is for testing.

### Everything that must differ

`tools/variant.js` rewrites all of these together:

| Where | Field | Why it collides otherwise |
|-------|-------|---------------------------|
| `package.json` | `name` | Electron derives **userData** from it — config, session, BoltDB queue, logs, TLS cache **and the single-instance lock** |
| `package.json` | `build.appId`, `productName`, `nsis.shortcutName` | Start Menu, Add/Remove Programs, install dir |
| `package.json` | `build.win.extraResources[].to` | engine filename in the Windows bundle |
| `package.json` | `build.mac.extraResources[].to` | engine filename in the macOS bundle |
| `main.js` | `PRODUCT_NAME` | window title, tray, autostart registry entry |
| `main.js` | `DEFAULT_DICOM_PORT`, `DEFAULT_HTTP_PORT`, `DEFAULT_AET` | port binding, AE title |
| `main.js` | `SENDER_EXE_NAME` | the filename `main.js` spawns and reaps |
| `index.html`, `login.html` | `<title>`, navbar brand span | what the operator sees |

`name` does the most work: changing it alone separates the entire profile.

---

## 4. The variant values, and the reason for each

```js
second: {
  pkgName:     'achyu-pacs2',
  productName: 'Achyu PACS 2',
  appId:       'com.achyu.pacs2',
  dicomPort:   8899,
  httpPort:    9045,
  aet:         'ACHYU2',
  senderExe:   'achyu2-sender',
  brandTail:   ' PACS 2',
}
```

**`achyu-pacs2` — no hyphen, deliberately.** The primary's uninstaller purges
`$APPDATA\achyu-pacs-*` to clean up `--instance` profiles. `achyu-pacs-2` would
match that wildcard, and uninstalling the primary would delete variant 2's
config and study queue **while variant 2 is still installed and running.**

**DICOM stays on 8899, deliberately.** The modalities at that site were already
configured to send to 8899; moving it would mean reconfiguring every modality.
This is safe only because the co-resident router is on 8432. **If a neighbour
also listens on 8899, whichever starts second fails to bind** — the engine
probes the port at startup and logs it rather than silently moving.

**HTTP 9045** — localhost-only API. It would self-resolve via `findFreePort`,
but a predictable number is easier to support over the phone.

**AE title `ACHYU2`** — the SCP does not enforce called-AET (see
`internal/scp/aetitle_test.go`), so this is identification, not routing.

**Engine `achyu2-sender`** — distinct from the primary too, so neither Achyu
product can reach the other's engine by image name either.

The engine **binary** is not varied. It is config-driven, so both products run
the same compiled engine against different config files; only the filename it
ships under changes.

---

## 5. The three protections, in `electron/main/main.js`

`startSender` runs these in order before every spawn:

```js
killOrphanedSender();   // our own pid file
reapOwnSenders();       // anything still serving OUR config
// ...
senderProc = spawn(SENDER_BINARY, ['--config', CONFIG_PATH], ...);
writeSenderPid(senderProc.pid);
```

### 5a. Per-product engine filename — the decisive fix

```js
const SENDER_EXE_NAME = 'achyu2-sender';
```

A legacy build's `taskkill /F /IM tarang-sender.exe` **cannot match**
`achyu2-sender.exe`. Proven by firing that exact command at a running variant:

```
taskkill /F /IM tarang-sender.exe
-> ERROR: The process "tarang-sender.exe" not found.
RESULT: engine SURVIVED
```

This is what ends the loop, and it requires **nothing** from the other product.
BharatPACS did not need rebuilding.

In dev mode `SENDER_BINARY` still falls back to `tarang-sender.exe`, because
`go build` writes that name and a working tree should not have to rename it.

### 5b. Ownership from the command line — stops us being the aggressor

Every engine is spawned as `<engine> --config <profile>\achyu\config.json`, so
the profile it serves is written into the process itself. `sendersForThisProfile`
reads it via:

```powershell
Get-WmiObject Win32_Process -Filter "Name='achyu2-sender.exe'"
```

and returns only pids whose command line contains **our** `CONFIG_PATH`.
`reapOwnSenders` kills only those.

- `Get-WmiObject`, not `Get-CimInstance` — Windows 7 ships PowerShell 2.0.
- If PowerShell is unavailable it returns `[]` and kills **nothing**. An orphan
  of ours is recoverable (it surfaces as a bind failure); another product's
  engine mid-transfer is not.

### 5c. The pid file — the fast path

`<profile>\achyu\sender.pid`, written on spawn, cleared on exit.
`killOrphanedSender` reaps it only after `pidIsSender` confirms the pid still
belongs to an engine — Windows recycles pids aggressively.

---

## 6. The fix that was wrong — read this before "improving" §5

The first attempt did not rename anything. It tried to make the engine
*recognise* co-resident installs and spare them:

- scan `%APPDATA%\*\*\sender.pid` for pids other installs had claimed
- sweep any `tarang-sender.exe` no pid file claimed
- stand down entirely if any sibling profile existed

It had its own simulated test (a fake `northwind-router` profile), which
passed. **It still failed in the field.**

`sender.pid` was introduced *by that same change*. BharatPACS, being older,
writes no pid file at all. The scan saw no siblings, concluded it had the
machine to itself, and swept BharatPACS's live engine.

The test passed because it modelled a sibling that behaves like us. The real
one did not.

**Rule:** anything that relies on a co-resident product *cooperating* — writing
a file, following a convention, being recent enough — fails against exactly
the legacy build causing the problem. Protections must work unilaterally.

---

## 7. `build/installer.nsh`

```nsis
!macro killSender
  nsExec::Exec 'taskkill /F /IM "${APP_EXECUTABLE_FILENAME}" /T'

  !if "${APP_PACKAGE_NAME}" == "achyu-pacs2"
    nsExec::Exec 'taskkill /F /IM "achyu2-sender.exe" /T'
  !else
    nsExec::Exec 'taskkill /F /IM "achyu-sender.exe" /T'
  !endif

  ; legacy name: PRIMARY ONLY — an in-place upgrade from a pre-rename build
  ; still has an engine running as tarang-sender.exe
  !if "${APP_PACKAGE_NAME}" == "achyu-pacs"
    nsExec::Exec 'taskkill /F /IM "tarang-sender.exe" /T'
  !endif
  ...
```

- **`${APP_EXECUTABLE_FILENAME}`, never a literal** — each installer stops its own product.
- **Legacy `tarang-sender.exe` is killed by the primary only.** Variant 2 never
  shipped under that name; killing it there would reach straight into
  BharatPACS's engine.
- Legacy AppData purges are gated on `APP_PACKAGE_NAME == achyu-pacs`, so variant
  2's uninstaller never deletes the primary's historical folders.
- Purge paths use `${APP_PACKAGE_NAME}` / `${PRODUCT_NAME}`, never a literal.
  Variant 2 purging `achyu-pacs` would delete the primary's queue.
- **Upgrades never purge.** `${ifNot} ${isUpdated}` guards `purgeAppData`, so an
  in-place upgrade keeps login, settings and queued studies.

---

## 8. A second bug the rename caused — and the guard for it

The rename first updated only `build.win.extraResources`. The macOS bundle
still shipped `tarang-sender` while `main.js` spawned `achyu-sender`:

```
[tarang] sender binary not found at .../Contents/Resources/achyu-sender
```

The app launched, spawned nothing, and showed **Sender offline** with no error
in any log — from its own point of view nothing had failed. `variant.js` now
rewrites both mappings.

Because the engine name lives in three places that have drifted twice,
`build.sh` refuses to package unless they agree:

```
node tools/check-engine-name.js
  macOS bundle ships "achyu-sender" but main.js spawns "achyu2-sender"
  The app would start, spawn nothing, and sit at "Sender offline"
x engine filename mismatch — run: node tools/variant.js second
```

Verified by reintroducing the original bug into `variant.js` and building with
`--variant second`: exit 1.

---

## 9. Building it

The Windows engine **must** be built with **go1.20.14 / 386** — Go dropped
Windows 7 at 1.21, and a newer binary will not start there. `build.sh` refuses
otherwise. See `WINDOWS7-DOWNGRADE.md`.

```bash
# one-time: the Win7-capable toolchain
go install golang.org/dl/go1.20.14@latest && go1.20.14 download

# build variant 2
GO_BIN=$(go env GOPATH)/bin/go1.20.14 ./build.sh --variant second --target win
```

`build.sh` applies the variant, checks the engine name agrees, builds the engine
to the **repo root** (where `extraResources` reads it — building it into `dist/`
silently packages a stale binary), and runs electron-builder.

**The variant switch edits tracked files.** Put the tree back afterwards:

```bash
node tools/variant.js primary
node tools/variant.js status        # variant: primary (Achyu PACS)
```

Output: `dist/Achyu PACS 2 Setup 0.1.1.exe`.

If `C:` is short on space, NSIS fails with `error creating mmap the size of …`.
Point temp elsewhere: `TEMP=D:\build-tmp TMP=D:\build-tmp ./build.sh ...`

---

## 10. Verifying the installer

Extract and inspect rather than trusting the build order:

```bash
7z e -y -oX "dist/Achyu PACS 2 Setup 0.1.1.exe" '$PLUGINSDIR/app-32.7z'
7z l X/app-32.7z | grep -oE "[a-z0-9-]*sender\.exe"    # expect achyu2-sender.exe
go version <extracted engine>                            # expect go1.20.14
```

And the property that matters, on a machine with the variant running:

```
taskkill /F /IM tarang-sender.exe
tasklist | findstr achyu2-sender       # still running
```

---

## 11. Deploying at the site

- Install variant 2 **alongside**; do not uninstall the co-resident router.
- **Uninstall any previous variant-2 build first** if you need its defaults to
  apply. An in-place upgrade deliberately keeps the existing config.
- If the neighbour is already on 8899, change one DICOM port before installing.
- Confirm both routers show **Connected** for longer than one backoff cycle —
  a minute or two. A single screenshot of one green and one red proves nothing.

---

## 12. Known gaps

**`taskkill /F /IM gdcmconv.exe /T` is still a blanket kill** in `killSender`.
`gdcmconv` is shared by every build, so installing or uninstalling one router
kills any transcode the other has in flight. Low impact — transcoding ships
**off**, and it only runs during install — but it is the same class of bug.

**Ownership checks are Windows-only.** `sendersForThisProfile` uses a WMI query;
on macOS it returns nothing, so only the pid-file path reaps there.

**The legacy router is still aggressive.** BharatPACS will keep running
`taskkill /IM tarang-sender.exe`. It can no longer reach our engine, but any
*other* product still shipping under that name remains exposed. Rebuilding
BharatPACS from this tree is the clean end state; it is no longer required.

**The engine log cannot tell builds apart.** `version=0.1.1` is identical across
every build. Confirm a swap by file size, or by which engine name is running.
