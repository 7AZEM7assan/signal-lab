// Captures the README screenshots from the real desktop app (needs a display; on headless Linux
// run it as: xvfb-run -a npm run screenshots). Output goes to ../docs/screenshots/app-*.png.
import { _electron as electron } from "playwright-core";
import { createRequire } from "node:module";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const out = path.join(here, "..", "..", "docs", "screenshots");
const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "signal-lab-shots-"));
const userData = fs.mkdtempSync(path.join(os.tmpdir(), "signal-lab-ud-"));
fs.mkdirSync(out, { recursive: true });

const app = await electron.launch({
  executablePath: createRequire(import.meta.url)("electron"),
  args: [path.join(here, ".."), `--user-data-dir=${userData}`],
  env: { ...process.env, SIGNALLAB_APP_DATA: dataDir, SIGNALLAB_NO_SANDBOX: process.getuid?.() === 0 ? "1" : "" },
  colorScheme: "light",
});
const win = await app.firstWindow();
await win.waitForSelector("#fields input", { state: "attached", timeout: 30000 });
const resize = (h) => app.evaluate(({ BrowserWindow }, height) => BrowserWindow.getAllWindows()[0].setContentSize(1180, height), h);
const shot = async (name) => { await win.evaluate(() => window.scrollTo(0, 0)); await win.screenshot({ path: path.join(out, name) }); };

await win.click('[data-tab="replay"]');
await win.click('#presets button:has-text("Fault storm")');
await win.click("#btnStart");
for (let i = 0; i < 600; i++) { if (["done", "stopped", "failed"].includes(await win.innerText("#rpState"))) break; await win.waitForTimeout(100); }
await win.waitForTimeout(1500);
await resize(1180);
await win.waitForTimeout(300);
await shot("app-replay.png");

await win.click('[data-tab="live"]'); await resize(860); await win.waitForTimeout(800); await shot("app-live.png");

await win.click('[data-tab="data"]');
await win.selectOption("#dKind", "alerts");
await win.click('#dataForm button[type=submit]');
await win.waitForSelector("#dataTable tbody tr");
await resize(860); await win.waitForTimeout(300); await shot("app-data.png");

await app.close();
fs.rmSync(dataDir, { recursive: true, force: true });
fs.rmSync(userData, { recursive: true, force: true });
console.log("wrote", fs.readdirSync(out).filter((f) => f.startsWith("app-")).join(", "));
