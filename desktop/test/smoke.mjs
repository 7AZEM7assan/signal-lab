// End-to-end smoke test of the desktop app: starts the real Electron app against the real engine,
// runs the Quick demo from the Live tab, checks the panel's main behaviours, quits, restarts on the
// same data folder and checks the data survived. It also rehearses the two startup failures (an
// unusable data folder and a busy port) and checks the access token never reaches the log file.
// On a headless Linux machine run it under a virtual display: xvfb-run -a npm run smoke
import { _electron as electron } from "playwright-core";
import { createRequire } from "node:module";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
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
check(await win.isVisible("#welcome"), "the first run shows a short welcome");
const help = await app.evaluate(({ Menu }) => Menu.getApplicationMenu().items.find((i) => i.label === "Help").submenu.items.map((i) => i.label));
check(help.some((l) => l.startsWith("Check for updates")), "the Help menu offers 'Check for updates'", help.join(" | "));
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

// Item: send to a service of your own (here a throwaway server on this computer).
{
  const seen = [];
  const own = http.createServer((req, res) => { let body = ""; req.on("data", (c) => { body += c; }); req.on("end", () => { seen.push({ url: req.url, key: req.headers["x-api-key"], token: req.headers["x-signallab-token"], body }); res.writeHead(200); res.end("{}"); }); });
  await new Promise((r) => own.listen(0, "127.0.0.1", r));
  await win.click('[data-tab="replay"]');
  check(await win.isVisible("#destBox") && await win.isVisible("#dataBox"), "Replay offers 'Where to send' and 'Data to send'");
  await win.check('#destBox input[value="own"]');
  await win.fill("#tUrl", `http://127.0.0.1:${own.address().port}/ingest?key=SECRET`);
  await win.fill("#tHeaders", "X-Api-Key: abc123");
  await win.fill('#fields [name="duration_s"]', "20");
  await win.click("#btnStart");
  check(await until(async () => (await win.innerText("#runDoneText").catch(() => "")).includes("sent to http://127.0.0.1"), 30000), "a replay to your own service finishes and says where it went", await win.innerText("#runDoneText").catch(() => ""));
  const own1 = await win.innerText("#rpStats");
  check(seen.length > 0 && seen.every((r) => r.key === "abc123" && r.token === undefined && r.url === "/ingest?key=SECRET"), "your service gets the header and the address, and never the app's token", `${seen.length} requests`);
  check(!own1.includes("SECRET") && !own1.includes("/ingest"), "the results show only the host, not the path or key");
  check(!(await win.isVisible("#runDoneView")), "no 'View in Data' for readings sent elsewhere");
  // Results: response codes as chips, latency bars, and a Copy button that puts a plain-text summary on the clipboard.
  check((await win.locator("#rpStats .codes .code.ok").count()) > 0 && (await win.locator("#rpStats .lat .lat-row").count()) === 4, "results show response-code chips and four latency bars");
  await win.click("#rpCopy");
  check(await until(async () => (await win.innerText("#rpCopyMsg")) === "Copied.", 3000), "'Copy results' reports that it copied");
  const copied = await app.evaluate(({ clipboard }) => clipboard.readText());
  check(copied.startsWith("Signal Lab replay: done") && copied.includes("Sent to: http://127.0.0.1") && !copied.includes("SECRET") && !copied.includes("abc123"), "the copied summary names the host and leaves out the key and the path", copied.split("\n")[0]);
  // "Send one test record" sends exactly one reading, with the header, and never the app's token.
  const before = seen.length;
  await win.click("#btnProbe");
  check(await until(async () => (await win.innerText("#runDoneText").catch(() => "")).startsWith("Replay done: 1 sent to http://127.0.0.1"), 20000), "'Send one test record' sends one reading and reports it", await win.innerText("#runDoneText").catch(() => ""));
  check(seen.length === before + 1 && seen.at(-1).key === "abc123" && seen.at(-1).token === undefined, "the test record carries your header and not the app's token", `${seen.length - before} request(s)`);
  // A ramp: the rate climbs from 50 to 200 records a second over 4 s, and the run is drawn second by second.
  const keep = {};
  for (const k of ["devices", "duration_s", "rate_per_s", "ramp_to_per_s", "ramp_s", "batch_size"]) keep[k] = await win.inputValue(`#fields [name="${k}"]`);
  for (const [k, v] of Object.entries({ devices: "10", duration_s: "60", rate_per_s: "50", ramp_to_per_s: "200", ramp_s: "4", batch_size: "10" })) await win.fill(`#fields [name="${k}"]`, v);
  const nRamp = seen.length;
  await win.click("#btnStart");
  check(await until(async () => (await win.innerText("#rpState")) === "done", 30000), "a ramped replay finishes");
  check(await until(() => win.isVisible("#rpChart"), 8000), "the run over time is drawn for a run of a few seconds");
  const chartText = await win.innerText("#rpChart");
  check(chartText.includes("Run over time") && /Sent up to \d/.test(chartText) && (await win.locator("#rpChart polyline").count()) === 3, "the chart has three lines and a summary", chartText.replace(/\n+/g, " | ").slice(0, 120));
  const tlJson = await win.evaluate(() => fetch("/app/api/replay/timeline").then((r) => r.json()));
  const reqs = tlJson.points.reduce((n, p) => n + p.requests, 0);
  check(reqs === 30 && seen.length === nRamp + 30 && tlJson.points.length >= 3, "the timeline counts every request (30 requests of 10 readings)", `${reqs} requests, ${tlJson.points.length} seconds`);
  // The report of that run: plain text, with no header value and no part of the address after the ?.
  check(await win.isVisible("#rpReport") && await win.isVisible("#rpReportJson"), "'Download report' is offered once a run has finished");
  const report = await win.evaluate(() => fetch("/app/api/replay/report").then((r) => r.text()));
  check(report.startsWith("# Signal Lab replay report") && report.includes("## Run over time") && !report.includes("SECRET") && !report.includes("abc123"), "the report is readable and holds no key or header", report.split("\n")[0]);
  // Scenarios: save these settings under a name, change a setting, load the scenario and get the setting back.
  await win.click("#scSaveOpen");
  await win.fill("#scName", "ramp test");
  await win.click("#scSave");
  check(await until(async () => (await win.innerText("#myScenarios")).includes("ramp test"), 5000), "a saved scenario appears as a chip", await win.innerText("#scMsg"));
  const scenarioJson = await win.evaluate(() => fetch("/app/api/scenarios").then((r) => r.text()));
  check(scenarioJson.includes("ramp_to_per_s") && !scenarioJson.includes("SECRET") && !scenarioJson.includes("abc123"), "the saved scenario holds the settings but no key or header value");
  await win.fill('#fields [name="ramp_to_per_s"]', "999");
  await win.click('#myScenarios .scUse:has-text("ramp test")');
  check((await win.inputValue('#fields [name="ramp_to_per_s"]')) === "200" && (await win.inputValue("#tUrl")).startsWith("http://127.0.0.1") && !(await win.inputValue("#tUrl")).includes("SECRET") && (await win.inputValue("#tHeaders")) === "", "loading a scenario restores the settings, without the key or headers");
  // Past runs: every finished run is listed; two can be compared side by side.
  check(await until(async () => (await win.locator("#historyTable tbody tr").count()) >= 3, 8000), "the finished runs are listed under Past runs", `${await win.locator("#historyTable tbody tr").count()} runs`);
  await win.locator("#historyTable tbody input[type=checkbox]").nth(0).check();
  await win.locator("#historyTable tbody input[type=checkbox]").nth(1).check();
  await win.click("#hCompare");
  check(await win.isVisible("#compareBox") && (await win.innerText("#compareBox")).includes("Peak accepted per second") && !(await win.innerText("#compareBox")).includes("SECRET"), "comparing two runs shows them side by side");
  for (const [k, v] of Object.entries(keep)) await win.fill(`#fields [name="${k}"]`, v);
  // A file whose columns are not recognised asks which column is which; the service then gets the right fields.
  const csvPath = path.join(tmp("signal-lab-csv-"), "werk.csv");
  fs.writeFileSync(csvPath, "Zeit,Maschine,Grad,Schwingung\n2025-01-15T08:00:00Z,press-01,61.5,2.1\n2025-01-15T08:00:02Z,pump-02,70.25,3.0\n");
  await win.check('input[name="src"][value="file"]'); // "My own file" opens the file chooser
  await win.setInputFiles("#fFile", csvPath);
  check(await until(() => win.isVisible("#mapBox"), 10000), "a file with other column names asks which column is which");
  await win.click("#mapApply");
  check((await win.innerText("#formError")).startsWith("Choose a column for"), "it will not continue until the required columns are chosen", await win.innerText("#formError"));
  for (const [field, col] of [["event_time", "Zeit"], ["device_id", "Maschine"], ["temperature_c", "Grad"], ["vibration_mm_s", "Schwingung"]]) await win.selectOption(`#map_${field}`, col);
  await win.click("#mapApply");
  check(await until(async () => (await win.innerText("#fileSummary")).includes("Zeit → event_time"), 10000), "after choosing, the summary shows the mapping", (await win.innerText("#fileSummary")).split("\n")[0]);
  check(await win.isVisible("#fMap") && !(await win.isVisible("#mapBox")), "'Change columns' is offered and the choice panel closes");
  // The data check: findings about the file are shown before anything is sent, and can be copied as text.
  check((await win.innerText("#fileSummary")).includes("Data check") && (await win.locator("#fileSummary .findings li").count()) >= 1, "the file gets a data check with at least one finding");
  await win.click("#dcCopy");
  check(await until(async () => (await win.innerText("#dcCopyMsg")).includes("Copied"), 3000), "'Copy report' reports that it copied");
  const dcText = await app.evaluate(({ clipboard }) => clipboard.readText());
  check(dcText.startsWith("Signal Lab data check: werk.csv") && dcText.includes("devices") && !dcText.includes("SECRET"), "the copied data check names the file and holds no keys", dcText.split("\n")[0]);
  const n1 = seen.length;
  await win.click("#btnStart");
  check(await until(() => seen.length > n1, 20000), "replaying the mapped file reaches your service");
  const sent = JSON.parse(seen.at(-1).body).events;
  check(sent.length === 2 && sent[0].device_id === "press-01" && sent[0].temperature_c === 61.5 && sent[1].vibration_mm_s === 3 && sent[0].event_id === "press-01-1", "the service gets Signal Lab's field names with numbers as numbers", JSON.stringify(sent[0]));
  await until(async () => (await win.innerText("#rpState")) === "done", 20000);
  // Speed follows the recorded time: the two readings are 2 s apart in the file, so with one reading per request
  // and speed 1 they reach your service about 2 s apart (and speed 0, the default, sends them at once).
  const batchBefore = await win.inputValue('#fields [name="batch_size"]');
  await win.fill('#fields [name="batch_size"]', "1");
  await win.fill('#fields [name="speed"]', "1");
  check((await win.innerText("#estimate")).includes("1× the recorded pace"), "the estimate mentions the recorded pace", await win.innerText("#estimate"));
  const nP = seen.length;
  await win.click("#btnStart");
  await until(() => seen.length >= nP + 1, 20000); const tFirst = Date.now();
  await until(() => seen.length >= nP + 2, 20000); const tSecond = Date.now();
  check(seen.length === nP + 2 && tSecond - tFirst >= 1400 && tSecond - tFirst <= 6000, "with Speed 1 the readings arrive at the recorded pace (2 s apart)", `${tSecond - tFirst} ms apart`);
  await until(async () => (await win.innerText("#rpState")) === "done", 20000);
  await win.fill('#fields [name="speed"]', "0");
  await win.fill('#fields [name="batch_size"]', batchBefore);
  await win.click("#fMap");
  check((await win.isVisible("#mapBox")) && (await win.inputValue("#map_temperature_c")) === "Grad", "'Change columns' reopens the choice with the current columns selected");
  await win.click("#mapCancel");
  await win.check('input[name="src"][value="generated"]');
  await win.check('#destBox input[value="builtin"]');
  await new Promise((r) => own.close(r));
  await win.fill('#fields [name="duration_s"]', "120"); // back to the default for the checks below
}

// Keyboard: Ctrl+2 opens Replay and Ctrl+1 goes back to Live (Command+number on a Mac does the same).
await win.keyboard.press("Control+2");
check((await win.getAttribute('[data-tab="replay"]', "aria-selected")) === "true", "Ctrl+2 switches to the Replay tab");
await win.keyboard.press("Control+1");
check((await win.getAttribute('[data-tab="live"]', "aria-selected")) === "true", "Ctrl+1 switches back to the Live tab");
check((await win.getAttribute('[data-tab="data"]', "title")).includes("(") && (await win.getAttribute('[data-tab="data"]', "aria-keyshortcuts")) !== null, "tabs show their shortcut");

const first = await stored(win);
check(first === 300, "events are stored in the embedded database", String(first));
if (shotDir) await win.screenshot({ path: path.join(shotDir, "desktop-storage.png") });
check(fs.existsSync(path.join(dataDir, "signallab.db")), "database file is created in the data folder");

await win.click('[data-tab="live"]');
await win.click("#welcomeClose");
check(!(await win.isVisible("#welcome")), "the welcome card can be dismissed");
await win.click('[data-theme-choice="dark"]');
check((await win.getAttribute("html", "data-theme")) === "dark" && (await win.getAttribute('[data-theme-choice="dark"]', "aria-pressed")) === "true", "the Appearance switch can force dark");
await sleep(400); // the choice is saved by the engine, not by the browser (the panel's address changes every launch)
await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setBounds({ x: 120, y: 100, width: 1010, height: 710 }));
await sleep(900); // the position is saved a moment after the last change
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
check(await until(async () => (await win.getAttribute("html", "data-theme")) === "dark", 8000), "the Appearance choice is remembered after a restart");
check(!(await win.isVisible("#welcome")), "the welcome card stays dismissed after a restart");
const size = await app.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].getBounds());
check(size.width === 1010 && size.height === 710, "the window comes back at the size it was left", `${size.width}x${size.height}`);
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
