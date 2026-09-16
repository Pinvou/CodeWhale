import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { ControllerLeaseManager } from "../controller-lease.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";
import { receiptCommitmentHex } from "../protocol-controller.js";

const CAPABILITIES = ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1"];

function publicKey() {
  return crypto.generateKeyPairSync("x25519").publicKey.export({ type: "spki", format: "der" })
    .subarray(-32).toString("base64");
}

function fixture({ clock = () => 1000, failConsume = false } = {}) {
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
    signTranscript: (preimage) => crypto.sign(null, preimage, signing.privateKey),
    resolveControlledEndpoint: async (endpointId) => endpointId === pairing.endpoint_id
      ? { socketIdentity: "socket-B" } : null,
    clock,
  });
  return { manager, pairings, reservations, slots, consumed, terminal, pairing };
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
