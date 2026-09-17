import assert from "node:assert/strict";
import crypto, { createHash } from "node:crypto";
import test from "node:test";

import { ControllerLivenessHooks } from "../controller-liveness.js";
import { buildLivenessAttestationPreimage } from "../protocol-controller.js";

const lease = Object.freeze({
  connection_id: "connection-0001",
  controller_lease_id: "lease-0001",
  lease_epoch: "1",
  relay_epoch: "epoch-test-1",
  remote_control_generation: "generation-0001",
  liveness_epoch_a: "live-a-1",
  liveness_epoch_b: "live-b-1",
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
