import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { ControllerLeaseManager } from "../controller-lease.js";
import { ControllerResumeHooks } from "../controller-resume.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";
import { ControllerRouter } from "../controller-router.js";
import { receiptCommitmentHex } from "../protocol-controller.js";

const CAPABILITIES = Object.freeze([
  "pairing_v1", "encrypted_frames_v1", "finished_receipt_v1", "role_liveness_v1", "resume_v1",
]);
const ZERO_NONCE = "AAAAAAAAAAAAAAAAAAAAAA==";

function publicKey() {
  return crypto.generateKeyPairSync("x25519").publicKey.export({ type: "spki", format: "der" })
    .subarray(-32).toString("base64");
}

function fixture({ startClock = 1_000 } = {}) {
  let clock = startClock;
  const pairing = Object.freeze({
    pairing_id: "pairing-0001",
    endpoint_id: "endpoint-0001",
    remote_control_generation: "generation-0001",
    handshake_public_key_b64: publicKey(),
    capabilities: Object.freeze([...CAPABILITIES]),
  });
  const pairings = {
    activePairing: (pairingId) => pairingId === pairing.pairing_id ? pairing : null,
    findActiveByCode: (code) => code === "2345678" ? pairing : null,
    consumeForControllerLease: async (value) => value,
    terminate: async () => {},
  };
  const manager = new ControllerLeaseManager({
    pairingStore: pairings,
    reservationManager: new ControllerReservationManager({
      pairingStore: pairings,
      interactionSlots: new EndpointInteractionSlots(),
      clock: () => clock,
    }),
    relayEpoch: "relay-epoch-0001",
    signingKeyId: "relay-signing-key-0001",
    signTranscript: (preimage) => crypto.sign(null, preimage, crypto.generateKeyPairSync("ed25519").privateKey),
    resolveControlledEndpoint: async () => ({ socketIdentity: "old-socket-B" }),
    clock: () => clock,
  });
  const hooks = new ControllerResumeHooks({
    leaseManager: manager,
    clock: () => clock,
    randomBytes: crypto.randomBytes,
  });
  return {
    manager,
    hooks,
    pairing,
    get clock() { return clock; },
    set clock(value) { clock = value; },
  };
}

async function createLease(value) {
  const reserved = await value.manager.claim({
    socketIdentity: "old-socket-A",
    source: "198.51.100.1",
    message: {
      v: 1,
      type: "controller_pairing_claim",
      request_id: "request-claim-0001",
      claim_request_id: "claim-stable-0001",
      pairing_code: "2345678",
      handshake_public_key_b64: publicKey(),
      key_algorithm: "x25519",
      capabilities: [...CAPABILITIES],
    },
  });
  const secretA = crypto.randomBytes(32);
  const secretB = crypto.randomBytes(32);
  const commitment = (senderRole, secret) => ({
    v: 1,
    type: "finished_receipt_commit",
    request_id: `request-commit-${senderRole}`,
    pairing_id: reserved.pairing_id,
    reservation_id: reserved.reservation_id,
    remote_control_generation: reserved.remote_control_generation,
    relay_epoch: reserved.relay_epoch,
    transcript_hash: reserved.transcript_hash,
    sender_role: senderRole,
    commitment: receiptCommitmentHex({
      relay_epoch: reserved.relay_epoch,
      pairing_id: reserved.pairing_id,
      reservation_id: reserved.reservation_id,
      transcript_hash: reserved.transcript_hash,
      sender_role: senderRole,
    }, secret),
  });
  const finished = (senderRole) => ({
    v: 1,
    type: "encrypted_handshake_finished",
    request_id: `request-finished-${senderRole}`,
    pairing_id: reserved.pairing_id,
    reservation_id: reserved.reservation_id,
    remote_control_generation: reserved.remote_control_generation,
    relay_epoch: reserved.relay_epoch,
    transcript_hash: reserved.transcript_hash,
    sender_role: senderRole,
    ciphertext_b64: "AQID",
    ciphertext_bytes: "3",
  });
  const receipt = (receiverRole, senderRole, secret) => ({
    v: 1,
    type: "finished_receipt_ack",
    request_id: `request-receipt-${receiverRole}`,
    pairing_id: reserved.pairing_id,
    reservation_id: reserved.reservation_id,
    remote_control_generation: reserved.remote_control_generation,
    relay_epoch: reserved.relay_epoch,
    transcript_hash: reserved.transcript_hash,
    sender_role: senderRole,
    receiver_role: receiverRole,
    receipt_secret_b64: secret.toString("base64"),
  });
  value.manager.acceptCommitment({
    message: commitment("A", secretA), role: "A", socketIdentity: "old-socket-A",
  });
  value.manager.acceptCommitment({
    message: commitment("B", secretB), role: "B", socketIdentity: "old-socket-B",
  });
  value.manager.forwardFinished({ message: finished("A"), role: "A", socketIdentity: "old-socket-A" });
  value.manager.forwardFinished({ message: finished("B"), role: "B", socketIdentity: "old-socket-B" });
  await value.manager.acceptReceipt({
    message: receipt("A", "B", secretB), role: "A", socketIdentity: "old-socket-A",
  });
  const committed = await value.manager.acceptReceipt({
    message: receipt("B", "A", secretA), role: "B", socketIdentity: "old-socket-B",
  });
  await value.hooks.onLeaseCreated(committed.lease);
  return committed;
}

function context(lease) {
  return {
    connection_id: lease.connection_id,
    controller_lease_id: lease.controller_lease_id,
    lease_epoch: lease.lease_epoch,
    relay_epoch: lease.relay_epoch,
    remote_control_generation: lease.remote_control_generation,
  };
}

function prepare(role, lease, token, requestId, challenge = ZERO_NONCE) {
  return {
    v: 1,
    type: "controller_resume_prepare",
    request_id: requestId,
    ...context(lease),
    sender_role: role,
    resume_token_b64: token,
    transport_challenge_b64: challenge,
    original_deadline_ms: lease.resume_deadline_ms,
  };
}

async function detached(value, committed) {
  assert.equal((await value.hooks.onSocketClosed({
    socket_identity: "old-socket-A", role: "A", lease: committed.lease,
  })).retain_lease, true);
  assert.equal((await value.hooks.onSocketClosed({
    socket_identity: "old-socket-B", role: "B", lease: committed.lease,
  })).retain_lease, true);
}

async function provisional(value, committed) {
  await detached(value, committed);
  const lease = committed.lease;
  await value.hooks.handle({
    message: prepare("A", lease, committed.messages.A.resume_token_b64, "request-prepare-A", ZERO_NONCE),
    socket_identity: "new-socket-A",
    role: "A",
    lease,
  });
  const result = await value.hooks.handle({
    message: prepare("B", lease, committed.messages.B.resume_token_b64, "request-prepare-B", "AQEBAQEBAQEBAQEBAQEBAQ=="),
    socket_identity: "new-socket-B",
    role: "B",
    lease,
  });
  assert.equal(false, value.hooks.isBusinessAllowed(lease.controller_lease_id));
  return result;
}

function activationMessage(type, role, lease, activationId, requestId, extra = {}) {
  return {
    v: 1,
    type,
    request_id: requestId,
    ...context(lease),
    activation_id: activationId,
    ...(role ? { sender_role: role } : {}),
    ...extra,
  };
}

async function pending(value, committed, provisionalResult) {
  const lease = committed.lease;
  const activationId = provisionalResult.deliveries[0].message.activation_id;
  await value.hooks.handle({
    message: activationMessage("encrypted_resume_confirm", "A", lease, activationId, "request-confirm-A", {
      ciphertext_b64: "AQID", ciphertext_bytes: "3",
    }),
    socket_identity: "new-socket-A", role: "A", lease,
  });
  await value.hooks.handle({
    message: activationMessage("encrypted_resume_confirm", "B", lease, activationId, "request-confirm-B", {
      ciphertext_b64: "BAQD", ciphertext_bytes: "3",
    }),
    socket_identity: "new-socket-B", role: "B", lease,
  });
  return value.hooks.handle({
    message: activationMessage("controller_resume_activation_ack", null, lease, activationId, "request-ack-B", {
      original_deadline_ms: lease.resume_deadline_ms,
    }),
    socket_identity: "new-socket-B", role: "B", lease,
  });
}

async function readyResult(value, committed, provisionalResult, epochB = "live-b-new-1") {
  const lease = committed.lease;
  const activationId = provisionalResult.deliveries[0].message.activation_id;
  await value.hooks.handle({
    message: activationMessage("controller_post_activation_liveness", null, lease, activationId, "request-live-B", {
      liveness_epoch_b: epochB, heartbeat_sequence_b: "1",
    }),
    socket_identity: "new-socket-B", role: "B", lease,
  });
  await value.hooks.handle({
    message: activationMessage("controller_resume_ready_ack", "A", lease, activationId, "request-ready-A", {
      liveness_epoch_b: epochB, heartbeat_sequence_b: "1",
    }),
    socket_identity: "new-socket-A", role: "A", lease,
  });
  return value.hooks.handle({
    message: activationMessage("controller_resume_ready_ack", "B", lease, activationId, "request-ready-B", {
      liveness_epoch_b: epochB, heartbeat_sequence_b: "1",
    }),
    socket_identity: "new-socket-B", role: "B", lease,
  });
}

test("both role-isolated tokens must prepare before provisional business-free resume", async () => {
  const value = fixture();
  const committed = await createLease(value);
  await detached(value, committed);
  const first = await value.hooks.handle({
    message: prepare("A", committed.lease, committed.messages.A.resume_token_b64, "request-prepare-A"),
    socket_identity: "new-socket-A", role: "A", lease: committed.lease,
  });
  assert.deepEqual(first.deliveries, []);
  assert.deepEqual(first.peer_updates.map((item) => item.role), ["A"]);
  const second = await value.hooks.handle({
    message: prepare("B", committed.lease, committed.messages.B.resume_token_b64, "request-prepare-B", "AQEBAQEBAQEBAQEBAQEBAQ=="),
    socket_identity: "new-socket-B", role: "B", lease: committed.lease,
  });
  assert.equal(second.deliveries.length, 2);
  assert(second.deliveries.every((item) => item.message.type === "controller_resume_provisional"));
  assert.equal(second.deliveries[0].message.activation_id, second.deliveries[1].message.activation_id);
  assert.deepEqual(second.peer_updates.map((item) => item.role), ["A", "B"]);
  assert.equal(second.peer_updates[0].counterpart, "new-socket-B");
  assert.equal(second.peer_updates[1].counterpart, "new-socket-A");
  assert.equal(value.hooks.isBusinessAllowed(committed.lease.controller_lease_id), false);
});

test("complete activation atomically rotates role tokens, epochs and sockets", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const provisionalResult = await provisional(value, committed);
  const pendingResult = await pending(value, committed, provisionalResult);
  assert(pendingResult.deliveries.every((item) => item.message.type === "controller_resume_activation_pending"));
  const result = await readyResult(value, committed, provisionalResult);
  assert.equal(value.hooks.isBusinessAllowed(committed.lease.controller_lease_id), false);
  const committedResult = await result.commit();
  const updated = committedResult.lease;
  assert.equal(updated.sockets.A, "new-socket-A");
  assert.equal(updated.sockets.B, "new-socket-B");
  assert.notEqual(updated.liveness_epoch_a, committed.lease.liveness_epoch_a);
  assert.notEqual(updated.liveness_epoch_b, committed.lease.liveness_epoch_b);
  assert.equal(value.hooks.isBusinessAllowed(updated.controller_lease_id), true);

  const resumedA = result.deliveries.find((item) => item.role === "A").message;
  const resumedB = result.deliveries.find((item) => item.role === "B").message;
  assert.notEqual(resumedA.new_resume_token_b64, resumedB.new_resume_token_b64);
  value.manager.verifyResumeToken({
    controllerLeaseId: updated.controller_lease_id,
    role: "A",
    resumeTokenB64: resumedA.new_resume_token_b64,
  });
  value.manager.verifyResumeToken({
    controllerLeaseId: updated.controller_lease_id,
    role: "B",
    resumeTokenB64: resumedB.new_resume_token_b64,
  });
  assert.throws(() => value.manager.verifyResumeToken({
    controllerLeaseId: updated.controller_lease_id,
    role: "A",
    resumeTokenB64: resumedB.new_resume_token_b64,
  }), /resume_token_invalid/u);
  for (const oldToken of [
    committed.messages.A.resume_token_b64,
    committed.messages.B.resume_token_b64,
  ]) {
    assert.throws(() => value.manager.verifyResumeToken({
      controllerLeaseId: updated.controller_lease_id,
      role: oldToken === committed.messages.A.resume_token_b64 ? "A" : "B",
      resumeTokenB64: oldToken,
    }), /resume_token_invalid/u);
  }
});

test("cross-role, invalid or stale tokens terminate the original lease", async () => {
  const value = fixture();
  const committed = await createLease(value);
  await detached(value, committed);
  await assert.rejects(value.hooks.handle({
    message: prepare("A", committed.lease, committed.messages.B.resume_token_b64, "request-prepare-A"),
    socket_identity: "new-socket-A", role: "A", lease: committed.lease,
  }), /resume_token_invalid/u);
  assert.equal(value.manager.lease(committed.lease.controller_lease_id), null);
  assert.equal(value.hooks.isBusinessAllowed(committed.lease.controller_lease_id), false);
});

test("activation ACK cannot skip either E2E confirmation and wrong liveness terminates", async () => {
  const first = fixture();
  const firstCommitted = await createLease(first);
  const firstProvisional = await provisional(first, firstCommitted);
  const activationId = firstProvisional.deliveries[0].message.activation_id;
  await assert.rejects(first.hooks.handle({
    message: activationMessage("controller_resume_activation_ack", null, firstCommitted.lease, activationId, "request-ack-B", {
      original_deadline_ms: firstCommitted.lease.resume_deadline_ms,
    }),
    socket_identity: "new-socket-B", role: "B", lease: firstCommitted.lease,
  }), /controller_resume_phase_invalid/u);
  assert.equal(first.manager.lease(firstCommitted.lease.controller_lease_id), null);

  const second = fixture();
  const secondCommitted = await createLease(second);
  const secondProvisional = await provisional(second, secondCommitted);
  await pending(second, secondCommitted, secondProvisional);
  await assert.rejects(second.hooks.handle({
    message: activationMessage("controller_post_activation_liveness", null, secondCommitted.lease, secondProvisional.deliveries[0].message.activation_id, "request-live-B", {
      liveness_epoch_b: "live-b-new-1", heartbeat_sequence_b: "0",
    }),
    socket_identity: "new-socket-B", role: "B", lease: secondCommitted.lease,
  }), /controller_resume_liveness_invalid/u);
  assert.equal(second.manager.lease(secondCommitted.lease.controller_lease_id), null);
});

test("one-sided prepare or ready ack expires inside the original deadline", async () => {
  const oneSide = fixture();
  const oneCommitted = await createLease(oneSide);
  await detached(oneSide, oneCommitted);
  await oneSide.hooks.handle({
    message: prepare("A", oneCommitted.lease, oneCommitted.messages.A.resume_token_b64, "request-prepare-A"),
    socket_identity: "new-socket-A", role: "A", lease: oneCommitted.lease,
  });
  oneSide.clock = Number(oneCommitted.lease.resume_deadline_ms);
  assert.equal(await oneSide.hooks.expire(), 1);
  assert.equal(oneSide.manager.lease(oneCommitted.lease.controller_lease_id), null);

  const readySide = fixture();
  const readyCommitted = await createLease(readySide);
  const readyProvisional = await provisional(readySide, readyCommitted);
  await pending(readySide, readyCommitted, readyProvisional);
  const activationId = readyProvisional.deliveries[0].message.activation_id;
  await readySide.hooks.handle({
    message: activationMessage("controller_post_activation_liveness", null, readyCommitted.lease, activationId, "request-live-B", {
      liveness_epoch_b: "live-b-ready-1", heartbeat_sequence_b: "1",
    }),
    socket_identity: "new-socket-B", role: "B", lease: readyCommitted.lease,
  });
  await readySide.hooks.handle({
    message: activationMessage("controller_resume_ready_ack", "A", readyCommitted.lease, activationId, "request-ready-A", {
      liveness_epoch_b: "live-b-ready-1", heartbeat_sequence_b: "1",
    }),
    socket_identity: "new-socket-A", role: "A", lease: readyCommitted.lease,
  });
  readySide.clock = Number(readyCommitted.lease.resume_deadline_ms);
  assert.equal(await readySide.hooks.expire(), 1);
  assert.equal(readySide.manager.lease(readyCommitted.lease.controller_lease_id), null);
});

test("commit failure invokes the same terminal cleanup path", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const provisionalResult = await provisional(value, committed);
  await pending(value, committed, provisionalResult);
  const result = await readyResult(value, committed, provisionalResult);
  await value.manager.terminateLease({
    controllerLeaseId: committed.lease.controller_lease_id,
    reason: "test_removed",
  });
  await assert.rejects(result.commit(), /controller_lease_not_found/u);
  await result.abort();
  assert.equal(value.manager.lease(committed.lease.controller_lease_id), null);
  assert.equal(value.hooks.isBusinessAllowed(committed.lease.controller_lease_id), false);
});

test("router reattaches authenticated sockets and commits only after final delivery", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const sent = new Map([["new-socket-A", []], ["new-socket-B", []]]);
  const router = new ControllerRouter({
    pairingRegistrationHandler: { register: async (message) => message },
    leaseManager: value.manager,
    send: (socket, message) => {
      sent.get(socket).push(message);
      return true;
    },
    resumeHooks: value.hooks,
  });
  router.addSocket({ socketIdentity: "new-socket-A", source: "198.51.100.2", socket: "new-socket-A" });
  router.addSocket({ socketIdentity: "new-socket-B", source: "198.51.100.3", socket: "new-socket-B" });
  router.authenticateControlled({
    socketIdentity: "new-socket-B",
    endpointId: "endpoint-0001",
    authenticatedContext: Object.freeze({ endpointId: "endpoint-0001" }),
  });
  await detached(value, committed);
  const lease = committed.lease;
  await router.handle({
    socketIdentity: "new-socket-A",
    message: prepare("A", lease, committed.messages.A.resume_token_b64, "request-prepare-A"),
  });
  const provisionalResult = await router.handle({
    socketIdentity: "new-socket-B",
    message: prepare("B", lease, committed.messages.B.resume_token_b64, "request-prepare-B", "AQEBAQEBAQEBAQEBAQEBAQ=="),
  });
  const activationId = provisionalResult.deliveries[0].message.activation_id;
  await router.handle({
    socketIdentity: "new-socket-A",
    message: activationMessage("encrypted_resume_confirm", "A", lease, activationId, "request-confirm-A", {
      ciphertext_b64: "AQID", ciphertext_bytes: "3",
    }),
  });
  await router.handle({
    socketIdentity: "new-socket-B",
    message: activationMessage("encrypted_resume_confirm", "B", lease, activationId, "request-confirm-B", {
      ciphertext_b64: "BAQD", ciphertext_bytes: "3",
    }),
  });
  await router.handle({
    socketIdentity: "new-socket-B",
    message: activationMessage("controller_resume_activation_ack", null, lease, activationId, "request-ack-B", {
      original_deadline_ms: lease.resume_deadline_ms,
    }),
  });
  await router.handle({
    socketIdentity: "new-socket-B",
    message: activationMessage("controller_post_activation_liveness", null, lease, activationId, "request-live-B", {
      liveness_epoch_b: "live-b-router-1", heartbeat_sequence_b: "1",
    }),
  });
  await router.handle({
    socketIdentity: "new-socket-A",
    message: activationMessage("controller_resume_ready_ack", "A", lease, activationId, "request-ready-A", {
      liveness_epoch_b: "live-b-router-1", heartbeat_sequence_b: "1",
    }),
  });
  await router.handle({
    socketIdentity: "new-socket-B",
    message: activationMessage("controller_resume_ready_ack", "B", lease, activationId, "request-ready-B", {
      liveness_epoch_b: "live-b-router-1", heartbeat_sequence_b: "1",
    }),
  });
  assert(sent.get("new-socket-A").some((message) => message.type === "controller_resumed"));
  assert(sent.get("new-socket-B").some((message) => message.type === "controller_resumed"));
  const updated = value.manager.lease(lease.controller_lease_id);
  assert.equal(updated.sockets.A, "new-socket-A");
  assert.equal(updated.sockets.B, "new-socket-B");
  assert.equal(value.hooks.isBusinessAllowed(lease.controller_lease_id), true);
});

test("losing either final delivery terminates instead of leaving an active lease", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const sent = new Map([["new-socket-A", []], ["new-socket-B", []]]);
  const router = new ControllerRouter({
    pairingRegistrationHandler: { register: async (message) => message },
    leaseManager: value.manager,
    send: (socket, message) => {
      if (message.type === "controller_resumed" && socket === "new-socket-B") return false;
      sent.get(socket).push(message);
      return true;
    },
    resumeHooks: value.hooks,
  });
  router.addSocket({ socketIdentity: "new-socket-A", source: "198.51.100.2", socket: "new-socket-A" });
  router.addSocket({ socketIdentity: "new-socket-B", source: "198.51.100.3", socket: "new-socket-B" });
  router.authenticateControlled({
    socketIdentity: "new-socket-B",
    endpointId: "endpoint-0001",
    authenticatedContext: Object.freeze({ endpointId: "endpoint-0001" }),
  });
  await detached(value, committed);
  const lease = committed.lease;
  await router.handle({
    socketIdentity: "new-socket-A",
    message: prepare("A", lease, committed.messages.A.resume_token_b64, "request-prepare-A"),
  });
  const provisionalResult = await router.handle({
    socketIdentity: "new-socket-B",
    message: prepare("B", lease, committed.messages.B.resume_token_b64, "request-prepare-B", "AQEBAQEBAQEBAQEBAQEBAQ=="),
  });
  const activationId = provisionalResult.deliveries[0].message.activation_id;
  const steps = [
    ["new-socket-A", "encrypted_resume_confirm", { sender_role: "A", ciphertext_b64: "AQID", ciphertext_bytes: "3" }],
    ["new-socket-B", "encrypted_resume_confirm", { sender_role: "B", ciphertext_b64: "BAQD", ciphertext_bytes: "3" }],
    ["new-socket-B", "controller_resume_activation_ack", { original_deadline_ms: lease.resume_deadline_ms }],
    ["new-socket-B", "controller_post_activation_liveness", { liveness_epoch_b: "live-b-loss-1", heartbeat_sequence_b: "1" }],
    ["new-socket-A", "controller_resume_ready_ack", { sender_role: "A", liveness_epoch_b: "live-b-loss-1", heartbeat_sequence_b: "1" }],
    ["new-socket-B", "controller_resume_ready_ack", { sender_role: "B", liveness_epoch_b: "live-b-loss-1", heartbeat_sequence_b: "1" }],
  ];
  for (const [socketIdentity, type, extra] of steps) {
    const request = router.handle({
      socketIdentity,
      message: activationMessage(type, null, lease, activationId, `request-${type}`, extra),
    });
    if (type === "controller_resume_ready_ack" && extra.sender_role === "B") {
      await assert.rejects(request, /endpoint_offline/u);
    } else {
      await request;
    }
  }
  assert.equal(value.manager.lease(lease.controller_lease_id), null);
  assert.equal(value.hooks.isBusinessAllowed(lease.controller_lease_id), false);
  assert(sent.get("new-socket-A").some((message) => message.type === "controller_resumed"));
  assert.equal(
    sent.get("new-socket-B").some((message) => message.type === "controller_resumed"),
    false,
  );
});
