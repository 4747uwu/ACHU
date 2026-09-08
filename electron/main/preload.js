// electron/main/preload.js — runs in an isolated context before the
// renderer loads. Exposes a tightly-scoped surface on window.tarangAPI so
// the renderer has just what it needs and nothing more.
//
// Pattern: tarangAPI.ready is a Promise that resolves to the bootstrap
// payload (token, ports, AET, paths). All renderer code that needs those
// values should `await window.tarangAPI.ready` before its first request.

const { contextBridge, ipcRenderer } = require('electron');

const bootstrapPromise = ipcRenderer.invoke('tarang:get-bootstrap');

contextBridge.exposeInMainWorld('tarangAPI', {
  // Awaitable bootstrap. Resolves to the same object the main process
  // built in bootstrapSender(): { token, httpPort, dicomPort, aet,
  // dataDir, stableAgeSeconds, retentionHours }.
  ready: bootstrapPromise,

  // updateConfig: patch fields the user is allowed to edit from Settings.
  updateConfig: (patch) => ipcRenderer.invoke('tarang:update-config', patch),

  // Auth/session helpers used by login and app shell.
  login: (credentials) => ipcRenderer.invoke('tarang:login', credentials),
  logout: () => ipcRenderer.invoke('tarang:logout'),
  isAuthenticated: () => ipcRenderer.invoke('tarang:is-authenticated'),
  getSession: () => ipcRenderer.invoke('tarang:get-session'),

  // readRecentLogs: tail today's log file from the sender's data dir.
  readRecentLogs: (opts) => ipcRenderer.invoke('tarang:read-recent-logs', opts),
});
