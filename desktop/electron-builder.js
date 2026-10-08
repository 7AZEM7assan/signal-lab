"use strict";
// electron-builder configuration. It is a script (not YAML) so the macOS signing options can depend
// on environment variables: see scripts/signing.js and the "Signing and notarization" section of
// desktop/README.md. Without Apple credentials the build is exactly as before (ad-hoc signed).
const { macSigning } = require("./scripts/signing");

module.exports = {
  appId: "dev.signallab.desktop",
  productName: "Signal Lab",
  copyright: "MIT License",
  directories: { output: "dist", buildResources: "build" },
  files: ["main.js", "preload.js", "preload-startup.js", "startup-error.html", "package.json"],
  // The Go engine for the platform being built; scripts/build-go.js puts it in resources/bin/<os>-<arch>.
  extraResources: [
    { from: "resources/bin/${os}-${arch}", to: "bin", filter: ["signallab*"] },
    { from: "THIRD_PARTY_NOTICES.txt", to: "THIRD_PARTY_NOTICES.txt" },
  ],
  artifactName: "Signal-Lab-${version}-${os}-${arch}.${ext}",
  asar: true,
  afterPack: "scripts/adhoc-sign.js",
  publish: null,
  linux: { executableName: "signal-lab", category: "Development", target: ["AppImage", "tar.gz"], icon: "build/icon.png" },
  win: { target: ["nsis", "zip"], icon: "build/icon.png", signAndEditExecutable: false },
  nsis: { oneClick: false, perMachine: false, allowToChangeInstallationDirectory: true },
  mac: { category: "public.app-category.developer-tools", target: ["dmg", "zip"], icon: "build/icon.png", ...macSigning(process.env) },
};
