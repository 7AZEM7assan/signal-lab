"use strict";
// electron-builder afterPack hook. Apple Silicon refuses to run an unsigned app bundle, and
// packaging modifies Electron's own signed bundle, so without a paid Developer ID certificate the
// bundle is ad-hoc signed ("-"). This does not remove Gatekeeper's first-launch prompt for a
// downloaded copy (see desktop/README.md); an app built on the user's own Mac has no such prompt.
// If a real signing identity is configured (CSC_LINK / CSC_NAME), electron-builder signs instead.
const { execFileSync } = require("node:child_process");
const path = require("node:path");

exports.default = async function adhocSign(context) {
  if (context.electronPlatformName !== "darwin") return;
  if (process.env.CSC_LINK || process.env.CSC_NAME) return;
  if (process.platform !== "darwin") {
    console.warn("  • skipping ad-hoc signing: it needs macOS's codesign tool (build the macOS app on a Mac)");
    return;
  }
  const app = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  console.log(`  • ad-hoc signing ${path.basename(app)}`);
  execFileSync("codesign", ["--force", "--deep", "--sign", "-", app], { stdio: "inherit" });
};
