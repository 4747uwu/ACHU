# BharatPACS — Sender Error-Log Ingestion API

Context to hand to Claude Code **in the PACS backend repo** so it can build the
endpoint that receives error reports from the Tarang sender desktop app.

## What the sender does

Whenever an **error-level log** occurs, the sender (`tarang-sender`):

1. Saves the error to disk (`<data_dir>/error-reports/<id>.json`) so it survives
   crashes / offline periods.
2. POSTs it to `POST https://pacs.bharatpacs.com/api/sender/error-logs`.
3. Retries spooled reports on a 30s ticker and on every app start until the
   server returns **2xx**, then deletes the local file.

So the endpoint **must be idempotent**: the same `id` may arrive more than once
(after a timeout/retry). Dedupe by upserting on `id`.

Auth: currently sent **without** a token (open endpoint, same as the existing
notifier). If you set `error_reporting.api_key` in the sender config, it arrives
as `Authorization: Bearer <key>` — wire that check only if you populate the key.

## Request body (exact contract)

```json
{
  "id": "00001718800000000000000-000001",
  "source": "tarang-sender",
  "appVersion": "0.1.0",
  "level": "ERROR",
  "message": "NOTIFIER POST failed",
  "attrs": {
    "study_uid": "1.2.840...",
    "http_status": 502,
    "err": "backend returned HTTP 502"
  },
  "occurredAt": "2026-06-19T09:14:32.123Z",
  "device": {
    "hostname": "RECEPTION-PC",
    "os": "windows",
    "arch": "amd64",
    "ips": ["192.168.1.38"],
    "macs": ["a4:bb:6d:11:22:33"]
  },
  "lab": {
    "labId": "LAB123",
    "orgId": "ORG456",
    "peerName": "BHARATPACS",
    "peerUrl": "http://206.189.133.52:8042"
  }
}
```

- `id` — client-generated, sortable, unique. **Upsert on this** to dedupe retries.
- `attrs` — free-form key/value bag (varies per error). Store as a flexible object.
- `occurredAt` — ISO-8601 UTC, when the error happened on the device.
- `device` / `lab` — identify the source machine and tenant.

Respond `200`/`201` with `{ "ok": true, "id": "<id>" }` on success. Any non-2xx
makes the sender keep the report and retry later.

---

## 1) Mongoose model — `models/SenderErrorLog.js`

```js
const mongoose = require('mongoose');

const DeviceSchema = new mongoose.Schema(
  {
    hostname: String,
    os: String,
    arch: String,
    ips: [String],
    macs: [String],
  },
  { _id: false }
);

const LabSchema = new mongoose.Schema(
  {
    labId: { type: String, index: true },
    orgId: { type: String, index: true },
    peerName: String,
    peerUrl: String,
  },
  { _id: false }
);

const SenderErrorLogSchema = new mongoose.Schema(
  {
    // Client-generated id. Unique so retries upsert instead of duplicating.
    clientId: { type: String, required: true, unique: true, index: true },
    source: { type: String, default: 'tarang-sender' },
    appVersion: String,
    level: { type: String, index: true },
    message: { type: String, required: true },
    // Free-form structured context — shape varies per error.
    attrs: { type: mongoose.Schema.Types.Mixed, default: {} },
    occurredAt: { type: Date, index: true },
    device: DeviceSchema,
    lab: LabSchema,
  },
  { timestamps: true } // createdAt = when the server received it
);

// Common dashboard query: latest errors for a given lab.
SenderErrorLogSchema.index({ 'lab.labId': 1, occurredAt: -1 });

module.exports = mongoose.model('SenderErrorLog', SenderErrorLogSchema);
```

> Note: the payload field is `id`; we store it as `clientId` to avoid clashing
> with Mongo's `_id`. The controller maps it.

## 2) Controller — `controllers/senderErrorLogController.js`

```js
const SenderErrorLog = require('../models/SenderErrorLog');

// POST /api/sender/error-logs
exports.createSenderErrorLog = async (req, res) => {
  try {
    const b = req.body || {};
    if (!b.id || !b.message) {
      return res.status(400).json({ ok: false, error: 'id and message are required' });
    }

    const doc = {
      clientId: b.id,
      source: b.source || 'tarang-sender',
      appVersion: b.appVersion,
      level: b.level,
      message: b.message,
      attrs: b.attrs || {},
      occurredAt: b.occurredAt ? new Date(b.occurredAt) : new Date(),
      device: b.device || {},
      lab: b.lab || {},
    };

    // Idempotent: the sender retries the same id until it gets a 2xx.
    const saved = await SenderErrorLog.findOneAndUpdate(
      { clientId: doc.clientId },
      { $set: doc },
      { upsert: true, new: true, setDefaultsOnInsert: true }
    );

    return res.status(201).json({ ok: true, id: saved.clientId });
  } catch (err) {
    // Duplicate-key race on concurrent retries → treat as success.
    if (err && err.code === 11000) {
      return res.status(200).json({ ok: true, id: req.body.id });
    }
    console.error('[senderErrorLog] save failed:', err);
    return res.status(500).json({ ok: false, error: 'internal error' });
  }
};

// GET /api/sender/error-logs?labId=&level=&limit=  (for an admin dashboard)
exports.listSenderErrorLogs = async (req, res) => {
  try {
    const { labId, orgId, level } = req.query;
    const limit = Math.min(parseInt(req.query.limit, 10) || 100, 1000);
    const filter = {};
    if (labId) filter['lab.labId'] = labId;
    if (orgId) filter['lab.orgId'] = orgId;
    if (level) filter.level = level;

    const items = await SenderErrorLog.find(filter)
      .sort({ occurredAt: -1 })
      .limit(limit)
      .lean();

    return res.json({ ok: true, count: items.length, items });
  } catch (err) {
    console.error('[senderErrorLog] list failed:', err);
    return res.status(500).json({ ok: false, error: 'internal error' });
  }
};
```

## 3) Route — `routes/senderErrorLog.js`

```js
const express = require('express');
const router = express.Router();
const ctrl = require('../controllers/senderErrorLogController');

// Public ingest (no auth today; sender sends no token by default).
// If you set error_reporting.api_key on the sender, add a bearer check here.
router.post('/error-logs', ctrl.createSenderErrorLog);

// Protect the read side with your existing admin auth middleware.
router.get('/error-logs', /* requireAdmin, */ ctrl.listSenderErrorLogs);

module.exports = router;
```

## 4) Server registration — `app.js` / `server.js`

```js
const senderErrorLogRoutes = require('./routes/senderErrorLog');

// Mount so the full path is /api/sender/error-logs
app.use('/api/sender', senderErrorLogRoutes);
```

> Make sure `express.json()` is registered **before** this route, and the body
> limit is large enough for the `attrs` object (the default 100kb is plenty).

---

## Endpoint summary

| Method | Path                        | Auth         | Purpose                          |
|--------|-----------------------------|--------------|----------------------------------|
| POST   | `/api/sender/error-logs`    | none (open)  | Sender pushes one error report   |
| GET    | `/api/sender/error-logs`    | admin only   | Dashboard lists recent errors    |

If you later want a bearer check on POST, set `error_reporting.api_key` in the
sender config (`%APPDATA%/.../config.json`) and validate
`Authorization: Bearer <key>` in the route.
