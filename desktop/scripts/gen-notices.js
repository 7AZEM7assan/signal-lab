"use strict";
// Regenerates THIRD_PARTY_NOTICES.txt: the licence text of every Go module linked into the engine.
// Run after changing Go dependencies:  node scripts/gen-notices.js
// (The Electron runtime and Chromium ship their own LICENSE files inside the packaged app.)
const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const root = path.resolve(__dirname, "..", "..");
const listing = execFileSync("go", ["list", "-deps", "-f", "{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}", "./cmd/signallab"], { cwd: root, encoding: "utf8" });
const mods = new Map();
for (const line of listing.split("\n")) {
  const [mod, version, dir] = line.split("|");
  if (!mod || mod === "signallab" || !dir) continue; // the project itself is covered by LICENSE
  mods.set(mod, { version, dir });
}
const out = [`Third-party software in the Signal Lab engine (Go modules linked into the "signallab" program).\n`,
  `The Go standard library and runtime (BSD-3-Clause, https://go.dev/LICENSE) are also linked in.\n`,
  `The Signal Lab desktop app also contains the Electron runtime and Chromium, whose licences are in\nLICENSE.electron.txt and LICENSES.chromium.html next to the application.\n`];
for (const [mod, { version, dir }] of [...mods].sort((a, b) => a[0].localeCompare(b[0]))) {
  const file = fs.readdirSync(dir).find((f) => /^(LICEN[CS]E|COPYING|NOTICE)/i.test(f));
  if (!file) { console.error(`no licence file found for ${mod}@${version}`); process.exit(1); }
  out.push(`\n${"=".repeat(78)}\n${mod} ${version}\n${"=".repeat(78)}\n`, fs.readFileSync(path.join(dir, file), "utf8").trim() + "\n");
}
fs.writeFileSync(path.join(__dirname, "..", "THIRD_PARTY_NOTICES.txt"), out.join("\n"));
console.log(`wrote THIRD_PARTY_NOTICES.txt for ${mods.size} modules`);
