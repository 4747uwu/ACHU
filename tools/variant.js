#!/usr/bin/env node
// tools/variant.js — switch this tree between installable product variants.
//
//   node tools/variant.js primary     restore the normal Achyu PACS build
//   node tools/variant.js second      make the next build "Achyu PACS 2"
//   node tools/variant.js status      report which variant is applied
//
// WHY A SECOND PRODUCT RATHER THAN A FLAG
//
// electron/main/main.js also supports `--instance=<id>`, which splits one
// installed product into extra profiles at runtime. That is the lighter tool,
// but it needs a shortcut carrying the flag, and both profiles share one
// Start Menu entry and one uninstall entry. A variant is a genuinely separate
// installed product: its own shortcut, its own uninstaller, its own ports, no
// flags to remember. For two permanent routers on a clinic machine, that is
// the one you want.
//
// WHAT MAKES TWO ROUTERS SAFE TO COEXIST
//
// Every value below is one that would otherwise collide. `name` is doing more
// work than it looks: Electron derives userData from it, so changing it alone
// separates config.json, session.json, the BoltDB queue, the logs, the TLS
// cache AND the single-instance lock (Chromium scopes that lock to the
// user-data directory).
//
// The engine binary is deliberately NOT varied. It is config-driven, so both
// products run the same tarang-sender.exe against different config files.

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const PKG = path.join(ROOT, 'package.json');
const MAIN = path.join(ROOT, 'electron', 'main', 'main.js');
const INDEX = path.join(ROOT, 'electron', 'renderer', 'index.html');
const LOGIN = path.join(ROOT, 'electron', 'renderer', 'login.html');

const VARIANTS = {
  primary: {
    pkgName: 'achyu-pacs',
    productName: 'Achyu PACS',
    appId: 'com.achyu.pacs',
    dicomPort: 8899,
    httpPort: 9044,
    aet: 'ACHYU',
    // The engine's FILENAME, which is per-product on purpose. Builds older
    // than this reap orphans with `taskkill /F /IM tarang-sender.exe`, the one
    // name every build used to share, so they kill any engine they find. A
    // product whose engine is named otherwise is simply not matched, which is
    // what lets a new build survive next to a legacy one we cannot patch.
    senderExe: 'achyu-sender',
    // Rendered in the navbar as Ach|yu| PACS — the amber span splits it.
    brandTail: ' PACS',
  },
  // Built to coexist with a DIFFERENT company's router on one machine. Every
  // per-install identity below differs from the primary — including the ones
  // that only collide when the neighbour is also built from this codebase,
  // which it may well be, since we supply that company too. The DICOM port is
  // the deliberate exception; see the note on it.
  second: {
    // NOT "achyu-pacs-2": the primary's uninstaller purges "achyu-pacs-*" to
    // clean up --instance profiles, and a hyphenated name would be swept away
    // with them. Without the hyphen it cannot match that wildcard.
    pkgName: 'achyu-pacs2',
    productName: 'Achyu PACS 2',
    appId: 'com.achyu.pacs2',
    // DICOM stays on 8899 ON PURPOSE. This variant is built to sit beside a
    // router belonging to another company on the same machine, and the
    // modalities at that site are already configured to send to 8899 —
    // changing it here would mean reconfiguring every modality, which is the
    // one cost this whole exercise exists to avoid.
    //
    // That only works while the co-resident router is NOT itself listening on
    // 8899. Two DICOM receivers cannot share a port: whichever binds second
    // fails. bootstrapSender probes it at startup and says so plainly rather
    // than moving the port behind the operator's back.
    dicomPort: 8899,
    httpPort: 9045,
    aet: 'ACHYU2',
    // Distinct from the primary's too, so neither Achyu product can reach the
    // other's engine by image name even if a future blanket kill creeps back.
    senderExe: 'achyu2-sender',
    brandTail: ' PACS 2',
  },
};

function edit(file, pairs) {
  let text = fs.readFileSync(file, 'utf8');
  for (const [find, replace] of pairs) {
    if (text.includes(replace) && !text.includes(find)) continue; // already applied
    if (!text.includes(find)) {
      throw new Error(`${path.relative(ROOT, file)}: could not find ${JSON.stringify(find)}`);
    }
    text = text.split(find).join(replace);
  }
  fs.writeFileSync(file, text);
}

function detect() {
  const pkg = JSON.parse(fs.readFileSync(PKG, 'utf8'));
  for (const [key, v] of Object.entries(VARIANTS)) {
    if (pkg.name === v.pkgName) return key;
  }
  return null;
}

function apply(target) {
  const to = VARIANTS[target];
  const from = VARIANTS[detect() === 'second' ? 'second' : 'primary'];
  if (!to) throw new Error(`unknown variant: ${target}`);

  // package.json: name drives userData and the lock; the rest are what the
  // operator sees in Start Menu, Add/Remove Programs and the shortcut.
  const pkg = JSON.parse(fs.readFileSync(PKG, 'utf8'));
  pkg.name = to.pkgName;
  pkg.description = `${to.productName} — DICOM Router`;
  pkg.build.appId = to.appId;
  pkg.build.productName = to.productName;
  pkg.build.nsis.shortcutName = to.productName;
  // Rename the engine as it is copied into resources. The SOURCE stays
  // whatever `go build` produced, so the Go side never has to change.
  //
  // BOTH platforms, and that is the point: rewriting only the Windows mapping
  // shipped a macOS bundle whose engine was still called tarang-sender while
  // main.js looked for achyu-sender. The app launched, found nothing to spawn,
  // and sat there reporting "Sender offline" with no error anywhere -- the
  // engine was right there in Resources under a name nobody asked for.
  for (const res of pkg.build.win.extraResources || []) {
    if (res.from === 'tarang-sender.exe') res.to = `${to.senderExe}.exe`;
  }
  for (const res of (pkg.build.mac && pkg.build.mac.extraResources) || []) {
    if (res.from === 'tarang-sender') res.to = to.senderExe;
  }
  if (pkg.build.dmg) pkg.build.dmg.title = to.productName;
  fs.writeFileSync(PKG, JSON.stringify(pkg, null, 2) + '\n');

  edit(MAIN, [
    [`const PRODUCT_NAME        = '${from.productName}';`,
     `const PRODUCT_NAME        = '${to.productName}';`],
    [`const DEFAULT_DICOM_PORT  = ${from.dicomPort};`,
     `const DEFAULT_DICOM_PORT  = ${to.dicomPort};`],
    [`const DEFAULT_HTTP_PORT   = ${from.httpPort};`,
     `const DEFAULT_HTTP_PORT   = ${to.httpPort};`],
    [`const DEFAULT_AET         = '${from.aet}';`,
     `const DEFAULT_AET         = '${to.aet}';`],
    [`const SENDER_EXE_NAME = '${from.senderExe}';`,
     `const SENDER_EXE_NAME = '${to.senderExe}';`],
  ]);

  edit(INDEX, [
    [`<title>${from.productName}</title>`, `<title>${to.productName}</title>`],
    [`<span class="brand-x">yu</span>${from.brandTail}</span>`,
     `<span class="brand-x">yu</span>${to.brandTail}</span>`],
  ]);
  edit(LOGIN, [
    [`<title>${from.productName}</title>`, `<title>${to.productName}</title>`],
  ]);

  return to;
}

const arg = (process.argv[2] || 'status').toLowerCase();
if (arg === 'status') {
  const cur = detect();
  console.log(cur ? `variant: ${cur} (${VARIANTS[cur].productName})` : 'variant: unrecognised');
} else if (VARIANTS[arg]) {
  const v = apply(arg);
  console.log(`Applied variant "${arg}":`);
  console.log(`  product   ${v.productName}`);
  console.log(`  package   ${v.pkgName}   (userData: %APPDATA%\\${v.pkgName})`);
  console.log(`  appId     ${v.appId}`);
  console.log(`  DICOM     ${v.dicomPort}`);
  console.log(`  HTTP API  ${v.httpPort}`);
  console.log(`  AE title  ${v.aet}`);
  console.log(`  engine    ${v.senderExe}.exe`);
  console.log('\nNow run:  npx electron-builder --win --ia32');
} else {
  console.error(`usage: node tools/variant.js [primary|second|status]`);
  process.exit(1);
}
