"use strict";
// Cross-compiles the Go engine into resources/bin/<os>-<arch>/ for electron-builder.
//   node scripts/build-go.js --host     build for this machine only (development, local packaging)
//   node scripts/build-go.js --os mac   build every architecture of one OS (linux, win or mac)
//   node scripts/build-go.js --all      build every supported target (needs only a Go toolchain)
// Compiling the engine for another OS is easy; packaging the Electron app for another OS is not
// (macOS bundles in particular must be built on a Mac), so package on the OS you are targeting.
// The engine is pure Go (CGO_ENABLED=0), so no C compiler or target OS is needed.

const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const root = path.resolve(__dirname, "..", "..");
const pkg = require("../package.json");
const TARGETS = [
  { os: "linux", arch: "x64", goos: "linux", goarch: "amd64" },
  { os: "linux", arch: "arm64", goos: "linux", goarch: "arm64" },
  { os: "win", arch: "x64", goos: "windows", goarch: "amd64" },
  { os: "win", arch: "arm64", goos: "windows", goarch: "arm64" },
  { os: "mac", arch: "x64", goos: "darwin", goarch: "amd64" },
  { os: "mac", arch: "arm64", goos: "darwin", goarch: "arm64" },
];
const hostOS = { darwin: "mac", win32: "win", linux: "linux" }[process.platform];
const args = process.argv.slice(2);
const osFlag = args.indexOf("--os") >= 0 ? args[args.indexOf("--os") + 1] : null;
if (osFlag && !["linux", "win", "mac"].includes(osFlag)) { console.error("--os must be linux, win or mac"); process.exit(1); }
const targets = args.includes("--all") ? TARGETS
  : osFlag ? TARGETS.filter((t) => t.os === osFlag)
  : TARGETS.filter((t) => t.os === hostOS && t.arch === process.arch);
if (targets.length === 0) { console.error(`No Go target for ${process.platform}/${process.arch}`); process.exit(1); }

for (const t of targets) {
  const outDir = path.join(__dirname, "..", "resources", "bin", `${t.os}-${t.arch}`);
  fs.mkdirSync(outDir, { recursive: true });
  const out = path.join(outDir, t.goos === "windows" ? "signallab.exe" : "signallab");
  console.log(`go build ${t.goos}/${t.goarch} -> ${path.relative(process.cwd(), out)}`);
  const r = spawnSync("go", ["build", "-trimpath", "-ldflags", `-s -w -X main.version=${pkg.version}`, "-o", out, "./cmd/signallab"], {
    cwd: root, stdio: "inherit", env: { ...process.env, CGO_ENABLED: "0", GOOS: t.goos, GOARCH: t.goarch },
  });
  if (r.status !== 0) { console.error("go build failed"); process.exit(r.status ?? 1); }
}
