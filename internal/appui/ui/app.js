"use strict";
// Signal Lab control panel. No framework and no build step; talks only to its own origin.
const $ = (id) => document.getElementById(id);
const desktop = window.signalLabDesktop || null; // set by the desktop app's preload script, if present
if (desktop) document.body.classList.add("in-desktop"); // the window's title bar already says "Signal Lab"
// On a Mac the desktop window has no title bar (the traffic lights sit over the sidebar), so the page leaves room for them.
if (desktop && /Macintosh/.test(navigator.userAgent)) document.documentElement.classList.add("mac-desktop");

// ---------- appearance: follow the system, or force light or dark (kept by the engine, see /app/api/prefs) ----------
// The panel's address changes on every launch, so the browser cannot remember a choice; the engine keeps it.
function applyTheme(choice) {
  const forced = choice === "light" || choice === "dark";
  if (forced) document.documentElement.dataset.theme = choice; else delete document.documentElement.dataset.theme;
  for (const b of document.querySelectorAll("[data-theme-choice]")) b.setAttribute("aria-pressed", String(b.dataset.themeChoice === (forced ? choice : "auto")));
}
document.querySelector(".seg").addEventListener("click", (e) => {
  const b = e.target.closest("[data-theme-choice]");
  if (!b) return;
  applyTheme(b.dataset.themeChoice);
  api("PUT", "/app/api/prefs", { theme: b.dataset.themeChoice }).catch(() => { /* not remembered */ });
});
async function loadPrefs() {
  try {
    const { prefs } = await api("GET", "/app/api/prefs");
    applyTheme(prefs.theme);
    $("welcome").hidden = !!prefs.welcome_seen;
  } catch { applyTheme("auto"); }
}

// ---------- plain-language help (one sentence each; the technical names stay visible) ----------
const TIPS = {
  // Replay fields
  seed: "A number that decides the random data: the same seed always produces the same readings.",
  devices: "How many simulated machines send readings.",
  duration_s: "How many seconds of machine time to simulate; it is not how long the run takes.",
  interval_s: "Seconds between two readings from the same machine.",
  anomaly_rate: "Chance, per reading, that a machine starts a short hot or shaky episode that should trigger alerts.",
  site_id: "A label for the plant the readings come from.",
  rate_per_s: "How many readings are sent per second; 0 sends as fast as possible.",
  batch_size: "How many readings go into each request to the service.",
  concurrency: "How many requests are in flight at the same time.",
  retries: "How many times a batch is sent again after the service answers 429 (\"too busy, try again later\").",
  malformed_rate: "Share of readings deliberately broken (missing value, wrong type, bad time) to check the service rejects them.",
  duplicate_rate: "Share of readings sent twice, to check the service never stores the same event twice (idempotency).",
  late_rate: "Share of readings stamped in the past, to check late data is still accepted at its original time.",
  late_seconds: "How far into the past the late readings are stamped.",
  burst_every: "Every N-th batch starts a burst of batches sent at once; 0 turns bursts off.",
  burst_size: "How many batches are released at the same instant in a burst.",
  jitter_ms: "Random extra delay before each request, up to this many milliseconds.",
  start: "When the first reading happens (UTC); blank means the run ends now.",
  sequence_start: "Number of each machine's first reading; with the same seed and start time it resends exactly the same events.",
  // Where to send and which data
  dest: "Choose whether the readings go to this app's own service or to a service of your own, such as one you are building or testing.",
  target_url: "The address your service accepts readings at. The app sends HTTP POST requests there.",
  payload_format: "How the readings are packed into each request: all in one object, as a plain list, one per line, or one reading per request.",
  target_headers: "Extra header lines added to every request, for example an Authorization key. They are kept in memory only.",
  dataset: "Send simulated readings, or replay a CSV, NDJSON or JSON file of your own.",
  rebase_time: "Moves every timestamp forward by the same amount so the newest reading is stamped now; many services refuse old timestamps.",
  responses: "How many answers came back for each HTTP status code, such as 200 for OK or 429 for too busy.",
  firsterror: "The first request that failed, with the start of what the service answered, to help you find the cause.",
  accepted_ext: "Readings in requests that your service answered with a 2xx status, meaning it took them.",
  // Status labels
  queue: "How many readings are waiting to be saved; when the waiting line is full the service answers 429.",
  accepted: "The service checked these readings and put them in its waiting line; they are saved a moment later.",
  invalid: "Readings the service refused because they were broken, or repeated inside one batch.",
  overload: "429 means \"too busy\": the waiting line was full, so the service refused the batch and the sender tried again later.",
  throttled: "429 means \"too busy\": the service's waiting line was full, so it refused the batch and the sender waited and retried.",
  gaveup: "Readings that were still refused after every allowed retry.",
  errors: "Requests that never reached the service or got an unexpected answer.",
  injected: "Problems added on purpose: broken readings, repeated readings and late readings.",
  latency: "How long the service took to answer each request; p50 is typical, p95 and p99 are the slow cases.",
  backpressure: "Backpressure means the service slows senders down (with 429) instead of accepting more than it can save.",
  idempotency: "Idempotency means sending the same event again changes nothing: it is stored only once.",
};

function tipEl(text) {
  const s = document.createElement("span");
  s.className = "tip"; s.tabIndex = 0; s.setAttribute("role", "img");
  s.setAttribute("aria-label", text); s.dataset.tip = text;
  return s;
}
for (const t of document.querySelectorAll("[data-tip-id]")) {
  const text = TIPS[t.dataset.tipId];
  if (!text) continue;
  t.tabIndex = 0; t.setAttribute("role", "img"); t.setAttribute("aria-label", text); t.dataset.tip = text;
}
// Keep help bubbles on screen, and let Escape dismiss them (WCAG 1.4.13).
function placeTip(e) {
  const t = e.target.closest?.(".tip");
  if (!t) return;
  document.body.classList.remove("tips-off");
  t.classList.toggle("tip-left", t.getBoundingClientRect().left > window.innerWidth * 0.5);
}
document.addEventListener("mouseover", placeTip);
document.addEventListener("focusin", placeTip);
document.addEventListener("keydown", (e) => { if (e.key === "Escape") document.body.classList.add("tips-off"); });

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
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

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
// Keyboard: Command+1 to 5 on a Mac, Ctrl+1 to 5 elsewhere, switch the section.
const MOD_KEY = /Macintosh/.test(navigator.userAgent) ? "⌘" : "Ctrl+";
document.querySelectorAll(".tabs [data-tab]").forEach((b, i) => {
  b.title = `${b.textContent.trim()} (${MOD_KEY}${i + 1})`;
  b.setAttribute("aria-keyshortcuts", `${MOD_KEY === "⌘" ? "Meta" : "Control"}+${i + 1}`);
});
document.addEventListener("keydown", (e) => {
  if (!(e.metaKey || e.ctrlKey) || e.altKey || e.shiftKey) return;
  const i = "12345".indexOf(e.key);
  if (i < 0 || e.key.length !== 1) return;
  e.preventDefault();
  showTab(TABS[i]);
});
window.addEventListener("hashchange", () => showTab(location.hash.slice(1)));

// ---------- Live: WebSocket feed, trends and metrics ----------
const TREND_POINTS = 60; // readings kept per device for the sparklines
let thresholds = { temp: 85, vib: 7.1 }; // kept in sync with Settings
const latest = new Map(), fired = new Map(), trend = new Map(), MAX_FIRED = 5000;
let nEvents = 0, nAlerts = 0, retryMs = 300, renderTimer = null;

const SVGNS = "http://www.w3.org/2000/svg";
function svgEl(tag, attrs) {
  const n = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) n.setAttribute(k, v);
  return n;
}
// A small line chart of the last readings with the alert threshold as a dashed line. Readings at
// or above the threshold get a triangle marker, so the alert does not depend on colour alone.
function spark(values, threshold, what, unit, device) {
  const W = 120, H = 30, P = 4;
  const svg = svgEl("svg", { class: "spark", viewBox: `0 0 ${W} ${H}`, role: "img" });
  const lo = Math.min(threshold, ...values), hi = Math.max(threshold, ...values), span = hi - lo || 1;
  const y = (v) => P + (H - 2 * P) * (1 - (v - lo) / span);
  const offset = TREND_POINTS - values.length;
  const x = (i) => P + ((W - 2 * P) * (i + offset)) / (TREND_POINTS - 1);
  const over = values.filter((v) => v >= threshold).length;
  const label = `${what} trend for ${device}: last ${values.length} readings, latest ${values[values.length - 1]} ${unit}, ` +
    `alert threshold ${threshold} ${unit}, ${over} reading${over === 1 ? "" : "s"} at or above it`;
  svg.setAttribute("aria-label", label);
  const title = svgEl("title"); title.textContent = label; svg.append(title);
  svg.append(svgEl("line", { class: "thr", x1: 0, x2: W, y1: y(threshold), y2: y(threshold) }));
  svg.append(svgEl("polyline", { class: "line", points: values.map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(" ") }));
  values.forEach((v, i) => {
    if (v < threshold) return;
    const cx = x(i), cy = y(v);
    svg.append(svgEl("path", { class: "pt", d: `M${cx.toFixed(1)} ${(cy - 4).toFixed(1)} L${(cx + 4).toFixed(1)} ${(cy + 3.5).toFixed(1)} L${(cx - 4).toFixed(1)} ${(cy + 3.5).toFixed(1)} Z` }));
  });
  svg.append(svgEl("circle", { class: "last", cx: x(values.length - 1).toFixed(1), cy: y(values[values.length - 1]).toFixed(1), r: 2 }));
  return svg;
}

// One reading (temperature or vibration) inside a device card: label, value with unit, and its trend.
function metric(label, value, unit, hit, chart) {
  return el("div", { class: "metric" },
    el("div", {}, el("span", { class: "m-l", text: label }), el("b", { class: "m-v" + (hit ? " hit" : "") }, value, el("small", { text: unit }))),
    chart);
}
function renderDevices() {
  renderTimer = null;
  const body = $("devices"); body.replaceChildren();
  const empty = latest.size === 0;
  $("devicesEmpty").hidden = !empty;
  body.hidden = empty;
  $("trendLegend").hidden = empty;
  if (empty) return;
  $("trendLegend").textContent = `Solid line: the last ${TREND_POINTS} readings. Dashed line: the alert threshold (${thresholds.temp} °C, ${thresholds.vib} mm/s). ▲ marks a reading at or above it.`;
  for (const [id, e] of [...latest].sort((a, b) => a[0].localeCompare(b[0]))) {
    const rules = fired.get(e.event_id);
    const tHit = rules?.has("temperature_high") || e.temperature_c >= thresholds.temp;
    const vHit = rules?.has("vibration_high") || e.vibration_mm_s >= thresholds.vib;
    const h = trend.get(id) || { temp: [e.temperature_c], vib: [e.vibration_mm_s] };
    const alert = tHit || vHit;
    const status = alert
      ? el("span", { class: "badge bad", text: `▲ Alert: ${[tHit && "temperature", vHit && "vibration"].filter(Boolean).join(" and ")}` })
      : el("span", { class: "badge ok", text: "OK" });
    body.append(el("article", { class: "device" + (alert ? " alert" : "") },
      el("div", { class: "dev-head" }, el("span", { class: "dev-name", text: id }), status),
      metric("Temperature", e.temperature_c.toFixed(2), "°C", tHit, spark(h.temp, thresholds.temp, "Temperature", "°C", id)),
      metric("Vibration", e.vibration_mm_s.toFixed(2), "mm/s", vHit, spark(h.vib, thresholds.vib, "Vibration", "mm/s", id)),
      el("div", { class: "dev-foot", title: e.event_time, text: `Last reading ${e.event_time.slice(11, 19)} UTC` })));
  }
}
function scheduleRender() { if (renderTimer == null) renderTimer = setTimeout(renderDevices, 250); }
const RULE_LABEL = { temperature_high: "Temperature", vibration_high: "Vibration" };
function addAlert(a) {
  $("noAlerts")?.remove();
  $("alertsEmpty").hidden = true;
  if (!fired.has(a.event_id)) fired.set(a.event_id, new Set());
  fired.get(a.event_id).add(a.rule);
  if (fired.size > MAX_FIRED) fired.delete(fired.keys().next().value);
  const li = el("li", { class: "bad" },
    el("span", { class: "badge bad", text: `▲ ${RULE_LABEL[a.rule] || a.rule}` }),
    el("span", { class: "a-dev", text: a.device_id }),
    el("span", { class: "a-text", text: `observed ${a.observed} (threshold ${a.threshold})` }),
    el("time", { datetime: a.event_time, title: a.event_time, text: `${a.event_time.slice(11, 19)} UTC` }));
  const list = $("alerts"); list.prepend(li);
  while (list.children.length > 100) list.lastChild.remove();
  scheduleRender();
}
function resetLive() {
  latest.clear(); fired.clear(); trend.clear(); nEvents = nAlerts = 0;
  $("nEvents").textContent = "0"; $("nAlerts").textContent = "0";
  const list = $("alerts"); list.replaceChildren(el("li", { class: "empty", id: "noAlerts", text: "No alerts yet. Alerts appear when a reading reaches a threshold." }));
  $("alertsEmpty").hidden = false;
  renderDevices();
}
function onEvent(d) {
  nEvents++; latest.set(d.device_id, d);
  let h = trend.get(d.device_id);
  if (!h) { h = { temp: [], vib: [] }; trend.set(d.device_id, h); }
  h.temp.push(d.temperature_c); h.vib.push(d.vibration_mm_s);
  if (h.temp.length > TREND_POINTS) { h.temp.shift(); h.vib.shift(); }
  $("nEvents").textContent = fmtInt(nEvents); scheduleRender();
}
function connectFeed() {
  const ws = new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/ws`);
  ws.onopen = () => { retryMs = 300; setPill($("chipWs"), "feed: connected", "ok"); };
  ws.onmessage = (m) => {
    let msg; try { msg = JSON.parse(m.data); } catch { return; }
    if (msg.type === "event") onEvent(msg.data);
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

// ---------- forms (Replay and Settings share the builder) ----------
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
  { group: "Faults to inject (all optional)", collapsible: true },
  { key: "malformed_rate", label: "Malformed (%)", min: 0, max: 100, step: "any", pct: true, hint: "Broken records the service must reject" },
  { key: "duplicate_rate", label: "Duplicated (%)", min: 0, max: 100, step: "any", pct: true, hint: "Same event sent twice" },
  { key: "late_rate", label: "Late (%)", min: 0, max: 100, step: "any", pct: true },
  { key: "late_seconds", label: "Late by (s)", min: 0, step: "any" },
  { key: "burst_every", label: "Burst every N batches", min: 0, step: 1, hint: "0 = off" },
  { key: "burst_size", label: "Burst size (batches)", min: 0, step: 1 },
  { key: "jitter_ms", label: "Jitter (ms)", min: 0, step: "any" },
  { group: "Advanced: repeat an exact run", collapsible: true },
  { key: "start", label: "Start time (UTC)", text: true, hint: "Blank = ending now" },
  { key: "sequence_start", label: "First sequence", min: 0, step: 1, hint: "0 = automatic" },
];
const PRESETS = [
  { name: "Quick demo", cfg: {}, note: "Five machines for two simulated minutes, no faults." },
  { name: "Fault storm", cfg: { devices: 6, duration_s: 300, interval_s: 3, malformed_rate: 0.05, duplicate_rate: 0.08, late_rate: 0.05, burst_every: 5, burst_size: 3, jitter_ms: 20 },
    note: "Broken, repeated and late readings plus bursts, to see the service cope." },
  { name: "Backpressure demo", tip: "backpressure",
    cfg: { devices: 8, duration_s: 300, interval_s: 5, rate_per_s: 80, batch_size: 10, concurrency: 2 },
    settings: { queue_capacity: 150, workers: 1, worker_batch_size: 10, lab_worker_delay_ms: 250 },
    note: "Also sets a small queue and a slow worker in Settings so the queue fills and the service answers 429." },
  { name: "Repeat exactly (idempotency)", tip: "idempotency", cfg: { start: "2025-01-15T08:00:00Z", sequence_start: 1000 },
    note: "Run this twice: the second run stores nothing new, because every event already exists." },
];
let replayDefaults = {};

function buildForm(container, fields, values) {
  container.replaceChildren();
  let target = container;
  for (const f of fields) {
    if (f.group) {
      if (f.collapsible) {
        const inner = el("div", { class: "fields" });
        container.append(el("details", { class: "accordion" }, el("summary", { text: f.group }), inner));
        target = inner;
      } else { target = container; container.append(el("div", { class: "group", text: f.group })); }
      continue;
    }
    const id = (container.id || "f") + "_" + f.key;
    const input = el("input", { id, name: f.key });
    if (f.text) input.type = "text"; else { input.type = "number"; input.step = f.step ?? "any"; if (f.min != null) input.min = f.min; if (f.max != null) input.max = f.max; input.inputMode = "decimal"; }
    const caption = el("span", { class: "lbl", text: f.label });
    if (TIPS[f.key] && container.id === "fields") caption.append(tipEl(TIPS[f.key]));
    const lab = el("label", { for: id }, caption);
    lab.append(input);
    if (f.hint) lab.append(el("span", { class: "hint", text: f.hint }));
    target.append(lab);
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
    const fail = (msg) => { const e = new Error(msg); e.field = f.key; throw e; };
    if (f.text) { if (input.value.trim() !== "" || f.key === "site_id") out[f.key] = input.value.trim(); continue; }
    if (input.value.trim() === "") fail(`${f.label} is empty`);
    const n = Number(input.value);
    if (!Number.isFinite(n)) fail(`${f.label} is not a number`);
    out[f.key] = f.pct ? n / 100 : n;
  }
  return out;
}
// Accordions start closed; open one when it holds a value that differs from the defaults, so a
// preset's faults are never hidden.
function syncAccordions() {
  for (const d of document.querySelectorAll("#fields details.accordion")) {
    const changed = [...d.querySelectorAll("input")].some((i) => {
      const f = REPLAY_FIELDS.find((x) => x.key === i.name);
      const def = replayDefaults[i.name] ?? "";
      const shown = f?.pct && def !== "" ? +(def * 100).toFixed(4) : def;
      return String(i.value) !== String(shown);
    });
    if (changed) d.open = true;
  }
}
function revealField(key) {
  const input = $("fields").querySelector(`[name="${key}"]`);
  if (!input) return;
  const d = input.closest("details");
  if (d) d.open = true;
  input.setAttribute("aria-invalid", "true");
  input.focus();
  input.addEventListener("input", () => input.removeAttribute("aria-invalid"), { once: true });
}

// ---------- Replay ----------
function updateEstimate() {
  if (src() === "file" && dataset) {
    const n = dataset.rows;
    let t = "";
    try { const r = readForm($("fields"), REPLAY_FIELDS).rate_per_s; if (r > 0) t = `, about ${(n / r).toFixed(1)} s to send`; } catch { /* ignore */ }
    $("estimate").textContent = `${fmtInt(n)} rows from your file will be sent${t}.`;
    return;
  }
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
    const b = el("button", { type: "button", text: p.name, title: `${p.note}${p.tip ? " " + TIPS[p.tip] : ""}` });
    box.append(b);
    b.addEventListener("click", async () => {
      $("formError").textContent = "";
      setFormValues($("fields"), REPLAY_FIELDS, { ...replayDefaults, ...p.cfg });
      syncAccordions();
      updateEstimate();
      let note = p.note;
      if (p.settings) {
        try { await api("PUT", "/app/api/settings", p.settings); await loadThresholds(); }
        catch (e) { $("formError").textContent = "Could not apply the preset's settings: " + e.message; note = ""; }
      }
      $("estimate").textContent += " " + note + (p.tip ? " " + TIPS[p.tip] : "");
    });
  }
}

// ---------- Replay: where to send, and which data ----------
const dest = () => document.querySelector('input[name="dest"]:checked').value;
const src = () => document.querySelector('input[name="src"]:checked').value;
const GENERATED_KEYS = ["seed", "devices", "duration_s", "interval_s", "anomaly_rate", "site_id", "start", "sequence_start"];
let dataset = null; // summary of the imported file, or null
let replayLimits = { external_rate: 2000, dataset_bytes: 32 << 20 };

function isLocalAddress(raw) {
  try {
    const h = new URL(raw).hostname.replace(/^\[|\]$/g, "").toLowerCase();
    return h === "localhost" || h.endsWith(".localhost") || /^127\./.test(h) || h === "::1";
  } catch { return false; }
}
function syncTargetUi() {
  $("ownBox").hidden = dest() !== "own";
  const raw = $("tUrl").value.trim();
  const remote = dest() === "own" && raw !== "" && !isLocalAddress(raw);
  $("tConfirmRow").hidden = !remote;
  $("tRateNote").hidden = !remote;
  $("tRateNote").textContent = remote ? `This service is not on your computer, so the rate (under Sending) must be between 1 and ${fmtInt(replayLimits.external_rate)} records per second.` : "";
  const file = src() === "file";
  $("fileBox").hidden = !file;
  for (const key of GENERATED_KEYS) { const i = $("fields").querySelector(`[name="${key}"]`); if (i) i.disabled = file; }
  updateEstimate();
}
function fieldError(id, msg) { const e = new Error(msg); e.fieldId = id; return e; }
function parseHeaders(text) {
  const out = {};
  text.split("\n").forEach((raw, i) => {
    const line = raw.trim();
    if (!line) return;
    const k = line.indexOf(":");
    if (k < 1) throw fieldError("tHeaders", `Header line ${i + 1} should look like Name: value`);
    out[line.slice(0, k).trim()] = line.slice(k + 1).trim();
  });
  return out;
}
// The destination and data choices as replay settings. Throws a message for the field to fix.
function readTarget(withData = true) {
  const out = {};
  if (dest() === "own") {
    const url = $("tUrl").value.trim();
    if (!url) throw fieldError("tUrl", "Enter the address of your service, or choose the built-in service");
    out.target_url = url;
    out.payload_format = $("tFormat").value;
    const h = parseHeaders($("tHeaders").value);
    if (Object.keys(h).length) out.target_headers = h;
    out.target_confirmed = $("tConfirm").checked;
  }
  if (withData && src() === "file") {
    if (!dataset) throw fieldError("fFile", "Choose a file first, or switch back to simulated readings");
    out.use_dataset = true;
    out.rebase_time = $("fRebase").checked;
  }
  return out;
}
function describeDataset(d) {
  const box = $("fileSummary"); box.replaceChildren();
  $("fRemove").hidden = !d;
  $("fMap").hidden = !(d && lastFile);
  if (!d) return;
  box.append(el("div", { class: "head", text: `${fmtInt(d.rows)} rows from ${d.name} (${d.format.toUpperCase()}), ${fmtInt(d.devices)} device${d.devices === 1 ? "" : "s"}` }));
  const ul = el("ul");
  const li = (t) => ul.append(el("li", { text: t }));
  if (d.first_time) li(`Times: ${fmtTime(d.first_time)} to ${fmtTime(d.last_time)}`);
  if (d.mapped) li("Columns recognised: " + Object.entries(d.mapped).map(([k, v]) => `${k} → ${v}`).join(", "));
  if (d.ignored?.length) li("Not sent (not Signal Lab fields): " + d.ignored.join(", "));
  if (d.derived?.length) li("Filled in for you: " + d.derived.join(", "));
  const p = Object.entries(d.problems || {});
  if (p.length) li(`Signal Lab's own checks would reject ${fmtInt(p.reduce((n, [, v]) => n + v, 0))} rows (${p.map(([k, v]) => `${v} ${k.replaceAll("_", " ")}`).join(", ")}). They are sent anyway, so you can see how your service reacts.`);
  else li("Every row passes Signal Lab's own checks.");
  li("Values are sent exactly as written in the file; units are not converted.");
  box.append(ul);
}
async function uploadDataset(file, choice) {
  if (file.size > replayLimits.dataset_bytes) throw new Error(`That file is larger than ${Math.round(replayLimits.dataset_bytes / 1048576)} MB.`);
  let res;
  const query = "?name=" + encodeURIComponent(file.name) + (choice ? "&map=" + encodeURIComponent(JSON.stringify(choice)) : "");
  try {
    res = await fetch("/app/api/replay/dataset" + query, { method: "PUT", headers: { "X-Requested-With": "signallab", "Content-Type": "application/octet-stream" }, body: file, cache: "no-store" });
  } catch (e) { banner("Cannot reach the Signal Lab service. Is the app still running?"); throw e; }
  let data = null; try { data = await res.json(); } catch { /* not JSON */ }
  if (!res.ok) {
    const err = new Error(data?.error?.message || `HTTP ${res.status}`);
    err.code = data?.error?.code; err.detail = data?.error;
    throw err;
  }
  return data.dataset;
}

// ---- which column is which: shown when a file's columns are not recognised, and from "Change columns" ----
const MAP_FIELDS = [
  ["event_time", "Time", true], ["device_id", "Machine or device", true], ["temperature_c", "Temperature (°C)", true], ["vibration_mm_s", "Vibration (mm/s)", true],
  ["event_id", "Event id", false], ["sequence", "Sequence number", false], ["site_id", "Site", false],
];
let mapFile = null, mapColumnNames = [], lastFile = null; // lastFile: the file chosen in this window, kept in memory so the columns can be changed
function showMapping(file, columns, found, intro) {
  mapFile = file; mapColumnNames = columns;
  const box = $("mapFields"); box.replaceChildren();
  for (const [key, label, required] of MAP_FIELDS) {
    const sel = el("select", { id: "map_" + key, name: key });
    sel.append(el("option", { value: "", text: required ? "Choose a column…" : "(none)" }));
    for (const c of columns) sel.append(el("option", { value: c, text: c }));
    sel.value = found?.[key] && columns.includes(found[key]) ? found[key] : "";
    box.append(el("label", { for: "map_" + key }, el("span", { text: label + (required ? "" : " (optional)") }), sel));
  }
  $("mapIntro").textContent = intro;
  $("mapBox").hidden = false;
  for (const [key, , required] of MAP_FIELDS) { if (required && !$("map_" + key).value) { $("map_" + key).focus(); break; } }
}
function initTarget() {
  for (const r of document.querySelectorAll('input[name="dest"], input[name="src"]')) r.addEventListener("change", syncTargetUi);
  $("tUrl").addEventListener("input", syncTargetUi);
  $("fFile").addEventListener("change", async () => {
    const f = $("fFile").files[0];
    if (!f) return;
    $("formError").textContent = "";
    $("fileSummary").textContent = "Reading the file…";
    try {
      dataset = await uploadDataset(f);
      lastFile = f;
      $("mapBox").hidden = true;
      document.querySelector('input[name="src"][value="file"]').checked = true;
    } catch (e) {
      if (e.code === "needs_mapping") showMapping(f, e.detail.columns, e.detail.found, `Signal Lab could not tell which columns of ${f.name} are which. Choose a column for each field; nothing is changed in your file.`);
      else $("formError").textContent = e.message;
    }
    describeDataset(dataset); // the previous file stays in place when the new one is refused
    $("fFile").value = "";
    syncTargetUi();
  });
  $("mapApply").addEventListener("click", async () => {
    const choice = {}, used = new Set();
    for (const [key, label, required] of MAP_FIELDS) {
      const col = $("map_" + key).value;
      if (!col) { if (required) { $("formError").textContent = `Choose a column for ${label}.`; $("map_" + key).focus(); return; } continue; }
      if (used.has(col)) { $("formError").textContent = `The column "${col}" is chosen twice; each column can feed one field.`; return; }
      used.add(col); choice[col] = key;
    }
    for (const c of mapColumnNames) if (!(c in choice)) choice[c] = ""; // columns that were not chosen are not sent
    $("formError").textContent = "";
    try {
      dataset = await uploadDataset(mapFile, choice);
      lastFile = mapFile;
    } catch (e) { $("formError").textContent = e.message; return; }
    $("mapBox").hidden = true;
    document.querySelector('input[name="src"][value="file"]').checked = true;
    describeDataset(dataset); syncTargetUi();
  });
  $("mapCancel").addEventListener("click", () => { $("mapBox").hidden = true; $("formError").textContent = ""; });
  $("fMap").addEventListener("click", () => {
    if (dataset && lastFile) showMapping(lastFile, dataset.columns, dataset.fields, "Choose which column feeds each field. Columns you do not choose are not sent.");
  });
  $("fRemove").addEventListener("click", async () => {
    try { await api("DELETE", "/app/api/replay/dataset"); } catch (e) { $("formError").textContent = e.message; return; }
    dataset = null; describeDataset(null);
    document.querySelector('input[name="src"][value="generated"]').checked = true;
    syncTargetUi();
  });
}

// What the page needs to describe a run once it finishes: the stored-row count before it started.
let runWatch = null;
async function startReplay(cfg) {
  let base = null;
  try { const st = await api("GET", "/app/api/state"); base = { events: st.storage.events, alerts: st.storage.alerts }; } catch { /* summary falls back to "accepted" */ }
  const snap = await api("POST", "/app/api/replay/start", cfg);
  runWatch = { startedAt: snap.started_at, base, reported: false };
  hideRunDone();
  renderReplay(snap);
  return snap;
}
$("replayForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("formError").textContent = "";
  let cfg;
  try { cfg = { ...readForm($("fields"), REPLAY_FIELDS), ...readTarget() }; }
  catch (err) {
    $("formError").textContent = err.message;
    if (err.fieldId) $(err.fieldId).focus(); else if (err.field) revealField(err.field);
    return;
  }
  try { await startReplay(cfg); }
  catch (err) { $("formError").textContent = err.message; }
});
// "Send one test record": a single simulated reading to your own service, to check the address and headers.
$("btnProbe").addEventListener("click", async () => {
  $("formError").textContent = "";
  try {
    const base = readForm($("fields"), REPLAY_FIELDS);
    const target = readTarget(false);
    await startReplay({ ...base, ...target, devices: 1, duration_s: base.interval_s, anomaly_rate: 0, malformed_rate: 0, duplicate_rate: 0, late_rate: 0,
      late_seconds: 0, burst_every: 0, burst_size: 0, jitter_ms: 0, rate_per_s: 0, batch_size: 1, concurrency: 1, retries: 0 });
  } catch (err) {
    $("formError").textContent = err.message;
    if (err.fieldId) $(err.fieldId).focus(); else if (err.field) revealField(err.field);
  }
});
$("btnStop").addEventListener("click", async () => { try { renderReplay(await api("POST", "/app/api/replay/stop")); } catch (err) { $("formError").textContent = err.message; } });

// "Run quick demo" in the Live tab's empty states: the Quick demo preset, then back to Live.
async function runQuickDemo(button) {
  const buttons = document.querySelectorAll(".demoBtn");
  for (const b of buttons) b.disabled = true;
  try {
    const cfg = { ...replayDefaults };
    setFormValues($("fields"), REPLAY_FIELDS, cfg); updateEstimate();
    await startReplay(cfg);
    showTab("live");
  } catch (err) {
    banner("Could not start the demo: " + err.message);
  } finally {
    for (const b of buttons) b.disabled = false;
  }
}
for (const b of document.querySelectorAll(".demoBtn")) b.addEventListener("click", () => runQuickDemo(b));

const STATE_CLS = { running: "warn", done: "ok", stopped: "warn", failed: "bad", idle: "" };
function stat(label, value, tipKey) {
  const l = el("span", { text: label });
  if (tipKey) l.append(tipEl(TIPS[tipKey]));
  return el("div", { class: "row" }, l, typeof value === "string" ? el("b", { text: value }) : el("div", { class: "val" }, value));
}
function hideRunDone() { $("runDone").hidden = true; $("rpSummary").hidden = true; }
function showRunDone(line) {
  $("runDoneText").textContent = line;
  $("runDone").hidden = false;
  $("rpSummary").textContent = line; $("rpSummary").hidden = false;
}
// Wait until the queue is empty and the stored count stops moving, then report how many rows the run added.
async function settledStoredCount(base) {
  if (!base) return null;
  let last = -1, stable = 0;
  for (let i = 0; i < 60; i++) {
    let st;
    try { st = await api("GET", "/app/api/state"); } catch { return null; }
    const events = st.storage.events;
    if (st.engine.queue_depth === 0 && events === last) { if (++stable >= 2) return Math.max(0, events - base.events); } else stable = 0;
    last = events;
    await sleep(250);
  }
  return Math.max(0, last - base.events);
}
async function finishRun(s) {
  const w = runWatch; w.reported = true;
  $("runDoneView").hidden = !!s.external; // readings sent to your own service are not stored here
  if (s.external) {
    let line = `${fmtInt(s.records_done)} sent to ${s.target || "your service"}, ${fmtInt(s.accepted)} accepted, ${fmtInt(s.errored_records)} failed`;
    if (s.throttled > 0) line += `, answered 429 ${times(s.throttled)}`;
    if (s.gave_up_records > 0) line += `, ${fmtInt(s.gave_up_records)} gave up after retries`;
    showRunDone((s.state === "stopped" ? "Stopped early: " : "Replay done: ") + line);
    return;
  }
  const stored = await settledStoredCount(w.base);
  let line = stored == null
    ? `${fmtInt(s.records_done)} sent, ${fmtInt(s.accepted)} accepted, ${fmtInt(s.rejected)} rejected`
    : `${fmtInt(s.records_done)} sent, ${fmtInt(stored)} stored, ${fmtInt(s.rejected)} rejected`;
  const already = stored == null ? 0 : s.accepted - stored;
  if (already > 0) line += ` (${fmtInt(already)} already stored)`;
  if (s.gave_up_records > 0) line += `, ${fmtInt(s.gave_up_records)} gave up after retries`;
  showRunDone((s.state === "stopped" ? "Stopped early: " : "Replay done: ") + line);
}
function viewInData() {
  $("dKind").value = "events"; $("dDevice").value = ""; $("dFrom").value = ""; $("dTo").value = "";
  showTab("data");
  search(false);
}
$("runDoneView").addEventListener("click", viewInData);
$("runDoneClose").addEventListener("click", () => { $("runDone").hidden = true; });

function renderReplay(s) {
  const running = s.state === "running";
  setPill($("rpState"), s.state, STATE_CLS[s.state]);
  setPill($("chipReplay"), "replay: " + s.state, STATE_CLS[s.state]);
  setPill($("rpMiniState"), s.state, STATE_CLS[s.state]);
  $("btnStart").disabled = running; $("btnStop").disabled = !running;
  const pct = s.planned ? Math.min(100, (100 * s.records_done) / s.planned) : 0;
  $("rpBar").style.width = pct + "%"; $("rpMiniBar").style.width = pct + "%";
  $("rpMiniText").textContent = s.state === "idle" ? "Ready to run"
    : `${fmtInt(s.records_done)}/${fmtInt(s.planned)} sent · ${fmtInt(s.rejected)} rejected · ${fmtInt(s.throttled)} × 429`;
  if (running && !runWatch) runWatch = { startedAt: s.started_at, base: null, reported: false }; // run started before this page loaded
  if ((s.state === "done" || s.state === "stopped") && runWatch && !runWatch.reported && runWatch.startedAt === s.started_at) finishRun(s);
  const box = $("rpStats"); box.replaceChildren();
  if (s.state === "idle") { $("rpCopy").hidden = true; box.append(el("p", { class: "muted", text: "No replay has run yet." })); return; }
  const rc = Object.entries(s.rejection_counts || {}).map(([k, v]) => `${v} ${k.replaceAll("_", " ")}`).join(", ");
  const inj = s.injected || {};
  const lat = s.latency || {};
  const rows = [
    ["Sent", `${fmtInt(s.records_done)} of ${fmtInt(s.planned)} records (${pct.toFixed(0)}%), ${fmtInt(s.batches_done)}/${fmtInt(s.batches)} batches`],
    ["Sent to", s.external ? (s.target || "your service") : "this app's built-in service"],
    ["Data", s.source || "–"],
    ["Elapsed", `${(s.elapsed_s || 0).toFixed(1)} s, ${fmtInt(Math.round(s.throughput_per_s || 0))} records/s`],
    s.external ? ["Accepted (2xx answer)", fmtInt(s.accepted), "accepted_ext"] : ["Accepted (queued)", fmtInt(s.accepted), "accepted"],
    ["Rejected as invalid", fmtInt(s.rejected) + (rc ? `: ${rc}` : ""), "invalid"],
    ["Told to slow down (429)", `${times(s.throttled)}, ${retries(s.retries)}`, "throttled"],
    ["Gave up after retries", `${fmtInt(s.gave_up_records)} records`, "gaveup"],
    ["Request errors", `${fmtInt(s.request_errors)} (${fmtInt(s.errored_records)} records)`, "errors"],
  ];
  if (s.status_counts && Object.keys(s.status_counts).length) rows.push(["Responses", statusChips(s.status_counts), "responses", statusLine(s.status_counts)]);
  if (s.first_error) rows.push(["First problem", firstErrorText(s.first_error), "firsterror"]);
  rows.push(["Injected faults", `${fmtInt(inj.malformed)} malformed, ${fmtInt(inj.duplicates)} duplicated, ${fmtInt(inj.late)} late`, "injected"]);
  const latText = lat.count ? `p50 ${lat.p50_ms} ms, p95 ${lat.p95_ms} ms, p99 ${lat.p99_ms} ms, max ${lat.max_ms} ms` : "–";
  rows.push(["Request latency", lat.count ? latencyBars(lat) : "–", "latency", latText]);
  for (const [k, v, tip] of rows) box.append(stat(k, v, tip));
  lastCopy = [`Signal Lab replay: ${s.state}`, ...rows.map(([k, v, , plain]) => `${k}: ${plain ?? v}`)].join("\n");
  $("rpCopy").hidden = false;
  if (s.state === "done" || s.state === "stopped") {
    box.append(el("p", { class: "muted small note", text: s.external
      ? "These readings went to your service and were not stored here, so they do not appear in the Live or Data tabs. Check your service for what it kept."
      : "Accepted means queued in memory, not yet stored. See the Data and Storage tabs for what was saved." }));
  }
}
const times = (n) => n === 1 ? "1 time" : `${fmtInt(n)} times`;
const retries = (n) => n === 1 ? "1 retry" : `${fmtInt(n)} retries`;
const STATUS_TEXT = { 200: "OK", 201: "Created", 202: "Accepted", 204: "No Content", 301: "Moved", 302: "Found", 307: "Redirect", 308: "Redirect", 400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed", 408: "Timeout", 409: "Conflict", 413: "Too Large", 415: "Unsupported Type", 422: "Unprocessable", 429: "Too Many Requests", 500: "Server Error", 502: "Bad Gateway", 503: "Unavailable", 504: "Gateway Timeout" };
function statusLine(counts) {
  return Object.entries(counts).sort((a, b) => Number(a[0]) - Number(b[0]))
    .map(([code, n]) => `${code}${STATUS_TEXT[code] ? " " + STATUS_TEXT[code] : ""} × ${fmtInt(n)}`).join(", ");
}
// Response codes as small chips: 2xx green, 429 and redirects amber, other 4xx and 5xx red.
function statusChips(counts) {
  const box = el("div", { class: "codes" });
  for (const [code, n] of Object.entries(counts).sort((a, b) => Number(a[0]) - Number(b[0]))) {
    const c = Number(code);
    const kind = c >= 200 && c < 300 ? "ok" : c === 429 || (c >= 300 && c < 400) ? "warn" : "bad";
    box.append(el("span", { class: "code " + kind, text: `${code}${STATUS_TEXT[code] ? " " + STATUS_TEXT[code] : ""} × ${fmtInt(n)}` }));
  }
  return box;
}
// Request latency as four small bars scaled to the slowest request.
function latencyBars(l) {
  const max = Math.max(l.max_ms || 0, 0.001);
  const wrap = el("div", { class: "lat" });
  for (const [name, v] of [["p50", l.p50_ms], ["p95", l.p95_ms], ["p99", l.p99_ms], ["max", l.max_ms]]) {
    const fill = el("div", {});
    fill.style.width = Math.max(2, Math.min(100, (100 * v) / max)) + "%";
    wrap.append(el("div", { class: "lat-row" }, el("span", { class: "lat-n", text: name }), el("div", { class: "lat-bar" }, fill), el("span", { class: "lat-v", text: `${v} ms` })));
  }
  return wrap;
}
let lastCopy = "";
$("rpCopy").addEventListener("click", async () => {
  const msg = $("rpCopyMsg");
  try { await navigator.clipboard.writeText(lastCopy); msg.textContent = "Copied."; }
  catch { msg.textContent = "Could not copy. Select the results and copy them by hand."; }
  setTimeout(() => { msg.textContent = ""; }, 2500);
});
function firstErrorText(e) {
  return `request ${e.batch}: ${e.message}${e.body ? ` — “${e.body}”` : ""}`;
}
async function pollReplay() {
  try { renderReplay(await api("GET", "/app/api/replay")); } catch { /* banner already shown */ }
}

// ---------- Data ----------
let cursor = null, shown = 0, applied = new URLSearchParams(); // applied = the filters the table (and the export) use
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
// Exports use the filters of the last search, so the file always matches the table on screen.
function updateExportLinks() {
  for (const [id, fmt] of [["exCsv", "csv"], ["exJson", "json"], ["exNd", "ndjson"]]) {
    const q = new URLSearchParams(applied); q.set("format", fmt);
    $(id).href = "/app/api/export?" + q;
  }
  const what = applied.get("kind") === "alerts" ? "alerts" : "readings";
  const dirty = dataParams(false).toString() !== applied.toString();
  $("exportNote").textContent = dirty
    ? "The filters changed: press Search so the table and the export use them."
    : `Exports every ${what === "alerts" ? "alert" : "reading"} that matches the filters of the table, not only the rows shown.`;
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
  const kind = more ? applied.get("kind") : $("dKind").value;
  const cols = kind === "alerts" ? ALERT_COLS : EVENT_COLS;
  const table = $("dataTable");
  if (!more) {
    cursor = null; shown = 0; applied = dataParams(false);
    table.tBodies[0].replaceChildren();
    table.tHead.replaceChildren(el("tr", {}, ...cols.map(([h, , num]) => el("th", { class: num ? "num" : "", text: h }))));
  }
  const p = new URLSearchParams(applied); p.set("limit", $("dLimit").value);
  if (more && cursor) p.set("cursor", cursor);
  $("dataNote").textContent = "Loading…";
  let res;
  try { res = await api("GET", "/app/api/data?" + p); } catch (e) { $("dataNote").textContent = e.message; updateExportLinks(); return; }
  for (const r of res.items) table.tBodies[0].append(el("tr", {}, ...cols.map(([, get, num]) => el("td", { class: num ? "num nowrap" : "nowrap", text: String(get(r)) }))));
  shown += res.items.length; cursor = res.next_cursor || null;
  $("btnMore").hidden = !cursor;
  const noun = kind === "alerts" ? "alerts" : "readings";
  $("dataNote").textContent = shown === 0 ? "Nothing matches. Run a replay first, or widen the filters." : `Showing ${fmtInt(shown)} ${noun}${cursor ? " (more available)" : ""}.`;
  updateExportLinks();
}
$("dataForm").addEventListener("submit", (e) => { e.preventDefault(); search(false); });
$("btnMore").addEventListener("click", () => search(true));
for (const id of ["dKind", "dDevice", "dFrom", "dTo"]) { $(id).addEventListener("change", updateExportLinks); $(id).addEventListener("input", updateExportLinks); }

// ---------- Settings ----------
const SETTINGS_FIELDS = [
  { key: "temp_alert_c", label: "Temperature alert (°C)", min: 0.1, step: "any", hint: "Alert when a reading is at or above this" },
  { key: "vib_alert_mm_s", label: "Vibration alert (mm/s)", min: 0.1, step: "any", hint: "Alert when at or above this" },
  { key: "queue_capacity", label: "Queue capacity (events)", min: 1, step: 1, hint: "Full queue = 429" },
  { key: "workers", label: "Workers", min: 1, max: 64, step: 1 },
  { key: "worker_batch_size", label: "Worker batch size", min: 1, step: 1 },
  { key: "lab_worker_delay_ms", label: "Artificial worker delay (ms)", min: 0, max: 10000, step: 1, hint: "A lab knob that slows storing so you can see backpressure" },
];
function setThresholds(settings) {
  if (!settings) return;
  thresholds = { temp: settings.temp_alert_c, vib: settings.vib_alert_mm_s };
  renderDevices();
}
async function loadThresholds() {
  try { setThresholds((await api("GET", "/app/api/settings")).settings); } catch { /* keep the defaults */ }
}
async function loadSettings() {
  try {
    const { settings } = await api("GET", "/app/api/settings");
    if (!$("settingsFields").children.length) buildForm($("settingsFields"), SETTINGS_FIELDS, settings); else setFormValues($("settingsFields"), SETTINGS_FIELDS, settings);
    setThresholds(settings);
  } catch { /* banner */ }
}
function settingsMsg(text, cls) { const m = $("settingsMsg"); m.textContent = text; m.className = "small " + (cls || ""); }
$("settingsForm").addEventListener("submit", async (e) => {
  e.preventDefault(); settingsMsg("");
  let body; try { body = readForm($("settingsFields"), SETTINGS_FIELDS); } catch (err) { settingsMsg(err.message, "bad"); return; }
  try {
    const r = await api("PUT", "/app/api/settings", body);
    setFormValues($("settingsFields"), SETTINGS_FIELDS, r.settings); setThresholds(r.settings);
    settingsMsg(r.engine_rebuilt ? "Saved. The ingest engine was rebuilt." : "Saved and applied.", "ok");
  } catch (err) { settingsMsg(err.message, "bad"); }
});
$("btnDefaults").addEventListener("click", async () => {
  settingsMsg("");
  try { const r = await api("POST", "/app/api/settings/reset"); setFormValues($("settingsFields"), SETTINGS_FIELDS, r.settings); setThresholds(r.settings); settingsMsg("Defaults restored.", "ok"); }
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
    resetLive(); hideRunDone();
    refreshState();
  } catch (err) { m.textContent = err.message; m.className = "small bad"; }
});
if (desktop) {
  $("desktopActions").hidden = false;
  $("btnOpenFolder").addEventListener("click", () => desktop.openDataFolder());
  $("btnChooseFolder").addEventListener("click", () => desktop.chooseDataFolder());
}

// ---------- first run: a short welcome, shown until dismissed (the engine remembers it) ----------
$("welcomeClose").addEventListener("click", () => {
  $("welcome").hidden = true;
  api("PUT", "/app/api/prefs", { welcome_seen: true }).catch(() => { /* shown again next time */ });
});

// ---------- boot ----------
(async function boot() {
  loadPrefs();
  showTab(location.hash.slice(1));
  const s = await refreshState().catch(() => null);
  initReplay(s?.replay_defaults || {});
  if (s?.replay_limits) replayLimits = s.replay_limits;
  dataset = s?.dataset || null;
  initTarget(); describeDataset(dataset); syncTargetUi();
  applied = dataParams(false); updateExportLinks();
  await loadThresholds();
  renderDevices();
  connectFeed(); pollMetrics(); pollReplay();
  setInterval(pollMetrics, 2000);
  setInterval(pollReplay, 700);
  setInterval(() => { if (!$("tab-storage").hidden) refreshState(); }, 3000);
})();
