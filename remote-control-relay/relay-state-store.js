import crypto from "node:crypto";
import { execFile } from "node:child_process";
import { constants as fsConstants } from "node:fs";
import * as nodeFs from "node:fs/promises";
import { promisify } from "node:util";
import { basename, dirname, isAbsolute, join, parse, relative, resolve, sep } from "node:path";

export const RELAY_CONTROLLER_STATE_SCHEMA = "pinvou-relay-controller-state";
export const RELAY_CONTROLLER_STATE_VERSION = 1;
export const DEFAULT_MAX_RELAY_CONTROLLER_STATE_BYTES = 16 * 1024 * 1024;
export const MAX_RELAY_STATE_NAMESPACES = 32;

const NAMESPACE = /^[a-z][a-z0-9_]{0,63}$/u;
const DECIMAL = /^(?:0|[1-9][0-9]{0,19})$/u;
const U64_MAX = 18_446_744_073_709_551_615n;
const MAX_JSON_DEPTH = 32;
const MAX_JSON_STRING_BYTES = 1024 * 1024;
const REPLACE_RETRY_DELAYS_MS = Object.freeze([0, 10, 25, 50, 100]);
const LOCK_RETRY_DELAYS_MS = Object.freeze([0, 10, 25, 50, 100, 200]);
const DIRECTORY_MARKER = ".pinvou-controller-state-directory";
const DIRECTORY_MARKER_CONTENT = "pinvou-controller-state-directory-v1\n";
const LOCK_SCHEMA = "pinvou-relay-controller-state-lock";
const LOCK_VERSION = 1;
const execFileAsync = promisify(execFile);
const PROCESS_HELD_STATE_PATHS = new Set();
const ABANDONED_OPEN_STORES = new Map();

const WINDOWS_ACL_SCRIPT = `
$ErrorActionPreference = 'Stop'
$target = $env:PINVOU_ACL_TARGET
$kind = $env:PINVOU_ACL_KIND
$current = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
$system = New-Object System.Security.Principal.SecurityIdentifier('S-1-5-18')
$admins = New-Object System.Security.Principal.SecurityIdentifier('S-1-5-32-544')
$sids = @($current, $system, $admins) | Select-Object -Unique
$acl = Get-Acl -LiteralPath $target
$acl.SetAccessRuleProtection($true, $false)
foreach ($rule in @($acl.Access)) { [void]$acl.RemoveAccessRuleSpecific($rule) }
$inheritance = if ($kind -eq 'directory') {
  [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
} else { [System.Security.AccessControl.InheritanceFlags]::None }
foreach ($sid in $sids) {
  $rule = New-Object System.Security.AccessControl.FileSystemAccessRule(
    $sid,
    [System.Security.AccessControl.FileSystemRights]::FullControl,
    $inheritance,
    [System.Security.AccessControl.PropagationFlags]::None,
    [System.Security.AccessControl.AccessControlType]::Allow
  )
  [void]$acl.AddAccessRule($rule)
}
$acl.SetOwner($current)
Set-Acl -LiteralPath $target -AclObject $acl
$check = Get-Acl -LiteralPath $target
if (-not $check.AreAccessRulesProtected) { throw 'ACL inheritance remains enabled' }
if ($check.Owner -ne $current.Value -and $check.Owner -ne $current.Translate([System.Security.Principal.NTAccount]).Value) { throw 'unexpected ACL owner' }
$actual = @($check.Access)
if ($actual.Count -ne $sids.Count) { throw 'unexpected ACL entry count' }
foreach ($rule in $actual) {
  if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { throw 'deny ACL present' }
  $sid = $rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
  if ($sids.Value -notcontains $sid) { throw 'unexpected ACL principal' }
  if (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::FullControl) -ne [System.Security.AccessControl.FileSystemRights]::FullControl) { throw 'insufficient ACL rights' }
  if ($kind -eq 'directory' -and $rule.InheritanceFlags -ne [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit') { throw 'directory ACL inheritance mismatch' }
  if ($kind -eq 'file' -and $rule.InheritanceFlags -ne [System.Security.AccessControl.InheritanceFlags]::None) { throw 'file ACL inheritance mismatch' }
}
[Console]::Out.Write('ok')
`;

const WINDOWS_ACL_VERIFY_SCRIPT = `
$ErrorActionPreference = 'Stop'
$target = $env:PINVOU_ACL_TARGET
$kind = $env:PINVOU_ACL_KIND
$current = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
$allowed = @($current.Value, 'S-1-5-18', 'S-1-5-32-544') | Select-Object -Unique
$acl = Get-Acl -LiteralPath $target
if (-not $acl.AreAccessRulesProtected) { throw 'ACL inheritance enabled' }
if ($acl.Owner -ne $current.Value -and $acl.Owner -ne $current.Translate([System.Security.Principal.NTAccount]).Value) { throw 'unexpected ACL owner' }
$actual = @($acl.Access)
if ($actual.Count -ne $allowed.Count) { throw 'unexpected ACL entry count' }
foreach ($rule in $actual) {
  if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { throw 'deny ACL present' }
  $sid = $rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
  if ($allowed -notcontains $sid) { throw 'unexpected ACL principal' }
  if (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::FullControl) -ne [System.Security.AccessControl.FileSystemRights]::FullControl) { throw 'insufficient ACL rights' }
  if ($kind -eq 'directory' -and $rule.InheritanceFlags -ne [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit') { throw 'directory ACL inheritance mismatch' }
  if ($kind -eq 'file' -and $rule.InheritanceFlags -ne [System.Security.AccessControl.InheritanceFlags]::None) { throw 'file ACL inheritance mismatch' }
}
[Console]::Out.Write('ok')
`;

const WINDOWS_REPARSE_VERIFY_SCRIPT = `
$ErrorActionPreference = 'Stop'
$json = [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:PINVOU_PATHS_B64))
$paths = ConvertFrom-Json -InputObject $json
foreach ($target in $paths) {
  $item = Get-Item -LiteralPath ([string]$target) -Force
  if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'reparse point rejected' }
}
[Console]::Out.Write('ok')
`;

function encodedPowerShell(script) {
  return Buffer.from(script, "utf16le").toString("base64");
}

function windowsChildEnvironment(extra) {
  const environment = { ...extra };
  for (const key of ["SystemRoot", "WINDIR", "PATH", "PATHEXT", "TEMP", "TMP"]) {
    if (process.env[key] !== undefined) environment[key] = process.env[key];
  }
  return environment;
}

function windowsPowerShellExecutable() {
  const systemRoot = process.env.SystemRoot || process.env.WINDIR;
  if (typeof systemRoot !== "string" || !isAbsolute(systemRoot)) {
    fail("relay_state_windows_runtime_unavailable");
  }
  return join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe");
}

async function runWindowsAclScript(script, target, kind) {
  const { stdout } = await execFileAsync(windowsPowerShellExecutable(), [
    "-NoLogo", "-NoProfile", "-NonInteractive",
    "-EncodedCommand", encodedPowerShell(script),
  ], {
    windowsHide: true,
    env: windowsChildEnvironment({ PINVOU_ACL_TARGET: target, PINVOU_ACL_KIND: kind }),
    timeout: 15_000,
    maxBuffer: 64 * 1024,
  });
  if (stdout !== "ok") fail("relay_state_acl_unverified");
}

export async function verifyWindowsAncestors(paths) {
  const encodedPaths = Buffer.from(JSON.stringify(paths), "utf8").toString("base64");
  const { stdout } = await execFileAsync(windowsPowerShellExecutable(), [
    "-NoLogo", "-NoProfile", "-NonInteractive",
    "-EncodedCommand", encodedPowerShell(WINDOWS_REPARSE_VERIFY_SCRIPT),
  ], {
    windowsHide: true,
    env: windowsChildEnvironment({ PINVOU_PATHS_B64: encodedPaths }),
    timeout: 15_000,
    maxBuffer: 64 * 1024,
  });
  if (stdout !== "ok") fail("relay_state_path_component_unsafe");
}

export async function protectWindowsPath(target, kind) {
  await runWindowsAclScript(WINDOWS_ACL_SCRIPT, target, kind);
}

export async function verifyWindowsPath(target, kind) {
  await runWindowsAclScript(WINDOWS_ACL_VERIFY_SCRIPT, target, kind);
}

async function defaultProcessIdentity(pid) {
  if (!Number.isSafeInteger(pid) || pid <= 0) return null;
  if (process.platform === "win32") {
    const script = `
$ErrorActionPreference = 'Stop'
$pidValue = [int]$env:PINVOU_LOCK_PID
$process = Get-Process -Id $pidValue -ErrorAction Stop
$os = Get-CimInstance Win32_OperatingSystem
[Console]::Out.Write(($os.LastBootUpTime.ToUniversalTime().Ticks.ToString() + ':' + $process.StartTime.ToUniversalTime().Ticks.ToString()))
`;
    try {
      const { stdout } = await execFileAsync(windowsPowerShellExecutable(), [
        "-NoLogo", "-NoProfile", "-NonInteractive",
        "-EncodedCommand", encodedPowerShell(script),
      ], {
        windowsHide: true,
        env: windowsChildEnvironment({ PINVOU_LOCK_PID: String(pid) }),
        timeout: 10_000,
        maxBuffer: 4096,
      });
      return /^[0-9]+:[0-9]+$/u.test(stdout) ? stdout : null;
    } catch {
      return null;
    }
  }
  try {
    const bootId = (await nodeFs.readFile("/proc/sys/kernel/random/boot_id", "utf8")).trim();
    const stat = await nodeFs.readFile(`/proc/${pid}/stat`, "utf8");
    const closing = stat.lastIndexOf(")");
    const fields = stat.slice(closing + 2).split(" ");
    const startTicks = fields[19];
    return bootId && /^[0-9]+$/u.test(startTicks) ? `${bootId}:${startTicks}` : null;
  } catch {
    try {
      const [{ stdout: processStart }, { stdout: bootIdentity }] = await Promise.all([
        execFileAsync("ps", ["-o", "lstart=", "-p", String(pid)], { timeout: 5000, maxBuffer: 4096 }),
        execFileAsync("sysctl", ["-n", "kern.boottime"], { timeout: 5000, maxBuffer: 4096 }),
      ]);
      const start = processStart.trim();
      const boot = bootIdentity.trim();
      return start && boot ? `sysctl:${boot}:${start}` : null;
    } catch {
      return null;
    }
  }
}

export class RelayStateStoreError extends Error {
  constructor(code, message = code, options) {
    super(message, options);
    this.name = "RelayStateStoreError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new RelayStateStoreError(code, code, cause ? { cause } : undefined);
}

function exactObject(value, required, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(`${label}_malformed`);
  const keys = Object.keys(value);
  if (keys.length !== required.length || required.some((key) => !Object.hasOwn(value, key))) {
    fail(`${label}_malformed`);
  }
  return value;
}

function decimal(value, label) {
  if (typeof value !== "string" || !DECIMAL.test(value)) fail(`${label}_malformed`);
  const parsed = BigInt(value);
  if (parsed > U64_MAX) fail(`${label}_malformed`);
  return parsed;
}

function nextDecimal(value) {
  const next = decimal(value, "state_revision") + 1n;
  if (next > U64_MAX) fail("state_revision_exhausted");
  return next.toString();
}

function validateJsonValue(value, depth = 0, seen = new Set()) {
  if (depth > MAX_JSON_DEPTH) fail("namespace_value_malformed");
  if (value === null || typeof value === "boolean") return;
  if (typeof value === "string") {
    if (Buffer.byteLength(value) > MAX_JSON_STRING_BYTES) fail("namespace_value_malformed");
    return;
  }
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value)) fail("namespace_value_malformed");
    return;
  }
  if (typeof value !== "object" || seen.has(value)) fail("namespace_value_malformed");
  seen.add(value);
  if (Array.isArray(value)) {
    for (const item of value) validateJsonValue(item, depth + 1, seen);
  } else {
    const prototype = Object.getPrototypeOf(value);
    if (prototype !== Object.prototype && prototype !== null) fail("namespace_value_malformed");
    for (const [key, item] of Object.entries(value)) {
      if (Buffer.byteLength(key) > 256 || ["__proto__", "prototype", "constructor"].includes(key)) {
        fail("namespace_value_malformed");
      }
      validateJsonValue(item, depth + 1, seen);
    }
  }
  seen.delete(value);
}

function canonicalJson(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function cloneJson(value) {
  return value === null ? null : JSON.parse(JSON.stringify(value));
}

function emptyState() {
  return {
    schema: RELAY_CONTROLLER_STATE_SCHEMA,
    version: RELAY_CONTROLLER_STATE_VERSION,
    revision: "0",
    namespaces: {},
  };
}

function validateNamespaceRecord(value, name) {
  const record = exactObject(value, ["revision", "value"], "namespace_record");
  decimal(record.revision, "namespace_revision");
  validateJsonValue(record.value);
  return [name, { revision: record.revision, value: cloneJson(record.value) }];
}

function validateCurrentState(value) {
  const state = exactObject(value, ["schema", "version", "revision", "namespaces"], "relay_state");
  if (state.schema !== RELAY_CONTROLLER_STATE_SCHEMA || state.version !== RELAY_CONTROLLER_STATE_VERSION) {
    fail("relay_state_unsupported");
  }
  const stateRevision = decimal(state.revision, "state_revision");
  if (!state.namespaces || typeof state.namespaces !== "object" || Array.isArray(state.namespaces)) {
    fail("relay_state_malformed");
  }
  const names = Object.keys(state.namespaces);
  if (names.length > MAX_RELAY_STATE_NAMESPACES) fail("relay_state_capacity_reached");
  const namespaces = {};
  for (const name of names.sort()) {
    if (!NAMESPACE.test(name)) fail("namespace_name_invalid");
    const [validatedName, record] = validateNamespaceRecord(state.namespaces[name], name);
    if (decimal(record.revision, "namespace_revision") > stateRevision) fail("relay_state_malformed");
    namespaces[validatedName] = record;
  }
  return { ...state, namespaces };
}

function migrateLegacyState(value) {
  const state = exactObject(value, ["schema", "version", "revision", "namespaces"], "relay_state");
  if (state.schema !== RELAY_CONTROLLER_STATE_SCHEMA || state.version !== 0
    || !Number.isSafeInteger(state.revision) || state.revision < 0
    || !Array.isArray(state.namespaces) || state.namespaces.length > MAX_RELAY_STATE_NAMESPACES) {
    fail("relay_state_unsupported");
  }
  const namespaces = {};
  for (const raw of state.namespaces) {
    const record = exactObject(raw, ["name", "revision", "value"], "legacy_namespace_record");
    if (!NAMESPACE.test(record.name) || Object.hasOwn(namespaces, record.name)
      || !Number.isSafeInteger(record.revision) || record.revision < 0) {
      fail("relay_state_malformed");
    }
    validateJsonValue(record.value);
    namespaces[record.name] = { revision: String(record.revision), value: cloneJson(record.value) };
  }
  return validateCurrentState({
    schema: RELAY_CONTROLLER_STATE_SCHEMA,
    version: RELAY_CONTROLLER_STATE_VERSION,
    revision: String(state.revision),
    namespaces,
  });
}

function parseState(contents) {
  let value;
  try {
    value = JSON.parse(contents);
  } catch (error) {
    fail("relay_state_invalid_json", error);
  }
  if (value?.schema === RELAY_CONTROLLER_STATE_SCHEMA && value?.version === 0) {
    return { state: migrateLegacyState(value), migrated: true };
  }
  return { state: validateCurrentState(value), migrated: false };
}

function serializeState(state, maxBytes) {
  const contents = `${canonicalJson(validateCurrentState(state))}\n`;
  if (Buffer.byteLength(contents) > maxBytes) fail("relay_state_capacity_reached");
  return contents;
}

function safeStatePath(input) {
  if (typeof input !== "string" || !isAbsolute(input)) fail("relay_state_path_invalid");
  const path = resolve(input);
  if (path === resolve(dirname(path))) fail("relay_state_path_invalid");
  return path;
}

function comparablePath(path) {
  return process.platform === "win32" ? path.toLocaleLowerCase("en-US") : path;
}

function pathWithin(child, parent) {
  const relation = relative(comparablePath(parent), comparablePath(child));
  return relation === "" || (relation !== ".." && !relation.startsWith(`..${sep}`) && !isAbsolute(relation));
}

function validateStateFilename(path) {
  const name = basename(path);
  if (comparablePath(name) === comparablePath(DIRECTORY_MARKER)
    || /(?:^|\.)(?:bak|lock|tmp|stale)(?:\.|$)/iu.test(name)) {
    fail("relay_state_filename_reserved");
  }
}

function sameFileIdentity(left, right) {
  return left && right && left.dev === right.dev && left.ino === right.ino
    && (left.dev !== "0" || left.ino !== "0");
}

function fileIdentityKey(identity) {
  return identity && (identity.dev !== "0" || identity.ino !== "0")
    ? `${identity.dev}:${identity.ino}`
    : null;
}

function openError(error) {
  const result = error instanceof RelayStateStoreError
    ? error
    : new RelayStateStoreError("relay_state_unavailable", "relay_state_unavailable", { cause: error });
  if (error?.cleanupErrors && !result.cleanupErrors) {
    Object.defineProperty(result, "cleanupErrors", {
      configurable: false,
      enumerable: false,
      writable: false,
      value: error.cleanupErrors,
    });
  }
  return result;
}

function attachReleaseError(error, releaseError) {
  Object.defineProperty(error, "releaseError", {
    configurable: false,
    enumerable: false,
    writable: false,
    value: releaseError,
  });
  return error;
}

function attachCleanupError(error, cleanupError) {
  if (!error.cleanupErrors) {
    Object.defineProperty(error, "cleanupErrors", {
      configurable: false,
      enumerable: false,
      writable: false,
      value: [],
    });
  }
  error.cleanupErrors.push(cleanupError);
  return error;
}

function fileIdentity(metadata) {
  if (typeof metadata.dev !== "bigint" || typeof metadata.ino !== "bigint") {
    fail("relay_state_file_identity_unavailable");
  }
  return { dev: metadata.dev.toString(), ino: metadata.ino.toString() };
}

function exactLock(value) {
  const lock = exactObject(value, ["schema", "version", "pid", "process_identity", "owner_id"], "relay_state_lock");
  if (lock.schema !== LOCK_SCHEMA || lock.version !== LOCK_VERSION
    || !Number.isSafeInteger(lock.pid) || lock.pid <= 0
    || typeof lock.process_identity !== "string" || lock.process_identity.length < 3 || lock.process_identity.length > 512
    || typeof lock.owner_id !== "string" || !/^[A-Za-z0-9_-]{24}$/u.test(lock.owner_id)) {
    fail("relay_state_lock_malformed");
  }
  return lock;
}

function processAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return error?.code === "EPERM";
  }
}

export class RelayStateStore {
  static async open(options) {
    const store = new RelayStateStore(options);
    await store.#open();
    return store;
  }

  #fs;
  #path;
  #backupPath;
  #lockPath;
  #markerPath;
  #legacyStatePath;
  #maxBytes;
  #randomBytes;
  #sleep;
  #processIdentity;
  #protectPath;
  #verifyPath;
  #verifyAncestors;
  #ancestorSnapshot = null;
  #state = null;
  #lockOwner = null;
  #lockTemporaryArtifact = null;
  #closed = false;
  #closing = false;
  #lockRemoved = false;
  #closeInFlight = null;
  #degraded = false;
  #mutation = Promise.resolve();

  constructor({
    statePath,
    legacyStatePath,
    maxBytes = DEFAULT_MAX_RELAY_CONTROLLER_STATE_BYTES,
    fs = nodeFs,
    randomBytes = crypto.randomBytes,
    sleep = (milliseconds) => new Promise((resolveSleep) => setTimeout(resolveSleep, milliseconds)),
    processIdentity = defaultProcessIdentity,
    protectPath = process.platform === "win32" ? protectWindowsPath : null,
    verifyPath = process.platform === "win32" ? verifyWindowsPath : null,
    verifyAncestors = process.platform === "win32" ? verifyWindowsAncestors : null,
  } = {}) {
    this.#path = safeStatePath(statePath);
    this.#legacyStatePath = safeStatePath(legacyStatePath);
    validateStateFilename(this.#path);
    if (comparablePath(this.#path) === comparablePath(this.#legacyStatePath)) {
      fail("relay_state_aliases_legacy");
    }
    if (pathWithin(this.#legacyStatePath, dirname(this.#path))) {
      fail("legacy_state_path_inside_controller_directory");
    }
    if (!Number.isSafeInteger(maxBytes) || maxBytes < 1024 || maxBytes > 256 * 1024 * 1024) {
      fail("relay_state_limit_invalid");
    }
    this.#backupPath = `${this.#path}.bak`;
    this.#lockPath = `${this.#path}.lock`;
    this.#markerPath = join(dirname(this.#path), DIRECTORY_MARKER);
    this.#maxBytes = maxBytes;
    this.#fs = fs;
    this.#randomBytes = randomBytes;
    this.#sleep = sleep;
    this.#processIdentity = processIdentity;
    this.#protectPath = protectPath;
    this.#verifyPath = verifyPath;
    this.#verifyAncestors = verifyAncestors;
  }

  async #open() {
    try {
      await this.#prepareDedicatedDirectory();
      await this.#rejectLegacyAliases();
      await this.#acquireLock();
      const primary = await this.#loadCandidate(this.#path);
      const backup = await this.#loadCandidate(this.#backupPath);
      if (primary.kind === "missing" && backup.kind === "missing") {
        this.#state = emptyState();
        await this.#persist(this.#state);
        return;
      }
      if (primary.kind === "valid") {
        this.#state = primary.state;
        if (primary.migrated) {
          await this.#persist(primary.state);
          return;
        }
        if (backup.kind !== "valid" || backup.migrated
          || canonicalJson(primary.state) !== canonicalJson(backup.state)) {
          fail("relay_state_unavailable");
        }
        return;
      }
      if (primary.kind === "invalid" && backup.kind === "valid" && !backup.migrated) {
        this.#state = backup.state;
        await this.#legacyAliasGuardedWrite(this.#path, serializeState(backup.state, this.#maxBytes));
        return;
      }
      fail("relay_state_unavailable");
    } catch (error) {
      const primaryError = openError(error);
      try {
        await this.#releaseLock();
      } catch (releaseError) {
        if (this.#lockOwner) {
          this.#markAbandonedOpen();
        }
        throw attachReleaseError(primaryError, releaseError);
      }
      throw primaryError;
    }
  }

  async #prepareDedicatedDirectory() {
    const directory = dirname(this.#path);
    const parent = dirname(directory);
    await this.#validatePathAncestors(parent, false);
    let created = false;
    try {
      await this.#fs.mkdir(directory, { recursive: false, mode: 0o700 });
      created = true;
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
    }
    const metadata = await this.#fs.lstat(directory);
    if (!metadata.isDirectory() || metadata.isSymbolicLink()) fail("relay_state_directory_unsafe");
    if (created) {
      await this.#protectFilesystemPath(directory, "directory");
    } else {
      await this.#verifyFilesystemPath(directory, "directory");
    }
    await this.#validateDedicatedContents();
    let markerCreated = false;
    let markerHandle;
    let markerOwned = false;
    try {
      markerHandle = await this.#fs.open(this.#markerPath, "wx", 0o600);
      markerOwned = true;
      try {
        await this.#protectFilesystemPath(this.#markerPath, "file");
        await markerHandle.writeFile(DIRECTORY_MARKER_CONTENT, "utf8");
        await markerHandle.sync();
        markerCreated = true;
      } finally {
        await markerHandle.close();
        markerHandle = null;
      }
    } catch (error) {
      try { await markerHandle?.close(); } catch {}
      if (markerOwned && !markerCreated) {
        try { await this.#fs.rm(this.#markerPath, { force: true }); } catch {}
      }
      if (error?.code !== "EEXIST") throw error;
    }
    const markerContents = await this.#readBoundedFile(
      this.#markerPath,
      256,
      "relay_state_directory_unsafe",
      !markerCreated,
    );
    if (markerContents !== DIRECTORY_MARKER_CONTENT) fail("relay_state_directory_unsafe");
    this.#ancestorSnapshot = await this.#validatePathAncestors(directory, true);
  }

  async #validateDedicatedContents() {
    const expected = new Set([
      basename(this.#path),
      basename(this.#backupPath),
      basename(this.#lockPath),
      DIRECTORY_MARKER,
    ]);
    const escaped = [basename(this.#path), basename(this.#backupPath), basename(this.#lockPath)]
      .map((value) => value.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&"));
    const temporary = new RegExp(`^(?:${escaped.join("|")})\\.[0-9]+\\.[a-f0-9]{16}\\.tmp$`, "u");
    const staleLock = new RegExp(`^${escaped[2]}\\.stale\\.[a-f0-9]{16}$`, "u");
    for (const entry of await this.#fs.readdir(dirname(this.#path), { withFileTypes: true })) {
      if (expected.has(entry.name) || temporary.test(entry.name) || staleLock.test(entry.name)) continue;
      fail("relay_state_directory_not_dedicated");
    }
  }

  async #validatePathAncestors(target, capture) {
    const absolute = resolve(target);
    const root = parse(absolute).root;
    const suffix = absolute.slice(root.length);
    const components = suffix ? suffix.split(sep).filter(Boolean) : [];
    let current = root;
    const snapshot = [];
    for (const component of components) {
      current = join(current, component);
      const metadata = await this.#fs.lstat(current);
      if (!metadata.isDirectory() || metadata.isSymbolicLink()) fail("relay_state_path_component_unsafe");
      const canonical = await this.#fs.realpath(current);
      if (comparablePath(canonical) !== comparablePath(resolve(current))) {
        fail("relay_state_path_component_unsafe");
      }
      if (capture) snapshot.push({ path: current, canonical, ...await this.#pathIdentity(current) });
    }
    if (process.platform === "win32") {
      if (typeof this.#verifyAncestors !== "function") fail("relay_state_path_verifier_unavailable");
      try {
        await this.#verifyAncestors(snapshot.length > 0 ? snapshot.map((entry) => entry.path) : components.reduce(
          (paths, component) => [...paths, join(paths.at(-1) || root, component)],
          [],
        ));
      } catch (error) {
        if (error instanceof RelayStateStoreError) throw error;
        fail("relay_state_path_component_unsafe", error);
      }
    }
    return snapshot;
  }

  async #assertAncestorSnapshot() {
    if (!this.#ancestorSnapshot) fail("relay_state_unavailable");
    for (const expected of this.#ancestorSnapshot) {
      const metadata = await this.#fs.lstat(expected.path);
      const canonical = await this.#fs.realpath(expected.path);
      const identity = await this.#pathIdentity(expected.path);
      if (!metadata.isDirectory() || metadata.isSymbolicLink()
        || comparablePath(canonical) !== comparablePath(expected.canonical)
        || !sameFileIdentity(identity, expected)) {
        fail("relay_state_path_changed");
      }
    }
  }

  async #protectFilesystemPath(target, kind) {
    if (process.platform === "win32") {
      if (typeof this.#protectPath !== "function") fail("relay_state_acl_unavailable");
      try {
        await this.#protectPath(target, kind);
      } catch (error) {
        if (error instanceof RelayStateStoreError) throw error;
        fail("relay_state_acl_unverified", error);
      }
      return;
    }
    await this.#fs.chmod(target, kind === "directory" ? 0o700 : 0o600);
    await this.#verifyFilesystemPath(target, kind);
  }

  async #verifyFilesystemPath(target, kind) {
    if (process.platform === "win32") {
      if (typeof this.#verifyPath !== "function") fail("relay_state_acl_unavailable");
      try {
        await this.#verifyPath(target, kind);
      } catch (error) {
        if (error instanceof RelayStateStoreError) throw error;
        fail("relay_state_acl_unverified", error);
      }
      return;
    }
    const metadata = await this.#fs.lstat(target);
    if (metadata.isSymbolicLink() || (kind === "directory" ? !metadata.isDirectory() : !metadata.isFile())
      || (metadata.mode & 0o077) !== 0
      || (typeof process.getuid === "function" && metadata.uid !== process.getuid())) {
      fail("relay_state_permissions_unsafe");
    }
  }

  async #canonicalCandidate(target) {
    try {
      return await this.#fs.realpath(target);
    } catch (error) {
      if (error?.code !== "ENOENT") throw error;
      const parent = await this.#fs.realpath(dirname(target));
      return join(parent, basename(target));
    }
  }

  async #pathIdentity(target) {
    return fileIdentity(await this.#fs.lstat(target, { bigint: true }));
  }

  async #handleIdentity(handle) {
    return fileIdentity(await handle.stat({ bigint: true }));
  }

  async #optionalIdentity(target) {
    try {
      const metadata = await this.#fs.lstat(target);
      if (metadata.isSymbolicLink()) {
        const canonical = await this.#fs.realpath(target);
        const followed = await this.#fs.stat(canonical, { bigint: true });
        return { canonical, identity: fileIdentity(followed) };
      }
      return { canonical: await this.#fs.realpath(target), identity: await this.#pathIdentity(target) };
    } catch (error) {
      if (error?.code === "ENOENT") return null;
      throw error;
    }
  }

  async #rejectLegacyAliases() {
    await this.#validatePathAncestors(dirname(this.#legacyStatePath), false);
    await this.#rejectLegacySymlink();
    const legacyCanonical = await this.#canonicalCandidate(this.#legacyStatePath);
    const controllerDirectoryCanonical = await this.#fs.realpath(dirname(this.#path));
    if (pathWithin(legacyCanonical, controllerDirectoryCanonical)) {
      fail("legacy_state_path_inside_controller_directory");
    }
    const managementPaths = await this.#managementPaths();
    for (const candidate of managementPaths) {
      if (comparablePath(candidate) === comparablePath(this.#legacyStatePath)
        || comparablePath(await this.#canonicalCandidate(candidate)) === comparablePath(legacyCanonical)) {
        fail("relay_state_aliases_legacy");
      }
    }
    const legacy = await this.#optionalIdentity(this.#legacyStatePath);
    if (!legacy) return;
    for (const candidate of managementPaths) {
      const controller = await this.#optionalIdentity(candidate);
      if (controller && sameFileIdentity(controller.identity, legacy.identity)) {
        fail("relay_state_aliases_legacy");
      }
    }
  }

  async #managementPaths() {
    const paths = [this.#path, this.#backupPath, this.#lockPath, this.#markerPath];
    const stateName = basename(this.#path).replace(/[.*+?^${}()|[\]\\]/gu, "\\$&");
    const managed = new RegExp(`^${stateName}(?:\\.bak|\\.lock)(?:\\.|$)|^${stateName}\\.[0-9]+\\.[a-f0-9]{16}\\.tmp$`, "u");
    for (const entry of await this.#fs.readdir(dirname(this.#path), { withFileTypes: true })) {
      if (managed.test(entry.name)) paths.push(join(dirname(this.#path), entry.name));
    }
    return [...new Set(paths)];
  }

  async #assertLegacyStillSeparate() {
    const managementIdentities = new Set();
    await this.#captureManagementIdentities(managementIdentities);
    await this.#assertLegacyAgainstIdentities(managementIdentities);
  }

  async #captureManagementIdentities(identities) {
    for (const candidate of await this.#managementPaths()) {
      const controller = await this.#optionalIdentity(candidate);
      const key = fileIdentityKey(controller?.identity);
      if (key) identities.add(key);
    }
  }

  async #assertLegacyAgainstIdentities(managementIdentities) {
    await this.#rejectLegacySymlink();
    const legacy = await this.#optionalIdentity(this.#legacyStatePath);
    const legacyKey = fileIdentityKey(legacy?.identity);
    if (legacyKey && managementIdentities.has(legacyKey)) fail("relay_state_aliases_legacy");
    const legacyCanonical = await this.#canonicalCandidate(this.#legacyStatePath);
    const controllerDirectoryCanonical = await this.#fs.realpath(dirname(this.#path));
    if (pathWithin(legacyCanonical, controllerDirectoryCanonical)) {
      fail("legacy_state_path_inside_controller_directory");
    }
    return legacy?.identity ?? null;
  }

  async #rejectLegacySymlink() {
    try {
      const metadata = await this.#fs.lstat(this.#legacyStatePath);
      if (metadata.isSymbolicLink()) fail("relay_state_aliases_legacy");
    } catch (error) {
      if (error?.code !== "ENOENT") throw error;
    }
  }

  async #legacyAliasGuardedWrite(path, contents) {
    await this.#assertAncestorSnapshot();
    const transaction = { protectedIdentities: new Set(), legacyBeforeIdentity: null };
    await this.#captureManagementIdentities(transaction.protectedIdentities);
    transaction.legacyBeforeIdentity = await this.#assertLegacyAgainstIdentities(
      transaction.protectedIdentities,
    );
    let operationError = null;
    try {
      await this.#writeAtomic(path, contents, transaction);
    } catch (error) {
      operationError = error;
    }
    let auditError = null;
    try {
      await this.#assertAncestorSnapshot();
      await this.#captureManagementIdentities(transaction.protectedIdentities);
      const legacyBeforeKey = fileIdentityKey(transaction.legacyBeforeIdentity);
      if (legacyBeforeKey && transaction.protectedIdentities.has(legacyBeforeKey)) {
        fail("relay_state_aliases_legacy");
      }
      await this.#assertLegacyAgainstIdentities(transaction.protectedIdentities);
    } catch (error) {
      auditError = error;
    }
    if (operationError) {
      throw operationError;
    }
    if (auditError) throw auditError;
  }

  async #readBoundedFile(target, limit, errorCode = "relay_state_file_unsafe", verifyAcl = true) {
    await this.#assertAncestorSnapshotIfReady();
    const before = await this.#fs.lstat(target);
    const beforeIdentity = await this.#pathIdentity(target);
    if (!before.isFile() || before.isSymbolicLink() || before.size > limit) fail(errorCode);
    if (verifyAcl) await this.#verifyFilesystemPath(target, "file");
    const flags = fsConstants.O_RDONLY | (fsConstants.O_NOFOLLOW || 0);
    let handle;
    try {
      handle = await this.#fs.open(target, flags);
      const opened = await handle.stat();
      const openedIdentity = await this.#handleIdentity(handle);
      if (!opened.isFile() || opened.size > limit || !sameFileIdentity(beforeIdentity, openedIdentity)) {
        fail(errorCode);
      }
      const chunks = [];
      let total = 0;
      while (total <= limit) {
        const buffer = Buffer.allocUnsafe(Math.min(64 * 1024, limit + 1 - total));
        const { bytesRead } = await handle.read(buffer, 0, buffer.length, total);
        if (bytesRead === 0) break;
        chunks.push(buffer.subarray(0, bytesRead));
        total += bytesRead;
      }
      if (total > limit) fail(errorCode);
      const after = await this.#fs.lstat(target);
      const afterIdentity = await this.#pathIdentity(target);
      if (after.isSymbolicLink() || !sameFileIdentity(openedIdentity, afterIdentity)) fail(errorCode);
      await this.#assertAncestorSnapshotIfReady();
      try {
        return new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks, total));
      } catch (error) {
        fail(errorCode, error);
      }
    } finally {
      try { await handle?.close(); } catch {}
    }
  }

  async #assertAncestorSnapshotIfReady() {
    if (this.#ancestorSnapshot) await this.#assertAncestorSnapshot();
  }

  async #acquireLock() {
    const processPathKey = comparablePath(this.#path);
    await this.#recoverAbandonedOpen(processPathKey);
    if (PROCESS_HELD_STATE_PATHS.has(processPathKey)) fail("relay_state_locked");
    const identity = await this.#processIdentity(process.pid);
    if (typeof identity !== "string" || identity.length < 3 || identity.length > 512) {
      fail("relay_state_process_identity_unavailable");
    }
    const owner = {
      schema: LOCK_SCHEMA,
      version: LOCK_VERSION,
      pid: process.pid,
      process_identity: identity,
      owner_id: this.#randomBytes(18).toString("base64url"),
    };
    const contents = `${canonicalJson(owner)}\n`;
    for (let attempt = 0; attempt < LOCK_RETRY_DELAYS_MS.length; attempt += 1) {
      const delay = LOCK_RETRY_DELAYS_MS[attempt];
      if (delay > 0) await this.#sleep(delay);
      await this.#assertAncestorSnapshot();
      const temporaryPath = `${this.#lockPath}.${process.pid}.${this.#randomBytes(8).toString("hex")}.tmp`;
      let handle;
      let temporaryIdentity = null;
      let publishedIdentity = null;
      try {
        handle = await this.#fs.open(temporaryPath, "wx", 0o600);
        temporaryIdentity = await this.#handleIdentity(handle);
        await this.#protectFilesystemPath(temporaryPath, "file");
        await handle.writeFile(contents, "utf8");
        await handle.sync();
        await handle.close();
        handle = null;
        await this.#fs.link(temporaryPath, this.#lockPath);
        publishedIdentity = temporaryIdentity;
        const published = await this.#fs.lstat(this.#lockPath);
        const publishedPathIdentity = await this.#pathIdentity(this.#lockPath);
        if (published.isSymbolicLink() || !sameFileIdentity(temporaryIdentity, publishedPathIdentity)) {
          fail("relay_state_lock_publish_failed");
        }
        this.#registerProvisionalLock(owner, temporaryPath, temporaryIdentity, processPathKey);
        await this.#removeOwnedLockTemporary();
        await this.#syncDirectory(dirname(this.#lockPath));
        this.#clearRecoveryRecord();
        return;
      } catch (error) {
        if (this.#lockOwner === owner) throw error;
        try {
          await handle?.close();
        } catch (cleanupError) {
          attachCleanupError(error, cleanupError);
        }
        if (publishedIdentity && temporaryIdentity) {
          try {
            await this.#removeOwnedPath(this.#lockPath, publishedIdentity);
          } catch (cleanupError) {
            attachCleanupError(error, cleanupError);
          }
        }
        if (temporaryIdentity) {
          try {
            await this.#removeOwnedPath(temporaryPath, temporaryIdentity);
          } catch (cleanupError) {
            attachCleanupError(error, cleanupError);
          }
        }
        if (error.cleanupErrors?.length) throw error;
        if (error?.code !== "EEXIST") throw error;
        const recovered = await this.#recoverStaleLock(attempt);
        if (!recovered) fail("relay_state_locked");
      }
    }
    fail("relay_state_locked");
  }

  #registerProvisionalLock(owner, temporaryPath, temporaryIdentity, processPathKey) {
    this.#lockOwner = owner;
    this.#lockTemporaryArtifact = { path: temporaryPath, identity: temporaryIdentity };
    PROCESS_HELD_STATE_PATHS.add(processPathKey);
    ABANDONED_OPEN_STORES.set(processPathKey, {
      store: this,
      cleanup: null,
      recoverable: false,
    });
  }

  #markAbandonedOpen() {
    const processPathKey = comparablePath(this.#path);
    const existing = ABANDONED_OPEN_STORES.get(processPathKey);
    if (existing?.store === this) {
      existing.recoverable = true;
      existing.cleanup = null;
      return;
    }
    ABANDONED_OPEN_STORES.set(processPathKey, {
      store: this,
      cleanup: null,
      recoverable: true,
    });
  }

  #clearRecoveryRecord() {
    const processPathKey = comparablePath(this.#path);
    if (ABANDONED_OPEN_STORES.get(processPathKey)?.store === this) {
      ABANDONED_OPEN_STORES.delete(processPathKey);
    }
  }

  async #recoverAbandonedOpen(processPathKey) {
    const abandoned = ABANDONED_OPEN_STORES.get(processPathKey);
    if (!abandoned) return;
    if (!abandoned.recoverable) fail("relay_state_locked");
    if (!abandoned.cleanup) {
      abandoned.cleanup = (async () => {
        await abandoned.store.#releaseLock();
        abandoned.store.#closed = true;
      })();
    }
    try {
      await abandoned.cleanup;
      if (ABANDONED_OPEN_STORES.get(processPathKey) === abandoned) {
        ABANDONED_OPEN_STORES.delete(processPathKey);
      }
    } catch (error) {
      abandoned.cleanup = null;
      fail("relay_state_locked", error);
    }
  }

  async #inspectLock() {
    try {
      const text = await this.#readBoundedFile(this.#lockPath, 4096, "relay_state_lock_malformed", false);
      let owner;
      try { owner = exactLock(JSON.parse(text)); } catch (error) {
        if (error instanceof RelayStateStoreError) return { kind: "malformed", text };
        return { kind: "malformed", text };
      }
      await this.#verifyFilesystemPath(this.#lockPath, "file");
      return { kind: "valid", owner, text, identity: await this.#pathIdentity(this.#lockPath) };
    } catch (error) {
      if (error?.code === "ENOENT") return { kind: "missing" };
      if (error instanceof RelayStateStoreError && error.code === "relay_state_lock_malformed") {
        return { kind: "malformed", text: null };
      }
      throw error;
    }
  }

  async #recoverStaleLock(attempt) {
    const first = await this.#inspectLock();
    if (first.kind === "missing") return true;
    if (first.kind === "malformed" && attempt < 2) return true;
    if (first.kind === "valid") {
      const actualIdentity = await this.#processIdentity(first.owner.pid);
      if (actualIdentity === first.owner.process_identity) return false;
      if (actualIdentity === null && processAlive(first.owner.pid)) return false;
    }
    const stalePath = `${this.#lockPath}.stale.${this.#randomBytes(8).toString("hex")}`;
    try {
      await this.#fs.rename(this.#lockPath, stalePath);
    } catch (error) {
      if (error?.code === "ENOENT") return true;
      fail("relay_state_locked", error);
    }
    let captured;
    try {
      captured = await this.#readBoundedFile(stalePath, 4096, "relay_state_lock_malformed", false);
    } catch {
      captured = null;
    }
    if (first.text !== captured) {
      try {
        await this.#fs.link(stalePath, this.#lockPath);
      } catch (error) {
        if (error?.code !== "EEXIST") fail("relay_state_locked", error);
      }
      try { await this.#fs.rm(stalePath, { force: true }); } catch {}
      return false;
    }
    await this.#fs.rm(stalePath, { force: true });
    await this.#syncDirectory(dirname(this.#lockPath));
    return true;
  }

  async #releaseLock() {
    if (!this.#lockOwner) return;
    if (!this.#lockRemoved) {
      const existing = await this.#inspectLock();
      if (existing.kind !== "valid" || existing.owner.pid !== this.#lockOwner.pid
        || existing.owner.process_identity !== this.#lockOwner.process_identity
        || existing.owner.owner_id !== this.#lockOwner.owner_id) {
        fail("relay_state_lock_ownership_lost");
      }
    }
    let releaseError = null;
    if (!this.#lockRemoved) {
      try {
        await this.#removeWithRetry(this.#lockPath);
        this.#lockRemoved = true;
      } catch (error) {
        releaseError = error;
      }
    }
    try {
      await this.#removeOwnedLockTemporary();
    } catch (cleanupError) {
      if (releaseError) attachCleanupError(releaseError, cleanupError);
      else releaseError = cleanupError;
    }
    if (releaseError) throw releaseError;
    await this.#syncDirectory(dirname(this.#lockPath));
    PROCESS_HELD_STATE_PATHS.delete(comparablePath(this.#path));
    this.#lockOwner = null;
    this.#lockRemoved = false;
    this.#clearRecoveryRecord();
  }

  async #removeOwnedLockTemporary() {
    if (!this.#lockTemporaryArtifact) return;
    await this.#removeOwnedPath(
      this.#lockTemporaryArtifact.path,
      this.#lockTemporaryArtifact.identity,
    );
    this.#lockTemporaryArtifact = null;
  }

  async #removeOwnedPath(target, expectedIdentity) {
    let metadata;
    try {
      metadata = await this.#fs.lstat(target);
    } catch (error) {
      if (error?.code === "ENOENT") return;
      throw error;
    }
    const actualIdentity = await this.#pathIdentity(target);
    if (metadata.isSymbolicLink() || !sameFileIdentity(expectedIdentity, actualIdentity)) {
      fail("relay_state_cleanup_ownership_lost");
    }
    await this.#removeWithRetry(target);
  }

  async #removeWithRetry(target) {
    let lastError;
    for (const delay of REPLACE_RETRY_DELAYS_MS) {
      if (delay > 0) await this.#sleep(delay);
      try {
        await this.#fs.rm(target, { force: true });
        return;
      } catch (error) {
        lastError = error;
        if (!["EACCES", "EBUSY", "EPERM"].includes(error?.code)) throw error;
      }
    }
    throw lastError;
  }

  async #loadCandidate(path) {
    try {
      const metadata = await this.#fs.lstat(path);
      if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size > this.#maxBytes) {
        return { kind: "invalid" };
      }
      const contents = await this.#readBoundedFile(path, this.#maxBytes, "relay_state_file_unsafe");
      try {
        return { kind: "valid", ...parseState(contents) };
      } catch (error) {
        if (error instanceof RelayStateStoreError) return { kind: "invalid" };
        throw error;
      }
    } catch (error) {
      if (error?.code === "ENOENT") return { kind: "missing" };
      throw error;
    }
  }

  async #replaceWithRetry(source, target) {
    let lastError;
    for (const delay of REPLACE_RETRY_DELAYS_MS) {
      if (delay > 0) await this.#sleep(delay);
      try {
        await this.#fs.rename(source, target);
        return;
      } catch (error) {
        lastError = error;
        if (!["EACCES", "EBUSY", "EPERM"].includes(error?.code)) throw error;
      }
    }
    throw lastError;
  }

  async #writeAtomic(path, contents, transaction) {
    const temporaryPath = `${path}.${process.pid}.${this.#randomBytes(8).toString("hex")}.tmp`;
    let handle;
    try {
      await this.#assertAncestorSnapshot();
      handle = await this.#fs.open(temporaryPath, "wx", 0o600);
      await this.#protectFilesystemPath(temporaryPath, "file");
      const temporaryIdentity = await this.#handleIdentity(handle);
      const temporaryKey = fileIdentityKey(temporaryIdentity);
      if (temporaryKey) transaction.protectedIdentities.add(temporaryKey);
      await handle.writeFile(contents, "utf8");
      await handle.sync();
      await handle.close();
      handle = null;
      const beforeReplace = await this.#fs.lstat(temporaryPath);
      if (beforeReplace.isSymbolicLink()
        || !sameFileIdentity(temporaryIdentity, await this.#pathIdentity(temporaryPath))) {
        fail("relay_state_temporary_file_changed");
      }
      const retired = await this.#optionalIdentity(path);
      const retiredKey = fileIdentityKey(retired?.identity);
      if (retiredKey) transaction.protectedIdentities.add(retiredKey);
      await this.#replaceWithRetry(temporaryPath, path);
      const installed = await this.#fs.lstat(path);
      if (installed.isSymbolicLink()
        || !sameFileIdentity(temporaryIdentity, await this.#pathIdentity(path))) {
        fail("relay_state_replace_changed");
      }
      await this.#syncDirectory(dirname(path));
      await this.#assertAncestorSnapshot();
    } catch (error) {
      try { await handle?.close(); } catch {}
      try { await this.#fs.rm(temporaryPath, { force: true }); } catch {}
      throw error;
    }
  }

  async #syncDirectory(path) {
    let lastError;
    for (const delay of REPLACE_RETRY_DELAYS_MS) {
      if (delay > 0) await this.#sleep(delay);
      let handle;
      try {
        handle = await this.#fs.open(path, "r");
        await handle.sync();
        return;
      } catch (error) {
        if (process.platform === "win32" && error?.code === "EPERM" && error?.syscall === "fsync") return;
        if (["EINVAL", "EISDIR", "ENOTSUP"].includes(error?.code)) return;
        lastError = error;
        if (!["EACCES", "EBUSY", "EPERM"].includes(error?.code)) throw error;
      } finally {
        try { await handle?.close(); } catch {}
      }
    }
    throw lastError;
  }

  async #persist(state) {
    const contents = serializeState(state, this.#maxBytes);
    try {
      // Stage the recovery copy first. The primary remains authoritative until
      // its atomic replacement succeeds, so a failed commit cannot roll back a
      // newer primary to an older authorization snapshot on the next restart.
      await this.#legacyAliasGuardedWrite(this.#backupPath, contents);
      await this.#legacyAliasGuardedWrite(this.#path, contents);
    } catch (error) {
      this.#degraded = true;
      fail("relay_state_persist_failed", error);
    }
  }

  #assertReady() {
    if (this.#closed) fail("relay_state_closed");
    if (this.#closing || this.#degraded || !this.#state) fail("relay_state_unavailable");
  }

  getPathPolicy() {
    this.#assertReady();
    return Object.freeze({
      controller_state_path: this.#path,
      legacy_web_state_path: this.#legacyStatePath,
    });
  }

  readNamespace(name) {
    this.#assertReady();
    if (!NAMESPACE.test(name)) fail("namespace_name_invalid");
    const record = this.#state.namespaces[name];
    return Object.freeze({
      store_revision: this.#state.revision,
      namespace_revision: record?.revision ?? "0",
      value: cloneJson(record?.value ?? null),
    });
  }

  compareAndSwap(name, expectedNamespaceRevision, nextValue) {
    const operation = this.#mutation.then(async () => {
      this.#assertReady();
      if (!NAMESPACE.test(name)) fail("namespace_name_invalid");
      decimal(expectedNamespaceRevision, "expected_namespace_revision");
      validateJsonValue(nextValue);
      const current = this.#state.namespaces[name];
      const currentRevision = current?.revision ?? "0";
      if (currentRevision !== expectedNamespaceRevision) fail("namespace_revision_conflict");
      if (!current && Object.keys(this.#state.namespaces).length >= MAX_RELAY_STATE_NAMESPACES) {
        fail("relay_state_capacity_reached");
      }
      const namespaceRevision = nextDecimal(currentRevision);
      const nextState = {
        ...this.#state,
        revision: nextDecimal(this.#state.revision),
        namespaces: {
          ...this.#state.namespaces,
          [name]: { revision: namespaceRevision, value: cloneJson(nextValue) },
        },
      };
      await this.#persist(nextState);
      this.#state = nextState;
      return Object.freeze({
        store_revision: nextState.revision,
        namespace_revision: namespaceRevision,
        value: cloneJson(nextValue),
      });
    });
    this.#mutation = operation.catch(() => {});
    return operation;
  }

  async close() {
    if (this.#closed) return;
    if (this.#closeInFlight) return this.#closeInFlight;
    const attempt = (async () => {
      if (!this.#closing) {
        await this.#mutation.catch(() => {});
        this.#closing = true;
      }
      await this.#releaseLock();
      this.#closed = true;
    })();
    this.#closeInFlight = attempt;
    try {
      await attempt;
    } finally {
      this.#closeInFlight = null;
    }
  }
}
