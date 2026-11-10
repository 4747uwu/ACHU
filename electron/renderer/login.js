const DEFAULT_API_URL   = 'https://pacs.achyutrs.com/api/auth/lab-login';
const DEFAULT_PEER_URL  = 'https://router.achyutrs.com';
// Keep in sync with TAB_PASS in app.js. This is UI access control, not
// cryptography — it ships in cleartext inside the renderer bundle.
const SETTINGS_PASSWORD = '18967';

// Backend API hosts, in preference order. Duplicated from API_HOSTS in
// electron/main/main.js on purpose: login runs before any config exists and
// the renderer cannot import from the main process. Keep the two in sync.
const API_HOSTS = ['pacs.achyutrs.com', 'achyu.xcentic.com'];

// The same login path on every known host, the host actually in apiUrl first
// when it is one of ours. A custom server is returned untouched and alone —
// an operator's own backend must never be retried against ours.
function loginUrlsFor(apiUrl) {
  let u; try { u = new URL(apiUrl); } catch (_) { return [apiUrl]; }
  const here = u.hostname.toLowerCase();
  if (!API_HOSTS.some((h) => h === here)) return [apiUrl];
  const hosts = [here, ...API_HOSTS.filter((h) => h !== here)];
  return hosts.map((h) => {
    const alt = new URL(apiUrl);
    alt.hostname = h;
    alt.protocol = 'https:';
    return alt.toString();
  });
}

// POST the login to each known host in turn. Only a host that is down or
// broken (thrown error, or 5xx) moves us to the next one.
//
// A 4xx is NEVER retried: it is an answer, not an outage. Replaying it would
// send a wrong password to a second host — doubling the failed-attempt count
// against the account and leaking the credential to one more server — and
// would report the second host's error for the first host's rejection.
async function loginFetch(apiUrl, options, fetchImpl) {
  const doFetch = fetchImpl || fetch;
  const urls = loginUrlsFor(apiUrl);
  for (let i = 0; i < urls.length; i++) {
    const last = i === urls.length - 1;
    try {
      const res = await doFetch(urls[i], options);
      if (res.status >= 500 && !last) continue; // host broken — try the next
      return res;                               // including every 4xx
    } catch (err) {
      if (last) throw err;                      // nowhere left to ask
    }
  }
}

const K_API_URL   = 'tarang_api_url';
const K_PEER_URL  = 'tarang_peer_url';
const K_PEER_USER = 'tarang_peer_user';

// ── Init ───────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', async () => {
  // Skip login if already authenticated.
  if (window.tarangAPI?.isAuthenticated) {
    try {
      if (await window.tarangAPI.isAuthenticated()) {
        window.location.href = 'index.html';
        return;
      }
    } catch (_) {}
  }

  if (new URLSearchParams(window.location.search).get('logout') === '1') {
    setStatus('Logged out successfully');
    setTimeout(() => setStatus(''), 3000);
  }

  document.getElementById('username').focus();
});

// ── Login ──────────────────────────────────────────────────────────────────

async function handleLogin(event) {
  event.preventDefault();

  const username = document.getElementById('username').value.trim();
  const password = document.getElementById('password').value.trim();

  if (!username || !password) { showErr('Username and password are required.'); return; }

  const apiUrl  = localStorage.getItem(K_API_URL)  || DEFAULT_API_URL;
  const peerUrl = localStorage.getItem(K_PEER_URL) || DEFAULT_PEER_URL;
  const peerUser = localStorage.getItem(K_PEER_USER) || '';

  setLoading(true);
  clearErr();

  try {
    const res = await loginFetch(apiUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        email: username,
        password,
        peer: { url: peerUrl, username: peerUser },
      }),
    });

    const data = await res.json().catch(() => ({}));

    if (!res.ok)             throw new Error(data.message || `Login failed (${res.status})`);
    if (data.success === false) throw new Error(data.message || 'Login rejected by server');

    const token = data.token;
    if (!token) throw new Error('No authentication token received');

    const user       = data.user || {};
    const labId      = user.lab?.identifier || user.labId || 'DEFAULT';
    const orgId      = user.organizationIdentifier || user.orgId || 'DEFAULT';
    const labSettings = user.lab?.settings || {};
    const serverPeer  = labSettings.peer || {};

    // Peer credentials: locally saved values take priority over server values.
    const peerPassEl = document.getElementById('cfgPeerPass');
    const savedPeerPass = peerPassEl?.value || '';

    const peer = {
      name:        serverPeer.name        || 'ACHYU',
      url:         peerUrl                || serverPeer.url      || DEFAULT_PEER_URL,
      username:    peerUser               || serverPeer.username || '',
      password:    savedPeerPass          || serverPeer.password || '',
      protocol:    serverPeer.protocol    || 'stow-rs',
      compression: serverPeer.compression || 'gzip',
      caCertPath:  serverPeer.caCertPath  || '',
    };

    const credentials = {
      token, labId, orgId,
      userName:  user.name || username,
      apiUrl,
      labSettings,
      loginTime: new Date().toISOString(),
      peer,
    };

    if (window.tarangAPI?.login) {
      const result = await window.tarangAPI.login(credentials);
      if (!result.success) throw new Error(result.error || 'Failed to save session');
    } else {
      localStorage.setItem('tarang_auth', JSON.stringify(credentials));
    }

    window.location.href = 'index.html';

  } catch (err) {
    console.error('[LOGIN]', err);
    showErr(err.message || 'Connection error — check settings and try again.');
    setLoading(false);
  }
}

function setLoading(on) {
  document.getElementById('spinner').classList.toggle('show', on);
  document.getElementById('loginBtn').disabled = on;
}
function showErr(msg) {
  const el = document.getElementById('errBox');
  el.textContent = msg; el.classList.add('show');
}
function clearErr() {
  const el = document.getElementById('errBox');
  el.classList.remove('show'); el.textContent = '';
}
function setStatus(msg) {
  document.getElementById('statusMsg').textContent = msg;
}

// ── Settings overlay ───────────────────────────────────────────────────────

function openGate() {
  document.getElementById('overlay').classList.add('show');
  document.getElementById('gate').style.display = '';
  document.getElementById('settingsForm').classList.remove('show');
  document.getElementById('gatePass').value = '';
  document.getElementById('gateErr').textContent = '';
  setTimeout(() => document.getElementById('gatePass').focus(), 60);
}

function closeOverlay() {
  document.getElementById('overlay').classList.remove('show');
  document.getElementById('gatePass').value = '';
  document.getElementById('gateErr').textContent = '';
}

function gateKeydown(e) {
  if (e.key === 'Enter') { e.preventDefault(); unlockSettings(); }
  if (e.key === 'Escape') closeOverlay();
}

function unlockSettings() {
  const entered = document.getElementById('gatePass').value;
  if (entered !== SETTINGS_PASSWORD) {
    document.getElementById('gateErr').textContent = 'Incorrect password.';
    document.getElementById('gatePass').value = '';
    document.getElementById('gatePass').focus();
    return;
  }
  // Correct — hide gate, show settings and populate fields.
  document.getElementById('gate').style.display = 'none';
  document.getElementById('settingsForm').classList.add('show');

  document.getElementById('cfgApiUrl').value   = localStorage.getItem(K_API_URL)   || DEFAULT_API_URL;
  document.getElementById('cfgPeerUrl').value  = localStorage.getItem(K_PEER_URL)  || DEFAULT_PEER_URL;
  document.getElementById('cfgPeerUser').value = localStorage.getItem(K_PEER_USER) || '';
  document.getElementById('cfgPeerPass').value = '';

  document.getElementById('cfgApiUrl').focus();
}

function saveSettings() {
  const apiUrl   = document.getElementById('cfgApiUrl').value.trim()   || DEFAULT_API_URL;
  const peerUrl  = document.getElementById('cfgPeerUrl').value.trim()  || DEFAULT_PEER_URL;
  const peerUser = document.getElementById('cfgPeerUser').value.trim();

  localStorage.setItem(K_API_URL,   apiUrl);
  localStorage.setItem(K_PEER_URL,  peerUrl);
  localStorage.setItem(K_PEER_USER, peerUser);

  closeOverlay();
}

// Close overlay on backdrop click.
document.getElementById('overlay').addEventListener('click', function(e) {
  if (e.target === this) closeOverlay();
});
