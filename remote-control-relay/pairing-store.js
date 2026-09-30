import crypto from "node:crypto";
import { validateControllerMessage } from "./protocol-controller.js";

export const PAIRING_NAMESPACE = "desktop_pairing";
export const PAIRING_CODE_ALPHABET = "23456789ABCDEFGHJKMNPQRSTUVWXYZ";
export const PAIRING_CODE_TTL_MS = 5 * 60_000;
export const PAIRING_CODE_ISSUE_QUARANTINE_MS = 10 * 60_000;
export const PAIRING_CODE_TERMINAL_QUARANTINE_MS = 2 * 60_000;
const MAX_GENERATION_ATTEMPTS = 32;
const MAX_RECORDS = 100_000;
const ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$/u;
const KEY_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/u;
const HEX = /^[a-f0-9]{64}$/u;
const OPEN_TOKEN = Symbol("PairingStore.open");

export class PairingStoreError extends Error {
  constructor(code, options) {
    super(code, options);
    this.name = "PairingStoreError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new PairingStoreError(code, cause ? { cause } : undefined);
}

function exact(value, keys, code) {
  if (!value || typeof value !== "object" || Array.isArray(value)
    || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) fail(code);
  return value;
}

function id(value, label) {
  if (typeof value !== "string" || !ID.test(value)) fail(`${label}_invalid`);
  return value;
}

function timestamp(value, label) {
  if (typeof value !== "string" || !/^(?:0|[1-9][0-9]{0,15})$/u.test(value)) fail(`${label}_invalid`);
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) fail(`${label}_invalid`);
  return parsed;
}

function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function digest(key, domain, value) {
  return crypto.createHmac("sha256", key).update(domain, "ascii").update(canonical(value), "utf8").digest("hex");
}

function codeDigest(key, code) {
  return crypto.createHmac("sha256", key).update("pinvou-pairing-code-digest-v1\0", "ascii")
    .update(code, "ascii").digest("hex");
}

function validateDigestRows(rows) {
  if (!Array.isArray(rows) || rows.length > 32) fail("pairing_tombstone_invalid");
  let previous = "";
  return rows.map((raw) => {
    const row = exact(raw, ["key_id", "digest_hex"], "pairing_tombstone_invalid");
    if (!KEY_ID.test(row.key_id) || !HEX.test(row.digest_hex) || row.key_id <= previous) fail("pairing_tombstone_invalid");
    previous = row.key_id;
    return { ...row };
  });
}

function validateState(value, now) {
  if (value === null) return {
    schema: "pinvou-desktop-pairing-store", version: 1, keyring_revision: "0",
    active_digest_key_id: null, digest_key_ids: [], tombstones: [], registrations: [],
  };
  const state = exact(value, [
    "schema", "version", "keyring_revision", "active_digest_key_id", "digest_key_ids",
    "tombstones", "registrations",
  ], "pairing_store_invalid");
  if (state.schema !== "pinvou-desktop-pairing-store" || state.version !== 1) fail("pairing_store_invalid");
  timestamp(state.keyring_revision, "keyring_revision");
  if (state.active_digest_key_id !== null && !KEY_ID.test(state.active_digest_key_id)) fail("pairing_store_invalid");
  if (!Array.isArray(state.digest_key_ids) || state.digest_key_ids.length > 32
    || state.digest_key_ids.some((key, index) => !KEY_ID.test(key) || (index > 0 && key <= state.digest_key_ids[index - 1]))) {
    fail("pairing_store_invalid");
  }
  if (state.active_digest_key_id !== null && !state.digest_key_ids.includes(state.active_digest_key_id)) fail("pairing_store_invalid");
  if (!Array.isArray(state.tombstones) || !Array.isArray(state.registrations)
    || state.tombstones.length > MAX_RECORDS || state.registrations.length > MAX_RECORDS) fail("pairing_store_invalid");
  const tombstones = state.tombstones.map((raw) => {
    const row = exact(raw, [
      "pairing_id", "code_digests", "issued_at_ms", "expires_at_ms", "reuse_after_ms", "terminal_at_ms",
    ], "pairing_tombstone_invalid");
    id(row.pairing_id, "pairing_id");
    const issued = timestamp(row.issued_at_ms, "issued_at_ms");
    const expires = timestamp(row.expires_at_ms, "expires_at_ms");
    const reuse = timestamp(row.reuse_after_ms, "reuse_after_ms");
    const terminal = row.terminal_at_ms === null ? null : timestamp(row.terminal_at_ms, "terminal_at_ms");
    if (expires !== issued + PAIRING_CODE_TTL_MS || reuse < issued + PAIRING_CODE_ISSUE_QUARANTINE_MS
      || (terminal !== null && (terminal < issued || terminal + PAIRING_CODE_TERMINAL_QUARANTINE_MS > reuse))) {
      fail("pairing_tombstone_invalid");
    }
    const codeDigests = validateDigestRows(row.code_digests);
    if (codeDigests.length === 0 && reuse > now) fail("pairing_tombstone_invalid");
    return { ...row, code_digests: codeDigests };
  });
  const seenPairings = new Set();
  for (const row of tombstones) if (seenPairings.has(row.pairing_id)) fail("pairing_store_invalid"); else seenPairings.add(row.pairing_id);
  const registrations = state.registrations.map((raw) => {
    const row = exact(raw, [
      "registration_request_id", "parameter_key_id", "parameter_digest_hex", "pairing_id",
      "endpoint_id", "remote_control_generation", "handshake_public_key_b64", "capabilities", "outcome",
      "retain_until_ms",
    ], "pairing_registration_invalid");
    id(row.registration_request_id, "registration_request_id");
    if (row.pairing_id !== null) id(row.pairing_id, "pairing_id");
    id(row.endpoint_id, "endpoint_id");
    id(row.remote_control_generation, "remote_control_generation");
    timestamp(row.retain_until_ms, "retain_until_ms");
    if (!KEY_ID.test(row.parameter_key_id) || !HEX.test(row.parameter_digest_hex)
      || (row.pairing_id !== null && !seenPairings.has(row.pairing_id)) || typeof row.handshake_public_key_b64 !== "string"
      || !Array.isArray(row.capabilities) || !row.outcome || typeof row.outcome !== "object") fail("pairing_registration_invalid");
    return structuredClone(row);
  });
  const requests = new Set();
  for (const row of registrations) {
    if (requests.has(row.registration_request_id)) fail("pairing_store_invalid");
    requests.add(row.registration_request_id);
  }
  return { ...structuredClone(state), tombstones, registrations };
}

function keyring(value) {
  if (!value || typeof value !== "object" || !Array.isArray(value.keys)
    || value.keys.length === 0 || value.keys.length > 32 || !KEY_ID.test(value.activeKeyId)) fail("pairing_keyring_invalid");
  const keys = new Map();
  for (const entry of value.keys) {
    const bytes = typeof entry?.key === "string" ? Buffer.byteLength(entry.key, "utf8") : 0;
    if (!KEY_ID.test(entry?.keyId) || bytes < 32 || bytes > 4096 || keys.has(entry.keyId)) fail("pairing_keyring_invalid");
    keys.set(entry.keyId, entry.key);
  }
  if (!keys.has(value.activeKeyId)) fail("pairing_keyring_invalid");
  return { keys, activeKeyId: value.activeKeyId, ids: [...keys.keys()].sort() };
}

function generateCode(randomBytes) {
  let code = "";
  while (code.length < 7) {
    const bytes = randomBytes(16);
    if (!Buffer.isBuffer(bytes) || bytes.length !== 16) fail("pairing_entropy_invalid");
    for (const byte of bytes) {
      if (byte >= 248) continue;
      code += PAIRING_CODE_ALPHABET[byte % 31];
      if (code.length === 7) break;
    }
  }
  return code;
}

export class PairingStore {
  #store; #keyring; #clock; #randomBytes; #active = new Map();

  constructor({ stateStore, digestKeyring, clock = Date.now, randomBytes = crypto.randomBytes } = {}, token) {
    if (token !== OPEN_TOKEN) fail("pairing_store_open_required");
    if (!stateStore || typeof stateStore.readNamespace !== "function" || typeof stateStore.compareAndSwap !== "function") {
      fail("pairing_state_store_invalid");
    }
    if (typeof clock !== "function" || typeof randomBytes !== "function") fail("pairing_runtime_invalid");
    this.#store = stateStore;
    this.#keyring = keyring(digestKeyring);
    this.#clock = clock;
    this.#randomBytes = randomBytes;
  }

  static async open(options) {
    const store = new PairingStore(options, OPEN_TOKEN);
    await store.#compact();
    await store.#syncKeyring();
    store.#rehydrate();
    return store;
  }

  #read() {
    const snapshot = this.#store.readNamespace(PAIRING_NAMESPACE);
    const now = this.#clock();
    if (!Number.isSafeInteger(now) || now < 0) fail("pairing_clock_invalid");
    return { revision: snapshot.namespace_revision, state: validateState(snapshot.value, now) };
  }

  async #compact() {
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const snapshot = this.#read();
      const now = this.#clock();
      if (!Number.isSafeInteger(now) || now < 0) fail("pairing_clock_invalid");
      const registrations = snapshot.state.registrations.filter((row) => Number(row.retain_until_ms) > now);
      const retainedPairings = new Set(registrations.map((row) => row.pairing_id).filter(Boolean));
      const tombstones = snapshot.state.tombstones.map((row) => (
        Number(row.reuse_after_ms) <= now && !retainedPairings.has(row.pairing_id) && row.code_digests.length > 0
          ? { ...row, code_digests: [] }
          : row
      ));
      if (registrations.length === snapshot.state.registrations.length
        && tombstones.every((row, index) => row === snapshot.state.tombstones[index])) return;
      try {
        await this.#store.compareAndSwap(PAIRING_NAMESPACE, snapshot.revision, {
          ...snapshot.state, registrations, tombstones,
        });
        return;
      } catch (error) {
        if (error?.code === "namespace_revision_conflict") continue;
        fail("pairing_persist_failed", error);
      }
    }
    fail("pairing_revision_conflict");
  }

  async #syncKeyring() {
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const snapshot = this.#read();
      const state = snapshot.state;
      const referenced = new Set([
        ...state.tombstones.flatMap((row) => row.code_digests.map((item) => item.key_id)),
        ...state.registrations.map((row) => row.parameter_key_id),
      ]);
      if ([...referenced].some((idValue) => !this.#keyring.keys.has(idValue))) fail("pairing_digest_key_unavailable");
      if (state.active_digest_key_id === this.#keyring.activeKeyId
        && JSON.stringify(state.digest_key_ids) === JSON.stringify(this.#keyring.ids)) return;
      const next = {
        ...state,
        keyring_revision: String(Number(state.keyring_revision) + 1),
        active_digest_key_id: this.#keyring.activeKeyId,
        digest_key_ids: this.#keyring.ids,
      };
      try {
        await this.#store.compareAndSwap(PAIRING_NAMESPACE, snapshot.revision, next);
        return;
      } catch (error) {
        if (error?.code === "namespace_revision_conflict") continue;
        fail("pairing_persist_failed", error);
      }
    }
    fail("pairing_revision_conflict");
  }

  #rehydrate() {
    const now = this.#clock();
    const { state } = this.#read();
    for (const registration of state.registrations) {
      const tombstone = state.tombstones.find((row) => row.pairing_id === registration.pairing_id);
      if (tombstone && tombstone.terminal_at_ms === null && Number(tombstone.expires_at_ms) > now) {
        this.#active.set(tombstone.pairing_id, Object.freeze({
          pairing_id: tombstone.pairing_id,
          endpoint_id: registration.endpoint_id,
          remote_control_generation: registration.remote_control_generation,
          handshake_public_key_b64: registration.handshake_public_key_b64,
          capabilities: Object.freeze([...registration.capabilities]),
          expires_at_ms: tombstone.expires_at_ms,
          code_digests: Object.freeze(tombstone.code_digests.map(Object.freeze)),
        }));
      }
    }
  }

  activePairing(pairingId) {
    const active = this.#active.get(pairingId);
    if (active && Number(active.expires_at_ms) <= this.#clock()) {
      this.#active.delete(pairingId);
      return null;
    }
    return active ?? null;
  }

  findActiveByCode(code) {
    if (typeof code !== "string" || !/^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u.test(code)) return null;
    const candidates = this.#keyring.ids.map((keyId) => ({
      key_id: keyId,
      digest_hex: codeDigest(this.#keyring.keys.get(keyId), code),
    }));
    const now = this.#clock();
    if (!Number.isSafeInteger(now) || now < 0) fail("pairing_clock_invalid");
    let match = null;
    for (const active of this.#active.values()) {
      if (Number(active.expires_at_ms) <= now) continue;
      let matched = false;
      for (const stored of active.code_digests) {
        const candidate = candidates.find((item) => item.key_id === stored.key_id);
        if (candidate) {
          matched = crypto.timingSafeEqual(
            Buffer.from(candidate.digest_hex, "hex"), Buffer.from(stored.digest_hex, "hex"),
          ) || matched;
        }
      }
      if (matched) match = active;
    }
    return match;
  }

  async terminate({ pairingId } = {}) {
    pairingId = id(pairingId, "pairing_id");
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const snapshot = this.#read();
      const index = snapshot.state.tombstones.findIndex((row) => row.pairing_id === pairingId);
      if (index < 0) fail("pairing_not_found");
      const current = snapshot.state.tombstones[index];
      if (current.terminal_at_ms !== null) {
        this.#active.delete(pairingId);
        return Object.freeze(structuredClone(current));
      }
      const now = this.#clock();
      if (!Number.isSafeInteger(now) || now < 0) fail("pairing_clock_invalid");
      if (now < Number(current.issued_at_ms)) fail("pairing_clock_invalid");
      const terminal = {
        ...current,
        terminal_at_ms: String(now),
        reuse_after_ms: String(Math.max(Number(current.reuse_after_ms), now + PAIRING_CODE_TERMINAL_QUARANTINE_MS)),
      };
      const tombstones = [...snapshot.state.tombstones];
      tombstones[index] = terminal;
      try {
        await this.#store.compareAndSwap(PAIRING_NAMESPACE, snapshot.revision, {
          ...snapshot.state, tombstones,
        });
        this.#active.delete(pairingId);
        return Object.freeze(structuredClone(terminal));
      } catch (error) {
        if (error?.code === "namespace_revision_conflict") continue;
        fail("pairing_persist_failed", error);
      }
    }
    fail("pairing_revision_conflict");
  }

  async consumeForControllerLease({
    pairingId, endpointId, remoteControlGeneration, reservationId, controllerLeaseId,
  } = {}) {
    pairingId = id(pairingId, "pairing_id");
    endpointId = id(endpointId, "endpoint_id");
    remoteControlGeneration = id(remoteControlGeneration, "remote_control_generation");
    reservationId = id(reservationId, "reservation_id");
    controllerLeaseId = id(controllerLeaseId, "controller_lease_id");
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const snapshot = this.#read();
      const index = snapshot.state.tombstones.findIndex((row) => row.pairing_id === pairingId);
      const registration = snapshot.state.registrations.find((row) => row.pairing_id === pairingId);
      if (index < 0 || !registration || snapshot.state.tombstones[index].terminal_at_ms !== null) {
        fail("pairing_not_active");
      }
      if (registration.endpoint_id !== endpointId
        || registration.remote_control_generation !== remoteControlGeneration) {
        fail("pairing_binding_mismatch");
      }
      const current = snapshot.state.tombstones[index];
      const now = this.#clock();
      if (!Number.isSafeInteger(now) || now < Number(current.issued_at_ms)
        || now >= Number(current.expires_at_ms)) fail("pairing_not_active");
      const terminal = {
        ...current,
        terminal_at_ms: String(now),
        reuse_after_ms: String(Math.max(
          Number(current.reuse_after_ms), now + PAIRING_CODE_TERMINAL_QUARANTINE_MS,
        )),
      };
      const tombstones = [...snapshot.state.tombstones];
      tombstones[index] = terminal;
      try {
        await this.#store.compareAndSwap(PAIRING_NAMESPACE, snapshot.revision, {
          ...snapshot.state, tombstones,
        });
        this.#active.delete(pairingId);
        return Object.freeze({
          pairing_id: pairingId,
          endpoint_id: endpointId,
          remote_control_generation: remoteControlGeneration,
          reservation_id: reservationId,
          controller_lease_id: controllerLeaseId,
          consumed_at_ms: String(now),
        });
      } catch (error) {
        if (error?.code === "namespace_revision_conflict") continue;
        fail("pairing_persist_failed", error);
      }
    }
    fail("pairing_revision_conflict");
  }

  async invalidateForRelayRestart() {
    for (let attempt = 0; attempt < 8; attempt += 1) {
      const snapshot = this.#read();
      const now = this.#clock();
      if (!Number.isSafeInteger(now) || now < 0) fail("pairing_clock_invalid");
      let invalidated = 0;
      const tombstones = snapshot.state.tombstones.map((row) => {
        if (row.terminal_at_ms !== null || Number(row.expires_at_ms) <= now) return row;
        invalidated += 1;
        const terminalAt = Math.max(now, Number(row.issued_at_ms));
        return {
          ...row,
          terminal_at_ms: String(terminalAt),
          reuse_after_ms: String(Math.max(
            Number(row.reuse_after_ms), terminalAt + PAIRING_CODE_TERMINAL_QUARANTINE_MS,
          )),
        };
      });
      if (invalidated === 0) {
        this.#active.clear();
        return 0;
      }
      try {
        await this.#store.compareAndSwap(PAIRING_NAMESPACE, snapshot.revision, {
          ...snapshot.state, tombstones,
        });
        this.#active.clear();
        return invalidated;
      } catch (error) {
        if (error?.code === "namespace_revision_conflict") continue;
        fail("pairing_persist_failed", error);
      }
    }
    fail("pairing_revision_conflict");
  }

  async register({ request, relayEpoch, sealCode } = {}) {
    if (!request || typeof request !== "object" || typeof sealCode !== "function") fail("pairing_registration_invalid");
    const registrationId = id(request.registration_request_id, "registration_request_id");
    id(request.endpoint_id, "endpoint_id");
    id(request.remote_control_generation, "remote_control_generation");
    id(relayEpoch, "relay_epoch");
    const parameters = { ...request };
    delete parameters.request_id;
    for (let casAttempt = 0; casAttempt < 8; casAttempt += 1) {
      const snapshot = this.#read();
      const known = snapshot.state.registrations.find((row) => row.registration_request_id === registrationId);
      if (known) {
        if (known.outcome.relay_epoch !== undefined && known.outcome.relay_epoch !== relayEpoch) {
          fail("pairing_registration_stale_relay_epoch");
        }
        const knownKey = this.#keyring.keys.get(known.parameter_key_id);
        const knownHash = digest(knownKey, "pinvou-pairing-registration-v1\0", parameters);
        if (!crypto.timingSafeEqual(Buffer.from(known.parameter_digest_hex, "hex"), Buffer.from(knownHash, "hex"))) {
          fail("pairing_registration_conflict");
        }
        return Object.freeze({
          result: Object.freeze({ ...structuredClone(known.outcome), request_id: request.request_id }),
          replayed: true,
        });
      }
      const now = this.#clock();
      if (!Number.isSafeInteger(now) || now < 0) fail("pairing_clock_invalid");
      let code;
      let digests;
      for (let attempt = 0; attempt < MAX_GENERATION_ATTEMPTS; attempt += 1) {
        code = generateCode(this.#randomBytes);
        digests = this.#keyring.ids.map((keyId) => ({
          key_id: keyId, digest_hex: codeDigest(this.#keyring.keys.get(keyId), code),
        }));
        const collision = snapshot.state.tombstones.some((row) => Number(row.reuse_after_ms) > now
          && row.code_digests.some((stored) => digests.some((candidate) => candidate.key_id === stored.key_id
            && crypto.timingSafeEqual(Buffer.from(candidate.digest_hex, "hex"), Buffer.from(stored.digest_hex, "hex")))));
        if (!collision) break;
        code = null;
      }
      if (code === null) {
        if (snapshot.state.registrations.length >= MAX_RECORDS) fail("pairing_capacity_reached");
        const outcome = {
          v: 1, type: "error", request_id: request.request_id,
          registration_request_id: registrationId,
          remote_control_generation: request.remote_control_generation,
          code: "pairing_code_generation_failed", message: "pairing code generation failed",
        };
        validateControllerMessage(outcome, "relay");
        const registration = {
          registration_request_id: registrationId, parameter_key_id: this.#keyring.activeKeyId,
          parameter_digest_hex: digest(
            this.#keyring.keys.get(this.#keyring.activeKeyId), "pinvou-pairing-registration-v1\0", parameters,
          ), pairing_id: null, endpoint_id: request.endpoint_id,
          remote_control_generation: request.remote_control_generation,
          handshake_public_key_b64: request.handshake_public_key_b64,
          capabilities: [...request.capabilities], outcome,
          retain_until_ms: String(now + PAIRING_CODE_ISSUE_QUARANTINE_MS),
        };
        try {
          await this.#store.compareAndSwap(PAIRING_NAMESPACE, snapshot.revision, {
            ...snapshot.state, registrations: [...snapshot.state.registrations, registration],
          });
          return Object.freeze({ result: Object.freeze(outcome), replayed: false });
        } catch (error) {
          if (error?.code === "namespace_revision_conflict") continue;
          fail("pairing_persist_failed", error);
        }
      }
      if (snapshot.state.tombstones.length >= MAX_RECORDS || snapshot.state.registrations.length >= MAX_RECORDS) {
        fail("pairing_capacity_reached");
      }
      let pairingId = null;
      for (let attempt = 0; attempt < MAX_GENERATION_ATTEMPTS; attempt += 1) {
        const pairingEntropy = this.#randomBytes(18);
        if (!Buffer.isBuffer(pairingEntropy) || pairingEntropy.length !== 18) fail("pairing_entropy_invalid");
        const candidate = `pairing-${pairingEntropy.toString("base64url")}`;
        if (!snapshot.state.tombstones.some((row) => row.pairing_id === candidate)) {
          pairingId = candidate;
          break;
        }
      }
      if (pairingId === null) fail("pairing_id_generation_exhausted");
      const issued = String(now);
      const expires = String(now + PAIRING_CODE_TTL_MS);
      const binding = {
        registration_request_id: registrationId,
        pairing_id: pairingId,
        remote_control_generation: request.remote_control_generation,
        relay_epoch: relayEpoch,
        expires_at_ms: expires,
      };
      const sealed = sealCode({ code, codeWrapPublicKeyB64: request.code_wrap_public_key_b64, binding });
      const outcome = {
        v: 1, type: "desktop_pairing_registered", request_id: request.request_id,
        registration_request_id: registrationId, pairing_id: pairingId,
        remote_control_generation: request.remote_control_generation, sealed_code_b64: sealed,
        expires_at_ms: expires, relay_epoch: relayEpoch,
      };
      validateControllerMessage(outcome, "relay");
      const tombstone = {
        pairing_id: pairingId, code_digests: digests, issued_at_ms: issued, expires_at_ms: expires,
        reuse_after_ms: String(now + PAIRING_CODE_ISSUE_QUARANTINE_MS), terminal_at_ms: null,
      };
      const registration = {
        registration_request_id: registrationId, parameter_key_id: this.#keyring.activeKeyId,
        parameter_digest_hex: digest(
          this.#keyring.keys.get(this.#keyring.activeKeyId), "pinvou-pairing-registration-v1\0", parameters,
        ), pairing_id: pairingId, endpoint_id: request.endpoint_id,
        remote_control_generation: request.remote_control_generation,
        handshake_public_key_b64: request.handshake_public_key_b64,
        capabilities: [...request.capabilities], outcome,
        retain_until_ms: tombstone.reuse_after_ms,
      };
      const next = {
        ...snapshot.state,
        tombstones: [...snapshot.state.tombstones, tombstone],
        registrations: [...snapshot.state.registrations, registration],
      };
      try {
        await this.#store.compareAndSwap(PAIRING_NAMESPACE, snapshot.revision, next);
        const active = Object.freeze({
          pairing_id: pairingId, endpoint_id: request.endpoint_id,
          remote_control_generation: request.remote_control_generation,
          handshake_public_key_b64: request.handshake_public_key_b64,
          capabilities: Object.freeze([...request.capabilities]), expires_at_ms: expires,
          code_digests: Object.freeze(digests.map(Object.freeze)),
        });
        this.#active.set(pairingId, active);
        return Object.freeze({ result: Object.freeze(outcome), replayed: false });
      } catch (error) {
        if (error?.code === "namespace_revision_conflict") continue;
        fail("pairing_persist_failed", error);
      }
    }
    fail("pairing_revision_conflict");
  }
}
