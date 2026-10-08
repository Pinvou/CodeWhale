import assert from "node:assert/strict";
import test from "node:test";

import { ControllerRouter } from "../controller-router.js";
import { ControllerLivenessHooks } from "../controller-liveness.js";
import { ControllerResumeHooks } from "../controller-resume.js";

function fixture({ clock = Date.now } = {}) {
  const sent = new Map([["socket-A", []], ["socket-B", []]]);
  const aborted = [];
  const terminated = [];
  const reserved = Object.freeze({
    v: 1,
    type: "controller_pairing_reserved",
    request_id: "request-claim-0001",
    reservation_id: "reservation-0001",
    transcript: Object.freeze({ endpoint_id: "endpoint-0001" }),
  });
  const activeLease = Object.freeze({
    connection_id: "connection-0001",
    controller_lease_id: "controller-lease-0001",
    lease_epoch: "1",
    relay_epoch: "relay-epoch-0001",
    remote_control_generation: "generation-0001",
    liveness_epoch_a: "liveness-epoch-A-0001",
    liveness_epoch_b: "liveness-epoch-B-0001",
    resume_token_a_sha256: "a".repeat(64),
    resume_token_b_sha256: "b".repeat(64),
    resume_deadline_ms: "30000",
    sockets: Object.freeze({ A: "socket-A", B: "socket-B" }),
  });
  const joined = {
    lease: activeLease,
    messages: Object.freeze({
      A: Object.freeze({ v: 1, type: "controller_joined", request_id: "request-claim-0001" }),
      B: Object.freeze({ v: 1, type: "controller_joined", request_id: "request-claim-0001" }),
    }),
  };
  let leaseAlive = true;
  let terminationObserver;
  const leases = {
    claimForDelivery: async () => ({ message: reserved, owns_initial_delivery: true }),
    acceptCommitment: ({ message, role }) => ({ message, target_role: role === "A" ? "B" : "A" }),
    forwardFinished: ({ message, role }) => ({ message, target_role: role === "A" ? "B" : "A" }),
    acceptReceipt: async ({ role }) => role === "A" ? joined : null,
    lease: (leaseId) => leaseAlive && leaseId === activeLease.controller_lease_id ? activeLease : null,
    setTerminationObserver: (observer) => { terminationObserver = observer; },
    terminateLease: async (input) => { leaseAlive = false; terminated.push(input); await terminationObserver({ controller_lease_id: activeLease.controller_lease_id, reason: input.reason }, activeLease); return true; },
    abortHandshakesBySocket: async () => 0,
    isPendingHandshake: () => true,
    abortBySocket: async (input) => {
      leaseAlive = false;
      aborted.push(input);
      return { handshakes: 1, leases: 0 };
    },
  };
  const liveness = new ControllerLivenessHooks({ clock });
  const resume = new ControllerResumeHooks({ clock });
  const router = new ControllerRouter({
    pairingRegistrationHandler: {
      register: async (message) => ({
        v: 1, type: "desktop_pairing_registered", request_id: message.request_id,
      }),
    },
    leaseManager: leases,
    livenessHooks: liveness,
    resumeHooks: resume,
    send: (socket, message) => {
      sent.get(socket).push(message);
      return true;
    },
  });
  router.addSocket({ socketIdentity: "socket-A", source: "198.51.100.1", socket: "socket-A" });
  router.addSocket({ socketIdentity: "socket-B", source: "198.51.100.2", socket: "socket-B" });
  router.authenticateControlled({
    socketIdentity: "socket-B",
    endpointId: "endpoint-0001",
    authenticatedContext: Object.freeze({ endpointId: "endpoint-0001", credential: "secret" }),
  });
  return { router, sent, aborted, terminated, leases, activeLease, liveness, resume };
}

async function joined(value) {
  for (const [socketIdentity, type] of [["socket-A", "controller_pairing_claim"],
    ["socket-B", "finished_receipt_ack"], ["socket-A", "finished_receipt_ack"]]) {
    await value.router.handle({ socketIdentity, message: { v: 1, type, request_id: `request-${type}-${socketIdentity}` } });
  }
}

test("real close hooks snapshot evidence before deleting it and freeze only the first close", async () => {
  let clock = 1_000;
  const value = fixture({ clock: () => clock });
  await joined(value);
  clock = 34_000;
  for (const role of ["A", "B"]) {
    const lease = value.activeLease;
    await value.router.handle({ socketIdentity: `socket-${role}`, message: {
      v: 1, type: "controller_liveness", connection_id: lease.connection_id,
      controller_lease_id: lease.controller_lease_id, lease_epoch: lease.lease_epoch,
      relay_epoch: lease.relay_epoch, remote_control_generation: lease.remote_control_generation,
      sender_role: role, liveness_epoch: role === "A" ? lease.liveness_epoch_a : lease.liveness_epoch_b,
      heartbeat_sequence: "1",
    } });
  }
  clock = 35_000;
  assert.equal((await value.router.removeSocket("socket-A")).leases, 0);
  assert.equal(value.liveness.resumeDeadline(value.activeLease), null);
  assert.equal(value.resume.phase(value.activeLease.controller_lease_id), "awaiting_prepare");
  clock = 50_000;
  await value.router.removeSocket("socket-B");
  assert.equal(value.router.resolveControlledEndpoint("endpoint-0001"), null);
  assert.equal(value.aborted.length, 0);
  clock = 63_999;
  assert.equal(await value.resume.expire(), 0);
  clock = 64_000;
  assert.equal(await value.resume.expire(), 1);
});

test("snapshot exceptions and invalid clocks cannot skip peer, hook or lease cleanup", async () => {
  for (const invalidClock of [false, true]) {
    let clock = 1_000;
    const value = fixture({ clock: () => clock });
    await joined(value);
    if (invalidClock) clock = Number.NaN;
    else value.liveness.resumeDeadline = () => { throw new Error("snapshot unavailable"); };
    await value.router.removeSocket("socket-B");
    assert.equal(value.router.resolveControlledEndpoint("endpoint-0001"), null);
    assert.equal(value.aborted.length, 1);
    assert.equal(value.leases.lease(value.activeLease.controller_lease_id), null);
    assert.equal(value.resume.phase(value.activeLease.controller_lease_id), null);
    clock = 1_000;
    assert.equal(ControllerLivenessHooks.prototype.resumeDeadline.call(value.liveness, value.activeLease), null);
    assert.deepEqual(await value.router.removeSocket("socket-B"), { handshakes: 0, leases: 0 });
  }
});

test("identifier contract: router accepts wire colon socket and endpoint IDs", () => {
  const { router } = fixture();
  router.addSocket({ socketIdentity: "socket:B", source: "198.51.100.3", socket: {} });
  router.authenticateControlled({ socketIdentity: "socket:B", endpointId: "endpoint:0001", authenticatedContext: {} });
  assert.deepEqual(router.resolveControlledEndpoint("endpoint:0001"), { socketIdentity: "socket:B" });
  assert.throws(() => router.addSocket({ socketIdentity: "socket~C", source: "source-C", socket: {} }),
    { code: "socket_identity_invalid" });
});

test("identifier contract: controlled Web endpoint mapping preserves released legacy identities", () => {
  const { router } = fixture();
  for (const endpointId of ["_old", "-old"]) {
    router.authenticateControlled({ socketIdentity: "socket-B", endpointId, authenticatedContext: {} });
    assert.deepEqual(router.resolveControlledEndpoint(endpointId), { socketIdentity: "socket-B" });
  }
});

test("router delivers reservation, handshake frames and role-specific joined results", async () => {
  const { router, sent } = fixture();
  await router.handle({
    socketIdentity: "socket-A",
    message: { v: 1, type: "controller_pairing_claim", request_id: "request-claim-0001" },
  });
  assert.equal(sent.get("socket-A")[0].type, "controller_pairing_reserved");
  assert.equal(sent.get("socket-B")[0].type, "controller_pairing_reserved");

  const commitment = {
    v: 1, type: "finished_receipt_commit", request_id: "request-commit-A",
  };
  await router.handle({ socketIdentity: "socket-A", message: commitment });
  assert.equal(sent.get("socket-A")[1], commitment);
  assert.equal(sent.get("socket-B")[1], commitment);
  const finished = {
    v: 1, type: "encrypted_handshake_finished", request_id: "request-finished-B",
  };
  await router.handle({ socketIdentity: "socket-B", message: finished });
  assert.equal(sent.get("socket-A")[2], finished);

  await router.handle({
    socketIdentity: "socket-B",
    message: { v: 1, type: "finished_receipt_ack", request_id: "request-receipt-A" },
  });
  await router.handle({
    socketIdentity: "socket-A",
    message: { v: 1, type: "finished_receipt_ack", request_id: "request-receipt-B" },
  });
  assert.equal(sent.get("socket-A").at(-1).type, "controller_joined");
  assert.equal(sent.get("socket-B").at(-1).type, "controller_joined");
});

test("router keeps registration on authenticated B and cleans up disconnected peers", async () => {
  const { router, sent, aborted } = fixture();
  await router.handle({
    socketIdentity: "socket-B",
    message: { v: 1, type: "desktop_pairing_register", request_id: "request-register-0001" },
  });
  assert.equal(sent.get("socket-B")[0].type, "desktop_pairing_registered");
  assert.deepEqual(router.resolveControlledEndpoint("endpoint-0001"), { socketIdentity: "socket-B" });
  await router.removeSocket("socket-B");
  assert.equal(router.resolveControlledEndpoint("endpoint-0001"), null);
  assert.equal(aborted.length, 1);
});

test("explicit controller disconnect forwards once and terminates the shared lease", async () => {
  const { router, sent, terminated, activeLease, liveness, resume } = fixture();
  await router.handle({
    socketIdentity: "socket-A",
    message: { v: 1, type: "controller_pairing_claim", request_id: "request-claim-0001" },
  });
  await router.handle({
    socketIdentity: "socket-B",
    message: { v: 1, type: "finished_receipt_ack", request_id: "request-receipt-A" },
  });
  await router.handle({
    socketIdentity: "socket-A",
    message: { v: 1, type: "finished_receipt_ack", request_id: "request-receipt-B" },
  });
  const disconnect = {
    v: 1,
    type: "controller_disconnect",
    request_id: "request-disconnect-0001",
    connection_id: "connection-0001",
    controller_lease_id: "controller-lease-0001",
    lease_epoch: "1",
    relay_epoch: "relay-epoch-0001",
    remote_control_generation: "generation-0001",
    sender_role: "A",
    reason: "user_requested",
  };
  await router.handle({ socketIdentity: "socket-A", message: disconnect });
  assert.deepEqual(sent.get("socket-B").at(-1), disconnect);
  assert.deepEqual(terminated, [{
    controllerLeaseId: "controller-lease-0001", reason: "user_requested",
    expectedLease: activeLease,
  }]);
  assert.equal(liveness.resumeDeadline(activeLease), null);
  assert.equal(resume.phase(activeLease.controller_lease_id), null);
});

test("router collapses internal claim failures to a bounded public error", async () => {
  const { router, sent } = fixture();
  await assert.rejects(router.handle({
    socketIdentity: "socket-A",
    message: { v: 1, type: "unknown", request_id: "request-invalid-0001" },
  }), (error) => error?.code === "controller_message_unsupported");
  assert.deepEqual(sent.get("socket-A").at(-1), {
    v: 1,
    type: "error",
    request_id: "request-invalid-0001",
    code: "invalid_message",
    message: "消息无效",
  });
});
