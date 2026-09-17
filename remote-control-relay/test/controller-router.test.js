import assert from "node:assert/strict";
import test from "node:test";

import { ControllerRouter } from "../controller-router.js";

function fixture() {
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
    sockets: Object.freeze({ A: "socket-A", B: "socket-B" }),
  });
  const joined = {
    lease: activeLease,
    messages: Object.freeze({
      A: Object.freeze({ v: 1, type: "controller_joined", request_id: "request-claim-0001" }),
      B: Object.freeze({ v: 1, type: "controller_joined", request_id: "request-claim-0001" }),
    }),
  };
  const leases = {
    claimForDelivery: async () => ({ message: reserved, owns_initial_delivery: true }),
    acceptCommitment: ({ message, role }) => ({ message, target_role: role === "A" ? "B" : "A" }),
    forwardFinished: ({ message, role }) => ({ message, target_role: role === "A" ? "B" : "A" }),
    acceptReceipt: async ({ role }) => role === "A" ? joined : null,
    lease: (leaseId) => leaseId === activeLease.controller_lease_id ? activeLease : null,
    terminateLease: async (input) => { terminated.push(input); return true; },
    abortHandshakesBySocket: async () => 0,
    isPendingHandshake: () => true,
    abortBySocket: async (input) => {
      aborted.push(input);
      return { handshakes: 1, leases: 0 };
    },
  };
  const router = new ControllerRouter({
    pairingRegistrationHandler: {
      register: async (message) => ({
        v: 1, type: "desktop_pairing_registered", request_id: message.request_id,
      }),
    },
    leaseManager: leases,
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
  return { router, sent, aborted, terminated };
}

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
  const { router, sent, terminated } = fixture();
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
  }]);
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
