// tools/logo/main.js — regenerate the Achyu app mark.
//
//   npm run logo      ->  logo.svg + logo.png (repo root), then npm run icons
//
// Why Electron and not a node image library: the mark is defined as SVG (see
// logo.svg.js), and nothing in devDependencies can rasterise SVG. Electron is
// already a devDependency and carries a full Chromium, so it renders the
// artwork through a <canvas> and reads back a true RGBA PNG. That keeps the
// icon reproducible from source with no new dependency.
//
// The canvas path is used deliberately in preference to capturePage(): it
// yields the alpha channel directly, independent of any window or compositor
// transparency support.
const { app, BrowserWindow } = require('electron');
const fs = require('fs');
const path = require('path');
const buildSvg = require('./logo.svg.js');

const ROOT = path.resolve(__dirname, '..', '..');
const SIZE = Number(process.env.LOGO_SIZE || 1024);
const OUT_PNG = process.env.LOGO_OUT || path.join(ROOT, 'logo.png');
const OUT_SVG = path.join(ROOT, 'logo.svg');

app.disableHardwareAcceleration();

app.whenReady().then(async () => {
  const win = new BrowserWindow({ width: 400, height: 400, show: false, webPreferences: { offscreen: true } });
  let code = 0;
  try {
    const svg = buildSvg({ S: SIZE });
    fs.writeFileSync(OUT_SVG, svg);
    console.log(`Wrote ${OUT_SVG}`);

    await win.loadURL('data:text/html,<body style="margin:0"></body>');
    const dataUrl = await win.webContents.executeJavaScript(`new Promise((res, rej) => {
      const svg = ${JSON.stringify(svg)};
      const img = new Image();
      img.onload = () => {
        const c = document.createElement('canvas');
        c.width = ${SIZE}; c.height = ${SIZE};
        const g = c.getContext('2d');
        g.clearRect(0, 0, ${SIZE}, ${SIZE});
        g.drawImage(img, 0, 0, ${SIZE}, ${SIZE});
        res(c.toDataURL('image/png'));
      };
      img.onerror = () => rej(new Error('SVG failed to decode'));
      img.src = 'data:image/svg+xml;base64,' + btoa(unescape(encodeURIComponent(svg)));
    })`);

    const buf = Buffer.from(dataUrl.split(',')[1], 'base64');
    fs.writeFileSync(OUT_PNG, buf);
    console.log(`Wrote ${OUT_PNG} (${SIZE}x${SIZE}, ${buf.length} bytes)`);
    console.log('Now run: npm run icons');
  } catch (e) {
    console.error('Logo generation failed:', e && e.stack ? e.stack : e);
    code = 1;
  }
  app.exit(code);
});
