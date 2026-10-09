// Generates the app icon in Apple's macOS style: a 1024 px canvas, an 824 px body with continuous
// ("squircle") corners, a soft top-lit gradient and a gentle shadow, with a white pulse line.
//   node scripts/gen-icon.js          writes build/icon.svg and the panel's copy (internal/appui/ui/icon.svg)
//   npx electron scripts/gen-icon.js --png   also renders build/icon.png (1024 px, transparent)
const fs = require("node:fs");
const path = require("node:path");

const root = path.join(__dirname, "..");
const SIZE = 1024, BODY = 824, N = 5; // N is the superellipse exponent (iOS/macOS icons are close to 5)

function squircle(cx, cy, half, n, steps = 360) {
  const pts = [];
  for (let i = 0; i < steps; i++) {
    const t = (i / steps) * 2 * Math.PI;
    const c = Math.cos(t), s = Math.sin(t);
    const x = cx + half * Math.sign(c) * Math.abs(c) ** (2 / n);
    const y = cy + half * Math.sign(s) * Math.abs(s) ** (2 / n);
    pts.push(`${x.toFixed(1)} ${y.toFixed(1)}`);
  }
  return "M" + pts.join("L") + "Z";
}

const c = SIZE / 2;
const body = squircle(c, c, BODY / 2, N);
const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${SIZE} ${SIZE}" width="${SIZE}" height="${SIZE}">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#5BB0FF"/>
      <stop offset="0.55" stop-color="#1A7BF2"/>
      <stop offset="1" stop-color="#0B55D1"/>
    </linearGradient>
    <linearGradient id="rim" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#fff" stop-opacity="0.55"/>
      <stop offset="0.35" stop-color="#fff" stop-opacity="0"/>
      <stop offset="1" stop-color="#000" stop-opacity="0.18"/>
    </linearGradient>
    <filter id="shadow" x="-10%" y="-10%" width="120%" height="125%">
      <feDropShadow dx="0" dy="14" stdDeviation="16" flood-color="#00112B" flood-opacity="0.34"/>
    </filter>
    <filter id="glyph" x="-10%" y="-10%" width="120%" height="130%">
      <feDropShadow dx="0" dy="8" stdDeviation="9" flood-color="#00246B" flood-opacity="0.35"/>
    </filter>
  </defs>
  <path d="${body}" fill="url(#bg)" filter="url(#shadow)"/>
  <path d="${body}" fill="none" stroke="url(#rim)" stroke-width="3"/>
  <g filter="url(#glyph)">
    <path d="M232 540 H356 L418 352 L512 704 L596 456 L652 540 H792" fill="none" stroke="#fff" stroke-width="52" stroke-linecap="round" stroke-linejoin="round"/>
    <circle cx="792" cy="540" r="40" fill="#fff"/>
  </g>
</svg>
`;

const targets = [path.join(root, "build", "icon.svg"), path.join(root, "..", "internal", "appui", "ui", "icon.svg")];
for (const t of targets) fs.writeFileSync(t, svg);
console.log("wrote", targets.map((t) => path.relative(path.join(root, ".."), t)).join(", "));

// Rendering to PNG needs Chromium, so it only runs under Electron.
if (process.argv.includes("--png") && process.versions.electron) {
  const { app, BrowserWindow } = require("electron");
  app.disableHardwareAcceleration();
  app.whenReady().then(async () => {
    const win = new BrowserWindow({ width: SIZE, height: SIZE, show: false, transparent: true, frame: false, useContentSize: true,
      webPreferences: { offscreen: true, backgroundThrottling: false } });
    await win.loadURL("data:text/html;charset=utf-8," + encodeURIComponent(`<style>html,body{margin:0;background:transparent}svg{display:block}</style>${svg}`));
    await new Promise((r) => setTimeout(r, 400));
    const img = await win.webContents.capturePage({ x: 0, y: 0, width: SIZE, height: SIZE });
    fs.writeFileSync(path.join(root, "build", "icon.png"), img.toPNG());
    console.log("wrote build/icon.png", img.getSize());
    app.quit();
  });
}
