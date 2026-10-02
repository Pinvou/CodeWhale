"use strict";

import assert from "node:assert/strict";
import crypto from "node:crypto";
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import WebSocket from "ws";

import { sealPairingCode } from "../pairing-code-sealer.js";
import { PairingStore } from "../pairing-store.js";
import { validateControllerMessage } from "../protocol-controller.js";
import { RelayStateStore } from "../relay-state-store.js";
import { ControllerSensitiveScanner } from "../controller-sensitive-scan.js";
import { ControllerTelemetry } from "../controller-telemetry.js";

const SENSITIVE_VALUES = Object.freeze({
  code: "2222222",
  endpoint: "endpointSentinelA1",
  lease: "leaseSentinelB2",
  token: "tokenSentinelC3-credential-000001",
  key: "keySentinelD4-credential-00000001",
  ip: "198.51.100.77",
  device_name: "deviceSentinelE5",
  prompt: "promptSentinelF6",
  reply: "replySentinelG7",
  tool_arguments: "toolArgsSentinelH8",
  file_name: "fileSentinelI9.txt",
  path: "/private/sentinel/J0",
  content: "contentSentinelK1",
});
const SENTINELS = Object.freeze(Object.values(SENSITIVE_VALUES));
const relayDir = dirname(dirname(fileURLToPath(import.meta.url)));

function freePort() {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.unref();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      server.close(() => address && typeof address === "object"
        ? resolve(address.port)
        : reject(new Error("scan_port_unavailable")));
    });
  });
}

function nextMessage(socket, type) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("scan_message_timeout")), 5000);
    const onMessage = (bytes) => {
      const message = JSON.parse(String(bytes));
      if (message.type !== type) return;
      clearTimeout(timer);
      socket.off("message", onMessage);
      resolve(message);
    };
    socket.on("message", onMessage);
  });
}

async function collectRealRelayRevokeSurface(directory) {
  const port = await freePort();
  const webDir = join(directory, "web");
  const blockedParent = join(directory, "blocked-state-parent");
  const statePath = join(blockedParent, "relay-state.json");
  await mkdir(webDir, { recursive: true });
  await writeFile(join(webDir, "index.html"), "<!doctype html><title>scan</title>");
  const child = spawn(process.execPath, [join(relayDir, "server.js")], {
    cwd: relayDir,
    env: {
      ...process.env,
      PORT: String(port),
      PINVOU_REMOTE_PUBLIC_BASE_PATH: "/pinvou3/remote",
      PINVOU_REMOTE_WEB_DIR: webDir,
      PINVOU_REMOTE_STATE_PATH: statePath,
      HEARTBEAT_INTERVAL_MS: "5000",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => { stdout += String(chunk); });
  child.stderr.on("data", (chunk) => { stderr += String(chunk); });
  try {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("scan_relay_start_timeout")), 10000);
      const ready = (chunk) => {
        if (!String(chunk).includes("pinvou remote relay listening")) return;
        clearTimeout(timer);
        child.stdout.off("data", ready);
        resolve();
      };
      child.stdout.on("data", ready);
      child.once("exit", () => reject(new Error("scan_relay_exited")));
    });
    const socket = await new Promise((resolve, reject) => {
      const candidate = new WebSocket(`ws://127.0.0.1:${port}/pinvou3/remote/ws`);
      candidate.once("open", () => resolve(candidate));
      candidate.once("error", reject);
    });
    const registered = nextMessage(socket, "desktop_endpoint_registered");
    socket.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_register",
      endpoint_id: SENSITIVE_VALUES.endpoint,
      access_token: SENSITIVE_VALUES.token,
      desktop_secret: SENSITIVE_VALUES.key,
    }));
    await registered;
    await writeFile(blockedParent, "not-a-directory");
    const rejected = nextMessage(socket, "error");
    socket.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: SENSITIVE_VALUES.endpoint,
      desktop_secret: SENSITIVE_VALUES.key,
      reason: SENTINELS.join("|"),
    }));
    assert.equal((await rejected).code, "revoke_persistence_failed");
    await new Promise((resolve) => setTimeout(resolve, 25));
    socket.terminate();
    assert.match(stderr, /revocation_persist_failed/u);
    assert.match(stdout, /"event":"revocation_persist_failed"/u);
    return Buffer.from(`${stdout}\n${stderr}`, "utf8");
  } finally {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGTERM");
    await new Promise((resolve) => {
      if (child.exitCode !== null || child.signalCode !== null) resolve();
      else child.once("exit", resolve);
    });
  }
}

function testSecurityOptions() {
  if (process.platform !== "win32") return {};
  return {
    protectPath: async () => {},
    verifyPath: async () => {},
    verifyAncestors: async () => {},
  };
}

function rawPublic(key) {
  return key.export({ type: "spki", format: "der" }).subarray(-32);
}

function pairingRegistration() {
  const wrap = crypto.generateKeyPairSync("x25519");
  const handshake = crypto.generateKeyPairSync("x25519");
  return {
    v: 1,
    type: "desktop_pairing_register",
    request_id: "scan-transport-request-1",
    registration_request_id: "scan-registration-request-1",
    endpoint_id: "scan-endpoint-1",
    remote_control_generation: "scan-generation-1",
    code_wrap_public_key_b64: rawPublic(wrap.publicKey).toString("base64"),
    handshake_public_key_b64: rawPublic(handshake.publicKey).toString("base64"),
    key_algorithm: "x25519",
    capabilities: ["pairing_v1"],
  };
}

function encryptedProtocolCapture() {
  // Exercise the protocol's AES-256-GCM primitive and opaque encrypted_frame.
  // Sensitive task fields exist only before encryption; the Relay sees the
  // validated envelope returned below.
  const plaintext = Buffer.from(JSON.stringify({
    v: 1,
    type: "task_submit",
    sensitive_test_values: SENSITIVE_VALUES,
  }), "utf8");
  const key = crypto.randomBytes(32);
  const nonce = crypto.randomBytes(12);
  const cipher = crypto.createCipheriv("aes-256-gcm", key, nonce);
  const ciphertext = Buffer.concat([cipher.update(plaintext), cipher.final(), cipher.getAuthTag()]);
  key.fill(0);
  plaintext.fill(0);

  const frame = {
    v: 1,
    type: "encrypted_frame",
    connection_id: "scan-connection-1",
    controller_lease_id: "scan-controller-lease-1",
    lease_epoch: "1",
    relay_epoch: "scan-relay-epoch-1",
    remote_control_generation: "scan-generation-1",
    frame_sequence: "1",
    direction: "a_to_b",
    payload_kind: "task_submit",
    ciphertext_b64: Buffer.concat([nonce, ciphertext]).toString("base64"),
    ciphertext_bytes: String(nonce.length + ciphertext.length),
  };
  return validateControllerMessage(frame, "A");
}

async function collectLifecycleSurfaces(t) {
  const directory = await mkdtemp(join(tmpdir(), "pinvou-controller-scan-"));
  const statePath = join(directory, "controller", "state.json");
  const stateStore = await RelayStateStore.open({
    statePath,
    legacyStatePath: join(directory, "legacy-web-state.json"),
    processIdentity: async (pid) => `scan-process:${pid}`,
    ...testSecurityOptions(),
  });
  t.after(async () => {
    try { await stateStore.close(); } catch {}
    await rm(directory, { recursive: true, force: true });
  });

  const auditLines = [];
  const telemetry = new ControllerTelemetry({
    auditSink: (record) => auditLines.push(`[controller-audit] ${JSON.stringify(record)}`),
  });
  const pairingStore = await PairingStore.open({
    stateStore,
    digestKeyring: {
      activeKeyId: "scan-digest-key-1",
      keys: [{ keyId: "scan-digest-key-1", key: "scan-only-digest-material-at-least-32-bytes" }],
    },
    clock: () => 1000,
    randomBytes: (size) => Buffer.alloc(size, 0),
  });
  await pairingStore.register({
    request: pairingRegistration(),
    relayEpoch: "scan-relay-epoch-1",
    sealCode: sealPairingCode,
  });
  telemetry.audit("desktop_pairing_registered");

  const frame = encryptedProtocolCapture();
  telemetry.auditBusiness(frame.payload_kind);

  // Exercise the production validator once for every prohibited category,
  // using the sentinel itself as an attacker-controlled JSON key. The crash
  // projection is built from the actual thrown errors, not a clean fixture.
  const protocolErrors = [];
  for (const sentinel of SENTINELS) {
    let protocolError;
    try {
      validateControllerMessage({ ...frame, [sentinel]: true }, "A");
    } catch (error) {
      protocolError = error;
    }
    assert.equal(protocolError?.code, "unknown_field");
    protocolErrors.push({
      name: protocolError.name,
      code: protocolError.code,
      message: protocolError.message,
    });
  }

  const relayLogs = await collectRealRelayRevokeSurface(directory);
  return {
    logs: Buffer.concat([Buffer.from(auditLines.join("\n"), "utf8"), relayLogs]),
    persistence: await readFile(statePath),
    packet_capture: Buffer.from(JSON.stringify(frame), "utf8"),
    crash_diagnostics: JSON.stringify(protocolErrors),
  };
}

test("real lifecycle logs, controller persistence, encrypted capture and diagnostics are sentinel-clean", async (t) => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const result = scanner.assertClean(await collectLifecycleSurfaces(t));
  assert.equal(result.clean, true);
  assert.deepEqual(result.scanned_surfaces, [
    "logs", "persistence", "packet_capture", "crash_diagnostics",
  ]);
});

test("scanner detects raw, base64 and hex sentinels without echoing them", async (t) => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const surfaces = await collectLifecycleSurfaces(t);
  surfaces.logs += SENTINELS[0];
  surfaces.persistence = Buffer.concat([
    surfaces.persistence,
    Buffer.from(Buffer.from(SENTINELS[1]).toString("base64"), "ascii"),
  ]);
  surfaces.packet_capture = Buffer.from(Buffer.from(SENTINELS[2]).toString("hex"));
  const result = scanner.scan(surfaces);
  assert.equal(result.clean, false);
  assert.deepEqual(result.matched_surfaces, ["logs", "persistence", "packet_capture"]);
  const encoded = JSON.stringify(result);
  for (const sentinel of SENTINELS) assert.equal(encoded.includes(sentinel), false);
  assert.throws(
    () => scanner.assertClean(surfaces),
    (error) => error.code === "controller_sensitive_sentinel_detected"
      && SENTINELS.every((sentinel) => !error.message.includes(sentinel)),
  );
});

test("scanner rejects raw, base64 and hex contamination in real crash diagnostics", async (t) => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const surfaces = await collectLifecycleSurfaces(t);
  surfaces.crash_diagnostics += [
    SENTINELS[0],
    Buffer.from(SENTINELS[1]).toString("base64"),
    Buffer.from(SENTINELS[2]).toString("hex"),
  ].join("");
  const result = scanner.scan(surfaces);
  assert.equal(result.clean, false);
  assert.deepEqual(result.matched_surfaces, ["crash_diagnostics"]);
  const encoded = JSON.stringify(result);
  for (const sentinel of SENTINELS) assert.equal(encoded.includes(sentinel), false);
  assert.throws(
    () => scanner.assertClean(surfaces),
    (error) => error.code === "controller_sensitive_sentinel_detected"
      && SENTINELS.every((sentinel) => !error.message.includes(sentinel)),
  );
});

test("scanner rejects arbitrary surface names instead of widening evidence scope", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  assert.throws(() => scanner.scan({ arbitrary_patch: "safe" }), /surface_unknown/u);
});
