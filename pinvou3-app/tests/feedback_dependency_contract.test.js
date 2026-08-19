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
  /const REGISTERED_FEEDBACK_RECEIVER_SN: &str = "219801A4BL522CM00002";/,
  "feedback must keep the exact historical receiver serial registered by the upload service",
);

const h3cUploader = FEEDBACK.match(
  /impl FeedbackUploader for H3cFeedbackUploader \{([\s\S]*?)\n\}/,
);
assert.ok(h3cUploader, "feedback must retain the H3C uploader implementation");
assert.match(
  h3cUploader[1],
  /let receiver_sn = REGISTERED_FEEDBACK_RECEIVER_SN;/,
  "feedback upload must select the registered receiver serial",
);
assert.match(
  h3cUploader[1],
  /request_upload_token\(receiver_sn\)\.await\?/,
  "feedback token requests must use the registered receiver serial",
);
assert.match(
  h3cUploader[1],
  /compute_check_code\(&token, receiver_sn\)/,
  "feedback check codes must use the registered receiver serial",
);
assert.match(
  h3cUploader[1],
  /upload_dbg_file\(&package\.dbg_path, receiver_sn, &check_code\)\.await/,
  "feedback file uploads must use the registered receiver serial",
);
assert.doesNotMatch(
  FEEDBACK,
  /PINVOU3_FEEDBACK_GW_SN|device_serial_number|resolve_device_serial/,
  "feedback must not read or fall back to a client-device serial",
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
