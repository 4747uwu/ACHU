#!/usr/bin/env node
//
// Tests for the receiver-selection logic in main.js.
//
//   node electron/main/peer-selection.test.js
//
// main.js cannot be require()d — it calls electron APIs at load — so the pure
// functions are extracted by source range and evaluated on their own. That is
// deliberate: these rules decide whether an operator's manual pin survives a
// restart or a re-login, and every one of them has a failure mode that is
// invisible until a site quietly starts sending to the wrong receiver.

const fs = require('fs');
const path = require('path');

const src = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');

function slice(startMarker, endMarker) {
  const a = src.indexOf(startMarker);
  const b = src.indexOf(endMarker, a);
  if (a < 0 || b < 0) {
    throw new Error(`could not extract ${startMarker} .. ${endMarker} from main.js`);
  }
  return src.slice(a, b);
}

// Everything from the receiver constants to the end of normalizePeer.
const code = slice('const ROUTER1 =', 'function readSession(');
const sandbox = new Function(`${code}
  return { ROUTER1, ROUTER2, canonicalPeerUrl, isKnownReceiver, getDefaultPeer, normalizePeer };
`)();

const { ROUTER1, ROUTER2, canonicalPeerUrl, isKnownReceiver, getDefaultPeer, normalizePeer } = sandbox;

let failures = 0;
function check(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g === w) {
    console.log(`  ok   ${name}`);
  } else {
    console.log(`  FAIL ${name}\n         got  ${g}\n         want ${w}`);
    failures++;
  }
}

console.log('canonicalPeerUrl');
// A receiver written with the wrong scheme, an extra slash or odd case must
// collapse onto the one URL that actually works — this is what migrates old
// installs on their next start.
check('router2 http -> https', canonicalPeerUrl('http://achyurouter.xcentic.com'), ROUTER2);
check('router2 trailing slash', canonicalPeerUrl('https://achyurouter.xcentic.com/'), ROUTER2);
check('router2 mixed case', canonicalPeerUrl('HTTPS://AchyuRouter.Xcentic.CoM'), ROUTER2);
check('router1 canonical', canonicalPeerUrl('http://router.achyutrs.com'), ROUTER1);
check('unknown host untouched', canonicalPeerUrl('https://my.own.server'), 'https://my.own.server');
check('blank stays blank', canonicalPeerUrl(''), '');

console.log('isKnownReceiver');
check('router1', isKnownReceiver('http://router.achyutrs.com/'), true);
check('router2', isKnownReceiver(ROUTER2), true);
check('stranger', isKnownReceiver('https://my.own.server'), false);

console.log('normalizePeer');
// Fresh install: automatic, both receivers, preferred first.
{
  const p = normalizePeer(null, null);
  check('fresh install is auto', p.selection, 'auto');
  check('fresh install url', p.url, ROUTER1);
  check('fresh install fallbacks', p.fallbackUrls, [ROUTER2]);
}

// An older session that stored router2 with the wrong scheme must end up on
// the working one, still in automatic mode.
{
  const p = normalizePeer({ url: 'http://achyurouter.xcentic.com/' }, null);
  check('legacy wrong-scheme url migrates', p.url, ROUTER1);
  check('legacy stays auto', p.selection, 'auto');
  check('legacy fallbacks fixed', p.fallbackUrls, [ROUTER2]);
}

// A manual pin must survive the startup replay of the session.
{
  const pinned = { url: ROUTER2, selection: 'manual', fallbackUrls: [] };
  const p = normalizePeer(pinned, null);
  check('pin survives startup replay', p.url, ROUTER2);
  check('pin stays manual', p.selection, 'manual');
  check('pin has no fallbacks', p.fallbackUrls, []);
}

// ...and a re-login. The login screen ALWAYS sends a url (usually the stored
// default), which is exactly how a pin gets silently wiped.
{
  const existing = { url: ROUTER2, selection: 'manual', fallbackUrls: [] };
  const fromLogin = { url: ROUTER1, username: 'u', password: 'p' };
  const p = normalizePeer(fromLogin, existing);
  check('pin survives re-login', p.url, ROUTER2);
  check('pin still manual after login', p.selection, 'manual');
}

// A custom server typed at login is a deliberate choice: pin it.
{
  const p = normalizePeer({ url: 'https://my.own.server' }, null);
  check('custom url pins manual', p.selection, 'manual');
  check('custom url kept', p.url, 'https://my.own.server');
  check('custom url has no fallbacks', p.fallbackUrls, []);
}

// Automatic stays automatic across a login, and credentials are preserved.
{
  const existing = { url: ROUTER1, selection: 'auto', username: 'olduser', password: 'oldpass' };
  const p = normalizePeer({ url: ROUTER1 }, existing);
  check('auto stays auto', p.selection, 'auto');
  check('auto keeps order', p.fallbackUrls, [ROUTER2]);
  check('credentials preserved', [p.username, p.password], ['olduser', 'oldpass']);
}

// Defaults must carry the multi-receiver shape, or bootstrapSender writes a
// config the engine reads as single-receiver.
{
  const d = getDefaultPeer();
  check('defaults carry fallbacks', d.fallbackUrls, [ROUTER2]);
  check('defaults are auto', d.selection, 'auto');
}

console.log('');
if (failures) {
  console.log(`${failures} FAILED`);
  process.exit(1);
}
console.log('all passed');
