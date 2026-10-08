"use strict";
// Decides how the macOS app is signed, from environment variables only.
//
//   developer-id  CSC_LINK (or CSC_NAME) names a Developer ID Application certificate AND Apple
//                 notarization credentials are set -> hardened runtime, signed, notarized.
//   adhoc         anything else -> the current behaviour: an ad-hoc signature (scripts/adhoc-sign.js),
//                 no notarization. If only part of the credentials is present, the reason says what is missing.
//
// Empty variables count as unset (CI passes empty strings for secrets that are not configured).

function isSet(env, name) {
  return typeof env[name] === "string" && env[name].trim() !== "";
}

function signingMode(env = process.env) {
  const identity = isSet(env, "CSC_LINK") || isSet(env, "CSC_NAME");
  const appleId = ["APPLE_ID", "APPLE_APP_SPECIFIC_PASSWORD", "APPLE_TEAM_ID"].every((n) => isSet(env, n));
  const apiKey = ["APPLE_API_KEY", "APPLE_API_KEY_ID", "APPLE_API_ISSUER"].every((n) => isSet(env, n));
  if (identity && (appleId || apiKey)) {
    return { mode: "developer-id", reason: `signing with the Developer ID certificate and notarizing with ${apiKey && !appleId ? "an App Store Connect API key" : "an Apple ID"}` };
  }
  if (!identity && !appleId && !apiKey) return { mode: "adhoc", reason: "no Apple credentials set: ad-hoc signed, not notarized" };
  const missing = [];
  if (!identity) missing.push("CSC_LINK or CSC_NAME (the signing certificate)");
  if (!appleId && !apiKey) missing.push("APPLE_ID + APPLE_APP_SPECIFIC_PASSWORD + APPLE_TEAM_ID, or APPLE_API_KEY + APPLE_API_KEY_ID + APPLE_API_ISSUER (notarization)");
  return { mode: "adhoc", reason: `incomplete Apple credentials, so falling back to an ad-hoc build; missing: ${missing.join("; ")}` };
}

// The electron-builder `mac` options that depend on the signing mode.
function macSigning(env = process.env) {
  const { mode, reason } = signingMode(env);
  console.log(`  • macOS signing: ${mode} (${reason})`);
  if (mode === "developer-id") {
    // identity is left unset: electron-builder takes it from CSC_LINK / CSC_NAME. `notarize: true`
    // reads APPLE_ID / APPLE_APP_SPECIFIC_PASSWORD / APPLE_TEAM_ID or the APPLE_API_* variables.
    return { hardenedRuntime: true, gatekeeperAssess: false, notarize: true };
  }
  return { identity: null, hardenedRuntime: false, notarize: false };
}

module.exports = { signingMode, macSigning };
