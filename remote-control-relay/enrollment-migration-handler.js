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

function credential(value, label = "migration_credential") {
  const bytes = typeof value === "string" ? Buffer.byteLength(value, "utf8") : 0;
  if (bytes < 32 || bytes > 4096) fail(`${label}_invalid`);
  return value;
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
  #requests = new Map();

  constructor({ registry } = {}) {
    if (!registry || typeof registry.enroll !== "function" || typeof registry.getStatus !== "function") {
      fail("migration_registry_invalid");
    }
    this.#registry = registry;
  }

  bindAuthenticatedLegacySocket({ socket, endpointId: id, accessToken } = {}) {
    if (!socket || (typeof socket !== "object" && typeof socket !== "function")) {
      fail("migration_socket_invalid");
    }
    id = endpointId(id);
    accessToken = credential(accessToken, "legacy_access_token");
    const proof = Object.freeze(Object.create(null));
    SOCKET_PROOFS.set(proof, { owner: this, socket, endpointId: id, accessToken, active: true });
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
    const known = this.#requests.get(id);
    if (known && known.endpointId !== record.endpointId) fail("migration_request_conflict");
    if (known) return known.result;
    try {
      const entry = await this.#registry.enroll({
        endpointId: record.endpointId,
        credential: record.accessToken,
      });
      const result = publicResult(entry, id, "claimed");
      this.#requests.set(id, { endpointId: record.endpointId, result });
      return result;
    } catch (error) {
      fail(error?.code === "endpoint_already_enrolled" ? "migration_claim_conflict" : "migration_claim_failed", error);
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
    }));
    const migrated = endpoints.filter((entry) => entry.enrollment_status === "active").length;
    return Object.freeze({
      total: endpoints.length,
      migrated,
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
  const restoreRequestId = stableRequestId === undefined
    ? `restore-${parsed.registry_sha256}`
    : requestId(stableRequestId);
  try {
    const restored = await registry.executeMigrationRequest({
      requestId: restoreRequestId,
      operations: [{ operation: "restore", snapshot: parsed.registry }],
    });
    return Object.freeze({
      restored: true,
      request_id: restoreRequestId,
      status: restored.status,
      restored_entry_count: restored.outcome.restored_entry_count,
    });
  } catch (error) {
    fail("migration_restore_failed", error);
  }
}

function validateOperation(raw) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) fail("migration_operation_invalid");
  const operation = raw.operation;
  if (operation === "allowlist") {
    return { operation, endpointId: endpointId(raw.endpoint_id), credential: credential(raw.credential) };
  }
  if (operation === "re_enroll") {
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
    return { operation: "allowlist", endpoint_id: operation.endpointId, credential: operation.credential };
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
export async function runEnrollmentBatch({ registry, operations, dryRun = true } = {}) {
  if (!registry || typeof registry.previewAtomicBatch !== "function"
    || typeof registry.applyAtomicBatch !== "function" || typeof registry.exportSnapshot !== "function") {
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
            : result.action === "re_enrolled" ? "would_re_enroll" : "already_enrolled",
        })),
      });
    }
    const snapshot = registry.exportSnapshot();
    const applied = await registry.applyAtomicBatch({
      operations: atomicOperations,
      expectedNamespaceRevision: snapshot.namespace_revision,
    });
    return Object.freeze({ dry_run: false, applied: applied.results.length, results: applied.results });
  } catch (error) {
    fail("migration_batch_failed", error);
  }
}
