import crypto from "node:crypto";
import { basename, dirname, isAbsolute, relative, resolve, sep } from "node:path";

export const ENROLLMENT_NAMESPACE = "endpoint_enrollment";
export const ENROLLMENT_REGISTRY_SCHEMA = "pinvou-endpoint-enrollment-registry";
export const ENROLLMENT_REGISTRY_VERSION = 2;

const CONFIG = new WeakMap();
const ENDPOINT_ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$/u;
const KEY_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/u;
const HEX_64 = /^[a-f0-9]{64}$/u;
const REVISION = /^(?:0|[1-9][0-9]{0,19})$/u;
const U64_MAX = 18_446_744_073_709_551_615n;
const CREDENTIAL_MIN_BYTES = 32;
const CREDENTIAL_MAX_BYTES = 4096;
const MAX_ENTRIES = 100_000;
const MAX_MIGRATION_REQUESTS = 100_000;
const MAX_BATCH_OPERATIONS = 10_000;
const MIGRATION_REQUEST_ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{7,127}$/u;
const MIGRATION_ERROR_CODE = /^[a-z][a-z0-9_]{0,127}$/u;
const PURPOSES = new Set(["web_v2_reregister", "desktop_pairing_register"]);
const DEPLOYMENTS = new Set(["official", "self_hosted", "development"]);

export class EnrollmentRegistryError extends Error {
  constructor(code, message = code, options) {
    super(message, options);
    this.name = "EnrollmentRegistryError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new EnrollmentRegistryError(code, code, cause ? { cause } : undefined);
}

function exactObject(value, required, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(`${label}_malformed`);
  const keys = Object.keys(value);
  if (keys.length !== required.length || required.some((key) => !Object.hasOwn(value, key))) {
    fail(`${label}_malformed`);
  }
  return value;
}

function parseBoolean(value, label) {
  if (value === undefined || value === "" || value === "0" || value === "false") return false;
  if (value === "1" || value === "true") return true;
  fail(`${label}_invalid`);
}

function validateEndpointId(endpointId) {
  if (typeof endpointId !== "string" || !ENDPOINT_ID.test(endpointId)) fail("endpoint_id_invalid");
  return endpointId;
}

function validateCredential(credential, label = "enrollment_credential") {
  if (typeof credential !== "string") fail(`${label}_invalid`);
  const bytes = Buffer.from(credential, "utf8");
  if (bytes.length < CREDENTIAL_MIN_BYTES || bytes.length > CREDENTIAL_MAX_BYTES) {
    fail(`${label}_invalid`);
  }
  if (bytes.toString("utf8") !== credential) fail(`${label}_invalid`);
  return bytes;
}

function validateRevision(value, label) {
  if (typeof value !== "string" || !REVISION.test(value) || BigInt(value) > U64_MAX) {
    fail(`${label}_invalid`);
  }
  return value;
}

function incrementRevision(value) {
  const next = BigInt(validateRevision(value, "endpoint_revision")) + 1n;
  if (next > U64_MAX) fail("endpoint_revision_exhausted");
  return next.toString();
}

function digestCredential(key, endpointId, credential) {
  const input = validateCredential(credential);
  return crypto.createHmac("sha256", key)
    .update("pinvou-enrollment-credential-v1\0", "utf8")
    .update(endpointId, "utf8")
    .update("\0", "utf8")
    .update(input)
    .digest("hex");
}

function digestMigrationParameters(key, keyId, requestId, canonicalParameters) {
  return {
    algorithm: "hmac-sha256-v1",
    key_id: keyId,
    digest_hex: crypto.createHmac("sha256", key)
      .update("pinvou-enrollment-migration-request-v1\0", "utf8")
      .update(requestId, "utf8")
      .update("\0", "utf8")
      .update(canonicalParameters, "utf8")
      .digest("hex"),
  };
}

function safeEqualHex(left, right) {
  if (!HEX_64.test(left) || !HEX_64.test(right)) return false;
  return crypto.timingSafeEqual(Buffer.from(left, "hex"), Buffer.from(right, "hex"));
}

function isoTimestamp(clock) {
  const milliseconds = clock();
  if (!Number.isSafeInteger(milliseconds) || milliseconds < 0 || milliseconds > 8_640_000_000_000_000) {
    fail("enrollment_clock_invalid");
  }
  const value = new Date(milliseconds).toISOString();
  if (value === "Invalid Date") fail("enrollment_clock_invalid");
  return value;
}

function validateTimestamp(value, nullable, label) {
  if (nullable && value === null) return null;
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/u.test(value)
    || Number.isNaN(Date.parse(value))) {
    fail(`${label}_malformed`);
  }
  return value;
}

function validateEntry(raw) {
  const entry = exactObject(raw, [
    "endpoint_id", "status", "revision", "credential", "enrolled_at", "rotated_at", "revoked_at", "audit",
  ], "enrollment_entry");
  validateEndpointId(entry.endpoint_id);
  if (entry.status !== "active" && entry.status !== "revoked") fail("enrollment_entry_malformed");
  validateRevision(entry.revision, "endpoint_revision");
  const credential = exactObject(entry.credential, ["algorithm", "key_id", "digest_hex"], "credential_digest");
  if (credential.algorithm !== "hmac-sha256-v1" || !KEY_ID.test(credential.key_id)
    || !HEX_64.test(credential.digest_hex)) fail("credential_digest_malformed");
  validateTimestamp(entry.enrolled_at, false, "enrolled_at");
  validateTimestamp(entry.rotated_at, true, "rotated_at");
  validateTimestamp(entry.revoked_at, true, "revoked_at");
  const audit = exactObject(entry.audit, ["source", "last_action"], "enrollment_audit");
  if (!["official", "self_hosted", "development", "development_tofu"].includes(audit.source)
    || !["enrolled", "rotated", "revoked"].includes(audit.last_action)) {
    fail("enrollment_audit_malformed");
  }
  if ((entry.status === "active" && entry.revoked_at !== null)
    || (entry.status === "revoked" && entry.revoked_at === null)) fail("enrollment_entry_malformed");
  const enrolledAt = Date.parse(entry.enrolled_at);
  if ((entry.rotated_at !== null && Date.parse(entry.rotated_at) < enrolledAt)
    || (entry.revoked_at !== null && Date.parse(entry.revoked_at) < enrolledAt)
    || (entry.rotated_at !== null && entry.revoked_at !== null
      && Date.parse(entry.revoked_at) < Date.parse(entry.rotated_at))
    || (entry.audit.last_action === "enrolled" && entry.rotated_at !== null)
    || (entry.audit.last_action === "rotated" && (entry.rotated_at === null || entry.status !== "active"))
    || (entry.audit.last_action === "revoked" && entry.status !== "revoked")) {
    fail("enrollment_entry_malformed");
  }
  return structuredClone(entry);
}

function validateMigrationResult(raw) {
  const result = exactObject(raw, ["endpoint_id", "revision", "action", "legacy_web_link"], "migration_result");
  validateEndpointId(result.endpoint_id);
  validateRevision(result.revision, "endpoint_revision");
  if (!["allowlisted", "already_enrolled", "re_enrolled"].includes(result.action)
    || !["preserved", "invalidated"].includes(result.legacy_web_link)
    || (result.action === "re_enrolled") !== (result.legacy_web_link === "invalidated")) {
    fail("migration_result_malformed");
  }
  return structuredClone(result);
}

function validateMigrationRequest(raw) {
  const request = exactObject(raw, [
    "request_id", "operation", "endpoint_ids", "parameters", "status",
    "created_at", "completed_at", "outcome",
  ], "migration_request");
  if (typeof request.request_id !== "string" || !MIGRATION_REQUEST_ID.test(request.request_id)) {
    fail("migration_request_malformed");
  }
  if (request.operation !== "batch" && request.operation !== "restore") fail("migration_request_malformed");
  if (!Array.isArray(request.endpoint_ids) || request.endpoint_ids.length > MAX_BATCH_OPERATIONS) {
    fail("migration_request_malformed");
  }
  const endpointIds = request.endpoint_ids.map(validateEndpointId);
  for (let index = 1; index < endpointIds.length; index += 1) {
    if (endpointIds[index - 1] >= endpointIds[index]) fail("migration_request_malformed");
  }
  if (request.operation === "batch" && endpointIds.length === 0) fail("migration_request_malformed");
  if (request.operation === "restore" && endpointIds.length !== 0) fail("migration_request_malformed");
  const parameters = exactObject(request.parameters, ["algorithm", "key_id", "digest_hex"], "migration_parameters");
  if (parameters.algorithm !== "hmac-sha256-v1" || !KEY_ID.test(parameters.key_id)
    || !HEX_64.test(parameters.digest_hex)) fail("migration_request_malformed");
  validateTimestamp(request.created_at, false, "migration_created_at");
  if (!(["pending", "succeeded", "failed"].includes(request.status))) fail("migration_request_malformed");
  if (request.status === "pending") {
    if (request.completed_at !== null || request.outcome !== null) fail("migration_request_malformed");
  } else {
    validateTimestamp(request.completed_at, false, "migration_completed_at");
    if (Date.parse(request.completed_at) < Date.parse(request.created_at)) fail("migration_request_malformed");
    const outcome = exactObject(
      request.outcome,
      ["code", "results", "restored_entry_count"],
      "migration_outcome",
    );
    if (!Array.isArray(outcome.results) || outcome.results.length > MAX_BATCH_OPERATIONS) {
      fail("migration_outcome_malformed");
    }
    outcome.results.map(validateMigrationResult);
    if (request.status === "succeeded") {
      if (outcome.code !== null) fail("migration_outcome_malformed");
      if (request.operation === "batch" && outcome.restored_entry_count !== null) fail("migration_outcome_malformed");
      if (request.operation === "batch") {
        const resultEndpointIds = outcome.results.map((result) => result.endpoint_id).sort();
        if (JSON.stringify(resultEndpointIds) !== JSON.stringify(endpointIds)) fail("migration_outcome_malformed");
      }
      if (request.operation === "restore"
        && (!Number.isSafeInteger(outcome.restored_entry_count) || outcome.restored_entry_count < 0
          || outcome.restored_entry_count > MAX_ENTRIES || outcome.results.length !== 0)) {
        fail("migration_outcome_malformed");
      }
    } else if (typeof outcome.code !== "string" || !MIGRATION_ERROR_CODE.test(outcome.code)
      || outcome.results.length !== 0 || outcome.restored_entry_count !== null) {
      fail("migration_outcome_malformed");
    }
  }
  return structuredClone(request);
}

function validateRegistry(raw) {
  if (raw === null) {
    return {
      schema: ENROLLMENT_REGISTRY_SCHEMA,
      version: ENROLLMENT_REGISTRY_VERSION,
      entries: [],
      migration_requests: [],
    };
  }
  if (!raw || typeof raw !== "object" || Array.isArray(raw)
    || !Object.hasOwn(raw, "schema") || !Object.hasOwn(raw, "version") || !Object.hasOwn(raw, "entries")) {
    fail("enrollment_registry_malformed");
  }
  if (raw.schema !== ENROLLMENT_REGISTRY_SCHEMA || (raw.version !== 1 && raw.version !== 2)) {
    fail("enrollment_registry_unsupported");
  }
  const legacy = raw.version === 1;
  const registry = exactObject(
    raw,
    legacy ? ["schema", "version", "entries"] : ["schema", "version", "entries", "migration_requests"],
    "enrollment_registry",
  );
  if (!Array.isArray(registry.entries) || registry.entries.length > MAX_ENTRIES) {
    fail("enrollment_registry_malformed");
  }
  if (registry.entries.length > MAX_ENTRIES) fail("enrollment_registry_capacity_reached");
  const entries = registry.entries.map(validateEntry);
  for (let index = 0; index < entries.length; index += 1) {
    if (index > 0 && entries[index - 1].endpoint_id >= entries[index].endpoint_id) {
      fail("enrollment_registry_malformed");
    }
  }
  const migrationRequests = legacy ? [] : registry.migration_requests;
  if (!Array.isArray(migrationRequests) || migrationRequests.length > MAX_MIGRATION_REQUESTS) {
    fail("enrollment_registry_malformed");
  }
  const requests = migrationRequests.map(validateMigrationRequest);
  for (let index = 1; index < requests.length; index += 1) {
    if (requests[index - 1].request_id >= requests[index].request_id) fail("enrollment_registry_malformed");
  }
  return {
    schema: ENROLLMENT_REGISTRY_SCHEMA,
    version: ENROLLMENT_REGISTRY_VERSION,
    entries,
    migration_requests: requests,
  };
}

function deepFreeze(value) {
  if (!value || typeof value !== "object" || Object.isFrozen(value)) return value;
  for (const child of Object.values(value)) deepFreeze(child);
  return Object.freeze(value);
}

export function validateEnrollmentRegistrySnapshot(snapshot) {
  return deepFreeze(validateRegistry(snapshot));
}

function publicEntry(entry) {
  if (!entry) return null;
  return Object.freeze({
    endpoint_id: entry.endpoint_id,
    status: entry.status,
    revision: entry.revision,
    enrolled_at: entry.enrolled_at,
    rotated_at: entry.rotated_at,
    revoked_at: entry.revoked_at,
  });
}

function publicMigrationRequest(request) {
  if (!request) return null;
  return deepFreeze({
    request_id: request.request_id,
    operation: request.operation,
    endpoint_ids: structuredClone(request.endpoint_ids),
    status: request.status,
    created_at: request.created_at,
    completed_at: request.completed_at,
    outcome: structuredClone(request.outcome),
  });
}

function validateMigrationRequestId(value) {
  if (typeof value !== "string" || !MIGRATION_REQUEST_ID.test(value)) fail("migration_request_id_invalid");
  return value;
}

function prepareAtomicOperations(operations) {
  if (!Array.isArray(operations) || operations.length === 0 || operations.length > MAX_BATCH_OPERATIONS) {
    fail("enrollment_batch_invalid");
  }
  const prepared = operations.map((raw) => {
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) fail("enrollment_batch_operation_invalid");
    if (raw.operation === "allowlist") {
      exactObject(raw, ["operation", "endpoint_id", "credential"], "enrollment_batch_operation");
      validateCredential(raw.credential);
      return {
        operation: "allowlist",
        endpointId: validateEndpointId(raw.endpoint_id),
        credential: raw.credential,
      };
    }
    if (raw.operation === "re_enroll") {
      exactObject(raw, [
        "operation", "endpoint_id", "current_credential", "new_credential", "expected_revision",
      ], "enrollment_batch_operation");
      validateCredential(raw.current_credential, "current_credential");
      validateCredential(raw.new_credential, "new_credential");
      return {
        operation: "re_enroll",
        endpointId: validateEndpointId(raw.endpoint_id),
        currentCredential: raw.current_credential,
        newCredential: raw.new_credential,
        expectedRevision: validateRevision(raw.expected_revision, "expected_endpoint_revision"),
      };
    }
    if (raw.operation === "restore") {
      exactObject(raw, ["operation", "snapshot"], "enrollment_batch_operation");
      return { operation: "restore", snapshot: validateRegistry(raw.snapshot) };
    }
    fail("enrollment_batch_operation_invalid");
  });
  const restoreCount = prepared.filter((operation) => operation.operation === "restore").length;
  if (restoreCount > 0 && (restoreCount !== 1 || prepared.length !== 1)) fail("enrollment_batch_restore_must_be_exclusive");
  const endpointIds = prepared
    .filter((operation) => operation.operation !== "restore")
    .map((operation) => operation.endpointId);
  if (new Set(endpointIds).size !== endpointIds.length) fail("enrollment_batch_duplicate_endpoint");
  return prepared;
}

function canonicalMigrationParameters(prepared) {
  return JSON.stringify(prepared.map((operation) => {
    if (operation.operation === "allowlist") {
      return { operation: "allowlist", endpoint_id: operation.endpointId, credential: operation.credential };
    }
    if (operation.operation === "re_enroll") {
      return {
        operation: "re_enroll",
        endpoint_id: operation.endpointId,
        current_credential: operation.currentCredential,
        new_credential: operation.newCredential,
        expected_revision: operation.expectedRevision,
      };
    }
    return { operation: "restore", snapshot: operation.snapshot };
  }));
}

function migrationDescriptor(prepared) {
  const restore = prepared[0].operation === "restore";
  return {
    operation: restore ? "restore" : "batch",
    endpointIds: restore ? [] : prepared.map((operation) => operation.endpointId).sort(),
    canonicalParameters: canonicalMigrationParameters(prepared),
  };
}

function applyPreparedOperations(registry, prepared, config, clock) {
  if (prepared[0].operation === "restore") {
    const restored = prepared[0].snapshot;
    return {
      registry: {
        ...restored,
        migration_requests: structuredClone(registry.migration_requests),
      },
      results: [],
      restoredEntryCount: restored.entries.length,
      changed: true,
    };
  }
  let timestamp = null;
  const now = () => {
    timestamp ??= isoTimestamp(clock);
    return timestamp;
  };
  let entries = structuredClone(registry.entries);
  const results = [];
  let changed = false;
  for (const operation of prepared) {
    const existingIndex = entries.findIndex((entry) => entry.endpoint_id === operation.endpointId);
    const existing = existingIndex < 0 ? null : entries[existingIndex];
    if (operation.operation === "allowlist") {
      if (existing) {
        if (existing.status === "active" && existing.credential.key_id === config.keyId
          && safeEqualHex(
            existing.credential.digest_hex,
            digestCredential(config.hmacKey, operation.endpointId, operation.credential),
          )) {
          results.push({
            endpoint_id: existing.endpoint_id,
            revision: existing.revision,
            action: "already_enrolled",
            legacy_web_link: "preserved",
          });
          continue;
        }
        fail(existing.status === "revoked" ? "endpoint_revoked" : "endpoint_already_enrolled");
      }
      if (entries.length >= MAX_ENTRIES) fail("enrollment_capacity_reached");
      const entry = {
        endpoint_id: operation.endpointId,
        status: "active",
        revision: "1",
        credential: {
          algorithm: "hmac-sha256-v1",
          key_id: config.keyId,
          digest_hex: digestCredential(config.hmacKey, operation.endpointId, operation.credential),
        },
        enrolled_at: now(),
        rotated_at: null,
        revoked_at: null,
        audit: { source: sourceForDeployment(config.deployment), last_action: "enrolled" },
      };
      entries.push(entry);
      changed = true;
      results.push({
        endpoint_id: entry.endpoint_id,
        revision: entry.revision,
        action: "allowlisted",
        legacy_web_link: "preserved",
      });
      continue;
    }
    if (!existing) fail("enrollment_required");
    if (existing.status !== "active") fail("endpoint_revoked");
    if (existing.revision !== operation.expectedRevision) fail("endpoint_revision_conflict");
    if (existing.credential.key_id !== config.keyId) fail("credential_key_unavailable");
    if (!safeEqualHex(
      existing.credential.digest_hex,
      digestCredential(config.hmacKey, operation.endpointId, operation.currentCredential),
    )) fail("invalid_enrollment_credential");
    const next = {
      ...existing,
      revision: incrementRevision(existing.revision),
      credential: {
        algorithm: "hmac-sha256-v1",
        key_id: config.keyId,
        digest_hex: digestCredential(config.hmacKey, operation.endpointId, operation.newCredential),
      },
      rotated_at: now(),
      audit: { ...existing.audit, last_action: "rotated" },
    };
    entries[existingIndex] = next;
    changed = true;
    results.push({
      endpoint_id: next.endpoint_id,
      revision: next.revision,
      action: "re_enrolled",
      legacy_web_link: "invalidated",
    });
  }
  entries.sort((left, right) => left.endpoint_id.localeCompare(right.endpoint_id));
  return {
    registry: { ...registry, entries },
    results,
    restoredEntryCount: null,
    changed,
  };
}

function sourceForDeployment(deployment) {
  if (deployment === "official") return "official";
  if (deployment === "self_hosted") return "self_hosted";
  return tofu ? "development_tofu" : "development";
}

function configuredAbsolutePath(value, label) {
  if (typeof value !== "string" || !isAbsolute(value)) fail(`${label}_invalid`);
  return resolve(value);
}

function validateControllerStateFilename(path) {
  const name = basename(path);
  if (comparablePath(name) === comparablePath(".pinvou-controller-state-directory")
    || /(?:^|\.)(?:bak|lock|tmp|stale)(?:\.|$)/iu.test(name)) {
    fail("controller_state_filename_reserved");
  }
}

function comparablePath(value) {
  return process.platform === "win32" ? value.toLocaleLowerCase("en-US") : value;
}

function pathWithin(child, parent) {
  const relation = relative(comparablePath(parent), comparablePath(child));
  return relation === "" || (relation !== ".." && !relation.startsWith(`..${sep}`) && !isAbsolute(relation));
}

export function loadEnrollmentRegistryConfig(env = process.env) {
  const deployment = env.PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT;
  if (!DEPLOYMENTS.has(deployment)) fail("enrollment_deployment_invalid");
  const runtimeEnvironment = env.NODE_ENV || "production";
  if (!["production", "development", "test"].includes(runtimeEnvironment)) {
    fail("enrollment_runtime_environment_invalid");
  }
  const developmentTofu = parseBoolean(env.PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU, "development_tofu");
  if (developmentTofu && (deployment !== "development" || runtimeEnvironment === "production")) {
    fail("development_tofu_forbidden");
  }
  if (deployment === "development" && runtimeEnvironment === "production") {
    fail("development_mode_forbidden");
  }
  if (!KEY_ID.test(env.PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID || "")) fail("enrollment_hmac_key_id_invalid");
  const hmacKey = Buffer.from(env.PINVOU_REMOTE_ENROLLMENT_HMAC_KEY || "", "utf8");
  if (hmacKey.length < 32 || hmacKey.length > 4096) fail("enrollment_hmac_key_invalid");
  const controllerStatePath = configuredAbsolutePath(
    env.PINVOU_REMOTE_CONTROLLER_STATE_PATH,
    "controller_state_path",
  );
  const legacyWebStatePath = configuredAbsolutePath(
    env.PINVOU_REMOTE_STATE_PATH,
    "legacy_web_state_path",
  );
  validateControllerStateFilename(controllerStatePath);
  if (comparablePath(controllerStatePath) === comparablePath(legacyWebStatePath)) {
    fail("controller_state_path_aliases_legacy");
  }
  if (pathWithin(legacyWebStatePath, dirname(controllerStatePath))) {
    fail("legacy_state_path_inside_controller_directory");
  }
  const config = Object.freeze({
    deployment,
    runtime_environment: runtimeEnvironment,
    development_tofu: developmentTofu,
    controller_state_path: controllerStatePath,
    legacy_web_state_path: legacyWebStatePath,
  });
  CONFIG.set(config, {
    ...config,
    keyId: env.PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID,
    hmacKey,
  });
  return config;
}

function configDetails(config) {
  const details = CONFIG.get(config);
  if (!details) fail("enrollment_config_invalid");
  return details;
}

export class EnrollmentRegistry {
  static open({ stateStore, config, clock = Date.now } = {}) {
    if (!stateStore || typeof stateStore.readNamespace !== "function"
      || typeof stateStore.compareAndSwap !== "function"
      || typeof stateStore.getPathPolicy !== "function") fail("enrollment_state_store_invalid");
    const details = configDetails(config);
    const pathPolicy = stateStore.getPathPolicy();
    if (comparablePath(pathPolicy.controller_state_path) !== comparablePath(details.controller_state_path)
      || comparablePath(pathPolicy.legacy_web_state_path) !== comparablePath(details.legacy_web_state_path)) {
      fail("enrollment_state_store_path_mismatch");
    }
    const registry = new EnrollmentRegistry(stateStore, details, clock);
    registry.#read();
    return registry;
  }

  #store;
  #config;
  #clock;

  constructor(store, config, clock) {
    this.#store = store;
    this.#config = config;
    this.#clock = clock;
  }

  #read() {
    try {
      const snapshot = this.#store.readNamespace(ENROLLMENT_NAMESPACE);
      return { ...snapshot, registry: validateRegistry(snapshot.value) };
    } catch (error) {
      if (error instanceof EnrollmentRegistryError) throw error;
      fail("enrollment_registry_unavailable", error);
    }
  }

  #credentialDigest(endpointId, credential) {
    return {
      algorithm: "hmac-sha256-v1",
      key_id: this.#config.keyId,
      digest_hex: digestCredential(this.#config.hmacKey, endpointId, credential),
    };
  }

  #find(registry, endpointId) {
    return registry.entries.find((entry) => entry.endpoint_id === endpointId);
  }

  #findMigrationRequest(registry, requestId) {
    return registry.migration_requests.find((request) => request.request_id === requestId);
  }

  #migrationContext(requestId, operations) {
    requestId = validateMigrationRequestId(requestId);
    const prepared = prepareAtomicOperations(operations);
    const descriptor = migrationDescriptor(prepared);
    return {
      requestId,
      prepared,
      descriptor,
      parameters: digestMigrationParameters(
        this.#config.hmacKey,
        this.#config.keyId,
        requestId,
        descriptor.canonicalParameters,
      ),
    };
  }

  #assertMigrationRequestMatches(request, context) {
    if (request.operation !== context.descriptor.operation
      || JSON.stringify(request.endpoint_ids) !== JSON.stringify(context.descriptor.endpointIds)) {
      fail("migration_request_conflict");
    }
    if (request.parameters.key_id !== this.#config.keyId) fail("migration_request_key_unavailable");
    if (request.parameters.algorithm !== context.parameters.algorithm
      || !safeEqualHex(request.parameters.digest_hex, context.parameters.digest_hex)) {
      fail("migration_request_conflict");
    }
  }

  #migrationResponse(request) {
    if (request.status === "failed") fail(request.outcome.code);
    return publicMigrationRequest(request);
  }

  async #write(snapshot, registry) {
    try {
      const result = await this.#store.compareAndSwap(
        ENROLLMENT_NAMESPACE,
        snapshot.namespace_revision,
        validateRegistry(registry),
      );
      return { ...result, registry: validateRegistry(result.value) };
    } catch (error) {
      if (error instanceof EnrollmentRegistryError) throw error;
      if (error?.code === "namespace_revision_conflict") fail("enrollment_revision_conflict", error);
      if (error?.code === "relay_state_capacity_reached") fail("enrollment_registry_capacity_reached", error);
      fail("enrollment_registry_unavailable", error);
    }
  }

  exportSnapshot() {
    const snapshot = this.#read();
    return deepFreeze({
      namespace_revision: snapshot.namespace_revision,
      registry: structuredClone(snapshot.registry),
    });
  }

  async importSnapshot({ snapshot, expectedNamespaceRevision } = {}) {
    const registry = validateRegistry(snapshot);
    expectedNamespaceRevision = validateRevision(expectedNamespaceRevision, "expected_namespace_revision");
    const current = this.#read();
    if (current.namespace_revision !== expectedNamespaceRevision) fail("enrollment_revision_conflict");
    const written = await this.#write(current, registry);
    return deepFreeze({
      namespace_revision: written.namespace_revision,
      registry: structuredClone(written.registry),
    });
  }

  async applyAtomicBatch({ operations, expectedNamespaceRevision } = {}) {
    const prepared = prepareAtomicOperations(operations);
    expectedNamespaceRevision = validateRevision(expectedNamespaceRevision, "expected_namespace_revision");
    const snapshot = this.#read();
    if (snapshot.namespace_revision !== expectedNamespaceRevision) fail("enrollment_revision_conflict");
    const applied = applyPreparedOperations(snapshot.registry, prepared, this.#config, this.#clock);
    if (!applied.changed) {
      return deepFreeze({
        namespace_revision: snapshot.namespace_revision,
        results: applied.results,
        restored_entry_count: applied.restoredEntryCount,
      });
    }
    const written = await this.#write(snapshot, applied.registry);
    return deepFreeze({
      namespace_revision: written.namespace_revision,
      results: applied.results,
      restored_entry_count: applied.restoredEntryCount,
    });
  }

  getMigrationRequest(requestId) {
    requestId = validateMigrationRequestId(requestId);
    return publicMigrationRequest(this.#findMigrationRequest(this.#read().registry, requestId));
  }

  async beginMigrationRequest({ requestId, operations } = {}) {
    const context = this.#migrationContext(requestId, operations);
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const snapshot = this.#read();
      const existing = this.#findMigrationRequest(snapshot.registry, context.requestId);
      if (existing) {
        this.#assertMigrationRequestMatches(existing, context);
        return publicMigrationRequest(existing);
      }
      if (snapshot.registry.migration_requests.length >= MAX_MIGRATION_REQUESTS) {
        fail("migration_request_capacity_reached");
      }
      const pending = {
        request_id: context.requestId,
        operation: context.descriptor.operation,
        endpoint_ids: context.descriptor.endpointIds,
        parameters: context.parameters,
        status: "pending",
        created_at: isoTimestamp(this.#clock),
        completed_at: null,
        outcome: null,
      };
      const registry = {
        ...snapshot.registry,
        migration_requests: [...snapshot.registry.migration_requests, pending]
          .sort((left, right) => left.request_id.localeCompare(right.request_id)),
      };
      try {
        await this.#write(snapshot, registry);
        return publicMigrationRequest(pending);
      } catch (error) {
        if (error?.code === "enrollment_revision_conflict") continue;
        throw error;
      }
    }
    fail("enrollment_revision_conflict");
  }

  async completeMigrationRequest({ requestId, operations } = {}) {
    const context = this.#migrationContext(requestId, operations);
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const snapshot = this.#read();
      const existing = this.#findMigrationRequest(snapshot.registry, context.requestId);
      if (!existing) fail("migration_request_not_found");
      this.#assertMigrationRequestMatches(existing, context);
      if (existing.status !== "pending") return this.#migrationResponse(existing);

      let applied;
      let failure = null;
      try {
        applied = applyPreparedOperations(snapshot.registry, context.prepared, this.#config, this.#clock);
      } catch (error) {
        if (!(error instanceof EnrollmentRegistryError)) throw error;
        failure = error;
        applied = {
          registry: snapshot.registry,
          results: [],
          restoredEntryCount: null,
        };
      }
      const terminal = {
        ...existing,
        status: failure ? "failed" : "succeeded",
        completed_at: isoTimestamp(this.#clock),
        outcome: failure ? {
          code: failure.code,
          results: [],
          restored_entry_count: null,
        } : {
          code: null,
          results: applied.results,
          restored_entry_count: applied.restoredEntryCount,
        },
      };
      const registry = {
        ...applied.registry,
        migration_requests: snapshot.registry.migration_requests.map((request) => (
          request.request_id === context.requestId ? terminal : request
        )),
      };
      try {
        await this.#write(snapshot, registry);
        return this.#migrationResponse(terminal);
      } catch (error) {
        if (error?.code === "enrollment_revision_conflict") continue;
        throw error;
      }
    }
    fail("enrollment_revision_conflict");
  }

  async executeMigrationRequest(input = {}) {
    const pending = await this.beginMigrationRequest(input);
    if (pending.status !== "pending") return this.#migrationResponse(pending);
    return this.completeMigrationRequest(input);
  }

  getStatus(endpointId) {
    validateEndpointId(endpointId);
    return publicEntry(this.#find(this.#read().registry, endpointId));
  }

  async enroll(options = {}) {
    return this.#enroll(options, false);
  }

  async #enroll({ endpointId, credential, expectedNamespaceRevision }, tofu) {
    endpointId = validateEndpointId(endpointId);
    validateCredential(credential);
    const snapshot = this.#read();
    if (expectedNamespaceRevision !== undefined
      && validateRevision(expectedNamespaceRevision, "expected_namespace_revision") !== snapshot.namespace_revision) {
      fail("enrollment_revision_conflict");
    }
    const existing = this.#find(snapshot.registry, endpointId);
    if (existing) {
      if (existing.status === "active" && existing.credential.key_id === this.#config.keyId
        && safeEqualHex(existing.credential.digest_hex, digestCredential(this.#config.hmacKey, endpointId, credential))) {
        return publicEntry(existing);
      }
      fail(existing.status === "revoked" ? "endpoint_revoked" : "endpoint_already_enrolled");
    }
    const timestamp = isoTimestamp(this.#clock);
    const entry = {
      endpoint_id: endpointId,
      status: "active",
      revision: "1",
      credential: this.#credentialDigest(endpointId, credential),
      enrolled_at: timestamp,
      rotated_at: null,
      revoked_at: null,
      audit: { source: sourceForDeployment(this.#config.deployment, tofu), last_action: "enrolled" },
    };
    const registry = {
      ...snapshot.registry,
      entries: [...snapshot.registry.entries, entry].sort((left, right) => (
        left.endpoint_id < right.endpoint_id ? -1 : left.endpoint_id > right.endpoint_id ? 1 : 0
      )),
    };
    await this.#write(snapshot, registry);
    return publicEntry(entry);
  }

  async authorizeRegistration({ endpointId, credential, purpose } = {}) {
    endpointId = validateEndpointId(endpointId);
    validateCredential(credential);
    if (!PURPOSES.has(purpose)) fail("registration_purpose_invalid");
    for (let attempt = 0; attempt < 4; attempt += 1) {
      const snapshot = this.#read();
      let entry = this.#find(snapshot.registry, endpointId);
      if (!entry && this.#config.development_tofu) {
        try {
          await this.#enroll({ endpointId, credential, expectedNamespaceRevision: snapshot.namespace_revision }, true);
          continue;
        } catch (error) {
          if (error?.code === "enrollment_revision_conflict") continue;
          throw error;
        }
      }
      if (!entry) fail("enrollment_required");
      if (entry.status !== "active") fail("endpoint_revoked");
      if (entry.credential.key_id !== this.#config.keyId) fail("credential_key_unavailable");
      const actual = digestCredential(this.#config.hmacKey, endpointId, credential);
      if (!safeEqualHex(entry.credential.digest_hex, actual)) fail("invalid_enrollment_credential");
      return Object.freeze({
        endpoint_id: endpointId,
        status: entry.status,
        revision: entry.revision,
        purpose,
      });
    }
    fail("enrollment_revision_conflict");
  }

  async rotateCredential({ endpointId, currentCredential, newCredential, expectedEndpointRevision } = {}) {
    endpointId = validateEndpointId(endpointId);
    validateCredential(currentCredential, "current_credential");
    validateCredential(newCredential, "new_credential");
    expectedEndpointRevision = validateRevision(expectedEndpointRevision, "expected_endpoint_revision");
    const snapshot = this.#read();
    const entry = this.#find(snapshot.registry, endpointId);
    if (!entry) fail("enrollment_required");
    if (entry.status !== "active") fail("endpoint_revoked");
    if (entry.revision !== expectedEndpointRevision) fail("endpoint_revision_conflict");
    if (entry.credential.key_id !== this.#config.keyId) fail("credential_key_unavailable");
    const currentDigest = digestCredential(this.#config.hmacKey, endpointId, currentCredential);
    if (!safeEqualHex(entry.credential.digest_hex, currentDigest)) fail("invalid_enrollment_credential");
    const timestamp = isoTimestamp(this.#clock);
    const next = {
      ...entry,
      revision: incrementRevision(entry.revision),
      credential: this.#credentialDigest(endpointId, newCredential),
      rotated_at: timestamp,
      audit: { ...entry.audit, last_action: "rotated" },
    };
    await this.#write(snapshot, {
      ...snapshot.registry,
      entries: snapshot.registry.entries.map((candidate) => candidate.endpoint_id === endpointId ? next : candidate),
    });
    return publicEntry(next);
  }

  async revoke({ endpointId, expectedEndpointRevision } = {}) {
    endpointId = validateEndpointId(endpointId);
    expectedEndpointRevision = validateRevision(expectedEndpointRevision, "expected_endpoint_revision");
    const snapshot = this.#read();
    const entry = this.#find(snapshot.registry, endpointId);
    if (!entry) fail("enrollment_required");
    if (entry.status === "revoked") return publicEntry(entry);
    if (entry.revision !== expectedEndpointRevision) fail("endpoint_revision_conflict");
    const next = {
      ...entry,
      status: "revoked",
      revision: incrementRevision(entry.revision),
      revoked_at: isoTimestamp(this.#clock),
      audit: { ...entry.audit, last_action: "revoked" },
    };
    await this.#write(snapshot, {
      ...snapshot.registry,
      entries: snapshot.registry.entries.map((candidate) => candidate.endpoint_id === endpointId ? next : candidate),
    });
    return publicEntry(next);
  }
}
