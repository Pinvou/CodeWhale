import crypto from "node:crypto";

import {
  CONTROLLER_PROTOCOL_VERSION,
  buildPairingTranscriptPreimage,
  controllerCapabilityHash,
  negotiateControllerCapabilities,
  validateControllerMessage,
  verifyReceiptCommitment,
} from "./protocol-controller.js";

const ID = /^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$/u;
const HANDSHAKE_TIMEOUT_MS = 20_000;
const RESUME_BUDGET_MS = 30_000;

export class ControllerLeaseError extends Error {
  constructor(code, options) {
    super(code, options);
    this.name = "ControllerLeaseError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new ControllerLeaseError(code, cause ? { cause } : undefined);
}

function id(value, label) {
  if (typeof value !== "string" || !ID.test(value)) fail(`${label}_invalid`);
  return value;
}

function role(value) {
  if (!new Set(["A", "B"]).has(value)) fail("controller_role_invalid");
  return value;
}

function opposite(value) { return value === "A" ? "B" : "A"; }

function random(manager, bytes, label) {
  const value = manager(bytes);
  if (!Buffer.isBuffer(value) || value.length !== bytes) fail(`${label}_entropy_invalid`);
  return value;
}

function randomId(manager, prefix) {
  return `${prefix}-${random(manager, 18, prefix).toString("base64url")}`;
}

function randomLeaseEpoch(manager) {
  return random(manager, 8, "lease_epoch").readBigUInt64BE().toString();
}

function tokenDigest(binding, token) {
  const hash = crypto.createHash("sha256").update("pinvou-controller-resume-token-v1\0", "ascii");
  for (const value of [
    binding.role,
    binding.relay_epoch,
    binding.connection_id,
    binding.controller_lease_id,
    binding.lease_epoch,
    binding.remote_control_generation,
  ]) {
    const bytes = Buffer.from(value, "utf8");
    const length = Buffer.alloc(4);
    length.writeUInt32BE(bytes.length);
    hash.update(length).update(bytes);
  }
  return hash.update(token).digest("hex");
}

function receiptSecret(value) {
  if (typeof value !== "string" || value.length !== 44) fail("receipt_secret_invalid");
  const bytes = Buffer.from(value, "base64");
  if (bytes.length !== 32 || bytes.toString("base64") !== value) fail("receipt_secret_invalid");
  return bytes;
}

function resumeToken(value) {
  if (typeof value !== "string" || value.length !== 44) fail("resume_token_invalid");
  const bytes = Buffer.from(value, "base64");
  if (bytes.length !== 32 || bytes.toString("base64") !== value) fail("resume_token_invalid");
  return bytes;
}

function resumeEpoch(value, label) {
  if (typeof value !== "string" || !ID.test(value)) fail(`${label}_invalid`);
  return value;
}

function socketsValue(sockets, roleValue) {
  if (!Object.hasOwn(sockets, roleValue)) fail("controller_resume_sockets_invalid");
  return sockets[roleValue];
}

export class ControllerLeaseManager {
  #pairings; #reservations; #relayEpoch; #signingKeyId; #sign; #resolveControlled;
  #clock; #randomBytes; #audit; #onLeaseTerminated; #handshakes = new Map(); #leases = new Map();
  #pendingLeaseIds = new Set();

  constructor({
    pairingStore,
    reservationManager,
    relayEpoch,
    signingKeyId,
    signTranscript,
    resolveControlledEndpoint,
    clock = Date.now,
    randomBytes = crypto.randomBytes,
    audit = () => {},
    onLeaseTerminated = async () => {},
  } = {}) {
    if (!pairingStore || typeof pairingStore.activePairing !== "function"
      || typeof pairingStore.consumeForControllerLease !== "function"
      || !reservationManager || typeof reservationManager.claim !== "function"
      || typeof reservationManager.beginHandshake !== "function"
      || typeof reservationManager.promoteToLease !== "function"
      || typeof reservationManager.release !== "function"
      || typeof reservationManager.releaseControllerLease !== "function"
      || typeof relayEpoch !== "string" || !ID.test(relayEpoch)
      || typeof signingKeyId !== "string" || !ID.test(signingKeyId)
      || typeof signTranscript !== "function" || typeof resolveControlledEndpoint !== "function"
      || typeof clock !== "function" || typeof randomBytes !== "function" || typeof audit !== "function"
      || typeof onLeaseTerminated !== "function") {
      fail("controller_lease_config_invalid");
    }
    this.#pairings = pairingStore;
    this.#reservations = reservationManager;
    this.#relayEpoch = relayEpoch;
    this.#signingKeyId = signingKeyId;
    this.#sign = signTranscript;
    this.#resolveControlled = resolveControlledEndpoint;
    this.#clock = clock;
    this.#randomBytes = randomBytes;
    this.#audit = audit;
    this.#onLeaseTerminated = onLeaseTerminated;
  }

  #emit(event) {
    try { this.#audit(event); } catch { /* audit cannot affect authorization */ }
  }

  #now(futureMs = 0) {
    const now = this.#clock();
    if (!Number.isSafeInteger(now) || now < 0 || now > Number.MAX_SAFE_INTEGER - futureMs) {
      fail("controller_lease_clock_invalid");
    }
    return now;
  }

  #uniqueLeaseId() {
    for (let attempt = 0; attempt < 32; attempt += 1) {
      const candidate = randomId(this.#randomBytes, "controller-lease");
      if (!this.#leases.has(candidate) && !this.#pendingLeaseIds.has(candidate)) {
        this.#pendingLeaseIds.add(candidate);
        return candidate;
      }
    }
    fail("controller_lease_id_generation_exhausted");
  }

  async claim({ message, socketIdentity, source } = {}) {
    socketIdentity = id(socketIdentity, "socket_identity");
    let request;
    try { request = validateControllerMessage(message, "A"); } catch (error) {
      fail("controller_claim_invalid", error);
    }
    if (request.type !== "controller_pairing_claim") fail("controller_claim_invalid");
    const reservation = this.#reservations.claim({
      pairingCode: request.pairing_code,
      requestId: request.claim_request_id,
      socketIdentity,
      source,
      handshakePublicKeyB64: request.handshake_public_key_b64,
    });
    const existing = this.#handshakes.get(reservation.reservation_id);
    if (existing) {
      if (JSON.stringify(existing.requested_capabilities_a)
        !== JSON.stringify([...request.capabilities].sort())) {
        fail("controller_claim_conflict");
      }
      return validateControllerMessage({
        ...existing.reserved_message, request_id: request.request_id,
      }, "relay");
    }

    try {
      const pairing = this.#pairings.activePairing(reservation.pairing_id);
      if (!pairing || pairing.endpoint_id !== reservation.endpoint_id
        || pairing.remote_control_generation !== reservation.remote_control_generation) {
        fail("pairing_code_invalid");
      }
      const controlled = await this.#resolveControlled(pairing.endpoint_id);
      if (!controlled || typeof controlled !== "object") fail("endpoint_offline");
      const controlledSocketIdentity = id(controlled.socketIdentity, "controlled_socket_identity");
      const capabilities = negotiateControllerCapabilities(request.capabilities, pairing.capabilities);
      const pending = this.#reservations.beginHandshake({
        reservationId: reservation.reservation_id, socketIdentity,
      });
      const controllerConnectionIdentity = randomId(this.#randomBytes, "controller");
      const transcript = Object.freeze({
        schema: "controller_pairing_transcript/v1",
        relay_epoch: this.#relayEpoch,
        pairing_id: pairing.pairing_id,
        reservation_id: reservation.reservation_id,
        endpoint_id: pairing.endpoint_id,
        remote_control_generation: pairing.remote_control_generation,
        controller_connection_identity: controllerConnectionIdentity,
        role_a: "A",
        handshake_public_key_a_b64: request.handshake_public_key_b64,
        role_b: "B",
        handshake_public_key_b_b64: pairing.handshake_public_key_b64,
        protocol_version: CONTROLLER_PROTOCOL_VERSION,
        negotiated_capabilities: capabilities,
        capability_hash: controllerCapabilityHash(capabilities),
        expires_at_ms: pending.handshake_deadline_ms,
        nonce_b64: random(this.#randomBytes, 16, "transcript_nonce").toString("base64"),
      });
      const preimage = buildPairingTranscriptPreimage(transcript);
      const transcriptHash = crypto.createHash("sha256").update(preimage).digest("hex");
      const signature = await this.#sign(preimage);
      if (!Buffer.isBuffer(signature) || signature.length !== 64) fail("relay_signature_invalid");
      if (this.#now() >= Number(pending.handshake_deadline_ms)) fail("controller_handshake_expired");
      const reserved = validateControllerMessage({
        v: CONTROLLER_PROTOCOL_VERSION,
        type: "controller_pairing_reserved",
        request_id: request.request_id,
        pairing_id: pairing.pairing_id,
        reservation_id: reservation.reservation_id,
        remote_control_generation: pairing.remote_control_generation,
        relay_epoch: this.#relayEpoch,
        transcript,
        transcript_hash: transcriptHash,
        relay_signing_key_id: this.#signingKeyId,
        signature_algorithm: "ed25519",
        reservation_expires_at_ms: reservation.reserved_deadline_ms,
        handshake_expires_at_ms: pending.handshake_deadline_ms,
        relay_signature_b64: signature.toString("base64"),
      }, "relay");
      this.#handshakes.set(reservation.reservation_id, {
        state: "handshake_pending",
        reservation: pending,
        request_id: request.request_id,
        controller_connection_identity: controllerConnectionIdentity,
        transcript_hash: transcriptHash,
        capabilities,
        requested_capabilities_a: Object.freeze([...request.capabilities].sort()),
        sockets: Object.freeze({ A: socketIdentity, B: controlledSocketIdentity }),
        commitments: new Map(),
        finished: new Set(),
        receipts: new Set(),
        reserved_message: reserved,
        commit_promise: null,
      });
      this.#emit("handshake_started");
      return reserved;
    } catch (error) {
      try {
        await this.#reservations.release({
          reservationId: reservation.reservation_id,
          socketIdentity,
          reason: "handshake_failed",
        });
      } catch (cleanupError) {
        throw new AggregateError([error, cleanupError], "controller_claim_cleanup_failed");
      }
      throw error;
    }
  }

  #handshake(message, senderRole, socketIdentity, expectedType) {
    senderRole = role(senderRole);
    socketIdentity = id(socketIdentity, "socket_identity");
    let request;
    try { request = validateControllerMessage(message, senderRole); } catch (error) {
      fail("controller_handshake_message_invalid", error);
    }
    if (request.type !== expectedType) fail("controller_handshake_message_invalid");
    const handshake = this.#handshakes.get(request.reservation_id);
    if (!handshake || handshake.state !== "handshake_pending") fail("controller_handshake_not_found");
    if (handshake.sockets[senderRole] !== socketIdentity) fail("controller_handshake_socket_mismatch");
    const reservation = handshake.reservation;
    if (request.pairing_id !== reservation.pairing_id
      || request.remote_control_generation !== reservation.remote_control_generation
      || request.relay_epoch !== this.#relayEpoch
      || request.transcript_hash !== handshake.transcript_hash) fail("controller_handshake_binding_mismatch");
    if (this.#now() >= Number(reservation.handshake_deadline_ms)) fail("controller_handshake_expired");
    return { request, handshake, senderRole };
  }

  acceptCommitment({ message, role: senderRole, socketIdentity } = {}) {
    const { request, handshake } = this.#handshake(
      message, senderRole, socketIdentity, "finished_receipt_commit",
    );
    if (handshake.commitments.has(senderRole)) fail("receipt_commitment_replayed");
    handshake.commitments.set(senderRole, request.commitment);
    this.#emit("receipt_commitment_accepted");
    return Object.freeze({ target_role: opposite(senderRole), message: request });
  }

  forwardFinished({ message, role: senderRole, socketIdentity } = {}) {
    const { request, handshake } = this.#handshake(
      message, senderRole, socketIdentity, "encrypted_handshake_finished",
    );
    if (!handshake.commitments.has(senderRole)) fail("receipt_commitment_missing");
    if (handshake.finished.has(senderRole)) fail("encrypted_finished_replayed");
    handshake.finished.add(senderRole);
    this.#emit("encrypted_finished_forwarded");
    return Object.freeze({ target_role: opposite(senderRole), message: request });
  }

  async acceptReceipt({ message, role: receiverRole, socketIdentity } = {}) {
    const { request, handshake } = this.#handshake(
      message, receiverRole, socketIdentity, "finished_receipt_ack",
    );
    const senderRole = request.sender_role;
    if (request.receiver_role !== receiverRole || senderRole !== opposite(receiverRole)) {
      fail("receipt_role_mismatch");
    }
    if (!handshake.finished.has(senderRole)) fail("encrypted_finished_missing");
    if (handshake.receipts.has(senderRole)) fail("finished_receipt_replayed");
    const commitment = handshake.commitments.get(senderRole);
    if (!commitment) fail("receipt_commitment_missing");
    const secret = receiptSecret(request.receipt_secret_b64);
    let verified = false;
    try {
      verified = verifyReceiptCommitment({
        relay_epoch: this.#relayEpoch,
        pairing_id: handshake.reservation.pairing_id,
        reservation_id: handshake.reservation.reservation_id,
        transcript_hash: handshake.transcript_hash,
        sender_role: senderRole,
      }, secret, commitment);
    } finally {
      secret.fill(0);
    }
    if (!verified) fail("finished_receipt_invalid");
    handshake.receipts.add(senderRole);
    this.#emit("finished_receipt_accepted");
    if (handshake.receipts.size < 2) return null;
    if (!handshake.commit_promise) handshake.commit_promise = this.#commit(handshake);
    return handshake.commit_promise;
  }

  async #commit(handshake) {
    handshake.state = "committing";
    const reservation = handshake.reservation;
    let controllerLeaseId = null;
    let tokenA = null;
    let tokenB = null;
    try {
      const now = this.#now(RESUME_BUDGET_MS);
      const connectionId = randomId(this.#randomBytes, "connection");
      controllerLeaseId = this.#uniqueLeaseId();
      const leaseEpoch = randomLeaseEpoch(this.#randomBytes);
      const livenessEpochA = randomId(this.#randomBytes, "liveness-a");
      const livenessEpochB = randomId(this.#randomBytes, "liveness-b");
      tokenA = random(this.#randomBytes, 32, "resume_token_a");
      for (let attempt = 0; attempt < 32; attempt += 1) {
        const candidate = random(this.#randomBytes, 32, "resume_token_b");
        if (!crypto.timingSafeEqual(tokenA, candidate)) {
          tokenB = candidate;
          break;
        }
        candidate.fill(0);
      }
      if (tokenB === null) fail("resume_token_generation_exhausted");
      const resumeDeadline = String(now + RESUME_BUDGET_MS);
      const lease = Object.freeze({
        connection_id: connectionId,
        controller_lease_id: controllerLeaseId,
        lease_epoch: leaseEpoch,
        relay_epoch: this.#relayEpoch,
        pairing_id: reservation.pairing_id,
        reservation_id: reservation.reservation_id,
        endpoint_id: reservation.endpoint_id,
        remote_control_generation: reservation.remote_control_generation,
        controller_connection_identity: handshake.controller_connection_identity,
        transcript_hash: handshake.transcript_hash,
        negotiated_capabilities: Object.freeze([...handshake.capabilities]),
        liveness_epoch_a: livenessEpochA,
        liveness_epoch_b: livenessEpochB,
        resume_token_a_sha256: tokenDigest({
          role: "A", relay_epoch: this.#relayEpoch, connection_id: connectionId,
          controller_lease_id: controllerLeaseId, lease_epoch: leaseEpoch,
          remote_control_generation: reservation.remote_control_generation,
        }, tokenA),
        resume_token_b_sha256: tokenDigest({
          role: "B", relay_epoch: this.#relayEpoch, connection_id: connectionId,
          controller_lease_id: controllerLeaseId, lease_epoch: leaseEpoch,
          remote_control_generation: reservation.remote_control_generation,
        }, tokenB),
        resume_deadline_ms: resumeDeadline,
        sockets: handshake.sockets,
      });
      const common = {
        v: CONTROLLER_PROTOCOL_VERSION,
        type: "controller_joined",
        request_id: handshake.request_id,
        pairing_id: reservation.pairing_id,
        reservation_id: reservation.reservation_id,
        transcript_hash: handshake.transcript_hash,
        controller_connection_identity: handshake.controller_connection_identity,
        connection_id: connectionId,
        controller_lease_id: controllerLeaseId,
        lease_epoch: leaseEpoch,
        relay_epoch: this.#relayEpoch,
        remote_control_generation: reservation.remote_control_generation,
        liveness_epoch_a: livenessEpochA,
        liveness_epoch_b: livenessEpochB,
        resume_deadline_ms: resumeDeadline,
      };
      const messages = Object.freeze({
        A: validateControllerMessage({ ...common, resume_token_b64: tokenA.toString("base64") }, "relay"),
        B: validateControllerMessage({ ...common, resume_token_b64: tokenB.toString("base64") }, "relay"),
      });
      await this.#pairings.consumeForControllerLease({
        pairingId: reservation.pairing_id,
        endpointId: reservation.endpoint_id,
        remoteControlGeneration: reservation.remote_control_generation,
        reservationId: reservation.reservation_id,
        controllerLeaseId,
      });
      if (this.#now() >= Number(reservation.handshake_deadline_ms)) {
        fail("controller_handshake_expired");
      }
      this.#reservations.promoteToLease({
        reservationId: reservation.reservation_id,
        socketIdentity: handshake.sockets.A,
        controllerLeaseId,
      });
      this.#leases.set(controllerLeaseId, lease);
      this.#pendingLeaseIds.delete(controllerLeaseId);
      this.#handshakes.delete(reservation.reservation_id);
      this.#emit("controller_lease_created");
      return Object.freeze({ lease, messages });
    } catch (error) {
      handshake.state = "failed";
      this.#handshakes.delete(reservation.reservation_id);
      try {
        await this.#reservations.release({
          reservationId: reservation.reservation_id,
          socketIdentity: handshake.sockets.A,
          reason: "handshake_failed",
        });
      } catch (cleanupError) {
        throw new AggregateError([error, cleanupError], "controller_lease_commit_cleanup_failed");
      }
      throw error;
    } finally {
      if (controllerLeaseId) this.#pendingLeaseIds.delete(controllerLeaseId);
      tokenA?.fill(0);
      tokenB?.fill(0);
    }
  }

  lease(controllerLeaseId) {
    controllerLeaseId = id(controllerLeaseId, "controller_lease_id");
    return this.#leases.get(controllerLeaseId) ?? null;
  }

  verifyResumeToken({ controllerLeaseId, role: tokenRole, resumeTokenB64 } = {}) {
    controllerLeaseId = id(controllerLeaseId, "controller_lease_id");
    tokenRole = role(tokenRole);
    const lease = this.#leases.get(controllerLeaseId);
    if (!lease) fail("controller_lease_not_found");
    const token = resumeToken(resumeTokenB64);
    try {
      const actual = Buffer.from(tokenDigest({
        role: tokenRole,
        relay_epoch: lease.relay_epoch,
        connection_id: lease.connection_id,
        controller_lease_id: lease.controller_lease_id,
        lease_epoch: lease.lease_epoch,
        remote_control_generation: lease.remote_control_generation,
      }, token), "ascii");
      const expected = Buffer.from(
        lease[`resume_token_${tokenRole.toLowerCase()}_sha256`],
        "ascii",
      );
      if (!crypto.timingSafeEqual(actual, expected)) fail("resume_token_invalid");
    } finally {
      token.fill(0);
    }
    return Object.freeze({ lease, role: tokenRole });
  }

  completeResume({
    controllerLeaseId,
    activationId,
    sockets,
    livenessEpochA,
    livenessEpochB,
    tokenA,
    tokenB,
    resumeDeadlineMs,
  } = {}) {
    controllerLeaseId = id(controllerLeaseId, "controller_lease_id");
    activationId = id(activationId, "activation_id");
    const lease = this.#leases.get(controllerLeaseId);
    if (!lease) fail("controller_lease_not_found");
    if (!sockets || typeof sockets !== "object") fail("controller_resume_sockets_invalid");
    const socketA = id(socketsValue(sockets, "A"), "resume_socket_a");
    const socketB = id(socketsValue(sockets, "B"), "resume_socket_b");
    if (socketA === socketB) fail("controller_resume_sockets_invalid");
    livenessEpochA = resumeEpoch(livenessEpochA, "liveness_epoch_a");
    livenessEpochB = resumeEpoch(livenessEpochB, "liveness_epoch_b");
    if (livenessEpochA === livenessEpochB
      || livenessEpochA === lease.liveness_epoch_a
      || livenessEpochB === lease.liveness_epoch_b) fail("liveness_epoch_invalid");
    const nextTokenA = resumeToken(tokenA?.toString("base64"));
    const nextTokenB = resumeToken(tokenB?.toString("base64"));
    try {
      if (nextTokenA.equals(nextTokenB)) fail("resume_token_invalid");
      const timestamp = this.#now(RESUME_BUDGET_MS);
      const resumeDeadline = resumeDeadlineMs ?? String(timestamp + RESUME_BUDGET_MS);
      if (!/^(0|[1-9][0-9]*)$/u.test(resumeDeadline)
        || !Number.isSafeInteger(Number(resumeDeadline))
        || Number(resumeDeadline) <= timestamp
        || Number(resumeDeadline) > timestamp + RESUME_BUDGET_MS) fail("controller_resume_deadline_invalid");
      const tokenADigest = tokenDigest({
        role: "A", relay_epoch: lease.relay_epoch, connection_id: lease.connection_id,
        controller_lease_id: lease.controller_lease_id, lease_epoch: lease.lease_epoch,
        remote_control_generation: lease.remote_control_generation,
      }, nextTokenA);
      const tokenBDigest = tokenDigest({
        role: "B", relay_epoch: lease.relay_epoch, connection_id: lease.connection_id,
        controller_lease_id: lease.controller_lease_id, lease_epoch: lease.lease_epoch,
        remote_control_generation: lease.remote_control_generation,
      }, nextTokenB);
      if (tokenADigest === lease.resume_token_a_sha256
        || tokenBDigest === lease.resume_token_b_sha256) {
        fail("resume_token_invalid");
      }
      const updated = Object.freeze({
        ...lease,
        liveness_epoch_a: livenessEpochA,
        liveness_epoch_b: livenessEpochB,
        resume_token_a_sha256: tokenADigest,
        resume_token_b_sha256: tokenBDigest,
        resume_deadline_ms: resumeDeadline,
        sockets: Object.freeze({ A: socketA, B: socketB }),
      });
      this.#leases.set(controllerLeaseId, updated);
      this.#emit("controller_resume_completed");
      return updated;
    } finally {
      nextTokenA.fill(0);
      nextTokenB.fill(0);
    }
  }

  async terminateLease({ controllerLeaseId, reason = "disconnected" } = {}) {
    controllerLeaseId = id(controllerLeaseId, "controller_lease_id");
    const lease = this.#leases.get(controllerLeaseId);
    if (!lease) return false;
    this.#leases.delete(controllerLeaseId);
    this.#reservations.releaseControllerLease({
      endpointId: lease.endpoint_id, controllerLeaseId,
    });
    await this.#onLeaseTerminated({
      endpoint_id: lease.endpoint_id,
      remote_control_generation: lease.remote_control_generation,
      controller_lease_id: lease.controller_lease_id,
      reason,
    });
    this.#emit("controller_lease_terminated");
    return true;
  }

  async abortHandshakesBySocket({ socketIdentity, reason = "owner_disconnected" } = {}) {
    socketIdentity = id(socketIdentity, "socket_identity");
    const handshakes = [...this.#handshakes.values()]
      .filter((handshake) => handshake.state !== "committing"
        && Object.values(handshake.sockets).includes(socketIdentity));
    const results = await Promise.allSettled(handshakes.map(async (handshake) => {
      handshake.state = "failed";
      this.#handshakes.delete(handshake.reservation.reservation_id);
      return this.#reservations.release({
        reservationId: handshake.reservation.reservation_id,
        socketIdentity: handshake.sockets.A,
        reason,
      });
    }));
    const failures = results.filter((result) => result.status === "rejected").map((result) => result.reason);
    if (failures.length > 0) throw new AggregateError(failures, "controller_handshake_abort_failed");
    return handshakes.length;
  }

  async terminateLeasesBySocket({ socketIdentity, reason = "owner_disconnected" } = {}) {
    socketIdentity = id(socketIdentity, "socket_identity");
    const leases = [...this.#leases.values()]
      .filter((lease) => Object.values(lease.sockets).includes(socketIdentity));
    const leaseResults = await Promise.allSettled(leases.map((lease) => this.terminateLease({
      controllerLeaseId: lease.controller_lease_id, reason,
    })));
    const leaseFailures = leaseResults.filter((result) => result.status === "rejected")
      .map((result) => result.reason);
    if (leaseFailures.length > 0) throw new AggregateError(leaseFailures, "controller_lease_abort_failed");
    return leases.length;
  }

  async abortBySocket({ socketIdentity, reason = "owner_disconnected" } = {}) {
    const [handshakes, leases] = await Promise.all([
      this.abortHandshakesBySocket({ socketIdentity, reason }),
      this.terminateLeasesBySocket({ socketIdentity, reason }),
    ]);
    return Object.freeze({ handshakes, leases });
  }

  async expire() {
    const now = this.#now();
    const expired = [...this.#handshakes.values()]
      .filter((handshake) => handshake.state !== "committing"
        && Number(handshake.reservation.handshake_deadline_ms) <= now);
    const results = await Promise.allSettled(expired.map(async (handshake) => {
      handshake.state = "failed";
      this.#handshakes.delete(handshake.reservation.reservation_id);
      return this.#reservations.release({
        reservationId: handshake.reservation.reservation_id,
        socketIdentity: handshake.sockets.A,
        reason: "timeout",
      });
    }));
    const failures = results.filter((result) => result.status === "rejected").map((result) => result.reason);
    if (failures.length > 0) throw new AggregateError(failures, "controller_handshake_expire_failed");
    return expired.length;
  }
}
