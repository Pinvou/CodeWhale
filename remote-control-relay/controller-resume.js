import crypto from "node:crypto";

import {
  CONTROLLER_PROTOCOL_VERSION,
  validateControllerMessage,
} from "./protocol-controller.js";

const TYPES = new Set([
  "controller_resume_prepare",
  "encrypted_resume_confirm",
  "controller_resume_activation_ack",
  "controller_post_activation_liveness",
  "controller_resume_ready_ack",
]);
const RESUME_BUDGET_MS = 30_000;
const ROLES = Object.freeze(["A", "B"]);

export class ControllerResumeHookError extends Error {
  constructor(code) {
    super(code);
    this.name = "ControllerResumeHookError";
    this.code = code;
  }
}

function fail(code) {
  throw new ControllerResumeHookError(code);
}

function now(clock) {
  const value = clock();
  if (!Number.isSafeInteger(value) || value < 0) fail("controller_resume_clock_invalid");
  return value;
}

function opposite(role) {
  return role === "A" ? "B" : "A";
}

function randomBytes(random, length) {
  const value = random(length);
  if (!Buffer.isBuffer(value) || value.length !== length) fail("controller_resume_entropy_invalid");
  return value;
}

function activationIdentifier(random, lease, challengeA, challengeB) {
  const hash = crypto.createHash("sha256").update("pinvou-controller-resume-activation-v1", "ascii");
  for (const value of [
    lease.relay_epoch,
    lease.connection_id,
    lease.controller_lease_id,
    lease.lease_epoch,
    lease.remote_control_generation,
    challengeA,
    challengeB,
  ]) {
    const bytes = Buffer.from(String(value), "ascii");
    hash.update(Buffer.from([bytes.length, 0, 0, 0])).update(bytes);
  }
  const randomPart = randomBytes(random, 16);
  const identifier = Buffer.concat([hash.digest(), randomPart])
    .toString("base64url").slice(0, 43);
  randomPart.fill(0);
  return `activation-${identifier}`;
}

function commonMessage(type, requestId, lease, extra) {
  return {
    v: CONTROLLER_PROTOCOL_VERSION,
    type,
    request_id: requestId,
    connection_id: lease.connection_id,
    controller_lease_id: lease.controller_lease_id,
    lease_epoch: lease.lease_epoch,
    relay_epoch: lease.relay_epoch,
    remote_control_generation: lease.remote_control_generation,
    ...extra,
  };
}

function peerUpdate(socketIdentity, role, lease, counterpart = null) {
  return Object.freeze({
    socket_identity: socketIdentity,
    role,
    controller_lease_id: lease.controller_lease_id,
    counterpart,
  });
}

function delivery(role, message) {
  return Object.freeze({ role, message: Object.freeze({ ...message }) });
}

function zeroCandidate(state) {
  state.candidate?.tokenA?.fill(0);
  state.candidate?.tokenB?.fill(0);
}

export class ControllerResumeHooks {
  #clock; #random; #leaseManager; #states = new Map();

  constructor({ clock = Date.now, randomBytes = crypto.randomBytes, leaseManager = null } = {}) {
    if (typeof clock !== "function" || typeof randomBytes !== "function") {
      fail("controller_resume_config_invalid");
    }
    if (leaseManager !== null && (
      typeof leaseManager.verifyResumeToken !== "function"
      || typeof leaseManager.completeResume !== "function"
      || typeof leaseManager.terminateLease !== "function"
    )) {
      fail("controller_resume_config_invalid");
    }
    this.#clock = clock;
    this.#random = randomBytes;
    this.#leaseManager = leaseManager;
  }

  handles(type) { return TYPES.has(type); }

  async handle({ message, socket_identity: socketIdentity, role, lease } = {}) {
    if (!message || typeof message.type !== "string" || !TYPES.has(message.type)) {
      fail("controller_resume_message_invalid");
    }
    if (!lease || message.controller_lease_id !== lease.controller_lease_id) {
      fail("controller_lease_not_found");
    }
    const state = this.#states.get(lease.controller_lease_id);
    if (!state) fail("controller_resume_state_not_found");
    const timestamp = now(this.#clock);
    if (state.phase !== "connected" && timestamp >= state.deadline) {
      await this.#terminate(state, lease, "resume_deadline_expired");
      fail("controller_resume_deadline_expired");
    }

    try {
      if (message.type === "controller_resume_prepare") {
        state.resumeStartedAt ??= timestamp;
        return this.#prepare({ message, socketIdentity, role, lease, state });
      }
      if (message.type === "encrypted_resume_confirm") {
        return this.#confirm({ message, socketIdentity, role, lease, state });
      }
      if (message.type === "controller_resume_activation_ack") {
        return this.#activate({ message, socketIdentity, role, lease, state });
      }
      if (message.type === "controller_post_activation_liveness") {
        return this.#postActivationLiveness({ message, socketIdentity, role, lease, state });
      }
      return this.#ready({ message, socketIdentity, role, lease, state });
    } catch (error) {
      await this.#terminate(state, lease, "controller_resume_failed");
      throw error;
    }
  }

  #prepare({ message, socketIdentity, role, lease, state }) {
    const senderRole = message.sender_role;
    if (!ROLES.includes(senderRole) || (ROLES.includes(role) && role !== senderRole)) {
      fail("controller_resume_role_invalid");
    }
    if (state.phase !== "awaiting_prepare" && state.phase !== "prepared") {
      fail("controller_resume_phase_invalid");
    }
    let validated;
    try {
      validated = validateControllerMessage(message, senderRole, lease);
    } catch (error) {
      fail("controller_resume_message_invalid");
    }
    if (validated.original_deadline_ms !== lease.resume_deadline_ms) {
      fail("controller_resume_deadline_mismatch");
    }
    const existing = state.prepared[senderRole];
    if (existing) {
      if (existing.socketIdentity !== socketIdentity
        || existing.requestId !== validated.request_id
        || existing.transportChallengeB64 !== validated.transport_challenge_b64) {
        fail("controller_resume_prepare_conflict");
      }
      return Object.freeze({ deliveries: Object.freeze([]) });
    }
    if (!this.#leaseManager) fail("controller_resume_manager_unavailable");
    this.#leaseManager.verifyResumeToken({
      controllerLeaseId: lease.controller_lease_id,
      role: senderRole,
      resumeTokenB64: validated.resume_token_b64,
    });
    if (Object.values(state.prepared).some((item) => item.socketIdentity === socketIdentity)) {
      fail("controller_resume_socket_conflict");
    }
    state.prepared[senderRole] = Object.freeze({
      socketIdentity,
      requestId: validated.request_id,
      transportChallengeB64: validated.transport_challenge_b64,
    });
    state.phase = "prepared";

    const counterpart = state.prepared[opposite(senderRole)];
    if (!counterpart) {
      return Object.freeze({
        deliveries: Object.freeze([]),
        peer_updates: Object.freeze([peerUpdate(socketIdentity, senderRole, lease)]),
      });
    }
    if (counterpart.socketIdentity === socketIdentity) fail("controller_resume_socket_conflict");
    state.activationId = activationIdentifier(
      this.#random,
      lease,
      state.prepared.A.transportChallengeB64,
      state.prepared.B.transportChallengeB64,
    );
    state.phase = "provisional";
    const provisionalA = validateControllerMessage(commonMessage(
      "controller_resume_provisional",
      state.prepared.A.requestId,
      lease,
      { activation_id: state.activationId, original_deadline_ms: lease.resume_deadline_ms },
    ), "relay", lease);
    const provisionalB = validateControllerMessage(commonMessage(
      "controller_resume_provisional",
      state.prepared.B.requestId,
      lease,
      { activation_id: state.activationId, original_deadline_ms: lease.resume_deadline_ms },
    ), "relay", lease);
    return Object.freeze({
      deliveries: Object.freeze([delivery("A", provisionalA), delivery("B", provisionalB)]),
      peer_updates: Object.freeze([
        peerUpdate(state.prepared.A.socketIdentity, "A", lease, state.prepared.B.socketIdentity),
        peerUpdate(state.prepared.B.socketIdentity, "B", lease, state.prepared.A.socketIdentity),
      ]),
    });
  }

  #confirm({ message, socketIdentity, role, lease, state }) {
    const senderRole = this.#sender(message, role);
    const prepared = state.prepared[senderRole];
    if (!prepared || prepared.socketIdentity !== socketIdentity) {
      fail("controller_resume_socket_invalid");
    }
    const validated = this.#validate(message, senderRole, lease);
    if (validated.activation_id !== state.activationId) fail("controller_resume_activation_invalid");
    const existing = state.confirmed[senderRole];
    if (existing) {
      if (existing.socketIdentity !== socketIdentity || existing.requestId !== validated.request_id) {
        fail("controller_resume_confirm_conflict");
      }
      if (existing.ciphertextB64 !== validated.ciphertext_b64
        || !["provisional", "confirmed", "pending_liveness"].includes(state.phase)) {
        fail("controller_resume_confirm_conflict");
      }
      return Object.freeze({
        deliveries: Object.freeze([delivery(opposite(senderRole), validated)]),
      });
    }
    this.#requirePhase(state, "provisional", "controller_resume_phase_invalid");
    state.confirmed[senderRole] = Object.freeze({
      socketIdentity,
      requestId: validated.request_id,
      ciphertextB64: validated.ciphertext_b64,
    });
    if (state.confirmed.A && state.confirmed.B) state.phase = "confirmed";
    return Object.freeze({
      deliveries: Object.freeze([delivery(opposite(senderRole), validated)]),
    });
  }

  #activate({ message, socketIdentity, role, lease, state }) {
    if (role !== "B" || message.sender_role !== undefined) {
      fail("controller_resume_role_invalid");
    }
    const prepared = state.prepared.B;
    if (!prepared || prepared.socketIdentity !== socketIdentity) {
      fail("controller_resume_socket_invalid");
    }
    const validated = this.#validate(message, "B", lease);
    if (validated.activation_id !== state.activationId
      || validated.original_deadline_ms !== lease.resume_deadline_ms) {
      fail("controller_resume_activation_invalid");
    }
    if (state.activationAck) {
      if (state.activationAck.socketIdentity !== socketIdentity
        || state.activationAck.requestId !== validated.request_id
        || !["pending_liveness", "liveness_confirmed"].includes(state.phase)) {
        fail("controller_resume_activation_conflict");
      }
      const retryPending = validateControllerMessage(commonMessage(
        "controller_resume_activation_pending",
        validated.request_id,
        lease,
        { activation_id: state.activationId, original_deadline_ms: lease.resume_deadline_ms },
      ), "relay", lease);
      return Object.freeze({
        deliveries: Object.freeze([delivery("A", retryPending), delivery("B", retryPending)]),
      });
    }
    this.#requirePhase(state, "confirmed", "controller_resume_phase_invalid");
    if (!state.candidate) this.#createCandidate(state);
    state.activationAck = Object.freeze({ socketIdentity, requestId: validated.request_id });
    state.phase = "pending_liveness";
    const pending = validateControllerMessage(commonMessage(
      "controller_resume_activation_pending",
      validated.request_id,
      lease,
      { activation_id: state.activationId, original_deadline_ms: lease.resume_deadline_ms },
    ), "relay", lease);
    return Object.freeze({
      deliveries: Object.freeze([delivery("A", pending), delivery("B", pending)]),
    });
  }

  #postActivationLiveness({ message, socketIdentity, role, lease, state }) {
    if (role !== "B") fail("controller_resume_role_invalid");
    const prepared = state.prepared.B;
    if (!prepared || prepared.socketIdentity !== socketIdentity) fail("controller_resume_socket_invalid");
    const validated = this.#validate(message, "B", lease);
    if (state.postActivationLiveness && state.phase === "liveness_confirmed") {
      if (state.postActivationLiveness.socketIdentity !== socketIdentity
        || state.postActivationLiveness.requestId !== validated.request_id
        || validated.liveness_epoch_b !== state.candidate.livenessEpochB) {
        fail("controller_resume_liveness_conflict");
      }
      const retryConfirmed = validateControllerMessage(commonMessage(
        "controller_post_activation_liveness_confirmed",
        validated.request_id,
        lease,
        {
          activation_id: state.activationId,
          liveness_epoch_b: state.candidate.livenessEpochB,
          heartbeat_sequence_b: "1",
        },
      ), "relay", lease);
      return Object.freeze({
        deliveries: Object.freeze([delivery("A", retryConfirmed), delivery("B", retryConfirmed)]),
      });
    }
    this.#requirePhase(state, "pending_liveness", "controller_resume_phase_invalid");
    if (validated.activation_id !== state.activationId || validated.heartbeat_sequence_b !== "1") {
      fail("controller_resume_liveness_invalid");
    }
    if (!state.candidate.livenessEpochB) {
      if (validated.liveness_epoch_b === lease.liveness_epoch_b) {
        fail("controller_resume_liveness_invalid");
      }
      state.candidate.livenessEpochB = validated.liveness_epoch_b;
    }
    if (validated.liveness_epoch_b !== state.candidate.livenessEpochB) {
      fail("controller_resume_liveness_invalid");
    }
    state.postActivationLiveness = Object.freeze({ socketIdentity, requestId: validated.request_id });
    state.phase = "liveness_confirmed";
    const confirmed = validateControllerMessage(commonMessage(
      "controller_post_activation_liveness_confirmed",
      validated.request_id,
      lease,
      {
        activation_id: state.activationId,
        liveness_epoch_b: state.candidate.livenessEpochB,
        heartbeat_sequence_b: "1",
      },
    ), "relay", lease);
    return Object.freeze({
      deliveries: Object.freeze([delivery("A", confirmed), delivery("B", confirmed)]),
    });
  }

  #ready({ message, socketIdentity, role, lease, state }) {
    this.#requirePhase(state, "liveness_confirmed", "controller_resume_phase_invalid");
    const senderRole = this.#sender(message, role);
    const prepared = state.prepared[senderRole];
    if (!prepared || prepared.socketIdentity !== socketIdentity) fail("controller_resume_socket_invalid");
    const validated = this.#validate(message, senderRole, lease);
    if (validated.activation_id !== state.activationId
      || validated.liveness_epoch_b !== state.candidate.livenessEpochB
      || validated.heartbeat_sequence_b !== "1") {
      fail("controller_resume_ready_invalid");
    }
    const existing = state.ready[senderRole];
    if (existing) {
      if (existing.socketIdentity !== socketIdentity || existing.requestId !== validated.request_id) {
        fail("controller_resume_ready_conflict");
      }
      return Object.freeze({ deliveries: Object.freeze([]) });
    }
    state.ready[senderRole] = Object.freeze({ socketIdentity, requestId: validated.request_id });
    if (!state.ready.A || !state.ready.B) {
      return Object.freeze({
        deliveries: Object.freeze([delivery(opposite(senderRole), validated)]),
      });
    }

    const resumeDeadline = now(this.#clock) + RESUME_BUDGET_MS;
    if (!Number.isSafeInteger(resumeDeadline)) fail("controller_resume_clock_invalid");
    const resumedA = validateControllerMessage(commonMessage(
      "controller_resumed",
      state.ready.A.requestId,
      lease,
      {
        activation_id: state.activationId,
        liveness_epoch_a: state.candidate.livenessEpochA,
        liveness_epoch_b: state.candidate.livenessEpochB,
        new_resume_token_b64: state.candidate.tokenA.toString("base64"),
        resume_deadline_ms: String(resumeDeadline),
      },
    ), "relay", lease);
    const resumedB = validateControllerMessage(commonMessage(
      "controller_resumed",
      state.ready.B.requestId,
      lease,
      {
        activation_id: state.activationId,
        liveness_epoch_a: state.candidate.livenessEpochA,
        liveness_epoch_b: state.candidate.livenessEpochB,
        new_resume_token_b64: state.candidate.tokenB.toString("base64"),
        resume_deadline_ms: String(resumeDeadline),
      },
    ), "relay", lease);
    state.phase = "committing";
    return Object.freeze({
      deliveries: Object.freeze([delivery("A", resumedA), delivery("B", resumedB)]),
      commit: async () => {
        if (state.phase !== "committing") fail("controller_resume_phase_invalid");
        if (now(this.#clock) >= state.deadline) fail("controller_resume_deadline_expired");
        if (!this.#leaseManager) fail("controller_resume_manager_unavailable");
        const updated = this.#leaseManager.completeResume({
          controllerLeaseId: lease.controller_lease_id,
          activationId: state.activationId,
          sockets: {
            A: state.prepared.A.socketIdentity,
            B: state.prepared.B.socketIdentity,
          },
          livenessEpochA: state.candidate.livenessEpochA,
          livenessEpochB: state.candidate.livenessEpochB,
          tokenA: state.candidate.tokenA,
          tokenB: state.candidate.tokenB,
          resumeDeadlineMs: String(resumeDeadline),
          latencyMs: state.resumeStartedAt === null
            ? undefined
            : Math.max(0, now(this.#clock) - state.resumeStartedAt),
        });
        zeroCandidate(state);
        // Retire all old per-attempt request IDs/ACKs, not just the tokens.
        // Otherwise a valid second cycle conflicts with the previous prepare.
        await this.onLeaseCreated(updated);
        return Object.freeze({ lease: updated });
      },
      abort: () => this.#terminate(state, lease, "controller_resume_failed"),
    });
  }

  async onLeaseCreated(lease) {
    if (!lease || typeof lease.controller_lease_id !== "string") {
      fail("controller_resume_lease_invalid");
    }
    const deadline = Number(lease.resume_deadline_ms);
    if (!Number.isSafeInteger(deadline) || deadline < 0
      || typeof lease.resume_token_a_sha256 !== "string"
      || typeof lease.resume_token_b_sha256 !== "string"
      || lease.resume_token_a_sha256 === lease.resume_token_b_sha256) {
      fail("controller_resume_lease_invalid");
    }
    this.#states.set(lease.controller_lease_id, {
      lease: { ...lease },
      phase: "connected",
      // The wire deadline is a cycle identity, not a connected-phase timer.
      // Freeze the actual authenticated role evidence only on the first close.
      deadline: null,
      disconnected: new Set(),
      prepared: {},
      confirmed: {},
      ready: {},
      activationId: null,
      activationAck: null,
      postActivationLiveness: null,
      candidate: null,
      resumeStartedAt: null,
    });
    return Object.freeze({ deliveries: Object.freeze([]) });
  }

  async onSocketClosed({ socket_identity: socketIdentity, role, lease, evidence_deadline_ms: evidenceDeadline } = {}) {
    if (!lease || !this.#states.has(lease.controller_lease_id)) {
      return Object.freeze({ retain_lease: false });
    }
    const state = this.#states.get(lease.controller_lease_id);
    if (!ROLES.includes(role) || lease.sockets?.[role] !== socketIdentity) {
      return Object.freeze({ retain_lease: false });
    }
    let timestamp;
    try { timestamp = now(this.#clock); } catch {
      zeroCandidate(state);
      this.#states.delete(lease.controller_lease_id);
      return Object.freeze({ retain_lease: false });
    }
    if (state.phase === "connected") {
      if (!Number.isSafeInteger(evidenceDeadline) || timestamp >= evidenceDeadline) {
        this.#states.delete(lease.controller_lease_id);
        return Object.freeze({ retain_lease: false });
      }
      state.deadline = evidenceDeadline;
      state.phase = "awaiting_prepare";
    }
    state.disconnected.add(role);
    return Object.freeze({ retain_lease: true });
  }

  async onLeaseTerminated({ controller_lease_id: controllerLeaseId } = {}) {
    if (controllerLeaseId) this.#states.delete(controllerLeaseId);
  }

  isBusinessAllowed(controllerLeaseId) {
    return this.#states.get(controllerLeaseId)?.phase === "connected";
  }

  phase(controllerLeaseId) {
    return this.#states.get(controllerLeaseId)?.phase ?? null;
  }

  async expire() {
    const timestamp = now(this.#clock);
    let expired = 0;
    for (const [controllerLeaseId, state] of this.#states) {
      if (state.phase === "connected" || timestamp < state.deadline) continue;
      await this.#terminate(
        state,
        { ...state.lease, controller_lease_id: controllerLeaseId },
        "resume_deadline_expired",
      );
      expired += 1;
    }
    return expired;
  }

  #createCandidate(state) {
    let tokenA = randomBytes(this.#random, 32);
    let tokenB = randomBytes(this.#random, 32);
    try {
      if (tokenA.equals(tokenB)) fail("controller_resume_entropy_invalid");
      state.candidate = {
        tokenA,
        tokenB,
        livenessEpochA: `live-a-${randomBytes(this.#random, 16).toString("base64url")}`,
        livenessEpochB: null,
      };
    } catch (error) {
      tokenA.fill(0);
      tokenB.fill(0);
      throw error;
    }
  }

  #sender(message, role) {
    const senderRole = message?.sender_role;
    if (!ROLES.includes(senderRole) || role !== senderRole) fail("controller_resume_role_invalid");
    return senderRole;
  }

  #validate(message, senderRole, lease) {
    try {
      return validateControllerMessage(message, senderRole, lease);
    } catch {
      fail("controller_resume_message_invalid");
    }
  }

  #requirePhase(state, expected, code) {
    if (state.phase !== expected) fail(code);
  }

  async #terminate(state, lease, reason) {
    zeroCandidate(state);
    state.phase = "terminated";
    if (this.#leaseManager) {
      await this.#leaseManager.terminateLease({
        controllerLeaseId: lease.controller_lease_id,
        reason,
      });
    }
    this.#states.delete(lease.controller_lease_id);
  }
}
