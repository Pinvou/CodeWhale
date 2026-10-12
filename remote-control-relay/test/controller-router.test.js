import assert from "node:assert/strict";
import test from "node:test";

import { ControllerRouter } from "../controller-router.js";
import { ControllerLivenessHooks } from "../controller-liveness.js";
import { ControllerResumeHooks } from "../controller-resume.js";

function fixture({ clock = Date.now, capabilities = ["encrypted_frames_v1", "task_v1"], realLeaseHooks = false } = {}) {
  const sent = new Map([["socket-A", []], ["socket-B", []]]);
  const aborted = [];
  const terminated = [];
  const audits = [];
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
    negotiated_capabilities: Object.freeze([...capabilities]),
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
    verifyResumeToken: () => { throw new Error("unexpected prepare in heartbeat regression"); },
    completeResume: () => { throw new Error("unexpected resume commit in heartbeat regression"); },
    isPendingHandshake: () => true,
    abortBySocket: async (input) => {
      leaseAlive = false;
      aborted.push(input);
      return { handshakes: 1, leases: 0 };
    },
  };
  const liveness = new ControllerLivenessHooks({ clock, ...(realLeaseHooks ? { leaseManager: leases } : {}) });
  const resume = new ControllerResumeHooks({ clock, ...(realLeaseHooks ? { leaseManager: leases } : {}) });
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
    audit: (event) => audits.push(event),
  });
  router.addSocket({ socketIdentity: "socket-A", source: "198.51.100.1", socket: "socket-A" });
  router.addSocket({ socketIdentity: "socket-B", source: "198.51.100.2", socket: "socket-B" });
  router.authenticateControlled({
    socketIdentity: "socket-B",
    endpointId: "endpoint-0001",
    authenticatedContext: Object.freeze({ endpointId: "endpoint-0001", credential: "secret" }),
  });
  return { router, sent, aborted, terminated, audits, leases, activeLease, liveness, resume };
}

async function connectController(router) {
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
}

function encryptedFrame(role, payloadKind, sequence = "1") {
  return {
    v: 1,
    type: "encrypted_frame",
    connection_id: "connection-0001",
    controller_lease_id: "controller-lease-0001",
    lease_epoch: "1",
    relay_epoch: "relay-epoch-0001",
    remote_control_generation: "generation-0001",
    frame_sequence: sequence,
    direction: role === "A" ? "a_to_b" : "b_to_a",
    payload_kind: payloadKind,
    ciphertext_b64: "AQID",
    ciphertext_bytes: "3",
  };
}

async function joined(value) {
  for (const [socketIdentity, type] of [["socket-A", "controller_pairing_claim"],
    ["socket-B", "finished_receipt_ack"], ["socket-A", "finished_receipt_ack"]]) {
    await value.router.handle({ socketIdentity, message: { v: 1, type, request_id: `request-${type}-${socketIdentity}` } });
  }
}

test("real close hooks snapshot evidence before deleting it and freeze only the first close", async () => {
  let clock = 1_000;
  const value = fixture({ clock: () => clock, capabilities: ["encrypted_frames_v1", "task_v1", "role_liveness_v1", "resume_v1"] });
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

test("surviving role heartbeats during recovery neither error nor extend the frozen deadline", async () => {
  for (const closedRole of ["A", "B"]) {
    let clock = 1_000;
    const value = fixture({ clock: () => clock, realLeaseHooks: true,
      capabilities: ["encrypted_frames_v1", "task_v1", "role_liveness_v1", "resume_v1"] });
    await joined(value);
    const lease = value.activeLease;
    const survivor = closedRole === "A" ? "B" : "A";
    const heartbeat = (role, sequence) => ({
      v: 1, type: "controller_liveness",
      connection_id: lease.connection_id, controller_lease_id: lease.controller_lease_id,
      lease_epoch: lease.lease_epoch, relay_epoch: lease.relay_epoch,
      remote_control_generation: lease.remote_control_generation, sender_role: role,
      liveness_epoch: role === "A" ? lease.liveness_epoch_a : lease.liveness_epoch_b,
      heartbeat_sequence: String(sequence),
    });
    clock = 2_000;
    for (const role of ["A", "B"]) {
      await value.router.handle({ socketIdentity: `socket-${role}`, message: heartbeat(role, 1) });
    }
    clock = 3_000;
    await value.router.removeSocket(`socket-${closedRole}`);
    assert.equal(value.resume.phase(lease.controller_lease_id), "awaiting_prepare");
    for (const sequence of [2, 3, 4]) {
      clock = 3_000 + sequence * 2_000;
      const before = value.sent.get(`socket-${survivor}`).length;
      await value.router.handle({ socketIdentity: `socket-${survivor}`,
        message: heartbeat(survivor, sequence) });
      assert.equal(value.sent.get(`socket-${survivor}`).length, before);
      assert.equal(value.leases.lease(lease.controller_lease_id), lease);
      assert.equal(value.terminated.length, 0);
    }
    // Shape, role, epoch and socket ownership still fail closed in this window.
    for (const patch of [{ sender_role: closedRole }, { liveness_epoch: "old-epoch" },
      { heartbeat_sequence: "0" }, { connection_id: "other-connection" }]) {
      await assert.rejects(value.router.handle({ socketIdentity: `socket-${survivor}`,
        message: { ...heartbeat(survivor, 5), ...patch } }));
      assert.equal(value.leases.lease(lease.controller_lease_id), lease);
    }
    clock = 31_999;
    assert.equal(await value.resume.expire(), 0);
    clock = 32_000;
    assert.equal(await value.resume.expire(), 1);
    assert.equal(value.leases.lease(lease.controller_lease_id), null);
  }
});

test("snapshot exceptions and invalid clocks cannot skip peer, hook or lease cleanup", async () => {
  for (const invalidClock of [false, true]) {
    let clock = 1_000;
    const value = fixture({ clock: () => clock, capabilities: ["encrypted_frames_v1", "task_v1", "role_liveness_v1", "resume_v1"] });
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
  const { router, sent, terminated, audits, activeLease, liveness, resume } = fixture();
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
  assert.deepEqual(audits, ["controller_joined"], "lease manager owns the sole terminal audit");
});

test("closing an unauthenticated controller socket never fabricates a lease lifecycle", async () => {
  const { router, audits, aborted } = fixture();
  await router.removeSocket("socket-A");
  assert.deepEqual(audits, []);
  assert.deepEqual(aborted, [{ socketIdentity: "socket-A", reason: "owner_disconnected", endpointId: undefined }]);
});

test("authenticated encrypted business frames are validated and forwarded both ways", async () => {
  const { router, sent, audits } = fixture();
  await connectController(router);
  sent.get("socket-A").length = 0;
  sent.get("socket-B").length = 0;

  const request = encryptedFrame("A", "capability_snapshot_request");
  assert.deepEqual(await router.handle({ socketIdentity: "socket-A", message: request }), request);
  assert.deepEqual(sent.get("socket-B"), [request]);

  const response = encryptedFrame("B", "capability_snapshot", "2");
  assert.deepEqual(await router.handle({ socketIdentity: "socket-B", message: response }), response);
  assert.deepEqual(sent.get("socket-A"), [response]);
  assert.deepEqual(audits.slice(-2), ["task_forwarded", "task_forwarded"]);
});

test("encrypted business frames fail closed on role or lease mismatch", async () => {
  const { router, sent, audits } = fixture();
  await connectController(router);
  sent.get("socket-A").length = 0;
  sent.get("socket-B").length = 0;

  const wrongDirection = { ...encryptedFrame("A", "capability_snapshot_request"), direction: "b_to_a" };
  await assert.rejects(
    router.handle({ socketIdentity: "socket-A", message: wrongDirection }),
    (error) => error?.code === "controller_business_invalid",
  );
  assert.equal(sent.get("socket-B").length, 0);
  assert.equal(sent.get("socket-A").at(-1).type, "error");
  assert.equal(sent.get("socket-A").at(-1).code, "invalid_message");

  const staleLease = { ...encryptedFrame("A", "capability_snapshot_request", "2"), lease_epoch: "2" };
  await assert.rejects(
    router.handle({ socketIdentity: "socket-A", message: staleLease }),
    (error) => error?.code === "controller_business_invalid",
  );
  assert.equal(sent.get("socket-B").length, 0);
  assert.equal(audits.at(-1), "task_rejected");
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

// DEV-06 审核修复：DEV-09 曾需要修改 server.js/router 才能接入 resume hook，
// 说明冻结的 hook 槽不完整。这里固化「新 hook 可在不改动 router/server 接线
// 的前提下注入」这一能力，并保证非法 hook 失败关闭。
function leaseManagerStub() {
  return {
    claimForDelivery: async () => null,
    isPendingHandshake: () => false,
    setTerminationObserver: () => {},
    verifyResumeToken: () => false,
    completeResume: () => { throw new Error("no fixture lease"); },
    acceptCommitment: () => null,
    forwardFinished: () => null,
    acceptReceipt: async () => null,
    lease: () => null,
    terminateLease: async () => false,
    abortHandshakesBySocket: async () => 0,
    abortBySocket: async () => ({ handshakes: 0, leases: 0 }),
  };
}

test("additional hooks extend the router without editing its wiring", async () => {
  const handled = [];
  const created = [];
  const extraHook = {
    handles: (type) => type === "controller_extra_probe",
    handle: async ({ message }) => { handled.push(message.type); return null; },
    onLeaseCreated: async (lease) => { created.push(lease.controller_lease_id); },
    onSocketClosed: async () => {},
    onLeaseTerminated: async () => {},
    expire: () => 0,
  };
  const router = new ControllerRouter({
    pairingRegistrationHandler: { register: async () => null },
    leaseManager: leaseManagerStub(),
    send: () => true,
    additionalHooks: [extraHook],
  });
  router.addSocket({ socketIdentity: "socket-A", source: "198.51.100.9", socket: "socket-A" });
  const result = await router.handle({
    socketIdentity: "socket-A",
    message: { v: 1, type: "controller_extra_probe", request_id: "request-extra-0001" },
  });
  assert.deepEqual(handled, ["controller_extra_probe"]);
  // The injected hook consumed the message: the router has no reply payload.
  assert.equal(result, null);
  assert.deepEqual(created, []);

  assert.throws(() => new ControllerRouter({
    pairingRegistrationHandler: { register: async () => null },
    leaseManager: leaseManagerStub(),
    send: () => true,
    additionalHooks: [{ handles: () => false }],
  }), (error) => error?.code === "controller_router_hooks_invalid");
});
