// The Achyu app mark: a geometric "A" on the deep-navy tile the login panel
// already uses (#0a0e27 family), with the reserved amber carrying the
// crossbar. White legs + one amber accent is the same two-tone rule as the
// "Ach/yu" wordmark, so the icon and the type read as one brand.
//
// Geometry notes, because the numbers are not arbitrary:
//   - Legs are STROKES, not a filled outline: one stroke-width controls the
//     whole weight, so the mark stays even when it is resized to 16px.
//   - The crossbar is inset to the counter (the opening between the legs)
//     rather than drawn across them. At icon sizes a bar that overhangs the
//     legs reads as a smudge; one that fills the counter stays a clean dash.
//   - Round caps/joins: the apex and feet keep their weight at 16px, where a
//     mitred point would thin out to nothing.
module.exports = function svg({
  S = 1024,           // canvas side
  W = 142,            // leg stroke width
  apexY = 238,
  footY = 782,
  footDX = 232,       // horizontal offset of each foot from centre
  barY = 622,
  barW = 84,          // deliberately lighter than the legs: a crossbar as
                      // thick as the stems reads as a filled block, not a bar
} = {}) {
  const cx = S / 2;
  const left = cx - footDX, right = cx + footDX;

  // The legs are slanted, so their HORIZONTAL half-width is wider than W/2.
  const len = Math.hypot(footDX, footY - apexY);
  const halfH = (W / 2) * (len / (footY - apexY));

  // Inner edge of each leg at a given height. The counter WIDENS towards the
  // feet, which is why the crossbar cannot be a rectangle: sized to fit the
  // narrow top it overhangs the legs, sized to fit the wide bottom it leaves
  // navy notches at the top. So the bar is a TRAPEZOID whose sides follow the
  // legs exactly, with a 2px outward bleed so antialiasing leaves no seam.
  const innerL = y => cx - footDX * ((y - apexY) / (footY - apexY)) + halfH - 2;
  const innerR = y => cx + footDX * ((y - apexY) / (footY - apexY)) - halfH + 2;
  const barTop = barY - barW / 2, barBot = barY + barW / 2;
  const bar = [
    [innerL(barTop), barTop], [innerR(barTop), barTop],
    [innerR(barBot), barBot], [innerL(barBot), barBot],
  ].map(([x, y]) => x.toFixed(1) + ',' + y.toFixed(1)).join(' ');

  return `<svg xmlns="http://www.w3.org/2000/svg" width="${S}" height="${S}" viewBox="0 0 ${S} ${S}">
  <defs>
    <linearGradient id="tile" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0"   stop-color="#1B2250"/>
      <stop offset="0.55" stop-color="#0D1231"/>
      <stop offset="1"   stop-color="#070A1C"/>
    </linearGradient>
    <radialGradient id="glow" cx="0.3" cy="0.18" r="0.75">
      <stop offset="0" stop-color="#6558e0" stop-opacity="0.30"/>
      <stop offset="1" stop-color="#6558e0" stop-opacity="0"/>
    </radialGradient>
  </defs>

  <rect x="0" y="0" width="${S}" height="${S}" rx="${Math.round(S * 0.225)}" fill="url(#tile)"/>
  <rect x="0" y="0" width="${S}" height="${S}" rx="${Math.round(S * 0.225)}" fill="url(#glow)"/>

  <g fill="none" stroke-linecap="round" stroke-linejoin="round">
    <path d="M ${left} ${footY} L ${cx} ${apexY} L ${right} ${footY}"
          stroke="#F4F5FB" stroke-width="${W}"/>
    <polygon points="${bar}" fill="#f5a524" stroke="none"/>
  </g>
</svg>`;
};
