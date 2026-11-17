import assert from "node:assert/strict";
import crypto from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { RelayStateStore } from "../relay-state-store.js";
import { ENROLLMENT_NAMESPACE, EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";
import { PAIRING_NAMESPACE, PairingStore } from "../pairing-store.js";
import { PairingRegistrationHandler } from "../pairing-registration-handler.js";

const MAX_BYTES = 32768;
const ENDPOINT = "shared-capacity-endpoint";
const ACCESS = "shared-capacity-access-credential-00000000";
const DESKTOP = "shared-capacity-desktop-credential-0000000";

async function fixture(t) {
  const root = await mkdtemp(join(tmpdir(), "pinvou-shared-capacity-"));
  const statePath = join(root, "controller", "state.json");
  const options = { statePath, legacyStatePath: join(root, "legacy.json"), maxBytes: MAX_BYTES,
    processIdentity: async (pid) => `shared-capacity-fixture:${pid}`,
    // These domain-budget tests use the same ACL adapters as registry fixtures;
    // they do not establish the native Windows permission contract.
    ...(process.platform === "win32" ? { protectPath: async () => {}, verifyPath: async () => {},
      verifyAncestors: async () => {} } : {}) };
  let store = await RelayStateStore.open(options);
  t.after(async () => { await store.close(); await rm(root, { recursive: true, force: true }); });
  const registry = EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig({
    NODE_ENV: "test", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "official",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: statePath, PINVOU_REMOTE_STATE_PATH: options.legacyStatePath,
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "shared-capacity-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "shared-capacity-enrollment-key-0000000000",
  }) });
  await registry.enroll({ endpointId: ENDPOINT, credential: ACCESS, desktopCredential: DESKTOP });
  const pairing = await PairingStore.open({ stateStore: store, clock: () => 1000,
    // Exercise the shared byte budget before the per-endpoint admission limits.
    // The test loop stays bounded at 64; production defaults remain unchanged.
    registrationBudget: { maxPerEndpointRecords: 64, maxPerEndpointWindow: 64 },
    digestKeyring: { activeKeyId: "shared-pairing-key", keys: [{ keyId: "shared-pairing-key",
      key: "shared-capacity-pairing-digest-key-0000000" }] } });
  const handler = new PairingRegistrationHandler({ pairingStore: pairing, relayEpoch: "shared-capacity-epoch",
    authorizeEndpoint: ({ endpointId }) => registry.authorizePairingRegistration({ endpointId, desktopSecret: DESKTOP }) });
  const rawKey = () => crypto.generateKeyPairSync("x25519").publicKey
    .export({ format: "der", type: "spki" }).subarray(-32).toString("base64");
  const wrap = rawKey(), handshake = rawKey();
  return { root, statePath, store, registry,
    register(index) { return handler.register({ v: 1, type: "desktop_pairing_register",
      request_id: `shared-request-${index}`, registration_request_id: `shared-registration-${index}`,
      endpoint_id: ENDPOINT, remote_control_generation: `shared-generation-${index}`,
      code_wrap_public_key_b64: wrap, handshake_public_key_b64: handshake,
      key_algorithm: "x25519", capabilities: ["pairing_v1"] }, {}); },
    async reopen() { await store.close(); store = await RelayStateStore.open(options); return store; } };
}

test("pairing-only shared-byte growth warns before exhaustion and again on reopen", async (t) => {
  const warnings = [];
  t.mock.method(console, "warn", (...args) => warnings.push(args));
  const f = await fixture(t);
  const enrollment = f.store.readNamespace(ENROLLMENT_NAMESPACE);
  let accepted = 0, failure;
  for (let index = 0; index < 64; index += 1) {
    try { await f.register(index); accepted += 1; } catch (error) { failure = error; break; }
  }
  assert.ok(accepted > 0 && accepted < 64, "real PairingStore must reach the bounded shared budget");
  assert.equal(failure?.code, "pairing_persist_failed");
  // The shared store revision advances; enrollment's own revision and data do not.
  assert.deepEqual(f.store.readNamespace(ENROLLMENT_NAMESPACE), { ...enrollment,
    store_revision: f.store.readNamespace(ENROLLMENT_NAMESPACE).store_revision });
  assert.equal(warnings.length, 1, "pairing alone must warn without an enrollment mutation");
  assert.equal(warnings[0][0], "[relay-capacity] controller_state");
  const warning = JSON.parse(warnings[0][1]);
  assert.deepEqual(Object.keys(warning).sort(), ["max_bytes", "serialized_bytes"]);
  assert.equal(warning.max_bytes, MAX_BYTES);
  assert.ok(warning.serialized_bytes >= Math.ceil(MAX_BYTES * 0.8));
  assert.ok(warning.serialized_bytes <= f.store.readCapacity().serialized_bytes);
  const output = warnings.flat().join(" ");
  for (const secret of [ENDPOINT, ACCESS, DESKTOP, f.statePath, "shared-registration-"]) {
    assert.equal(output.includes(secret), false);
  }
  const reopened = await f.reopen();
  assert.equal(warnings.length, 2, "an existing high-water snapshot warns during owner open");
  assert.equal(JSON.parse(warnings[1][1]).serialized_bytes, reopened.readCapacity().serialized_bytes);
  const pairing = reopened.readNamespace(PAIRING_NAMESPACE);
  const committed = await reopened.compareAndSwap(PAIRING_NAMESPACE, pairing.namespace_revision, pairing.value);
  assert.deepEqual(committed.value, pairing.value);
  assert.equal(warnings.length, 2);
});

test("a warning sink failure cannot turn a committed pairing into failure or poison the next CAS", async (t) => {
  let attempts = 0;
  t.mock.method(console, "warn", () => { attempts += 1; throw new Error("offline warning sink"); });
  const f = await fixture(t);
  for (let index = 0; f.store.readCapacity().serialized_bytes < Math.ceil(MAX_BYTES * 0.8); index += 1) {
    assert.ok(index < 64);
    const result = await f.register(index);
    assert.equal(result.type, "desktop_pairing_registered");
  }
  assert.equal(attempts, 1);
  const before = f.store.readNamespace("next_probe");
  const committed = await f.store.compareAndSwap("next_probe", before.namespace_revision, { ready: true });
  assert.deepEqual(committed.value, { ready: true });
  assert.equal(attempts, 1);
});
