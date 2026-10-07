"use strict";
// Exposes exactly two actions to the panel, nothing else. The panel runs with context isolation,
// no Node integration and the sandbox on; these calls go to the main process, which checks that
// the caller is the panel's own origin.
const { contextBridge, ipcRenderer } = require("electron");

contextBridge.exposeInMainWorld("signalLabDesktop", {
  openDataFolder: () => ipcRenderer.invoke("signallab:open-data-folder"),
  chooseDataFolder: () => ipcRenderer.invoke("signallab:choose-data-folder"),
});
