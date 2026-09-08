// electron/main/main.js — Electron main process integration for tarang-sender.
//
// What it does:
//
//  1. Generates a random API token for the renderer↔sender HTTP channel
//  2. Writes / patches the sender's config.json (the renderer can't touch disk)
//  3. Spawns the tarang-sender child process and pipes its stdout/stderr
//     into a daily-rotated log file (the same one the sender writes to)
//  4. Restarts the sender if it exits unexpectedly
//  5. Exposes IPC handlers used by the renderer:
//       - tarang:get-bootstrap   → token, ports, AET, paths
//       - tarang:update-config   → persist Stable Age / Retention edits
//       - tarang:read-recent-logs → tail today's log file
//
// This file is illustrative — the existing Bharat PACS Electron app
// already has its own auth flow, window management, and packaging.
// Drop these handlers into the existing main.js, adjust paths, and the
// renderer code in ../renderer works unchanged.

const { app, BrowserWindow, ipcMain, Menu, Tray, safeStorage } = require('electron');
const { spawn } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const os = require('os');
const net = require('net');

// ------------------------------------------------------------------------
// Configuration paths
// ------------------------------------------------------------------------

// ------------------------------------------------------------------------
// Instance identity
//
// Two routers can run on one machine — a site with two separate modality
// networks, or an old and a new receiver running side by side during a
// migration. Everything that is per-INSTALL rather than per-machine keys off
// the id below; everything genuinely per-machine (the executable, the bundled
// gdcmconv) is still shared.
//
// Chosen with `--instance=<id>` on the command line, or ACHYU_INSTANCE in the
// environment. Absent — the overwhelmingly common case — it is empty and every
// path, name and lock is byte-for-byte what it was before, so existing
// installs are untouched by this feature.
//
// Give the second instance its own shortcut:
//   "C:\...\Achyu PACS.exe" --instance=b
// ------------------------------------------------------------------------

const INSTANCE_ID = (() => {
  const flag = '--instance=';
  const fromArgv = process.argv.find(a => a.startsWith(flag));
  const raw = (fromArgv ? fromArgv.slice(flag.length) : process.env.ACHYU_INSTANCE) || '';
  // Folded to a token that is safe in a path, a registry value and a window
  // title. An id that sanitises away to nothing is treated as absent rather
  // than silently becoming a nameless second profile.
  return raw.toLowerCase().replace(/[^a-z0-9-]/g, '').slice(0, 16);
})();

const INSTANCE_SUFFIX = INSTANCE_ID ? `-${INSTANCE_ID}` : '';
const INSTANCE_LABEL  = INSTANCE_ID ? ` (${INSTANCE_ID})` : '';

// This MUST run before anything reads userData — APP_DATA_DIR immediately
// below, and requestSingleInstanceLock(), which Chromium scopes to the
// user-data directory. That scoping is what makes the whole feature work:
// two different userData dirs both acquire a lock, while a second copy of the
// SAME instance is still refused, which is the behaviour we want to keep.
// Verified both directions on Electron 22 before relying on it.
if (INSTANCE_ID) {
  app.setPath('userData', app.getPath('userData') + INSTANCE_SUFFIX);
}

// ------------------------------------------------------------------------
// Variant identity
//
// A SECOND router installed alongside the first is produced by changing the
// four values below plus the matching fields in package.json — that is what
// `node tools/variant.js second` does. Everything downstream reads these, so
// a variant never needs edits scattered through the file.
//
// This is separate from INSTANCE_ID above and they compose: the variant is
// baked in at build time (a distinct installed product, its own Start Menu
// entry, no flags to pass), while --instance splits one installed product
// into extra profiles at runtime.
//
// userData does NOT appear here: Electron derives it from package.json "name",
// so changing that field alone already separates config, session, the BoltDB
// queue, the logs, the TLS cache and the single-instance lock.
// ------------------------------------------------------------------------

const PRODUCT_NAME        = 'Achyu PACS';
const DEFAULT_DICOM_PORT  = 8899;
const DEFAULT_HTTP_PORT   = 9044;
const DEFAULT_AET         = 'ACHYU';

const APP_DATA_DIR = path.join(app.getPath('userData'), 'achyu');
const CONFIG_PATH  = path.join(APP_DATA_DIR, 'config.json');
const SESSION_PATH = path.join(APP_DATA_DIR, 'session.json');
const DATA_DIR     = path.join(APP_DATA_DIR, 'data');
// Records the pid of the engine THIS instance spawned, so a restart can reap
// its own orphan without touching another instance's engine.
const SENDER_PID_PATH = path.join(APP_DATA_DIR, 'sender.pid');
const LOG_DIR      = path.join(DATA_DIR, 'logs');

// SENDER_EXE_NAME is this product's engine filename, WITHOUT extension.
//
// Deliberately not "tarang-sender", and deliberately different per variant.
// Every build of this codebase used to ship the engine under that one shared
// name, and builds older than this reap orphans with a blanket
//
//     taskkill /F /IM tarang-sender.exe
//
// which kills every engine on the machine on every start -- including one
// mid-transfer belonging to another product. A site running two of our
// routers had them killing each other in a loop: 52 engine restarts in 23
// minutes, no errors logged, because nothing had crashed. They were being
// executed.
//
// A binary already installed at a site cannot be patched. What we control is
// the name it is hunting for, so we stop using it. A legacy build's blanket
// kill cannot match achyu-sender.exe, and the loop ends without that build
// having to change at all.
//
// tools/variant.js rewrites this line, and the matching extraResources "to"
// in package.json, so the two can never drift apart.
const SENDER_EXE_NAME = 'achyu-sender';

// Path to the engine binary.
// Packaged build: binary is copied into the Electron resources folder.
// Dev mode (npm start / electron .): binary lives at the project root or bin/.
//
// Dev keeps tarang-sender as a fallback: `go build` still writes that name, so
// a working tree is not forced to rename its output just to run the app.
const SENDER_BINARY = (() => {
  const win = process.platform === 'win32';
  const exe = win ? SENDER_EXE_NAME + '.exe' : SENDER_EXE_NAME;
  if (app.isPackaged) {
    return path.join(process.resourcesPath, exe);
  }
  const devRoot = path.join(__dirname, '..', '..');
  const legacy = win ? 'tarang-sender.exe' : 'tarang-sender';
  const candidates = [
    path.join(devRoot, exe),
    path.join(devRoot, 'bin', exe),
    path.join(devRoot, legacy),
    path.join(devRoot, 'bin', legacy),
  ];
  return candidates.find(p => fs.existsSync(p)) || candidates[0];
})();

// Path to the gdcmconv binary (for JPEG 2000 compression).
// Packaged: bundled into resources alongside tarang-sender.
// Dev: look next to tarang-sender.exe, then in PATH.
const GDCMCONV_BINARY = (() => {
  const win = process.platform === 'win32';
  const name = win ? 'gdcmconv.exe' : 'gdcmconv';
  if (app.isPackaged) {
    return path.join(process.resourcesPath, name);
  }
  const devRoot = path.join(__dirname, '..', '..');
  const candidate = path.join(devRoot, name);
  return fs.existsSync(candidate) ? candidate : name; // fall back to PATH
})();

// Per-modality lossless policy: CT, MR, CR and DX all on. Mirrors
// config.DefaultLosslessModalityPolicy() on the Go side — keep the two in
// step, and see that function for why CR and DX were off and are now back on.
//
// The map records EXCEPTIONS: a modality that appears nowhere here is still
// enabled, which is why PT/NM/US/MG are absent rather than listed as true.
// Inert while lossless.enabled is false (the shipped default) — it decides
// only WHICH modalities transcode once a site switches the mode on.
const LOSSLESS_MODALITY_POLICY = { CT: true, MR: true, CR: true, DX: true };

// ------------------------------------------------------------------------
// Bootstrap state — lives for the lifetime of the Electron process
// ------------------------------------------------------------------------

let bootstrap = null;   // { token, httpPort, dicomPort, aet, dataDir, ... }
let senderProc = null;  // ChildProcess
let restartTimer = null;
let restartAttempts = 0;
let suppressRestart = false;
let mainWindow = null;
let tray = null;         // held at module scope so it isn't garbage collected

// ------------------------------------------------------------------------
// Encrypted storage
//
// config.json and session.json both hold secrets — the API token, the
// receiver credentials, the login token — so both are encrypted at rest with
// Electron's safeStorage, which on Windows is Chromium's OSCrypt.
//
// The two files use deliberately different encodings:
//
//   config.json   base64 TEXT of the blob. The Go engine reads this file too,
//                 and its format detection keys off the first byte: '{' means
//                 plaintext JSON, anything else is base64 ciphertext.
//   session.json  the raw Buffer. Only Electron ever reads it, so there is no
//                 reason to pay for base64.
//
// Note there is NO generic plaintext readJSON/writeJSONAtomic pair any more.
// Both files this process persists hold secrets, and leaving a
// general-purpose cleartext writer sitting beside them is exactly how
// cleartext credentials creep back in a later edit.
// ------------------------------------------------------------------------

function encryptionAvailable() {
  try {
    return safeStorage.isEncryptionAvailable();
  } catch (_) {
    return false;
  }
}

// readConfig returns the parsed config, or null. It tolerates a plaintext file
// so an install that predates encryption still opens; the Go engine re-saves
// it encrypted on its next start.
function readConfig() {
  if (!fs.existsSync(CONFIG_PATH)) return null;
  try {
    const text = fs.readFileSync(CONFIG_PATH, 'utf8').replace(/^﻿/, '').trim();
    if (text === '') return null;
    if (text.startsWith('{')) return JSON.parse(text);
    return JSON.parse(safeStorage.decryptString(Buffer.from(text, 'base64')));
  } catch (err) {
    console.error('[tarang] could not read config:', err.message);
    return null;
  }
}

function writeConfig(cfg) {
  const json = JSON.stringify(cfg, null, 2);
  let payload;
  if (encryptionAvailable()) {
    payload = safeStorage.encryptString(json).toString('base64');
  } else {
    // No OS keyring (a Linux dev box, typically). Persisting in cleartext is
    // better than refusing to start, but say so.
    console.warn('[tarang] OS encryption unavailable — config will be stored in PLAINTEXT');
    payload = json;
  }
  const tmp = CONFIG_PATH + '.tmp';
  fs.writeFileSync(tmp, payload, { mode: 0o600 });
  fs.renameSync(tmp, CONFIG_PATH);
}

function readSessionFile() {
  if (!fs.existsSync(SESSION_PATH)) return null;
  try {
    const raw = fs.readFileSync(SESSION_PATH);
    const asText = raw.toString('utf8').replace(/^﻿/, '').trim();
    if (asText.startsWith('{')) return JSON.parse(asText); // pre-encryption install
    return JSON.parse(safeStorage.decryptString(raw));
  } catch (err) {
    console.error('[tarang] could not read session:', err.message);
    return null;
  }
}

function writeSession(session) {
  const json = JSON.stringify(session, null, 2);
  const payload = encryptionAvailable() ? safeStorage.encryptString(json) : Buffer.from(json, 'utf8');
  const tmp = SESSION_PATH + '.tmp';
  fs.writeFileSync(tmp, payload, { mode: 0o600 });
  fs.renameSync(tmp, SESSION_PATH);
}

function getDefaultPeer() {
  return {
    name: 'ACHYU',
    url: 'https://router.achyutrs.com',
    username: 'Xcentic',
    password: 'XcenticIngestion123',
    compression: 'gzip',
    caCertPath: '',
    protocol: 'stow-rs',
  };
}

function normalizePeer(inputPeer, existingPeer) {
  const defaults = getDefaultPeer();
  // Strip empty strings so stored blanks don't mask hardcoded defaults.
  const strip = (obj) => {
    if (!obj) return {};
    const out = {};
    for (const [k, v] of Object.entries(obj)) {
      if (v !== '') out[k] = v;
    }
    return out;
  };
  const peer = { ...defaults, ...strip(existingPeer), ...strip(inputPeer) };
  return {
    name: peer.name,
    url: peer.url,
    username: peer.username,
    password: peer.password,
    compression: peer.compression || 'gzip',
    caCertPath: peer.caCertPath || '',
    protocol: peer.protocol || 'stow-rs',
  };
}

function readSession() {
  return readSessionFile();
}

function clearSession() {
  if (fs.existsSync(SESSION_PATH)) {
    fs.unlinkSync(SESSION_PATH);
  }
}

function loadRendererPage(name) {
  if (!mainWindow || mainWindow.isDestroyed()) return;
  mainWindow.loadFile(path.join(__dirname, '..', 'renderer', name));
}

function buildSessionFromCredentials(credentials, existingPeer) {
  const c = credentials || {};
  const peerInput = c.peer || c.labSettings?.peer || null;
  const session = {
    token: c.token,
    labId: c.labId || c.lab_id || c.user?.lab?.identifier || 'DEFAULT',
    orgId: c.orgId || c.org_id || c.user?.organizationIdentifier || 'DEFAULT',
    userName: c.userName || c.user?.name || 'Operator',
    apiUrl: c.apiUrl || '',
    loginTime: c.loginTime || new Date().toISOString(),
    peer: normalizePeer(peerInput, existingPeer),
  };
  return session;
}

// ------------------------------------------------------------------------
// Config bootstrap — call this AFTER the user has logged in and the auth
// API has handed back labId/orgId/peer credentials.
// ------------------------------------------------------------------------

// Derive the backend API base from the login URL so all server endpoints
// (study notifier, error-log reporting) follow whatever server the operator
// logged into — "use the login url up to /api". E.g.
//   https://pacs.achyutrs.com/api/auth/lab-login  ->  https://pacs.achyutrs.com/api
function deriveApiBase(apiUrl) {
  const FALLBACK = 'https://pacs.achyutrs.com/api';
  if (!apiUrl) return FALLBACK;
  const i = apiUrl.indexOf('/api');
  if (i !== -1) return apiUrl.slice(0, i + 4); // include the "/api" segment
  try { return new URL(apiUrl).origin + '/api'; } catch (_) { return FALLBACK; }
}

async function bootstrapSender({ labId, orgId, peer, apiUrl }) {
  fs.mkdirSync(APP_DATA_DIR, { recursive: true });
  fs.mkdirSync(DATA_DIR,    { recursive: true });
  fs.mkdirSync(LOG_DIR,     { recursive: true });

  // Stop our own engine before anything below probes a port. It is holding
  // this instance's HTTP port from the previous session, and a probe that saw
  // its own child would conclude the port was taken and move it — every
  // restart, walking the port upward one number at a time. This function
  // always ends by starting the engine again, so stopping here rather than
  // further down costs nothing.
  suppressRestart = false;
  if (senderProc) {
    stopSender({ permanent: true });
    let waited = 0;
    while (senderProc && waited < 5000) {
      await new Promise(r => setTimeout(r, 100));
      waited += 100;
    }
  }

  // Existing config wins for fields the user customized; new fields get
  // defaulted on first run.
  //
  // This MUST go through readConfig(). writeConfig() stores the file as
  // base64 AES-GCM (safeStorage), so a bare JSON.parse of the raw bytes
  // throws on every install that has ever been written — and this function
  // then rebuilds the whole config from the defaults below and saves it.
  // The symptom is that every setting an operator edits (DICOM port, AE
  // title, stable age, retention) silently reverts to default on the next
  // restart, while the save itself appears to work, because the IPC handler
  // that writes it does decrypt correctly. Nothing else notices: the token
  // and ports are handed to the renderer fresh each start, and the peer
  // credentials are restored from session.json.
  const cfg = readConfig() ?? {};

  cfg.lab_id   = labId;
  cfg.org_id   = orgId;
  cfg.log_level = cfg.log_level || 'info';

  cfg.dicom = {
    port: cfg.dicom?.port ?? DEFAULT_DICOM_PORT,
    aet:  cfg.dicom?.aet  ?? DEFAULT_AET,
    stable_age_seconds: cfg.dicom?.stable_age_seconds ?? 15,
    // Secure DICOM shares the plaintext port — each connection is identified
    // by its first byte — so enabling it costs nothing for modalities that
    // send plaintext, and a site that switches one to Secure DICOM needs no
    // change here.
    tls: {
      enabled: cfg.dicom?.tls?.enabled ?? true,
      cert_file: cfg.dicom?.tls?.cert_file ?? '',
      key_file: cfg.dicom?.tls?.key_file ?? '',
      allow_legacy_rc4: cfg.dicom?.tls?.allow_legacy_rc4 ?? false,
    },
  };
  // The DICOM port is deliberately NOT relocated when it is busy. Modalities
  // are configured to reach a specific port, so moving it silently would look
  // exactly like the router having stopped accepting studies. Say so instead,
  // and let the engine fail loudly on the bind.
  const dicomFree = await portIsFree(cfg.dicom.port, null);
  if (!dicomFree) {
    console.warn(`[tarang] DICOM port ${cfg.dicom.port} is already in use. If another ` +
      `instance owns it, give this one its own port in Settings — it will not be moved ` +
      `automatically, because modalities are configured to reach this number.`);
  }

  const preferredHTTP = cfg.http?.port ?? DEFAULT_HTTP_PORT;
  const httpPort = await findFreePort(preferredHTTP);
  if (httpPort !== preferredHTTP) {
    console.warn(`[tarang] HTTP API port ${preferredHTTP} in use; using ${httpPort} instead`);
  }
  cfg.http = {
    port: httpPort,
    api_token: cfg.http?.api_token || crypto.randomBytes(24).toString('hex'),
  };
  cfg.storage = {
    data_dir: DATA_DIR,
    retention_hours: cfg.storage?.retention_hours ?? 24,
    max_disk_gb: cfg.storage?.max_disk_gb ?? 50,
  };
  cfg.peer = {
    name:     peer.name,
    url:      peer.url,
    // Preserve credentials already in config.json — the auth API returns none,
    // so taking peer.username/password here would blank them out on every login.
    username: cfg.peer?.username || peer.username,
    password: cfg.peer?.password || peer.password,
    compression: peer.compression || 'gzip',
    ca_cert_path: peer.caCertPath || '',
    protocol: peer.protocol || 'stow-rs',
  };
  cfg.tag_injection = {
    enabled: true,
    private_creator: labId,
    private_organisation: orgId,
  };
  cfg.transfer = {
    concurrent_workers: cfg.transfer?.concurrent_workers ?? 6,
    bucket_size_mb: cfg.transfer?.bucket_size_mb ?? 32,
    max_http_retries: cfg.transfer?.max_http_retries ?? 5,
    http_timeout_seconds: Math.max(cfg.transfer?.http_timeout_seconds ?? 0, 600),
  };
  // Compression (JPEG2000 via gdcmconv) is a Windows-only feature — gdcmconv and
  // its GDCM DLLs are only bundled in the Windows build. On macOS/Linux it is
  // skipped ENTIRELY: forced off with no gdcmconv path, regardless of any stored
  // value, so series are always sent uncompressed. On Windows it is opt-in and
  // defaults OFF (respects an explicit user opt-in from Settings).
  const compressionSupported = process.platform === 'win32';
  // Mirror the engine's Available() guard here too, so the config we write
  // never claims a compression mode the binary cannot actually perform.
  const gdcmconvUsable = compressionSupported && fs.existsSync(GDCMCONV_BINARY);
  cfg.compression = {
    enabled: compressionSupported ? (cfg.compression?.enabled ?? false) : false,
    skip_already_compressed: cfg.compression?.skip_already_compressed ?? true,
    gdcmconv_path: compressionSupported && fs.existsSync(GDCMCONV_BINARY)
      ? GDCMCONV_BINARY
      : (compressionSupported ? (cfg.compression?.gdcmconv_path ?? '') : ''),
    // Do NOT migrate the old quality-based fields: a quality of 87 (87% quality)
    // is not a rate of 87 (87:1) — reusing it would massively over-compress.
    // Old configs simply fall back to the 10:1 default.
    default_rate: cfg.compression?.default_rate ?? 0,
    modality_rate: cfg.compression?.modality_rate ?? { CT: 10 },

    // JPEG 2000 LOSSLESS (.90) is a separate, independent mode. Like the lossy
    // mode above it now defaults OFF: studies leave byte-for-byte as the
    // modality sent them unless a site opts in. Lossless remains the safe
    // choice when a site does want it — the output is pixel-for-pixel
    // identical and typically 40-60% smaller — but nothing rewrites pixel
    // data by default. It stays force-disabled wherever gdcmconv cannot run,
    // mirroring the engine's own availability guard, so an opt-in on a
    // machine without the binary cannot route series into a transcode queue
    // that cannot succeed.
    lossless: {
      enabled: gdcmconvUsable
        ? (cfg.compression?.lossless?.enabled ?? false)
        : false,
      skip_already_compressed: cfg.compression?.lossless?.skip_already_compressed ?? true,
      // Exceptions only — an absent modality is enabled.
      //
      // Object.assign, NOT `?? {}`: a nullish default only applies when the
      // key is missing entirely, so on every install that already carries a
      // stored map — which is every install that has ever opened Settings —
      // the policy would never arrive. Assigning it over the stored value
      // lands the four policy entries while leaving any other per-modality
      // choice the operator made intact.
      modality_enabled: Object.assign(
        {},
        cfg.compression?.lossless?.modality_enabled,
        LOSSLESS_MODALITY_POLICY,
      ),
    },
  };
  // Backend endpoints follow the server the operator logged into.
  const apiBase = deriveApiBase(apiUrl);
  // Notifier: pre-register study the instant the first instance arrives.
  // No API key — endpoint is open.
  cfg.notifier = {
    backend_url: apiBase + '/orthanc2/instance-exe-received',
    api_key: '',
  };
  // Error reporting: spool every error-level log and POST it to the backend
  // with device + lab identity, so field failures surface centrally.
  cfg.error_reporting = {
    enabled: cfg.error_reporting?.enabled ?? true,
    backend_url: apiBase + '/sender/error-logs',
    api_key: cfg.error_reporting?.api_key ?? '',
  };
  // Lab activation gate: asked before every notification and every upload.
  // fail_open defaults true because wrongly blocking a working lab loses
  // studies, while wrongly allowing a lapsed one costs a few uploads.
  cfg.lab_status = {
    enabled: cfg.lab_status?.enabled ?? true,
    backend_url: apiBase + '/sender/lab-status',
    api_key: cfg.lab_status?.api_key ?? '',
    poll_seconds: cfg.lab_status?.poll_seconds ?? 60,
    fail_open: cfg.lab_status?.fail_open ?? true,
  };
  cfg.resilience = {
    network_retry: cfg.resilience?.network_retry ?? true,
    instance_checkpoint: cfg.resilience?.instance_checkpoint ?? true,
    connectivity_watchdog: cfg.resilience?.connectivity_watchdog ?? true,
  };

  writeConfig(cfg);

  // Collect all non-loopback IPv4 addresses so the renderer can display
  // them in the worklist tab — helps operators configure their modalities.
  const localIPs = (() => {
    const nets = os.networkInterfaces();
    const ips = [];
    for (const iface of Object.values(nets)) {
      for (const addr of iface) {
        if (addr.family === 'IPv4' && !addr.internal) ips.push(addr.address);
      }
    }
    return ips;
  })();

  bootstrap = {
    token: cfg.http.api_token,
    httpPort: cfg.http.port,
    dicomPort: cfg.dicom.port,
    aet: cfg.dicom.aet,
    dataDir: DATA_DIR,
    stableAgeSeconds: cfg.dicom.stable_age_seconds,
    retentionHours: cfg.storage.retention_hours,
    localIPs,
    instanceId: INSTANCE_ID,
    productName: PRODUCT_NAME,
  };
  // Reset suppressRestart so that normal crash restarts work again
  suppressRestart = false;
  startSender();
}

// ------------------------------------------------------------------------
// Process lifecycle
// ------------------------------------------------------------------------

// portIsFree reports whether `port` can be bound right now.
//
// `host` matters: the HTTP API binds 127.0.0.1 while the DICOM receiver binds
// the wildcard, and a loopback probe can succeed against a port some other
// process holds on the wildcard. Probe each one the way its server binds it.
function portIsFree(port, host = '127.0.0.1') {
  return new Promise(resolve => {
    const srv = net.createServer();
    srv.once('error', () => resolve(false));
    srv.once('listening', () => srv.close(() => resolve(true)));
    if (host) srv.listen(port, host); else srv.listen(port);
  });
}

// findFreePort returns `preferred` when it is bindable, else the next free
// port above it.
//
// Only the HTTP API port is ever moved this way. It is loopback-only and it
// reaches the renderer through the bootstrap object, so nothing outside this
// process depends on the number — whereas the DICOM port is written into every
// modality's configuration, and relocating THAT silently would present as the
// router having quietly stopped accepting studies. The chosen port is written
// back to config.json, so an instance keeps the same one across restarts
// instead of drifting upward.
async function findFreePort(preferred, tries = 40) {
  for (let port = preferred; port < preferred + tries; port++) {
    if (await portIsFree(port)) return port;
  }
  return preferred; // give up; let the engine report the real bind error
}

// ---- engine pid tracking -------------------------------------------------
//
// Reaping orphans used to be a blanket `taskkill /IM tarang-sender.exe`, which
// is fatal the moment two routers share a machine: starting one would kill the
// other's engine, mid-transfer, with no indication why. So each instance
// records the pid of its own child and reaps only that.

function readSenderPid() {
  try {
    const pid = Number(fs.readFileSync(SENDER_PID_PATH, 'utf8').trim());
    return Number.isInteger(pid) && pid > 0 ? pid : null;
  } catch (_) {
    return null;
  }
}

function writeSenderPid(pid) {
  try { fs.writeFileSync(SENDER_PID_PATH, String(pid)); } catch (_) {}
}

function clearSenderPid() {
  try { fs.unlinkSync(SENDER_PID_PATH); } catch (_) {}
}

// pidIsSender confirms the pid is still OUR kind of process before anything
// kills it. Windows recycles pids aggressively, so acting on a stale pid file
// alone risks killing whatever unrelated process inherited the number.
function pidIsSender(pid) {
  try {
    const { spawnSync } = require('child_process');
    if (process.platform === 'win32') {
      const r = spawnSync('tasklist', ['/FI', `PID eq ${pid}`, '/NH', '/FO', 'CSV'], { encoding: 'utf8' });
      return (r.stdout || '').toLowerCase().includes(SENDER_EXE_NAME);
    }
    const r = spawnSync('ps', ['-p', String(pid), '-o', 'comm='], { encoding: 'utf8' });
    return (r.stdout || '').toLowerCase().includes(SENDER_EXE_NAME);
  } catch (_) {
    return false;
  }
}

// listSenderPids returns the pid of every running engine on the machine,
// across all instances.
function listSenderPids() {
  try {
    const { spawnSync } = require('child_process');
    if (process.platform === 'win32') {
      const r = spawnSync('tasklist', ['/FI', `IMAGENAME eq ${SENDER_EXE_NAME}.exe`, '/NH', '/FO', 'CSV'],
        { encoding: 'utf8' });
      // tasklist CSV rows look like:
      //   "tarang-sender.exe","1234","Console","1","8,000 K"
      // Split on the quote-comma-quote seam rather than pattern-matching, so
      // a localised header or an odd column cannot break the parse.
      return (r.stdout || '')
        .split('\n')
        .map(line => Number((line.split('","')[1] || '').trim()))
        .filter(pid => Number.isInteger(pid) && pid > 0);
    }
    const r = spawnSync('pgrep', ['-f', SENDER_EXE_NAME], { encoding: 'utf8' });
    return (r.stdout || '')
      .split('\n')
      .map(line => Number(line.trim()))
      .filter(pid => Number.isInteger(pid) && pid > 0);
  } catch (_) {
    return [];
  }
}

// sendersForThisProfile returns the pid of every engine running with THIS
// instance's config file on its command line.
//
// Ownership is read from the process itself rather than from a pid file.
// Every engine is spawned as `tarang-sender.exe --config <profile>\achyu\
// config.json`, so the profile it serves is written into the process and
// cannot be confused with another product's.
//
// This replaced a scan for sibling pid files, and the difference is not
// cosmetic -- the old scan cost a live site its transfers. The pid file is
// recent, so a co-resident build old enough to predate it writes none at all.
// The scan therefore saw no siblings, concluded it had the machine to itself,
// and swept an engine that was in active use. A command line is present
// whatever the other build's age, branding, or vintage, so this cannot repeat.
function sendersForThisProfile() {
  if (process.platform !== 'win32') return [];
  try {
    const { spawnSync } = require('child_process');
    // Get-WmiObject, not Get-CimInstance: this has to work on the Windows 7
    // machines still in the field, which ship PowerShell 2.0.
    const script =
      `Get-WmiObject Win32_Process -Filter "Name='${SENDER_EXE_NAME}.exe'" | ` +
      "ForEach-Object { $_.ProcessId.ToString() + '|' + $_.CommandLine }";
    const r = spawnSync('powershell',
      ['-NoProfile', '-NonInteractive', '-Command', script],
      { encoding: 'utf8', windowsHide: true, timeout: 20000 });

    const want = CONFIG_PATH.toLowerCase();
    const out = [];
    for (const line of (r.stdout || '').split('\n')) {
      const bar = line.indexOf('|');
      if (bar < 0) continue;
      const pid = Number(line.slice(0, bar).trim());
      const cmd = line.slice(bar + 1).toLowerCase();
      if (Number.isInteger(pid) && pid > 0 && cmd.includes(want)) out.push(pid);
    }
    return out;
  } catch (_) {
    // No answer means no evidence of ownership, and without evidence nothing
    // is killed. Our own stale engine is recoverable -- it surfaces as a bind
    // failure we log. Another product's engine, mid-transfer, is not.
    return [];
  }
}

// reapOwnSenders kills engines left behind by a previous session of THIS
// instance: ones the pid file missed because it did not exist in the build
// that spawned them, or lost when Electron was hard-killed.
//
// It can only reach engines serving our own config file, so a co-resident
// product's engine is out of range by construction rather than by convention.
// That matters because the aggressor here does not have to be us -- a build
// old enough to predate this still runs `taskkill /F /IM tarang-sender.exe`
// and will kill ours on sight. Being correct on our side is what stops this
// machine's two routers from taking turns killing each other; the other side
// has to ship this fix too before the loop actually ends.
function reapOwnSenders() {
  for (const pid of sendersForThisProfile()) {
    if (senderProc && pid === senderProc.pid) continue;
    try {
      const { spawnSync } = require('child_process');
      spawnSync('taskkill', ['/F', '/PID', String(pid)], { stdio: 'ignore' });
      console.log(`[tarang] reaped our own orphaned engine pid=${pid}`);
    } catch (_) {}
  }
}

// killOrphanedSender reaps an engine left behind by a previous session of THIS
// instance — one that outlived an Electron crash and still holds this
// instance's DICOM port, HTTP port and BoltDB file.
function killOrphanedSender() {
  const pid = readSenderPid();
  if (!pid) return;
  if (!pidIsSender(pid)) { clearSenderPid(); return; }
  try {
    const { spawnSync } = require('child_process');
    if (process.platform === 'win32') {
      spawnSync('taskkill', ['/F', '/PID', String(pid)], { stdio: 'ignore' });
    } else {
      process.kill(pid, 'SIGKILL');
    }
    console.log(`[tarang] reaped orphaned engine pid=${pid}`);
  } catch (_) {}
  clearSenderPid();
}

function startSender() {
  if (senderProc) return;
  suppressRestart = false;

  // Reap an engine left over from a previous session of this instance that
  // wasn't shut down cleanly. Without this the old process keeps the HTTP port
  // bound with a stale token and the new frontend hangs on its first status
  // fetch ("Connecting…" forever).
  //
  // Scoped to our OWN recorded pid, never to the image name: a blanket
  // taskkill would reach into a second instance and kill its engine.
  killOrphanedSender();
  // Then any engine still serving OUR config from a previous session --
  // see reapOwnSenders. Never reaches another product's engine.
  reapOwnSenders();

  if (!fs.existsSync(SENDER_BINARY)) {
    console.error(`[tarang] sender binary not found at ${SENDER_BINARY}`);
    return;
  }
  console.log(`[tarang] spawning ${SENDER_BINARY}`);

  senderProc = spawn(SENDER_BINARY, ['--config', CONFIG_PATH], {
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  });
  writeSenderPid(senderProc.pid);

  senderProc.stdout.on('data', d => process.stdout.write(d));
  senderProc.stderr.on('data', d => process.stderr.write(d));

  senderProc.on('exit', (code, signal) => {
    console.warn(`[tarang] sender exited code=${code} signal=${signal}`);
    senderProc = null;
    // The pid is dead; drop it so the next start does not try to reap a
    // number Windows may since have handed to something else.
    clearSenderPid();
    if (app.isReady() && !app.isQuitting && !suppressRestart) {
      // Exponential-backoff restart, capped at 30s.
      restartAttempts = Math.min(restartAttempts + 1, 6);
      const delay = Math.min(30000, 1000 * Math.pow(2, restartAttempts - 1));
      console.warn(`[tarang] restarting in ${delay}ms (attempt ${restartAttempts})`);
      restartTimer = setTimeout(startSender, delay);
    }
    suppressRestart = false;
  });

  // Reset attempts after a successful 30s of uptime.
  setTimeout(() => {
    if (senderProc) restartAttempts = 0;
  }, 30000);
}

function stopSender(options = {}) {
  if (options.permanent === true) {
    suppressRestart = true;
  }
  if (restartTimer) {
    clearTimeout(restartTimer);
    restartTimer = null;
  }
  if (!senderProc) return;
  console.log('[tarang] stopping sender');
  // SIGTERM lets the sender drain associations and shut down cleanly.
  senderProc.kill('SIGTERM');
  // Hard stop after 10s if it's still around.
  setTimeout(() => {
    if (senderProc) senderProc.kill('SIGKILL');
  }, 10000);
}

// ------------------------------------------------------------------------
// IPC handlers used by the renderer
// ------------------------------------------------------------------------

function registerIPC() {
  ipcMain.handle('tarang:get-bootstrap', () => bootstrap);

  ipcMain.handle('tarang:is-authenticated', () => {
    return Boolean(readSession());
  });

  ipcMain.handle('tarang:get-session', () => {
    const s = readSession();
    if (!s) return null;
    return {
      labId: s.labId,
      orgId: s.orgId,
      userName: s.userName,
      apiUrl: s.apiUrl,
      loginTime: s.loginTime,
    };
  });

  ipcMain.handle('tarang:login', async (_evt, credentials) => {
    if (!credentials || !credentials.token) {
      throw new Error('invalid login payload: missing token');
    }

    fs.mkdirSync(APP_DATA_DIR, { recursive: true });
    const existingConfig = readConfig() || {};
    const existingPeer = existingConfig.peer || null;
    const session = buildSessionFromCredentials(credentials, existingPeer);
    writeSession(session);

    await bootstrapSender({
      labId: session.labId,
      orgId: session.orgId,
      peer: session.peer,
      apiUrl: session.apiUrl,
    });

    return { success: true };
  });

  ipcMain.handle('tarang:logout', async () => {
    clearSession();
    bootstrap = null;
    stopSender({ permanent: true });
    loadRendererPage('login.html');
    return { success: true };
  });

  ipcMain.handle('tarang:update-config', async (_evt, patch) => {
    const cfg = readConfig();
    if (!cfg) throw new Error('config not bootstrapped');
    if (patch.aet != null) {
      cfg.dicom = cfg.dicom || {};
      // Trimmed only because the wire field is space-padded; case and
      // characters are deliberately not policed, since the called AE title
      // never gates an association.
      cfg.dicom.aet = String(patch.aet).trim().slice(0, 16);
    }
    if (patch.stable_age_seconds != null) {
      cfg.dicom = cfg.dicom || {};
      cfg.dicom.stable_age_seconds = Number(patch.stable_age_seconds);
    }
    if (patch.dicom_port != null) {
      cfg.dicom = cfg.dicom || {};
      cfg.dicom.port = Number(patch.dicom_port);
    }
    if (patch.retention_hours != null) {
      cfg.storage = cfg.storage || {};
      cfg.storage.retention_hours = Number(patch.retention_hours);
    }
    writeConfig(cfg);
    if (bootstrap) {
      bootstrap.stableAgeSeconds = cfg.dicom.stable_age_seconds;
      bootstrap.retentionHours = cfg.storage.retention_hours;
      if (patch.dicom_port != null) bootstrap.dicomPort = cfg.dicom.port;
      if (patch.aet != null) bootstrap.aet = cfg.dicom.aet;
    }
    return { ok: true };
  });

  ipcMain.handle('tarang:read-recent-logs', async (_evt, opts = {}) => {
    const limit = Math.min(2000, opts.limit || 500);
    const today = new Date().toISOString().slice(0, 10);
    const file = path.join(LOG_DIR, `tarang-${today}.log`);
    if (!fs.existsSync(file)) return [];
    // Read the last ~1MB of the file and return the last N lines.
    const stat = fs.statSync(file);
    const tailBytes = Math.min(1024 * 1024, stat.size);
    const fd = fs.openSync(file, 'r');
    const buf = Buffer.alloc(tailBytes);
    fs.readSync(fd, buf, 0, tailBytes, stat.size - tailBytes);
    fs.closeSync(fd);
    const lines = buf.toString('utf8').split(/\r?\n/).filter(Boolean);
    return lines.slice(-limit);
  });
}

// ------------------------------------------------------------------------
// App lifecycle
// ------------------------------------------------------------------------

const gotTheLock = app.requestSingleInstanceLock();

if (!gotTheLock) {
  app.quit();
} else {
  app.on('second-instance', (event, commandLine, workingDirectory) => {
    // Someone tried to run a second instance, we should focus our window.
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore();
      mainWindow.focus();
    }
  });

  app.on('before-quit', () => {
    app.isQuitting = true;
    stopSender({ permanent: true });
  });

  app.whenReady().then(async () => {
    // Register for auto-start on Windows login so the sender comes up
    // automatically after a reboot — no manual intervention needed.
    // One autostart entry PER INSTANCE. The name is the registry value name,
    // so two instances sharing it would overwrite each other and only the
    // last one written would come back after a reboot. `args` is what makes
    // the restored instance the right one — without it Windows relaunches the
    // bare exe, which is always the default instance.
    app.setLoginItemSettings({
      openAtLogin: true,
      name: `${PRODUCT_NAME}${INSTANCE_LABEL}`,
      args: INSTANCE_ID ? [`--instance=${INSTANCE_ID}`] : [],
    });

    registerIPC();

    const session = readSession();
    if (session) {
      try {
        await bootstrapSender({
          labId: session.labId,
          orgId: session.orgId,
          peer: normalizePeer(session.peer, null),
          apiUrl: session.apiUrl,
        });
      } catch (err) {
        console.error('[tarang] startup bootstrap failed:', err);
        // bootstrap stays null — renderer will redirect to login
      }
    }

    Menu.setApplicationMenu(null);

    mainWindow = new BrowserWindow({
      width: 1400, height: 900,
      title: `${PRODUCT_NAME}${INSTANCE_LABEL}`,
      icon: path.join(__dirname, '..', 'logo.ico'),
      autoHideMenuBar: true,
      webPreferences: {
        preload: path.join(__dirname, 'preload.js'),
        contextIsolation: true,
        nodeIntegration: false,
      },
    });

    // A page's own <title> replaces the BrowserWindow title option the moment
    // it loads, so setting the title once above is not enough — both windows
    // end up reading "Achyu PACS" in the taskbar and in Alt-Tab, which is
    // precisely where an operator needs to tell two routers apart. Re-apply
    // the label on every title change instead.
    mainWindow.on('page-title-updated', (event, title) => {
      event.preventDefault();
      mainWindow.setTitle(`${PRODUCT_NAME}${INSTANCE_LABEL}`);
    });

    // X minimizes to the taskbar rather than closing, because the sender must
    // keep receiving studies. Minimize, NOT hide: hiding removes the window
    // from the taskbar, which reads as "the app quit" and sends operators
    // hunting the tray or relaunching while the sender is still running.
    mainWindow.on('close', (event) => {
      if (app.isQuitting) return;
      event.preventDefault();
      mainWindow.minimize();
    });

    createTray();

    if (session) {
      mainWindow.loadFile(path.join(__dirname, '..', 'renderer', 'index.html'));
    } else {
      mainWindow.loadFile(path.join(__dirname, '..', 'renderer', 'login.html'));
    }
  });
}

// createTray gives the operator the only two things they ever need from the
// tray: get the window back, and actually quit.
function createTray() {
  try {
    tray = new Tray(path.join(__dirname, '..', 'logo.ico'));
  } catch (err) {
    console.error('[tarang] tray unavailable:', err.message);
    return;
  }

  const showWindow = () => {
    if (!mainWindow || mainWindow.isDestroyed()) return;
    if (mainWindow.isMinimized()) mainWindow.restore();
    mainWindow.show();
    mainWindow.focus();
  };

  // Two instances put two icons in the tray. Without the id in the tooltip
  // and the menu, an operator has no way to tell which one they are quitting.
  tray.setToolTip(`${PRODUCT_NAME}${INSTANCE_LABEL}`);
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: `Open ${PRODUCT_NAME}${INSTANCE_LABEL}`, click: showWindow },
    { type: 'separator' },
    {
      label: 'Quit',
      click: () => {
        // Tell the close handler above to step aside for this one.
        app.isQuitting = true;
        app.quit();
      },
    },
  ]));
  tray.on('double-click', showWindow);
}

// Exported so the existing Electron app (which has its own login) can
// call this from its post-login handler.
module.exports = { bootstrapSender, stopSender };
