import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import crypto from "node:crypto";
import { chmod, lstat, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { WebSocket, WebSocketServer } from "ws";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";
import { createEnrollmentGateway, validateLegacyRollbackTopology } from "../enrollment-gateway.js";
import {
  createOfficialEnrollmentProofVerifier,
  EnrollmentMigrationHandler,
  createEnrollmentBackup,
  restoreEnrollmentBackup,
  runEnrollmentBatch,
} from "../enrollment-migration-handler.js";
import { protectWindowsPath, RelayStateStore, verifyWindowsPath } from "../relay-state-store.js";
import { main as migrationCli } from "../scripts/enrollment-migrate.js";

const relayDir = dirname(dirname(fileURLToPath(import.meta.url)));
const ACCESS_A = "legacy-access-token-a-0000000000000000000001";
const ACCESS_B = "legacy-access-token-b-0000000000000000000002";
const ACCESS_C = "legacy-access-token-c-0000000000000000000003";
const DESKTOP_SECRET = "legacy-desktop-secret-000000000000000000001";

function officialIdentity() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  return {
    privateKey,
    verifier: createOfficialEnrollmentProofVerifier({
      publicKeys: [{ key_id: "official-test-key", public_key_pem: publicKey.export({ type: "spki", format: "pem" }) }],
    }),
  };
}

function signedEnrollmentProof({ privateKey, endpointId, accessToken, desktopSecret, challenge }) {
  const now = Date.now();
  const proof = {
    schema: "pinvou-official-endpoint-enrollment-proof",
    version: 1,
    key_id: "official-test-key",
    device_id: endpointId,
    endpoint_id: endpointId,
    access_token_sha256: crypto.createHash("sha256").update(accessToken, "utf8").digest("hex"),
    desktop_secret_sha256: crypto.createHash("sha256").update(desktopSecret, "utf8").digest("hex"),
    socket_challenge_b64: challenge,
    issued_at_ms: now - 1000,
    expires_at_ms: now + 60_000,
  };
  return {
    proof,
    signatureB64: crypto.sign(null, Buffer.from(JSON.stringify(proof), "utf8"), privateKey).toString("base64url"),
  };
}

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

function testStoreSecurityOptions() {
  if (process.platform !== "win32") return {};
  return {
    protectPath: async () => {},
    verifyPath: async () => {},
    verifyAncestors: async () => {},
  };
}

async function fixture(t, deployment = "official") {
  const root = await mkdtemp(join(tmpdir(), "pinvou-enrollment-migration-"));
  const env = environment(root, deployment);
  const config = loadEnrollmentRegistryConfig(env);
  const store = await RelayStateStore.open({
    statePath: config.controller_state_path,
    legacyStatePath: config.legacy_web_state_path,
    processIdentity: async (pid) => `migration-test-process:${pid}`,
    ...testStoreSecurityOptions(),
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
  const identity = officialIdentity();
  const handler = new EnrollmentMigrationHandler({ registry, identityVerifier: identity.verifier });
  const socket = {};
  const challenge = handler.issueClaimChallenge({ socket, endpointId: "official-endpoint-1" });
  const signed = signedEnrollmentProof({
    privateKey: identity.privateKey,
    endpointId: "official-endpoint-1",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: challenge.socket_challenge_b64,
  });
  const proof = handler.bindAuthenticatedLegacySocket({
    socket,
    endpointId: "official-endpoint-1",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: challenge.socket_challenge_b64,
    enrollmentProof: signed.proof,
    signatureB64: signed.signatureB64,
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

  const restartedHandler = new EnrollmentMigrationHandler({ registry, identityVerifier: identity.verifier });
  const restartedSocket = {};
  const restartedChallenge = restartedHandler.issueClaimChallenge({
    socket: restartedSocket,
    endpointId: "official-endpoint-1",
  });
  const restartedSigned = signedEnrollmentProof({
    privateKey: identity.privateKey,
    endpointId: "official-endpoint-1",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: restartedChallenge.socket_challenge_b64,
  });
  const restartedProof = restartedHandler.bindAuthenticatedLegacySocket({
    socket: restartedSocket,
    endpointId: "official-endpoint-1",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: restartedChallenge.socket_challenge_b64,
    enrollmentProof: restartedSigned.proof,
    signatureB64: restartedSigned.signatureB64,
  });
  const afterRestart = await restartedHandler.claim({
    socket: restartedSocket,
    proof: restartedProof,
    requestId: "request-0001",
  });
  assert.equal(afterRestart.revision, first.revision);
  assert.equal(afterRestart.legacy_web_link, "preserved");

  const otherSocket = {};
  const otherChallenge = handler.issueClaimChallenge({ socket: otherSocket, endpointId: "official-endpoint-2" });
  const otherSigned = signedEnrollmentProof({
    privateKey: identity.privateKey,
    endpointId: "official-endpoint-2",
    accessToken: ACCESS_B,
    desktopSecret: DESKTOP_SECRET,
    challenge: otherChallenge.socket_challenge_b64,
  });
  const otherProof = handler.bindAuthenticatedLegacySocket({
    socket: otherSocket,
    endpointId: "official-endpoint-2",
    accessToken: ACCESS_B,
    desktopSecret: DESKTOP_SECRET,
    challenge: otherChallenge.socket_challenge_b64,
    enrollmentProof: otherSigned.proof,
    signatureB64: otherSigned.signatureB64,
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

test("official migration rejects unsigned TOFU endpoints, replay and cross-endpoint proofs", async (t) => {
  const { registry } = await fixture(t);
  const identity = officialIdentity();
  const handler = new EnrollmentMigrationHandler({ registry, identityVerifier: identity.verifier });
  const socket = {};
  const challenge = handler.issueClaimChallenge({ socket, endpointId: "official-proof-a" });
  const signed = signedEnrollmentProof({
    privateKey: identity.privateKey,
    endpointId: "official-proof-a",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: challenge.socket_challenge_b64,
  });
  const tampered = { ...signed.proof, endpoint_id: "official-proof-b", device_id: "official-proof-b" };
  assert.throws(() => handler.bindAuthenticatedLegacySocket({
    socket,
    endpointId: "official-proof-a",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: challenge.socket_challenge_b64,
    enrollmentProof: tampered,
    signatureB64: signed.signatureB64,
  }), (error) => error?.code === "migration_identity_proof_invalid");

  assert.throws(() => handler.bindAuthenticatedLegacySocket({
    socket,
    endpointId: "official-proof-a",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: challenge.socket_challenge_b64,
    enrollmentProof: signed.proof,
    signatureB64: signed.signatureB64,
  }), (error) => error?.code === "migration_socket_challenge_invalid");

  const validChallenge = handler.issueClaimChallenge({ socket, endpointId: "official-proof-a" });
  const validSigned = signedEnrollmentProof({
    privateKey: identity.privateKey,
    endpointId: "official-proof-a",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: validChallenge.socket_challenge_b64,
  });

  const proof = handler.bindAuthenticatedLegacySocket({
    socket,
    endpointId: "official-proof-a",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: validChallenge.socket_challenge_b64,
    enrollmentProof: validSigned.proof,
    signatureB64: validSigned.signatureB64,
  });
  assert.throws(() => handler.bindAuthenticatedLegacySocket({
    socket,
    endpointId: "official-proof-a",
    accessToken: ACCESS_A,
    desktopSecret: DESKTOP_SECRET,
    challenge: validChallenge.socket_challenge_b64,
    enrollmentProof: validSigned.proof,
    signatureB64: validSigned.signatureB64,
  }), (error) => error?.code === "migration_socket_challenge_invalid");
  await handler.claim({ socket, proof, requestId: "official-proof-request-0001" });

  const attacker = crypto.generateKeyPairSync("ed25519");
  const attackerSocket = {};
  const attackerChallenge = handler.issueClaimChallenge({ socket: attackerSocket, endpointId: "anonymous-tofu" });
  const attackerSigned = signedEnrollmentProof({
    privateKey: attacker.privateKey,
    endpointId: "anonymous-tofu",
    accessToken: ACCESS_B,
    desktopSecret: DESKTOP_SECRET,
    challenge: attackerChallenge.socket_challenge_b64,
  });
  assert.throws(() => handler.bindAuthenticatedLegacySocket({
    socket: attackerSocket,
    endpointId: "anonymous-tofu",
    accessToken: ACCESS_B,
    desktopSecret: DESKTOP_SECRET,
    challenge: attackerChallenge.socket_challenge_b64,
    enrollmentProof: attackerSigned.proof,
    signatureB64: attackerSigned.signatureB64,
  }), (error) => error?.code === "migration_identity_proof_invalid");
});

test("self-hosted batch dry-run, apply, re-enroll and backup restore are explicit", async (t) => {
  const { registry } = await fixture(t, "self_hosted");
  const allowlist = [{ operation: "allowlist", endpoint_id: "self-hosted-1", credential: ACCESS_A }];
  const dryRun = await runEnrollmentBatch({
    registry, operations: allowlist, dryRun: true, requestId: "batch-allowlist-0001",
  });
  assert.equal(dryRun.applied, 0);
  assert.equal(registry.getStatus("self-hosted-1"), null);
  const applied = await runEnrollmentBatch({
    registry, operations: allowlist, dryRun: false, requestId: "batch-allowlist-0001",
  });
  assert.equal(applied.results[0].legacy_web_link, "preserved");
  assert.deepEqual(await runEnrollmentBatch({
    registry, operations: allowlist, dryRun: false, requestId: "batch-allowlist-0001",
  }), applied);
  await rejectsCode(runEnrollmentBatch({
    registry,
    operations: [{ operation: "allowlist", endpoint_id: "self-hosted-1", credential: ACCESS_B }],
    dryRun: false,
    requestId: "batch-allowlist-0001",
  }), "migration_request_conflict");
  const revision = registry.getStatus("self-hosted-1").revision;
  const reenrolled = await runEnrollmentBatch({
    registry,
    operations: [{
      operation: "re_enroll",
      endpoint_id: "self-hosted-1",
      current_credential: ACCESS_A,
      new_credential: ACCESS_B,
      expected_revision: revision,
    }],
    dryRun: false,
    requestId: "batch-reenroll-0001",
  });
  assert.equal(reenrolled.results[0].legacy_web_link, "invalidated");
  await rejectsCode(registry.authorizeRegistration({
    endpointId: "self-hosted-1", credential: ACCESS_A, purpose: "web_v2_reregister",
  }), "invalid_enrollment_credential");
  await registry.authorizeRegistration({
    endpointId: "self-hosted-1", credential: ACCESS_B, purpose: "web_v2_reregister",
  });
  await rejectsCode(runEnrollmentBatch({
    registry,
    operations: [{
      operation: "allowlist", endpoint_id: "strict-operation", credential: ACCESS_A, ignored: true,
    }],
    dryRun: true,
    requestId: "batch-strict-operation-0001",
  }), "migration_operation_invalid");

  const backup = createEnrollmentBackup(registry);
  const tampered = structuredClone(backup);
  tampered.registry_sha256 = "0".repeat(64);
  await rejectsCode(restoreEnrollmentBackup({
    registry,
    backup: tampered,
  }), "migration_backup_invalid");
  await registry.enroll({ endpointId: "temporary-endpoint", credential: ACCESS_C });
  await restoreEnrollmentBackup({ registry, backup, requestId: "restore-self-hosted-0001" });
  assert.equal(registry.getStatus("temporary-endpoint"), null);
});

test("a failing batch performs no prefix write", async (t) => {
  const { registry } = await fixture(t, "self_hosted");
  await registry.enroll({ endpointId: "batch-endpoint-2", credential: ACCESS_C });
  const before = registry.exportSnapshot();
  await rejectsCode(runEnrollmentBatch({
    registry,
    dryRun: false,
    requestId: "batch-failure-0001",
    operations: [
      { operation: "allowlist", endpoint_id: "batch-endpoint-1", credential: ACCESS_A },
      { operation: "allowlist", endpoint_id: "batch-endpoint-2", credential: ACCESS_B },
    ],
  }), "migration_batch_failed");
  assert.equal(registry.getStatus("batch-endpoint-1"), null);
  assert.equal(registry.getStatus("batch-endpoint-2").revision, "1");
  const after = registry.exportSnapshot();
  assert.deepEqual(after.registry.entries, before.registry.entries);
  assert.deepEqual(after.registry.migration_requests.map((request) => ({
    request_id: request.request_id,
    status: request.status,
    code: request.outcome?.code,
  })), [{
    request_id: "batch-failure-0001",
    status: "failed",
    code: "endpoint_already_enrolled",
  }]);
});

test("backup restore accepts canonical v1 and rejects malformed v2", async (t) => {
  const { registry } = await fixture(t, "self_hosted");
  await registry.enroll({ endpointId: "backup-endpoint", credential: ACCESS_A });
  const current = createEnrollmentBackup(registry);
  const v1Registry = {
    schema: current.registry.schema,
    version: 1,
    entries: structuredClone(current.registry.entries),
  };
  const v1Backup = {
    ...current,
    registry: v1Registry,
    registry_sha256: crypto.createHash("sha256").update(JSON.stringify(v1Registry), "utf8").digest("hex"),
  };
  await registry.enroll({ endpointId: "temporary-v1-endpoint", credential: ACCESS_B });
  await restoreEnrollmentBackup({ registry, backup: v1Backup, requestId: "restore-v1-backup-0001" });
  assert.equal(registry.getStatus("backup-endpoint").status, "active");
  assert.equal(registry.getStatus("temporary-v1-endpoint"), null);

  const malformed = structuredClone(createEnrollmentBackup(registry));
  malformed.registry.entries = [{}];
  malformed.registry_sha256 = crypto.createHash("sha256")
    .update(JSON.stringify(malformed.registry), "utf8").digest("hex");
  await rejectsCode(restoreEnrollmentBackup({
    registry,
    backup: malformed,
    requestId: "restore-malformed-0001",
  }), "migration_backup_invalid");
  assert.equal(registry.getStatus("backup-endpoint").status, "active");
});

test("self-hosted CLI is machine-readable, private-file only, and never prints credentials", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-enrollment-cli-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const input = join(root, "batch.json");
  await writeFile(input, JSON.stringify({
    schema: "pinvou-enrollment-migration-batch",
    version: 1,
    request_id: "cli-migration-request-0001",
    operations: [{ operation: "allowlist", endpoint_id: "cli-endpoint", credential: ACCESS_A }],
  }), { mode: 0o600 });
  const output = [];
  const errors = [];
  const io = { log: (value) => output.push(value), error: (value) => errors.push(value) };
  if (process.platform === "win32") {
    assert.equal(await migrationCli(["--dry-run", "--input", input], environment(root, "self_hosted"), io), 1);
    assert.equal(JSON.parse(errors.pop()).code, "input_permissions_unsafe");
    await protectWindowsPath(input, "file");
  } else {
    await chmod(input, 0o644);
    assert.equal(await migrationCli(["--dry-run", "--input", input], environment(root, "self_hosted"), io), 1);
    assert.equal(JSON.parse(errors.pop()).code, "input_permissions_unsafe");
    await chmod(input, 0o600);
  }
  assert.equal(await migrationCli(["--dry-run", "--input", input], environment(root, "self_hosted"), io), 0);
  assert.equal(JSON.parse(output[0]).dry_run, true);
  assert.equal(errors.length, 0);
  assert.doesNotMatch(`${output.join("\n")}\n${errors.join("\n")}`, new RegExp(ACCESS_A, "u"));
  assert.equal(await migrationCli(["--apply", "--input", input], environment(root, "official"), io), 1);
  assert.equal(JSON.parse(errors.at(-1)).code, "self_hosted_mode_required");

  const backup = join(root, "enrollment-backup.json");
  assert.equal(await migrationCli(["--backup", "--output", backup], environment(root, "self_hosted"), io), 0);
  if (process.platform === "win32") await verifyWindowsPath(backup, "file");
  else assert.equal((await lstat(backup)).mode & 0o077, 0);
});

// DEV-02 审核修复：回滚门禁的验收是“未满足门禁时自动失败”，但此前测试只做了
// check-enrollment-gated-v2-rollback.mjs 的源码字符串断言，脚本行为从未被执行。
// 下面直接以子进程运行门禁脚本，覆盖两条必须失败的路径。
async function runRollbackGate(env) {
  const repoRoot = dirname(relayDir);
  return new Promise((resolve) => {
    const child = spawn(
      process.execPath,
      [join(repoRoot, "scripts", "check-enrollment-gated-v2-rollback.mjs")],
      { cwd: repoRoot, env, shell: false, windowsHide: true, stdio: ["ignore", "pipe", "pipe"] },
    );
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += String(chunk); });
    child.stderr.on("data", (chunk) => { stderr += String(chunk); });
    child.once("error", (error) => resolve({ code: null, stdout, stderr: `${stderr}${error?.message}` }));
    child.once("close", (code) => resolve({ code, stdout, stderr }));
  });
}

test("rollback gate refuses without an inventory and never reports safe", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-rollback-gate-"));
  t.after(async () => { await rm(root, { recursive: true, force: true }); });
  const result = await runRollbackGate({
    ...process.env,
    ...environment(root, "self_hosted"),
    PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY: "",
  });
  assert.equal(result.code, 1);
  const failure = JSON.parse(result.stderr.trim().split(/\r?\n/u).at(-1));
  assert.equal(failure.ok, false);
  assert.equal(failure.safe_to_run_legacy_v2, false);
  assert.equal(failure.code, "legacy_endpoint_inventory_required");
  assert.doesNotMatch(result.stdout, /safe_to_run_legacy_v2/u);
});

test("rollback gate fails closed when its coverage input is unusable", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-rollback-gate-"));
  t.after(async () => { await rm(root, { recursive: true, force: true }); });
  const inventory = join(root, "inventory.json");
  await writeFile(
    inventory,
    JSON.stringify({ schema: "not-a-legacy-inventory", version: 1, endpoint_ids: [] }),
    "utf8",
  );
  if (process.platform === "win32") await protectWindowsPath(inventory, "file");
  else await chmod(inventory, 0o600);
  const result = await runRollbackGate({
    ...process.env,
    ...environment(root, "self_hosted"),
    PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY: inventory,
  });
  assert.equal(result.code, 1);
  const failure = JSON.parse(result.stderr.trim().split(/\r?\n/u).at(-1));
  assert.equal(failure.ok, false);
  assert.equal(failure.safe_to_run_legacy_v2, false);
  // Both codes are fail-closed for the same reason: the coverage step refused
  // before the gateway preflight could ever report success.
  assert.ok(
    ["inventory_document_invalid", "input_permissions_unsafe"].includes(failure.code),
    `unexpected gate failure code: ${failure.code}`,
  );
  assert.doesNotMatch(result.stdout, /safe_to_run_legacy_v2/u);
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

function openSocket(url, options) {
  return new Promise((resolve, reject) => {
    const { protocol, ...socketOptions } = options || {};
    const socket = new WebSocket(url, protocol, socketOptions);
    socket.once("open", () => resolve(socket));
    socket.once("error", reject);
  });
}

function socketClosed(socket) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("close timeout")), 5000);
    socket.once("close", (code) => {
      clearTimeout(timer);
      resolve(code);
    });
    socket.once("error", () => {});
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

function nextMessageMatching(socket, predicate) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      socket.off("message", onMessage);
      reject(new Error("matching message timeout"));
    }, 5000);
    const onMessage = (data) => {
      const message = JSON.parse(data.toString("utf8"));
      if (!predicate(message)) return;
      clearTimeout(timer);
      socket.off("message", onMessage);
      resolve(message);
    };
    socket.on("message", onMessage);
  });
}

test("gateway does not echo controller protocol absent upstream and rejects mixed versions", async (t) => {
  const received = [];
  const oldServer = http.createServer();
  const oldWss = new WebSocketServer({ server: oldServer, handleProtocols: () => false });
  oldWss.on("connection", (socket) => {
    socket.on("message", (raw) => {
      const message = String(raw);
      received.push(message);
      if (message.includes('"request_id":"downstream-mixed"')) {
        socket.send('{"v":1,"v":2,"type":"desktop_endpoint_registered"}');
      } else if (message.includes('"request_id":"downstream-escaped"')) {
        socket.send('{"v":2,"\\u0076":1,"type":"desktop_endpoint_registered"}');
      } else {
        // Legacy v2 ordinary duplicate fields remain accepted downstream.
        socket.send('{"v":2,"type":"desktop_endpoint_registered",'
          + '"endpoint_id":"old-endpoint","endpoint_id":"old-endpoint"}');
      }
    });
  });
  await new Promise((resolve) => oldServer.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    for (const socket of oldWss.clients) socket.terminate();
    await new Promise((resolve) => oldServer.close(resolve));
  });
  const oldPort = oldServer.address().port;
  const gateway = createEnrollmentGateway({
    registry: { async authorizeRegistration() {} },
    upstreamUrl: `ws://127.0.0.1:${oldPort}/pinvou3/remote/ws`,
  });
  const address = await gateway.listen({ host: "127.0.0.1", port: 0 });
  t.after(() => gateway.close());
  const gatewayWs = `ws://127.0.0.1:${address.port}/pinvou3/remote/ws`;

  await assert.rejects(openSocket(gatewayWs, { protocol: "pinvou-desktop-controller-v1" }),
    /Server sent no subprotocol/u);
  const mixed = await openSocket(gatewayWs);
  t.after(() => mixed.terminate());
  const denied = nextMessage(mixed);
  const mixedClosed = socketClosed(mixed);
  mixed.send('{"v":1,"v":2,"type":"desktop_endpoint_register",'
    + '"endpoint_id":"old-endpoint","access_token":"test-token"}');
  assert.equal((await denied).code, "endpoint_not_found");
  assert.equal(await mixedClosed, 1008);
  const escaped = await openSocket(gatewayWs);
  t.after(() => escaped.terminate());
  const escapedDenied = nextMessage(escaped);
  const escapedClosed = socketClosed(escaped);
  escaped.send('{"v":1,"\\u0076":2,"type":"desktop_endpoint_register",'
    + '"endpoint_id":"old-endpoint","access_token":"test-token"}');
  assert.equal((await escapedDenied).code, "endpoint_not_found");
  assert.equal(await escapedClosed, 1008);
  assert.deepEqual(received, []);

  const legacy = await openSocket(gatewayWs);
  t.after(() => legacy.terminate());
  const registered = nextMessage(legacy);
  const duplicateOrdinaryField = '{"v":2,"type":"desktop_endpoint_register",'
    + '"endpoint_id":"old-endpoint","endpoint_id":"old-endpoint",'
    + '"access_token":"test-token"}';
  legacy.send(duplicateOrdinaryField);
  assert.equal((await registered).type, "desktop_endpoint_registered");
  assert.deepEqual(received, [duplicateOrdinaryField]);
  const subsequentDenied = nextMessage(legacy);
  const legacyClosed = socketClosed(legacy);
  legacy.send('{"v":1,"v":2,"type":"rpc_request"}');
  assert.equal((await subsequentDenied).code, "endpoint_not_found");
  assert.equal(await legacyClosed, 1008);
  assert.deepEqual(received, [duplicateOrdinaryField]);

  for (const requestId of ["downstream-mixed", "downstream-escaped"]) {
    const downstream = await openSocket(gatewayWs);
    t.after(() => downstream.terminate());
    const ordinaryReply = nextMessage(downstream);
    downstream.send('{"v":2,"type":"desktop_endpoint_register",'
      + '"endpoint_id":"old-endpoint","access_token":"test-token"}');
    assert.equal((await ordinaryReply).type, "desktop_endpoint_registered");
    const rejectedReply = nextMessage(downstream);
    const downstreamClosed = socketClosed(downstream);
    downstream.send(JSON.stringify({ v: 2, type: "rpc_request", request_id: requestId }));
    assert.equal((await rejectedReply).code, "endpoint_not_found");
    assert.equal(await downstreamClosed, 1008);
    assert.equal(received.at(-1).includes(requestId), true);
  }
  for (let attempt = 0; attempt < 50 && oldWss.clients.size > 0; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.equal(oldWss.clients.size, 0);
});

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

  const gateway = createEnrollmentGateway({
    registry,
    upstreamUrl: legacyWs,
    allowedWebOrigins: ["https://allowed.example"],
  });
  assert.throws(() => createEnrollmentGateway({
    registry,
    upstreamUrl: "wss://relay.public.example/pinvou3/remote/ws",
  }), (error) => error?.code === "gateway_upstream_not_loopback");
  const address = await gateway.listen({ host: "127.0.0.1", port: 0 });
  t.after(() => gateway.close());
  const gatewayHttp = `http://127.0.0.1:${address.port}/pinvou3/remote`;
  const gatewayWs = `ws://127.0.0.1:${address.port}/pinvou3/remote/ws`;

  await assert.rejects(openSocket(gatewayWs, { origin: "https://evil.example" }));
  await assert.rejects(openSocket(gatewayWs, { origin: new URL(gatewayHttp).origin }));
  const allowedOrigin = await openSocket(gatewayWs, { origin: "https://allowed.example" });
  allowedOrigin.terminate();
  await assert.rejects(openSocket(gatewayWs, { headers: { "x-forwarded-for": "198.51.100.8" } }));

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

  const forwarded = nextMessageMatching(desktop, (message) => message.id === "large-valid-frame");
  web.send(JSON.stringify({
    v: 2,
    type: "rpc_request",
    id: "large-valid-frame",
    method: "test.large",
    args: { payload: "x".repeat(1024 * 1024) },
  }));
  assert.equal((await forwarded).args.payload.length, 1024 * 1024);

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

  const oversizedFirstFrame = await openSocket(gatewayWs);
  const oversizedDenied = nextMessage(oversizedFirstFrame);
  const oversizedClosed = socketClosed(oversizedFirstFrame);
  oversizedFirstFrame.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: "legacy-preserved-endpoint",
    access_token: ACCESS_A,
    desktop_secret: DESKTOP_SECRET,
    padding: "x".repeat(20 * 1024),
  }));
  assert.equal((await oversizedDenied).code, "endpoint_not_found");
  assert.equal(await oversizedClosed, 1008);

  const payloadOverflow = await openSocket(gatewayWs);
  const payloadClosed = socketClosed(payloadOverflow);
  payloadOverflow.send("x".repeat(4 * 1024 * 1024 + 1));
  assert.equal(await payloadClosed, 1009);
  assert.equal((await fetch(`${gatewayHttp}/healthz`)).ok, true);

  // B reuses its v2 desktop endpoint for controller traffic. The gateway
  // must preserve the negotiated protocol on both sides of its proxy hop.
  const controllerDesktop = await openSocket(gatewayWs,
    { protocol: "pinvou-desktop-controller-v1" });
  t.after(() => controllerDesktop.terminate());
  assert.equal(controllerDesktop.protocol, "pinvou-desktop-controller-v1");
  const controllerRegistered = nextMessage(controllerDesktop);
  controllerDesktop.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: "legacy-preserved-endpoint",
    access_token: ACCESS_A,
    desktop_secret: DESKTOP_SECRET,
  }));
  assert.equal((await controllerRegistered).type, "desktop_endpoint_registered");
  // Registering a replacement desktop can race with an ordinary v2 stream
  // notification. Match the response to this v1 probe explicitly instead of
  // treating an unrelated queued v2 frame as the probe result.
  const controllerDisabled = nextMessageMatching(controllerDesktop,
    (message) => message.code === "desktop_controller_unavailable");
  controllerDesktop.send(JSON.stringify({ v: 1, type: "controller_capabilities" }));
  assert.equal((await controllerDisabled).code, "desktop_controller_unavailable");

  const constrained = createEnrollmentGateway({
    registry,
    upstreamUrl: legacyWs,
    trustedProxyIps: ["127.0.0.1"],
    connectionLimit: 10,
    maxSourceBuckets: 1,
  });
  const constrainedAddress = await constrained.listen({ host: "127.0.0.1", port: 0 });
  t.after(() => constrained.close());
  const constrainedWs = `ws://127.0.0.1:${constrainedAddress.port}/pinvou3/remote/ws`;
  const trustedSource = await openSocket(constrainedWs, {
    headers: { "x-forwarded-for": "198.51.100.10" },
  });
  t.after(() => trustedSource.terminate());
  await assert.rejects(openSocket(constrainedWs, {
    headers: { "x-forwarded-for": "198.51.100.11" },
  }));
  await assert.rejects(openSocket(constrainedWs, {
    headers: { "x-forwarded-for": "198.51.100.10, 198.51.100.11" },
  }));

  assert.doesNotThrow(() => createEnrollmentGateway({
    registry,
    upstreamUrl: "ws://[::1]:8787/pinvou3/remote/ws",
  }));

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
  await rejectsCode(validateLegacyRollbackTopology({
    publicEntrypoints: [gatewayHttp],
    legacyRelayUrl: legacyWs,
    legacyRelayBindHost: "127.0.0.1",
    listenerInspector: async () => ["0.0.0.0"],
  }), "gateway_legacy_listener_exposed");
  await rejectsCode(validateLegacyRollbackTopology({
    publicEntrypoints: [gatewayHttp],
    legacyRelayUrl: `ws://localhost:${relayPort}/pinvou3/remote/ws`,
    legacyRelayBindHost: "127.0.0.1",
  }), "gateway_direct_bypass_exposed");
  await rejectsCode(validateLegacyRollbackTopology({
    publicEntrypoints: [gatewayHttp],
    legacyRelayUrl: `http://127.0.0.1:${relayPort}/pinvou3/remote/ws`,
    legacyRelayBindHost: "127.0.0.1",
  }), "gateway_upstream_invalid");
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
