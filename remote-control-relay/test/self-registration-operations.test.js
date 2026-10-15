import assert from "node:assert/strict";
import crypto from "node:crypto";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { test } from "node:test";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig, SELF_REGISTRATION_LIMITS } from "../enrollment-registry.js";
import { RelayStateStore, protectWindowsPath } from "../relay-state-store.js";
import { PairingStore } from "../pairing-store.js";
import { sealPairingCode } from "../pairing-code-sealer.js";
import { main as migrationCli } from "../scripts/enrollment-migrate.js";

const ACCESS = "self-registration-access-0000000000000001";
const DESKTOP = "self-registration-desktop-00000000000001";
const endpoint = index => `ep_${String(index).padStart(24, "0")}`;
function environment(root) {
  return { NODE_ENV: "production", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
    PINVOU_REMOTE_ENROLLMENT_SELF_REGISTRATION: "true",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "self-registration-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "self-registration-material-at-least-32-bytes",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(root, "controller", "state.json"),
    PINVOU_REMOTE_STATE_PATH: join(root, "legacy.json") };
}
async function register(registry, index) {
  return registry.authorizeDesktopRegistration({ endpointId: endpoint(index),
    accessToken: ACCESS, desktopSecret: DESKTOP, admitSelfRegistration: true });
}
function memoryFixture() {
  const env = environment(resolve("self-registration-operations-fixture"));
  let value = null, revision = "0", writes = 0;
  const store = {
    getPathPolicy: () => ({ controller_state_path: env.PINVOU_REMOTE_CONTROLLER_STATE_PATH, legacy_web_state_path: env.PINVOU_REMOTE_STATE_PATH }),
    readCapacity: () => ({ max_bytes: 16 * 1024 * 1024 }),
    readNamespaceRevision: () => ({ store_revision: revision, namespace_revision: revision }),
    readNamespace: () => ({ store_revision: revision, namespace_revision: revision, value: structuredClone(value) }),
    async compareAndSwap(_name, expected, next) {
      await Promise.resolve();
      if (expected !== revision) throw Object.assign(new Error("conflict"), { code: "namespace_revision_conflict" });
      value = structuredClone(next); revision = String(BigInt(revision) + 1n); writes++;
      return this.readNamespace();
    },
  };
  return { registry: EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(env) }), writes: () => writes };
}

test("anonymous quota rejects a distributed identity flood without blocking known devices or administrator enrollment", async () => {
  const { registry, writes } = memoryFixture();
  for (let i = 0; i < SELF_REGISTRATION_LIMITS.max_entries; i++) await register(registry, i);
  const before = writes();
  await assert.rejects(register(registry, 9999), { code: "self_registration_capacity_reached" });
  assert.equal(writes(), before);
  await register(registry, 0);
  assert.equal(writes(), before, "known identities reconnect even when anonymous quota is full");
  await registry.enroll({ endpointId: "administrator-device", credential: ACCESS, desktopCredential: DESKTOP });
  assert.equal(registry.getControllerReadiness("administrator-device").desktop_ready, true);
  const capacity = registry.getSelfRegistrationCapacity();
  assert.equal(capacity.entries, SELF_REGISTRATION_LIMITS.max_entries);
  assert.ok(capacity.bytes <= capacity.max_bytes);
});

test("inventory and cleanup are explicit, revision checked, atomic and never remove administrator or revoked identities", async () => {
  const { registry, writes } = memoryFixture();
  await register(registry, 1); await register(registry, 2);
  await registry.enroll({ endpointId: "administrator-device", credential: ACCESS, desktopCredential: DESKTOP });
  const inventory = registry.listSelfRegistrations();
  assert.equal(inventory.entries.length, 2);
  assert.equal(JSON.stringify(inventory).includes(ACCESS), false);
  assert.equal(JSON.stringify(inventory).includes("digest_hex"), false);
  const before = writes();
  const selection = { ...inventory, entries: [inventory.entries[0]] };
  assert.equal((await registry.cleanupSelfRegistrations({ inventory: selection })).removed, 0);
  assert.equal(writes(), before);
  const mixed = { ...inventory, entries: [...inventory.entries,
    { endpoint_id: "administrator-device", revision: "1", status: "active" }] };
  await assert.rejects(registry.cleanupSelfRegistrations({ inventory: mixed, dryRun: false }), { code: "self_registration_cleanup_conflict" });
  assert.equal(writes(), before);
  assert.equal((await registry.cleanupSelfRegistrations({ inventory: selection, dryRun: false })).removed, 1);
  await assert.rejects(registry.authorizePairingRegistration({ endpointId: endpoint(1), desktopSecret: DESKTOP }), { code: "enrollment_required" });
  await assert.rejects(registry.cleanupSelfRegistrations({ inventory, dryRun: false }), { code: "enrollment_revision_conflict" });
  assert.equal(registry.getControllerReadiness("administrator-device").desktop_ready, true);
  await registry.revoke({ endpointId: endpoint(2), expectedEndpointRevision: "1" });
  await assert.rejects(registry.cleanupSelfRegistrations({ inventory: registry.listSelfRegistrations(), dryRun: false }), { code: "self_registration_cleanup_revoked" });
  await assert.rejects(register(registry, 2), { code: "endpoint_revoked" });
  await register(registry, 1); // Explicit cleanup is de-registration, not a permanent ban.
  assert.equal(registry.getSelfRegistrationCapacity().entries, 2);
});

async function diskFixture(t, maxBytes = 16384, nativePermissions = false) {
  const root = await mkdtemp(join(tmpdir(), "pinvou-self-registration-"));
  if (process.platform === "win32" && nativePermissions) await protectWindowsPath(root, "directory");
  const env = environment(root);
  const store = await RelayStateStore.open({ statePath: env.PINVOU_REMOTE_CONTROLLER_STATE_PATH,
    legacyStatePath: env.PINVOU_REMOTE_STATE_PATH, maxBytes,
    processIdentity: async pid => `self-registration-fixture:${pid}`,
    ...(process.platform === "win32" && !nativePermissions ? { protectPath: async () => {}, verifyPath: async () => {}, verifyAncestors: async () => {} } : {}) });
  t.after(async () => { await store.close(); await rm(root, { recursive: true, force: true }); });
  return { root, env, store, registry: EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(env) }) };
}

test("anonymous saturation leaves real shared-store capacity for an existing user's sealed pairing registration", async t => {
  const { store, registry, env } = await diskFixture(t);
  await registry.enroll({ endpointId: "existing-user", credential: ACCESS, desktopCredential: DESKTOP });
  let rejected = false;
  for (let i = 0; i < 100; i++) {
    const before = await readFile(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
    try { await register(registry, i); }
    catch (error) {
      assert.equal(error.code, "enrollment_registry_capacity_reached");
      assert.deepEqual(await readFile(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), before);
      rejected = true; break;
    }
  }
  assert.equal(rejected, true);
  const key = () => crypto.generateKeyPairSync("x25519").publicKey.export({ type: "spki", format: "der" }).subarray(-32).toString("base64");
  const pairing = await PairingStore.open({ stateStore: store,
    digestKeyring: { activeKeyId: "pairing-key", keys: [{ keyId: "pairing-key", key: "independent-pairing-material-at-least-32-bytes" }] } });
  const response = await pairing.register({ relayEpoch: "relay-epoch-1", sealCode: sealPairingCode,
    request: { v: 1, type: "desktop_pairing_register", request_id: "request-pairing-1",
      registration_request_id: "registration-pairing-1", endpoint_id: "existing-user",
      remote_control_generation: "generation-1", key_algorithm: "x25519", capabilities: ["pairing_v1"],
      code_wrap_public_key_b64: key(), handshake_public_key_b64: key() } });
  assert.equal(response.result.type, "desktop_pairing_registered");
  await registry.enroll({ endpointId: "new-administrator-device", credential: ACCESS });
  assert.equal(registry.getControllerReadiness("new-administrator-device").web_ready, true);
});

test("capacity reserve is enforced against the queued store state and refusal does not degrade the writer", async t => {
  const { store } = await diskFixture(t, 4096);
  const before = store.readCapacity();
  const first = store.compareAndSwap("other_writer", "0", { payload: "x".repeat(1700) });
  const denied = store.compareAndSwap("anonymous_writer", "0", { payload: "x".repeat(1500) }, { capacityReserveBytes: 1024 });
  await first;
  await assert.rejects(denied, { code: "relay_state_capacity_reached" });
  assert.equal(store.readNamespace("anonymous_writer").namespace_revision, "0");
  assert.ok(store.readCapacity().serialized_bytes > before.serialized_bytes);
  await store.compareAndSwap("legitimate_writer", "0", { payload: "x".repeat(100) });
  for (const capacityReserveBytes of [-1, 4096, NaN]) {
    await assert.rejects(store.compareAndSwap("invalid", "0", {}, { capacityReserveBytes }), { code: "relay_state_capacity_reserve_invalid" });
  }
});

test("offline CLI writes a private redacted inventory, checks cleanup without writes and applies only the selected identity", async t => {
  const { registry, store, env, root } = await diskFixture(t, 16384, true);
  await register(registry, 1);
  await registry.enroll({ endpointId: "administrator-device", credential: ACCESS });
  await store.close();
  const inventoryPath = join(root, "inventory.json");
  const output = [], errors = [];
  const io = { log: s => output.push(JSON.parse(s)), error: s => errors.push(JSON.parse(s)) };
  assert.equal(await migrationCli(["--list-self-registrations", "--output", inventoryPath], env, io), 0, JSON.stringify(errors));
  const raw = await readFile(inventoryPath, "utf8"), inventory = JSON.parse(raw);
  assert.equal(inventory.entries.length, 1);
  assert.equal(raw.includes(ACCESS), false);
  if (process.platform !== "win32") assert.equal((await stat(inventoryPath)).mode & 0o777, 0o600);
  const before = await readFile(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
  assert.equal(await migrationCli(["--check-self-registration-cleanup", "--input", inventoryPath], env, io), 0);
  assert.deepEqual(await readFile(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), before);
  assert.equal(await migrationCli(["--apply-self-registration-cleanup", "--input", inventoryPath], env, io), 0);
  assert.equal(output.at(-1).removed, 1);
  assert.deepEqual(errors, []);
});
