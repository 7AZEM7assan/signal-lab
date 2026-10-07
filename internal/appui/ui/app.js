"use strict";
// Signal Lab control panel. No framework and no build step; talks only to its own origin.
const $ = (id) => document.getElementById(id);
const desktop = window.signalLabDesktop || null; // set by the desktop app's preload script, if present

// ---------- helpers ----------
async function api(method, path, body) {
  const opts = { method, headers: { "X-Requested-With": "signallab" }, cache: "no-store" };
  if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
  let res;
  try { res = await fetch(path, opts); } catch (e) { banner("Cannot reach the Signal Lab service. Is the app still running?"); throw e; }
  banner("");
  let data = null;
  try { data = await res.json(); } catch { /* not JSON */ }
  if (res.status === 401) { banner("This window's session is no longer valid. Reopen Signal Lab (or use the link printed at startup)."); }
  if (!res.ok) {
    const err = new Error(data?.error?.message || `HTTP ${res.status}`);
    err.status = res.status; err.code = data?.error?.code;
    throw err;
  }
  return data;
}
function banner(msg) { const b = $("banner"); b.textContent = msg; b.hidden = !msg; }
function fmtInt(n) { return n == null ? "–" : Number(n).toLocaleString(); }
function fmtBytes(n) {
  if (n == null) return "–";
  const u = ["B", "KB", "MB", "GB"]; let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
}
function fmtTime(s) { return s ? s.replace("T", " ").replace(/\.\d+Z$/, "Z").replace(/Z$/, " UTC") : "–"; }
function setPill(el, text, cls) { el.textContent = text; el.className = "pill " + (cls || ""); }
function el(tag, props, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) { if (k === "class") n.className = v; else if (k === "text") n.textContent = v; else n.setAttribute(k, v); }
  for (const k of kids) n.append(k);
  return n;
}

// ---------- tabs ----------
const TABS = ["live", "replay", "data", "settings", "storage"];
function showTab(name) {
  if (!TABS.includes(name)) name = "live";
  for (const t of TABS) {
    $("tab-" + t).hidden = t !== name;
    document.querySelector(`[data-tab="${t}"]`).setAttribute("aria-selected", String(t === name));
  }
  if (location.hash !== "#" + name) history.replaceState(null, "", "#" + name);
  if (name === "storage") refreshState();
  if (name === "settings") loadSettings();
  if (name === "data") { loadDevices(); }
}
document.querySelector(".tabs").addEventListener("click", (e) => { const t = e.target.closest("[data-tab]"); if (t) showTab(t.dataset.tab); });
window.addEventListener("hashchange", () => showTab(location.hash.slice(1)));

// ---------- Live: WebSocket feed + metrics ----------
const latest = new Map(), fired = new Map(), MAX_FIRED = 5000;
let nEvents = 0, nAlerts = 0, retryMs = 300, renderQueued = false;
function renderDevices() {
  renderQueued = false;
  const body = $("devices"); body.replaceChildren();
  if (latest.size === 0) { body.append(el("tr", {}, el("td", { colspan: 4, class: "muted", text: "Nothing yet. Start a run in the Replay tab." }))); return; }
  for (const [id, e] of [...latest].sort((a, b) => a[0].localeCompare(b[0]))) {
    const rules = fired.get(e.event_id);
    const tr = el("tr", {},
      el("td", { class: "nowrap", text: id }),
      el("td", { class: "nowrap", title: e.event_time, text: e.event_time.slice(11, 19) }),
      el("td", { class: "num" + (rules?.has("temperature_high") ? " hit" : ""), text: e.temperature_c.toFixed(2) }),
      el("td", { class: "num" + (rules?.has("vibration_high") ? " hit" : ""), text: e.vibration_mm_s.toFixed(2) }));
    body.append(tr);
  }
}
function scheduleRender() { if (!renderQueued) { renderQueued = true; requestAnimationFrame(renderDevices); } }
function addAlert(a) {
  $("noAlerts")?.remove();
  if (!fired.has(a.event_id)) fired.set(a.event_id, new Set());
  fired.get(a.event_id).add(a.rule);
  if (fired.size > MAX_FIRED) fired.delete(fired.keys().next().value);
  const li = el("li", { class: "bad", text: `${a.event_time}  ${a.device_id}  ${a.rule}  observed ${a.observed} (threshold ${a.threshold})` });
  const list = $("alerts"); list.prepend(li);
  while (list.children.length > 100) list.lastChild.remove();
  scheduleRender();
}
function connectFeed() {
  const ws = new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/ws`);
  ws.onopen = () => { retryMs = 300; setPill($("chipWs"), "feed: connected", "ok"); $("chipWs").textContent = "feed: connected"; };
  ws.onmessage = (m) => {
    let msg; try { msg = JSON.parse(m.data); } catch { return; }
    if (msg.type === "event") { nEvents++; latest.set(msg.data.device_id, msg.data); $("nEvents").textContent = fmtInt(nEvents); scheduleRender(); }
    else if (msg.type === "alert") { nAlerts++; $("nAlerts").textContent = fmtInt(nAlerts); addAlert(msg.data); }
  };
  ws.onclose = () => { setPill($("chipWs"), "feed: reconnecting", "warn"); setTimeout(connectFeed, retryMs); retryMs = Math.min(retryMs * 2, 8000); };
  ws.onerror = () => ws.close();
}
function sample(text, name, label) {
  let total = 0, found = false;
  for (const line of text.split("\n")) {
    if (!line.startsWith(name)) continue;
    const rest = line.slice(name.length);
    if (rest[0] !== "{" && rest[0] !== " ") continue;
    if (label && !rest.includes(label)) continue;
    total += parseFloat(line.slice(line.lastIndexOf(" ") + 1)); found = true;
  }
  return found ? total : null;
}
async function pollMetrics() {
  try {
    const res = await fetch("/metrics", { cache: "no-store" });
    const t = await res.text();
    const depth = sample(t, "signallab_queue_depth"), cap = sample(t, "signallab_queue_capacity");
    if (depth != null && cap) {
      $("mQueue").textContent = `${fmtInt(depth)} / ${fmtInt(cap)}`;
      const f = depth / cap;
      $("queueBar").style.width = Math.min(100, 100 * f) + "%";
      $("queueBar").style.background = f > 0.8 ? "var(--bad)" : f > 0.5 ? "var(--warn)" : "var(--ok)";
      setPill($("chipQueue"), `queue: ${fmtInt(depth)}/${fmtInt(cap)}`, f > 0.8 ? "bad" : f > 0.5 ? "warn" : "ok");
    }
    $("mAccepted").textContent = fmtInt(sample(t, "signallab_ingest_events_total", 'outcome="accepted"'));
    $("mInvalid").textContent = fmtInt(sample(t, "signallab_ingest_events_total", 'outcome="rejected_invalid"'));
    $("mOverload").textContent = fmtInt(sample(t, "signallab_ingest_events_total", 'outcome="rejected_overload"'));
    $("mProcessed").textContent = ["stored", "duplicate", "failed"].map((o) => fmtInt(sample(t, "signallab_processed_events_total", `outcome="${o}"`))).join(" / ");
  } catch { /* leave the last values */ }
}

// ---------- Replay ----------
const REPLAY_FIELDS = [
  { group: "Data to generate" },
  { key: "seed", label: "Seed", min: 0, step: 1, hint: "Same seed, same data" },
  { key: "devices", label: "Devices", min: 1, max: 200, step: 1 },
  { key: "duration_s", label: "Duration (s)", min: 1, step: 1, hint: "Simulated time" },
  { key: "interval_s", label: "Reading every (s)", min: 0.1, step: "any" },
  { key: "anomaly_rate", label: "Anomalies (%)", min: 0, max: 100, step: "any", pct: true, hint: "Chance a device starts a hot/shaky episode" },
  { key: "site_id", label: "Site id", text: true },
  { group: "Sending" },
  { key: "rate_per_s", label: "Rate (records/s)", min: 0, step: "any", hint: "0 = as fast as possible" },
  { key: "batch_size", label: "Batch size", min: 1, step: 1 },
  { key: "concurrency", label: "Connections", min: 1, max: 16, step: 1 },
  { key: "retries", label: "Retries on 429", min: 0, max: 100, step: 1 },
  { group: "Faults to inject (all optional)" },
  { key: "malformed_rate", label: "Malformed (%)", min: 0, max: 100, step: "any", pct: true, hint: "Broken records the service must reject" },
  { key: "duplicate_rate", label: "Duplicated (%)", min: 0, max: 100, step: "any", pct: true, hint: "Same event sent twice" },
  { key: "late_rate", label: "Late (%)", min: 0, max: 100, step: "any", pct: true },
  { key: "late_seconds", label: "Late by (s)", min: 0, step: "any" },
  { key: "burst_every", label: "Burst every N batches", min: 0, step: 1, hint: "0 = off" },
  { key: "burst_size", label: "Burst size (batches)", min: 0, step: 1 },
  { key: "jitter_ms", label: "Jitter (ms)", min: 0, step: "any" },
  { group: "Advanced: repeat an exact run" },
  { key: "start", label: "Start time (UTC)", text: true, hint: "Blank = ending now" },
  { key: "sequence_start", label: "First sequence", min: 0, step: 1, hint: "0 = automatic" },
];
const PRESETS = [
  { name: "Quick demo", cfg: {} },
  { name: "Fault storm", cfg: { devices: 6, duration_s: 300, interval_s: 3, malformed_rate: 0.05, duplicate_rate: 0.08, late_rate: 0.05, burst_every: 5, burst_size: 3, jitter_ms: 20 } },
  { name: "Backpressure demo", cfg: { devices: 8, duration_s: 300, interval_s: 5, rate_per_s: 80, batch_size: 10, concurrency: 2 },
    settings: { queue_capacity: 150, workers: 1, worker_batch_size: 10, lab_worker_delay_ms: 250 },
    note: "Also sets a small queue and a slow worker in Settings so the queue fills and the service answers 429." },
  { name: "Repeat exactly (idempotency)", cfg: { start: "2025-01-15T08:00:00Z", sequence_start: 1000 }, note: "Run this twice: the second run stores nothing new, because every event already exists." },
];
let replayDefaults = null;
function buildForm(container, fields, values) {
  container.replaceChildren();
  for (const f of fields) {
    if (f.group) { container.append(el("div", { class: "group", text: f.group })); continue; }
    const id = (container.id || "f") + "_" + f.key;
    const input = el("input", { id, name: f.key });
    if (f.text) input.type = "text"; else { input.type = "number"; input.step = f.step ?? "any"; if (f.min != null) input.min = f.min; if (f.max != null) input.max = f.max; input.inputMode = "decimal"; }
    const lab = el("label", { for: id }, f.label, input);
    if (f.hint) lab.append(el("span", { class: "hint", text: f.hint }));
    container.append(lab);
  }
  setFormValues(container, fields, values);
}
function setFormValues(container, fields, values) {
  for (const f of fields) {
    if (f.group) continue;
    const input = container.querySelector(`[name="${f.key}"]`);
    let v = values[f.key];
    if (v == null) v = "";
    else if (f.pct) v = +(v * 100).toFixed(4);
    input.value = v;
  }
}
function readForm(container, fields) {
  const out = {};
  for (const f of fields) {
    if (f.group) continue;
    const input = container.querySelector(`[name="${f.key}"]`);
    if (f.text) { if (input.value.trim() !== "" || f.key === "site_id") out[f.key] = input.value.trim(); continue; }
    if (input.value.trim() === "") throw new Error(`${f.label} is empty`);
    const n = Number(input.value);
    if (!Number.isFinite(n)) throw new Error(`${f.label} is not a number`);
    out[f.key] = f.pct ? n / 100 : n;
  }
  return out;
}
function updateEstimate() {
  try {
    const c = readForm($("fields"), REPLAY_FIELDS);
    const n = Math.floor(c.duration_s / c.interval_s) * c.devices;
    let t = "";
    if (c.rate_per_s > 0) t = `, about ${(n / c.rate_per_s).toFixed(1)} s to send`;
    $("estimate").textContent = Number.isFinite(n) ? `About ${fmtInt(n)} readings will be generated${t}.` : "";
  } catch { $("estimate").textContent = ""; }
}
function initReplay(defaults) {
  replayDefaults = defaults;
  buildForm($("fields"), REPLAY_FIELDS, defaults);
  $("fields").addEventListener("input", updateEstimate);
  updateEstimate();
  const box = $("presets");
  for (const p of PRESETS) {
    box.append(el("button", { type: "button", text: p.name, title: p.note || "" }));
    box.lastChild.addEventListener("click", async () => {
      $("formError").textContent = "";
      setFormValues($("fields"), REPLAY_FIELDS, { ...replayDefaults, ...p.cfg });
      updateEstimate();
      if (p.settings) {
        try { await api("PUT", "/app/api/settings", p.settings); $("formError").textContent = ""; $("estimate").textContent += " " + p.note; }
        catch (e) { $("formError").textContent = "Could not apply the preset's settings: " + e.message; }
      } else if (p.note) { $("estimate").textContent += " " + p.note; }
    });
  }
}
$("replayForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("formError").textContent = "";
  let cfg;
  try { cfg = readForm($("fields"), REPLAY_FIELDS); } catch (err) { $("formError").textContent = err.message; return; }
  try { renderReplay(await api("POST", "/app/api/replay/start", cfg)); }
  catch (err) { $("formError").textContent = err.message; }
});
$("btnStop").addEventListener("click", async () => { try { renderReplay(await api("POST", "/app/api/replay/stop")); } catch (err) { $("formError").textContent = err.message; } });

const STATE_CLS = { running: "warn", done: "ok", stopped: "warn", failed: "bad", idle: "" };
function stat(label, value) { return el("div", { class: "row" }, el("span", { text: label }), el("b", { text: value })); }
function renderReplay(s) {
  const running = s.state === "running";
  setPill($("rpState"), s.state, STATE_CLS[s.state]);
  setPill($("chipReplay"), "replay: " + s.state, STATE_CLS[s.state]);
  $("btnStart").disabled = running; $("btnStop").disabled = !running;
  const pct = s.planned ? Math.min(100, (100 * s.records_done) / s.planned) : 0;
  $("rpBar").style.width = pct + "%";
  const box = $("rpStats"); box.replaceChildren();
  if (s.state === "idle") { box.append(el("p", { class: "muted", text: "No replay has run yet." })); return; }
  const rc = Object.entries(s.rejection_counts || {}).map(([k, v]) => `${v} ${k.replaceAll("_", " ")}`).join(", ");
  const inj = s.injected || {};
  const lat = s.latency || {};
  const rows = [
    ["Sent", `${fmtInt(s.records_done)} of ${fmtInt(s.planned)} records (${pct.toFixed(0)}%), ${fmtInt(s.batches_done)}/${fmtInt(s.batches)} batches`],
    ["Elapsed", `${(s.elapsed_s || 0).toFixed(1)} s, ${fmtInt(Math.round(s.throughput_per_s || 0))} records/s`],
    ["Accepted (queued)", fmtInt(s.accepted)],
    ["Rejected as invalid", fmtInt(s.rejected) + (rc ? `: ${rc}` : "")],
    ["Told to slow down (429)", `${fmtInt(s.throttled)} times, ${fmtInt(s.retries)} retries`],
    ["Gave up after retries", `${fmtInt(s.gave_up_records)} records`],
    ["Request errors", `${fmtInt(s.request_errors)} (${fmtInt(s.errored_records)} records)`],
    ["Injected faults", `${fmtInt(inj.malformed)} malformed, ${fmtInt(inj.duplicates)} duplicated, ${fmtInt(inj.late)} late`],
    ["Request latency", lat.count ? `p50 ${lat.p50_ms} ms, p95 ${lat.p95_ms} ms, p99 ${lat.p99_ms} ms, max ${lat.max_ms} ms` : "–"],
  ];
  for (const [k, v] of rows) box.append(stat(k, v));
  if (s.state === "done" || s.state === "stopped") {
    box.append(el("p", { class: "muted small note", text: "Accepted means queued in memory, not yet stored. See the Data and Storage tabs for what was saved." }));
  }
}
async function pollReplay() {
  try { renderReplay(await api("GET", "/app/api/replay")); } catch { /* banner already shown */ }
}

// ---------- Data ----------
let cursor = null, shown = 0;
const EVENT_COLS = [["Event time (UTC)", (r) => r.event_time], ["Device", (r) => r.device_id], ["Event id", (r) => r.event_id], ["Seq", (r) => r.sequence ?? ""], ["Temp °C", (r) => r.temperature_c, true], ["Vibration mm/s", (r) => r.vibration_mm_s, true], ["Received", (r) => r.received_at]];
const ALERT_COLS = [["Event time (UTC)", (r) => r.event_time], ["Device", (r) => r.device_id], ["Rule", (r) => r.rule], ["Observed", (r) => r.observed, true], ["Threshold", (r) => r.threshold, true], ["Event id", (r) => r.event_id], ["Raised", (r) => r.created_at]];
function dataParams(withLimit) {
  const p = new URLSearchParams();
  p.set("kind", $("dKind").value);
  if ($("dDevice").value) p.set("device_id", $("dDevice").value);
  if ($("dFrom").value.trim()) p.set("from", $("dFrom").value.trim());
  if ($("dTo").value.trim()) p.set("to", $("dTo").value.trim());
  if (withLimit) p.set("limit", $("dLimit").value);
  return p;
}
function updateExportLinks() {
  const p = dataParams(false);
  for (const [id, fmt] of [["exCsv", "csv"], ["exNd", "ndjson"], ["exJson", "json"]]) {
    const q = new URLSearchParams(p); q.set("format", fmt);
    $(id).href = "/app/api/export?" + q;
  }
}
async function loadDevices() {
  try {
    const { devices } = await api("GET", "/app/api/devices");
    const sel = $("dDevice"), cur = sel.value;
    sel.replaceChildren(el("option", { value: "", text: "All devices" }));
    for (const d of devices) sel.append(el("option", { value: d.device_id, text: `${d.device_id} (${fmtInt(d.events)})` }));
    sel.value = cur;
  } catch { /* ignore */ }
  updateExportLinks();
}
async function search(more) {
  const kind = $("dKind").value;
  const cols = kind === "alerts" ? ALERT_COLS : EVENT_COLS;
  const table = $("dataTable");
  if (!more) {
    cursor = null; shown = 0;
    table.tBodies[0].replaceChildren();
    table.tHead.replaceChildren(el("tr", {}, ...cols.map(([h, , num]) => el("th", { class: num ? "num" : "", text: h }))));
  }
  const p = dataParams(true);
  if (more && cursor) p.set("cursor", cursor);
  $("dataNote").textContent = "Loading…";
  let res;
  try { res = await api("GET", "/app/api/data?" + p); } catch (e) { $("dataNote").textContent = e.message; return; }
  for (const r of res.items) table.tBodies[0].append(el("tr", {}, ...cols.map(([, get, num]) => el("td", { class: num ? "num nowrap" : "nowrap", text: String(get(r)) }))));
  shown += res.items.length; cursor = res.next_cursor || null;
  $("btnMore").hidden = !cursor;
  $("dataNote").textContent = shown === 0 ? "Nothing matches. Run a replay first, or widen the filters." : `Showing ${fmtInt(shown)} ${kind}${cursor ? " (more available)" : ""}.`;
  updateExportLinks();
}
$("dataForm").addEventListener("submit", (e) => { e.preventDefault(); search(false); });
$("btnMore").addEventListener("click", () => search(true));
for (const id of ["dKind", "dDevice", "dFrom", "dTo"]) $(id).addEventListener("change", updateExportLinks);

// ---------- Settings ----------
const SETTINGS_FIELDS = [
  { key: "temp_alert_c", label: "Temperature alert (°C)", min: 0.1, step: "any", hint: "Alert when a reading is at or above this" },
  { key: "vib_alert_mm_s", label: "Vibration alert (mm/s)", min: 0.1, step: "any", hint: "Alert when at or above this" },
  { key: "queue_capacity", label: "Queue capacity (events)", min: 1, step: 1, hint: "Full queue = 429" },
  { key: "workers", label: "Workers", min: 1, max: 64, step: 1 },
  { key: "worker_batch_size", label: "Worker batch size", min: 1, step: 1 },
  { key: "lab_worker_delay_ms", label: "Artificial worker delay (ms)", min: 0, max: 10000, step: 1, hint: "A lab knob that slows storing so you can see backpressure" },
];
let settingsDefaults = null;
async function loadSettings() {
  try {
    const { settings, defaults } = await api("GET", "/app/api/settings");
    settingsDefaults = defaults;
    if (!$("settingsFields").children.length) buildForm($("settingsFields"), SETTINGS_FIELDS, settings); else setFormValues($("settingsFields"), SETTINGS_FIELDS, settings);
  } catch { /* banner */ }
}
function settingsMsg(text, cls) { const m = $("settingsMsg"); m.textContent = text; m.className = "small " + (cls || ""); }
$("settingsForm").addEventListener("submit", async (e) => {
  e.preventDefault(); settingsMsg("");
  let body; try { body = readForm($("settingsFields"), SETTINGS_FIELDS); } catch (err) { settingsMsg(err.message, "bad"); return; }
  try {
    const r = await api("PUT", "/app/api/settings", body);
    setFormValues($("settingsFields"), SETTINGS_FIELDS, r.settings);
    settingsMsg(r.engine_rebuilt ? "Saved. The ingest engine was rebuilt." : "Saved and applied.", "ok");
  } catch (err) { settingsMsg(err.message, "bad"); }
});
$("btnDefaults").addEventListener("click", async () => {
  settingsMsg("");
  try { const r = await api("POST", "/app/api/settings/reset"); setFormValues($("settingsFields"), SETTINGS_FIELDS, r.settings); settingsMsg("Defaults restored.", "ok"); }
  catch (err) { settingsMsg(err.message, "bad"); }
});

// ---------- Storage ----------
async function refreshState() {
  let s;
  try { s = await api("GET", "/app/api/state"); } catch { return; }
  // Releases carry a number (0.2.0); a build from source reports "dev", which reads better spelled out.
  const release = /^v?\d/.test(s.version || "");
  $("version").textContent = release ? "v" + s.version.replace(/^v/, "") : s.version ? "development build" : "";
  $("aboutVersion").textContent = release ? `Version ${s.version.replace(/^v/, "")}.` : "This is a development build (built from source).";
  const st = s.storage;
  $("sEvents").textContent = fmtInt(st.events); $("sAlerts").textContent = fmtInt(st.alerts); $("sDevices").textContent = fmtInt(st.devices);
  $("sOldest").textContent = fmtTime(st.oldest_event_time); $("sNewest").textContent = fmtTime(st.newest_event_time);
  $("sSize").textContent = fmtBytes(st.db_bytes); $("sPath").textContent = s.data_dir;
  return s;
}
$("clearConfirm").addEventListener("input", () => { $("btnClear").disabled = $("clearConfirm").value !== "DELETE"; });
$("clearForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  const m = $("clearMsg"); m.textContent = ""; m.className = "small";
  try {
    const r = await api("POST", "/app/api/storage/clear", { confirm: $("clearConfirm").value });
    m.textContent = `Deleted ${fmtInt(r.deleted_events)} events and ${fmtInt(r.deleted_alerts)} alerts.`; m.className = "small ok";
    $("clearConfirm").value = ""; $("btnClear").disabled = true;
    latest.clear(); fired.clear(); nEvents = nAlerts = 0; $("nEvents").textContent = "0"; $("nAlerts").textContent = "0"; renderDevices();
    refreshState();
  } catch (err) { m.textContent = err.message; m.className = "small bad"; }
});
if (desktop) {
  $("desktopActions").hidden = false;
  $("btnOpenFolder").addEventListener("click", () => desktop.openDataFolder());
  $("btnChooseFolder").addEventListener("click", () => desktop.chooseDataFolder());
}

// ---------- boot ----------
(async function boot() {
  showTab(location.hash.slice(1));
  const s = await refreshState().catch(() => null);
  initReplay(s?.replay_defaults || {});
  if (s) renderDevices();
  connectFeed(); pollMetrics(); pollReplay();
  setInterval(pollMetrics, 2000);
  setInterval(pollReplay, 700);
  setInterval(() => { if (!$("tab-storage").hidden) refreshState(); }, 3000);
})();
