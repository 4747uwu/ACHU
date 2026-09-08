// renderer/api.js — thin wrapper around the local sender's HTTP API.
//
// The Electron main process owns the API token: it generates a random one
// at login, writes it into the sender's config.json, spawns the binary,
// then exposes the token to the renderer via window.tarangAPI.ready (a
// promise resolving to {token, httpPort, ...}, set by preload.js).
// The renderer never touches disk.
//
// Development fallback: if window.tarangAPI is missing (e.g. you opened
// renderer/index.html directly in a browser to iterate on UI), we honor
// URL query params ?token=…&port=… and stub the IPC methods. Lets us
// poke the live UI against a running sender without spinning up Electron.
//
// All non-health endpoints require Authorization: Bearer <token>.
// On any non-2xx response we throw with the server's error message
// pre-extracted; the UI can surface it to the operator.

(function () {
  // Dev fallback path — only fires outside Electron.
  if (!window.tarangAPI) {
    const qs = new URLSearchParams(window.location.search);
    const token = qs.get('token') || 'dev-token';
    const port  = Number(qs.get('port') || 9044);
    window.tarangAPI = {
      ready: Promise.resolve({
        token,
        httpPort: port,
        dicomPort: Number(qs.get('dicom') || 1008),
        aet: 'ACHYU',
        dataDir: '(dev)',
        stableAgeSeconds: 15,
        retentionHours: 24,
        localIPs: ['127.0.0.1'],
      }),
      updateConfig: async (patch) => {
        console.log('[dev] updateConfig (no-op)', patch);
        return { ok: true };
      },
      readRecentLogs: async () => {
        return [
          't=00:00:00.000 level=INFO msg="dev mode — no real log file" hint="run inside Electron for live tail"',
        ];
      },
    };
    console.warn('tarang: running in dev mode without Electron preload (token from ?token= query param)');
  }

  window.tarangAPI.ready.then((boot) => {
    if (!boot) {
      // Bootstrap not ready — send back to login so the user can re-authenticate
      // and trigger a fresh bootstrapSender() call.
      console.warn('tarang: bootstrap missing, redirecting to login');
      if (!window.location.pathname.endsWith('login.html')) {
        window.location.href = 'login.html';
      }
      return;
    }
    const BASE  = `http://127.0.0.1:${boot.httpPort}`;
    let TOKEN = boot.token;

    async function request(method, path, body) {
      const doFetch = (tok) => {
        const headers = { 'Authorization': `Bearer ${tok}` };
        const opts = { method, headers };
        if (body !== undefined) {
          headers['Content-Type'] = 'application/json';
          opts.body = JSON.stringify(body);
        }
        return fetch(`${BASE}${path}`, opts);
      };

      let resp = await doFetch(TOKEN);

      // Token mismatch (binary restarted with same token now that we preserve it,
      // but guard against edge cases by re-fetching bootstrap on 401).
      if (resp.status === 401) {
        try {
          const fresh = await window.tarangAPI.ready;
          if (fresh && fresh.token && fresh.token !== TOKEN) {
            TOKEN = fresh.token;
            resp = await doFetch(TOKEN);
          }
        } catch (_) {}
      }

      if (!resp.ok) {
        let msg = `${method} ${path}: HTTP ${resp.status}`;
        try {
          const j = await resp.json();
          if (j && j.error) msg = `${method} ${path}: ${j.error}`;
        } catch (_) {}
        const err = new Error(msg);
        err.status = resp.status;
        throw err;
      }
      if (resp.status === 204) return null;
      const ct = resp.headers.get('Content-Type') || '';
      if (ct.startsWith('application/json')) return resp.json();
      return resp.text();
    }

    window.tarang = {
      token: TOKEN,
      // Base URL of the local sender API. Exposed so other renderer modules
      // (settings-form, progress-modal) hit the SAME IPv4 host+port instead of
      // hardcoding "localhost:<port>" — "localhost" can resolve to IPv6 ::1 on
      // Windows 7, where the Go server (IPv4 127.0.0.1 only) isn't listening,
      // producing "failed to fetch".
      baseUrl: BASE,
      status:   () => request('GET', '/api/status'),
      health:   () => fetch(`${BASE}/api/health`).then(r => r.json()),
      getConfig: () => request('GET', '/api/config'),
      patchConfig: (patch) => request('PATCH', '/api/config', patch),

      // The activation gate. GET reads the cached verdict; POST forces a
      // fresh check — which is what "Check again" must call, because a GET
      // may answer from a verdict seconds old and so appear to do nothing
      // right after an account is reactivated.
      labStatus:        () => request('GET',  '/api/lab-status'),
      refreshLabStatus: () => request('POST', '/api/lab-status'),

      worklist: ({ status = '', limit = 100 } = {}) => {
        const qs = new URLSearchParams();
        if (status) qs.set('status', status);
        qs.set('limit', String(limit));
        return request('GET', `/api/worklist?${qs}`);
      },

      studyDetail: (uid) => request('GET', `/api/study/${encodeURIComponent(uid)}`),
      retryStudy:  (uid) => request('POST', `/api/study/${encodeURIComponent(uid)}/retry`),
      deleteStudy: (uid) => request('DELETE', `/api/study/${encodeURIComponent(uid)}`),
      transferProgress: (seriesUID) => request('GET', `/api/transfer/${encodeURIComponent(seriesUID)}/progress`),

      queueDepth: () => request('GET', '/api/queue/depth'),
    };

    document.dispatchEvent(new CustomEvent('tarang-ready'));
  });
})();

