import crypto from "node:crypto";

const ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$/u;
const SOURCE = /^[\x21-\x7e]{1,128}$/u;
const CODE = /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u;
const REVISION = /^(0|[1-9][0-9]{0,19})$/u;
const U64_MAX = 18_446_744_073_709_551_615n;
const RESERVED_TIMEOUT_MS = 10_000;
const HANDSHAKE_TIMEOUT_MS = 20_000;
const RESERVATION_ID_ATTEMPTS = 32;
const CAS_ATTEMPTS = 8;
const RELEASE_REASONS = new Set([
  "failed", "owner_disconnected", "endpoint_disconnected", "handshake_failed", "refreshed", "timeout",
]);

export class ControllerReservationError extends Error {
  constructor(code) {
    super(code);
    this.name = "ControllerReservationError";
    this.code = code;
  }
}

function fail(code) { throw new ControllerReservationError(code); }

function identifier(value, label) {
  if (typeof value !== "string" || !ID.test(value)) fail(`${label}_invalid`);
  return value;
}

function sourceIdentifier(value) {
  if (typeof value !== "string" || !SOURCE.test(value)) fail("source_invalid");
  return value;
}

function revision(value) {
  if (typeof value !== "string" || !REVISION.test(value)) fail("interaction_slot_revision_invalid");
  const parsed = BigInt(value);
  if (parsed > U64_MAX) fail("interaction_slot_revision_invalid");
  return parsed;
}

function slotOwner(value) {
  if (value === null) return null;
  if (!value || typeof value !== "object" || Array.isArray(value)
    || !["web_lease", "desktop_reservation", "controller_lease"].includes(value.kind)) {
    fail("interaction_slot_owner_invalid");
  }
  const keys = Object.keys(value).sort();
  if (keys.length !== 2 || keys[0] !== "kind" || keys[1] !== "owner_id") {
    fail("interaction_slot_owner_invalid");
  }
  return Object.freeze({ kind: value.kind, owner_id: identifier(value.owner_id, "owner_id") });
}

function normalizeCode(value) {
  if (typeof value !== "string" || value.length > 32) return null;
  const normalized = value.toUpperCase().replace(/[\s-]+/gu, "");
  return CODE.test(normalized) ? normalized : null;
}

function publicInvalid() { fail("pairing_code_invalid"); }

function publicKeyHash(value) {
  if (typeof value !== "string" || value.length !== 44
    || !/^[A-Za-z0-9+/]+={0,2}$/u.test(value)) publicInvalid();
  const bytes = Buffer.from(value, "base64");
  if (bytes.length !== 32 || bytes.toString("base64") !== value) publicInvalid();
  return crypto.createHash("sha256").update(bytes).digest("hex");
}

function consume(buckets, key, limit, windowMs, now, maxBuckets) {
  const bucket = buckets.get(key);
  if (!bucket || now - bucket.started_at >= windowMs) {
    if (!bucket && buckets.size >= maxBuckets) {
      let scanned = 0;
      for (const [candidate, value] of buckets) {
        if (now - value.started_at >= windowMs) buckets.delete(candidate);
        scanned += 1;
        if (scanned >= 64) break;
      }
      if (buckets.size >= maxBuckets) return false;
    }
    buckets.set(key, { started_at: now, count: 1 });
    return true;
  }
  if (bucket.count >= limit) return false;
  bucket.count += 1;
  return true;
}

export class EndpointInteractionSlots {
  #slots = new Map();

  read(endpointId) {
    endpointId = identifier(endpointId, "endpoint_id");
    const slot = this.#slots.get(endpointId);
    return Object.freeze({
      revision: slot?.revision ?? "0",
      owner: slot?.owner ? structuredClone(slot.owner) : null,
    });
  }

  compareAndSwap(endpointId, expectedRevision, owner) {
    endpointId = identifier(endpointId, "endpoint_id");
    const expected = revision(expectedRevision);
    owner = slotOwner(owner);
    const current = this.#slots.get(endpointId);
    const currentRevision = current?.revision ?? "0";
    if (currentRevision !== expectedRevision || BigInt(currentRevision) !== expected) {
      fail("interaction_slot_revision_conflict");
    }
    const next = expected + 1n;
    if (next > U64_MAX) fail("interaction_slot_revision_exhausted");
    const nextRevision = String(next);
    if (owner === null) this.#slots.set(endpointId, { revision: nextRevision, owner: null });
    else this.#slots.set(endpointId, { revision: nextRevision, owner });
    return this.read(endpointId);
  }

  acquireWeb({ endpointId, leaseId } = {}) {
    endpointId = identifier(endpointId, "endpoint_id");
    leaseId = identifier(leaseId, "lease_id");
    for (let attempt = 0; attempt < CAS_ATTEMPTS; attempt += 1) {
      const current = this.read(endpointId);
      if (current.owner?.kind !== undefined && current.owner.kind !== "web_lease") {
        fail("legacy_endpoint_not_found");
      }
      try {
        return this.compareAndSwap(endpointId, current.revision, { kind: "web_lease", owner_id: leaseId });
      } catch (error) {
        if (error?.code === "interaction_slot_revision_conflict") continue;
        throw error;
      }
    }
    fail("interaction_slot_revision_conflict");
  }

  releaseWeb({ endpointId, leaseId } = {}) {
    const current = this.read(endpointId);
    if (current.owner?.kind !== "web_lease" || current.owner.owner_id !== leaseId) return false;
    this.compareAndSwap(endpointId, current.revision, null);
    return true;
  }

  releaseController({ endpointId, controllerLeaseId } = {}) {
    const current = this.read(endpointId);
    if (current.owner?.kind !== "controller_lease"
      || current.owner.owner_id !== controllerLeaseId) return false;
    this.compareAndSwap(endpointId, current.revision, null);
    return true;
  }
}

export class ControllerReservationManager {
  #pairings; #slots; #clock; #randomBytes; #onInvalidated; #audit;
  #reservations = new Map(); #sourceBuckets = new Map(); #connectionBuckets = new Map();
  #globalBuckets = new Map(); #requests = new Map(); #releases = new Map(); #limits;
  #codeProofKey = crypto.randomBytes(32);

  constructor({
    pairingStore, interactionSlots, clock = Date.now, randomBytes = crypto.randomBytes,
    onPairingInvalidated = async () => {}, audit = () => {},
    sourceLimit = 30, connectionLimit = 10, globalLimit = 10_000, windowMs = 60_000,
    maxSourceBuckets = 4096, maxConnectionBuckets = 4096,
  } = {}) {
    if (!pairingStore || typeof pairingStore.findActiveByCode !== "function"
      || typeof pairingStore.terminate !== "function" || !(interactionSlots instanceof EndpointInteractionSlots)
      || typeof clock !== "function" || typeof randomBytes !== "function"
      || typeof onPairingInvalidated !== "function" || typeof audit !== "function") {
      fail("controller_reservation_config_invalid");
    }
    for (const value of [sourceLimit, connectionLimit, globalLimit, windowMs,
      maxSourceBuckets, maxConnectionBuckets]) {
      if (!Number.isSafeInteger(value) || value <= 0) fail("controller_reservation_config_invalid");
    }
    this.#pairings = pairingStore;
    this.#slots = interactionSlots;
    this.#clock = clock;
    this.#randomBytes = randomBytes;
    this.#onInvalidated = onPairingInvalidated;
    this.#audit = audit;
    this.#limits = Object.freeze({
      sourceLimit, connectionLimit, globalLimit, windowMs, maxSourceBuckets, maxConnectionBuckets,
    });
  }

  #emit(event) {
    try { this.#audit(event); } catch { /* audit must never change authorization state */ }
  }

  #now(futureMs = 0) {
    const now = this.#clock();
    if (!Number.isSafeInteger(now) || now < 0 || now > Number.MAX_SAFE_INTEGER - futureMs) {
      fail("controller_reservation_clock_invalid");
    }
    return now;
  }

  #reservationId() {
    for (let attempt = 0; attempt < RESERVATION_ID_ATTEMPTS; attempt += 1) {
      const entropy = this.#randomBytes(18);
      if (!Buffer.isBuffer(entropy) || entropy.length !== 18) fail("controller_reservation_entropy_invalid");
      const candidate = `reservation-${entropy.toString("base64url")}`;
      if (!this.#reservations.has(candidate)) return candidate;
    }
    fail("controller_reservation_entropy_exhausted");
  }

  #codeProof(code) {
    return crypto.createHmac("sha256", this.#codeProofKey).update(code, "ascii").digest();
  }

  claim({ pairingCode, requestId, socketIdentity, source, handshakePublicKeyB64 } = {}) {
    requestId = identifier(requestId, "request_id");
    socketIdentity = identifier(socketIdentity, "socket_identity");
    source = sourceIdentifier(source);
    const previousRequest = this.#requests.get(requestId);
    if (previousRequest) {
      const code = normalizeCode(pairingCode);
      const keyHash = publicKeyHash(handshakePublicKeyB64);
      const previous = this.#reservations.get(previousRequest.reservation_id);
      if (code && previous?.owner_socket_identity === socketIdentity
        && previous.controller_public_key_sha256 === keyHash
        && ["reserved", "handshake_pending"].includes(previous.state)
        && crypto.timingSafeEqual(previousRequest.code_proof, this.#codeProof(code))) return previous;
      this.#emit("claim_request_conflict");
      publicInvalid();
    }
    const now = this.#now(RESERVED_TIMEOUT_MS);
    if (!consume(this.#sourceBuckets, source, this.#limits.sourceLimit, this.#limits.windowMs, now,
      this.#limits.maxSourceBuckets)
      || !consume(this.#connectionBuckets, socketIdentity, this.#limits.connectionLimit,
        this.#limits.windowMs, now, this.#limits.maxConnectionBuckets)
      || !consume(this.#globalBuckets, "global", this.#limits.globalLimit,
        this.#limits.windowMs, now, 1)) {
      this.#emit("claim_rate_limited");
      publicInvalid();
    }
    const code = normalizeCode(pairingCode);
    if (!code) {
      this.#emit("claim_invalid");
      publicInvalid();
    }
    const keyHash = publicKeyHash(handshakePublicKeyB64);
    const pairing = this.#pairings.findActiveByCode(code);
    if (!pairing) {
      this.#emit("claim_invalid");
      publicInvalid();
    }
    let reservationId = null;
    for (let attempt = 0; attempt < CAS_ATTEMPTS; attempt += 1) {
      const current = this.#slots.read(pairing.endpoint_id);
      if (current.owner !== null) {
        const incumbent = current.owner.kind === "desktop_reservation"
          ? this.#reservations.get(current.owner.owner_id) : null;
        if (incumbent?.pairing_id === pairing.pairing_id) {
          this.#emit("claim_lost");
          publicInvalid();
        }
        this.#emit("claim_busy");
        fail("controller_busy");
      }
      reservationId ??= this.#reservationId();
      const reservation = Object.freeze({
        reservation_id: reservationId,
        request_id: requestId,
        pairing_id: pairing.pairing_id,
        endpoint_id: pairing.endpoint_id,
        remote_control_generation: pairing.remote_control_generation,
        owner_socket_identity: socketIdentity,
        controller_public_key_sha256: keyHash,
        state: "reserved",
        reserved_deadline_ms: String(now + RESERVED_TIMEOUT_MS),
        handshake_deadline_ms: null,
      });
      try {
        this.#slots.compareAndSwap(pairing.endpoint_id, current.revision, {
          kind: "desktop_reservation", owner_id: reservationId,
        });
        this.#reservations.set(reservationId, reservation);
        this.#requests.set(requestId, Object.freeze({
          reservation_id: reservationId, code_proof: this.#codeProof(code),
        }));
        this.#pairings.retainForReservation?.(pairing.pairing_id);
        this.#emit("claim_reserved");
        return reservation;
      } catch (error) {
        if (error?.code === "interaction_slot_revision_conflict") continue;
        throw error;
      }
    }
    fail("controller_busy");
  }

  beginHandshake({ reservationId, socketIdentity } = {}) {
    const current = this.#owned(reservationId, socketIdentity);
    if (current.state !== "reserved") fail("reservation_state_invalid");
    const now = this.#now(HANDSHAKE_TIMEOUT_MS);
    if (now >= Number(current.reserved_deadline_ms)) fail("reservation_expired");
    const next = Object.freeze({
      ...current, state: "handshake_pending", handshake_deadline_ms: String(now + HANDSHAKE_TIMEOUT_MS),
    });
    this.#reservations.set(reservationId, next);
    return next;
  }

  #owned(reservationId, socketIdentity) {
    reservationId = identifier(reservationId, "reservation_id");
    socketIdentity = identifier(socketIdentity, "socket_identity");
    const reservation = this.#reservations.get(reservationId);
    if (!reservation || reservation.owner_socket_identity !== socketIdentity) fail("reservation_owner_invalid");
    return reservation;
  }

  // A read-only ownership check for async claim/commit boundaries. A completed
  // release is distinct from a reservation owned by another socket.
  reservationForOwner({ reservationId, socketIdentity } = {}) {
    reservationId = identifier(reservationId, "reservation_id");
    socketIdentity = identifier(socketIdentity, "socket_identity");
    const reservation = this.#reservations.get(reservationId);
    if (!reservation) return null;
    if (reservation.owner_socket_identity !== socketIdentity) fail("reservation_owner_invalid");
    return reservation;
  }

  async release({ reservationId, socketIdentity, reason = "failed" } = {}) {
    let reservation = this.#owned(reservationId, socketIdentity);
    if (!RELEASE_REASONS.has(reason)) fail("reservation_release_reason_invalid");
    const pending = this.#releases.get(reservation.reservation_id);
    if (pending) return pending;
    if (reservation.state !== "invalidating") {
      reservation = Object.freeze({
        ...reservation, state: "invalidating", invalidation_reason: reason,
      });
      this.#reservations.set(reservation.reservation_id, reservation);
    }
    const operation = this.#invalidate(reservation);
    this.#releases.set(reservation.reservation_id, operation);
    try { return await operation; } finally { this.#releases.delete(reservation.reservation_id); }
  }

  promoteToLease({ reservationId, socketIdentity, controllerLeaseId } = {}) {
    const reservation = this.#owned(reservationId, socketIdentity);
    controllerLeaseId = identifier(controllerLeaseId, "controller_lease_id");
    if (reservation.state !== "handshake_pending") fail("reservation_state_invalid");
    if (this.#now() >= Number(reservation.handshake_deadline_ms)) fail("reservation_expired");
    const slot = this.#slots.read(reservation.endpoint_id);
    if (slot.owner?.kind !== "desktop_reservation"
      || slot.owner.owner_id !== reservation.reservation_id) {
      fail("reservation_slot_lost");
    }
    this.#slots.compareAndSwap(reservation.endpoint_id, slot.revision, {
      kind: "controller_lease", owner_id: controllerLeaseId,
    });
    this.#reservations.delete(reservation.reservation_id);
    this.#requests.delete(reservation.request_id);
    this.#pairings.releaseReservation?.(reservation.pairing_id);
    this.#emit("reservation_promoted");
    return Object.freeze({
      ...reservation, state: "controller_connected", controller_lease_id: controllerLeaseId,
    });
  }

  async #invalidate(reservation) {
    try {
      await this.#pairings.terminate({ pairingId: reservation.pairing_id });
      await this.#onInvalidated({
        pairing_id: reservation.pairing_id,
        endpoint_id: reservation.endpoint_id,
        remote_control_generation: reservation.remote_control_generation,
        reason: reservation.invalidation_reason,
      });
    } catch (error) {
      this.#emit("reservation_invalidation_failed");
      throw error;
    }
    const slot = this.#slots.read(reservation.endpoint_id);
    if (slot.owner?.kind === "desktop_reservation"
      && slot.owner.owner_id === reservation.reservation_id) {
      this.#slots.compareAndSwap(reservation.endpoint_id, slot.revision, null);
    }
    this.#reservations.delete(reservation.reservation_id);
    this.#requests.delete(reservation.request_id);
    this.#pairings.releaseReservation?.(reservation.pairing_id);
    this.#emit("reservation_released");
    return true;
  }

  async releaseOwned({ socketIdentity, reason = "owner_disconnected" } = {}) {
    socketIdentity = identifier(socketIdentity, "socket_identity");
    const owned = [...this.#reservations.values()]
      .filter((reservation) => reservation.owner_socket_identity === socketIdentity);
    return this.#releaseAll(owned, reason);
  }

  async releaseEndpoint({ endpointId, reason = "endpoint_disconnected" } = {}) {
    endpointId = identifier(endpointId, "endpoint_id");
    const owned = [...this.#reservations.values()]
      .filter((reservation) => reservation.endpoint_id === endpointId);
    return this.#releaseAll(owned, reason);
  }

  async #releaseAll(reservations, reason) {
    if (!RELEASE_REASONS.has(reason)) fail("reservation_release_reason_invalid");
    const results = await Promise.allSettled(reservations.map((reservation) => this.release({
      reservationId: reservation.reservation_id,
      socketIdentity: reservation.owner_socket_identity,
      reason,
    })));
    const failures = results.filter((result) => result.status === "rejected").map((result) => result.reason);
    if (failures.length > 0) throw new AggregateError(failures, "reservation_cleanup_failed");
    return reservations.length;
  }

  async expire() {
    const now = this.#now();
    const expired = [...this.#reservations.values()].filter((reservation) => (
      reservation.state === "reserved"
        ? Number(reservation.reserved_deadline_ms) <= now
        : reservation.state === "handshake_pending"
          ? Number(reservation.handshake_deadline_ms) <= now
          : reservation.state === "invalidating"
    ));
    return this.#releaseAll(expired, "timeout");
  }

  acquireWeb(input) { return this.#slots.acquireWeb(input); }
  releaseWeb(input) { return this.#slots.releaseWeb(input); }
  releaseControllerLease(input) { return this.#slots.releaseController(input); }
}
