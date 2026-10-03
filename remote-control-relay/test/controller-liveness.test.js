import assert from "node:assert/strict";
import crypto, { createHash } from "node:crypto";
import test from "node:test";
import { readFileSync } from "node:fs";

import { ControllerLivenessHooks } from "../controller-liveness.js";
import { buildLivenessAttestationPreimage, buildPeerLivenessAttestationPreimage } from "../protocol-controller.js";

const lease = Object.freeze({
  connection_id: "connection-0001",
  controller_lease_id: "lease-0001",
  lease_epoch: "1",
  relay_epoch: "epoch-test-1",
  remote_control_generation: "generation-0001",
  liveness_epoch_a: "live-a-1",
  liveness_epoch_b: "live-b-1",
  negotiated_capabilities: Object.freeze(["role_liveness_v1"]),
  sockets: Object.freeze({ A: "socket-A", B: "socket-B" }),
});

function heartbeat(role, sequence) {
  return {
    v: 1,
    type: "controller_liveness",
    connection_id: lease.connection_id,
    controller_lease_id: lease.controller_lease_id,
    lease_epoch: lease.lease_epoch,
    relay_epoch: lease.relay_epoch,
    remote_control_generation: lease.remote_control_generation,
    sender_role: role,
    liveness_epoch: role === "A" ? lease.liveness_epoch_a : lease.liveness_epoch_b,
    heartbeat_sequence: String(sequence),
  };
}

test("Rust B heartbeat contract keeps an older-than-30s lease resumable without B renewing A", async () => {
  const rustFrame = JSON.parse(readFileSync(new URL(
    "../../pinvou3-app/tests/fixtures/controller_role_b_heartbeat.json", import.meta.url,
  ), "utf8"));
  const value = await started();
  value.now = 29_000;
  await value.hooks.handle({
    message: rustFrame, socket_identity: "socket-B", role: "B", lease,
  });
  value.now = 31_000;
  assert.equal(value.hooks.resumeDeadline(lease), null, "B alone cannot extend A's cutoff");
  await value.hooks.handle({
    message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease,
  });
  assert.equal(value.hooks.resumeDeadline(lease), 59_000, "both roles keep the established lease eligible");
  await assert.rejects(() => value.hooks.handle({
    message: rustFrame, socket_identity: "socket-B", role: "B", lease,
  }), /controller_liveness_sequence_replay/u);
  value.now = 59_000;
  assert.equal(value.hooks.resumeDeadline(lease), null, "deadline remains finite");
});

function challenge(requestId = "request-0001") {
  return {
    v: 1,
    type: "controller_liveness_challenge_a",
    request_id: requestId,
    connection_id: lease.connection_id,
    controller_lease_id: lease.controller_lease_id,
    lease_epoch: lease.lease_epoch,
    relay_epoch: lease.relay_epoch,
    remote_control_generation: lease.remote_control_generation,
    challenge_id: "challenge-0001",
    challenge_b64: "AAAAAAAAAAAAAAAAAAAAAA==",
  };
}

function encryptedLiveness() {
  return {
    v: 1,
    type: "encrypted_controller_liveness_a",
    connection_id: lease.connection_id,
    controller_lease_id: lease.controller_lease_id,
    lease_epoch: lease.lease_epoch,
    ciphertext_b64: "AQID",
    ciphertext_bytes: "3",
  };
}

function peerRequest(id = "challenge-peer-1") {
  return { ...challenge("request-peer-1"), type: "controller_liveness_attestation_request_b", challenge_id: id };
}

test("B proof requests fail closed when role liveness was not negotiated", async () => {
  const hooks = new ControllerLivenessHooks({ clock: () => 1_000 });
  const unavailable = { ...lease, negotiated_capabilities: [] };
  await hooks.onLeaseCreated(unavailable);
  await assert.rejects(hooks.handle({ message: peerRequest(), role: "A", socket_identity: "socket-A", lease: unavailable }), { code: "unsupported_capability" });
});

test("A cannot obtain B proof from its own activity, wrong role or old B socket", async () => {
  const value = await started();
  await value.hooks.handle({ message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease });
  const send = (message = peerRequest(), role = "A", socket = "socket-A", context = lease) => value.hooks.handle({message, role, socket_identity: socket, lease: context});
  await assert.rejects(() => send(), /controller_liveness_attestation_stale/u);
  await value.hooks.handle({ message: heartbeat("B", 1), socket_identity: "socket-B", role: "B", lease });
  await assert.rejects(() => send(peerRequest(), "B", "socket-B"), /role/u);
  await assert.rejects(() => send(peerRequest(), "A", "socket-old"), /controller_socket_invalid/u);
  for (const field of ["connection_id", "controller_lease_id", "relay_epoch", "remote_control_generation"]) {
    await assert.rejects(() => send({ ...peerRequest(), [field]: "wrong-context" }));
  }
  value.now = 5_001;
  await value.hooks.handle({ message: heartbeat("A", 2), socket_identity: "socket-A", role: "A", lease });
  await assert.rejects(() => send(), /controller_liveness_attestation_stale/u);
});

test("B proof signs every envelope field in a distinct domain and consumes its challenge ID", async () => {
  const value = await started();
  await value.hooks.handle({ message: heartbeat("B", 3), socket_identity: "socket-B", role: "B", lease });
  value.now = 2_000;
  const result = await value.hooks.handle({ message: peerRequest(), socket_identity: "socket-A", role: "A", lease });
  const proof = result.deliveries[0].message;
  assert.equal(result.deliveries[0].role, "A");
  assert.equal(proof.heartbeat_sequence_b, "3");
  assert.equal(proof.attestation_sequence, "1");
  assert.equal(proof.observed_age_ms, "2000");
  assert.equal(proof.remaining_ttl_ms, "28000");
  const signature = Buffer.from(proof.relay_signature_b64, "base64");
  assert.equal(crypto.verify(null, buildPeerLivenessAttestationPreimage(proof), value.publicKey, signature), true);
  for (const field of Object.keys(proof).filter((name) => name !== "relay_signature_b64")) {
    const tampered = { ...proof, [field]: field === "v" ? 2 : `${proof[field]}x` };
    if (field === "type") assert.throws(() => buildPeerLivenessAttestationPreimage(tampered));
    else assert.equal(crypto.verify(null, buildPeerLivenessAttestationPreimage(tampered), value.publicKey, signature), false, field);
  }
  await assert.rejects(() => value.hooks.handle({ message: {...peerRequest(), challenge_b64: "AQEBAQEBAQEBAQEBAQEBAQ=="}, socket_identity: "socket-A", role: "A", lease }), /controller_liveness_challenge_replay/u);
  await value.hooks.onSocketClosed({ lease });
  await assert.rejects(() => value.hooks.handle({ message: peerRequest("challenge-peer-2"), socket_identity: "socket-A", role: "A", lease }), /controller_liveness_state_not_found/u);
});

test("B proof history is bounded, pruned and resets only with a new authenticated cycle", async () => {
  const value = await started();
  await value.hooks.handle({ message: heartbeat("B", 1), socket_identity: "socket-B", role: "B", lease });
  for (let i = 0; i < 64; i++) {
    const result = await value.hooks.handle({message: peerRequest(`challenge-peer-${i}`), role: "A", socket_identity: "socket-A", lease});
    assert.equal(result.deliveries[0].message.attestation_sequence, String(i + 1));
    assert.equal(result.deliveries[0].message.heartbeat_sequence_b, "1");
  }
  await assert.rejects(() => value.hooks.handle({message: peerRequest("challenge-over-capacity"), role: "A", socket_identity: "socket-A", lease}), /controller_liveness_challenge_capacity/u);
  value.now = 30_001;
  await value.hooks.handle({ message: heartbeat("B", 2), socket_identity: "socket-B", role: "B", lease });
  const later = await value.hooks.handle({message: peerRequest("challenge-fresh"), role: "A", socket_identity: "socket-A", lease});
  assert.equal(later.deliveries[0].message.attestation_sequence, "65");
  assert.equal(later.deliveries[0].message.heartbeat_sequence_b, "2");
  const rotated = {...lease, liveness_epoch_b: "live-b-2", sockets: {A: "socket-new-A", B: "socket-new-B"}};
  await value.hooks.onLeaseCreated(rotated);
  await assert.rejects(() => value.hooks.handle({message: heartbeat("B", 3), role: "B", socket_identity: "socket-B", lease: rotated}), /controller_socket_invalid/u);
  await assert.rejects(() => value.hooks.handle({message: peerRequest("challenge-rotated"), role: "A", socket_identity: "socket-new-A", lease: rotated}), /controller_liveness_attestation_stale/u);
});

test("unconfigured signer refuses B proof even when B heartbeat is fresh", async () => {
  const hooks = new ControllerLivenessHooks({ clock: () => 0 });
  await hooks.onLeaseCreated(lease);
  await hooks.handle({message: heartbeat("B", 1), role: "B", socket_identity: "socket-B", lease});
  await assert.rejects(() => hooks.handle({message: peerRequest(), role: "A", socket_identity: "socket-A", lease}), /controller_liveness_attestation_unavailable/u);
});

function fixture({ now = 0 } = {}) {
  const { privateKey, publicKey } = crypto.generateKeyPairSync("ed25519");
  const hooks = new ControllerLivenessHooks({
    clock: () => now,
    signingKeyId: "relay-key-test-1",
    signingPrivateKey: privateKey,
  });
  return { hooks, privateKey, publicKey, set now(value) { now = value; } };
}

async function started({ now = 0 } = {}) {
  const value = fixture({ now });
  await value.hooks.onLeaseCreated(lease);
  return value;
}

test("transport heartbeats are role isolated and only strict sequences advance", async () => {
  const value = await started();
  const a = await value.hooks.handle({
    message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease,
  });
  assert.deepEqual(a.deliveries.map((item) => item.role), ["B"]);

  value.now = 5_000;
  const b = await value.hooks.handle({
    message: heartbeat("B", 9), socket_identity: "socket-B", role: "B", lease,
  });
  assert.deepEqual(b.deliveries.map((item) => item.role), ["A"]);

  await assert.rejects(() => value.hooks.handle({
    message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease,
  }), /controller_liveness_sequence_replay/u);
  await assert.rejects(() => value.hooks.handle({
    message: heartbeat("A", 2), socket_identity: "socket-B", role: "A", lease,
  }), /controller_socket_invalid/u);
  await assert.rejects(() => value.hooks.handle({
    message: { ...heartbeat("A", 2), liveness_epoch: lease.liveness_epoch_b },
    socket_identity: "socket-A", role: "A", lease,
  }), /controller_liveness_epoch_invalid/u);
});

test("encrypted direct liveness is blind forwarded and cannot extend Relay role records", async () => {
  const value = await started();
  const message = encryptedLiveness();
  const result = await value.hooks.handle({
    message, socket_identity: "socket-A", role: "A", lease,
  });
  assert.deepEqual(result.deliveries, [{ role: "B", message }]);
  await assert.rejects(() => value.hooks.handle({
    message: { ...message, controller_lease_id: "lease-other" },
    socket_identity: "socket-A", role: "A", lease,
  }), /controller_lease_not_found/u);
});

test("Relay attestation is signed, fresh, challenge-bound and single-use", async () => {
  const value = await started();
  await value.hooks.handle({
    message: heartbeat("A", 2), socket_identity: "socket-A", role: "A", lease,
  });
  value.now = 3_000;
  await value.hooks.handle({
    message: challenge(), socket_identity: "socket-B", role: "B", lease,
  });
  const result = await value.hooks.handle({
    message: { ...challenge("request-0002"), type: "controller_liveness_attestation_request_a" },
    socket_identity: "socket-B", role: "B", lease,
  });
  const attestation = result.deliveries[0].message;
  assert.equal(result.deliveries[0].role, "B");
  assert.equal(attestation.type, "controller_liveness_attestation_a");
  assert.equal(attestation.liveness_epoch_a, lease.liveness_epoch_a);
  assert.equal(attestation.heartbeat_sequence_a, "2");
  assert.equal(attestation.attestation_sequence, "1");
  assert.equal(attestation.observed_age_ms, "3000");
  assert.equal(attestation.remaining_ttl_ms, "27000");
  assert.equal(attestation.issued_at_relay, "3000");
  assert.equal(attestation.relay_signing_key_id, "relay-key-test-1");
  const preimage = buildLivenessAttestationPreimage(attestation);
  assert.equal(crypto.verify(null, preimage, value.publicKey, Buffer.from(attestation.relay_signature_b64, "base64")), true);
  assert.equal(createHash("sha256").update(preimage).digest("hex").length, 64);

  await assert.rejects(() => value.hooks.handle({
    message: { ...challenge("request-0003"), type: "controller_liveness_attestation_request_a" },
    socket_identity: "socket-B", role: "B", lease,
  }), /controller_liveness_challenge_not_found/u);
});

test("B activity never refreshes the A observation used by attestation", async () => {
  const value = await started();
  await value.hooks.handle({
    message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease,
  });
  value.now = 5_000;
  await value.hooks.handle({
    message: challenge(), socket_identity: "socket-B", role: "B", lease,
  });
  value.now = 6_000;
  await value.hooks.handle({
    message: heartbeat("B", 100), socket_identity: "socket-B", role: "B", lease,
  });
  await assert.rejects(() => value.hooks.handle({
    message: { ...challenge("request-0002"), type: "controller_liveness_attestation_request_a" },
    socket_identity: "socket-B", role: "B", lease,
  }), /controller_liveness_attestation_stale/u);
});

test("long-lived attestation survives more than 64 cycles without forgetting recent replays", async () => {
  const value = await started();
  for (let sequence = 1; sequence <= 128; sequence += 1) {
    value.now = sequence * 5_000;
    await value.hooks.handle({
      message: heartbeat("A", sequence), socket_identity: "socket-A", role: "A", lease,
    });
    const message = {
      ...challenge(`request-${sequence}`), challenge_id: `challenge-${sequence}`,
    };
    await value.hooks.handle({ message, socket_identity: "socket-B", role: "B", lease });
    const result = await value.hooks.handle({
      message: { ...message, type: "controller_liveness_attestation_request_a" },
      socket_identity: "socket-B", role: "B", lease,
    });
    const proof = result.deliveries[0].message;
    assert.equal(proof.attestation_sequence, String(sequence));
    assert.equal(crypto.verify(null, buildLivenessAttestationPreimage(proof), value.publicKey,
      Buffer.from(proof.relay_signature_b64, "base64")), true);
    await assert.rejects(() => value.hooks.handle({
      message: { ...message, request_id: `replay-${sequence}` },
      socket_identity: "socket-B", role: "B", lease,
    }), /controller_liveness_challenge_replay/u);
    assert.equal(await value.hooks.expire(), 0);
  }
  await assert.rejects(() => value.hooks.handle({
    message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease,
  }), /controller_liveness_sequence_replay/u);
  value.now = 129 * 5_000;
  await value.hooks.handle({
    message: heartbeat("A", 129), socket_identity: "socket-A", role: "A", lease,
  });
  const reusedPair = { ...challenge("fresh-request"), challenge_id: "challenge-1" };
  await value.hooks.handle({ message: reusedPair, socket_identity: "socket-B", role: "B", lease });
  const freshProof = await value.hooks.handle({
    message: { ...reusedPair, type: "controller_liveness_attestation_request_a" },
    socket_identity: "socket-B", role: "B", lease,
  });
  assert.equal(freshProof.deliveries[0].message.attestation_sequence, "129");
});

test("expired pending challenges cannot be attested even without a heartbeat cleanup tick", async () => {
  const value = await started();
  await value.hooks.handle({ message: challenge(), socket_identity: "socket-B", role: "B", lease });
  value.now = 30_001;
  await value.hooks.handle({
    message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease,
  });
  await assert.rejects(() => value.hooks.handle({
    message: { ...challenge("request-expired"), type: "controller_liveness_attestation_request_a" },
    socket_identity: "socket-B", role: "B", lease,
  }), /controller_liveness_challenge_not_found/u);
});

test("attestation fails closed without a configured signing key", async () => {
  let now = 0;
  const hooks = new ControllerLivenessHooks({ clock: () => now });
  await hooks.onLeaseCreated(lease);
  await hooks.handle({ message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease });
  now = 1_000;
  await hooks.handle({ message: challenge(), socket_identity: "socket-B", role: "B", lease });
  await assert.rejects(() => hooks.handle({
    message: { ...challenge("request-0002"), type: "controller_liveness_attestation_request_a" },
    socket_identity: "socket-B", role: "B", lease,
  }), /controller_liveness_attestation_unavailable/u);
});

test("socket closure and expiry clean bounded hook state without retaining a lease", async () => {
  const value = await started();
  assert.deepEqual(await value.hooks.onSocketClosed({
    socket_identity: "socket-A", role: "A", lease,
  }), { retain_lease: false });
  await assert.rejects(() => value.hooks.handle({
    message: heartbeat("A", 1), socket_identity: "socket-A", role: "A", lease,
  }), /controller_liveness_state_not_found/u);

  const second = await started();
  second.now = 30_000;
  assert.equal(await second.hooks.expire(), 1);
  assert.equal(await second.hooks.expire(), 0);
});
