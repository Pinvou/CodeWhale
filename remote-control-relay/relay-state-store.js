import crypto from "node:crypto";
import * as nodeFs from "node:fs/promises";
import { dirname, isAbsolute, resolve } from "node:path";

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

function processAlive(pid) {
  if (!Number.isSafeInteger(pid) || pid <= 0) return false;
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
  #maxBytes;
  #randomBytes;
  #sleep;
  #state = null;
  #lockOwner = null;
  #lockHandle = null;
  #closed = false;
  #degraded = false;
  #mutation = Promise.resolve();

  constructor({
    statePath,
    maxBytes = DEFAULT_MAX_RELAY_CONTROLLER_STATE_BYTES,
    fs = nodeFs,
    randomBytes = crypto.randomBytes,
    sleep = (milliseconds) => new Promise((resolveSleep) => setTimeout(resolveSleep, milliseconds)),
  } = {}) {
    this.#path = safeStatePath(statePath);
    if (!Number.isSafeInteger(maxBytes) || maxBytes < 1024 || maxBytes > 256 * 1024 * 1024) {
      fail("relay_state_limit_invalid");
    }
    this.#backupPath = `${this.#path}.bak`;
    this.#lockPath = `${this.#path}.lock`;
    this.#maxBytes = maxBytes;
    this.#fs = fs;
    this.#randomBytes = randomBytes;
    this.#sleep = sleep;
  }

  async #open() {
    try {
      await this.#ensurePrivateDirectory();
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
        await this.#writeAtomic(this.#path, serializeState(backup.state, this.#maxBytes));
        return;
      }
      fail("relay_state_unavailable");
    } catch (error) {
      await this.#releaseLock();
      if (error instanceof RelayStateStoreError) throw error;
      fail("relay_state_unavailable", error);
    }
  }

  async #ensurePrivateDirectory() {
    const directory = dirname(this.#path);
    await this.#fs.mkdir(directory, { recursive: true, mode: 0o700 });
    const metadata = await this.#fs.lstat(directory);
    if (!metadata.isDirectory() || metadata.isSymbolicLink()) fail("relay_state_directory_unsafe");
    try { await this.#fs.chmod(directory, 0o700); } catch (error) {
      if (process.platform !== "win32") throw error;
    }
  }

  async #acquireLock() {
    const owner = {
      pid: process.pid,
      owner_id: this.#randomBytes(18).toString("base64url"),
    };
    for (let attempt = 0; attempt < 2; attempt += 1) {
      try {
        const handle = await this.#fs.open(this.#lockPath, "wx", 0o600);
        await handle.writeFile(`${JSON.stringify(owner)}\n`, "utf8");
        await handle.sync();
        this.#lockHandle = handle;
        this.#lockOwner = owner;
        return;
      } catch (error) {
        if (error?.code !== "EEXIST") throw error;
        let existing;
        try {
          const text = await this.#fs.readFile(this.#lockPath, "utf8");
          if (Buffer.byteLength(text) > 4096) fail("relay_state_locked");
          existing = JSON.parse(text);
        } catch (readError) {
          if (readError instanceof RelayStateStoreError) throw readError;
          fail("relay_state_locked", readError);
        }
        if (processAlive(existing?.pid)) fail("relay_state_locked");
        const stalePath = `${this.#lockPath}.stale.${this.#randomBytes(8).toString("hex")}`;
        try {
          await this.#fs.rename(this.#lockPath, stalePath);
          await this.#fs.rm(stalePath, { force: true });
        } catch (replaceError) {
          if (replaceError?.code !== "ENOENT") fail("relay_state_locked", replaceError);
        }
      }
    }
    fail("relay_state_locked");
  }

  async #releaseLock() {
    try { await this.#lockHandle?.close(); } catch {}
    this.#lockHandle = null;
    if (!this.#lockOwner) return;
    try {
      const existing = JSON.parse(await this.#fs.readFile(this.#lockPath, "utf8"));
      if (existing?.pid === this.#lockOwner.pid && existing?.owner_id === this.#lockOwner.owner_id) {
        await this.#fs.rm(this.#lockPath, { force: true });
      }
    } catch {}
    this.#lockOwner = null;
  }

  async #loadCandidate(path) {
    try {
      const metadata = await this.#fs.lstat(path);
      if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size > this.#maxBytes) {
        return { kind: "invalid" };
      }
      try { await this.#fs.chmod(path, 0o600); } catch (error) {
        if (process.platform !== "win32") throw error;
      }
      const contents = await this.#fs.readFile(path, "utf8");
      if (Buffer.byteLength(contents) > this.#maxBytes) return { kind: "invalid" };
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

  async #writeAtomic(path, contents) {
    const temporaryPath = `${path}.${process.pid}.${this.#randomBytes(8).toString("hex")}.tmp`;
    let handle;
    try {
      handle = await this.#fs.open(temporaryPath, "wx", 0o600);
      await handle.writeFile(contents, "utf8");
      await handle.sync();
      await handle.close();
      handle = null;
      await this.#replaceWithRetry(temporaryPath, path);
      try { await this.#fs.chmod(path, 0o600); } catch (error) {
        if (process.platform !== "win32") throw error;
      }
      await this.#syncDirectory(dirname(path));
    } catch (error) {
      try { await handle?.close(); } catch {}
      try { await this.#fs.rm(temporaryPath, { force: true }); } catch {}
      throw error;
    }
  }

  async #syncDirectory(path) {
    let handle;
    try {
      handle = await this.#fs.open(path, "r");
      await handle.sync();
    } catch (error) {
      if (!["EACCES", "EINVAL", "EISDIR", "ENOTSUP", "EPERM"].includes(error?.code)) throw error;
    } finally {
      try { await handle?.close(); } catch {}
    }
  }

  async #persist(state) {
    const contents = serializeState(state, this.#maxBytes);
    try {
      // Stage the recovery copy first. The primary remains authoritative until
      // its atomic replacement succeeds, so a failed commit cannot roll back a
      // newer primary to an older authorization snapshot on the next restart.
      await this.#writeAtomic(this.#backupPath, contents);
      await this.#writeAtomic(this.#path, contents);
    } catch (error) {
      this.#degraded = true;
      fail("relay_state_persist_failed", error);
    }
  }

  #assertReady() {
    if (this.#closed) fail("relay_state_closed");
    if (this.#degraded || !this.#state) fail("relay_state_unavailable");
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
    await this.#mutation.catch(() => {});
    this.#closed = true;
    await this.#releaseLock();
  }
}
