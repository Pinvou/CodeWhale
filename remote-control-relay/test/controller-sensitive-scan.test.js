"use strict";

import assert from "node:assert/strict";
import crypto from "node:crypto";
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { runInNewContext } from "node:vm";
import test from "node:test";
import WebSocket from "ws";

import { sealPairingCode } from "../pairing-code-sealer.js";
import { PairingStore } from "../pairing-store.js";
import { validateControllerMessage } from "../protocol-controller.js";
import { RelayStateStore } from "../relay-state-store.js";
import { ControllerSensitiveScanner, CONTROLLER_SCAN_SURFACES } from "../controller-sensitive-scan.js";
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

const cleanSurfaces = () => Object.fromEntries(CONTROLLER_SCAN_SURFACES.map((name) => [name, "collected-safe-data"]));

test("reflection and byte-conversion failures never expose proxy exceptions", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const secret = "canary-must-not-escape-from-proxy";
  const fail = () => { throw new Error(secret); };
  const revoked = Proxy.revocable({}, {});
  revoked.revoke();
  const cases = [
    [new Proxy(cleanSurfaces(), { ownKeys: fail }), "controller_scan_surfaces_invalid"],
    [new Proxy(cleanSurfaces(), { getOwnPropertyDescriptor: fail }), "controller_scan_surface_invalid"],
    [revoked.proxy, "controller_scan_surfaces_invalid"],
    [{ ...cleanSurfaces(), logs: new Proxy({}, { getPrototypeOf: fail }) }, "controller_scan_surface_invalid"],
    [{ ...cleanSurfaces(), logs: new Proxy(Buffer.from("safe"), { get: fail }) }, "controller_scan_surface_invalid"],
    [{ ...cleanSurfaces(), logs: revoked.proxy }, "controller_scan_surface_invalid"],
  ];
  for (const [surfaces, code] of cases) {
    for (const scan of [() => scanner.scan(surfaces), () => scanner.assertClean(surfaces)]) {
      assert.throws(scan, error => error.code === code && error.message === code && !error.stack.includes(secret));
    }
  }
  assert.equal(scanner.assertClean(cleanSurfaces()).clean, true);
});

test("typed-array conversion scans actual bytes without replacement hooks", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  for (const make of [value => Buffer.from(value), value => new Uint8Array(Buffer.from(value))]) {
    for (const name of CONTROLLER_SCAN_SURFACES) {
      const value = make(SENTINELS[0]);
      value.valueOf = () => "safe replacement";
      value.toString = () => "safe replacement";
      value[Symbol.iterator] = function* () { yield 0; };
      Object.defineProperty(value, "length", { get() { throw new Error("must-not-read-shadow-length"); } });
      assert.deepEqual(scanner.scan({ ...cleanSurfaces(), [name]: value }).matched_surfaces, [name]);
      assert.throws(() => scanner.assertClean({ ...cleanSurfaces(), [name]: value }), { code: "controller_sensitive_sentinel_detected" });
    }
  }
});

test("unsupported binary views fail closed instead of becoming JSON evidence", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const buffer = Uint8Array.from(Buffer.from(SENTINELS[0].padEnd(16, " "))).buffer;
  for (const value of [new DataView(buffer), new Int8Array(buffer), new Uint16Array(buffer), new Float64Array(buffer)]) {
    for (const name of CONTROLLER_SCAN_SURFACES) {
      for (const scan of [() => scanner.scan({ ...cleanSurfaces(), [name]: value }),
        () => scanner.assertClean({ ...cleanSurfaces(), [name]: value })]) {
        assert.throws(scan, error => error.code === "controller_scan_surface_invalid"
          && error.message === error.code && !error.stack.includes(SENTINELS[0]));
      }
    }
  }
  assert.equal(scanner.assertClean(cleanSurfaces()).clean, true);
});

test("Uint8Array slots remain authoritative across realms and replaced prototypes", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const foreign = runInNewContext("new Uint8Array(bytes)", { bytes: [...Buffer.from(SENTINELS[0])] });
  const unprototyped = Object.setPrototypeOf(Buffer.from(SENTINELS[0]), null);
  for (const value of [foreign, unprototyped]) {
    for (const name of CONTROLLER_SCAN_SURFACES) {
      assert.deepEqual(scanner.scan({ ...cleanSurfaces(), [name]: value }).matched_surfaces, [name]);
      assert.throws(() => scanner.assertClean({ ...cleanSurfaces(), [name]: value }),
        { code: "controller_sensitive_sentinel_detected" });
    }
  }
});

test("offset byte views scan exactly their owned range", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const bytes = Buffer.from(`${SENTINELS[0]}safe${SENTINELS[0]}`);
  const offset = SENTINELS[0].length;
  for (const make of [() => bytes.subarray(offset, offset + 4),
    () => new Uint8Array(bytes.buffer, bytes.byteOffset + offset, 4),
    () => runInNewContext("new Uint8Array(buffer, offset, 4)",
      { buffer: bytes.buffer, offset: bytes.byteOffset + offset })]) {
    const value = make();
    for (const name of CONTROLLER_SCAN_SURFACES) {
      assert.equal(scanner.assertClean({ ...cleanSurfaces(), [name]: value }).clean, true);
    }
  }
  const contaminated = new Uint8Array(bytes.buffer, bytes.byteOffset + offset + 4, SENTINELS[0].length);
  assert.deepEqual(scanner.scan({ ...cleanSurfaces(), logs: contaminated }).matched_surfaces, ["logs"]);
});

test("phantom surface descriptors and malformed sentinels retain fixed error codes", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const phantom = new Proxy(cleanSurfaces(), {
    getOwnPropertyDescriptor(target, name) {
      return name === "logs" ? undefined : Reflect.getOwnPropertyDescriptor(target, name);
    },
  });
  assert.throws(() => scanner.scan(phantom), { code: "controller_scan_surface_invalid" });
  assert.throws(() => scanner.assertClean(phantom), { code: "controller_scan_surface_invalid" });
  for (const values of [null, [], Array(33).fill("valid-sentinel")]) {
    assert.throws(() => new ControllerSensitiveScanner(values), { code: "controller_scan_sentinels_invalid" });
  }
  for (const value of [null, "short", "a".repeat(4097), "invalid\ud800"]) {
    assert.throws(() => new ControllerSensitiveScanner([value]),
      error => error.code === "controller_scan_sentinel_invalid" && error.message === error.code);
  }
  assert.equal(scanner.assertClean(cleanSurfaces()).clean, true);
});

test("assigning public lookalike state cannot replace registered detection needles", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  scanner.needles = [];
  assert.throws(() => scanner.assertClean({ ...cleanSurfaces(), logs: SENTINELS[0] }),
    { code: "controller_sensitive_sentinel_detected" });
  assert.equal(scanner.assertClean(cleanSurfaces()).clean, true);
});

test("sparse or accessor canary lists cannot register an empty detection rule set", () => {
  const inherited = Array(1);
  Object.setPrototypeOf(inherited, { 0: SENTINELS[0] });
  let accessed = false;
  const accessor = Array(1);
  Object.defineProperty(accessor, "0", { get() { accessed = true; throw new Error(SENTINELS[0]); } });
  for (const values of [Array(1), [SENTINELS[0], , SENTINELS[1]], inherited, accessor]) {
    assert.throws(() => new ControllerSensitiveScanner(values),
      error => error.code === "controller_scan_sentinel_invalid"
        && error.message === error.code && !error.stack.includes(SENTINELS[0]));
  }
  assert.equal(accessed, false);
  const values = [...SENTINELS];
  values.map = () => [];
  values[Symbol.iterator] = function* () {};
  const scanner = new ControllerSensitiveScanner(values);
  assert.throws(() => scanner.assertClean({ ...cleanSurfaces(), logs: SENTINELS[0] }),
    { code: "controller_sensitive_sentinel_detected" });
  assert.equal(scanner.assertClean(cleanSurfaces()).clean, true);
});

test("every scan requires all four owned evidence surfaces", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  for (let mask = 0; mask < 15; mask += 1) {
    const partial = Object.fromEntries(CONTROLLER_SCAN_SURFACES
      .filter((_, index) => mask & (1 << index)).map((name) => [name, "safe"]));
    for (const scan of [() => scanner.scan(partial), () => scanner.assertClean(partial)]) {
      assert.throws(scan, (error) => error.code === "controller_scan_surfaces_incomplete");
    }
  }
  const inherited = Object.create(cleanSurfaces());
  assert.throws(() => scanner.scan(inherited), { code: "controller_scan_surfaces_incomplete" });
  assert.equal(scanner.assertClean(cleanSurfaces()).scanned_surfaces.length, 4);
});

test("invalid surface values fail with bounded codes without echoing data", () => {
  const scanner = new ControllerSensitiveScanner(SENTINELS);
  const cycle = {}; cycle.self = cycle;
  for (const name of CONTROLLER_SCAN_SURFACES) {
    for (const value of [undefined, null, 42, true, 1n, Symbol("private"), () => "private", cycle, { value: 1n },
      { toJSON() { throw new Error(SENTINELS[0]); } }]) {
      assert.throws(() => scanner.assertClean({ ...cleanSurfaces(), [name]: value }),
        (error) => error.code === "controller_scan_surface_invalid"
          && error.message === error.code && !error.message.includes(SENTINELS[0]));
    }
    const accessor = cleanSurfaces();
    Object.defineProperty(accessor, name, { enumerable: true, get() { throw new Error(SENTINELS[0]); } });
    assert.throws(() => scanner.scan(accessor), { code: "controller_scan_surface_invalid" });
  }
  assert.throws(() => scanner.scan({ ...cleanSurfaces(), extra: SENTINELS[0] }),
    { code: "controller_scan_surface_unknown" });
  assert.throws(() => scanner.scan({ ...cleanSurfaces(), [Symbol("private")]: "secret" }),
    { code: "controller_scan_surface_unknown" });
});

test("registered whole-value encoding variants are detected on every evidence surface", () => {
  const sentinel = "secret/\u9ad8\u00ff?\"\\\n";
  const raw = Buffer.from(sentinel);
  const unicode = [...sentinel].map((char) => `\\u${char.charCodeAt(0).toString(16).padStart(4, "0")}`).join("");
  const variants = [sentinel, raw.toString("base64"), raw.toString("base64").replace(/=+$/u, ""),
    raw.toString("base64url"), raw.toString("base64url").padEnd(Math.ceil(raw.length / 3) * 4, "="),
    raw.toString("hex"), raw.toString("hex").toUpperCase(), JSON.stringify(sentinel).slice(1, -1),
    unicode, unicode.replace(/[a-f]/gu, (char) => char.toUpperCase()), encodeURIComponent(sentinel),
    raw.toString("hex").replace(/../gu, (hex) => `%${hex}`), raw.toString("hex").replace(/../gu, (hex) => `%${hex.toUpperCase()}`)];
  const scanner = new ControllerSensitiveScanner([sentinel]);
  for (const name of CONTROLLER_SCAN_SURFACES) for (const variant of variants) {
    const surfaces = { ...cleanSurfaces(), [name]: `prefix:${variant}:suffix` };
    const result = scanner.scan(surfaces);
    assert.deepEqual(result.matched_surfaces, [name]);
    assert.throws(() => scanner.assertClean(surfaces), { code: "controller_sensitive_sentinel_detected" });
    assert.equal(JSON.stringify(result).includes(sentinel), false);
  }
  assert.equal(scanner.assertClean({ logs: Buffer.from("safe"), persistence: { entries: [] },
    packet_capture: new Uint8Array([1, 2, 3]), crash_diagnostics: [] }).clean, true);
});
