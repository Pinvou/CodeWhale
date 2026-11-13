import assert from "node:assert/strict";
import crypto from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { sealPairingCode } from "../pairing-code-sealer.js";
import { PairingStore, DEFAULT_PAIRING_REGISTRATION_LIMITS } from "../pairing-store.js";
import { PairingRegistrationHandler } from "../pairing-registration-handler.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "../controller-reservation.js";
import { buildSealedCodeAad, parseSealedCodeV1, validateControllerMessage } from "../protocol-controller.js";

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

test("pairing HMAC configuration rejects known example placeholders before reading or writing state", async () => {
  for (const key of ["replace-with-at-least-32-random-bytes",
    "replace-with-at-least-32-independent-random-bytes", "", "a".repeat(31), "a".repeat(4097)]) {
    const store = new MemoryStateStore();
    await assert.rejects(PairingStore.open({ stateStore: store,
      digestKeyring: { activeKeyId: "digest-key-1", keys: [{ keyId: "digest-key-1", key }] },
    }), { code: "pairing_keyring_invalid" });
    assert.equal(store.revision, "0");
    assert.equal(store.value, null);
  }
  for (const key of ["a".repeat(32), "a".repeat(4096), "界".repeat(11)]) {
    await PairingStore.open({ stateStore: new MemoryStateStore(),
      digestKeyring: { activeKeyId: "digest-key-1", keys: [{ keyId: "digest-key-1", key }] },
    });
  }
});

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

test("exact reservation release preserves another retained pairing through GC", async () => {
  let now = 1000;
  let entropyByte = 0;
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => now,
    randomBytes: size => Buffer.alloc(size, entropyByte) });
  const make = async cycle => (await store.register({ request: registration({
    request_id: `release-transport-${cycle}`, registration_request_id: `release-registration-${cycle}`,
    remote_control_generation: `release-generation-${cycle}`,
  }), relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode })).result.pairing_id;
  const first = await make(0);
  store.retainForReservation(first);
  now = 700_000;
  entropyByte = 1;
  const second = await make(1);
  assert.equal(stateStore.value.tombstones.some(row => row.pairing_id === first), true);
  store.retainForReservation(second);
  const before = JSON.stringify(stateStore.value);
  assert.throws(() => store.releaseReservation({ toString: () => second }), { code: "pairing_id_invalid" });
  store.releaseReservation("pairing:unknown");
  store.releaseReservation(first);
  store.releaseReservation(first);
  assert.equal(JSON.stringify(stateStore.value), before, "release itself does not erase durable evidence");
  now = 1_400_000;
  entropyByte = 2;
  await make(2);
  assert.equal(stateStore.value.tombstones.some(row => row.pairing_id === first), false);
  assert.equal(stateStore.value.tombstones.some(row => row.pairing_id === second), true);
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

// DEV-03 审核修复：验收要求「代码库不存在 B/A 生成路径；码明文不进日志或遥测」，
// 但原测试只断言持久化状态里没有明文。这里用真实解封得到的明文做日志/信道扫描。
test("the plaintext pairing code never reaches logs, telemetry or wire projections", async () => {
  const receiver = crypto.generateKeyPairSync("x25519");
  const request = registration({
    code_wrap_public_key_b64: rawPublic(receiver.publicKey).toString("base64"),
  });
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({
    stateStore, digestKeyring: keyring(), clock: () => 1000, randomBytes: deterministicEntropy(0),
  });

  const captured = [];
  const originals = { log: console.log, info: console.info, warn: console.warn, error: console.error };
  for (const key of Object.keys(originals)) {
    console[key] = (...args) => { captured.push(args.map(String).join(" ")); };
  }
  let registered;
  try {
    registered = await store.register({
      request, relayEpoch: "relay-epoch-0001", sealCode: sealPairingCode,
    });
  } finally {
    Object.assign(console, originals);
  }

  const binding = {
    registration_request_id: request.registration_request_id,
    pairing_id: registered.result.pairing_id,
    remote_control_generation: request.remote_control_generation,
    relay_epoch: "relay-epoch-0001",
    expires_at_ms: registered.result.expires_at_ms,
  };
  const plaintext = openCode(receiver.privateKey, binding, registered.result.sealed_code_b64);
  assert.match(plaintext, /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u);

  // 1) the wire projection carries only the sealed envelope.
  assert.equal(registered.result.code, undefined);
  assert.ok(!JSON.stringify(registered.result).includes(plaintext));
  // 2) a real registration logs nothing that contains the code.
  assert.ok(!captured.join("\n").includes(plaintext), "the plaintext code must never be logged");
  // 3) persisted state only keeps digests.
  assert.ok(!JSON.stringify(stateStore.value).includes(plaintext));
  // 4) the pairing modules have no logging surface to leak through at all.
  for (const file of ["pairing-store.js", "pairing-registration-handler.js", "pairing-code-sealer.js"]) {
    const source = await readFile(new URL(`../${file}`, import.meta.url), "utf8");
    assert.doesNotMatch(
      source,
      /console\.|logger|process\.stdout|process\.stderr/u,
      `${file} must stay free of logging calls`,
    );
  }
  // 5) the client validates codes and never generates them (no fallback path).
  const controlled = await readFile(
    new URL("../../pinvou3-app/src-tauri/src/features/remote_control/controller/controlled.rs", import.meta.url),
    "utf8",
  );
  assert.match(controlled, /fn validate_pairing_code/u);
  assert.doesNotMatch(controlled, /fn \w*pairing_code\w*generate|fn \w*generate\w*pairing_code/u);
  assert.doesNotMatch(controlled, /rand::|thread_rng|OsRng/u);
});

function smallRegistrationBudget(overrides = {}) {
  return { maxPerEndpointRecords: 2, maxGlobalRecords: 4, windowMs: 1000,
    maxPerEndpointWindow: 2, maxGlobalWindow: 4, ...overrides };
}

function registeredRequest(endpoint, sequence, overrides = {}) {
  const request = registration({ endpoint_id: endpoint, registration_request_id: `quota-${endpoint}-${sequence}`,
    request_id: `transport-${endpoint}-${sequence}`, remote_control_generation: `generation-${endpoint}-${sequence}`, ...overrides });
  assert.doesNotThrow(() => validateControllerMessage(request, "B"), "registration fixtures must pass the real wire writer contract");
  return request;
}

function barrier() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

test("registration flood is endpoint-fair and known retries never spend another slot", async () => {
  const stateStore = new MemoryStateStore();
  let entropyCalls = 0;
  let sealCalls = 0;
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000,
    randomBytes: (size) => { entropyCalls += 1; return crypto.randomBytes(size); } });
  const seal = (value) => { sealCalls += 1; return sealPairingCode(value); };
  const first = registeredRequest("endpoint-flood", 0);
  const allocated = await store.register({ request: first, relayEpoch: "epoch-quota", sealCode: seal });
  for (let index = 1; index < DEFAULT_PAIRING_REGISTRATION_LIMITS.maxPerEndpointWindow; index += 1) {
    await store.register({ request: registeredRequest("endpoint-flood", index), relayEpoch: "epoch-quota", sealCode: seal });
  }
  const before = [stateStore.revision, entropyCalls, sealCalls];
  await assert.rejects(store.register({ request: registeredRequest("endpoint-flood", 99),
    relayEpoch: "epoch-quota", sealCode: seal }), { code: "controller_busy" });
  assert.deepEqual([stateStore.revision, entropyCalls, sealCalls], before, "reject before generation/sealing/persistence");
  const replay = await store.register({ request: { ...first, request_id: "new-transport-retry" },
    relayEpoch: "epoch-quota", sealCode: seal });
  assert.equal(replay.replayed, true);
  assert.equal(replay.result.sealed_code_b64, allocated.result.sealed_code_b64);
  assert.equal(replay.result.request_id, "new-transport-retry");
  assert.deepEqual([stateStore.revision, entropyCalls, sealCalls], before);
  assert.equal((await store.register({ request: registeredRequest("endpoint-other", 1),
    relayEpoch: "epoch-quota", sealCode: seal })).result.type, "desktop_pairing_registered");
});

test("registration quotas survive JSON reopen, TTL expiry, quarantine and clock rollback", async () => {
  let now = 0;
  const stateStore = new MemoryStateStore();
  const options = { stateStore, digestKeyring: keyring(), clock: () => now,
    registrationBudget: smallRegistrationBudget({ maxPerEndpointWindow: 1, maxGlobalWindow: 2 }) };
  let store = await PairingStore.open(options);
  const first = registeredRequest("endpoint-restart", 1);
  const initial = await store.register({ request: first, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  stateStore.value = JSON.parse(JSON.stringify(stateStore.value));
  store = await PairingStore.open(options);
  const next = registeredRequest("endpoint-restart", 2);
  await assert.rejects(store.register({ request: next, relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "controller_busy" });
  now = 1000; // The exact window boundary releases the rate, not the receipt.
  await store.register({ request: next, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  now = 500;
  await assert.rejects(store.register({ request: registeredRequest("endpoint-restart", 3),
    relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "controller_busy" });
  now = 300_001;
  assert.equal(store.activePairing(initial.result.pairing_id), null);
  assert.equal((await store.register({ request: first, relayEpoch: "epoch-quota", sealCode: sealPairingCode })).replayed, true);
  await assert.rejects(store.register({ request: registeredRequest("endpoint-restart", 3),
    relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "controller_busy" }, "expired code remains in quarantine accounting");
  now = 600_001;
  await store.register({ request: registeredRequest("endpoint-restart", 3), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  assert.equal(stateStore.value.registrations.length, 2);
  assert.equal(stateStore.value.tombstones.some((row) => row.pairing_id === initial.result.pairing_id), false);
});

test("sealing rejection has a bounded persisted receipt and cannot bypass admission rate", async () => {
  const stateStore = new MemoryStateStore();
  const options = { stateStore, digestKeyring: keyring(), clock: () => 1000,
    registrationBudget: smallRegistrationBudget({ maxPerEndpointWindow: 1, maxGlobalWindow: 2 }) };
  let store = await PairingStore.open(options);
  // A shape-valid noncontributory X25519 wrapping key reaches the real sealer.
  const invalid = registeredRequest("endpoint-invalid-wrap", 1, { code_wrap_public_key_b64: Buffer.alloc(32).toString("base64") });
  const failure = await store.register({ request: invalid, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  assert.equal(failure.result.type, "error");
  assert.equal(failure.result.code, "pairing_code_generation_failed");
  assert.equal(stateStore.value.registrations[0].pairing_id, null);
  assert.equal(stateStore.value.tombstones.length, 0);
  store = await PairingStore.open(options);
  const replay = await store.register({ request: { ...invalid, request_id: "retry-invalid-wrap" },
    relayEpoch: "epoch-quota", sealCode: () => assert.fail("known rejection must not seal again") });
  assert.equal(replay.replayed, true);
  assert.equal(replay.result.code, "pairing_code_generation_failed");
  await assert.rejects(store.register({ request: registeredRequest("endpoint-invalid-wrap", 2),
    relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "controller_busy" });
  await store.register({ request: registeredRequest("endpoint-valid-wrap", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  assert.doesNotMatch(JSON.stringify(stateStore.value), /error:|openssl|private_key|ERR_OSSL/u);
});

test("global retained capacity rejects new registrations while exact replay remains readable", async () => {
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000,
    registrationBudget: smallRegistrationBudget({ maxPerEndpointRecords: 1, maxGlobalRecords: 2,
      maxPerEndpointWindow: 1, maxGlobalWindow: 2 }) });
  const first = registeredRequest("endpoint-global-a", 1);
  await store.register({ request: first, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  await store.register({ request: registeredRequest("endpoint-global-b", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  await assert.rejects(store.register({ request: registeredRequest("endpoint-global-c", 1),
    relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "controller_busy" });
  assert.equal((await store.register({ request: first, relayEpoch: "epoch-quota", sealCode: sealPairingCode })).replayed, true);
  assert.equal(stateStore.value.registrations.length, 2);
});

test("pairing serialized budget rejects before allocation and preserves shared-file headroom", async () => {
  const stateStore = new MemoryStateStore();
  let entropy = 0;
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000,
    randomBytes: (size) => { entropy += 1; return crypto.randomBytes(size); },
    registrationBudget: { maxNamespaceBytes: 32 * 1024 } });
  let accepted = 0;
  for (let index = 0; index < 100; index += 1) {
    const before = [stateStore.revision, entropy];
    try {
      await store.register({ request: registeredRequest(`endpoint-bytes-${index}`, 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
      accepted += 1;
    } catch (error) {
      assert.equal(error.code, "controller_busy");
      assert.deepEqual([stateStore.revision, entropy], before);
      break;
    }
  }
  assert.ok(accepted > 0 && accepted < 100);
  assert.equal(stateStore.value.registrations.length, accepted);
  assert.ok(Buffer.byteLength(JSON.stringify(stateStore.value), "utf8") <= 32 * 1024);
});

test("bounded endpoint queue coalesces exact flights, snapshots inputs and lets another endpoint proceed", async () => {
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000 });
  const entered = barrier();
  const release = barrier();
  const originalCas = stateStore.compareAndSwap.bind(stateStore);
  let blocked = true;
  stateStore.compareAndSwap = async (...args) => {
    if (blocked) { blocked = false; entered.resolve(); await release.promise; }
    return originalCas(...args);
  };
  const first = registeredRequest("endpoint-queue-a", 1);
  const active = store.register({ request: first, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  const pending = [active];
  await entered.promise;
  try {
    const duplicate = store.register({ request: { ...first, request_id: "transport-coalesced" }, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
    pending.push(duplicate);
    await assert.rejects(store.register({ request: { ...first, remote_control_generation: "generation-conflict" },
      relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "pairing_registration_conflict" });
    const queued = registeredRequest("endpoint-queue-a", 2);
    const second = store.register({ request: queued, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
    pending.push(second);
    queued.endpoint_id = "endpoint-mutated-after-admission";
    const denied = store.register({ request: registeredRequest("endpoint-queue-a", 3),
      relayEpoch: "epoch-quota", sealCode: sealPairingCode });
    pending.push(denied);
    let admission = "pending";
    denied.then(() => { admission = "accepted"; }, (error) => { admission = error.code; });
    // The first CAS is explicitly held, so a queued acceptance cannot settle.
    // Flush the current turn rather than waiting on a potentially wrong gate.
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(admission, "controller_busy");
    const other = store.register({ request: registeredRequest("endpoint-queue-b", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
    pending.push(other);
    release.resolve();
    const [a, same, b, c] = await Promise.all([active, duplicate, second, other]);
    assert.equal(a.replayed, false);
    assert.equal(same.replayed, true);
    assert.equal(same.result.request_id, "transport-coalesced");
    assert.equal(same.result.sealed_code_b64, a.result.sealed_code_b64);
    assert.equal(b.result.type, "desktop_pairing_registered");
    assert.equal(c.result.type, "desktop_pairing_registered");
    assert.equal(stateStore.value.registrations.length, 3);
    assert.equal(stateStore.value.registrations.find((row) => row.registration_request_id === queued.registration_request_id).endpoint_id, "endpoint-queue-a");
  } finally {
    release.resolve();
    await Promise.allSettled(pending);
  }
});

test("independent store owners cannot overshoot the durable global quota through CAS races", async () => {
  const stateStore = new MemoryStateStore();
  const options = { stateStore, digestKeyring: keyring(), clock: () => 1000,
    registrationBudget: smallRegistrationBudget({ maxPerEndpointRecords: 1, maxGlobalRecords: 2,
      maxPerEndpointWindow: 1, maxGlobalWindow: 2 }) };
  const left = await PairingStore.open(options);
  await left.register({ request: registeredRequest("endpoint-cas-seed", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  const right = await PairingStore.open(options);
  const results = await Promise.allSettled([
    left.register({ request: registeredRequest("endpoint-cas-left", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode }),
    right.register({ request: registeredRequest("endpoint-cas-right", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode }),
  ]);
  assert.equal(results.filter((result) => result.status === "fulfilled").length, 1);
  assert.equal(results.find((result) => result.status === "rejected").reason.code, "controller_busy");
  assert.equal(stateStore.value.registrations.length, 2);
});

test("global admission queue is bounded and cannot block an already committed replay", async () => {
  const stateStore = new MemoryStateStore();
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000 });
  const seed = registeredRequest("endpoint-queue-seed", 1);
  const seeded = await store.register({ request: seed, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  const entered = barrier();
  const release = barrier();
  const originalCas = stateStore.compareAndSwap.bind(stateStore);
  let blocked = true;
  stateStore.compareAndSwap = async (...args) => {
    if (blocked) { blocked = false; entered.resolve(); await release.promise; }
    return originalCas(...args);
  };
  const pending = [store.register({ request: registeredRequest("endpoint-pending-0", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode })];
  await entered.promise;
  try {
    for (let index = 1; index < 64; index += 1) pending.push(store.register({
      request: registeredRequest(`endpoint-pending-${index}`, 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode,
    }));
    const overflow = store.register({ request: registeredRequest("endpoint-overflow", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
    pending.push(overflow);
    let overflowState = "pending";
    overflow.then(() => { overflowState = "accepted"; }, (error) => { overflowState = error.code; });
    const replay = store.register({ request: { ...seed, request_id: "queue-full-replay" }, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
    pending.push(replay);
    let replayState = "pending";
    replay.then((result) => { replayState = result; }, (error) => { replayState = error.code; });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(overflowState, "controller_busy");
    assert.equal(replayState.replayed, true);
    assert.equal(replayState.result.sealed_code_b64, seeded.result.sealed_code_b64);
    release.resolve();
    assert.equal((await Promise.all(pending.slice(0, 64))).length, 64);
    assert.equal(stateStore.value.registrations.length, 65);
  } finally {
    release.resolve();
    await Promise.allSettled(pending);
  }
});

test("an unknown persistence acknowledgement does not poison the queue or double-charge its replay", async () => {
  // This memory owner deliberately remains readable after the lost response.
  // A production state owner's own fail-stop/read gates are not bypassed.
  const stateStore = new MemoryStateStore();
  let entropy = 0;
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => 1000,
    registrationBudget: smallRegistrationBudget({ maxPerEndpointWindow: 1, maxGlobalWindow: 2 }),
    randomBytes: (size) => { entropy += 1; return crypto.randomBytes(size); } });
  const originalCas = stateStore.compareAndSwap.bind(stateStore);
  let failReply = true;
  stateStore.compareAndSwap = async (...args) => {
    const result = await originalCas(...args);
    if (failReply) { failReply = false; throw Object.assign(new Error("lost storage acknowledgement"), { code: "storage_reply_lost" }); }
    return result;
  };
  const request = registeredRequest("endpoint-unknown-write", 1);
  await assert.rejects(store.register({ request, relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "pairing_persist_failed" });
  const before = [stateStore.revision, entropy];
  const replay = await store.register({ request: { ...request, request_id: "unknown-write-retry" }, relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  assert.equal(replay.replayed, true);
  assert.deepEqual([stateStore.revision, entropy], before);
  assert.equal(stateStore.value.registrations.length, 1);
  assert.equal((await store.register({ request: registeredRequest("endpoint-after-failure", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode })).result.type,
    "desktop_pairing_registered");
});

test("unpublished persistence failures spend a bounded work rate without starving another endpoint", async () => {
  const stateStore = new MemoryStateStore();
  let now = 1000;
  let entropy = 0;
  const store = await PairingStore.open({ stateStore, digestKeyring: keyring(), clock: () => now,
    registrationBudget: smallRegistrationBudget(),
    randomBytes: (size) => { entropy += 1; return crypto.randomBytes(size); } });
  stateStore.failWrites = true;
  const request = registeredRequest("endpoint-write-failure", 1);
  for (let index = 0; index < 2; index += 1) {
    await assert.rejects(store.register({ request, relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "pairing_persist_failed" });
  }
  const before = entropy;
  await assert.rejects(store.register({ request, relayEpoch: "epoch-quota", sealCode: sealPairingCode }), { code: "controller_busy" });
  assert.equal(entropy, before, "refuse before further crypto even though no receipt was published");
  assert.equal(stateStore.value.registrations.length, 0);
  stateStore.failWrites = false;
  await store.register({ request: registeredRequest("endpoint-other", 1), relayEpoch: "epoch-quota", sealCode: sealPairingCode });
  now = 2000;
  assert.equal((await store.register({ request, relayEpoch: "epoch-quota", sealCode: sealPairingCode })).result.type, "desktop_pairing_registered");
});

test("invalid or unfair registration budgets fail before any state publication", async () => {
  for (const registrationBudget of [
    { maxPerEndpointRecords: 3, maxGlobalRecords: 4 },
    { maxPerEndpointWindow: 3, maxGlobalWindow: 4 },
    { maxNamespaceBytes: 31 * 1024 }, { maxNamespaceBytes: 16 * 1024 * 1024 },
    { windowMs: 600_001 }, { maxGlobalRecords: Infinity }, { unsupportedPolicy: 1 },
  ]) {
    const stateStore = new MemoryStateStore();
    await assert.rejects(PairingStore.open({ stateStore, digestKeyring: keyring(), registrationBudget }), { code: "pairing_registration_limits_invalid" });
    assert.equal(stateStore.revision, "0");
    assert.equal(stateStore.value, null);
  }
});
