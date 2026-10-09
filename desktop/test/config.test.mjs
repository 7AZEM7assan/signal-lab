// Tests for the optional macOS signing and notarization switch. Run with: node --test test/
import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const root = path.join(path.dirname(fileURLToPath(import.meta.url)), "..");
const { signingMode, macSigning } = require("../scripts/signing.js");
const { block, apply } = require("../scripts/download-links.js");
import fs from "node:fs";

const CERT = { CSC_LINK: "base64-of-the-p12", CSC_KEY_PASSWORD: "x" };
const APPLE_ID = { APPLE_ID: "me@example.com", APPLE_APP_SPECIFIC_PASSWORD: "abcd-efgh-ijkl-mnop", APPLE_TEAM_ID: "ABCDE12345" };
const API_KEY = { APPLE_API_KEY: "/path/AuthKey_X.p8", APPLE_API_KEY_ID: "KEYID", APPLE_API_ISSUER: "issuer-uuid" };

test("no credentials: ad-hoc build, nothing notarized", () => {
  assert.equal(signingMode({}).mode, "adhoc");
  assert.deepEqual(macSigning({}), { identity: null, hardenedRuntime: false, notarize: false });
});

test("certificate plus Apple ID credentials: signed and notarized", () => {
  const env = { ...CERT, ...APPLE_ID };
  assert.equal(signingMode(env).mode, "developer-id");
  const mac = macSigning(env);
  assert.equal(mac.notarize, true);
  assert.equal(mac.hardenedRuntime, true);
  assert.notEqual(mac.identity, null, "identity must not be forced to null when signing");
});

test("certificate plus App Store Connect API key also works", () => {
  const env = { CSC_NAME: "Developer ID Application: Me (ABCDE12345)", ...API_KEY };
  assert.equal(signingMode(env).mode, "developer-id");
  assert.match(signingMode(env).reason, /API key/);
});

test("partial credentials never half-enable signing, and say what is missing", () => {
  const onlyCert = signingMode({ ...CERT });
  assert.equal(onlyCert.mode, "adhoc");
  assert.match(onlyCert.reason, /APPLE_ID/);
  const onlyApple = signingMode({ ...APPLE_ID });
  assert.equal(onlyApple.mode, "adhoc");
  assert.match(onlyApple.reason, /CSC_LINK/);
  assert.equal(macSigning({ ...APPLE_ID }).notarize, false);
  assert.deepEqual(macSigning({ APPLE_ID: "me@example.com", ...CERT }).notarize, false, "an Apple ID without its password and team is not enough");
});

test("empty variables (unset CI secrets) count as unset", () => {
  const env = { CSC_LINK: "", APPLE_ID: "", APPLE_APP_SPECIFIC_PASSWORD: "  ", APPLE_TEAM_ID: "" };
  assert.equal(signingMode(env).mode, "adhoc");
});

test("the real electron-builder config follows the environment", () => {
  const load = (env) => {
    const saved = { ...process.env };
    for (const k of Object.keys(process.env)) if (/^(CSC_|APPLE_)/.test(k)) delete process.env[k];
    Object.assign(process.env, env);
    delete require.cache[require.resolve(path.join(root, "electron-builder.js"))];
    try { return require(path.join(root, "electron-builder.js")); } finally {
      for (const k of Object.keys(process.env)) if (/^(CSC_|APPLE_)/.test(k)) delete process.env[k];
      Object.assign(process.env, saved);
    }
  };
  const plain = load({});
  assert.equal(plain.mac.identity, null);
  assert.equal(plain.mac.notarize, false);
  assert.equal(plain.afterPack, "scripts/adhoc-sign.js");
  const signed = load({ ...CERT, ...APPLE_ID });
  assert.equal(signed.mac.notarize, true);
  assert.equal(signed.mac.hardenedRuntime, true);
  // Everything else is identical, so signing never changes what is packaged.
  const { mac: _a, ...restPlain } = plain, { mac: _b, ...restSigned } = signed;
  assert.deepEqual(restSigned, restPlain);
  assert.ok(plain.files.includes("startup-error.html") && plain.files.includes("preload-startup.js"), "the startup error screen must be packaged");
});

test("README download links match the version in package.json", () => {
  // After raising the version, run: node scripts/download-links.js --write  (then publish the release).
  const { version } = require("../package.json");
  const text = fs.readFileSync(path.join(root, "..", "README.md"), "utf8");
  assert.ok(text.includes(block(version)), `README.md download links are not for v${version}: run node scripts/download-links.js --write`);
  assert.equal(apply(text, version), text, "applying the links again must change nothing");
  for (const name of ["mac-arm64.dmg", "mac-x64.dmg", "win-x64.exe", "win-arm64.exe", "linux-x86_64.AppImage", "linux-x64.tar.gz"]) {
    assert.ok(text.includes(`/releases/download/v${version}/Signal-Lab-${version}-${name}`), name);
  }
});
