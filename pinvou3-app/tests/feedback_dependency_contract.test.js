"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const APP_ROOT = path.resolve(__dirname, "..");
const CARGO_TOML = fs.readFileSync(
  path.join(APP_ROOT, "src-tauri", "Cargo.toml"),
  "utf8",
);
const CARGO_LOCK = fs.readFileSync(
  path.join(APP_ROOT, "src-tauri", "Cargo.lock"),
  "utf8",
);
const FEEDBACK = fs.readFileSync(
  path.join(APP_ROOT, "src-tauri", "src", "features", "feedback", "mod.rs"),
  "utf8",
);

assert.match(
  FEEDBACK,
  /md5::compute\(input\.as_bytes\(\)\)/,
  "feedback check-code protocol must continue to use MD5",
);
assert.match(
  FEEDBACK,
  /const FIXED_FEEDBACK_GW_SN: Option<&str> = Some\("[A-Za-z0-9]+"\);/,
  "feedback must keep the registered receiver serial used to obtain an upload token",
);
assert.match(
  FEEDBACK,
  /pub fn resolve_device_serial\(\) -> Option<String> \{\s*if let Some\(serial\) = FIXED_FEEDBACK_GW_SN \{\s*return Some\(serial\.to_string\(\)\);\s*\}/,
  "feedback must resolve the registered receiver serial before any client-device fallback",
);
assert.match(
  CARGO_TOML,
  /^md5\s*=\s*"0\.7"\s*$/m,
  "Cargo.toml must keep the direct md5 dependency used by feedback",
);
assert.match(
  CARGO_LOCK,
  /\[\[package\]\]\r?\nname = "md5"\r?\nversion = "0\.7\.0"/,
  "Cargo.lock must retain the md5 package",
);

const appPackage = CARGO_LOCK.match(
  /\[\[package\]\]\r?\nname = "pinvou3-tauri"[\s\S]*?dependencies = \[([\s\S]*?)\r?\n\]/,
);
assert.ok(appPackage, "Cargo.lock must contain the pinvou3-tauri package");
assert.match(
  appPackage[1],
  /^ "md5",\r?$/m,
  "pinvou3-tauri must retain md5 as a direct locked dependency",
);

console.log("feedback dependency contract ok");
