import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, access, stat } from "node:fs/promises";
import crypto from "node:crypto";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import net from "node:net";
import { once } from "node:events";
import test from "node:test";
import { RelayStateStore } from "../relay-state-store.js";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";
import { createEnrollmentCoverageControl, requestEnrollmentCapacity,
  requestEnrollmentCoverage } from "../enrollment-coverage-control.js";
import { main } from "../scripts/enrollment-migrate.js";

async function fixture(t, maxBytes = 16384) {
  const root = await mkdtemp(join(tmpdir(), "pinvou-capacity-"));
  const statePath = join(root, "controller", "state.json");
  const legacyStatePath = join(root, "legacy.json");
  const probe = net.createServer().listen(0, "127.0.0.1");
  await once(probe, "listening");
  const port = probe.address().port;
  await new Promise((done) => probe.close(done));
  const env = { NODE_ENV: "test", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "official",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "capacity-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "test-capacity-enrollment-key-000000000000",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: statePath, PINVOU_REMOTE_STATE_PATH: legacyStatePath,
    PINVOU_REMOTE_GATEWAY_CONTROL_HOST: "127.0.0.1", PINVOU_REMOTE_GATEWAY_CONTROL_PORT: String(port),
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: "test-capacity-control-key-000000000000000" };
  const store = await RelayStateStore.open({ statePath, legacyStatePath, maxBytes,
    processIdentity: async (pid) => `capacity-fixture:${pid}`,
    ...(process.platform === "win32" ? { protectPath: async () => {}, verifyPath: async () => {},
      verifyAncestors: async () => {} } : {}) });
  const registry = EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(env) });
  let control;
  t.after(async () => { if (control) await control.close(); await store.close(); await rm(root, { recursive: true }); });
  return { store, registry, env, statePath,
    async listen() { control = createEnrollmentCoverageControl({ registry, stateStore: store,
      gatewayInstanceId: "1".repeat(64), env }); await control.listen(); } };
}

const operation = (id) => ({ operation: "allowlist", endpoint_id: id,
  credential: "test-capacity-access-credential-0000000000" });

test("live capacity reports exact disk encoding and three retained states without a CAS", async (t) => {
  const f = await fixture(t);
  const succeeded = { requestId: "capacity-succeeded-001", operations: [operation("endpoint-success")] };
  const pending = { requestId: "capacity-pending-0001", operations: [operation("endpoint-pending")] };
  const failed = { requestId: "capacity-failed-00001", operations: [operation("endpoint-success")] };
  await f.registry.executeMigrationRequest(succeeded);
  await f.registry.beginMigrationRequest(pending);
  await assert.rejects(f.registry.executeMigrationRequest(failed));
  await f.store.compareAndSwap("other_capacity", "0", { payload: "x".repeat(200) });
  const before = await readFile(f.statePath);
  const revision = f.store.readNamespaceRevision("endpoint_enrollment");
  await f.listen();
  const projection = await requestEnrollmentCapacity({ env: f.env });
  assert.equal(projection.storage.serialized_bytes, before.length);
  assert.equal(projection.storage.max_bytes, 16384);
  assert.deepEqual(projection.ledger, { ...revision, total: 3, max_requests: 100000,
    pending: 1, succeeded: 1, failed: 1 });
  assert.deepEqual(await readFile(f.statePath), before);
  assert.deepEqual(await f.registry.executeMigrationRequest(succeeded), f.registry.getMigrationRequest(succeeded.requestId));
  await assert.rejects(f.registry.executeMigrationRequest({ ...succeeded, operations: [operation("changed-endpoint")] }),
    { code: "migration_request_conflict" });
  const old = f.registry.exportSnapshot();
  await f.registry.importSnapshot({ snapshot: old.registry, expectedNamespaceRevision: old.namespace_revision });
  assert.equal(f.registry.getMigrationCapacity().total, 3);
  const coverage = await requestEnrollmentCoverage({ endpointIds: ["endpoint-success"], env: f.env });
  assert.equal(Object.hasOwn(coverage, "ledger"), false);
  assert.equal(coverage.coverage.total, 1);
  const output = [];
  assert.equal(await main(["--capacity"], f.env, { log: (s) => output.push(JSON.parse(s)), error: assert.fail }), 0);
  assert.equal(output[0].ledger.total, 3);
});

test("capacity refuses unauthorized caller and closed writer without touching disk", async (t) => {
  const f = await fixture(t);
  await f.registry.beginMigrationRequest({ requestId: "capacity-pending-auth", operations: [operation("auth-endpoint")] });
  const before = await readFile(f.statePath);
  await f.listen();
  await assert.rejects(requestEnrollmentCapacity({ env: { ...f.env,
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: "wrong-control-key-000000000000000000000" } }));
  assert.deepEqual(await readFile(f.statePath), before);
  await f.store.close();
  await assert.rejects(requestEnrollmentCapacity({ env: f.env }));
  assert.throws(() => f.store.readCapacity(), { code: "relay_state_closed" });
  assert.deepEqual(await readFile(f.statePath), before);
});

test("shared byte budget rejects new request atomically and retains original pending proof", async (t) => {
  const f = await fixture(t, 2048);
  const original = { requestId: "capacity-small-pending", operations: [operation("small-endpoint")] };
  await f.registry.beginMigrationRequest(original);
  let revision = "0";
  for (let size = 32; size < 2048; size += 32) {
    try { revision = (await f.store.compareAndSwap("other_capacity", revision, { value: "x".repeat(size) })).namespace_revision; }
    catch (error) { assert.equal(error.code, "relay_state_capacity_reached"); break; }
  }
  const before = await readFile(f.statePath);
  await assert.rejects(f.registry.beginMigrationRequest({ requestId: "capacity-refused-new", operations: [operation("new-endpoint")] }),
    { code: "enrollment_registry_capacity_reached" });
  assert.deepEqual(await readFile(f.statePath), before);
  assert.equal((await f.registry.beginMigrationRequest(original)).status, "pending");
  await assert.rejects(f.registry.completeMigrationRequest(original), { code: "enrollment_registry_capacity_reached" });
  assert.deepEqual(await readFile(f.statePath), before);
  assert.equal(f.registry.getMigrationRequest(original.requestId).status, "pending");
  assert.equal(f.registry.getMigrationCapacity().total, 1);
});

test("signed capacity response still rejects a foreign nonce or identifying field", async (t) => {
  const f = await fixture(t);
  await f.listen();
  for (const mutate of [(value) => { value.request_nonce = "0".repeat(64); },
    (value) => { value.ledger.request_id = "must-not-be-exposed"; }]) {
    await assert.rejects(requestEnrollmentCapacity({ env: f.env, fetchImpl: async (...args) => {
      const actual = await fetch(...args);
      const value = await actual.json();
      mutate(value);
      const bytes = Buffer.from(JSON.stringify(value));
      const signature = crypto.createHmac("sha256", f.env.PINVOU_REMOTE_GATEWAY_CONTROL_KEY)
        .update("pinvou-enrollment-coverage-capacity-response-v1\0", "ascii").update(bytes).digest("hex");
      return new Response(bytes, { headers: { "x-pinvou-coverage-signature": signature } });
    } }), { code: "coverage_response_invalid" });
  }
});

test("CLI observation of missing state never initializes an offline store", async (t) => {
  const f = await fixture(t);
  await f.listen();
  assert.equal((await stat(dirname(f.statePath))).isDirectory(), true);
  const missing = join(dirname(f.statePath), "missing", "state.json");
  await assert.rejects(access(missing), { code: "ENOENT" });
  const failures = [];
  assert.equal(await main(["--capacity"], { ...f.env, PINVOU_REMOTE_CONTROLLER_STATE_PATH: missing },
    { log: assert.fail, error: (s) => failures.push(JSON.parse(s)) }), 1);
  assert.equal(failures.length, 1);
  await assert.rejects(access(missing), { code: "ENOENT" });
});
