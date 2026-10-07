"use strict";
// Signal Lab desktop shell.
//
// The product is the Go service. This shell does four things: starts it as a child process on a
// free loopback port with a per-launch secret token, shows its control panel in a window, offers
// a few OS-level actions (open or change the data folder), and shuts the service down cleanly so
// queued events reach the database before the app exits.

const { app, BrowserWindow, Menu, dialog, ipcMain, session, shell } = require("electron");
const { spawn } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");

const PROJECT_URL = "https://github.com/7AZEM7assan/signal-lab";
const START_TIMEOUT_MS = 20_000;
const STOP_TIMEOUT_MS = 12_000;

// Allows running as root in containers and CI, where Chromium's sandbox cannot start.
if (process.env.SIGNALLAB_NO_SANDBOX === "1") app.commandLine.appendSwitch("no-sandbox");

let mainWindow = null;
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

function startEngine() {
  return new Promise((resolve, reject) => {
    const bin = enginePath();
    if (!fs.existsSync(bin)) { reject(new Error(`The Signal Lab engine is missing: ${bin}`)); return; }
    fs.mkdirSync(dataDir(), { recursive: true });
    const token = crypto.randomBytes(32).toString("hex");
    const log = openLog();
    log.write(`\n--- ${new Date().toISOString()} starting ${bin} (data: ${dataDir()})\n`);
    const child = spawn(bin, ["app", "--data-dir", dataDir(), "--addr", "127.0.0.1:0", "--ready-json"], {
      env: { ...process.env, SIGNALLAB_APP_TOKEN: token },
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    });
    const eng = { child, token, url: "", origin: "", port: 0, exited: false };
    child.stderr.on("data", (d) => log.write(d));
    let out = "", settled = false;
    const timer = setTimeout(() => fail(new Error("The engine did not start within 20 seconds. See the log for details.")), START_TIMEOUT_MS);
    function fail(err) { if (!settled) { settled = true; clearTimeout(timer); try { child.kill(); } catch { /* gone */ } reject(err); } }
    child.stdout.on("data", (d) => {
      log.write(d);
      out += d.toString();
      for (const line of out.split("\n")) {
        if (!line.startsWith("{")) continue;
        try {
          const msg = JSON.parse(line);
          if (msg.event === "ready" && !settled) {
            settled = true; clearTimeout(timer);
            const u = new URL(msg.url);
            eng.url = msg.url; eng.origin = u.origin; eng.port = Number(u.port);
            resolve(eng);
          }
        } catch { /* partial line */ }
      }
    });
    child.on("error", (e) => fail(e));
    child.on("exit", (code, signal) => {
      eng.exited = true;
      log.write(`--- engine exited (code ${code}, signal ${signal})\n`);
      log.end();
      if (!settled) fail(new Error(`The engine exited during startup (code ${code}). See the log for details.`));
      else if (!quitting && !restarting && engine === eng) engineCrashed(code, signal);
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

async function launch() {
  try {
    restarting = true;
    engine = await startEngine();
  } catch (err) {
    restarting = false;
    const r = await dialog.showMessageBox({ type: "error", title: "Signal Lab", message: "Signal Lab could not start.", detail: String(err.message ?? err),
      buttons: ["Open log", "Quit"], defaultId: 1, cancelId: 1 });
    if (r.response === 0) shell.showItemInFolder(logPath());
    app.quit();
    return;
  }
  restarting = false;
  if (!mainWindow) createWindow();
  await mainWindow.loadURL(engine.url); // the first request sets the session cookie and redirects to the panel
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

async function chooseDataFolder() {
  const pick = await dialog.showOpenDialog(mainWindow ?? undefined, {
    title: "Choose a folder for Signal Lab's data", defaultPath: dataDir(), properties: ["openDirectory", "createDirectory"],
  });
  if (pick.canceled || !pick.filePaths[0]) return false;
  const dir = pick.filePaths[0];
  if (path.resolve(dir) === path.resolve(dataDir())) return false;
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
    if (mainWindow) { if (mainWindow.isMinimized()) mainWindow.restore(); mainWindow.focus(); }
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
