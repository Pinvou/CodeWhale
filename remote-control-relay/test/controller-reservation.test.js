import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";

function publicKey() {
  return crypto.generateKeyPairSync("x25519").publicKey.export({ type: "spki", format: "der" })
    .subarray(-32).toString("base64");
}

function pairingStore() {
  const active = Object.freeze({
    pairing_id: "pairing-0001", endpoint_id: "endpoint-0001",
    remote_control_generation: "generation-0001", expires_at_ms: "300000",
  });
  const terminal = [];
  return {
    terminal,
    findActiveByCode: (code) => code === "2345678" ? active : null,
    terminate: async ({ pairingId }) => { terminal.push(pairingId); },
  };
}

function claim(manager, overrides = {}) {
  return manager.claim({
    pairingCode: "234-5678",
    requestId: "claim-request-0001",
    socketIdentity: "socket-0001",
    source: "source-0001",
    handshakePublicKeyB64: publicKey(),
    ...overrides,
  });
}

test("monotonic reservation phases expire despite wall rollback without changing wire deadlines", async () => {
  for (const phase of ["reserved", "handshake_pending"]) {
    let wall = 1000; let elapsed = 0;
    const slots = new EndpointInteractionSlots();
    const manager = new ControllerReservationManager({ pairingStore: pairingStore(), interactionSlots: slots,
      clock: () => wall, elapsedClock: () => elapsed });
    const key = publicKey();
    let reservation = claim(manager, { handshakePublicKeyB64: key });
    assert.equal(reservation.reserved_deadline_ms, "11000");
    if (phase === "handshake_pending") {
      reservation = manager.beginHandshake({ reservationId: reservation.reservation_id, socketIdentity: "socket-0001" });
      assert.equal(reservation.handshake_deadline_ms, "21000");
    }
    elapsed = phase === "reserved" ? 9999 : 19999; wall = 100;
    assert.equal(claim(manager, { handshakePublicKeyB64: key }), reservation);
    elapsed += 1;
    assert.throws(() => claim(manager, { handshakePublicKeyB64: key }), { code: "pairing_code_invalid" });
    assert.throws(() => phase === "reserved"
      ? manager.beginHandshake({ reservationId: reservation.reservation_id, socketIdentity: "socket-0001" })
      : manager.promoteToLease({ reservationId: reservation.reservation_id, socketIdentity: "socket-0001", controllerLeaseId: "lease-0001" }), { code: "reservation_expired" });
    assert.equal(await manager.expire(), 1);
    assert.equal(slots.read("endpoint-0001").owner, null);
  }
});

test("wall forward and invalid elapsed clocks fail closed without resetting reservation budgets", async () => {
  for (const elapsedValue of [NaN, Infinity, -1, 1]) {
    let wall = 1000; let elapsed = 2;
    const slots = new EndpointInteractionSlots();
    const manager = new ControllerReservationManager({ pairingStore: pairingStore(), interactionSlots: slots,
      clock: () => wall, elapsedClock: () => elapsed });
    const reservation = claim(manager);
    elapsed = elapsedValue;
    assert.throws(() => manager.beginHandshake({ reservationId: reservation.reservation_id, socketIdentity: "socket-0001" }),
      { code: "controller_reservation_clock_invalid" });
    assert.equal(slots.read("endpoint-0001").owner.owner_id, reservation.reservation_id);
    elapsed = 10002;
    assert.equal(await manager.expire(), 1);
  }
  let wall = 1000;
  const manager = new ControllerReservationManager({ pairingStore: pairingStore(), interactionSlots: new EndpointInteractionSlots(),
    clock: () => wall, elapsedClock: () => 0 });
  const reservation = claim(manager);
  wall = 11000;
  assert.throws(() => manager.beginHandshake({ reservationId: reservation.reservation_id, socketIdentity: "socket-0001" }), { code: "reservation_expired" });
  assert.equal(await manager.expire(), 1);
});

test("elapsed expiry invalidation retry retains exact slot and never grants a new budget", async () => {
  let wall = 1000; let elapsed = 0; let failed = true;
  const slots = new EndpointInteractionSlots(); const pairings = pairingStore();
  pairings.terminate = async () => { if (failed) throw new Error("fixed-invalidation-failure"); };
  const manager = new ControllerReservationManager({ pairingStore: pairings, interactionSlots: slots,
    clock: () => wall, elapsedClock: () => elapsed });
  const key = publicKey();
  const reservation = claim(manager, { handshakePublicKeyB64: key });
  elapsed = 10000; wall = 100;
  await assert.rejects(manager.expire(), /reservation_cleanup_failed/u);
  assert.equal(manager.reservationForOwner({ reservationId: reservation.reservation_id, socketIdentity: "socket-0001" }).state, "invalidating");
  assert.equal(slots.read("endpoint-0001").owner.owner_id, reservation.reservation_id);
  assert.throws(() => claim(manager, { handshakePublicKeyB64: key }), { code: "pairing_code_invalid" });
  failed = false;
  assert.equal(await manager.expire(), 1);
  const replacement = slots.acquireWeb({ endpointId: "endpoint-0001", leaseId: "replacement-web" });
  assert.equal(await manager.expire(), 0);
  assert.deepEqual(slots.read("endpoint-0001"), replacement);
  const restarted = new ControllerReservationManager({ pairingStore: pairings, interactionSlots: slots,
    clock: () => wall, elapsedClock: () => 0 });
  assert.equal(restarted.reservationForOwner({ reservationId: reservation.reservation_id, socketIdentity: "socket-0001" }), null);
  assert.equal(await restarted.expire(), 0);
});

test("identifier contract: reservation accepts wire colon IDs without changing ownership", async () => {
  const slots = new EndpointInteractionSlots();
  const manager = new ControllerReservationManager({ pairingStore: pairingStore(), interactionSlots: slots });
  const result = claim(manager, { requestId: "claim:request:0001", socketIdentity: "socket:A" });
  assert.equal(result.owner_socket_identity, "socket:A");
  assert.equal(slots.read("endpoint-0001").owner.owner_id, result.reservation_id);
  await manager.release({ reservationId: result.reservation_id, socketIdentity: "socket:A", reason: "failed" });
  assert.equal(slots.read("endpoint-0001").owner, null);
});

test("identifier contract: shared Web slots preserve legacy leading underscore and hyphen IDs", () => {
  const slots = new EndpointInteractionSlots();
  for (const endpointId of ["_old", "-old"]) {
    slots.acquireWeb({ endpointId, leaseId: "web-lease-0001" });
    assert.equal(slots.read(endpointId).owner.kind, "web_lease");
    assert.equal(slots.releaseWeb({ endpointId, leaseId: "web-lease-0001" }), true);
    assert.equal(slots.read(endpointId).owner, null);
  }
});

test("two A claims and Web joins share one endpoint interaction slot", async () => {
  const pairings = pairingStore();
  const slots = new EndpointInteractionSlots();
  const manager = new ControllerReservationManager({ pairingStore: pairings, interactionSlots: slots, clock: () => 1000 });
  const winner = claim(manager);
  assert.equal(winner.state, "reserved");
  assert.throws(() => claim(manager, {
    requestId: "claim-request-0002", socketIdentity: "socket-0002", source: "source-0002",
  }), (error) => error?.code === "pairing_code_invalid");
  assert.throws(() => manager.acquireWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0001" }),
    (error) => error?.code === "legacy_endpoint_not_found");
  assert.equal(pairings.terminal.length, 0);

  await manager.release({
    reservationId: winner.reservation_id, socketIdentity: "socket-0001", reason: "owner_disconnected",
  });
  assert.deepEqual(pairings.terminal, ["pairing-0001"]);
  assert.equal(manager.acquireWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0001" }).owner.owner_id,
    "web-lease-0001");
  assert.equal(manager.acquireWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0002" }).owner.owner_id,
    "web-lease-0002");
});

test("an existing Web owner rejects claim without consuming the code", () => {
  const pairings = pairingStore();
  const slots = new EndpointInteractionSlots();
  slots.acquireWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0001" });
  const manager = new ControllerReservationManager({ pairingStore: pairings, interactionSlots: slots });
  assert.throws(() => claim(manager), (error) => error?.code === "controller_busy");
  assert.deepEqual(pairings.terminal, []);
  assert.equal(slots.read("endpoint-0001").owner.owner_id, "web-lease-0001");
});

test("reservation owner, reserved timeout and handshake timeout are enforced", async () => {
  let now = 1000;
  const invalidated = [];
  const manager = new ControllerReservationManager({
    pairingStore: pairingStore(), interactionSlots: new EndpointInteractionSlots(), clock: () => now,
    onPairingInvalidated: async (event) => invalidated.push(event),
  });
  const reserved = claim(manager);
  assert.throws(() => manager.beginHandshake({
    reservationId: reserved.reservation_id, socketIdentity: "socket-other",
  }), (error) => error?.code === "reservation_owner_invalid");
  now = 10_999;
  assert.equal(await manager.expire(), 0);
  now = 11_000;
  assert.equal(await manager.expire(), 1);
  assert.equal(invalidated[0].reason, "timeout");

  now = 20_000;
  const second = claim(manager, {
      requestId: "claim-request-0002", socketIdentity: "socket-0002", source: "source-0002",
    });
  now = 29_000;
  const pending = manager.beginHandshake({
    reservationId: second.reservation_id,
    socketIdentity: "socket-0002",
  });
  assert.equal(pending.state, "handshake_pending");
  assert.equal(pending.handshake_deadline_ms, "49000");
  now = 48_999;
  assert.equal(await manager.expire(), 0);
  now = 49_000;
  assert.equal(await manager.expire(), 1);
});

test("invalid, expired, revoked-looking codes and rate limits share one public error", () => {
  const pairings = pairingStore();
  const audits = [];
  const manager = new ControllerReservationManager({
    pairingStore: pairings, interactionSlots: new EndpointInteractionSlots(),
    sourceLimit: 2, connectionLimit: 2, globalLimit: 10, audit: (event) => audits.push(event),
  });
  for (const pairingCode of ["bad", "7654321", "ABCDEFG"]) {
    assert.throws(() => claim(manager, { pairingCode }), (error) => error?.code === "pairing_code_invalid");
  }
  assert.ok(audits.includes("claim_rate_limited"));
  assert.deepEqual(pairings.terminal, []);
});

test("A and Web simultaneous acquisition has exactly one CAS winner", async () => {
  const pairings = pairingStore();
  const slots = new EndpointInteractionSlots();
  const manager = new ControllerReservationManager({ pairingStore: pairings, interactionSlots: slots });
  const outcomes = await Promise.allSettled([
    Promise.resolve().then(() => claim(manager)),
    Promise.resolve().then(() => manager.acquireWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0001" })),
  ]);
  assert.equal(outcomes.filter((item) => item.status === "fulfilled").length, 1);
  assert.equal(outcomes.filter((item) => item.status === "rejected").length, 1);
  assert.ok(slots.read("endpoint-0001").owner);
});

test("claim retry is idempotent and accepts an IPv6 source identity", () => {
  const manager = new ControllerReservationManager({
    pairingStore: pairingStore(), interactionSlots: new EndpointInteractionSlots(),
    connectionLimit: 1,
  });
  const handshakePublicKeyB64 = publicKey();
  const first = claim(manager, { source: "[2001:db8::1]:443", handshakePublicKeyB64 });
  const retried = claim(manager, { source: "[2001:db8::1]:443", handshakePublicKeyB64 });
  assert.deepEqual(retried, first);
  assert.throws(() => claim(manager, {
    source: "[2001:db8::1]:443", handshakePublicKeyB64: publicKey(),
  }), (error) => error?.code === "pairing_code_invalid");
});

test("failed durable invalidation keeps the slot closed and is retryable", async () => {
  let failTermination = true;
  const invalidated = [];
  const pairings = pairingStore();
  pairings.terminate = async ({ pairingId }) => {
    if (failTermination) throw new Error("durable write failed");
    pairings.terminal.push(pairingId);
  };
  const manager = new ControllerReservationManager({
    pairingStore: pairings, interactionSlots: new EndpointInteractionSlots(),
    onPairingInvalidated: async (event) => invalidated.push(event),
  });
  const reservation = claim(manager);
  await assert.rejects(manager.release({
    reservationId: reservation.reservation_id,
    socketIdentity: "socket-0001",
    reason: "owner_disconnected",
  }), /durable write failed/u);
  assert.throws(() => manager.acquireWeb({
    endpointId: "endpoint-0001", leaseId: "web-lease-0001",
  }), (error) => error?.code === "legacy_endpoint_not_found");

  failTermination = false;
  assert.equal(await manager.expire(), 1);
  assert.deepEqual(pairings.terminal, ["pairing-0001"]);
  assert.deepEqual(invalidated, [{
    pairing_id: "pairing-0001",
    endpoint_id: "endpoint-0001",
    remote_control_generation: "generation-0001",
    reason: "owner_disconnected",
  }]);
  assert.equal(manager.acquireWeb({
    endpointId: "endpoint-0001", leaseId: "web-lease-0001",
  }).owner.owner_id, "web-lease-0001");
});

test("failed replacement-code notification also keeps invalidation retryable", async () => {
  let notificationAttempts = 0;
  const terminal = new Set();
  const manager = new ControllerReservationManager({
    pairingStore: {
      findActiveByCode: pairingStore().findActiveByCode,
      terminate: async ({ pairingId }) => terminal.add(pairingId),
    },
    interactionSlots: new EndpointInteractionSlots(),
    onPairingInvalidated: async () => {
      notificationAttempts += 1;
      if (notificationAttempts === 1) throw new Error("notification queue unavailable");
    },
  });
  const reservation = claim(manager);
  await assert.rejects(manager.release({
    reservationId: reservation.reservation_id,
    socketIdentity: "socket-0001",
    reason: "handshake_failed",
  }), /notification queue unavailable/u);
  assert.throws(() => manager.acquireWeb({
    endpointId: "endpoint-0001", leaseId: "web-lease-0001",
  }), (error) => error?.code === "legacy_endpoint_not_found");
  assert.equal(await manager.expire(), 1);
  assert.equal(notificationAttempts, 2);
  assert.deepEqual([...terminal], ["pairing-0001"]);
});

test("disconnect cleanup releases every reservation owned by one socket", async () => {
  const terminal = [];
  const active = new Map([
    ["2345678", Object.freeze({
      pairing_id: "pairing-0001", endpoint_id: "endpoint-0001",
      remote_control_generation: "generation-0001",
    })],
    ["2345679", Object.freeze({
      pairing_id: "pairing-0002", endpoint_id: "endpoint-0002",
      remote_control_generation: "generation-0002",
    })],
  ]);
  const manager = new ControllerReservationManager({
    pairingStore: {
      findActiveByCode: (code) => active.get(code) ?? null,
      terminate: async ({ pairingId }) => terminal.push(pairingId),
    },
    interactionSlots: new EndpointInteractionSlots(),
  });
  claim(manager, { handshakePublicKeyB64: publicKey() });
  claim(manager, {
    pairingCode: "2345679", requestId: "claim-request-0002",
    handshakePublicKeyB64: publicKey(),
  });
  assert.equal(await manager.releaseOwned({ socketIdentity: "socket-0001" }), 2);
  assert.deepEqual(terminal.sort(), ["pairing-0001", "pairing-0002"]);
});

test("endpoint disconnect cleanup invalidates only that endpoint reservation", async () => {
  const terminal = [];
  const manager = new ControllerReservationManager({
    pairingStore: {
      findActiveByCode: (code) => code === "2345678" ? Object.freeze({
        pairing_id: "pairing-0001", endpoint_id: "endpoint-0001",
        remote_control_generation: "generation-0001",
      }) : null,
      terminate: async ({ pairingId }) => terminal.push(pairingId),
    },
    interactionSlots: new EndpointInteractionSlots(),
  });
  claim(manager);
  assert.equal(await manager.releaseEndpoint({ endpointId: "endpoint-0002" }), 0);
  assert.equal(await manager.releaseEndpoint({ endpointId: "endpoint-0001" }), 1);
  assert.deepEqual(terminal, ["pairing-0001"]);
});

test("source bucket capacity fails closed with the public invalid-code error", () => {
  const audits = [];
  const manager = new ControllerReservationManager({
    pairingStore: pairingStore(), interactionSlots: new EndpointInteractionSlots(),
    maxSourceBuckets: 1, audit: (event) => audits.push(event),
  });
  assert.throws(() => claim(manager, {
    pairingCode: "invalid", source: "198.51.100.1",
  }), (error) => error?.code === "pairing_code_invalid");
  assert.throws(() => claim(manager, {
    pairingCode: "invalid", source: "198.51.100.2", socketIdentity: "socket-0002",
  }), (error) => error?.code === "pairing_code_invalid");
  assert.ok(audits.includes("claim_rate_limited"));
});

test("reservation ID collisions do not overwrite an existing owner", () => {
  const active = new Map([
    ["2345678", Object.freeze({
      pairing_id: "pairing-0001", endpoint_id: "endpoint-0001",
      remote_control_generation: "generation-0001",
    })],
    ["2345679", Object.freeze({
      pairing_id: "pairing-0002", endpoint_id: "endpoint-0002",
      remote_control_generation: "generation-0002",
    })],
  ]);
  const entropy = [Buffer.alloc(18, 1), Buffer.alloc(18, 1), Buffer.alloc(18, 2)];
  const slots = new EndpointInteractionSlots();
  const manager = new ControllerReservationManager({
    pairingStore: {
      findActiveByCode: (code) => active.get(code) ?? null,
      terminate: async () => {},
    },
    interactionSlots: slots,
    randomBytes: () => entropy.shift(),
  });
  const first = claim(manager);
  const second = claim(manager, {
    pairingCode: "2345679", requestId: "claim-request-0002",
    socketIdentity: "socket-0002", source: "source-0002",
  });
  assert.notEqual(second.reservation_id, first.reservation_id);
  assert.equal(slots.read("endpoint-0001").owner.owner_id, first.reservation_id);
  assert.equal(slots.read("endpoint-0002").owner.owner_id, second.reservation_id);
});

test("slot CAS rejects stale revisions and preserves Web-to-Web takeover", () => {
  const slots = new EndpointInteractionSlots();
  const first = slots.acquireWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0001" });
  const second = slots.acquireWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0002" });
  assert.equal(second.revision, "2");
  assert.equal(slots.releaseWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0001" }), false);
  assert.throws(() => slots.compareAndSwap("endpoint-0001", first.revision, null),
    (error) => error?.code === "interaction_slot_revision_conflict");
  assert.equal(slots.releaseWeb({ endpointId: "endpoint-0001", leaseId: "web-lease-0002" }), true);
});

test("a pending reservation promotes to a controller lease without terminating pairing", () => {
  const pairings = pairingStore();
  const slots = new EndpointInteractionSlots();
  const manager = new ControllerReservationManager({
    pairingStore: pairings, interactionSlots: slots, clock: () => 1000,
  });
  const reservation = claim(manager);
  manager.beginHandshake({
    reservationId: reservation.reservation_id, socketIdentity: "socket-0001",
  });
  const promoted = manager.promoteToLease({
    reservationId: reservation.reservation_id,
    socketIdentity: "socket-0001",
    controllerLeaseId: "controller-lease-0001",
  });
  assert.equal(promoted.state, "controller_connected");
  assert.equal(slots.read("endpoint-0001").owner.kind, "controller_lease");
  assert.equal(pairings.terminal.length, 0);
  assert.throws(() => manager.beginHandshake({
    reservationId: reservation.reservation_id, socketIdentity: "socket-0001",
  }), (error) => error?.code === "reservation_owner_invalid");
});
