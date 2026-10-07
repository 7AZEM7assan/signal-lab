// End-to-end smoke test of the desktop app: starts the real Electron app against the real engine,
// runs a replay through the panel, quits, restarts on the same data folder and checks the data
// survived. On a headless Linux machine run it under a virtual display: xvfb-run -a npm run smoke
import { _electron as electron } from "playwright-core";
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, "..");
const electronPath = createRequire(import.meta.url)("electron");
const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "signal-lab-smoke-"));
const userData = fs.mkdtempSync(path.join(os.tmpdir(), "signal-lab-ud-"));
const shotDir = process.env.SMOKE_SHOTS;
let failures = 0;
const check = (ok, what, detail = "") => { console.log(`${ok ? "PASS" : "FAIL"}  ${what}${detail ? "  (" + detail + ")" : ""}`); if (!ok) failures++; };

async function launch() {
  // SMOKE_PACKAGED=/path/to/the/built/executable tests an installed build instead of the source tree.
  const packaged = process.env.SMOKE_PACKAGED;
  return electron.launch({
    executablePath: packaged || electronPath,
    args: [...(packaged ? [] : [root]), `--user-data-dir=${userData}`],
    env: { ...process.env, SIGNALLAB_APP_DATA: dataDir, SIGNALLAB_NO_SANDBOX: process.env.SIGNALLAB_NO_SANDBOX ?? (process.getuid?.() === 0 ? "1" : "") },
  });
}
const stored = async (win) => {
  await win.click('[data-tab="storage"]');
  await win.waitForTimeout(800);
  return Number((await win.innerText("#sEvents")).replace(/[^\d]/g, ""));
};

let app = await launch();
let win = await app.firstWindow();
await win.waitForSelector("#fields input", { state: "attached", timeout: 30000 });
check((await win.title()) === "Signal Lab", "window opens with the control panel", await win.title());
check(!(await win.url()).includes("token="), "the secret token is not left in the page URL", await win.url());

const iso = await win.evaluate(() => ({
  bridge: Object.keys(window.signalLabDesktop || {}).sort().join(","),
  node: typeof window.require, proc: typeof window.process,
}));
check(iso.bridge === "chooseDataFolder,openDataFolder", "only the two folder actions are exposed to the page", iso.bridge);
check(iso.node === "undefined" && iso.proc === "undefined", "page has no Node.js access (context isolation)");

for (let i = 0; i < 50 && !(await win.innerText("#chipWs")).includes("connected"); i++) await win.waitForTimeout(100);
check((await win.innerText("#chipWs")).includes("connected"), "live feed connects");

await win.click('[data-tab="replay"]');
await win.click("#btnStart");
let state = "";
for (let i = 0; i < 300; i++) { state = await win.innerText("#rpState"); if (["done", "stopped", "failed"].includes(state)) break; await win.waitForTimeout(100); }
check(state === "done", "a replay started from the panel runs to completion", state);
if (shotDir) { fs.mkdirSync(shotDir, { recursive: true }); await win.screenshot({ path: path.join(shotDir, "desktop-replay.png") }); }

await win.waitForTimeout(1200); // let the queue drain to disk
const first = await stored(win);
check(first > 0, "events are stored in the embedded database", String(first));
if (shotDir) await win.screenshot({ path: path.join(shotDir, "desktop-storage.png") });
check(fs.existsSync(path.join(dataDir, "signallab.db")), "database file is created in the data folder");

await app.close();
await new Promise((r) => setTimeout(r, 1500));
let orphan = false;
if (process.platform !== "win32") {
  try { execFileSync("pgrep", ["-f", "--", `app --data-dir ${dataDir}`], { stdio: "pipe" }); orphan = true; } catch (e) { orphan = e.status !== 1; } // status 1 = no match; anything else means pgrep itself failed
}
check(!orphan, "closing the window also stops the engine process");

app = await launch();
win = await app.firstWindow();
await win.waitForSelector("#fields input", { state: "attached", timeout: 30000 });
const again = await stored(win);
check(again === first, "data is still there after restarting the app", `${again} vs ${first}`);
await app.close();

fs.rmSync(dataDir, { recursive: true, force: true });
fs.rmSync(userData, { recursive: true, force: true });
console.log(failures ? `\n${failures} check(s) failed` : "\nAll checks passed");
process.exit(failures ? 1 : 0);
