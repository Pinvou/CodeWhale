import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { ControllerLeaseManager } from "../controller-lease.js";
import { ControllerResumeHooks } from "../controller-resume.js";
import { ControllerLivenessHooks } from "../controller-liveness.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";
import { ControllerRouter } from "../controller-router.js";
import { isValidControllerIdentifier, receiptCommitmentHex } from "../protocol-controller.js";

const CAPABILITIES = Object.freeze([
  "pairing_v1", "encrypted_frames_v1", "finished_receipt_v1", "role_liveness_v1", "resume_v1",
]);
const ZERO_NONCE = "AAAAAAAAAAAAAAAAAAAAAA==";

function publicKey() {
  return crypto.generateKeyPairSync("x25519").publicKey.export({ type: "spki", format: "der" })
    .subarray(-32).toString("base64");
}

function fixture({ startClock = 1_000, onLeaseTerminated = async () => {} } = {}) {
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
  const slots = new EndpointInteractionSlots();
  const reservations = new ControllerReservationManager({
    pairingStore: pairings, interactionSlots: slots, clock: () => clock,
  });
  const manager = new ControllerLeaseManager({
    pairingStore: pairings,
    reservationManager: reservations,
    relayEpoch: "relay-epoch-0001",
    signingKeyId: "relay-signing-key-0001",
    signTranscript: (preimage) => crypto.sign(null, preimage, crypto.generateKeyPairSync("ed25519").privateKey),
    resolveControlledEndpoint: async () => ({ socketIdentity: "old-socket-B" }),
    clock: () => clock,
    onLeaseTerminated,
  });
  const hooks = new ControllerResumeHooks({
    leaseManager: manager,
    clock: () => clock,
    randomBytes: crypto.randomBytes,
  });
  const liveness = new ControllerLivenessHooks({ clock: () => clock, leaseManager: manager });
  return {
    manager,
    hooks,
    liveness,
    pairing,
    slots,
    reservations,
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
  await value.liveness.onLeaseCreated(committed.lease);
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
    evidence_deadline_ms: value.liveness.resumeDeadline(committed.lease),
  })).retain_lease, true);
  assert.equal((await value.hooks.onSocketClosed({
    socket_identity: "old-socket-B", role: "B", lease: committed.lease,
    evidence_deadline_ms: value.liveness.resumeDeadline(committed.lease),
  })).retain_lease, true);
}

async function heartbeat(value, lease, role, sequence) {
  return value.liveness.handle({
    message: { v: 1, type: "controller_liveness", ...context(lease), sender_role: role,
      liveness_epoch: role === "A" ? lease.liveness_epoch_a : lease.liveness_epoch_b,
      heartbeat_sequence: String(sequence) },
    socket_identity: lease.sockets[role], role, lease,
  });
}

test("long healthy connection freezes latest authenticated role cutoffs, not the wire identity", async () => {
  const value = fixture();
  const committed = await createLease(value);
  value.clock = 29_000;
  await heartbeat(value, committed.lease, "A", 1);
  await heartbeat(value, committed.lease, "B", 1);
  value.clock = 34_000;
  await heartbeat(value, committed.lease, "A", 2);
  await heartbeat(value, committed.lease, "B", 2);
  value.clock = 35_000;
  const provisionalResult = await provisional(value, committed);
  assert.equal(provisionalResult.deliveries.length, 2);
  assert.equal(provisionalResult.deliveries[0].message.original_deadline_ms, "31000");
  // Even later authenticated heartbeats on either role cannot reset a frozen
  // recovery cycle. Repeated close and prepare retry share the same cutoff.
  value.clock = 50_000;
  await heartbeat(value, committed.lease, "A", 3);
  await heartbeat(value, committed.lease, "B", 3);
  await detached(value, committed);
  value.clock = 64_000;
  assert.equal(await value.hooks.expire(), 1);
  assert.equal(value.manager.lease(committed.lease.controller_lease_id), null);
});

test("missing or expired A evidence fails closed and B activity cannot create resume budget", async () => {
  for (const missing of [false, true]) {
    const value = fixture();
    const committed = await createLease(value);
    value.clock = 29_000;
    await heartbeat(value, committed.lease, "B", 1);
    if (missing) await value.liveness.onSocketClosed({ lease: committed.lease });
    value.clock = 31_000;
    await assert.rejects(heartbeat(value, committed.lease, "B", 2),
      { code: missing ? "controller_liveness_state_not_found" : "controller_liveness_expired" });
    const evidenceDeadline = value.liveness.resumeDeadline(committed.lease);
    assert.equal(evidenceDeadline, null);
    const result = await value.hooks.onSocketClosed({ socket_identity: "old-socket-A", role: "A",
      lease: committed.lease, evidence_deadline_ms: evidenceDeadline });
    assert.equal(result.retain_lease, false);
    assert.equal(value.hooks.phase(committed.lease.controller_lease_id), null);
  }
});

test("a second resume uses exactly the next identity delivered to both roles", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const first = await provisional(value, committed);
  await pending(value, committed, first);
  value.clock = 5_000;
  const result = await readyResult(value, committed, first);
  const updated = (await result.commit()).lease;
  assert.equal(updated.resume_deadline_ms, "35000");
  assert(result.deliveries.every(({ message }) => message.resume_deadline_ms === "35000"));
  await value.liveness.onLeaseCreated(updated);
  value.clock = 6_000;
  assert.equal((await value.hooks.onSocketClosed({ socket_identity: "new-socket-A", role: "A",
    lease: updated, evidence_deadline_ms: value.liveness.resumeDeadline(updated) })).retain_lease, true);
  const token = result.deliveries.find(({ role }) => role === "A").message.new_resume_token_b64;
  const second = await value.hooks.handle({ message: prepare("A", updated, token, "second-prepare-A"),
    socket_identity: "third-socket-A", role: "A", lease: updated });
  assert.deepEqual(second.deliveries, []);
  value.clock = 35_000;
  assert.equal(await value.hooks.expire(), 1);
});

test("final delivery delay cannot commit after the frozen recovery cutoff", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const first = await provisional(value, committed);
  await pending(value, committed, first);
  const result = await readyResult(value, committed, first);
  value.clock = 31_000;
  await assert.rejects(result.commit(), /controller_resume_deadline_expired/u);
  await result.abort();
  assert.equal(value.manager.lease(committed.lease.controller_lease_id), null);
});

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
  assert.equal(resumedA.resume_deadline_ms, String(value.clock + 30_000));
  assert.equal(resumedB.resume_deadline_ms, resumedA.resume_deadline_ms);
  assert.equal(updated.resume_deadline_ms, resumedA.resume_deadline_ms);
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

test("resume epoch grammar follows wire identifiers and rejects invalid finals before mutation", async () => {
  assert.equal(isValidControllerIdentifier("live:a-2"), true);
  assert.equal(isValidControllerIdentifier("live~a-2"), false);
  assert.equal(isValidControllerIdentifier(":live-a-2"), false);
  assert.equal(isValidControllerIdentifier("live:a-2\n"), false);
  const value = fixture();
  const committed = await createLease(value);
  const args = {
    controllerLeaseId: committed.lease.controller_lease_id,
    expectedLease: committed.lease,
    activationId: "activation-grammar-1",
    sockets: { A: "grammar-socket-A", B: "grammar-socket-B" },
    livenessEpochA: "live:a-2",
    livenessEpochB: "live:b-2",
    tokenA: Buffer.alloc(32, 3),
    tokenB: Buffer.alloc(32, 4),
  };
  for (const [field, label] of [["livenessEpochA", "liveness_epoch_a"], ["livenessEpochB", "liveness_epoch_b"]]) {
    assert.throws(() => value.manager.completeResume({ ...args, [field]: "live~bad-2" }), new RegExp(label + "_invalid", "u"));
    assert.deepEqual(value.manager.lease(committed.lease.controller_lease_id), committed.lease);
  }
  const updated = value.manager.completeResume(args);
  assert.equal(updated.liveness_epoch_a, "live:a-2");
  assert.equal(updated.liveness_epoch_b, "live:b-2");
  assert.equal(updated.resume_deadline_ms, String(value.clock + 30_000));
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
  const committed = await createLease(value);
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
  const committed = await createLease(value);
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

test("completed resume exact retries do not terminate or rotate the healthy lease", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const first = await provisional(value, committed);
  await pending(value, committed, first);
  const result = await readyResult(value, committed, first);
  const updated = (await result.commit()).lease;
  // The router also announces the committed lease to all hooks.
  await value.hooks.onLeaseCreated(updated);
  const activationId = first.deliveries[0].message.activation_id;
  const retries = [
    ["A", prepare("A", committed.lease, committed.messages.A.resume_token_b64, "request-prepare-A", ZERO_NONCE)],
    ["B", prepare("B", committed.lease, committed.messages.B.resume_token_b64, "request-prepare-B", "AQEBAQEBAQEBAQEBAQEBAQ==")],
    ["A", activationMessage("encrypted_resume_confirm", "A", committed.lease, activationId, "request-confirm-A", { ciphertext_b64: "AQID", ciphertext_bytes: "3" })],
    ["B", activationMessage("encrypted_resume_confirm", "B", committed.lease, activationId, "request-confirm-B", { ciphertext_b64: "BAQD", ciphertext_bytes: "3" })],
    ["B", activationMessage("controller_resume_activation_ack", null, committed.lease, activationId, "request-ack-B", { original_deadline_ms: committed.lease.resume_deadline_ms })],
    ["B", activationMessage("controller_post_activation_liveness", null, committed.lease, activationId, "request-live-B", { liveness_epoch_b: "live-b-new-1", heartbeat_sequence_b: "1" })],
    ["A", activationMessage("controller_resume_ready_ack", "A", committed.lease, activationId, "request-ready-A", { liveness_epoch_b: "live-b-new-1", heartbeat_sequence_b: "1" })],
    ["B", activationMessage("controller_resume_ready_ack", "B", committed.lease, activationId, "request-ready-B", { liveness_epoch_b: "live-b-new-1", heartbeat_sequence_b: "1" })],
  ];
  for (const [role, message] of retries) {
    const replay = await value.hooks.handle({ message, role, socket_identity: `new-socket-${role}`, lease: updated });
    assert.deepEqual(replay.deliveries, []);
    assert.deepEqual(value.manager.lease(updated.controller_lease_id), updated);
    assert.equal(value.hooks.phase(updated.controller_lease_id), "connected");
  }
  for (const [role, message] of retries.slice(2)) {
    for (const change of [
      { socket_identity: "untrusted-socket" },
      { message: { ...message, request_id: "altered-request" } },
      { message: { ...message, remote_control_generation: "other-generation" } },
      { message: { ...message, extra: "not-in-schema" } },
    ]) {
      await assert.rejects(value.hooks.handle({ message, role, socket_identity: `new-socket-${role}`, lease: updated, ...change }));
      assert.deepEqual(value.manager.lease(updated.controller_lease_id), updated);
    }
  }
});

test("provisional delivery failure terminates the real lease even without result.abort", async () => {
  const value = fixture();
  const router = new ControllerRouter({
    pairingRegistrationHandler: { register: async (message) => message },
    leaseManager: value.manager,
    send: (_socket, message) => message.type !== "controller_resume_provisional",
    livenessHooks: value.liveness, resumeHooks: value.hooks,
  });
  const committed = await createLease(value);
  for (const role of ["A", "B"]) router.addSocket({ socketIdentity: `new-socket-${role}`, source: "198.51.100.2", socket: `new-socket-${role}` });
  router.authenticateControlled({ socketIdentity: "new-socket-B", endpointId: "endpoint-0001", authenticatedContext: { endpointId: "endpoint-0001" } });
  await detached(value, committed);
  await router.handle({ socketIdentity: "new-socket-A", message: prepare("A", committed.lease, committed.messages.A.resume_token_b64, "request-prepare-A", ZERO_NONCE) });
  await assert.rejects(router.handle({ socketIdentity: "new-socket-B", message: prepare("B", committed.lease, committed.messages.B.resume_token_b64, "request-prepare-B", "AQEBAQEBAQEBAQEBAQEBAQ==") }), /endpoint_offline/u);
  assert.equal(value.manager.lease(committed.lease.controller_lease_id), null);
  assert.equal(value.hooks.phase(committed.lease.controller_lease_id), null);
  assert.equal(value.liveness.resumeDeadline(committed.lease), null);
});

function recoverySteps(lease, activationId) {
  return [
    ["A", "encrypted_resume_confirm", { sender_role: "A", ciphertext_b64: "AQID", ciphertext_bytes: "3" }],
    ["B", "encrypted_resume_confirm", { sender_role: "B", ciphertext_b64: "BAQD", ciphertext_bytes: "3" }],
    ["B", "controller_resume_activation_ack", { original_deadline_ms: lease.resume_deadline_ms }],
    ["B", "controller_post_activation_liveness", { liveness_epoch_b: "live-b-matrix-1", heartbeat_sequence_b: "1" }],
    ["A", "controller_resume_ready_ack", { sender_role: "A", liveness_epoch_b: "live-b-matrix-1", heartbeat_sequence_b: "1" }],
    ["B", "controller_resume_ready_ack", { sender_role: "B", liveness_epoch_b: "live-b-matrix-1", heartbeat_sequence_b: "1" }],
  ].map(([role, type, extra], index) => ({ role, message: activationMessage(type, null, lease, activationId, `matrix-${index}`, extra) }));
}

async function routedFixture(options = {}) {
  const value = fixture(options);
  const router = new ControllerRouter({
    pairingRegistrationHandler: { register: async (message) => message },
    leaseManager: value.manager, send: options.send ?? (() => true),
    livenessHooks: value.liveness, resumeHooks: value.hooks,
  });
  const committed = await createLease(value);
  const socketIdentities = options.socketIdentities ?? { A: "new-socket-A", B: "new-socket-B" };
  for (const role of ["A", "B"]) router.addSocket({ socketIdentity: socketIdentities[role], source: "198.51.100.2", socket: socketIdentities[role] });
  router.authenticateControlled({ socketIdentity: socketIdentities.B, endpointId: "endpoint-0001", authenticatedContext: { endpointId: "endpoint-0001" } });
  await detached(value, committed);
  return { ...value, value, router, committed, socketIdentities };
}

async function routedProvisional(value) {
  const { router, committed, socketIdentities } = value;
  await router.handle({ socketIdentity: socketIdentities.A, message: prepare("A", committed.lease, committed.messages.A.resume_token_b64, "request-prepare-A", ZERO_NONCE) });
  return router.handle({ socketIdentity: socketIdentities.B, message: prepare("B", committed.lease, committed.messages.B.resume_token_b64, "request-prepare-B", "AQEBAQEBAQEBAQEBAQEBAQ==") });
}

test("A05 identifier propagation accepts actual colon resume peer updates through final rotation", async () => {
  const value = await routedFixture({ socketIdentities: { A: "new:socket:A", B: "new:socket:B" } });
  const result = await routedProvisional(value);
  for (const { role, message } of recoverySteps(value.committed.lease, result.deliveries[0].message.activation_id)) {
    await value.router.handle({ socketIdentity: value.socketIdentities[role], message });
  }
  const current = value.manager.lease(value.committed.lease.controller_lease_id);
  assert.deepEqual(current.sockets, value.socketIdentities);
  assert.equal(value.hooks.phase(current.controller_lease_id), "connected");
  assert.equal(value.hooks.isBusinessAllowed(current.controller_lease_id), true);
});

test("A05 identifier propagation rejects tilde peer updates and terminates only the exact real lease", async () => {
  const value = await routedFixture();
  const original = value.hooks.handle.bind(value.hooks);
  value.hooks.handle = async (input) => {
    const result = await original(input);
    return { ...result, peer_updates: result.peer_updates?.map((update) => ({ ...update, socket_identity: "new~socket" })) };
  };
  await assert.rejects(routedProvisional(value), { code: "controller_peer_updates_invalid" });
  assert.equal(value.manager.lease(value.committed.lease.controller_lease_id), null);
  assert.equal(value.hooks.phase(value.committed.lease.controller_lease_id), null);
  assert.equal(value.liveness.resumeDeadline(value.committed.lease), null);
});

test("each accepted resume delivery failure releases the authoritative lease and all peer links", async () => {
  const failures = [
    ["controller_resume_provisional", "A"], ["controller_resume_provisional", "B"],
    ["encrypted_resume_confirm", "A"], ["encrypted_resume_confirm", "B"],
    ["controller_resume_activation_pending", "A"], ["controller_resume_activation_pending", "B"],
    ["controller_post_activation_liveness_confirmed", "A"], ["controller_post_activation_liveness_confirmed", "B"],
    ["controller_resume_ready_ack", "B"], ["controller_resumed", "A"], ["controller_resumed", "B"],
  ];
  for (const [failedType, failedRole] of failures) {
    let notifications = 0;
    const value = await routedFixture({
      onLeaseTerminated: async () => { notifications += 1; },
      send: (socket, message) => !(message.type === failedType && socket === `new-socket-${failedRole}`),
    });
    const run = async () => {
      const provisionalResult = await routedProvisional(value);
      for (const { role, message } of recoverySteps(value.committed.lease, provisionalResult.deliveries[0].message.activation_id)) {
        await value.router.handle({ socketIdentity: `new-socket-${role}`, message });
      }
    };
    await assert.rejects(run(), /endpoint_offline/u, `${failedType}:${failedRole}`);
    assert.equal(value.manager.lease(value.committed.lease.controller_lease_id), null);
    assert.equal(value.hooks.phase(value.committed.lease.controller_lease_id), null);
    assert.equal(value.liveness.resumeDeadline(value.committed.lease), null);
    assert.equal(notifications, 1);
    for (const role of ["A", "B"]) {
      await assert.rejects(value.router.handle({ socketIdentity: `new-socket-${role}`, message: { type: "controller_disconnect" } }), /controller_role_invalid/u);
    }
  }
});

test("partial peer updates and abort errors still perform precise authoritative cleanup", async () => {
  const value = await routedFixture();
  const original = value.hooks.handle.bind(value.hooks);
  value.hooks.handle = async (input) => {
    const result = await original(input);
    if (result.peer_updates?.length === 1) return Object.freeze({ ...result,
      peer_updates: [...result.peer_updates, { ...result.peer_updates[0], role: "B", socket_identity: "missing-socket" }],
      abort: async () => { throw new Error("injected-abort-error"); },
    });
    return result;
  };
  await assert.rejects(routedProvisional(value), /endpoint_offline/u);
  assert.equal(value.manager.lease(value.committed.lease.controller_lease_id), null);
  assert.equal(value.hooks.phase(value.committed.lease.controller_lease_id), null);
  await assert.rejects(value.router.handle({ socketIdentity: "new-socket-A", message: { type: "controller_disconnect" } }), /controller_role_invalid/u);
});

test("post-rotation hook install failure cleans both new and predecessor hook owners", async () => {
  for (const which of ["hooks", "liveness"]) {
    const value = await routedFixture();
    const original = value[which].onLeaseCreated.bind(value[which]);
    value[which].onLeaseCreated = async (lease) => {
      if (lease.sockets.A === "new-socket-A") throw new Error("injected-hook-install");
      return original(lease);
    };
    const result = await routedProvisional(value);
    const steps = recoverySteps(value.committed.lease, result.deliveries[0].message.activation_id);
    for (const { role, message } of steps.slice(0, -1)) await value.router.handle({ socketIdentity: `new-socket-${role}`, message });
    await assert.rejects(value.router.handle({ socketIdentity: "new-socket-B", message: steps.at(-1).message }), /injected-hook-install/u);
    assert.equal(value.manager.lease(value.committed.lease.controller_lease_id), null);
    assert.equal(value.hooks.phase(value.committed.lease.controller_lease_id), null);
    assert.equal(value.liveness.resumeDeadline(value.committed.lease), null);
  }
});

test("completed transaction abort and stale expected owner cannot terminate the new revision", async () => {
  const value = await routedFixture();
  const result = await routedProvisional(value);
  let final;
  for (const { role, message } of recoverySteps(value.committed.lease, result.deliveries[0].message.activation_id)) {
    final = await value.router.handle({ socketIdentity: `new-socket-${role}`, message });
  }
  const updated = value.manager.lease(value.committed.lease.controller_lease_id);
  await final.abort();
  assert.equal(await value.manager.terminateLease({ controllerLeaseId: updated.controller_lease_id,
    expectedLease: value.committed.lease }), false);
  await value.hooks.onLeaseTerminated(updated, value.committed.lease);
  await value.liveness.onLeaseTerminated(updated, value.committed.lease);
  assert.equal(value.manager.lease(updated.controller_lease_id), updated);
  assert.equal(value.hooks.phase(updated.controller_lease_id), "connected");
  assert.notEqual(value.liveness.resumeDeadline(updated), null);
});

test("release failures still notify exact observers and aggregate every failure", async (t) => {
  for (const code of ["release_adapter_failed", "interaction_slot_revision_exhausted", "observer_also_failed"]) {
    await t.test(code, async () => {
      let notifications = 0;
      let observed;
      const hookError = new Error("fixed-hook-failure");
      const observerError = new Error("fixed-observer-failure");
      const releaseError = Object.assign(new Error(code), { code });
      const value = fixture({ onLeaseTerminated: async () => { notifications += 1; throw hookError; } });
      value.manager.setTerminationObserver(async (event, lease, previous) => {
        observed = { event, lease, previous };
        if (code === "observer_also_failed") throw observerError;
      });
      const committed = await createLease(value);
      const owner = value.slots.read(committed.lease.endpoint_id);
      value.reservations.releaseControllerLease = () => { throw releaseError; };
      await assert.rejects(value.manager.terminateLease({ controllerLeaseId: committed.lease.controller_lease_id,
        expectedLease: committed.lease }), (error) => {
        assert.ok(error instanceof AggregateError);
        assert.deepEqual(error.errors, code === "observer_also_failed"
          ? [releaseError, observerError, hookError] : [releaseError, hookError]);
        return true;
      });
      assert.equal(observed.lease, committed.lease);
      assert.equal(observed.event.controller_lease_id, committed.lease.controller_lease_id);
      assert.equal(notifications, 1);
      assert.deepEqual(value.slots.read(committed.lease.endpoint_id), owner);
      assert.equal(value.manager.lease(committed.lease.controller_lease_id), null);
      const replacement = value.slots.compareAndSwap(committed.lease.endpoint_id, owner.revision,
        { kind: "web_lease", owner_id: "replacement-web" });
      assert.equal(await value.manager.terminateLease({ controllerLeaseId: committed.lease.controller_lease_id,
        expectedLease: committed.lease }), false);
      assert.deepEqual(value.slots.read(committed.lease.endpoint_id), replacement);
      assert.equal(notifications, 1);
    });
  }
});

test("a failing replacement callback cannot prevent hook cleanup or duplicate notifications", async () => {
  let notifications = 0;
  const value = await routedFixture({ onLeaseTerminated: async () => { notifications += 1; throw new Error("notification-failed"); } });
  await assert.rejects(value.manager.terminateLease({ controllerLeaseId: value.committed.lease.controller_lease_id,
    expectedLease: value.committed.lease }), /controller_lease_termination_callback_failed/u);
  assert.equal(value.manager.lease(value.committed.lease.controller_lease_id), null);
  assert.equal(value.hooks.phase(value.committed.lease.controller_lease_id), null);
  assert.equal(value.liveness.resumeDeadline(value.committed.lease), null);
  assert.equal(await value.manager.terminateLease({ controllerLeaseId: value.committed.lease.controller_lease_id }), false);
  assert.equal(notifications, 1);
});

test("either silent role expires the real connected lease and releases its interaction slot", async () => {
  for (const activeRole of ["A", "B", "both"]) {
    const value = fixture();
    const router = new ControllerRouter({ pairingRegistrationHandler: { register: async (m) => m },
      leaseManager: value.manager, send: () => true, livenessHooks: value.liveness, resumeHooks: value.hooks });
    const committed = await createLease(value);
    value.clock = 30_000;
    for (const role of activeRole === "both" ? ["A", "B"] : [activeRole]) await heartbeat(value, committed.lease, role, 1);
    value.clock = 31_000;
    if (activeRole === "both") {
      assert.equal(await router.expire(), 0);
      assert.equal(value.manager.lease(committed.lease.controller_lease_id), committed.lease);
    } else {
      assert.equal(await router.expire(), 1);
      assert.equal(value.manager.lease(committed.lease.controller_lease_id), null);
      assert.equal(value.hooks.phase(committed.lease.controller_lease_id), null);
      // The real reservation manager must admit a new claim, not controller_busy.
      const newLease = await createLease(value);
      assert.notEqual(newLease.lease.controller_lease_id, committed.lease.controller_lease_id);
    }
  }
});

test("late old-owner expiry cannot terminate a freshly rotated lease", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const provisionalResult = await provisional(value, committed);
  await pending(value, committed, provisionalResult);
  const result = await readyResult(value, committed, provisionalResult);
  const updated = (await result.commit()).lease;
  result.finalize();
  // The old liveness owner expires while its new installation is delayed.
  value.clock = 31_000;
  assert.equal(await value.liveness.expire(), 1);
  assert.equal(value.manager.lease(updated.controller_lease_id), updated);
  await value.liveness.onLeaseCreated(updated);
  assert.notEqual(value.liveness.resumeDeadline(updated), null);
});

test("authenticated explicit disconnect terminates even if its notification cannot be delivered", async () => {
  const value = await routedFixture({ send: (_socket, message) => message.type !== "controller_disconnect" });
  const result = await routedProvisional(value);
  for (const { role, message } of recoverySteps(value.committed.lease, result.deliveries[0].message.activation_id)) {
    await value.router.handle({ socketIdentity: `new-socket-${role}`, message });
  }
  const updated = value.manager.lease(value.committed.lease.controller_lease_id);
  await assert.rejects(value.router.handle({ socketIdentity: "new-socket-A", message: {
    v: 1, type: "controller_disconnect", request_id: "disconnect-request", ...context(updated),
    sender_role: "A", reason: "user_requested",
  } }), /endpoint_offline/u);
  assert.equal(value.manager.lease(updated.controller_lease_id), null);
  assert.equal(value.hooks.phase(updated.controller_lease_id), null);
  assert.equal(value.liveness.resumeDeadline(updated), null);
});

test("exact accepted earlier-phase retries cannot roll back or terminate an in-flight later phase", async () => {
  const value = fixture();
  const committed = await createLease(value);
  const first = await provisional(value, committed);
  await pending(value, committed, first);
  const result = await readyResult(value, committed, first);
  assert.equal(value.hooks.phase(committed.lease.controller_lease_id), "committing");
  const activationId = first.deliveries[0].message.activation_id;
  const retries = [
    ["A", prepare("A", committed.lease, committed.messages.A.resume_token_b64, "request-prepare-A", ZERO_NONCE)],
    ["A", activationMessage("encrypted_resume_confirm", "A", committed.lease, activationId, "request-confirm-A", { ciphertext_b64: "AQID", ciphertext_bytes: "3" })],
    ["A", activationMessage("controller_resume_ready_ack", "A", committed.lease, activationId, "request-ready-A", { liveness_epoch_b: "live-b-new-1", heartbeat_sequence_b: "1" })],
  ];
  for (const [role, message] of retries) {
    assert.deepEqual((await value.hooks.handle({ message, role, socket_identity: `new-socket-${role}`, lease: committed.lease })).deliveries, []);
    assert.equal(value.hooks.phase(committed.lease.controller_lease_id), "committing");
    assert.equal(value.manager.lease(committed.lease.controller_lease_id), committed.lease);
  }
  await result.commit();
  result.finalize();
});
