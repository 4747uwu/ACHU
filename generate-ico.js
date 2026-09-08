// generate-ico.js — build every app-icon asset from the single source logo.
//
//   logo.png (repo root)  ->  electron/logo.png   (byte-for-byte copy)
//                             electron/app.png    (byte-for-byte copy)
//                             electron/logo.ico   (multi-size, used by the
//                                                  window, tray, installer)
//                             electron/app.ico    (kept in sync)
//
// Everything here is LOSSLESS. PNG is a lossless format and we keep it that
// way: full 8-bit RGBA (colorType 6), no palette quantisation, no dithering,
// no JPEG anywhere in the chain. deflateLevel 9 is zlib's *lossless* maximum
// — it only makes the file smaller, never changes a pixel.
//
// The source is 1334x1067 with transparent margins. An .ico entry must be
// square, so the artwork is cropped to its opaque bounding box and then
// centred on a transparent square canvas — that keeps the aspect ratio (no
// stretching) and gives the mark a small, even margin at every icon size.
//
// The source logo.png is itself generated from SVG by tools/logo — run
// `npm run logo` to rebuild the artwork and then these assets in one step.
// Edit the mark's geometry in tools/logo/logo.svg.js, never by hand-editing
// the PNG, or the next regeneration silently discards the change.
//
// Run:  npm run icons
const { Jimp, ResizeStrategy } = require('jimp');
const pngToIco = (() => {
  const m = require('png-to-ico');
  return m.default || m;
})();
const fs = require('fs');
const path = require('path');

const ROOT = __dirname;
const SRC_PNG = path.resolve(ROOT, 'logo.png');
const ELECTRON_DIR = path.resolve(ROOT, 'electron');

// Windows reads 256 for the Explorer tile and 16/24/32 for the taskbar, tray
// and title bar. Shipping every size beats letting Windows downscale one.
const ICO_SIZES = [256, 128, 64, 48, 32, 24, 16];

// Breathing room around the mark, as a fraction of the square side. Tray icons
// look cramped when the artwork runs to the very edge.
const PADDING = 0.04;

// pngjs options: lossless RGBA at zlib's maximum compression.
const PNG_OPTS = { deflateLevel: 9, deflateStrategy: 3, colorType: 6 };

/** Bounding box of everything that is not fully transparent. */
function opaqueBounds(img) {
  const { width: w, height: h, data } = img.bitmap;
  let minX = w, minY = h, maxX = -1, maxY = -1;
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      if (data[(y * w + x) * 4 + 3] > 8) {
        if (x < minX) minX = x;
        if (x > maxX) maxX = x;
        if (y < minY) minY = y;
        if (y > maxY) maxY = y;
      }
    }
  }
  // A fully transparent image would leave maxX at -1; fall back to the whole
  // canvas rather than cropping to nothing.
  if (maxX < 0) return { x: 0, y: 0, w, h };
  return { x: minX, y: minY, w: maxX - minX + 1, h: maxY - minY + 1 };
}

(async () => {
  if (!fs.existsSync(SRC_PNG)) {
    throw new Error(`source logo not found: ${SRC_PNG}`);
  }

  const src = await Jimp.read(SRC_PNG);
  console.log(`Source: ${SRC_PNG} (${src.bitmap.width}x${src.bitmap.height})`);

  // 1. Copy the source PNG verbatim — no re-encode, so not a single byte of
  //    image data is touched.
  for (const name of ['logo.png', 'app.png']) {
    const dest = path.join(ELECTRON_DIR, name);
    fs.copyFileSync(SRC_PNG, dest);
    console.log(`Copied  ->  ${dest}`);
  }

  // 2. Crop to the artwork, then centre it on a transparent square.
  const box = opaqueBounds(src);
  console.log(`Artwork bounds: ${box.w}x${box.h} at (${box.x},${box.y})`);
  const art = src.clone().crop(box);

  const side = Math.round(Math.max(box.w, box.h) * (1 + PADDING * 2));
  const canvas = new Jimp({ width: side, height: side, color: 0x00000000 });
  canvas.blit({
    src: art,
    x: Math.round((side - box.w) / 2),
    y: Math.round((side - box.h) / 2),
  });
  console.log(`Square canvas: ${side}x${side} (transparent background)`);

  // 3. One lossless PNG per icon size.
  const buffers = [];
  for (const size of ICO_SIZES) {
    const frame = canvas.clone().resize({ w: size, h: size, mode: ResizeStrategy.BICUBIC });
    buffers.push(await frame.getBuffer('image/png', PNG_OPTS));
  }
  console.log(`Rendered sizes: ${ICO_SIZES.join(', ')}`);

  // 4. Pack them into the .ico files. png-to-ico stores each PNG frame as-is,
  //    so the archive stays lossless too.
  const ico = await pngToIco(buffers);
  for (const name of ['logo.ico', 'app.ico']) {
    const dest = path.join(ELECTRON_DIR, name);
    fs.writeFileSync(dest, ico);
    console.log(`Written ->  ${dest} (${ico.length} bytes)`);
  }

  console.log('Done — all icon assets rebuilt losslessly.');
})().catch((e) => {
  console.error('Icon generation failed:', e.message);
  console.error(e.stack);
  process.exit(1);
});
