"use strict";
// electron-builder afterPack hook. Apple Silicon refuses to run an unsigned app bundle, and
// packaging modifies Electron's own signed bundle, so without a paid Developer ID certificate the
// bundle is ad-hoc signed ("-"). This does not remove Gatekeeper's first-launch prompt for a
// downloaded copy (see desktop/README.md); an app built on the user's own Mac has no such prompt.
// With a Developer ID certificate and Apple notarization credentials (see scripts/signing.js),
// electron-builder signs and notarizes instead and this hook does nothing.
const { execFileSync } = require("node:child_process");
const path = require("node:path");
const { signingMode } = require("./signing");

exports.default = async function adhocSign(context) {
  if (context.electronPlatformName !== "darwin") return;
  if (signingMode(process.env).mode === "developer-id") return; // electron-builder signs and notarizes instead
  if (process.platform !== "darwin") {
    console.warn("  • skipping ad-hoc signing: it needs macOS's codesign tool (build the macOS app on a Mac)");
    return;
  }
  const app = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  console.log(`  • ad-hoc signing ${path.basename(app)}`);
  execFileSync("codesign", ["--force", "--deep", "--sign", "-", app], { stdio: "inherit" });
};
