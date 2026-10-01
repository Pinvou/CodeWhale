import crypto from "node:crypto";
import {
  compareEnrollmentIdentifiers,
  validateEnrollmentRegistrySnapshot,
} from "./enrollment-registry.js";

const REQUEST_ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{7,127}$/u;
const ENDPOINT_ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$/u;
const BACKUP_SCHEMA = "pinvou-enrollment-migration-backup";
const BACKUP_VERSION = 1;
const SOCKET_PROOFS = new WeakMap();
const SOCKET_CHALLENGES = new WeakMap();
const OFFICIAL_VERIFIERS = new WeakMap();
const PROOF_KEY_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/u;
const PROOF_MAX_LIFETIME_MS = 5 * 60_000;

export class EnrollmentMigrationError extends Error {
  constructor(code, options) {
    super(code, options);
    this.name = "EnrollmentMigrationError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new EnrollmentMigrationError(code, cause ? { cause } : undefined);
}

function requestId(value) {
  if (typeof value !== "string" || !REQUEST_ID.test(value)) fail("migration_request_id_invalid");
  return value;
}

function endpointId(value) {
  if (typeof value !== "string" || !ENDPOINT_ID.test(value)) fail("migration_endpoint_id_invalid");
  return value;
}

function exactObject(value, expected, code) {
  if (!value || typeof value !== "object" || Array.isArray(value)
    || Object.getPrototypeOf(value) !== Object.prototype
    || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...expected].sort())) fail(code);
  return value;
}

function credential(value, label = "migration_credential") {
  const bytes = typeof value === "string" ? Buffer.byteLength(value, "utf8") : 0;
  if (bytes < 32 || bytes > 4096) fail(`${label}_invalid`);
  return value;
}

function base64Url(value, bytes, label) {
  if (typeof value !== "string" || !/^[A-Za-z0-9_-]+$/u.test(value)) fail(`${label}_invalid`);
  const decoded = Buffer.from(value, "base64url");
  if (decoded.length !== bytes || decoded.toString("base64url") !== value) fail(`${label}_invalid`);
  return decoded;
}

function proofPayload(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail("migration_identity_proof_invalid");
  const keys = Object.keys(value);
  const required = [
    "schema", "version", "key_id", "device_id", "endpoint_id", "access_token_sha256",
    "desktop_secret_sha256", "socket_challenge_b64", "issued_at_ms", "expires_at_ms",
  ];
  if (keys.length !== required.length || required.some((key) => !Object.hasOwn(value, key))) {
    fail("migration_identity_proof_invalid");
  }
  if (value.schema !== "pinvou-official-endpoint-enrollment-proof" || value.version !== 1
    || !PROOF_KEY_ID.test(value.key_id) || !ENDPOINT_ID.test(value.device_id)
    || !ENDPOINT_ID.test(value.endpoint_id) || !/^[a-f0-9]{64}$/u.test(value.access_token_sha256)
    || !/^[a-f0-9]{64}$/u.test(value.desktop_secret_sha256)) fail("migration_identity_proof_invalid");
  base64Url(value.socket_challenge_b64, 32, "migration_socket_challenge");
  if (!Number.isSafeInteger(value.issued_at_ms) || !Number.isSafeInteger(value.expires_at_ms)
    || value.issued_at_ms < 0 || value.expires_at_ms <= value.issued_at_ms
    || value.expires_at_ms - value.issued_at_ms > PROOF_MAX_LIFETIME_MS) {
    fail("migration_identity_proof_invalid");
  }
  return value;
}

function canonicalProof(value) {
  const proof = proofPayload(value);
  return Buffer.from(JSON.stringify({
    schema: proof.schema,
    version: proof.version,
    key_id: proof.key_id,
    device_id: proof.device_id,
    endpoint_id: proof.endpoint_id,
    access_token_sha256: proof.access_token_sha256,
    desktop_secret_sha256: proof.desktop_secret_sha256,
    socket_challenge_b64: proof.socket_challenge_b64,
    issued_at_ms: proof.issued_at_ms,
    expires_at_ms: proof.expires_at_ms,
  }), "utf8");
}

export function createOfficialEnrollmentProofVerifier({ publicKeys, clock = Date.now } = {}) {
  if (!Array.isArray(publicKeys) || publicKeys.length === 0 || publicKeys.length > 32
    || typeof clock !== "function") fail("migration_identity_verifier_invalid");
  const keys = new Map();
  for (const entry of publicKeys) {
    if (!entry || typeof entry !== "object" || Array.isArray(entry)
      || Object.keys(entry).length !== 2 || !PROOF_KEY_ID.test(entry.key_id)
      || typeof entry.public_key_pem !== "string" || entry.public_key_pem.length > 8192
      || keys.has(entry.key_id)) fail("migration_identity_verifier_invalid");
    let key;
    try { key = crypto.createPublicKey(entry.public_key_pem); } catch { fail("migration_identity_verifier_invalid"); }
    if (key.asymmetricKeyType !== "ed25519") fail("migration_identity_verifier_invalid");
    keys.set(entry.key_id, key);
  }
  const verifier = Object.freeze(Object.create(null));
  OFFICIAL_VERIFIERS.set(verifier, { keys, clock });
  return verifier;
}

function verifyOfficialEnrollmentProof(verifier, { proof, signatureB64, expected }) {
  const state = OFFICIAL_VERIFIERS.get(verifier);
  if (!state) fail("migration_identity_verifier_invalid");
  proof = proofPayload(proof);
  const key = state.keys.get(proof.key_id);
  const signature = base64Url(signatureB64, 64, "migration_identity_signature");
  const now = state.clock();
  if (!Number.isSafeInteger(now) || now < proof.issued_at_ms || now >= proof.expires_at_ms
    || proof.endpoint_id !== expected.endpointId || proof.device_id !== expected.endpointId
    || proof.access_token_sha256 !== crypto.createHash("sha256").update(expected.accessToken, "utf8").digest("hex")
    || proof.desktop_secret_sha256 !== crypto.createHash("sha256").update(expected.desktopSecret, "utf8").digest("hex")
    || proof.socket_challenge_b64 !== expected.challenge || !key
    || !crypto.verify(null, canonicalProof(proof), key, signature)) fail("migration_identity_proof_invalid");
  return proof;
}

function publicResult(entry, request, action) {
  return Object.freeze({
    request_id: request,
    endpoint_id: entry.endpoint_id,
    status: entry.status,
    revision: entry.revision,
    action,
    legacy_web_link: action === "re_enrolled" ? "invalidated" : "preserved",
  });
}

/**
 * DEV-06 calls bindAuthenticatedLegacySocket only after the existing v2 Relay
 * has authenticated the desktop socket. The returned proof is opaque and tied
 * to that exact socket. The migration request itself never carries a token.
 */
export class EnrollmentMigrationHandler {
  #registry;
  #identityVerifier;

  constructor({ registry, identityVerifier } = {}) {
    if (!registry || typeof registry.executeMigrationRequest !== "function" || typeof registry.getStatus !== "function"
      || typeof registry.getControllerReadiness !== "function") {
      fail("migration_registry_invalid");
    }
    if (identityVerifier !== undefined && !OFFICIAL_VERIFIERS.has(identityVerifier)) {
      fail("migration_identity_verifier_invalid");
    }
    this.#registry = registry;
    this.#identityVerifier = identityVerifier ?? null;
  }

  issueClaimChallenge({ socket, endpointId: id } = {}) {
    if (!this.#identityVerifier) fail("migration_identity_verifier_required");
    if (!socket || (typeof socket !== "object" && typeof socket !== "function")) {
      fail("migration_socket_invalid");
    }
    id = endpointId(id);
    const challenge = crypto.randomBytes(32).toString("base64url");
    SOCKET_CHALLENGES.set(socket, { owner: this, endpointId: id, challenge, consumed: false });
    return Object.freeze({ endpoint_id: id, socket_challenge_b64: challenge });
  }

  bindAuthenticatedLegacySocket({
    socket, endpointId: id, accessToken, desktopSecret, challenge, enrollmentProof, signatureB64,
  } = {}) {
    if (!socket || (typeof socket !== "object" && typeof socket !== "function")) {
      fail("migration_socket_invalid");
    }
    id = endpointId(id);
    accessToken = credential(accessToken, "legacy_access_token");
    desktopSecret = credential(desktopSecret, "legacy_desktop_secret");
    const issued = SOCKET_CHALLENGES.get(socket);
    if (!issued || issued.owner !== this || issued.endpointId !== id || issued.consumed
      || issued.challenge !== challenge) fail("migration_socket_challenge_invalid");
    issued.consumed = true;
    verifyOfficialEnrollmentProof(this.#identityVerifier, {
      proof: enrollmentProof,
      signatureB64,
      expected: { endpointId: id, accessToken, desktopSecret, challenge },
    });
    const proof = Object.freeze(Object.create(null));
    SOCKET_PROOFS.set(proof, { owner: this, socket, endpointId: id, accessToken, desktopSecret, active: true });
    return proof;
  }

  releaseAuthenticatedLegacySocket(proof) {
    const record = SOCKET_PROOFS.get(proof);
    if (record?.owner === this) record.active = false;
  }

  async claim({ socket, proof, requestId: id } = {}) {
    id = requestId(id);
    const record = SOCKET_PROOFS.get(proof);
    if (!record || record.owner !== this || !record.active || record.socket !== socket) {
      fail("migration_socket_proof_invalid");
    }
    try {
      const request = await this.#registry.executeMigrationRequest({
        requestId: id,
        operations: [{
          operation: "claim_roles",
          endpoint_id: record.endpointId,
          credential: record.accessToken,
          desktop_credential: record.desktopSecret,
        }],
      });
      if (request.status !== "succeeded") fail("migration_claim_failed");
      const operationResult = request.outcome?.results?.[0];
      if (!operationResult || operationResult.endpoint_id !== record.endpointId) {
        fail("migration_claim_failed");
      }
      return Object.freeze({ ...publicResult({ ...operationResult, status: "active" }, id, "claimed"),
        desktop_ready: this.#registry.getControllerReadiness(record.endpointId).desktop_ready });
    } catch (error) {
      if (error?.code === "migration_request_conflict") fail("migration_request_conflict", error);
      fail(["endpoint_already_enrolled", "endpoint_revoked"].includes(error?.code)
        ? "migration_claim_conflict" : "migration_claim_failed", error);
    }
  }

  coverage(legacyEndpointIds) {
    if (!Array.isArray(legacyEndpointIds) || legacyEndpointIds.length > 100_000) {
      fail("migration_inventory_invalid");
    }
    const unique = [...new Set(legacyEndpointIds.map(endpointId))].sort(compareEnrollmentIdentifiers);
    const endpoints = unique.map((id) => ({
      endpoint_id: id,
      enrollment_status: this.#registry.getStatus(id)?.status ?? "not_migrated",
      ...this.#registry.getControllerReadiness(id),
    }));
    const migrated = endpoints.filter((entry) => entry.desktop_ready).length;
    return Object.freeze({
      total: endpoints.length,
      migrated,
      web_migrated: endpoints.filter((entry) => entry.web_ready).length,
      not_migrated: endpoints.length - migrated,
      coverage_percent: endpoints.length === 0 ? 100 : Number(((migrated / endpoints.length) * 100).toFixed(2)),
      endpoints: Object.freeze(endpoints.map(Object.freeze)),
    });
  }
}

function validateBackup(value) {
  const keys = value && typeof value === "object" && !Array.isArray(value) ? Object.keys(value).sort() : [];
  if (!value || typeof value !== "object" || Array.isArray(value)
    || JSON.stringify(keys) !== JSON.stringify(["namespace_revision", "registry", "registry_sha256", "schema", "version"])
    || value.schema !== BACKUP_SCHEMA || value.version !== BACKUP_VERSION
    || typeof value.namespace_revision !== "string" || !/^(?:0|[1-9][0-9]{0,19})$/u.test(value.namespace_revision)
    || typeof value.registry_sha256 !== "string"
    || value.registry_sha256 !== backupDigest(value.registry)) {
    fail("migration_backup_invalid");
  }
  try {
    return {
      ...structuredClone(value),
      registry: structuredClone(validateEnrollmentRegistrySnapshot(value.registry)),
    };
  } catch (error) {
    fail("migration_backup_invalid", error);
  }
}

function backupDigest(registry) {
  return crypto.createHash("sha256").update(JSON.stringify(registry), "utf8").digest("hex");
}

export function createEnrollmentBackup(registry) {
  if (!registry || typeof registry.exportSnapshot !== "function") fail("migration_registry_invalid");
  const snapshot = registry.exportSnapshot();
  return Object.freeze({
    schema: BACKUP_SCHEMA,
    version: BACKUP_VERSION,
    namespace_revision: snapshot.namespace_revision,
    registry: snapshot.registry,
    registry_sha256: backupDigest(snapshot.registry),
  });
}

export async function restoreEnrollmentBackup({ registry, backup, requestId: stableRequestId } = {}) {
  if (!registry || typeof registry.executeMigrationRequest !== "function") fail("migration_registry_invalid");
  const parsed = validateBackup(backup);
  const restoreRequestId = requestId(stableRequestId);
  try {
    const restored = await registry.executeMigrationRequest({
      requestId: restoreRequestId,
      operations: [{ operation: "restore", snapshot: parsed.registry }],
      reportReplay: true,
    });
    return Object.freeze({
      restored: restored.replayed === false,
      applied: restored.replayed === false,
      replayed: restored.replayed === true,
      request_id: restoreRequestId,
      status: restored.status,
      restored_entry_count: restored.replayed ? 0 : restored.outcome.restored_entry_count,
    });
  } catch (error) {
    fail("migration_restore_failed", error);
  }
}

function validateOperation(raw) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) fail("migration_operation_invalid");
  const operation = raw.operation;
  if (operation === "allowlist") {
    const dual = Object.hasOwn(raw, "desktop_credential");
    exactObject(raw, dual ? ["operation", "endpoint_id", "credential", "desktop_credential", "expected_revision"]
      : ["operation", "endpoint_id", "credential"], "migration_operation_invalid");
    return { operation, endpointId: endpointId(raw.endpoint_id), credential: credential(raw.credential),
      ...(dual ? { desktopCredential: credential(raw.desktop_credential, "desktop_credential"), expectedRevision: raw.expected_revision } : {}) };
  }
  if (operation === "set_desktop_credential") {
    exactObject(raw, ["operation", "endpoint_id", "desktop_credential", "expected_revision"], "migration_operation_invalid");
    return { operation, endpointId: endpointId(raw.endpoint_id),
      desktopCredential: credential(raw.desktop_credential, "desktop_credential"), expectedRevision: raw.expected_revision };
  }
  if (operation === "re_enroll") {
    exactObject(raw, [
      "operation", "endpoint_id", "current_credential", "new_credential", "expected_revision",
    ], "migration_operation_invalid");
    return {
      operation,
      endpointId: endpointId(raw.endpoint_id),
      currentCredential: credential(raw.current_credential, "current_credential"),
      newCredential: credential(raw.new_credential, "new_credential"),
      expectedRevision: typeof raw.expected_revision === "string" ? raw.expected_revision : fail("expected_revision_invalid"),
    };
  }
  fail("migration_operation_invalid");
}

function registryOperation(operation) {
  if (operation.operation === "allowlist") {
    return { operation: "allowlist", endpoint_id: operation.endpointId, credential: operation.credential,
      ...(operation.desktopCredential === undefined ? {} : {
        desktop_credential: operation.desktopCredential, expected_revision: operation.expectedRevision,
      }) };
  }
  if (operation.operation === "set_desktop_credential") {
    return { operation: operation.operation, endpoint_id: operation.endpointId,
      desktop_credential: operation.desktopCredential, expected_revision: operation.expectedRevision };
  }
  return {
    operation: "re_enroll",
    endpoint_id: operation.endpointId,
    current_credential: operation.currentCredential,
    new_credential: operation.newCredential,
    expected_revision: operation.expectedRevision,
  };
}

/** DEV-01 owns validation and the single namespace CAS; no prefix can persist. */
export async function runEnrollmentBatch({ registry, operations, dryRun = true, requestId: stableRequestId } = {}) {
  if (!registry || typeof registry.previewAtomicBatch !== "function"
    || typeof registry.executeMigrationRequest !== "function") {
    fail("migration_registry_invalid");
  }
  if (!Array.isArray(operations) || operations.length === 0 || operations.length > 100_000) {
    fail("migration_batch_invalid");
  }
  const parsed = operations.map(validateOperation);
  if (new Set(parsed.map((operation) => operation.endpointId)).size !== parsed.length) {
    fail("migration_batch_duplicate_endpoint");
  }
  const atomicOperations = parsed.map(registryOperation);
  stableRequestId = requestId(stableRequestId);
  try {
    if (dryRun) {
      const preview = registry.previewAtomicBatch({ operations: atomicOperations });
      return Object.freeze({
        dry_run: true,
        applied: 0,
        results: preview.results.map((result) => Object.freeze({
          endpoint_id: result.endpoint_id,
          action: result.action === "allowlisted"
            ? "would_allowlist"
            : result.action === "re_enrolled" ? "would_re_enroll"
              : result.action === "desktop_credential_set" ? "would_set_desktop_credential" : "already_enrolled",
        })),
      });
    }
    const applied = await registry.executeMigrationRequest({
      requestId: stableRequestId,
      operations: atomicOperations,
    });
    return Object.freeze({
      dry_run: false,
      request_id: stableRequestId,
      applied: applied.outcome.results.length,
      results: applied.outcome.results,
    });
  } catch (error) {
    if (error?.code === "migration_request_conflict") fail("migration_request_conflict", error);
    fail("migration_batch_failed", error);
  }
}
