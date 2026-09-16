import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { WebSocket } from "ws";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";
import { createEnrollmentGateway, validateLegacyRollbackTopology } from "../enrollment-gateway.js";
import {
  EnrollmentMigrationHandler,
  createEnrollmentBackup,
  restoreEnrollmentBackup,
  runEnrollmentBatch,
} from "../enrollment-migration-handler.js";
import { RelayStateStore } from "../relay-state-store.js";
import { main as migrationCli } from "../scripts/enrollment-migrate.js";

const relayDir = dirname(dirname(fileURLToPath(import.meta.url)));
const ACCESS_A = "legacy-access-token-a-0000000000000000000001";
const ACCESS_B = "legacy-access-token-b-0000000000000000000002";
const ACCESS_C = "legacy-access-token-c-0000000000000000000003";
const DESKTOP_SECRET = "legacy-desktop-secret-000000000000000000001";

function environment(root, deployment = "official") {
  return {
    NODE_ENV: "test",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: deployment,
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "0",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "migration-test-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "migration-test-hmac-material-at-least-32-bytes",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(root, "controller", "controller-state.json"),
    PINVOU_REMOTE_STATE_PATH: join(root, "legacy-v2-state.json"),
  };
}

async function fixture(t, deployment = "official") {
  const root = await mkdtemp(join(tmpdir(), "pinvou-enrollment-migration-"));
  const env = environment(root, deployment);
  const config = loadEnrollmentRegistryConfig(env);
  const store = await RelayStateStore.open({
    statePath: config.controller_state_path,
    legacyStatePath: config.legacy_web_state_path,
  });
  t.after(async () => {
    await store.close();
    await rm(root, { recursive: true, force: true });
  });
  return { root, env, store, registry: EnrollmentRegistry.open({ stateStore: store, config }) };
}

async function rejectsCode(promise, code) {
  await assert.rejects(promise, (error) => error?.code === code);
}

test("official claim is socket-bound, stable, conflicting-safe and keeps the Web token", async (t) => {
  const { store, registry } = await fixture(t);
  const handler = new EnrollmentMigrationHandler({ registry });
  const socket = {};
  const proof = handler.bindAuthenticatedLegacySocket({
    socket,
    endpointId: "official-endpoint-1",
    accessToken: ACCESS_A,
  });
  await rejectsCode(handler.claim({ socket: {}, proof, requestId: "request-0001" }), "migration_socket_proof_invalid");
  const first = await handler.claim({ socket, proof, requestId: "request-0001" });
  const retry = await handler.claim({ socket, proof, requestId: "request-0001" });
  assert.deepEqual(retry, first);
  assert.equal(first.legacy_web_link, "preserved");
  await registry.authorizeRegistration({
    endpointId: "official-endpoint-1",
    credential: ACCESS_A,
    purpose: "web_v2_reregister",
  });

  const restartedHandler = new EnrollmentMigrationHandler({ registry });
  const restartedSocket = {};
  const restartedProof = restartedHandler.bindAuthenticatedLegacySocket({
    socket: restartedSocket,
    endpointId: "official-endpoint-1",
    accessToken: ACCESS_A,
  });
  const afterRestart = await restartedHandler.claim({
    socket: restartedSocket,
    proof: restartedProof,
    requestId: "request-0001",
  });
  assert.equal(afterRestart.revision, first.revision);
  assert.equal(afterRestart.legacy_web_link, "preserved");

  const otherSocket = {};
  const otherProof = handler.bindAuthenticatedLegacySocket({
    socket: otherSocket,
    endpointId: "official-endpoint-2",
    accessToken: ACCESS_B,
  });
  await rejectsCode(handler.claim({ socket: otherSocket, proof: otherProof, requestId: "request-0001" }), "migration_request_conflict");
  assert.deepEqual(handler.coverage(["official-endpoint-2", "official-endpoint-1"]), {
    total: 2,
    migrated: 1,
    not_migrated: 1,
    coverage_percent: 50,
    endpoints: [
      { endpoint_id: "official-endpoint-1", enrollment_status: "active" },
      { endpoint_id: "official-endpoint-2", enrollment_status: "not_migrated" },
    ],
  });
  assert.doesNotMatch(JSON.stringify(store.readNamespace("endpoint_enrollment")), new RegExp(ACCESS_A, "u"));
});

test("self-hosted batch dry-run, apply, re-enroll and backup restore are explicit", async (t) => {
  const { store, registry } = await fixture(t, "self_hosted");
  const allowlist = [{ operation: "allowlist", endpoint_id: "self-hosted-1", credential: ACCESS_A }];
  const dryRun = await runEnrollmentBatch({ registry, stateStore: store, operations: allowlist, dryRun: true });
  assert.equal(dryRun.applied, 0);
  assert.equal(registry.getStatus("self-hosted-1"), null);
  const applied = await runEnrollmentBatch({ registry, stateStore: store, operations: allowlist, dryRun: false });
  assert.equal(applied.results[0].legacy_web_link, "preserved");
  const revision = registry.getStatus("self-hosted-1").revision;
  const reenrolled = await runEnrollmentBatch({
    registry,
    stateStore: store,
    operations: [{
      operation: "re_enroll",
      endpoint_id: "self-hosted-1",
      current_credential: ACCESS_A,
      new_credential: ACCESS_B,
      expected_revision: revision,
    }],
    dryRun: false,
  });
  assert.equal(reenrolled.results[0].legacy_web_link, "invalidated");
  await rejectsCode(registry.authorizeRegistration({
    endpointId: "self-hosted-1", credential: ACCESS_A, purpose: "web_v2_reregister",
  }), "invalid_enrollment_credential");
  await registry.authorizeRegistration({
    endpointId: "self-hosted-1", credential: ACCESS_B, purpose: "web_v2_reregister",
  });

  const backup = createEnrollmentBackup(store);
  const tampered = structuredClone(backup);
  tampered.registry_sha256 = "0".repeat(64);
  await rejectsCode(restoreEnrollmentBackup({
    stateStore: store,
    backup: tampered,
    expectedCurrentRevision: store.readNamespace("endpoint_enrollment").namespace_revision,
  }), "migration_backup_invalid");
  await registry.enroll({ endpointId: "temporary-endpoint", credential: ACCESS_C });
  const current = store.readNamespace("endpoint_enrollment");
  await restoreEnrollmentBackup({ stateStore: store, backup, expectedCurrentRevision: current.namespace_revision });
  assert.equal(registry.getStatus("temporary-endpoint"), null);
});

test("partial batch failure restores the registry atomically", async (t) => {
  const { store, registry } = await fixture(t, "self_hosted");
  let writes = 0;
  const failingRegistry = {
    getStatus: registry.getStatus.bind(registry),
    authorizeRegistration: registry.authorizeRegistration.bind(registry),
    rotateCredential: registry.rotateCredential.bind(registry),
    async enroll(input) {
      writes += 1;
      if (writes === 2) {
        const error = new Error("injected");
        error.code = "injected_failure";
        throw error;
      }
      return registry.enroll(input);
    },
  };
  await rejectsCode(runEnrollmentBatch({
    registry: failingRegistry,
    stateStore: store,
    dryRun: false,
    operations: [
      { operation: "allowlist", endpoint_id: "batch-endpoint-1", credential: ACCESS_A },
      { operation: "allowlist", endpoint_id: "batch-endpoint-2", credential: ACCESS_B },
    ],
  }), "migration_batch_rolled_back");
  assert.equal(registry.getStatus("batch-endpoint-1"), null);
  assert.equal(registry.getStatus("batch-endpoint-2"), null);
});

test("self-hosted CLI is machine-readable, private-file only, and never prints credentials", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-enrollment-cli-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const input = join(root, "batch.json");
  await writeFile(input, JSON.stringify({
    schema: "pinvou-enrollment-migration-batch",
    version: 1,
    operations: [{ operation: "allowlist", endpoint_id: "cli-endpoint", credential: ACCESS_A }],
  }), { mode: 0o600 });
  if (process.platform !== "win32") await chmod(input, 0o600);
  const output = [];
  const errors = [];
  const io = { log: (value) => output.push(value), error: (value) => errors.push(value) };
  assert.equal(await migrationCli(["--dry-run", "--input", input], environment(root, "self_hosted"), io), 0);
  assert.equal(JSON.parse(output[0]).dry_run, true);
  assert.equal(errors.length, 0);
  assert.doesNotMatch(`${output.join("\n")}\n${errors.join("\n")}`, new RegExp(ACCESS_A, "u"));
  assert.equal(await migrationCli(["--apply", "--input", input], environment(root, "official"), io), 1);
  assert.equal(JSON.parse(errors.at(-1)).code, "self_hosted_mode_required");
});

function freePort() {
  return new Promise((resolve, reject) => {
    const server = http.createServer();
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      server.close((error) => error ? reject(error) : resolve(port));
    });
  });
}

async function waitForHealth(url, child) {
  for (let attempt = 0; attempt < 80; attempt += 1) {
    if (child.exitCode !== null) throw new Error(`legacy relay exited: ${child.exitCode}`);
    try {
      const response = await fetch(url);
      if (response.ok) return;
    } catch {}
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error("legacy relay did not become ready");
}

function openSocket(url) {
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(url);
    socket.once("open", () => resolve(socket));
    socket.once("error", reject);
  });
}

function nextMessage(socket) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("message timeout")), 5000);
    socket.once("message", (data) => {
      clearTimeout(timer);
      resolve(JSON.parse(data.toString("utf8")));
    });
  });
}

test("real legacy v2 Relay is safe only behind the enrollment gateway", async (t) => {
  const { root, registry } = await fixture(t);
  const relayPort = await freePort();
  const legacyHttp = `http://127.0.0.1:${relayPort}/pinvou3/remote`;
  const legacyWs = `ws://127.0.0.1:${relayPort}/pinvou3/remote/ws`;
  const child = spawn(process.execPath, [join(relayDir, "server.js")], {
    cwd: relayDir,
    env: {
      ...process.env,
      PORT: String(relayPort),
      PINVOU_REMOTE_HOST: "127.0.0.1",
      PINVOU_REMOTE_STATE_PATH: join(root, "legacy-runtime-state.json"),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  t.after(() => { if (child.exitCode === null) child.kill(); });
  await waitForHealth(`${legacyHttp}/healthz`, child);

  const bootstrap = await openSocket(legacyWs);
  const bootstrapReply = nextMessage(bootstrap);
  bootstrap.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: "legacy-preserved-endpoint",
    access_token: ACCESS_A,
    desktop_secret: DESKTOP_SECRET,
  }));
  assert.equal((await bootstrapReply).type, "desktop_endpoint_registered");
  bootstrap.close();
  await registry.enroll({ endpointId: "legacy-preserved-endpoint", credential: ACCESS_A });

  const gateway = createEnrollmentGateway({ registry, upstreamUrl: legacyWs });
  assert.throws(() => createEnrollmentGateway({
    registry,
    upstreamUrl: "wss://relay.public.example/pinvou3/remote/ws",
  }), (error) => error?.code === "gateway_upstream_not_loopback");
  const address = await gateway.listen({ host: "127.0.0.1", port: 0 });
  t.after(() => gateway.close());
  const gatewayHttp = `http://127.0.0.1:${address.port}/pinvou3/remote`;
  const gatewayWs = `ws://127.0.0.1:${address.port}/pinvou3/remote/ws`;

  const desktop = await openSocket(gatewayWs);
  t.after(() => desktop.terminate());
  const registered = nextMessage(desktop);
  desktop.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: "legacy-preserved-endpoint",
    access_token: ACCESS_A,
    desktop_secret: DESKTOP_SECRET,
  }));
  assert.equal((await registered).type, "desktop_endpoint_registered");

  const web = await openSocket(gatewayWs);
  t.after(() => web.terminate());
  const joined = nextMessage(web);
  web.send(JSON.stringify({
    v: 2,
    type: "web_client_join",
    endpoint_id: "legacy-preserved-endpoint",
    access_token: ACCESS_A,
    stream_epoch: null,
    after_seq: 0,
  }));
  assert.equal((await joined).type, "web_client_joined");

  const anonymous = await openSocket(gatewayWs);
  t.after(() => anonymous.terminate());
  const denied = nextMessage(anonymous);
  anonymous.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: "anonymous-endpoint",
    access_token: ACCESS_B,
    desktop_secret: DESKTOP_SECRET,
  }));
  assert.equal((await denied).code, "endpoint_not_found");

  const safe = await validateLegacyRollbackTopology({
    publicEntrypoints: [gatewayHttp],
    legacyRelayUrl: legacyWs,
    legacyRelayBindHost: "127.0.0.1",
  });
  assert.equal(safe.safe_to_run_legacy_v2, true);
  await rejectsCode(validateLegacyRollbackTopology({
    publicEntrypoints: [gatewayHttp],
    legacyRelayUrl: "ws://relay.public.example/pinvou3/remote",
    legacyRelayBindHost: "127.0.0.1",
  }), "gateway_direct_bypass_exposed");
  await rejectsCode(validateLegacyRollbackTopology({
    publicEntrypoints: [legacyHttp],
    legacyRelayUrl: legacyWs,
    legacyRelayBindHost: "127.0.0.1",
  }), "gateway_preflight_unverified");
  await rejectsCode(validateLegacyRollbackTopology({
    publicEntrypoints: ["http://127.0.0.1:1/pinvou3/remote"],
    legacyRelayUrl: legacyWs,
    legacyRelayBindHost: "127.0.0.1",
  }), "gateway_preflight_unavailable");
  await rejectsCode(validateLegacyRollbackTopology({
    publicEntrypoints: [gatewayHttp],
    legacyRelayUrl: legacyWs,
    legacyRelayBindHost: "0.0.0.0",
  }), "gateway_legacy_bind_not_loopback");
});

test("deployment artifacts force gateway coverage and contain no credential interpolation", async () => {
  const nginx = await readFile(join(relayDir, "nginx-enrollment-gateway.conf"), "utf8");
  const cli = await readFile(join(relayDir, "scripts", "enrollment-migrate.js"), "utf8");
  const deployGate = await readFile(join(dirname(relayDir), "scripts", "check-enrollment-gated-v2-rollback.mjs"), "utf8");
  assert.match(nginx, /proxy_pass http:\/\/127\.0\.0\.1:8790/u);
  assert.doesNotMatch(nginx, /proxy_pass\s+[^;]*8787/u);
  assert.match(cli, /credentials are accepted only in a mode-0600 input file/u);
  assert.doesNotMatch(cli, /execFile|spawn\(|shell\s*:/u);
  assert.match(deployGate, /safe_to_run_legacy_v2/u);
  assert.match(deployGate, /not_migrated/u);
  assert.match(deployGate, /shell: false/u);
});
