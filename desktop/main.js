"use strict";
// Signal Lab desktop shell.
//
// The product is the Go service. This shell does four things: starts it as a child process on a
// free loopback port with a per-launch secret token, shows its control panel in a window, offers
// a few OS-level actions (open or change the data folder), and shuts the service down cleanly so
// queued events reach the database before the app exits. If the service cannot start, it shows
// an error screen with the reason instead of an empty window.

const { app, BrowserWindow, Menu, clipboard, dialog, ipcMain, session, shell } = require("electron");
const { spawn } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");

const PROJECT_URL = "https://github.com/7AZEM7assan/signal-lab";
const START_TIMEOUT_MS = 20_000;
const STOP_TIMEOUT_MS = 12_000;
const TAIL_BYTES = 8 * 1024;

// Allows running as root in containers and CI, where Chromium's sandbox cannot start.
if (process.env.SIGNALLAB_NO_SANDBOX === "1") app.commandLine.appendSwitch("no-sandbox");

let mainWindow = null;
let errorWindow = null;
let startupInfo = null; // what the error screen shows
let engine = null; // { child, url, origin, port, token, exited }
let quitting = false;
let restarting = false;

const configPath = () => path.join(app.getPath("userData"), "config.json");
const logPath = () => path.join(app.getPath("userData"), "logs", "signal-lab.log");

function readConfig() {
  try { return JSON.parse(fs.readFileSync(configPath(), "utf8")); } catch { return {}; }
}
function writeConfig(cfg) {
  fs.mkdirSync(path.dirname(configPath()), { recursive: true });
  const tmp = configPath() + ".tmp";
  fs.writeFileSync(tmp, JSON.stringify(cfg, null, 2));
  fs.renameSync(tmp, configPath());
}
function dataDir() {
  return process.env.SIGNALLAB_APP_DATA || readConfig().dataDir || path.join(app.getPath("userData"), "data");
}

// The Go binary ships in resources/bin (packaged) or desktop/resources/bin/<platform>-<arch> (development).
function enginePath() {
  const exe = process.platform === "win32" ? "signallab.exe" : "signallab";
  if (process.env.SIGNALLAB_ENGINE) return process.env.SIGNALLAB_ENGINE;
  if (app.isPackaged) return path.join(process.resourcesPath, "bin", exe);
  const os = { darwin: "mac", win32: "win", linux: "linux" }[process.platform];
  return path.join(__dirname, "resources", "bin", `${os}-${process.arch}`, exe);
}

function openLog() {
  fs.mkdirSync(path.dirname(logPath()), { recursive: true });
  try { if (fs.statSync(logPath()).size > 5 * 1024 * 1024) fs.renameSync(logPath(), logPath() + ".old"); } catch { /* no log yet */ }
  return fs.createWriteStream(logPath(), { flags: "a" });
}

// Nothing the engine prints may put the access token into the log file or the error screen.
// The ready line is the channel that hands the token to this process, so it is never copied as is.
function redact(text, token) {
  let out = text.replace(/token=[^&\s"']+/g, "token=[redacted]").replace(/"token"\s*:\s*"[^"]*"/g, '"token":"[redacted]"');
  if (token) out = out.split(token).join("[redacted]");
  return out;
}

function startEngine() {
  return new Promise((resolve, reject) => {
    const bin = enginePath();
    const failure = (reason, extra = "") => Object.assign(new Error(reason), { reason, extra });
    if (!fs.existsSync(bin)) { reject(failure(`The Signal Lab engine program is missing: ${bin}`)); return; }
    const token = crypto.randomBytes(32).toString("hex");
    const log = openLog();
    const logLine = (s) => log.write(redact(s, token));
    logLine(`\n--- ${new Date().toISOString()} starting ${bin} (data: ${dataDir()})\n`);
    // SIGNALLAB_ENGINE_ADDR is a testing aid (for example to rehearse a busy port); normally the engine picks a free port.
    const addr = process.env.SIGNALLAB_ENGINE_ADDR || "127.0.0.1:0";
    const child = spawn(bin, ["app", "--data-dir", dataDir(), "--addr", addr, "--ready-json"], {
      env: { ...process.env, SIGNALLAB_APP_TOKEN: token },
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    });
    const eng = { child, token: "", url: "", origin: "", port: 0, exited: false };
    let tail = "";
    const remember = (s) => { tail = (tail + redact(s, token)).slice(-TAIL_BYTES); };
    child.stderr.on("data", (d) => { logLine(d.toString()); remember(d.toString()); });
    let out = "", settled = false;
    const timer = setTimeout(() => fail(failure("The engine did not start within 20 seconds.")), START_TIMEOUT_MS);
    function fail(err) {
      if (settled) return;
      settled = true; clearTimeout(timer);
      try { child.kill(); } catch { /* gone */ }
      err.extra = err.extra || tail;
      reject(err);
    }
    child.stdout.on("data", (d) => {
      out += d.toString();
      let nl;
      while ((nl = out.indexOf("\n")) >= 0) {
        const line = out.slice(0, nl); out = out.slice(nl + 1);
        let msg = null;
        if (line.startsWith("{")) { try { msg = JSON.parse(line); } catch { /* not JSON */ } }
        if (msg?.event === "ready" && !settled) {
          settled = true; clearTimeout(timer);
          eng.token = msg.token; eng.url = msg.url; eng.origin = new URL(msg.url).origin; eng.port = msg.port;
          logLine(`ready addr=${msg.addr} port=${msg.port}\n`); // address and port only
          resolve(eng);
        } else {
          logLine(line + "\n"); remember(line + "\n");
        }
      }
    });
    child.on("error", (e) => fail(failure(`The engine could not be started: ${e.message}`)));
    child.on("exit", (code, signal) => {
      eng.exited = true;
      logLine(`--- engine exited (code ${code}, signal ${signal})\n`);
      log.end();
      if (!settled) {
        // The engine's last words explain why ("signallab app: the data folder ... is not writable").
        const said = [...tail.matchAll(/^signallab app: (.+)$/gm)].pop();
        fail(failure(said ? said[1] : `The engine stopped during startup (exit code ${code}).`));
      } else if (!quitting && !restarting && engine === eng) engineCrashed(code, signal);
    });
  });
}

// Asks the engine to drain its queue and exit; kills it if it does not do so in time.
function stopEngine(eng) {
  return new Promise((resolve) => {
    if (!eng || eng.exited) { resolve(); return; }
    const done = () => { clearTimeout(kill); resolve(); };
    const kill = setTimeout(() => { try { eng.child.kill(); } catch { /* gone */ } }, STOP_TIMEOUT_MS);
    eng.child.once("exit", done);
    const req = http.request({ host: "127.0.0.1", port: eng.port, path: "/app/api/shutdown", method: "POST",
      headers: { "X-SignalLab-Token": eng.token } }, (res) => res.resume());
    req.on("error", () => { try { eng.child.kill(); } catch { /* gone */ } });
    req.setTimeout(3000, () => req.destroy());
    req.end();
  });
}

async function engineCrashed(code, signal) {
  const r = await dialog.showMessageBox(mainWindow ?? undefined, {
    type: "error", title: "Signal Lab", message: "The Signal Lab engine stopped unexpectedly.",
    detail: `Exit code ${code}${signal ? `, signal ${signal}` : ""}. Events that were queued but not yet saved may be lost. Your saved data is safe.`,
    buttons: ["Restart", "Open log", "Quit"], defaultId: 0, cancelId: 2,
  });
  if (r.response === 1) { shell.showItemInFolder(logPath()); }
  if (r.response === 2) { app.quit(); return; }
  await launch();
}

function isOurs(url) {
  try { return !!engine && new URL(url).origin === engine.origin; } catch { return false; }
}

function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1180, height: 820, minWidth: 820, minHeight: 560, title: "Signal Lab", show: false,
    // On a Mac the title bar is hidden and the traffic lights sit over the sidebar, like Finder and Notes.
    ...(process.platform === "darwin" ? { titleBarStyle: "hiddenInset", trafficLightPosition: { x: 18, y: 18 } } : {}),
    webPreferences: { preload: path.join(__dirname, "preload.js"), contextIsolation: true, nodeIntegration: false, sandbox: true,
      spellcheck: false },
  });
  mainWindow.once("ready-to-show", () => mainWindow.show());
  mainWindow.on("closed", () => { mainWindow = null; });
  // The panel never navigates away or opens windows; anything else is refused.
  mainWindow.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  mainWindow.webContents.on("will-navigate", (e, url) => { if (!isOurs(url)) e.preventDefault(); });
  return mainWindow;
}

// ---- startup error screen ----

function adviceFor(reason) {
  if (/port in use/i.test(reason)) return "Another program is using the port the engine was told to use. Close that program, or remove the SIGNALLAB_ENGINE_ADDR setting so Signal Lab picks a free port, then try again.";
  if (/data folder/i.test(reason)) return "Signal Lab needs a folder it can write to. Fix the folder or its permissions and try again, or choose another data folder below (your old data is not moved or deleted).";
  if (/missing/i.test(reason)) return "The installation looks incomplete. Reinstall Signal Lab.";
  return "Try again. If it keeps failing, copy the details and send them with a bug report.";
}

function showStartupError(err) {
  const reason = String(err.reason || err.message || err);
  startupInfo = {
    reason,
    advice: adviceFor(reason),
    details: [
      `Signal Lab ${app.getVersion()} on ${process.platform}/${process.arch}`,
      `Time: ${new Date().toISOString()}`,
      `Reason: ${reason}`,
      `Data folder: ${dataDir()}`,
      `Engine: ${enginePath()}`,
      `Log file: ${logPath()}`,
      "",
      "Engine output (token removed):",
      (err.extra || "").trim() || "(none)",
    ].join("\n"),
  };
  if (errorWindow) { errorWindow.webContents.reload(); errorWindow.focus(); return; } // a retry failed again: show the new reason
  errorWindow = new BrowserWindow({
    width: 760, height: 560, minWidth: 520, minHeight: 420, title: "Signal Lab could not start", show: false,
    webPreferences: { preload: path.join(__dirname, "preload-startup.js"), contextIsolation: true, nodeIntegration: false, sandbox: true, spellcheck: false },
  });
  errorWindow.once("ready-to-show", () => errorWindow.show());
  errorWindow.on("closed", () => { errorWindow = null; });
  errorWindow.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  errorWindow.webContents.on("will-navigate", (e) => e.preventDefault());
  const nonce = crypto.randomBytes(16).toString("base64");
  const html = fs.readFileSync(path.join(__dirname, "startup-error.html"), "utf8").replaceAll("__NONCE__", nonce);
  errorWindow.loadURL("data:text/html;charset=utf-8," + encodeURIComponent(html));
}
const fromErrorWindow = (event) => !!errorWindow && event.sender === errorWindow.webContents;
ipcMain.handle("signallab:startup-info", (event) => { if (!fromErrorWindow(event)) throw new Error("not allowed"); return startupInfo; });
ipcMain.handle("signallab:startup-copy", (event) => { if (!fromErrorWindow(event)) throw new Error("not allowed"); clipboard.writeText(startupInfo.details); return true; });
ipcMain.handle("signallab:startup-open-log", async (event) => {
  if (!fromErrorWindow(event)) throw new Error("not allowed");
  fs.mkdirSync(path.dirname(logPath()), { recursive: true });
  return (await shell.openPath(path.dirname(logPath()))) || "";
});
ipcMain.handle("signallab:startup-choose-folder", async (event) => {
  if (!fromErrorWindow(event)) throw new Error("not allowed");
  const dir = await pickDataFolder(errorWindow);
  if (!dir) return false;
  writeConfig({ ...readConfig(), dataDir: dir });
  await launch();
  return true;
});
ipcMain.handle("signallab:startup-retry", async (event) => { if (!fromErrorWindow(event)) throw new Error("not allowed"); await launch(); return true; });
ipcMain.handle("signallab:startup-quit", (event) => { if (!fromErrorWindow(event)) throw new Error("not allowed"); app.quit(); return true; });

async function launch() {
  try {
    restarting = true;
    engine = await startEngine();
  } catch (err) {
    restarting = false;
    engine = null;
    showStartupError(err);
    return;
  }
  restarting = false;
  if (!mainWindow) createWindow();
  await mainWindow.loadURL(`${engine.url}/?token=${engine.token}`); // the first request sets the session cookie and redirects to the panel
  if (errorWindow) errorWindow.close(); // only after the main window exists, so the app does not quit in between
}

function buildMenu() {
  const isMac = process.platform === "darwin";
  const template = [
    ...(isMac ? [{ role: "appMenu" }] : []),
    { label: "File", submenu: [
      { label: "Open data folder", click: () => shell.openPath(dataDir()) },
      { label: "Change data folder…", click: () => chooseDataFolder() },
      { type: "separator" },
      isMac ? { role: "close" } : { role: "quit" },
    ] },
    { role: "editMenu" },
    { label: "View", submenu: [
      { role: "reload" }, { type: "separator" }, { role: "resetZoom" }, { role: "zoomIn" }, { role: "zoomOut" },
      { role: "togglefullscreen" },
      ...(app.isPackaged ? [] : [{ type: "separator" }, { role: "toggleDevTools" }]),
    ] },
    { role: "windowMenu" },
    { label: "Help", submenu: [
      { label: "Project page", click: () => shell.openExternal(PROJECT_URL) },
      { label: "Show log file", click: () => shell.showItemInFolder(logPath()) },
    ] },
  ];
  Menu.setApplicationMenu(Menu.buildFromTemplate(template));
}

async function pickDataFolder(parent) {
  const pick = await dialog.showOpenDialog(parent ?? undefined, {
    title: "Choose a folder for Signal Lab's data", defaultPath: dataDir(), properties: ["openDirectory", "createDirectory"],
  });
  if (pick.canceled || !pick.filePaths[0]) return null;
  return pick.filePaths[0];
}

async function chooseDataFolder() {
  if (!engine) return false; // nothing to move while the engine is not running; the error screen has its own chooser
  const dir = await pickDataFolder(mainWindow);
  if (!dir || path.resolve(dir) === path.resolve(dataDir())) return false;
  const ok = await dialog.showMessageBox(mainWindow ?? undefined, {
    type: "question", title: "Change data folder", message: "Restart Signal Lab with this folder?",
    detail: `${dir}\n\nQueued events are saved first. Existing data in the old folder is not moved or deleted; you can switch back at any time.`,
    buttons: ["Restart with this folder", "Cancel"], defaultId: 0, cancelId: 1,
  });
  if (ok.response !== 0) return false;
  restarting = true;
  const old = engine;
  await stopEngine(old);
  writeConfig({ ...readConfig(), dataDir: dir });
  await launch();
  return true;
}

function senderIsPanel(event) {
  const url = event.senderFrame?.url || event.sender?.getURL?.() || "";
  return isOurs(url);
}
ipcMain.handle("signallab:open-data-folder", async (event) => {
  if (!senderIsPanel(event)) throw new Error("not allowed");
  const err = await shell.openPath(dataDir());
  return err || "";
});
ipcMain.handle("signallab:choose-data-folder", async (event) => {
  if (!senderIsPanel(event)) throw new Error("not allowed");
  return chooseDataFolder();
});

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", () => {
    const w = mainWindow || errorWindow;
    if (w) { if (w.isMinimized()) w.restore(); w.focus(); }
  });
  app.whenReady().then(async () => {
    app.setAboutPanelOptions({ applicationName: "Signal Lab", applicationVersion: app.getVersion(), copyright: "MIT License", website: PROJECT_URL });
    // The panel needs no browser permissions (camera, location, notifications, ...).
    session.defaultSession.setPermissionRequestHandler((_wc, _perm, cb) => cb(false));
    buildMenu();
    await launch();
  });
  app.on("window-all-closed", () => app.quit());
  app.on("before-quit", (e) => {
    if (quitting) return;
    quitting = true;
    if (engine && !engine.exited) {
      e.preventDefault();
      stopEngine(engine).finally(() => app.quit());
    }
  });
}
