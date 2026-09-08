#!/usr/bin/env node
//
// tools/check-engine-name.js — refuse to build a bundle whose engine nobody
// will look for.
//
// The engine's filename lives in three places that must agree:
//
//   1. SENDER_EXE_NAME in electron/main/main.js   — the name the app spawns
//   2. build.win.extraResources[].to              — the name in the Windows bundle
//   3. build.mac.extraResources[].to              — the name in the macOS bundle
//
// They have drifted twice. The second time, a macOS bundle carried the engine
// as "tarang-sender" while main.js spawned "achyu-sender". The app launched,
// found nothing, and reported "Sender offline" — with no error in any log,
// because from its own point of view nothing had failed. The binary was right
// there in Resources under a name nobody asked for.
//
// That is a silent, shipped, field-visible failure produced by a one-line
// omission. A build that stops is cheaper.

const fs = require('fs');

const main = fs.readFileSync('electron/main/main.js', 'utf8');
const m = main.match(/SENDER_EXE_NAME\s*=\s*['"]([^'"]+)['"]/);
if (!m) {
  console.error('  SENDER_EXE_NAME not found in electron/main/main.js');
  process.exit(1);
}
const want = m[1];

const build = require('../package.json').build;
const winRes = (build.win && build.win.extraResources) || [];
const macRes = (build.mac && build.mac.extraResources) || [];

// Matched on the SOURCE filename, which `go build` produces and which never
// varies by product — only the destination is renamed per variant.
const win = winRes.find(r => /(^|\/)tarang-sender\.exe$/.test(r.from));
const mac = macRes.find(r => /(^|\/)tarang-sender$/.test(r.from));

const problems = [];
if (win && win.to !== want + '.exe') {
  problems.push(`Windows bundle ships "${win.to}" but main.js spawns "${want}.exe"`);
}
if (mac && mac.to !== want) {
  problems.push(`macOS bundle ships "${mac.to}" but main.js spawns "${want}"`);
}

if (problems.length) {
  for (const p of problems) console.error('  ' + p);
  console.error('  The app would start, spawn nothing, and sit at "Sender offline"');
  console.error('  with nothing logged. Run: node tools/variant.js <variant>');
  process.exit(1);
}

process.stdout.write(want);
