import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

import { sealPairingCode } from "../pairing-code-sealer.js";
import { PairingStore } from "../pairing-store.js";
import { PairingRegistrationHandler } from "../pairing-registration-handler.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";
import { buildSealedCodeAad, parseSealedCodeV1 } from "../protocol-controller.js";

const X25519_SPKI_PREFIX = Buffer.from("302a300506032b656e032100", "hex");
const X25519_PKCS8_PREFIX = Buffer.from("302e020100300506032b656e04220420", "hex");
const BINDING = Object.freeze({
  registration_request_id: "registration-request-0001",
  pairing_id: "pairing-0001",
  remote_control_generation: "generation-0001",
  relay_epoch: "epoch-test-1",
  expires_at_ms: "30000",
});

class MemoryStateStore {
  revision = "0";
  value = null;
  failWrites = false;

  readNamespace() {
    return { namespace_revision: this.revision, value: structuredClone(this.value) };
  }

  async compareAndSwap(_name, expected, value) {
    await new Promise((resolve) => setImmediate(resolve));
    if (this.failWrites) throw Object.assign(new Error("persist failed"), { code: "persist_failed" });
    if (expected !== this.revision) throw Object.assign(new Error("conflict"), { code: "namespace_revision_conflict" });
    this.revision = String(Number(this.revision) + 1);
    this.value = structuredClone(value);
    return { namespace_revision: this.revision, value: structuredClone(value) };
  }
}

function registration(overrides = {}) {
  const wrap = crypto.generateKeyPairSync("x25519");
  const handshake = crypto.generateKeyPairSync("x25519");
  return {
    v: 1,
    type: "desktop_pairing_register",
    request_id: "transport-request-0001",
    registration_request_id: "registration-request-0001",
    endpoint_id: "endpoint-0001",
    remote_control_generation: "generation-0001",
    code_wrap_public_key_b64: rawPublic(wrap.publicKey).toString("base64"),
    handshake_public_key_b64: rawPublic(handshake.publicKey).toString("base64"),
    key_algorithm: "x25519",
    capabilities: ["pairing_v1"],
    ...overrides,
  };
}

test("identifier contract: pairing persists and reloads colon wire IDs without a tilde alias", async () => {
  const stateStore = new MemoryStateStore();
  const options = { stateStore, digestKeyring: keyring(), clock: () => 1000 };
  const store = await PairingStore.open(options);
  const request = registration({ registration_request_id: "registration:0001", endpoint_id: "endpoint:0001",
    remote_control_generation: "generation:0001" });
  const first = await store.register({ request, relayEpoch: "relay:epoch:0001", sealCode: sealPairingCode });
  assert.equal(first.result.type, "desktop_pairing_registered");
  const restarted = await PairingStore.open(options);
  assert.equal(restarted.activePairing(first.result.pairing_id).endpoint_id, "endpoint:0001");
  await assert.rejects(store.register({ request: registration({ endpoint_id: "endpoint~invalid" }),
    relayEpoch: "relay:epoch:0001", sealCode: sealPairingCode }));
});

function keyring(activeKeyId = "digest-key-1", includeOld = true) {
  const keys = includeOld ? [{ keyId: "digest-key-1", key: "digest-key-one-material-at-least-32-bytes" }] : [];
  if (activeKeyId !== "digest-key-1") keys.push({ keyId: activeKeyId, key: "digest-key-two-material-at-least-32-bytes" });
  return { activeKeyId, keys };
}

function deterministicEntropy(byte = 0) {
  return (size) => Buffer.alloc(size, byte);
}

function lp(value) {
  const bytes = Buffer.from(value, "utf8");
  const length = Buffer.alloc(4);
  length.writeUInt32BE(bytes.length);
  return Buffer.concat([length, bytes]);
}

function rawPublic(key) {
  return key.export({ type: "spki", format: "der" }).subarray(-32);
}

function openCode(privateKey, binding, sealed) {
  const parsed = parseSealedCodeV1(sealed);
  const relayPublic = crypto.createPublicKey({
    key: Buffer.concat([X25519_SPKI_PREFIX, parsed.relay_ephemeral_public_key]),
    format: "der",
    type: "spki",
  });
  const shared = crypto.diffieHellman({ privateKey, publicKey: relayPublic });
  const info = Buffer.concat([
    Buffer.from("pinvou-code-wrap-key-v1", "ascii"),
    ...[
      binding.registration_request_id, binding.pairing_id, binding.remote_control_generation,
      binding.relay_epoch, binding.expires_at_ms,
    ].map(lp),
  ]);
  const key = Buffer.from(crypto.hkdfSync(
    "sha256", shared, Buffer.from("pinvou-code-wrap-salt-v1", "ascii"), info, 32,
  ));
  try {
    const encrypted = parsed.ciphertext_and_tag;
    const decipher = crypto.createDecipheriv("aes-256-gcm", key, parsed.nonce);
    decipher.setAAD(buildSealedCodeAad(binding), { plaintextLength: 7 });
    decipher.setAuthTag(encrypted.subarray(-16));
    return Buffer.concat([decipher.update(encrypted.subarray(0, -16)), decipher.final()]).toString("ascii");
  } finally {
    shared.fill(0);
    key.fill(0);
  }
}

test("one-shot Relay sealer round-trips a seven-character code and changes every envelope", () => {
  const receiver = crypto.generateKeyPairSync("x25519");
  const input = {
    code: "2345678",
    codeWrapPublicKeyB64: rawPublic(receiver.publicKey).toString("base64"),
    binding: BINDING,
  };
  const first = sealPairingCode(input);
  const second = sealPairingCode(input);
  assert.notEqual(first, second);
  assert.equal(Buffer.from(first, "base64").length, 68);
  assert.equal(openCode(receiver.privateKey, BINDING, first), input.code);
  assert.equal(openCode(receiver.privateKey, BINDING, second), input.code);
});

test("sealed code rejects stale bindings, invalid codes and non-contributory peer keys", () => {
  const receiver = crypto.generateKeyPairSync("x25519");
  const publicKey = rawPublic(receiver.publicKey).toString("base64");
  const sealed = sealPairingCode({ code: "ABCDEFG", codeWrapPublicKeyB64: publicKey, binding: BINDING });
  assert.throws(() => openCode(receiver.privateKey, { ...BINDING, pairing_id: "pairing-other" }, sealed));
  assert.throws(() => sealPairingCode({ code: "1ABCDEF", codeWrapPublicKeyB64: publicKey, binding: BINDING }),
    (error) => error?.code === "pairing_code_invalid");

  const zeroPublic = Buffer.alloc(32).toString("base64");
  assert.throws(() => sealPairingCode({ code: "ABCDEFG", codeWrapPublicKeyB64: zeroPublic, binding: BINDING }),
    (error) => ["code_wrap_public_key_non_contributory", "pairing_code_seal_failed"].includes(error?.code));
});

test("production sealer owns ephemeral keys and nonce with no caller-controlled secret hooks", async () => {
  const source = await import("node:fs/promises").then(({ readFile }) => readFile(
    new URL("../pairing-code-sealer.js", import.meta.url), "utf8",
  ));
  assert.doesNotMatch(source, /nonce\s*[:=].*input|privateKey\s*[:=].*input|randomBytes\s*[:=].*input/u);
  assert.match(source, /generateKeyPairSync\("x25519"\)/u);
  assert.match(source, /crypto\.randomBytes\(12\)/u);

  const privateKey = crypto.createPrivateKey({
    key: Buffer.concat([X25519_PKCS8_PREFIX, Buffer.alloc(32, 7)]), format: "der", type: "pkcs8",
  });
  assert.equal(privateKey.asymmetricKeyType, "x25519");
});

test("pairing store persists tombstone before returning and replays a sealed result after restart", async () => {
  const stateStore = new MemoryStateStore();
  const request = registration();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(0),
  });
  const first = await store.register({ request, relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode });
  assert.equal(first.replayed, false);
  assert.equal(stateStore.value.tombstones.length, 1);
  assert.equal(stateStore.value.registrations.length, 1);
  assert.doesNotMatch(JSON.stringify(stateStore.value), /2222222/u);
  assert.equal(store.activePairing(first.result.pairing_id).endpoint_id, request.endpoint_id);
  assert.equal(store.findActiveByCode("2222222").pairing_id, first.result.pairing_id);
  assert.equal(store.findActiveByCode("2222223"), null);
  assert.equal(store.findActiveByCode("222222O"), null);

  const restarted = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 2000, randomBytes: deterministicEntropy(1),
  });
  assert.equal(restarted.activePairing(first.result.pairing_id).remote_control_generation,
    request.remote_control_generation);
  const retry = await restarted.register({
    request: { ...request, request_id: "transport-request-retry" },
    relayEpoch: "relay-epoch-0001",
    sealCode: sealPairingCode,
  });
  assert.equal(retry.replayed, true);
  assert.equal(retry.result.request_id, "transport-request-retry");
  assert.equal(retry.result.sealed_code_b64, first.result.sealed_code_b64);
  await assert.rejects(restarted.register({
    request: { ...request, request_id: "transport-conflict", endpoint_id: "endpoint-other" },
    relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  }), (error) => error?.code === "pairing_registration_conflict");
});

test("pairing store seals collision exhaustion and checks every retained digest key", async () => {
  const stateStore = new MemoryStateStore();
  const firstStore = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(0),
  });
  await firstStore.register({ request: registration(), relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode });

  const rotated = await PairingStore.open({
    stateStore, digestKeyring: keyring("digest-key-2"), clock: () => 2000,
    randomBytes: deterministicEntropy(0),
  });
  const failedRequest = registration({
    request_id: "transport-request-0002",
    registration_request_id: "registration-request-0002",
    remote_control_generation: "generation-0002",
  });
  const failed = await rotated.register({
    request: failedRequest, relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  assert.equal(failed.result.code, "pairing_code_generation_failed");
  assert.equal(stateStore.value.tombstones.length, 1);
  const retry = await rotated.register({
    request: { ...failedRequest, request_id: "transport-request-0002-retry" },
    relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  assert.equal(retry.replayed, true);
  assert.equal(retry.result.code, "pairing_code_generation_failed");

  await assert.rejects(PairingStore.open({
    stateStore, digestKeyring: keyring("digest-key-2", false), clock: () => 2000,
  }), (error) => error?.code === "pairing_digest_key_unavailable");
  const oldPairingId = stateStore.value.tombstones[0].pairing_id;
  const compacted = await PairingStore.open({
    stateStore, digestKeyring: keyring("digest-key-2", false), clock: () => 700_000,
    randomBytes: (size) => Buffer.alloc(size, size === 16 ? 0 : 1),
  });
  assert.equal(compacted.activePairing(oldPairingId), null);
  assert.equal(compacted.findActiveByCode("2222222"), null);
  assert.equal(stateStore.value.tombstones.length, 0);
  assert.equal(stateStore.value.registrations.length, 0);
  const reused = await compacted.register({
    request: registration({
      request_id: "transport-request-0003",
      registration_request_id: "registration-request-0003",
      remote_control_generation: "generation-0003",
    }),
    relayEpoch: "relay-epoch-0001",
    sealCode: sealPairingCode,
  });
  assert.notEqual(reused.result.pairing_id, oldPairingId);
  assert.equal(compacted.findActiveByCode("2222222").pairing_id, reused.result.pairing_id);
  assert.equal(stateStore.value.tombstones.length, 1);
});

test("pairing store never returns a code when the durable CAS fails", async () => {
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(3),
  });
  stateStore.failWrites = true;
  await assert.rejects(store.register({
    request: registration(), relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  }), (error) => error?.code === "pairing_persist_failed");
  assert.equal(store.activePairing("pairing-anything"), null);
});

test("a running store collects expired records and never reissues an ID when entropy repeats", async () => {
  let now = 1000;
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => now, randomBytes: deterministicEntropy(0),
  });
  const pairingIds = new Set();
  for (let cycle = 0; cycle < 5; cycle += 1) {
    const result = await store.register({
      request: registration({
        request_id: `transport-cycle-${cycle}`,
        registration_request_id: `registration-cycle-${cycle}`,
        remote_control_generation: `generation-cycle-${cycle}`,
      }),
      relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
    });
    assert.equal(pairingIds.has(result.result.pairing_id), false);
    pairingIds.add(result.result.pairing_id);
    assert.equal(stateStore.value.tombstones.length, 1);
    assert.equal(stateStore.value.registrations.length, 1);
    assert.equal(store.findActiveByCode("2222222").pairing_id, result.result.pairing_id);
    now += 700_000;
  }
});

test("GC retains an invalidating reservation until failed notification is retried", async () => {
  let now = 1000;
  let entropyByte = 0;
  let notifications = 0;
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => now,
    randomBytes: (size) => Buffer.alloc(size, entropyByte),
  });
  const first = await store.register({
    request: registration(), relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  const slots = new EndpointInteractionSlots();
  const manager = new ControllerReservationManager({
    pairingStore: store, interactionSlots: slots, clock: () => now,
    onPairingInvalidated: async () => {
      notifications += 1;
      if (notifications === 1) throw new Error("notification unavailable");
    },
  });
  const reservation = manager.claim({
    pairingCode: "2222222", requestId: "claim-gc-0001", socketIdentity: "socket-gc-0001",
    source: "source-gc-0001",
    handshakePublicKeyB64: rawPublic(crypto.generateKeyPairSync("x25519").publicKey).toString("base64"),
  });
  await assert.rejects(manager.release({
    reservationId: reservation.reservation_id, socketIdentity: "socket-gc-0001",
  }), /notification unavailable/u);
  now = 700_000;
  entropyByte = 1;
  await store.register({
    request: registration({
      request_id: "transport-gc-0002", registration_request_id: "registration-gc-0002",
      endpoint_id: "endpoint-gc-0002", remote_control_generation: "generation-gc-0002",
    }),
    relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  assert.equal(stateStore.value.tombstones.some((row) => row.pairing_id === first.result.pairing_id), true);
  assert.equal(await manager.expire(), 1);
  assert.equal(slots.read("endpoint-0001").owner, null);
  entropyByte = 2;
  await store.register({
    request: registration({
      request_id: "transport-gc-0003", registration_request_id: "registration-gc-0003",
      endpoint_id: "endpoint-gc-0003", remote_control_generation: "generation-gc-0003",
    }),
    relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  assert.equal(stateStore.value.tombstones.some((row) => row.pairing_id === first.result.pairing_id), false);
  assert.equal(notifications, 2);
});

test("concurrent registration handlers converge through the global namespace CAS", async () => {
  const stateStore = new MemoryStateStore();
  const left = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000 });
  const right = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000 });
  const request = registration();
  const [first, second] = await Promise.all([
    left.register({ request, relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode }),
    right.register({
      request: { ...request, request_id: "transport-request-concurrent" },
      relayEpoch: "relay-epoch-0001",
      sealCode: sealPairingCode,
    }),
  ]);
  assert.equal(stateStore.value.tombstones.length, 1);
  assert.equal(stateStore.value.registrations.length, 1);
  assert.equal(first.result.sealed_code_b64, second.result.sealed_code_b64);
  assert.equal([first.replayed, second.replayed].filter((value) => value === false).length, 1);
  assert.equal([first.replayed, second.replayed].filter((value) => value === true).length, 1);
});

test("registration handler authorizes B before allocation and returns a strict idempotent result", async () => {
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(4),
  });
  const authorized = [];
  const handler = new PairingRegistrationHandler({
    pairingStore: store,
    relayEpoch: "relay-epoch-0001",
    authorizeEndpoint: async (input) => {
      authorized.push(input);
      if (input.context !== "trusted-socket") throw new Error("untrusted");
    },
  });
  const request = registration();
  const first = await handler.register(request, "trusted-socket");
  assert.equal(first.type, "desktop_pairing_registered");
  const retry = await handler.register({ ...request, request_id: "transport-request-retry" }, "trusted-socket");
  assert.equal(retry.request_id, "transport-request-retry");
  assert.equal(retry.sealed_code_b64, first.sealed_code_b64);
  assert.equal(authorized.length, 2);
  await assert.rejects(handler.register({ ...request, request_id: "transport-untrusted" }, "bad-socket"),
    (error) => error?.code === "pairing_endpoint_unauthorized");
  const terminal = await store.terminate({ pairingId: first.pairing_id });
  assert.equal(terminal.terminal_at_ms, "1000");
  assert.equal(store.activePairing(first.pairing_id), null);
});

test("controller lease consumption has one durable winner and never revives pairing", async () => {
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(5),
  });
  const request = registration();
  const allocated = await store.register({
    request, relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  const common = {
    pairingId: allocated.result.pairing_id,
    endpointId: request.endpoint_id,
    remoteControlGeneration: request.remote_control_generation,
    reservationId: "reservation-0001",
  };
  const outcomes = await Promise.allSettled([
    store.consumeForControllerLease({ ...common, controllerLeaseId: "controller-lease-0001" }),
    store.consumeForControllerLease({ ...common, controllerLeaseId: "controller-lease-0002" }),
  ]);
  assert.equal(outcomes.filter((result) => result.status === "fulfilled").length, 1);
  assert.equal(outcomes.filter((result) => result.status === "rejected"
    && result.reason?.code === "pairing_not_active").length, 1);
  assert.equal(stateStore.value.tombstones[0].terminal_at_ms, "1000");
  assert.equal(store.activePairing(allocated.result.pairing_id), null);

  const restarted = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 2000,
  });
  assert.equal(restarted.activePairing(allocated.result.pairing_id), null);
  await assert.rejects(restarted.consumeForControllerLease({
    ...common, controllerLeaseId: "controller-lease-0003",
  }), (error) => error?.code === "pairing_not_active");
});

test("failed controller lease consumption keeps pairing active", async () => {
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(6),
  });
  const request = registration();
  const allocated = await store.register({
    request, relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  stateStore.failWrites = true;
  await assert.rejects(store.consumeForControllerLease({
    pairingId: allocated.result.pairing_id,
    endpointId: request.endpoint_id,
    remoteControlGeneration: request.remote_control_generation,
    reservationId: "reservation-0001",
    controllerLeaseId: "controller-lease-0001",
  }), (error) => error?.code === "pairing_persist_failed");
  assert.equal(store.activePairing(allocated.result.pairing_id).endpoint_id, request.endpoint_id);
  assert.equal(stateStore.value.tombstones[0].terminal_at_ms, null);
});

test("Relay restart invalidation terminates old codes and rejects old-epoch registration replay", async () => {
  const stateStore = new MemoryStateStore();
  const request = registration();
  const first = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(7),
  });
  const allocated = await first.register({
    request, relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
  });
  const restarted = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 2000,
  });
  assert.equal(await restarted.invalidateForRelayRestart(), 1);
  assert.equal(restarted.activePairing(allocated.result.pairing_id), null);
  assert.equal(stateStore.value.tombstones[0].terminal_at_ms, "2000");
  await assert.rejects(restarted.register({
    request: { ...request, request_id: "transport-request-after-restart" },
    relayEpoch: "relay-epoch-0002",
    sealCode: sealPairingCode,
  }), (error) => error?.code === "pairing_registration_stale_relay_epoch");
});
