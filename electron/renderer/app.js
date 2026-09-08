// renderer/app.js — orchestrates the four tabs.
//
// The renderer polls the local Go service for state. Polling beats
// WebSockets here for one big reason: the UI is single-window and
// localhost-only, so the simplicity wins. We poll the worklist every
// 3 seconds while it's visible, and pause polling when the tab loses
// focus to spare the CPU on idle clinic workstations.
//
// All initialization runs after 'tarang-ready' fires (dispatched by
// api.js once the preload bootstrap is in place).

let appInitialized = false;

function bootstrapApp() {
  if (appInitialized) return;
  if (!window.tarang) return;
  appInitialized = true;
  init();
}

document.addEventListener('tarang-ready', bootstrapApp);

// In dev mode, api.js may dispatch tarang-ready before app.js loads.
// This direct check ensures we still initialize.
bootstrapApp();

function init() {
  'use strict';

  // ---- tabs --------------------------------------------------------

  const tabs  = document.querySelectorAll('.topbar .tab');
  const panes = document.querySelectorAll('.content .pane');

  const RESTRICTED_TABS = new Set(['settings', 'connection', 'logs']);
  // Keep in sync with SETTINGS_PASSWORD in login.js. This gates the
  // settings/connection/logs tabs from curious reception staff; it is UI
  // access control, not cryptography.
  const TAB_PASS = '18967';
  let tabsUnlocked = false;
  let pendingTab = null;

  window.tabGateCancel = function () {
    document.getElementById('tab-gate-overlay').style.display = 'none';
    document.getElementById('tab-gate-pass').value = '';
    document.getElementById('tab-gate-err').textContent = '';
    pendingTab = null;
  };
  window.tabGateSubmit = function () {
    const val = document.getElementById('tab-gate-pass').value;
    if (val !== TAB_PASS) {
      document.getElementById('tab-gate-err').textContent = 'Incorrect password.';
      document.getElementById('tab-gate-pass').value = '';
      document.getElementById('tab-gate-pass').focus();
      return;
    }
    tabsUnlocked = true;
    document.getElementById('tab-gate-overlay').style.display = 'none';
    document.getElementById('tab-gate-pass').value = '';
    document.getElementById('tab-gate-err').textContent = '';
    if (pendingTab) { setTab(pendingTab); pendingTab = null; }
  };
  document.getElementById('tab-gate-pass').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') window.tabGateSubmit();
    if (e.key === 'Escape') window.tabGateCancel();
  });

  let activeTab = 'worklist';
  function setTab(name) {
    activeTab = name;
    tabs.forEach(t => t.classList.toggle('active', t.dataset.tab === name));
    panes.forEach(p => p.classList.toggle('active', p.dataset.pane === name));
    if (name === 'worklist')   refreshWorklist();
    if (name === 'settings')   loadSettings();
    if (name === 'connection') refreshConnection();
    if (name === 'logs')       refreshLogs();
  }
  tabs.forEach(t => t.addEventListener('click', () => {
    const name = t.dataset.tab;
    if (RESTRICTED_TABS.has(name) && !tabsUnlocked) {
      pendingTab = name;
      document.getElementById('tab-gate-title').textContent =
        name.charAt(0).toUpperCase() + name.slice(1);
      document.getElementById('tab-gate-err').textContent = '';
      document.getElementById('tab-gate-pass').value = '';
      const overlay = document.getElementById('tab-gate-overlay');
      overlay.style.display = 'flex';
      setTimeout(() => document.getElementById('tab-gate-pass').focus(), 60);
      return;
    }
    setTab(name);
  }));

  // ---- top-bar status & footer ------------------------------------

  const connDot   = document.getElementById('conn-dot');
  const connLabel = document.getElementById('conn-label');
  const footFlight = document.getElementById('foot-flight');
  const footStable = document.getElementById('foot-stable');
  const footVersion = document.getElementById('foot-version');
  const footRecv = document.getElementById('foot-receive');
  const logoutBtn = document.getElementById('user-logout');

  if (logoutBtn && window.tarangAPI && typeof window.tarangAPI.logout === 'function') {
    logoutBtn.addEventListener('click', async () => {
      try {
        await window.tarangAPI.logout();
      } catch (err) {
        toast('error', `Logout failed: ${err.message}`);
      }
    });
  }

  // Populate DICOM chip from bootstrap (available immediately — no poll needed).
  // AET and port are shown once; every local IP is a click-to-copy chip.
  let chipIPs  = ['127.0.0.1'];
  let chipPort = 8899;
  let chipAET  = 'ACHYU';

  // refreshDicomChip re-renders the chip after the AE title or port is edited
  // in Settings, so the operator sees what they just saved without a restart.
  function refreshDicomChip(aet, port) {
    if (aet) chipAET = aet;
    if (port) chipPort = port;
    renderDicomChip(chipAET, chipPort, chipIPs);
  }

  window.tarangAPI.ready.then((boot) => {
    if (!boot) return;
    chipIPs  = (boot.localIPs && boot.localIPs.length) ? boot.localIPs : ['127.0.0.1'];
    chipPort = boot.dicomPort || 8899;
    chipAET  = boot.aet || 'ACHYU';
    renderDicomChip(chipAET, chipPort, chipIPs);

    // With two routers on one machine the windows are otherwise identical,
    // and acting on the wrong one is easy to do and hard to notice.
    const badge = document.getElementById('instance-badge');
    if (badge && boot.instanceId) {
      badge.textContent = boot.instanceId;
      badge.hidden = false;
      document.title = `Achyu PACS (${boot.instanceId})`;
    }
  });

  function renderDicomChip(aet, port, ips) {
    const el = document.getElementById('dicom-ip-info');
    if (!el) return;

    el.innerHTML =
      `<span class="dicom-seg">AET <b>${escapeText(aet)}</b></span>` +
      `<span class="dicom-div"></span>` +
      `<span class="dicom-seg">Port <b>${port}</b></span>` +
      `<span class="dicom-div"></span>` +
      ips.map(ip =>
        `<button type="button" class="ip-copy" data-ip="${escapeAttr(ip)}" title="Click to copy">${escapeText(ip)}</button>`
      ).join('');

    el.querySelectorAll('.ip-copy').forEach(btn => {
      btn.addEventListener('click', async () => {
        const ip = btn.dataset.ip;
        try {
          await navigator.clipboard.writeText(ip);
        } catch (_) {
          const ta = document.createElement('textarea');
          ta.value = ip;
          ta.style.position = 'fixed'; ta.style.opacity = '0';
          document.body.appendChild(ta);
          ta.select();
          try { document.execCommand('copy'); } catch (_) {}
          ta.remove();
        }
        const original = ip;
        btn.classList.add('copied');
        btn.textContent = 'Copied ✓';
        setTimeout(() => { btn.textContent = original; btn.classList.remove('copied'); }, 1100);
        toast('info', `Copied ${ip}`);
      });
    });
  }

  let lastStatus = null;

  async function refreshStatus() {
    try {
      const s = await window.tarang.status();
      lastStatus = s;

      // The pill reports whether studies can actually LEAVE this machine,
      // which is the peer probe — not whether this loopback call worked.
      // Driving it from the local socket meant it sat green for 90 minutes
      // at a site where nothing had uploaded, so nobody rang us.
      if (s.peer_online === false) {
        // Amber, not red: the sender itself is healthy and still receiving and
        // storing studies. What has stopped is delivery. This is the exact
        // state that sat green for 90 minutes and so never got reported.
        connDot.className = 'dot warn';
        connLabel.textContent = 'Receiver unreachable';
        connLabel.title = s.peer_error || 'The PACS receiver is not responding.';
      } else {
        connDot.className = 'dot ok';
        connLabel.textContent = 'Connected';
        connLabel.title = s.peer_last_success
          ? `Receiver last confirmed ${new Date(s.peer_last_success).toLocaleTimeString()}`
          : '';
      }

      footFlight.textContent  = `in flight ${s.queue_depth}`;
      footVersion.textContent = `v${s.version}`;
      footRecv.textContent    = `recv ${s.dicom_port}`;
      renderLabBanner(s);
    } catch (err) {
      // This branch is now specifically "the local engine is not answering",
      // which is a different failure from the receiver being unreachable.
      connDot.className = 'dot err';
      connLabel.textContent = 'Sender offline';
      connLabel.title = err.message || '';
      // Deliberately NOT clearing the banner here. The local sender being
      // unreachable says nothing about the account, and flipping the banner
      // on every hiccup trains operators to ignore it.
    }
  }
  setInterval(refreshStatus, 5000);
  refreshStatus();

  // ---- account-inactive banner ------------------------------------
  //
  // Driven by /api/status, which the poll above already fetches every 5s —
  // no second polling loop and no extra request.

  const labBanner        = document.getElementById('lab-banner');
  const labBannerMessage = document.getElementById('lab-banner-message');
  const labBannerHold    = document.getElementById('lab-banner-hold');
  const labBannerRecheck = document.getElementById('lab-banner-recheck');

  const DEFAULT_INACTIVE_MSG = 'Your account is inactive. Please contact Achyu.';

  function renderLabBanner(s) {
    // Strictly === false. An absent field means an older engine without the
    // gate, which is not evidence of a problem — showing a scary banner on
    // no evidence is worse than showing none.
    if (!s || s.lab_active !== false) {
      labBanner.style.display = 'none';
      return;
    }

    labBanner.style.display = 'flex';
    labBannerMessage.textContent = s.lab_status_message || DEFAULT_INACTIVE_MSG;

    // What is actually piling up, so the operator can see nothing is lost.
    const held = [];
    if (s.pending_notifications > 0) {
      held.push(`${s.pending_notifications} notification${s.pending_notifications === 1 ? '' : 's'}`);
    }
    if (s.queue_depth > 0) {
      held.push(`${s.queue_depth} transfer${s.queue_depth === 1 ? '' : 's'}`);
    }
    labBannerHold.textContent = held.length
      ? `On hold: ${held.join(', ')}. Studies are still being received and stored.`
      : 'Studies are still being received and stored.';
  }

  labBannerRecheck.addEventListener('click', async () => {
    labBannerRecheck.disabled = true;
    const original = labBannerRecheck.textContent;
    labBannerRecheck.textContent = 'Checking…';
    try {
      // POST, not GET: this must force a check, or it does nothing visible
      // in the one moment it matters — just after reactivation.
      const v = await window.tarang.refreshLabStatus();
      if (v.active) {
        labBanner.style.display = 'none';
        toast('info', 'Account is active — held studies will resume shortly.');
      } else if (!v.known) {
        toast('error', 'Could not reach the account server. Will keep retrying.');
      } else {
        toast('error', v.message || DEFAULT_INACTIVE_MSG);
      }
    } catch (err) {
      toast('error', `Check failed: ${err.message}`);
    } finally {
      labBannerRecheck.disabled = false;
      labBannerRecheck.textContent = original;
      refreshStatus();
    }
  });

  // ---- worklist ---------------------------------------------------

  const worklistBody  = document.getElementById('worklist-body');
  const worklistEmpty = document.getElementById('worklist-empty');
  const worklistSearch = document.getElementById('worklist-search');
  const worklistStatus = document.getElementById('worklist-status');

  document.getElementById('worklist-refresh').addEventListener('click', refreshWorklist);
  worklistSearch.addEventListener('input', renderWorklist);
  worklistStatus.addEventListener('change', refreshWorklist);

  let allStudies = [];

  // Study-level progress cache — populated by pollUploads, consumed by renderWorklist.
  // Map<studyUID → { percent, instSent, instTotal, speedMBs }>
  const studyProgressCache = new Map();

  async function refreshWorklist() {
    try {
      const r = await window.tarang.worklist({
        status: worklistStatus.value,
        limit: 500,
      });
      allStudies = r.studies || [];
      renderWorklist();
      checkForSendingStudies(allStudies);
    } catch (err) {
      toast('error', `Worklist refresh failed: ${err.message}`);
    }
  }

  function renderWorklist() {
    const term = worklistSearch.value.trim().toLowerCase();
    const filtered = term
      ? allStudies.filter(s =>
          (s.patient_name || '').toLowerCase().includes(term) ||
          (s.patient_id || '').toLowerCase().includes(term) ||
          (s.accession_number || '').toLowerCase().includes(term) ||
          (s.study_instance_uid || '').includes(term)
        )
      : allStudies;

    if (filtered.length === 0) {
      worklistBody.innerHTML = '';
      worklistEmpty.style.display = '';
      return;
    }
    worklistEmpty.style.display = 'none';

    worklistBody.innerHTML = filtered.map(s => {
      const uid    = s.study_instance_uid;
      const active = s.status === 'sending' || s.status === 'transcoding';
      const prog   = studyProgressCache.get(uid);

      let statusCell = `<span class="status-dot" data-status="${escapeAttr(s.status || 'unknown')}"><span class="dot"></span>${escapeText(s.status || '—')}</span>`;

      if (active) {
        const isTranscoding  = s.status === 'transcoding';
        const pct            = prog ? prog.percent : 0;
        const instSent       = prog ? prog.instSent : 0;
        const instTotal      = prog ? prog.instTotal : 0;
        const speed          = (prog && prog.speedMBs > 0) ? `${prog.speedMBs.toFixed(1)} MB/s` : '';
        const fillAttr       = isTranscoding ? ' data-indeterminate="true"' : '';
        const fillStyle      = isTranscoding ? '' : `style="width:${pct}%"`;
        const instText       = (!isTranscoding && instTotal > 0) ? `${instSent}/${instTotal}` : '';

        statusCell += `
          <div class="row-progress" id="sprog-${safeId(uid)}">
            <div class="row-prog-track">
              <div class="row-prog-fill"${fillAttr} ${fillStyle}></div>
            </div>
            <div class="row-prog-meta">
              ${!isTranscoding ? `<span class="row-prog-pct">${pct}%</span>` : '<span class="row-prog-pct">—</span>'}
              ${instText ? `<span>${instText} files</span>` : ''}
              ${speed    ? `<span class="row-prog-speed">${speed}</span>` : ''}
            </div>
          </div>`;
      }

      const mods    = (s.modalities || []).join(', ');
      const subline = [escapeText(s.patient_id || ''), escapeText(mods)].filter(Boolean).join(' · ');

      return `
        <tr data-uid="${escapeAttr(uid)}" ${active ? 'data-active="true"' : ''}>
          <td>
            <div class="patient-cell">
              <span class="monogram">${escapeText(initials(s.patient_name))}</span>
              <span class="patient-text">
                <span class="patient-name">${escapeText(s.patient_name || '—')}</span>
                <span class="patient-sub mono">${subline || '—'}</span>
              </span>
            </div>
          </td>
          <td class="mono">${escapeText(s.accession_number || '—')}</td>
          <td class="mono">${formatDate(s.study_date)}<br><span style="color:var(--text-mute)">${formatTime(s.study_time)}</span></td>
          <td class="mono">${s.series_count || 0} / ${s.instance_count || 0}</td>
          <td>${statusCell}</td>
          <td class="uid">${truncateUID(uid)}</td>
        </tr>`;
    }).join('');

    // Bind row clicks for the detail drawer.
    worklistBody.querySelectorAll('tr').forEach(tr => {
      tr.addEventListener('click', () => openDrawer(tr.dataset.uid));
    });
  }

  // Update only the progress elements in existing rows — avoids full re-render flicker.
  function updateWorklistProgressBars() {
    for (const [uid, prog] of studyProgressCache.entries()) {
      const el = document.getElementById(`sprog-${safeId(uid)}`);
      if (!el) continue;
      const fill = el.querySelector('.row-prog-fill');
      const pct  = el.querySelector('.row-prog-pct');
      const meta = el.querySelectorAll('.row-prog-meta span');
      if (fill && !fill.dataset.indeterminate) {
        fill.style.width = prog.percent + '%';
      }
      if (pct) pct.textContent = prog.percent + '%';
      // meta[1] = inst count, meta[2] = speed
      if (meta[1] && prog.instTotal > 0) meta[1].textContent = `${prog.instSent}/${prog.instTotal} files`;
      if (meta[2] && prog.speedMBs > 0)  meta[2].textContent = `${prog.speedMBs.toFixed(1)} MB/s`;
    }
  }

  // Auto-refresh while worklist is visible. Pause on hidden tab.
  setInterval(() => {
    if (activeTab === 'worklist' && !document.hidden) {
      refreshWorklist();
    }
  }, 3000);

  // ---- detail drawer ---------------------------------------------

  const drawer       = document.getElementById('drawer');
  const drawerTitle  = document.getElementById('drawer-title');
  const drawerMeta   = document.getElementById('drawer-meta');
  const drawerSeries = document.getElementById('drawer-series');
  document.getElementById('drawer-close').addEventListener('click', closeDrawer);
  const progressTimers = new Map();

  let drawerUID = null;

  async function openDrawer(uid) {
    drawerUID = uid;
    drawer.classList.add('open');
    drawerMeta.innerHTML = '<dt>Loading…</dt><dd></dd>';
    drawerSeries.innerHTML = '';
    try {
      const r = await window.tarang.studyDetail(uid);
      drawerTitle.textContent = r.study.patient_name || '(no patient name)';
      drawerMeta.innerHTML = `
        <dt>Study UID</dt><dd class="uid">${escapeText(r.study.study_instance_uid)}</dd>
        <dt>Patient ID</dt><dd class="mono">${escapeText(r.study.patient_id || '—')}</dd>
        <dt>Accession</dt><dd class="mono">${escapeText(r.study.accession_number || '—')}</dd>
        <dt>Description</dt><dd>${escapeText(r.study.study_description || '—')}</dd>
        <dt>Date</dt><dd class="mono">${formatDate(r.study.study_date)} ${formatTime(r.study.study_time)}</dd>
        <dt>Status</dt><dd><span class="pill" data-status="${r.study.status}">${r.study.status}</span></dd>
        <dt>Modalities</dt><dd>${(r.study.modalities || []).join(', ')}</dd>
        <dt>Total size</dt><dd class="mono">${formatSize(r.study.total_size)}</dd>
        <dt>Received</dt><dd class="mono">${formatTime8601(r.study.first_received_at)}</dd>
        ${r.study.delivered_at ? `<dt>Delivered</dt><dd class="mono">${formatTime8601(r.study.delivered_at)}</dd>` : ''}
      `;
      drawerSeries.innerHTML = (r.series || []).map(ser => `
        <div class="series">
          <div class="num">${escapeText(ser.series_number || '—')}</div>
          <div class="desc">
            ${escapeText(ser.modality || '—')} · ${escapeText(ser.series_description || '(no description)')}
            <small>${ser.instance_count} instances · ${formatSize(ser.total_size)}</small>
            ${ser.status === 'sending' ? `<div class="series-progress" id="series-progress-${safeId(ser.series_instance_uid)}">sending...</div>` : ''}
          </div>
          <div><span class="pill" data-status="${ser.status}">${ser.status || '—'}</span></div>
        </div>
      `).join('');
      (r.series || []).forEach((ser) => {
        if (ser.status === 'sending') {
          startSeriesProgressPoll(ser.series_instance_uid);
        }
      });
    } catch (err) {
      drawerMeta.innerHTML = `<dt>Error</dt><dd>${escapeText(err.message)}</dd>`;
    }
  }

  function closeDrawer() {
    for (const timer of progressTimers.values()) {
      clearInterval(timer);
    }
    progressTimers.clear();
    drawer.classList.remove('open');
    drawerUID = null;
  }

  function startSeriesProgressPoll(seriesUID) {
    if (!seriesUID || progressTimers.has(seriesUID)) return;
    const elId = `series-progress-${safeId(seriesUID)}`;
    const tick = async () => {
      try {
        const p = await window.tarang.transferProgress(seriesUID);
        const el = document.getElementById(elId);
        if (!el) return;
        const percent = Number.isFinite(p.percent) ? p.percent : 0;
        el.innerHTML = `<div class="prog-row"><div class="prog-bar"><span style="width:${percent}%"></span></div><small>${percent}% · ${p.instances_sent || 0}/${p.total_instances || 0} · ETA ${p.eta_s || 0}s</small></div>`;
        if (p.status === 'complete' || p.status === 'failed') {
          clearInterval(progressTimers.get(seriesUID));
          progressTimers.delete(seriesUID);
        }
      } catch (_) {
        // 404 can happen briefly while worker just started.
      }
    };

    tick();
    progressTimers.set(seriesUID, setInterval(tick, 1000));
  }

  document.getElementById('drawer-retry').addEventListener('click', async () => {
    if (!drawerUID) return;
    try {
      const r = await window.tarang.retryStudy(drawerUID);
      toast('info', `Re-queued ${r.series_requeued} of ${r.series_total} series.`);
      setTimeout(refreshWorklist, 200);
    } catch (err) {
      toast('error', `Retry failed: ${err.message}`);
    }
  });

  document.getElementById('drawer-delete').addEventListener('click', async () => {
    if (!drawerUID) return;
    if (!confirm('Delete this study locally? Already-delivered instances are NOT removed from the central PACS.')) return;
    try {
      await window.tarang.deleteStudy(drawerUID);
      toast('info', 'Study deleted locally.');
      closeDrawer();
      refreshWorklist();
    } catch (err) {
      toast('error', `Delete failed: ${err.message}`);
    }
  });

  // ---- active uploads tracking ------------------------------------
  // Polls all in-flight series purely to aggregate per-study progress and
  // feed the single progress bar shown in each worklist row. There is no
  // separate uploads panel — the worklist row bar is the only progress UI.

  let uploadsPollTimer = null;
  // Map<seriesUID → { studyUID, patientName, modality, seriesDesc, progress }>
  const activeUploads = new Map();

  function checkForSendingStudies(studies) {
    if (studies.some(s => s.status === 'sending' || s.status === 'transcoding') && !uploadsPollTimer) {
      startUploadsPoll();
    }
  }

  async function pollUploads() {
    try {
      const r = await window.tarang.worklist({ status: 'sending', limit: 100 });
      const sendingStudies = r.studies || [];

      // Discover all series currently sending
      const liveSeries = new Set();
      for (const study of sendingStudies) {
        try {
          const detail = await window.tarang.studyDetail(study.study_instance_uid);
          for (const ser of (detail.series || [])) {
            if (ser.status !== 'sending') continue;
            liveSeries.add(ser.series_instance_uid);
            if (!activeUploads.has(ser.series_instance_uid)) {
              activeUploads.set(ser.series_instance_uid, {
                studyUID:   study.study_instance_uid,
                patientName: study.patient_name || '(unknown)',
                modality:   ser.modality || '—',
                seriesDesc: ser.series_description || '',
                progress:   null,
              });
            }
          }
        } catch (_) {}
      }

      // Fetch progress for every tracked series; evict finished ones
      for (const [uid, info] of activeUploads.entries()) {
        try {
          info.progress = await window.tarang.transferProgress(uid);
        } catch (_) { /* 404 means transfer ended */ }

        // Drop series that are no longer sending and have a terminal status
        if (!liveSeries.has(uid)) {
          const st = info.progress?.status;
          if (!st || st === 'complete' || st === 'failed') {
            activeUploads.delete(uid);
          }
        }
      }

      // Aggregate series progress into study-level totals for the worklist bars.
      const studyAgg = new Map();
      for (const [, info] of activeUploads.entries()) {
        const p = info.progress;
        if (!p || !info.studyUID) continue;
        if (!studyAgg.has(info.studyUID)) {
          studyAgg.set(info.studyUID, { bytes: 0, sent: 0, instTotal: 0, instSent: 0, elapsed: 0 });
        }
        const a = studyAgg.get(info.studyUID);
        a.bytes    += p.total_bytes     || 0;
        a.sent     += p.bytes_sent      || 0;
        a.instTotal+= p.total_instances || 0;
        a.instSent += p.instances_sent  || 0;
        a.elapsed   = Math.max(a.elapsed, p.elapsed_s || 0);
      }
      for (const [sUID, a] of studyAgg.entries()) {
        const speedMBs = (a.elapsed > 0 && a.sent > 0) ? (a.sent / (a.elapsed * 1024 * 1024)) : 0;
        studyProgressCache.set(sUID, {
          percent:  a.bytes > 0 ? Math.min(100, Math.round(a.sent * 100 / a.bytes)) : 0,
          instSent: a.instSent,
          instTotal:a.instTotal,
          speedMBs,
        });
      }
      // Push updates into worklist rows without re-rendering.
      updateWorklistProgressBars();

      if (activeUploads.size === 0) {
        stopUploadsPoll();
      }
    } catch (_) {}
  }

  function startUploadsPoll() {
    if (uploadsPollTimer) return;
    pollUploads();
    uploadsPollTimer = setInterval(pollUploads, 1500);
  }

  function stopUploadsPoll() {
    if (uploadsPollTimer) {
      clearInterval(uploadsPollTimer);
      uploadsPollTimer = null;
    }
  }

  // ---- settings ---------------------------------------------------

  const MODALITIES = ['CT', 'MR', 'CR', 'DX', 'PT', 'NM', 'US', 'MG'];

  // Wire range sliders to display their live value.
  function wireRangeVal(inputId, valId) {
    const input = document.getElementById(inputId);
    const val   = document.getElementById(valId);
    if (!input || !val) return;
    input.addEventListener('input', () => { val.textContent = input.value; });
  }
  wireRangeVal('cfg-compression-default-rate', 'cfg-compression-default-rate-val');
  MODALITIES.forEach(m => {
    wireRangeVal(`cfg-q-${m}`, `cfg-q-${m}-val`);
    const toggle = document.getElementById(`cfg-en-${m}`);
    const slider = document.getElementById(`cfg-q-${m}`);
    const valEl  = document.getElementById(`cfg-q-${m}-val`);
    if (toggle && slider) {
      toggle.addEventListener('change', () => {
        slider.disabled = !toggle.checked;
        if (valEl) valEl.style.opacity = toggle.checked ? '' : '0.35';
      });
    }
  });

  let pristineSettings = null;

  async function loadSettings() {
    try {
      const s = await window.tarang.status();
      const cfg = await window.tarang.getConfig();
      document.getElementById('cfg-lab-id').value     = cfg.lab_id || s.lab_id || '';
      document.getElementById('cfg-org-id').value     = cfg.org_id || s.org_id || '';
      document.getElementById('cfg-hostname').value   = s.hostname || '';

      // PACS Receiver — show current values, leave password blank.
      const peer = cfg.peer || {};
      document.getElementById('cfg-peer-url').value  = peer.url      || '';
      document.getElementById('cfg-peer-user').value = peer.username  || '';
      document.getElementById('cfg-peer-pass').value = '';
      document.getElementById('cfg-dicom-port').value = cfg.dicom?.port || s.dicom_port || 8899;
      document.getElementById('cfg-aet').value        = cfg.dicom?.aet || 'ACHYU';
      document.getElementById('cfg-data-dir').value   = cfg.storage?.data_dir || '';

      // Account status, read-only. lab_active is absent on an older engine,
      // which is reported as unknown rather than as a problem.
      const labStatusField = document.getElementById('cfg-lab-status');
      if (s.lab_gate_enabled === false) {
        labStatusField.value = 'Not checked (activation gate disabled)';
      } else if (s.lab_active === false) {
        labStatusField.value = `Inactive${s.lab_status ? ' — ' + s.lab_status : ''}`;
      } else if (s.lab_active === true) {
        labStatusField.value = s.lab_status_known === false
          ? 'Active (server unreachable, using last known status)'
          : 'Active';
      } else {
        labStatusField.value = 'Unknown';
      }

      // Secure DICOM is read-only here: it shares the plaintext port and needs
      // no per-site tuning, so surfacing a toggle would only invite somebody
      // to switch off a feature that costs nothing.
      const tls = cfg.dicom?.tls || {};
      document.getElementById('cfg-dicom-tls').value = tls.enabled === false
        ? 'Disabled — plaintext DICOM only'
        : `Enabled on port ${cfg.dicom?.port || s.dicom_port || 8899} (TLS and plaintext)`;

      const stable    = cfg.dicom?.stable_age_seconds || 15;
      const retention = cfg.storage?.retention_hours || 24;
      document.getElementById('cfg-stable-age').value = stable;
      document.getElementById('cfg-retention').value  = retention;
      footStable.textContent = `stable age ${stable}s`;

      // Compression settings.
      const comp = cfg.compression || {};
      document.getElementById('cfg-compression-enabled').checked = Boolean(comp.enabled);
      document.getElementById('cfg-compression-skip').checked    = comp.skip_already_compressed !== false;
      const defR = comp.default_rate || 10;
      document.getElementById('cfg-compression-default-rate').value = defR;
      document.getElementById('cfg-compression-default-rate-val').textContent = defR;
      const mq = comp.modality_rate || {};
      MODALITIES.forEach(m => {
        const rawR = mq[m]; // undefined = not in map
        const enabled = rawR !== undefined && rawR > 0;
        const displayR = enabled ? rawR : 10;
        const toggle = document.getElementById(`cfg-en-${m}`);
        const el     = document.getElementById(`cfg-q-${m}`);
        const vl     = document.getElementById(`cfg-q-${m}-val`);
        if (toggle) toggle.checked = enabled;
        if (el) { el.value = displayR; el.disabled = !enabled; }
        if (vl) { vl.textContent = displayR; vl.style.opacity = enabled ? '' : '0.35'; }
      });

      // Lossless is a separate mode, and like the lossy mode it now ships
      // OFF — so the checkbox reads `=== true`, not `!== false`: a config
      // with no lossless key at all must render as unchecked, matching what
      // the engine will actually do. Its per-modality map still records
      // EXCEPTIONS only (an absent modality is enabled), which is why the
      // grid below keeps `!== false` — that map decides which modalities
      // transcode once the mode itself is switched on.
      const ll = comp.lossless || {};
      document.getElementById('cfg-lossless-enabled').checked = ll.enabled === true;
      document.getElementById('cfg-lossless-skip').checked    = ll.skip_already_compressed !== false;
      const llMap = ll.modality_enabled || {};
      MODALITIES.forEach(m => {
        const box = document.getElementById(`cfg-ll-${m}`);
        if (box) box.checked = llMap[m] !== false;
      });
      syncLosslessEnabledState();

      const res = cfg.resilience || {};
      document.getElementById('cfg-resilience-network-retry').checked      = res.network_retry !== false;
      document.getElementById('cfg-resilience-instance-checkpoint').checked = res.instance_checkpoint !== false;
      document.getElementById('cfg-resilience-watchdog').checked            = res.connectivity_watchdog !== false;

      pristineSettings = { stable, retention, comp: JSON.parse(JSON.stringify(comp)), res: JSON.parse(JSON.stringify(res)) };
      document.getElementById('settings-status').textContent = '';
    } catch (err) {
      toast('error', `Settings load failed: ${err.message}`);
    }
  }

  // Grey out the per-modality lossless grid while the mode is off, for the
  // same reason the lossy ratios grey out: nobody should be tuning a control
  // that currently does nothing.
  function syncLosslessEnabledState() {
    const on = document.getElementById('cfg-lossless-enabled').checked;
    const grid = document.getElementById('lossless-grid');
    if (grid) grid.style.opacity = on ? '' : '0.35';
    MODALITIES.forEach(m => {
      const box = document.getElementById(`cfg-ll-${m}`);
      if (box) box.disabled = !on;
    });
    const skip = document.getElementById('cfg-lossless-skip');
    if (skip) skip.disabled = !on;
  }
  document.getElementById('cfg-lossless-enabled')
    .addEventListener('change', syncLosslessEnabledState);

  document.getElementById('settings-save').addEventListener('click', async () => {
    const stable    = Number(document.getElementById('cfg-stable-age').value);
    const retention = Number(document.getElementById('cfg-retention').value);
    if (!Number.isFinite(stable) || stable < 5 || stable > 300) {
      toast('error', 'Stable age must be between 5 and 300 seconds.');
      return;
    }
    if (!Number.isFinite(retention) || retention < 1 || retention > 720) {
      toast('error', 'Retention must be between 1 and 720 hours.');
      return;
    }

    // Gather compression settings.
    //
    // Every modality is written explicitly, for both maps. PATCH /api/config
    // deep-merges maps rather than replacing them, so an omitted key keeps its
    // previous value — a "record only the exceptions" payload could never
    // clear an exception once it had been set.
    const defR = Number(document.getElementById('cfg-compression-default-rate').value);
    const modalityRate = {};
    const losslessEnabled = {};
    MODALITIES.forEach(m => {
      const toggle = document.getElementById(`cfg-en-${m}`);
      const el     = document.getElementById(`cfg-q-${m}`);
      // rate=0 means "disabled" for this modality on the Go side
      modalityRate[m] = (toggle && toggle.checked && el) ? Number(el.value) : 0;

      const llBox = document.getElementById(`cfg-ll-${m}`);
      losslessEnabled[m] = llBox ? llBox.checked : true;
    });

    const aet = document.getElementById('cfg-aet').value.trim();


    // Peer settings — only include password if the user typed one.
    const peerUrl  = document.getElementById('cfg-peer-url').value.trim();
    const peerUser = document.getElementById('cfg-peer-user').value.trim();
    const peerPass = document.getElementById('cfg-peer-pass').value;
    if (peerUrl && !isValidUrl(peerUrl)) {
      toast('error', 'Receiver URL must be a valid http:// or https:// URL.');
      return;
    }
    const peerPatch = {};
    if (peerUrl)  peerPatch.url      = peerUrl;
    if (peerUser) peerPatch.username = peerUser;
    if (peerPass) peerPatch.password = peerPass;

    // DICOM port — gather here so it's in scope when building patch below.
    const dicomPortRaw = parseInt(document.getElementById('cfg-dicom-port').value, 10);
    const dicomPortValid = Number.isFinite(dicomPortRaw) && dicomPortRaw >= 1024 && dicomPortRaw <= 65535;

    try {
      const patch = {
        dicom: {
          stable_age_seconds: stable,
          ...(dicomPortValid ? { port: dicomPortRaw } : {}),
          ...(aet ? { aet } : {}),
        },
        storage: { retention_hours: retention },
        compression: {
          enabled:                 document.getElementById('cfg-compression-enabled').checked,
          skip_already_compressed: document.getElementById('cfg-compression-skip').checked,
          default_rate:            defR,
          modality_rate:           modalityRate,
          lossless: {
            enabled:                 document.getElementById('cfg-lossless-enabled').checked,
            skip_already_compressed: document.getElementById('cfg-lossless-skip').checked,
            modality_enabled:        losslessEnabled,
          },
        },
        resilience: {
          network_retry:         document.getElementById('cfg-resilience-network-retry').checked,
          instance_checkpoint:   document.getElementById('cfg-resilience-instance-checkpoint').checked,
          connectivity_watchdog: document.getElementById('cfg-resilience-watchdog').checked,
        },
      };
      if (Object.keys(peerPatch).length > 0) patch.peer = peerPatch;

      await window.tarang.patchConfig(patch);
      // Mirror into the Electron-owned config too, so the value survives the
      // next bootstrap and the DICOM chip re-renders with it.
      if (dicomPortValid || aet) {
        await window.tarangAPI.updateConfig({
          ...(dicomPortValid ? { dicom_port: dicomPortRaw } : {}),
          ...(aet ? { aet } : {}),
        });
      }
      if (aet) refreshDicomChip(aet, dicomPortValid ? dicomPortRaw : undefined);
      pristineSettings = { stable, retention };
      footStable.textContent = `stable age ${stable}s`;
      document.getElementById('cfg-peer-pass').value = ''; // clear after save
      document.getElementById('settings-status').textContent = 'saved';
      toast('info', 'Settings saved. Port changes require a sender restart.');
    } catch (err) {
      toast('error', `Save failed: ${err.message}`);
    }
  });

  document.getElementById('settings-revert').addEventListener('click', () => {
    if (!pristineSettings) return;
    document.getElementById('cfg-stable-age').value = pristineSettings.stable;
    document.getElementById('cfg-retention').value  = pristineSettings.retention;
    // Reload full settings to revert compression sliders too.
    loadSettings();
    document.getElementById('settings-status').textContent = '';
  });

  // ---- connection -------------------------------------------------

  async function refreshConnection() {
    try {
      const s = await window.tarang.status();
      document.getElementById('kpi-peer-name').textContent = s.peer_name || '—';
      document.getElementById('kpi-peer-url').textContent  = s.peer_url || '—';
      document.getElementById('kpi-queue').textContent     = s.queue_depth ?? '—';
      document.getElementById('kpi-uptime').textContent    = humanDuration(s.uptime_s);
      document.getElementById('kpi-version').textContent   = `v${s.version}`;
    } catch (err) {
      toast('error', `Status fetch failed: ${err.message}`);
    }

    // Throughput: count delivered studies in the last 24h.
    try {
      const r = await window.tarang.worklist({ status: 'delivered', limit: 1000 });
      const cutoff = Date.now() - 24 * 3600 * 1000;
      const recent = (r.studies || []).filter(s => {
        const t = s.delivered_at ? Date.parse(s.delivered_at) : 0;
        return t >= cutoff;
      });
      document.getElementById('kpi-throughput').textContent = recent.length;
    } catch (err) {
      // non-fatal
    }
  }

  document.getElementById('conn-ping').addEventListener('click', async () => {
    const result = document.getElementById('conn-result');
    result.textContent = 'pinging…';
    const start = performance.now();
    try {
      await window.tarang.health();
      const ms = Math.round(performance.now() - start);
      result.textContent = `local sender OK (${ms}ms). Receiver reachability is verified by every transfer attempt; check Worklist for failed studies.`;
    } catch (err) {
      result.textContent = `local sender unreachable: ${err.message}`;
    }
  });

  // ---- logs -------------------------------------------------------

  let logsPaused = false;
  document.getElementById('logs-pause').addEventListener('click', (e) => {
    logsPaused = !logsPaused;
    e.target.textContent = logsPaused ? 'Resume auto-refresh' : 'Pause auto-refresh';
  });
  document.getElementById('logs-refresh').addEventListener('click', refreshLogs);
  document.getElementById('logs-level').addEventListener('change', refreshLogs);

  async function refreshLogs() {
    try {
      // The renderer can't read disk; main process exposes a tail helper
      // via window.tarangAPI.readRecentLogs(limit).
      const lines = await window.tarangAPI.readRecentLogs({ limit: 500 });
      const filter = document.getElementById('logs-level').value;
      const filtered = filter
        ? lines.filter(l => l.includes(`level=${filter}`))
        : lines;
      const body = document.getElementById('logs-body');
      body.innerHTML = filtered.map(l => {
        let cls = 'info';
        if (l.includes('level=ERROR')) cls = 'error';
        else if (l.includes('level=WARN')) cls = 'warn';
        return `<div class="line ${cls}">${escapeText(l)}</div>`;
      }).join('');
      body.scrollTop = body.scrollHeight;
      document.getElementById('logs-status').textContent = `${filtered.length} lines`;
    } catch (err) {
      document.getElementById('logs-status').textContent = err.message;
    }
  }

  setInterval(() => {
    if (activeTab === 'logs' && !logsPaused && !document.hidden) refreshLogs();
  }, 2000);

  // ---- helpers ----------------------------------------------------

  function isValidUrl(s) {
    try { const u = new URL(s); return u.protocol === 'http:' || u.protocol === 'https:'; }
    catch (_) { return false; }
  }

  function escapeText(s) {
    return String(s ?? '').replace(/[&<>"']/g, c => ({
      '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
    }[c]));
  }
  function escapeAttr(s) { return escapeText(s); }
  function safeId(s) {
    return String(s ?? '').replace(/[^a-zA-Z0-9_-]/g, '_');
  }

  // Patient monogram initials. DICOM names are "Last^First^…"; some sources
  // use "Last, First". Take the first letter of the first two components.
  function initials(name) {
    const parts = String(name || '')
      .split(/[\^,]/)
      .map(p => p.trim())
      .filter(Boolean);
    if (parts.length === 0) return '—';
    const last  = parts[0] || '';
    const first = parts[1] || '';
    const out = ((first[0] || '') + (last[0] || '')).toUpperCase();
    return out || (last[0] || '—').toUpperCase();
  }

  function formatDate(d) {
    if (!d || d.length !== 8) return '';
    return `${d.slice(0,4)}-${d.slice(4,6)}-${d.slice(6,8)}`;
  }
  function formatTime(t) {
    if (!t) return '';
    const s = String(t);
    if (s.length < 4) return s;
    return `${s.slice(0,2)}:${s.slice(2,4)}`;
  }
  function formatTime8601(t) {
    if (!t) return '';
    return new Date(t).toLocaleString();
  }
  function formatSize(n) {
    if (!n) return '0 B';
    if (n < 1024) return `${n} B`;
    if (n < 1024*1024) return `${(n/1024).toFixed(1)} KB`;
    if (n < 1024*1024*1024) return `${(n/1024/1024).toFixed(1)} MB`;
    return `${(n/1024/1024/1024).toFixed(2)} GB`;
  }
  function truncateUID(uid) {
    if (!uid) return '';
    if (uid.length <= 28) return uid;
    return uid.slice(0, 14) + '…' + uid.slice(-12);
  }
  function humanDuration(secs) {
    if (!secs) return '—';
    if (secs < 60) return `${secs}s`;
    if (secs < 3600) return `${Math.floor(secs/60)}m`;
    if (secs < 86400) return `${Math.floor(secs/3600)}h`;
    return `${Math.floor(secs/86400)}d`;
  }

  function toast(level, msg) {
    const host = document.getElementById('toast-host');
    const el = document.createElement('div');
    el.className = `toast ${level}`;
    el.textContent = msg;
    host.appendChild(el);
    setTimeout(() => el.remove(), 4000);
  }
}
