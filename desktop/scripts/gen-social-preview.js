// Renders the repository's social-preview image (1280 x 640): the picture shown when a link to the project is
// shared. It is made from the app icon and a real screenshot, so it never shows anything the app does not do.
//   npx electron scripts/gen-social-preview.js     writes docs/social-preview.png
// GitHub has no API for the image: upload it under Settings > General > Social preview.
const fs = require("node:fs");
const path = require("node:path");
const root = path.join(__dirname, "..", "..");
const b64 = (f) => fs.readFileSync(path.join(root, f)).toString("base64");
const icon = `data:image/svg+xml;base64,${b64("desktop/build/icon.svg")}`;
const shot = `data:image/png;base64,${b64("docs/screenshots/app-live-dark.png")}`;
const html = `<!doctype html><meta charset="utf-8"><style>
*{box-sizing:border-box} html,body{margin:0;width:1280px;height:640px;overflow:hidden}
body{font:-apple-system,BlinkMacSystemFont,"SF Pro Display","Helvetica Neue",Arial,sans-serif;font-family:-apple-system,BlinkMacSystemFont,"SF Pro Display","Helvetica Neue",Arial,sans-serif;color:#f5f5f7;background:radial-gradient(1100px 600px at 82% 18%,#1d2a44 0%,#0f1117 58%,#0a0a0c 100%);position:relative}
.left{position:absolute;left:72px;top:92px;width:560px}
.icon{width:116px;height:116px;margin:-8px 0 14px -8px}
h1{font-size:76px;line-height:1;margin:0 0 22px;font-weight:700;letter-spacing:-2px}
p{font-size:30px;line-height:1.3;margin:0 0 30px;color:#c7c7cc;letter-spacing:-.3px}
.tags{display:flex;gap:10px;flex-wrap:wrap}
.tags span{font-size:20px;font-weight:600;padding:7px 16px;border-radius:99px;background:rgba(255,255,255,.1);color:#e8e8ed}
.card{position:absolute;left:660px;top:78px;width:760px;border-radius:22px;overflow:hidden;box-shadow:0 30px 80px rgba(0,0,0,.6),0 0 0 1px rgba(255,255,255,.1);transform:rotate(-2.2deg)}
.card img{display:block;width:760px}
</style><div class="left"><img class="icon" src="${icon}"><h1>Signal Lab</h1><p>Replay simulated telemetry at your own service, and see how it copes.</p><div class="tags"><span>macOS</span><span>Windows</span><span>Linux</span><span>Open source</span></div></div><div class="card"><img src="${shot}"></div>`;
const { app, BrowserWindow } = require("electron");
app.disableHardwareAcceleration();
app.whenReady().then(async () => {
  const win = new BrowserWindow({ width: 1280, height: 640, show: false, useContentSize: true, webPreferences: { offscreen: true, backgroundThrottling: false } });
  await win.loadURL("data:text/html;charset=utf-8," + encodeURIComponent(html));
  await new Promise((r) => setTimeout(r, 700));
  const img = await win.webContents.capturePage({ x: 0, y: 0, width: 1280, height: 640 });
  fs.writeFileSync(path.join(root, "docs", "social-preview.png"), img.toPNG());
  console.log("wrote docs/social-preview.png", img.getSize());
  app.quit();
});
