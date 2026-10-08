"use strict";
// Preload for the startup error screen only (never for the control panel). It exposes the few
// actions that screen needs; the main process checks that each call comes from that window.
const { contextBridge, ipcRenderer } = require("electron");

contextBridge.exposeInMainWorld("signalLabStartup", {
  getInfo: () => ipcRenderer.invoke("signallab:startup-info"),
  copyDetails: () => ipcRenderer.invoke("signallab:startup-copy"),
  openLogFolder: () => ipcRenderer.invoke("signallab:startup-open-log"),
  chooseFolder: () => ipcRenderer.invoke("signallab:startup-choose-folder"),
  retry: () => ipcRenderer.invoke("signallab:startup-retry"),
  quit: () => ipcRenderer.invoke("signallab:startup-quit"),
});
