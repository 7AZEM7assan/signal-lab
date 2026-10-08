// End-to-end smoke test of the desktop app: starts the real Electron app against the real engine,
// runs the Quick demo from the Live tab, checks the panel's main behaviours, quits, restarts on the
// same data folder and checks the data survived. It also rehearses the two startup failures (an
// unusable data folder and a busy port) and checks the access token never reaches the log file.
// On a headless Linux machine run it under a virtual display: xvfb-run -a npm run smoke
import { _electron as electron } from "playwright-core";
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, "..");
const electronPath = createRequire(import.meta.url)("electron");
const tmp = (p) => fs.mkdtempSync(path.join(os.tmpdir(), p));
const dataDir = tmp("signal-lab-smoke-");
const userData = tmp("signal-lab-ud-");
const shotDir = process.env.SMOKE_SHOTS;
let failures = 0;
const check = (ok, what, detail = "") => { console.log(`${ok ? "PASS" : "FAIL"}  ${what}${detail ? "  (" + detail + ")" : ""}`); if (!ok) failures++; };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 20000, step = 100) { const t0 = Date.now(); for (;;) { const v = await fn(); if (v) return v; if (Date.now() - t0 > ms) return false; await sleep(step); } }

async function launch(opts = {}) {
  // SMOKE_PACKAGED=/path/to/the/built/executable tests an installed build instead of the source tree.
  const packaged = process.env.SMOKE_PACKAGED;
  return electron.launch({
    executablePath: packaged || electronPath,
    args: [...(packaged ? [] : [root]), `--user-data-dir=${opts.userData ?? userData}`],
    env: { ...process.env, ...(opts.dataDir === null ? {} : { SIGNALLAB_APP_DATA: opts.dataDir ?? dataDir }), SIGNALLAB_NO_SANDBOX: process.env.SIGNALLAB_NO_SANDBOX ?? (process.getuid?.() === 0 ? "1" : ""), ...(opts.env ?? {}) },
  });
}
const stored = async (win) => {
  await win.click('[data-tab="storage"]');
  await win.waitForTimeout(800);
  return Number((await win.innerText("#sEvents")).replace(/[^\d]/g, ""));
};
const panelReady = (win) => win.waitForSelector("#fields input", { state: "attached", timeout: 30000 });

// ---------------------------------------------------------------- normal run
let app = await launch();
let win = await app.firstWindow();
await panelReady(win);
check((await win.title()) === "Signal Lab", "window opens with the control panel", await win.title());
check(!(await win.url()).includes("token="), "the secret token is not left in the page URL", await win.url());

const iso = await win.evaluate(() => ({
  bridge: Object.keys(window.signalLabDesktop || {}).sort().join(","),
  node: typeof window.require, proc: typeof window.process,
  startup: typeof window.signalLabStartup,
  heading: getComputedStyle(document.querySelector(".brand h1")).position,
  icon: document.querySelector(".brand img")?.naturalWidth > 0,
}));
check(iso.bridge === "chooseDataFolder,openDataFolder", "only the two folder actions are exposed to the page", iso.bridge);
check(iso.node === "undefined" && iso.proc === "undefined", "page has no Node.js access (context isolation)");
check(iso.startup === "undefined", "the panel cannot reach the startup-error actions");
check(iso.icon && iso.heading === "absolute", "header shows the app icon, and the duplicate title is hidden in the window");

check(await until(async () => (await win.innerText("#chipWs")).includes("connected"), 10000), "live feed connects");

// Item: Quick demo from the Live tab's empty state, then the one-line summary.
check(await win.isVisible("#devicesEmpty .demoBtn"), "Live tab offers 'Run quick demo' when empty");
await win.click("#devicesEmpty .demoBtn");
check(await until(() => win.isVisible("#runDone"), 60000), "a finished run shows the summary strip");
const summary = await win.innerText("#runDoneText");
check(/^Replay done: 300 sent, 300 stored, 0 rejected$/.test(summary), "summary reads '300 sent, 300 stored, 0 rejected'", summary);
check(await win.getAttribute('[data-tab="live"]', "aria-selected") === "true", "we stay on (return to) the Live tab");
await win.waitForTimeout(1200);
check((await win.locator("#devices svg.spark").count()) === 10, "sparklines: two per device for five devices");
check((await win.locator("#devices svg.spark line.thr").count()) === 10, "every sparkline draws the alert threshold");
if (shotDir) { fs.mkdirSync(shotDir, { recursive: true }); await win.screenshot({ path: path.join(shotDir, "desktop-live.png") }); }

await win.click("#runDoneView");
check(await until(async () => (await win.locator("#dataTable tbody tr").count()) > 0), "'View in Data' opens the Data tab with rows");
check((await win.getAttribute("#exCsv", "href")).includes("format=csv") && (await win.innerText("#exJson")) === "Export JSON", "Export CSV and Export JSON are offered");

await win.click('[data-tab="replay"]');
check((await win.locator("#fields details.accordion[open]").count()) === 0 && (await win.locator("#fields details.accordion").count()) === 2, "Faults and Advanced are accordions that start closed");
check((await win.evaluate(() => getComputedStyle(document.getElementById("runbar")).position)) === "sticky", "the preset and Start bar is sticky");
if (shotDir) await win.screenshot({ path: path.join(shotDir, "desktop-replay.png") });

const first = await stored(win);
check(first === 300, "events are stored in the embedded database", String(first));
if (shotDir) await win.screenshot({ path: path.join(shotDir, "desktop-storage.png") });
check(fs.existsSync(path.join(dataDir, "signallab.db")), "database file is created in the data folder");

await app.close();
await sleep(1500);
let orphan = false;
if (process.platform !== "win32") {
  try { execFileSync("pgrep", ["-f", "--", `app --data-dir ${dataDir}`], { stdio: "pipe" }); orphan = true; } catch (e) { orphan = e.status !== 1; } // status 1 = no match; anything else means pgrep itself failed
}
check(!orphan, "closing the window also stops the engine process");

// The token must reach the shell (it works) but never the log.
const logFile = path.join(userData, "logs", "signal-lab.log");
const log = fs.existsSync(logFile) ? fs.readFileSync(logFile, "utf8") : "";
check(/ready addr=127\.0\.0\.1:\d+ port=\d+/.test(log), "log records the engine's address and port", (log.match(/ready addr=\S+ port=\d+/) || [""])[0]);
check(!/token=/i.test(log) && !/[0-9a-f]{64}/i.test(log), "log contains no access token");

app = await launch();
win = await app.firstWindow();
await panelReady(win);
const again = await stored(win);
check(again === first, "data is still there after restarting the app", `${again} vs ${first}`);
await app.close();

// ---------------------------------------------------------------- startup failures
async function errorScreen(label, opts, expectReason) {
  const a = await launch(opts);
  const w = await a.firstWindow();
  const ok = await until(async () => { try { return (await w.innerText("#reason")) !== "Starting…"; } catch { return false; } }, 30000);
  const reason = ok ? await w.innerText("#reason") : "";
  check(ok && (await w.title()) === "Signal Lab could not start", `${label}: an error screen appears instead of a blank window`, (await w.title().catch(() => "")));
  check(expectReason.test(reason), `${label}: it names the reason`, reason.slice(0, 110));
  const details = (await w.textContent("#details").catch(() => "")) ?? ""; // inside a closed <details>, so innerText would be empty
  check(details.includes("Data folder:") && details.includes("Log file:") && !/[0-9a-f]{64}/i.test(details), `${label}: details are complete and token-free`);
  return { a, w, reason };
}

// 1. a data folder that cannot be created (its parent is a file)
const blocker = path.join(tmp("signal-lab-blocker-"), "i-am-a-file");
fs.writeFileSync(blocker, "x");
{
  const { a, w } = await errorScreen("unusable data folder", { dataDir: path.join(blocker, "data"), userData: tmp("signal-lab-ud2-") }, /data folder/i);
  await a.evaluate(({ shell }) => { shell.openPath = async (p) => { globalThis.__opened = p; return ""; }; });
  await w.click("#copy");
  const copied = await a.evaluate(({ clipboard }) => clipboard.readText());
  check(copied.includes("Reason:") && copied.includes("data folder"), "'Copy details' puts the details on the clipboard");
  await w.click("#openlog");
  await sleep(300);
  const opened = await a.evaluate(() => globalThis.__opened);
  check(typeof opened === "string" && opened.endsWith("logs"), "'Open log folder' opens the log folder", String(opened));
  // Fix the problem and press "Try again": the panel should open and the error screen go away.
  fs.rmSync(blocker);
  await w.click("#retry");
  const panel = await until(async () => {
    for (const x of a.windows()) { try { if ((await x.title()) === "Signal Lab" && (await x.locator("#fields input").count()) > 0) return x; } catch { /* closing */ } }
    return null;
  }, 30000);
  check(!!panel, "'Try again' starts the app once the problem is fixed");
  check(await until(() => a.windows().length === 1, 5000), "the error screen closes after a successful retry");
  await a.close();
}

// 1b. the saved data folder is unusable: choose another one from the error screen itself
{
  const ud = tmp("signal-lab-ud4-");
  const bad = path.join(tmp("signal-lab-blocker2-"), "file");
  fs.writeFileSync(bad, "x");
  fs.writeFileSync(path.join(ud, "config.json"), JSON.stringify({ dataDir: path.join(bad, "data") }));
  const good = tmp("signal-lab-newdata-");
  const a = await launch({ dataDir: null, userData: ud }); // no env override: the saved config decides
  const w = await a.firstWindow();
  check(await until(async () => { try { return /data folder/i.test(await w.innerText("#reason")); } catch { return false; } }, 30000), "saved unusable folder: error screen explains it");
  await a.evaluate(({ dialog }, dir) => { dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [dir] }); }, good);
  await w.click("#choose");
  const panel = await until(async () => {
    for (const x of a.windows()) { try { if ((await x.title()) === "Signal Lab" && (await x.locator("#fields input").count()) > 0) return x; } catch { /* closing */ } }
    return null;
  }, 30000);
  check(!!panel, "'Choose another data folder' starts the app with the new folder");
  check(fs.existsSync(path.join(good, "signallab.db")), "the new folder is used for the database");
  check(JSON.parse(fs.readFileSync(path.join(ud, "config.json"), "utf8")).dataDir === good, "the choice is remembered");
  await a.close();
}

// 2. a port that is already in use
{
  const busy = net.createServer();
  await new Promise((r) => busy.listen(0, "127.0.0.1", r));
  const port = busy.address().port;
  const dir = tmp("signal-lab-port-");
  const { a } = await errorScreen("port in use", { dataDir: dir, userData: tmp("signal-lab-ud3-"), env: { SIGNALLAB_ENGINE_ADDR: `127.0.0.1:${port}` } }, new RegExp(`port in use.*127\\.0\\.0\\.1:${port}`, "i"));
  check(!fs.existsSync(path.join(dir, "signallab.db")), "nothing was created on disk when the port was busy");
  await a.close();
  busy.close();
}

for (const d of [dataDir, userData]) fs.rmSync(d, { recursive: true, force: true });
console.log(failures ? `\n${failures} check(s) failed` : "\nAll checks passed");
process.exit(failures ? 1 : 0);
