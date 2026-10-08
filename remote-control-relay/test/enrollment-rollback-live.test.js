import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import { once } from "node:events";
import { mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import net from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";
import { protectWindowsPath, RelayStateStore } from "../relay-state-store.js";
import { requestEnrollmentCoverage, loadEnrollmentCoverageControlConfig, createEnrollmentCoverageControl } from "../enrollment-coverage-control.js";
import { createEnrollmentGateway, validateLegacyRollbackTopology } from "../enrollment-gateway.js";
import { checkLegacyRollback } from "../../scripts/check-enrollment-gated-v2-rollback.mjs";

const relay = dirname(dirname(fileURLToPath(import.meta.url)));
const root = dirname(relay);

async function freePort() {
  const server = net.createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

function run(script, env, timeout = 30_000) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [script], {
      cwd: root, env: { ...process.env, ...env }, windowsHide: true,
      shell: false, stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    const timer = setTimeout(() => { child.kill(); reject(new Error("test CLI timed out")); }, timeout);
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.once("error", (error) => { clearTimeout(timer); reject(error); });
    child.once("close", (code) => { clearTimeout(timer); resolve({ code, stdout, stderr }); });
  });
}

async function gatewayFixture(t) {
  const directory = await realpath(await mkdtemp(join(tmpdir(), "pinvou-rollback-live-")));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const gatewayPort = await freePort();
  const controlPort = await freePort();
  const upstream = http.createServer((_request, response) => response.end("test loopback listener"));
  upstream.listen(0, "127.0.0.1");
  await once(upstream, "listening");
  t.after(() => new Promise((resolve) => upstream.close(resolve)));
  const env = {
    NODE_ENV: "test", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "rollback-test-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: crypto.randomBytes(32).toString("hex"),
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(directory, "controller", "state.json"),
    PINVOU_REMOTE_STATE_PATH: join(directory, "legacy.json"),
    PINVOU_REMOTE_GATEWAY_HOST: "127.0.0.1", PINVOU_REMOTE_GATEWAY_PORT: String(gatewayPort),
    PINVOU_REMOTE_GATEWAY_CONTROL_HOST: "127.0.0.1", PINVOU_REMOTE_GATEWAY_CONTROL_PORT: String(controlPort),
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: crypto.randomBytes(32).toString("hex"),
    PINVOU_REMOTE_LEGACY_RELAY_URL: `ws://127.0.0.1:${upstream.address().port}/pinvou3/remote/ws`,
    PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST: "127.0.0.1",
    PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: JSON.stringify([`http://127.0.0.1:${gatewayPort}/pinvou3/remote`]),
    PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY: join(directory, "inventory.json"),
  };
  const config = loadEnrollmentRegistryConfig(env);
  // Real file permissions/DACL and real process identity: the CLI must consume
  // exactly the same protected store, not a mock or an unlocked replica.
  const store = await RelayStateStore.open({ statePath: config.controller_state_path, legacyStatePath: config.legacy_web_state_path });
  const registry = EnrollmentRegistry.open({ stateStore: store, config });
  await registry.enroll({ endpointId: "legacy-rollback-endpoint", credential: crypto.randomBytes(32).toString("hex"),
    desktopCredential: crypto.randomBytes(32).toString("hex") });
  await store.close();
  await writeFile(env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY, JSON.stringify({
    schema: "pinvou-legacy-endpoint-inventory", version: 1, endpoint_ids: ["legacy-rollback-endpoint"],
  }), { mode: 0o600 });
  if (process.platform === "win32") await protectWindowsPath(env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY, "file");
  const child = spawn(process.execPath, [join(relay, "scripts", "enrollment-gateway-server.js")], {
    cwd: root, env: { ...process.env, ...env }, windowsHide: true, shell: false, stdio: ["ignore", "pipe", "pipe"],
  });

  t.after(async () => {
    if (child.exitCode !== null || child.signalCode !== null) return;
    const exited = once(child, "exit");
    child.kill();
    await exited;
  });
  await new Promise((resolve, reject) => {
    let output = "";
    const timer = setTimeout(() => reject(new Error("test gateway startup timeout")), 30_000);
    child.stdout.on("data", (chunk) => {
      output += chunk;
      if (output.includes('"enrollment_gateway":"v1"')) { clearTimeout(timer); resolve(); }
    });
    child.once("exit", () => { clearTimeout(timer); reject(new Error("test gateway startup failed")); });
    child.once("error", (error) => { clearTimeout(timer); reject(error); });
  });
  return { env, config, child };
}

test("rollback live gate: a real registry revocation during the preflight barrier cannot reuse initial coverage",
  { timeout: 90_000 }, async (t) => {
    const directory = await realpath(await mkdtemp(join(tmpdir(), "pinvou-rollback-revoke-")));
    let store;
    let upstream;
    let gateway;
    let control;
    t.after(async () => {
      await control?.close();
      await gateway?.close();
      if (upstream) await new Promise((resolve) => upstream.close(resolve));
      await store?.close();
      await rm(directory, { recursive: true, force: true });
    });
    const env = { NODE_ENV: "test", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
      PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(directory, "controller", "state.json"),
      PINVOU_REMOTE_STATE_PATH: join(directory, "legacy.json"),
      PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "revoke-test-key", PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: crypto.randomBytes(32).toString("hex"),
      PINVOU_REMOTE_GATEWAY_CONTROL_HOST: "127.0.0.1", PINVOU_REMOTE_GATEWAY_CONTROL_PORT: String(await freePort()),
      PINVOU_REMOTE_GATEWAY_CONTROL_KEY: crypto.randomBytes(32).toString("hex"),
      PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY: join(directory, "inventory.json") };
    store = await RelayStateStore.open({ statePath: env.PINVOU_REMOTE_CONTROLLER_STATE_PATH,
      legacyStatePath: env.PINVOU_REMOTE_STATE_PATH });
    const registry = EnrollmentRegistry.open({ stateStore: store, config: loadEnrollmentRegistryConfig(env) });
    await registry.enroll({ endpointId: "revoked-endpoint", credential: crypto.randomBytes(32).toString("hex"),
      desktopCredential: crypto.randomBytes(32).toString("hex") });
    upstream = http.createServer((_request, response) => response.end("test loopback listener"));
    upstream.listen(0, "127.0.0.1");
    await once(upstream, "listening");
    gateway = createEnrollmentGateway({ registry, upstreamUrl: `ws://127.0.0.1:${upstream.address().port}/ws` });
    const address = await gateway.listen({ host: "127.0.0.1", port: 0 });
    control = createEnrollmentCoverageControl({ registry, stateStore: store, gatewayInstanceId: gateway.instanceId, env });
    await control.listen();
    await writeFile(env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY, JSON.stringify({
      schema: "pinvou-legacy-endpoint-inventory", version: 1, endpoint_ids: ["revoked-endpoint"],
    }), { mode: 0o600 });
    if (process.platform === "win32") await protectWindowsPath(env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY, "file");
    let enteredResolve;
    let releaseResolve;
    const entered = new Promise((resolve) => { enteredResolve = resolve; });
    const release = new Promise((resolve) => { releaseResolve = resolve; });
    const checked = checkLegacyRollback(env, (instance) => validateLegacyRollbackTopology({
      publicEntrypoints: [`http://127.0.0.1:${address.port}/pinvou3/remote`],
      legacyRelayUrl: `ws://127.0.0.1:${upstream.address().port}/ws`, legacyRelayBindHost: "127.0.0.1",
      expectedGatewayInstance: instance,
      fetchImpl: async (url, options) => { enteredResolve(); await release; return fetch(url, options); },
    }));
    const rejection = assert.rejects(checked, { code: "legacy_endpoint_coverage_incomplete" });
    await entered;
    try { await registry.revoke({ endpointId: "revoked-endpoint", expectedEndpointRevision: "1" }); }
    finally { releaseResolve(); }
    await rejection;
    assert.equal(registry.getStatus("revoked-endpoint").status, "revoked");
  });

test("rollback live gate: real gateway owner, coverage and preflight CLI succeed without acquiring a second lock",
  { timeout: 90_000 }, async (t) => {
    const { env, child } = await gatewayFixture(t);
    const paths = [env.PINVOU_REMOTE_CONTROLLER_STATE_PATH, `${env.PINVOU_REMOTE_CONTROLLER_STATE_PATH}.bak`,
      `${env.PINVOU_REMOTE_CONTROLLER_STATE_PATH}.lock`];
    const before = await Promise.all(paths.map((path) => readFile(path)));
    const result = await run(join(root, "scripts", "check-enrollment-gated-v2-rollback.mjs"), env);
    const output = result.code === 0 ? JSON.parse(result.stdout.trim()) : JSON.parse(result.stderr.trim());
    assert.equal(result.code, 0, `gate failed: ${output.code}`);
    assert.equal(output.safe_to_run_legacy_v2, true);
    assert.equal(output.migrated, 1);
    assert.equal(output.not_migrated, 0);
    assert.equal(output.coverage_percent, 100);
    assert.equal(child.exitCode, null);
    assert.deepEqual(await Promise.all(paths.map((path) => readFile(path))), before);
    assert.doesNotMatch(result.stdout + result.stderr, new RegExp(env.PINVOU_REMOTE_GATEWAY_CONTROL_KEY, "u"));
  });

test("rollback live gate: protected control rejects unsigned, wrong-key, wrong-store, replay and partial coverage",
  { timeout: 90_000 }, async (t) => {
    const { env } = await gatewayFixture(t);
    const url = `http://127.0.0.1:${env.PINVOU_REMOTE_GATEWAY_CONTROL_PORT}/coverage`;
    const unsigned = await fetch(url, { method: "POST", body: "{}" });
    assert.equal(unsigned.status, 403);
    assert.equal((await unsigned.json()).code, "coverage_authorization_failed");
    await assert.rejects(requestEnrollmentCoverage({ endpointIds: ["legacy-rollback-endpoint"],
      env: { ...env, PINVOU_REMOTE_GATEWAY_CONTROL_KEY: crypto.randomBytes(32).toString("hex") } }),
    { code: "coverage_response_unverified" });
    await assert.rejects(requestEnrollmentCoverage({ endpointIds: ["legacy-rollback-endpoint"],
      env: { ...env, PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(dirname(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), "other.json") } }),
    { code: "coverage_control_rejected" });
    const snapshot = await requestEnrollmentCoverage({ endpointIds: ["legacy-rollback-endpoint"], env });
    assert.equal(snapshot.coverage.migrated, 1);
    assert.equal(snapshot.namespace_revision, "1");
    let captured;
    await requestEnrollmentCoverage({ endpointIds: ["legacy-rollback-endpoint"], env,
      fetchImpl: (target, options) => { captured = { target, options }; return fetch(target, options); } });
    const replay = await fetch(captured.target, captured.options);
    assert.equal(replay.status, 400);
    assert.equal((await replay.json()).code, "coverage_request_replayed");
    const stale = JSON.parse(captured.options.body);
    stale.nonce = crypto.randomBytes(32).toString("hex");
    stale.issued_at_ms -= 60_000;
    const staleBytes = Buffer.from(JSON.stringify(stale));
    const signature = crypto.createHmac("sha256", env.PINVOU_REMOTE_GATEWAY_CONTROL_KEY)
      .update("pinvou-enrollment-coverage-request-v1\0").update(staleBytes).digest("hex");
    const staleResponse = await fetch(url, { method: "POST", body: staleBytes,
      headers: { "x-pinvou-coverage-signature": signature } });
    assert.equal(staleResponse.status, 400);
    assert.equal((await staleResponse.json()).code, "coverage_request_invalid");
    await writeFile(env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY, JSON.stringify({
      schema: "pinvou-legacy-endpoint-inventory", version: 1, endpoint_ids: ["legacy-rollback-endpoint", "missing-endpoint"],
    }));
    const incomplete = await run(join(root, "scripts", "check-enrollment-gated-v2-rollback.mjs"), env);
    assert.equal(incomplete.code, 1);
    assert.equal(JSON.parse(incomplete.stderr.trim()).code, "legacy_endpoint_coverage_incomplete");
    const leakCheck = incomplete.stdout + incomplete.stderr + JSON.stringify(snapshot);
    assert.ok(!leakCheck.includes(env.PINVOU_REMOTE_GATEWAY_CONTROL_KEY));
    assert.ok(!leakCheck.includes(env.PINVOU_REMOTE_ENROLLMENT_HMAC_KEY));
    assert.ok(!JSON.stringify(snapshot).includes("legacy-rollback-endpoint"));
  });

test("rollback live gate: authentic coverage cannot qualify a different public gateway instance",
  { timeout: 90_000 }, async (t) => {
    const { env } = await gatewayFixture(t);
    const snapshot = await requestEnrollmentCoverage({ endpointIds: ["legacy-rollback-endpoint"], env });
    const other = createEnrollmentGateway({
      registry: { authorizeWebRegistration: async () => { throw new Error("denied"); },
        authorizeDesktopRegistration: async () => { throw new Error("denied"); } },
      upstreamUrl: env.PINVOU_REMOTE_LEGACY_RELAY_URL,
    });
    const address = await other.listen({ host: "127.0.0.1", port: 0 });
    t.after(() => other.close());
    const otherEntry = `http://127.0.0.1:${address.port}/pinvou3/remote`;
    const result = await run(join(root, "scripts", "check-enrollment-gated-v2-rollback.mjs"),
      { ...env, PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: JSON.stringify([otherEntry]) });
    assert.equal(result.code, 1);
    assert.equal(JSON.parse(result.stderr.trim()).code, "gateway_instance_mismatch");
    // A wrongly split proxy may forward health to the correct gateway but WS
    // to another owner. Keep the real WS denial probe; only route the health
    // read explicitly to the real correct server for this split-route test.
    await assert.rejects(validateLegacyRollbackTopology({ publicEntrypoints: [otherEntry],
      legacyRelayUrl: env.PINVOU_REMOTE_LEGACY_RELAY_URL, legacyRelayBindHost: "127.0.0.1",
      expectedGatewayInstance: snapshot.gateway_instance_id,
      fetchImpl: () => fetch(`http://127.0.0.1:${env.PINVOU_REMOTE_GATEWAY_PORT}/pinvou3/remote/healthz`),
    }), { code: "gateway_websocket_route_unverified" });
  });

test("rollback control configuration is explicit, independent-key and literal-loopback only", () => {
  assert.equal(loadEnrollmentCoverageControlConfig({}), null);
  assert.throws(() => loadEnrollmentCoverageControlConfig({}, { required: true }), { code: "coverage_control_required" });
  const valid = { PINVOU_REMOTE_GATEWAY_CONTROL_HOST: "127.0.0.1", PINVOU_REMOTE_GATEWAY_CONTROL_PORT: "8791",
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: crypto.randomBytes(32).toString("hex") };
  for (const host of ["0.0.0.0", "::", "localhost", "198.51.100.1"]) {
    assert.throws(() => loadEnrollmentCoverageControlConfig({ ...valid, PINVOU_REMOTE_GATEWAY_CONTROL_HOST: host }),
      { code: "coverage_control_host_not_loopback" });
  }
  assert.throws(() => loadEnrollmentCoverageControlConfig({ ...valid,
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: valid.PINVOU_REMOTE_GATEWAY_CONTROL_KEY }), { code: "coverage_control_key_invalid" });
  assert.throws(() => loadEnrollmentCoverageControlConfig({ ...valid,
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: "short" }), { code: "coverage_control_key_invalid" });
});

test("rollback coverage client rejects tampered, cached-nonce and inconsistent signed responses", async () => {
  const env = { NODE_ENV: "test", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(tmpdir(), "controller-test", "state.json"),
    PINVOU_REMOTE_STATE_PATH: join(tmpdir(), "legacy-test.json"),
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "test-key", PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: crypto.randomBytes(32).toString("hex"),
    PINVOU_REMOTE_GATEWAY_CONTROL_HOST: "127.0.0.1", PINVOU_REMOTE_GATEWAY_CONTROL_PORT: "8791",
    PINVOU_REMOTE_GATEWAY_CONTROL_KEY: crypto.randomBytes(32).toString("hex") };
  for (const mutation of ["tamper", "nonce", "store", "revision", "coverage", "stale"]) {
    await assert.rejects(requestEnrollmentCoverage({ endpointIds: ["test-endpoint"], env,
      fetchImpl: async (_url, options) => {
        const request = JSON.parse(options.body);
        const result = { ok: true, schema: "pinvou-enrollment-coverage-response", version: 1,
          request_nonce: request.nonce, store_binding: request.store_binding, gateway_instance_id: "a".repeat(64),
          store_revision: "1", namespace_revision: "1", checked_at_ms: Date.now(),
          coverage: { total: 1, migrated: 1, web_migrated: 1, not_migrated: 0, coverage_percent: 100 } };
        if (mutation === "nonce") result.request_nonce = "b".repeat(64);
        if (mutation === "store") result.store_binding = "b".repeat(64);
        if (mutation === "revision") result.namespace_revision = "18446744073709551616";
        if (mutation === "coverage") result.coverage.web_migrated = 0;
        if (mutation === "stale") result.checked_at_ms -= 60_000;
        const contents = Buffer.from(JSON.stringify(result));
        const signature = crypto.createHmac("sha256", env.PINVOU_REMOTE_GATEWAY_CONTROL_KEY)
          .update("pinvou-enrollment-coverage-response-v1\0").update(contents).digest("hex");
        return new Response(mutation === "tamper" ? Buffer.from(JSON.stringify({ ...result, store_revision: "2" })) : contents,
          { status: 200, headers: { "x-pinvou-coverage-signature": signature } });
      },
    }), { code: mutation === "tamper" ? "coverage_response_unverified" : "coverage_response_invalid" });
  }
});
