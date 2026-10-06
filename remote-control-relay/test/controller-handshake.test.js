import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { ControllerLeaseManager } from "../controller-lease.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";
import { receiptCommitmentHex } from "../protocol-controller.js";
import { ControllerRouter } from "../controller-router.js";

const CAPABILITIES = ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1"];

function publicKey() {
  return crypto.generateKeyPairSync("x25519").publicKey.export({ type: "spki", format: "der" })
    .subarray(-32).toString("base64");
}

function fixture({ clock = () => 1000, failConsume = false, resolveControlled, signTranscript, consumeLease } = {}) {
  const pairing = Object.freeze({
    pairing_id: "pairing-0001",
    endpoint_id: "endpoint-0001",
    remote_control_generation: "generation-0001",
    handshake_public_key_b64: publicKey(),
    capabilities: Object.freeze([...CAPABILITIES]),
  });
  const consumed = [];
  const terminal = [];
  const pairings = {
    activePairing: (pairingId) => pairingId === pairing.pairing_id ? pairing : null,
    findActiveByCode: (code) => code === "2345678" ? pairing : null,
    consumeForControllerLease: async (value) => {
      if (failConsume) throw new Error("atomic commit failed");
      if (consumeLease) await consumeLease();
      consumed.push(value);
      return value;
    },
    terminate: async ({ pairingId }) => terminal.push(pairingId),
  };
  const slots = new EndpointInteractionSlots();
  const reservations = new ControllerReservationManager({
    pairingStore: pairings, interactionSlots: slots, clock,
  });
  const signing = crypto.generateKeyPairSync("ed25519");
  const manager = new ControllerLeaseManager({
    pairingStore: pairings,
    reservationManager: reservations,
    relayEpoch: "relay-epoch-0001",
    signingKeyId: "relay-signing-key-0001",
    signTranscript: (preimage) => signTranscript
      ? signTranscript(preimage, signing.privateKey) : crypto.sign(null, preimage, signing.privateKey),
    resolveControlledEndpoint: resolveControlled || (async (endpointId) => endpointId === pairing.endpoint_id
      ? { socketIdentity: "socket-B" } : null),
    clock,
  });
  return { manager, pairings, reservations, slots, consumed, terminal, pairing };
}

for (const phase of ["resolve", "sign"]) {
  test(`overlapping reservation/lease expiry cannot publish a claim during ${phase}`, async () => {
    let now = 1000;
    const entered = deferred();
    const gate = deferred();
    const { manager, reservations, slots, terminal } = fixture({ clock: () => now,
      resolveControlled: async () => {
        if (phase === "resolve") { entered.resolve(); await gate.promise; }
        return { socketIdentity: "socket-B" };
      },
      signTranscript: async (preimage, key) => {
        if (phase === "sign") { entered.resolve(); await gate.promise; }
        return crypto.sign(null, preimage, key);
      },
    });
    const settled = Promise.allSettled([start(manager)]);
    await entered.promise;
    now = 31_000;
    await Promise.all([reservations.expire(), manager.expire()]);
    gate.resolve();
    const result = (await settled)[0];
    assert.equal(result.status, "rejected");
    assert.ok(!(result.reason instanceof AggregateError));
    assert.equal(slots.read("endpoint-0001").owner, null);
    assert.deepEqual(terminal, ["pairing-0001"]);
  });
}

test("a stale first endpoint lookup cannot publish a handshake for disconnected B", async () => {
  const entered = deferred();
  const gate = deferred();
  let online = true;
  let calls = 0;
  const { manager, slots, terminal } = fixture({ resolveControlled: async () => {
    const result = online ? { socketIdentity: "socket-B" } : null;
    calls += 1;
    if (calls === 1) { entered.resolve(); await gate.promise; }
    return result;
  } });
  const settled = Promise.allSettled([start(manager)]);
  await entered.promise;
  online = false;
  await manager.abortBySocket({ socketIdentity: "socket-B" });
  gate.resolve();
  const result = (await settled)[0];
  assert.equal(result.status, "rejected");
  assert.equal(result.reason.code, "endpoint_offline");
  assert.equal(slots.read("endpoint-0001").owner, null);
  assert.deepEqual(terminal, ["pairing-0001"]);
});

test("expiry during atomic consume reports expiry, not a second-release AggregateError", async () => {
  let now = 1000;
  const entered = deferred();
  const gate = deferred();
  const { manager, reservations, slots, terminal } = fixture({ clock: () => now,
    consumeLease: async () => { entered.resolve(); await gate.promise; } });
  const reserved = await start(manager);
  const secretA = crypto.randomBytes(32);
  const secretB = crypto.randomBytes(32);
  submitCommitmentsAndFinished(manager, reserved, secretA, secretB);
  await manager.acceptReceipt({ message: receiptMessage(reserved, "A", secretA), role: "B", socketIdentity: "socket-B" });
  const settled = Promise.allSettled([manager.acceptReceipt({ message: receiptMessage(reserved, "B", secretB),
    role: "A", socketIdentity: "socket-A" })]);
  await entered.promise;
  now = 21_000;
  await Promise.all([reservations.expire(), manager.expire()]);
  gate.resolve();
  const result = (await settled)[0];
  assert.equal(result.status, "rejected");
  assert.equal(result.reason.code, "controller_handshake_expired");
  assert.equal(slots.read("endpoint-0001").owner, null);
  assert.deepEqual(terminal, ["pairing-0001"]);
});

test("router capability-conflict retry and commitment replay preserve the incumbent handshake", async () => {
  const entered = deferred();
  const gate = deferred();
  let router;
  const { manager, terminal } = fixture({ resolveControlled: (endpoint) => router.resolveControlledEndpoint(endpoint),
    signTranscript: async (preimage, key) => { entered.resolve(); await gate.promise; return crypto.sign(null, preimage, key); } });
  router = new ControllerRouter({ pairingRegistrationHandler: { register: async () => {} }, leaseManager: manager,
    send: () => true });
  for (const role of ["A", "B"]) router.addSocket({ socketIdentity: `socket-${role}`, source: `source-${role}`, socket: {} });
  router.authenticateControlled({ socketIdentity: "socket-B", endpointId: "endpoint-0001", authenticatedContext: {} });
  const message = claimMessage();
  const first = router.handle({ socketIdentity: "socket-A", message });
  await entered.promise;
  await assert.rejects(router.handle({ socketIdentity: "socket-A", message: { ...message,
    request_id: "request-conflict", capabilities: ["pairing_v1", "finished_receipt_v1"] } }),
  (error) => error.code === "controller_claim_conflict");
  gate.resolve();
  const reserved = await first;
  const secret = crypto.randomBytes(32);
  const commitment = commitmentMessage(reserved, "A", secret);
  await router.handle({ socketIdentity: "socket-A", message: commitment });
  await assert.rejects(router.handle({ socketIdentity: "socket-A", message: commitment }),
    (error) => error.code === "receipt_commitment_replayed");
  await router.handle({ socketIdentity: "socket-A", message: finishedMessage(reserved, "A") });
  assert.deepEqual(terminal, []);
});

test("failed duplicate reserved delivery has no authority to abort the original handshake", async () => {
  let router;
  let rejectRetryDelivery = false;
  const { manager, terminal } = fixture({ resolveControlled: (endpoint) => router.resolveControlledEndpoint(endpoint) });
  router = new ControllerRouter({ pairingRegistrationHandler: { register: async () => {} }, leaseManager: manager,
    send: (_socket, message) => !(rejectRetryDelivery && message.type === "controller_pairing_reserved") });
  for (const role of ["A", "B"]) router.addSocket({ socketIdentity: `socket-${role}`, source: `source-${role}`, socket: {} });
  router.authenticateControlled({ socketIdentity: "socket-B", endpointId: "endpoint-0001", authenticatedContext: {} });
  const message = claimMessage();
  const reserved = await router.handle({ socketIdentity: "socket-A", message });
  rejectRetryDelivery = true;
  // An identical transport ID is legal too; cleanup authority cannot be inferred
  // from request_id differences or from successful claim() response alone.
  await assert.rejects(router.handle({ socketIdentity: "socket-A", message }),
    (error) => error.code === "endpoint_offline");
  assert.deepEqual(terminal, []);
  rejectRetryDelivery = false;
  const secretA = crypto.randomBytes(32);
  const secretB = crypto.randomBytes(32);
  for (const [role, secret] of [["A", secretA], ["B", secretB]]) {
    await router.handle({ socketIdentity: `socket-${role}`, message: commitmentMessage(reserved, role, secret) });
    await router.handle({ socketIdentity: `socket-${role}`, message: finishedMessage(reserved, role) });
  }
  await router.handle({ socketIdentity: "socket-B", message: receiptMessage(reserved, "A", secretA) });
  const committed = await router.handle({ socketIdentity: "socket-A", message: receiptMessage(reserved, "B", secretB) });
  assert.equal(manager.lease(committed.lease.controller_lease_id), committed.lease);
});

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

for (const phase of ["resolve", "sign"]) {
  test(`concurrent stable claim retries share one preparation during ${phase}`, async () => {
    const entered = deferred();
    const gate = deferred();
    let signs = 0;
    let lookups = 0;
    const { manager, slots, terminal } = fixture({
      resolveControlled: async () => {
        lookups += 1;
        if (phase === "resolve" && lookups === 1) { entered.resolve(); await gate.promise; }
        return { socketIdentity: "socket-B" };
      },
      signTranscript: async (preimage, key) => {
        signs += 1;
        if (phase === "sign") { entered.resolve(); await gate.promise; }
        return crypto.sign(null, preimage, key);
      },
    });
    const message = claimMessage();
    const first = manager.claim({ message, socketIdentity: "socket-A", source: "source-A" });
    await entered.promise;
    const retry = manager.claim({
      message: { ...message, request_id: "request-retry" }, socketIdentity: "socket-A", source: "source-A",
    });
    // Attach rejection handlers before opening the deterministic async window.
    const results = Promise.allSettled([first, retry]);
    gate.resolve();
    const settled = await results;
    assert.deepEqual(settled.map((item) => item.status), ["fulfilled", "fulfilled"]);
    assert.equal(settled[0].value.transcript_hash, settled[1].value.transcript_hash);
    assert.equal(settled[1].value.request_id, "request-retry");
    assert.equal(signs, 1);
    assert.deepEqual(terminal, []);
    assert.equal(slots.read("endpoint-0001").owner.kind, "desktop_reservation");
  });

  test(`disconnect cancels an unpublished claim during ${phase}`, async () => {
    const entered = deferred();
    const gate = deferred();
    const { manager, slots, terminal } = fixture({
      resolveControlled: async () => {
        if (phase === "resolve") { entered.resolve(); await gate.promise; }
        return { socketIdentity: "socket-B" };
      },
      signTranscript: async (preimage, key) => {
        if (phase === "sign") { entered.resolve(); await gate.promise; }
        return crypto.sign(null, preimage, key);
      },
    });
    const operation = start(manager);
    const settled = Promise.allSettled([operation]);
    await entered.promise;
    const aborted = await manager.abortBySocket({ socketIdentity: phase === "resolve" ? "socket-A" : "socket-B" });
    gate.resolve();
    assert.equal((await settled)[0].status, "rejected");
    assert.equal(aborted.handshakes, 1);
    assert.equal(slots.read("endpoint-0001").owner, null);
    assert.deepEqual(terminal, ["pairing-0001"]);
  });
}

test("router rejects late receipts and reclaims without terminating an established real lease", async () => {
  let router;
  const { manager, slots } = fixture({ resolveControlled: (endpoint) => router.resolveControlledEndpoint(endpoint) });
  router = new ControllerRouter({
    pairingRegistrationHandler: { register: async () => {} }, leaseManager: manager, send: () => true,
  });
  for (const role of ["A", "B"]) {
    router.addSocket({ socketIdentity: `socket-${role}`, source: `source-${role}`, socket: {} });
  }
  router.authenticateControlled({ socketIdentity: "socket-B", endpointId: "endpoint-0001", authenticatedContext: {} });
  const claim = claimMessage();
  const reserved = await router.handle({ socketIdentity: "socket-A", message: claim });
  const secretA = crypto.randomBytes(32);
  const secretB = crypto.randomBytes(32);
  for (const [role, secret] of [["A", secretA], ["B", secretB]]) {
    await router.handle({ socketIdentity: `socket-${role}`, message: commitmentMessage(reserved, role, secret) });
    await router.handle({ socketIdentity: `socket-${role}`, message: finishedMessage(reserved, role) });
  }
  await router.handle({ socketIdentity: "socket-B", message: receiptMessage(reserved, "A", secretA) });
  const committed = await router.handle({ socketIdentity: "socket-A", message: receiptMessage(reserved, "B", secretB) });
  const leaseId = committed.lease.controller_lease_id;
  await assert.rejects(router.handle({ socketIdentity: "socket-A", message: receiptMessage(reserved, "B", secretB) }));
  assert.equal(manager.lease(leaseId), committed.lease);
  await assert.rejects(router.handle({ socketIdentity: "socket-A", message: claim }));
  assert.equal(manager.lease(leaseId), committed.lease);
  assert.equal(slots.read("endpoint-0001").owner.owner_id, leaseId);
  assert.equal((await router.removeSocket("socket-A")).leases, 1);
  assert.equal(manager.lease(leaseId), null);
  router.addSocket({ socketIdentity: "socket-A-new", source: "source-new", socket: {} });
  // The fixture supplies another active pairing: the still-connected B must
  // not remain blocked by a dangling ID after the old lease was terminated.
  const next = await router.handle({ socketIdentity: "socket-A-new",
    message: claimMessage({ claim_request_id: "claim-after-close" }) });
  assert.equal(next.type, "controller_pairing_reserved");
});

test("authenticated B closure cancels preparation before its first lookup returns", async () => {
  const entered = deferred();
  const gate = deferred();
  let router;
  let lookups = 0;
  const { manager, slots, terminal } = fixture({ resolveControlled: async (endpoint) => {
    const result = router.resolveControlledEndpoint(endpoint);
    if (++lookups === 1) { entered.resolve(); await gate.promise; }
    return result;
  } });
  router = new ControllerRouter({ pairingRegistrationHandler: { register: async () => {} }, leaseManager: manager, send: () => true });
  for (const role of ["A", "B"]) router.addSocket({ socketIdentity: `socket-${role}`, source: `source-${role}`, socket: {} });
  router.authenticateControlled({ socketIdentity: "socket-B", endpointId: "endpoint-0001", authenticatedContext: {} });
  const settled = Promise.allSettled([router.handle({ socketIdentity: "socket-A", message: claimMessage() })]);
  await entered.promise;
  assert.equal((await router.removeSocket("socket-B")).handshakes, 1);
  assert.equal(slots.read("endpoint-0001").owner, null);
  gate.resolve();
  assert.equal((await settled)[0].status, "rejected");
  assert.deepEqual(terminal, ["pairing-0001"]);
});

for (const delivery of ["reserved", "joined"]) {
  test(`failed ${delivery} delivery releases only its owned state and clears links`, async () => {
    let router;
    let failDelivery = true;
    const { manager, slots, terminal } = fixture({ resolveControlled: (endpoint) => router.resolveControlledEndpoint(endpoint) });
    router = new ControllerRouter({ pairingRegistrationHandler: { register: async () => {} }, leaseManager: manager,
      send: (_socket, message) => !(failDelivery && message.type === `controller_${delivery === "reserved" ? "pairing_reserved" : "joined"}`) });
    for (const role of ["A", "B"]) router.addSocket({ socketIdentity: `socket-${role}`, source: `source-${role}`, socket: {} });
    router.authenticateControlled({ socketIdentity: "socket-B", endpointId: "endpoint-0001", authenticatedContext: {} });
    if (delivery === "reserved") {
      await assert.rejects(router.handle({ socketIdentity: "socket-A", message: claimMessage() }),
        (error) => error.code === "endpoint_offline");
      assert.deepEqual(terminal, ["pairing-0001"]);
    } else {
      const reserved = await router.handle({ socketIdentity: "socket-A", message: claimMessage() });
      const secretA = crypto.randomBytes(32);
      const secretB = crypto.randomBytes(32);
      for (const [role, secret] of [["A", secretA], ["B", secretB]]) {
        await router.handle({ socketIdentity: `socket-${role}`, message: commitmentMessage(reserved, role, secret) });
        await router.handle({ socketIdentity: `socket-${role}`, message: finishedMessage(reserved, role) });
      }
      await router.handle({ socketIdentity: "socket-B", message: receiptMessage(reserved, "A", secretA) });
      await assert.rejects(router.handle({ socketIdentity: "socket-A", message: receiptMessage(reserved, "B", secretB) }),
        (error) => error.code === "endpoint_offline");
    }
    assert.equal(slots.read("endpoint-0001").owner, null);
    // The fixture supplies a fresh pairing regardless of earlier terminal state.
    // New admission succeeds only if stale peer links/lease IDs were cleared.
    failDelivery = false;
    const next = await router.handle({ socketIdentity: "socket-A", message: claimMessage({ claim_request_id: "claim-next" }) });
    assert.equal(next.type, "controller_pairing_reserved");
  });
}

function claimMessage(overrides = {}) {
  return {
    v: 1,
    type: "controller_pairing_claim",
    request_id: "request-claim-0001",
    claim_request_id: "claim-stable-0001",
    pairing_code: "2345678",
    handshake_public_key_b64: publicKey(),
    key_algorithm: "x25519",
    capabilities: [...CAPABILITIES],
    ...overrides,
  };
}

function binding(reserved, senderRole) {
  return {
    relay_epoch: reserved.relay_epoch,
    pairing_id: reserved.pairing_id,
    reservation_id: reserved.reservation_id,
    transcript_hash: reserved.transcript_hash,
    sender_role: senderRole,
  };
}

function common(reserved) {
  return {
    v: 1,
    pairing_id: reserved.pairing_id,
    reservation_id: reserved.reservation_id,
    remote_control_generation: reserved.remote_control_generation,
    relay_epoch: reserved.relay_epoch,
    transcript_hash: reserved.transcript_hash,
  };
}

function commitmentMessage(reserved, senderRole, secret) {
  return {
    ...common(reserved),
    type: "finished_receipt_commit",
    request_id: `request-commit-${senderRole}`,
    sender_role: senderRole,
    commitment: receiptCommitmentHex(binding(reserved, senderRole), secret),
  };
}

function finishedMessage(reserved, senderRole) {
  return {
    ...common(reserved),
    type: "encrypted_handshake_finished",
    request_id: `request-finished-${senderRole}`,
    sender_role: senderRole,
    ciphertext_b64: "AQID",
    ciphertext_bytes: "3",
  };
}

function receiptMessage(reserved, senderRole, secret) {
  const receiverRole = senderRole === "A" ? "B" : "A";
  return {
    ...common(reserved),
    type: "finished_receipt_ack",
    request_id: `request-receipt-${senderRole}`,
    sender_role: senderRole,
    receiver_role: receiverRole,
    receipt_secret_b64: secret.toString("base64"),
  };
}

async function start(manager) {
  return manager.claim({
    message: claimMessage(), socketIdentity: "socket-A", source: "198.51.100.1",
  });
}

function submitCommitmentsAndFinished(manager, reserved, secretA, secretB) {
  assert.equal(manager.acceptCommitment({
    message: commitmentMessage(reserved, "A", secretA), role: "A", socketIdentity: "socket-A",
  }).target_role, "B");
  assert.equal(manager.acceptCommitment({
    message: commitmentMessage(reserved, "B", secretB), role: "B", socketIdentity: "socket-B",
  }).target_role, "A");
  assert.equal(manager.forwardFinished({
    message: finishedMessage(reserved, "A"), role: "A", socketIdentity: "socket-A",
  }).target_role, "B");
  assert.equal(manager.forwardFinished({
    message: finishedMessage(reserved, "B"), role: "B", socketIdentity: "socket-B",
  }).target_role, "A");
}

test("two opposite receipts atomically consume pairing and create one controller lease", async () => {
  const { manager, slots, consumed, terminal } = fixture();
  const reserved = await start(manager);
  assert.equal(reserved.transcript.endpoint_id, "endpoint-0001");
  assert.equal(reserved.transcript.negotiated_capabilities.join(","),
    "encrypted_frames_v1,finished_receipt_v1,pairing_v1");
  const secretA = crypto.randomBytes(32);
  const secretB = crypto.randomBytes(32);
  submitCommitmentsAndFinished(manager, reserved, secretA, secretB);

  assert.equal(await manager.acceptReceipt({
    message: receiptMessage(reserved, "A", secretA), role: "B", socketIdentity: "socket-B",
  }), null);
  const committed = await manager.acceptReceipt({
    message: receiptMessage(reserved, "B", secretB), role: "A", socketIdentity: "socket-A",
  });
  assert.equal(consumed.length, 1);
  assert.equal(terminal.length, 0);
  assert.equal(committed.messages.A.controller_lease_id, committed.messages.B.controller_lease_id);
  assert.notEqual(committed.messages.A.resume_token_b64, committed.messages.B.resume_token_b64);
  assert.equal(manager.lease(committed.lease.controller_lease_id), committed.lease);
  assert.equal(slots.read("endpoint-0001").owner.kind, "controller_lease");
  assert.equal(JSON.stringify(committed.lease).includes(secretA.toString("base64")), false);
  assert.equal(JSON.stringify(committed.lease).includes(secretB.toString("base64")), false);
  assert.deepEqual(await manager.abortBySocket({ socketIdentity: "socket-A" }), {
    handshakes: 0, leases: 1,
  });
  assert.equal(slots.read("endpoint-0001").owner, null);
});

test("commitment, Finished and receipt enforce order, role, socket and replay", async () => {
  const { manager } = fixture();
  const reserved = await start(manager);
  const secretA = crypto.randomBytes(32);
  assert.throws(() => manager.forwardFinished({
    message: finishedMessage(reserved, "A"), role: "A", socketIdentity: "socket-A",
  }), (error) => error?.code === "receipt_commitment_missing");
  manager.acceptCommitment({
    message: commitmentMessage(reserved, "A", secretA), role: "A", socketIdentity: "socket-A",
  });
  assert.throws(() => manager.acceptCommitment({
    message: commitmentMessage(reserved, "A", secretA), role: "A", socketIdentity: "socket-A",
  }), (error) => error?.code === "receipt_commitment_replayed");
  assert.throws(() => manager.forwardFinished({
    message: finishedMessage(reserved, "A"), role: "A", socketIdentity: "socket-other",
  }), (error) => error?.code === "controller_handshake_socket_mismatch");
  manager.forwardFinished({
    message: finishedMessage(reserved, "A"), role: "A", socketIdentity: "socket-A",
  });
  await assert.rejects(manager.acceptReceipt({
    message: receiptMessage(reserved, "A", crypto.randomBytes(32)),
    role: "B",
    socketIdentity: "socket-B",
  }), (error) => error?.code === "finished_receipt_invalid");
});

test("stable claim retry echoes transport request and rejects changed capabilities", async () => {
  const { manager } = fixture();
  const message = claimMessage();
  const first = await manager.claim({
    message, socketIdentity: "socket-A", source: "198.51.100.1",
  });
  const retried = await manager.claim({
    message: { ...message, request_id: "request-claim-retry" },
    socketIdentity: "socket-A",
    source: "198.51.100.1",
  });
  assert.equal(retried.request_id, "request-claim-retry");
  assert.equal(retried.reservation_id, first.reservation_id);
  await assert.rejects(manager.claim({
    message: {
      ...message,
      request_id: "request-claim-conflict",
      capabilities: ["pairing_v1", "finished_receipt_v1"],
    },
    socketIdentity: "socket-A",
    source: "198.51.100.1",
  }), (error) => error?.code === "controller_claim_conflict");
});

test("atomic commit failure creates no lease and terminates the pairing", async () => {
  const { manager, slots, consumed, terminal } = fixture({ failConsume: true });
  const reserved = await start(manager);
  const secretA = crypto.randomBytes(32);
  const secretB = crypto.randomBytes(32);
  submitCommitmentsAndFinished(manager, reserved, secretA, secretB);
  await manager.acceptReceipt({
    message: receiptMessage(reserved, "A", secretA), role: "B", socketIdentity: "socket-B",
  });
  await assert.rejects(manager.acceptReceipt({
    message: receiptMessage(reserved, "B", secretB), role: "A", socketIdentity: "socket-A",
  }), /atomic commit failed/u);
  assert.equal(consumed.length, 0);
  assert.deepEqual(terminal, ["pairing-0001"]);
  assert.equal(slots.read("endpoint-0001").owner, null);
});

test("handshake timeout and either socket disconnect invalidate the pairing", async () => {
  let now = 1000;
  const first = fixture({ clock: () => now });
  await start(first.manager);
  now = 21_000;
  assert.equal(await first.manager.expire(), 1);
  assert.deepEqual(first.terminal, ["pairing-0001"]);

  const second = fixture();
  await start(second.manager);
  assert.deepEqual(await second.manager.abortBySocket({ socketIdentity: "socket-B" }), {
    handshakes: 1, leases: 0,
  });
  assert.deepEqual(second.terminal, ["pairing-0001"]);
});
