import { lstat, readFile, realpath } from "node:fs/promises";
import { isIP } from "node:net";
import { resolve } from "node:path";
import { isDirectCliEntry } from "../cli-entrypoint.js";

const REQUIRED_KEYS = Object.freeze([
  "NODE_ENV",
  "PINVOU_DESKTOP_CONTROLLER_ENABLED",
  "PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT",
  "PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU",
  "PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID",
  "PINVOU_REMOTE_ENROLLMENT_HMAC_KEY",
  "PINVOU_REMOTE_PAIRING_HMAC_KEY_ID",
  "PINVOU_REMOTE_PAIRING_HMAC_KEY",
  "PINVOU_REMOTE_RELAY_SIGNING_KEY_ID",
  "PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64",
  "PINVOU_REMOTE_CONTROLLER_STATE_PATH",
  "PINVOU_REMOTE_STATE_PATH",
  "PINVOU_REMOTE_ALLOWED_PROXY_IPS",
  "PINVOU_REMOTE_TRUSTED_PROXY_IPS",
]);

const EXACT_VALUES = Object.freeze({
  NODE_ENV: "production",
  PINVOU_DESKTOP_CONTROLLER_ENABLED: "1",
  PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
  PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "false",
  PINVOU_REMOTE_CONTROLLER_STATE_PATH:
    "/var/lib/pinvou-remote-relay/state/controller-state.json",
  PINVOU_REMOTE_STATE_PATH: "/opt/pinvou-remote-relay/data/relay-state.json",
});

const STATE_DIRECTORIES = Object.freeze([
  { path: "/", privateDirectory: false },
  { path: "/var", privateDirectory: false },
  { path: "/var/lib", privateDirectory: false },
  { path: "/var/lib/pinvou-remote-relay", privateDirectory: true },
  { path: "/var/lib/pinvou-remote-relay/state", privateDirectory: true },
]);

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

export function parseControllerEnvironment(source) {
  const entries = new Map();
  for (const rawLine of String(source).split(/\r?\n/u)) {
    const line = rawLine.trim();
    if (line === "" || line.startsWith("#")) continue;
    const match = /^([A-Z][A-Z0-9_]*)=(.*)$/u.exec(line);
    if (!match) fail("controller_env_line_invalid");
    const [, key, value] = match;
    if (entries.has(key)) fail("controller_env_duplicate_key");
    entries.set(key, value);
  }
  return entries;
}

export function validateControllerEnvironment(entries) {
  if (!(entries instanceof Map)) fail("controller_env_entries_invalid");
  for (const key of REQUIRED_KEYS) {
    if (!entries.has(key) || entries.get(key).length === 0) {
      fail("controller_env_required_value_missing");
    }
  }
  // Match the implemented server dispatcher; official live claims are not wired.
  if (entries.get("PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT") === "official") {
    fail("official_controller_claim_dispatcher_unavailable");
  }
  for (const [key, expected] of Object.entries(EXACT_VALUES)) {
    if (entries.get(key) !== expected) fail("controller_env_required_value_invalid");
  }
  const allowed = parseProxyIps(entries.get("PINVOU_REMOTE_ALLOWED_PROXY_IPS"));
  const trusted = parseProxyIps(entries.get("PINVOU_REMOTE_TRUSTED_PROXY_IPS"));
  if (
    allowed.size !== trusted.size
    || [...allowed].some((address) => !trusted.has(address))
  ) {
    fail("controller_env_proxy_ips_mismatch");
  }
}

function parseProxyIps(value) {
  const addresses = String(value || "")
    .split(",")
    .map((address) => address.trim())
    .filter(Boolean);
  if (addresses.length === 0 || addresses.some((address) => isIP(address) === 0)) {
    fail("controller_env_proxy_ips_invalid");
  }
  return new Set(addresses);
}

export function validateControllerEnvironmentMetadata({
  isFile,
  isSymbolicLink,
  mode,
  uid,
  gid,
  expectedUid = 0,
  expectedGid = 0,
  expectedPath,
  resolvedPath,
} = {}) {
  if (!isFile || isSymbolicLink) fail("controller_env_file_type_invalid");
  if (mode !== 0o600) fail("controller_env_permissions_invalid");
  if (uid !== expectedUid || gid !== expectedGid) fail("controller_env_owner_invalid");
  if (resolvedPath !== expectedPath) fail("controller_env_path_invalid");
}

export async function validateControllerEnvironmentFile(
  filePath,
  { expectedUid = 0, expectedGid = 0 } = {},
) {
  const expectedPath = resolve(filePath);
  const metadata = await lstat(expectedPath);
  // Reject a link before resolving or reading it so the privileged deploy process
  // never follows an attacker-controlled target merely to discover it is invalid.
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    fail("controller_env_file_type_invalid");
  }
  const resolvedPath = await realpath(expectedPath);
  validateControllerEnvironmentMetadata({
    isFile: metadata.isFile(),
    isSymbolicLink: metadata.isSymbolicLink(),
    mode: metadata.mode & 0o777,
    uid: metadata.uid,
    gid: metadata.gid,
    expectedUid,
    expectedGid,
    expectedPath,
    resolvedPath,
  });
  const source = await readFile(expectedPath, "utf8");
  validateControllerEnvironment(parseControllerEnvironment(source));
}

// Check every ancestor before install -d can follow links or change ownership.
// Absent managed directories may be created only after this read-only preflight.
export async function validateControllerStateDirectories(
  directories = STATE_DIRECTORIES,
  { expectedUid = 0, expectedGid = 0 } = {},
) {
  for (const { path, privateDirectory } of directories) {
    let metadata;
    try {
      metadata = await lstat(path);
    } catch (error) {
      if (error.code === "ENOENT") continue;
      fail("controller_state_directory_read_failed");
    }
    if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
      fail("controller_state_directory_type_invalid");
    }
    if (metadata.uid !== expectedUid || metadata.gid !== expectedGid) {
      fail("controller_state_directory_owner_invalid");
    }
    const mode = metadata.mode & 0o777;
    if ((mode & 0o022) !== 0 || (privateDirectory && mode !== 0o700)) {
      fail("controller_state_directory_permissions_invalid");
    }
    if (await realpath(path) !== path) fail("controller_state_directory_path_invalid");
  }
}

if (isDirectCliEntry(import.meta.url)) {
  validateControllerEnvironmentFile(process.argv[2] || "")
    .then(() => validateControllerStateDirectories())
    .catch((error) => {
      // Never print file contents or variable values: this file contains production secrets.
      console.error(error?.code || "controller_env_preflight_failed");
      process.exitCode = 1;
    });
}
