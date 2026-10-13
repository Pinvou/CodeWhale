import crypto from "node:crypto";
import { isControllerIdentifier, isRelaySigningKeyIdentifier } from "./identifier-contract.js";

import {
  CONTROLLER_PROTOCOL_VERSION,
  buildLivenessAttestationPreimage,
  buildPeerLivenessAttestationPreimage,
  validateControllerMessage,
  validateControllerRuntimeMessage,
} from "./protocol-controller.js";

const TYPES = new Set([
  "controller_liveness",
  "controller_liveness_challenge_a",
  "encrypted_controller_liveness_a",
  "controller_liveness_attestation_request_a",
  "controller_liveness_attestation_request_b",
]);
const TOTAL_BUDGET_MS = 30_000;
const MAX_ATTESTATION_AGE_MS = 5_000;
const MAX_PENDING_CHALLENGES = 8;
const MAX_USED_CHALLENGES = 64;

export class ControllerLivenessHookError extends Error {
  constructor(code) {
    super(code);
    this.name = "ControllerLivenessHookError";
    this.code = code;
  }
}

function fail(code) {
  throw new ControllerLivenessHookError(code);
}

function integer(value, field) {
  if (!Number.isSafeInteger(value) || value < 0) fail("controller_liveness_clock_invalid");
  return value;
}

function leaseContext(lease) {
  if (!lease || typeof lease !== "object") fail("controller_lease_not_found");
  return lease;
}

function socketMatches({ lease, role, socketIdentity }) {
  if (!["A", "B"].includes(role)) fail("controller_role_invalid");
  if (lease.sockets?.[role] !== socketIdentity) fail("controller_socket_invalid");
}

function delivery(role, message) {
  return Object.freeze({ role, message: Object.freeze({ ...message }) });
}

function signingConfiguration({ signingKeyId, signingPrivateKeyB64, signingPrivateKey, sign } = {}) {
  const keyId = signingKeyId ?? process.env.PINVOU_REMOTE_RELAY_SIGNING_KEY_ID ?? "";
  const encodedKey = signingPrivateKeyB64 ?? process.env.PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64 ?? "";
  if (!signingPrivateKey && !encodedKey && typeof sign !== "function") {
    return { signingKeyId: "", signAttestation: null };
  }
  if (!isRelaySigningKeyIdentifier(keyId)) fail("controller_liveness_signing_key_invalid");
  if (typeof sign !== "function" && typeof sign !== "undefined") {
    fail("controller_liveness_signing_key_invalid");
  }
  const key = signingPrivateKey ?? (encodedKey ? parsePrivateKey(encodedKey) : null);
  const signAttestation = sign
    ?? ((preimage) => crypto.sign(null, preimage, key ?? parsePrivateKey("")));
  if (typeof signAttestation !== "function") fail("controller_liveness_signing_key_invalid");
  return { signingKeyId: keyId, signAttestation };
}

function parsePrivateKey(value) {
  if (typeof value !== "string" || value.length === 0) fail("controller_liveness_signing_key_invalid");
  const der = Buffer.from(value, "base64");
  if (der.length === 0 || der.toString("base64") !== value) {
    fail("controller_liveness_signing_key_invalid");
  }
  try {
    const key = crypto.createPrivateKey({ key: der, format: "der", type: "pkcs8" });
    if (key.asymmetricKeyType !== "ed25519") fail("controller_liveness_signing_key_invalid");
    return key;
  } catch (error) {
    if (error?.name === "ControllerLivenessHookError") throw error;
    fail("controller_liveness_signing_key_invalid");
  }
}

function newState(lease, now) {
  return {
    controller_lease_id: lease.controller_lease_id,
    last_liveness_a: { epoch: lease.liveness_epoch_a, sequence: 0n, at: now },
    last_liveness_b: { epoch: lease.liveness_epoch_b, sequence: 0n, at: now },
    attestation_sequence: 0n,
    peer_b_attestation_sequence: 0n,
    peer_b_used_challenges: new Map(),
    pending_challenges: new Map(),
    used_challenges: new Map(),
  };
}

function pruneChallenges(state, now) {
  for (const [challengeId, challenge] of state.pending_challenges) {
    if (now - challenge.sent_at > TOTAL_BUDGET_MS) state.pending_challenges.delete(challengeId);
  }
  // Retain replay history for the full challenge validity window. Strict
  // heartbeat/attestation sequences remain the epoch-wide replay barrier.
  for (const [key, consumedAt] of state.used_challenges) {
    if (now - consumedAt > TOTAL_BUDGET_MS) state.used_challenges.delete(key);
  }
}

function registerChallenge(state, message, now) {
  pruneChallenges(state, now);
  const key = `${message.challenge_id}\u0000${message.challenge_b64}`;
  if (state.used_challenges.has(key) || state.pending_challenges.has(message.challenge_id)) {
    fail("controller_liveness_challenge_replay");
  }
  if (state.pending_challenges.size >= MAX_PENDING_CHALLENGES) {
    fail("controller_liveness_challenge_capacity");
  }
  state.pending_challenges.set(message.challenge_id, {
    challenge_b64: message.challenge_b64,
    sent_at: now,
  });
}

function consumeChallenge(state, message, now) {
  pruneChallenges(state, now);
  if (state.used_challenges.size >= MAX_USED_CHALLENGES) {
    fail("controller_liveness_challenge_capacity");
  }
  state.pending_challenges.delete(message.challenge_id);
  state.used_challenges.set(`${message.challenge_id}\u0000${message.challenge_b64}`, now);
}

export class ControllerLivenessHooks {
  #clock; #states = new Map(); #signing; #leaseManager;

  constructor(options = {}) {
    if (typeof options.clock !== "function" && options.clock !== undefined) {
      fail("controller_liveness_clock_invalid");
    }
    this.#clock = options.clock ?? Date.now;
    if (options.leaseManager != null && typeof options.leaseManager.terminateLease !== "function") {
      fail("controller_liveness_config_invalid");
    }
    this.#leaseManager = options.leaseManager ?? null;
    this.#signing = signingConfiguration(options);
  }

  handles(type) { return TYPES.has(type); }

  async handle({ message, socket_identity: socketIdentity, role, lease } = {}) {
    const now = integer(this.#clock(), "now");
    const context = leaseContext(lease);
    socketMatches({ lease: context, role, socketIdentity });
    if (!message || typeof message.type !== "string") fail("controller_liveness_message_invalid");
    if (message.controller_lease_id !== context.controller_lease_id) {
      fail("controller_lease_not_found");
    }
    const state = this.#states.get(context.controller_lease_id);
    if (!state) fail("controller_liveness_state_not_found");
    // Validate using the authenticated socket's actual role, not the role a
    // specialized handler expects. A B socket cannot request proof as A.
    validateControllerMessage(message, role, context);

    // Enforce the existing evidence cutoff before any update, proof or forward.
    // The periodic cleanup tick may run late; a late authenticated heartbeat
    // must not turn an expired cycle back into a resumable lease.
    const deadline = Math.min(state.last_liveness_a.at, state.last_liveness_b.at) + TOTAL_BUDGET_MS;
    if (now >= deadline) {
      await this.#expireState(context.controller_lease_id, state);
      fail("controller_liveness_expired");
    }

    if (message.type === "controller_liveness") {
      return this.#acceptTransportHeartbeat({ message, role, context, state, now });
    }
    if (message.type === "controller_liveness_challenge_a") {
      return this.#acceptChallenge({ message, context, state, now });
    }
    if (message.type === "encrypted_controller_liveness_a") {
      return this.#forwardEncryptedLiveness({ message, context, state, now });
    }
    if (message.type === "controller_liveness_attestation_request_b") {
      return this.#issuePeerBAttestation({ message, context, state, now });
    }
    return this.#issueAttestation({ message, context, state, now });
  }

  #issuePeerBAttestation({ message, context, state, now }) {
    const validated = validateControllerRuntimeMessage(message, "A", {
      lease: Object.fromEntries(["connection_id", "controller_lease_id", "lease_epoch", "relay_epoch", "remote_control_generation"].map(field => [field, context[field]])),
      negotiated_capabilities: context.negotiated_capabilities,
    });
    if (!this.#signing.signAttestation) fail("controller_liveness_attestation_unavailable");
    for (const [id, consumedAt] of state.peer_b_used_challenges) {
      if (now - consumedAt > TOTAL_BUDGET_MS) state.peer_b_used_challenges.delete(id);
    }
    const challengeKey = validated.challenge_id;
    if (state.peer_b_used_challenges.has(challengeKey)) fail("controller_liveness_challenge_replay");
    if (state.peer_b_used_challenges.size >= MAX_USED_CHALLENGES) fail("controller_liveness_challenge_capacity");
    const evidence = state.last_liveness_b;
    const observedAge = now - evidence.at;
    if (evidence.epoch !== context.liveness_epoch_b || evidence.sequence === 0n
      || !Number.isSafeInteger(observedAge) || observedAge < 0 || observedAge > MAX_ATTESTATION_AGE_MS) {
      fail("controller_liveness_attestation_stale");
    }
    const sequence = state.peer_b_attestation_sequence + 1n;
    const unsigned = {
      v: CONTROLLER_PROTOCOL_VERSION,
      type: "controller_liveness_attestation_b",
      request_id: validated.request_id,
      connection_id: context.connection_id,
      controller_lease_id: context.controller_lease_id,
      lease_epoch: context.lease_epoch,
      relay_epoch: context.relay_epoch,
      remote_control_generation: context.remote_control_generation,
      liveness_epoch_b: evidence.epoch,
      heartbeat_sequence_b: evidence.sequence.toString(),
      attestation_sequence: sequence.toString(),
      challenge_id: validated.challenge_id,
      challenge_b64: validated.challenge_b64,
      observed_age_ms: observedAge.toString(),
      remaining_ttl_ms: (TOTAL_BUDGET_MS - observedAge).toString(),
      issued_at_relay: now.toString(),
      relay_signing_key_id: this.#signing.signingKeyId,
      signature_algorithm: "ed25519",
    };
    const signature = this.#signing.signAttestation(buildPeerLivenessAttestationPreimage(unsigned));
    if (!Buffer.isBuffer(signature) || signature.length !== 64) fail("controller_liveness_signature_invalid");
    const attestation = validateControllerMessage({ ...unsigned, relay_signature_b64: signature.toString("base64") }, "relay", context);
    state.peer_b_attestation_sequence = sequence;
    state.peer_b_used_challenges.set(challengeKey, now);
    return Object.freeze({ deliveries: Object.freeze([delivery("A", attestation)]) });
  }

  #acceptTransportHeartbeat({ message, role, context, state, now }) {
    const epochField = role === "A" ? "liveness_epoch_a" : "liveness_epoch_b";
    const validated = validateControllerMessage(message, role, context);
    if (validated.liveness_epoch !== context[epochField]) fail("controller_liveness_epoch_invalid");
    const sequence = BigInt(validated.heartbeat_sequence);
    const last = role === "A" ? state.last_liveness_a : state.last_liveness_b;
    if (sequence <= last.sequence) fail("controller_liveness_sequence_replay");
    last.epoch = validated.liveness_epoch;
    last.sequence = sequence;
    last.at = now;
    return Object.freeze({ deliveries: Object.freeze([delivery(role === "A" ? "B" : "A", validated)]) });
  }

  #acceptChallenge({ message, context, state, now }) {
    const validated = validateControllerMessage(message, "B", context);
    registerChallenge(state, validated, now);
    return Object.freeze({ deliveries: Object.freeze([delivery("A", validated)]) });
  }

  #forwardEncryptedLiveness({ message, context }) {
    const validated = validateControllerMessage(message, "A", context);
    return Object.freeze({ deliveries: Object.freeze([delivery("B", validated)]) });
  }

  #issueAttestation({ message, context, state, now }) {
    const validated = validateControllerMessage(message, "B", context);
    if (!this.#signing.signAttestation) fail("controller_liveness_attestation_unavailable");
    pruneChallenges(state, now);
    const pending = state.pending_challenges.get(validated.challenge_id);
    if (!pending || pending.challenge_b64 !== validated.challenge_b64) {
      fail("controller_liveness_challenge_not_found");
    }
    const observedAge = now - state.last_liveness_a.at;
    if (!Number.isSafeInteger(observedAge) || observedAge < 0 || observedAge > MAX_ATTESTATION_AGE_MS) {
      fail("controller_liveness_attestation_stale");
    }
    state.attestation_sequence += 1n;
    const unsigned = {
      v: CONTROLLER_PROTOCOL_VERSION,
      type: "controller_liveness_attestation_a",
      request_id: validated.request_id,
      connection_id: context.connection_id,
      controller_lease_id: context.controller_lease_id,
      lease_epoch: context.lease_epoch,
      relay_epoch: context.relay_epoch,
      remote_control_generation: context.remote_control_generation,
      liveness_epoch_a: state.last_liveness_a.epoch,
      heartbeat_sequence_a: state.last_liveness_a.sequence.toString(),
      attestation_sequence: state.attestation_sequence.toString(),
      challenge_id: validated.challenge_id,
      challenge_b64: validated.challenge_b64,
      observed_age_ms: observedAge.toString(),
      remaining_ttl_ms: Math.max(0, TOTAL_BUDGET_MS - observedAge).toString(),
      issued_at_relay: now.toString(),
      relay_signing_key_id: this.#signing.signingKeyId,
      signature_algorithm: "ed25519",
    };
    const signature = this.#signing.signAttestation(buildLivenessAttestationPreimage(unsigned));
    if (!Buffer.isBuffer(signature) || signature.length !== 64) {
      fail("controller_liveness_signature_invalid");
    }
    const attestation = validateControllerMessage({
      ...unsigned,
      relay_signature_b64: signature.toString("base64"),
    }, "relay", {
      ...context,
      liveness_epoch_a: state.last_liveness_a.epoch,
      challenge_id: validated.challenge_id,
      challenge_b64: validated.challenge_b64,
      last_heartbeat_sequence_a: state.last_liveness_a.sequence.toString(),
      last_attestation_sequence: (state.attestation_sequence - 1n).toString(),
    });
    consumeChallenge(state, validated, now);
    return Object.freeze({ deliveries: Object.freeze([delivery("B", attestation)]) });
  }

  async onLeaseCreated(lease) {
    const now = integer(this.#clock(), "now");
    const context = leaseContext(lease);
    if (!isControllerIdentifier(context.controller_lease_id)
      || typeof context.liveness_epoch_a !== "string"
      || typeof context.liveness_epoch_b !== "string") {
      fail("controller_liveness_lease_invalid");
    }
    if (this.#states.get(context.controller_lease_id)?.leaseOwner === lease) {
      return Object.freeze({ deliveries: Object.freeze([]) });
    }
    this.#states.set(context.controller_lease_id, { ...newState(context, now), leaseOwner: lease });
    return Object.freeze({ deliveries: Object.freeze([]) });
  }

  async onSocketClosed({ lease } = {}) {
    if (lease?.controller_lease_id && this.#states.get(lease.controller_lease_id)?.leaseOwner === lease) {
      this.#states.delete(lease.controller_lease_id);
    }
    return Object.freeze({ retain_lease: false });
  }

  // Snapshot synchronously before onSocketClosed removes evidence. The
  // authenticated join/resume seeds a finite cycle; subsequent role heartbeats
  // can advance it only while connected. B activity never renews A's cutoff.
  resumeDeadline(lease) {
    const state = this.#states.get(lease?.controller_lease_id);
    if (!state || state.last_liveness_a.epoch !== lease.liveness_epoch_a
      || state.last_liveness_b.epoch !== lease.liveness_epoch_b) return null;
    const timestamp = integer(this.#clock(), "now");
    const deadline = Math.min(state.last_liveness_a.at, state.last_liveness_b.at) + TOTAL_BUDGET_MS;
    if (!Number.isSafeInteger(deadline) || timestamp >= deadline) return null;
    return deadline;
  }

  async onLeaseTerminated({ controller_lease_id: controllerLeaseId } = {}, leaseOwner) {
    const state = this.#states.get(controllerLeaseId);
    if (state && (leaseOwner === undefined || state.leaseOwner === leaseOwner)) this.#states.delete(controllerLeaseId);
  }

  async expire() {
    const now = integer(this.#clock(), "now");
    let expired = 0;
    for (const [controllerLeaseId, state] of this.#states) {
      const deadline = Math.min(state.last_liveness_a.at, state.last_liveness_b.at) + TOTAL_BUDGET_MS;
      if (now >= deadline) {
        await this.#expireState(controllerLeaseId, state);
        expired += 1;
      } else {
        pruneChallenges(state, now);
      }
    }
    return expired;
  }

  async #expireState(controllerLeaseId, state) {
    // Revoke synchronously, before awaiting callbacks, so concurrent late
    // messages cannot renew evidence. Only retire the captured lease owner;
    // callbacks may install a replacement with the same lease ID.
    if (this.#states.get(controllerLeaseId) === state) this.#states.delete(controllerLeaseId);
    if (this.#leaseManager) await this.#leaseManager.terminateLease({
      controllerLeaseId, expectedLease: state.leaseOwner, reason: "controller_liveness_expired",
    });
  }
}
