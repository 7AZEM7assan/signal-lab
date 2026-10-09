// Generates the app icon in Apple's macOS style. The default icon is black (a graphite body, a white
// pulse line and a blue end dot) on a 1024 px canvas with an 824 px continuous-corner ("squircle")
// body and a soft shadow. A second, light version is made for macOS 26 and later, which switches
// between them with the system appearance.
//
//   node scripts/gen-icon.js                       writes build/icon.svg, the panel's copy and build/icon.icon/
//   npx electron scripts/gen-icon.js --png         also renders build/icon.png (1024 px, transparent)
//   node scripts/gen-icon.js --compile             also compiles build/Assets.car and build/icon.icns (macOS, Xcode 26+)
//
// The compiled files are committed, so building the installers does not need Xcode 26.
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { execFileSync } = require("node:child_process");

const root = path.join(__dirname, "..");
const SIZE = 1024, BODY = 824, N = 5; // N is the superellipse exponent (macOS and iOS icons are close to 5)

function squircle(cx, cy, half, n, steps = 360) {
  const pts = [];
  for (let i = 0; i < steps; i++) {
    const t = (i / steps) * 2 * Math.PI;
    const c = Math.cos(t), s = Math.sin(t);
    pts.push(`${(cx + half * Math.sign(c) * Math.abs(c) ** (2 / n)).toFixed(1)} ${(cy + half * Math.sign(s) * Math.abs(s) ** (2 / n)).toFixed(1)}`);
  }
  return "M" + pts.join("L") + "Z";
}

const PULSE = "M232 540 H356 L418 352 L512 704 L596 456 L652 540 H760";
const body = squircle(SIZE / 2, SIZE / 2, BODY / 2, N);

// The default (black) icon, also used for Windows, Linux, older macOS and the panel's header.
const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${SIZE} ${SIZE}" width="${SIZE}" height="${SIZE}">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#2E2E33"/>
      <stop offset="1" stop-color="#0A0A0C"/>
    </linearGradient>
    <linearGradient id="rim" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#fff" stop-opacity="0.38"/>
      <stop offset="0.3" stop-color="#fff" stop-opacity="0"/>
      <stop offset="1" stop-color="#000" stop-opacity="0.35"/>
    </linearGradient>
    <filter id="shadow" x="-10%" y="-10%" width="120%" height="125%">
      <feDropShadow dx="0" dy="14" stdDeviation="16" flood-color="#000" flood-opacity="0.38"/>
    </filter>
    <filter id="glyph" x="-10%" y="-10%" width="120%" height="130%">
      <feDropShadow dx="0" dy="8" stdDeviation="9" flood-color="#000" flood-opacity="0.5"/>
    </filter>
  </defs>
  <path d="${body}" fill="url(#bg)" filter="url(#shadow)"/>
  <path d="${body}" fill="none" stroke="url(#rim)" stroke-width="3"/>
  <g filter="url(#glyph)">
    <path d="${PULSE}" fill="none" stroke="#fff" stroke-width="52" stroke-linecap="round" stroke-linejoin="round"/>
    <circle cx="792" cy="540" r="40" fill="#4DA3FF"/>
  </g>
</svg>
`;

// The macOS 26 icon document (Icon Composer format): one glyph layer, with a light and a dark look.
const glyph = (stroke, dot) => `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${SIZE} ${SIZE}" width="${SIZE}" height="${SIZE}">
  <path d="${PULSE}" fill="none" stroke="${stroke}" stroke-width="52" stroke-linecap="round" stroke-linejoin="round"/>
  <circle cx="792" cy="540" r="40" fill="${dot}"/>
</svg>
`;
const iconJson = {
  "fill-specializations": [
    { value: { "automatic-gradient": "display-p3:0.89000,0.92000,0.98000,1.00000" } },
    { appearance: "dark", value: { solid: "display-p3:0.07000,0.07000,0.08000,1.00000" } },
  ],
  groups: [{
    layers: [{
      glass: false,
      "image-name-specializations": [{ value: "glyph-light.svg" }, { appearance: "dark", value: "glyph-dark.svg" }],
      name: "pulse",
      position: { scale: 1, "translation-in-points": [0, 0] },
    }],
    shadow: { kind: "neutral", opacity: 0.5 },
    translucency: { enabled: false, value: 0 },
  }],
  "supported-platforms": { squares: ["macOS"] },
};

const iconDir = path.join(root, "build", "icon.icon");
fs.mkdirSync(path.join(iconDir, "Assets"), { recursive: true });
fs.writeFileSync(path.join(iconDir, "icon.json"), JSON.stringify(iconJson, null, 2) + "\n");
fs.writeFileSync(path.join(iconDir, "Assets", "glyph-light.svg"), glyph("#1D1D1F", "#0A66D6"));
fs.writeFileSync(path.join(iconDir, "Assets", "glyph-dark.svg"), glyph("#FFFFFF", "#4DA3FF"));

const targets = [path.join(root, "build", "icon.svg"), path.join(root, "..", "internal", "appui", "ui", "icon.svg")];
for (const t of targets) fs.writeFileSync(t, svg);
console.log("wrote", [...targets, iconDir].map((t) => path.relative(path.join(root, ".."), t)).join(", "));

// Compile the icon document the way electron-builder does (needs actool from Xcode 26 or later).
if (process.argv.includes("--compile")) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "icon-compile-"));
  const src = path.join(tmp, "Icon.icon"), out = path.join(tmp, "out");
  fs.cpSync(iconDir, src, { recursive: true });
  fs.mkdirSync(out);
  execFileSync("actool", [src, "--compile", out, "--output-format", "human-readable-text", "--notices", "--warnings",
    "--output-partial-info-plist", path.join(out, "assetcatalog_generated_info.plist"), "--app-icon", "Icon", "--include-all-app-icons",
    "--accent-color", "AccentColor", "--enable-on-demand-resources", "NO", "--development-region", "en", "--target-device", "mac",
    "--minimum-deployment-target", "26.0", "--platform", "macosx"], { stdio: "inherit" });
  fs.copyFileSync(path.join(out, "Assets.car"), path.join(root, "build", "Assets.car"));
  fs.copyFileSync(path.join(out, "Icon.icns"), path.join(root, "build", "icon.icns"));
  fs.rmSync(tmp, { recursive: true, force: true });
  console.log("wrote build/Assets.car, build/icon.icns");
}

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
