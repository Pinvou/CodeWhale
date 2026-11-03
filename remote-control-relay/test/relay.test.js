import assert from "node:assert/strict";
import crypto from "node:crypto";
import { after, before, test } from "node:test";
import { spawn } from "node:child_process";
import { mkdir, mkdtemp, readFile, readdir, realpath, rm, writeFile } from "node:fs/promises";
import { connect, createServer } from "node:net";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import WebSocket from "ws";
import {
  buildPairingTranscriptPreimage,
  buildSealedCodeAad,
  parseSealedCodeV1,
  receiptCommitmentHex,
  CONTROLLER_SUBPROTOCOL,
} from "../protocol-controller.js";
import { smokeControllerRoute } from "../scripts/controller-route-smoke.mjs";
import { main as migrationCli } from "../scripts/enrollment-migrate.js";
import { protectWindowsPath } from "../relay-state-store.js";

const X25519_SPKI_PREFIX = Buffer.from("302a300506032b656e032100", "hex");

function getFreePort() {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.unref();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      server.close(() => {
        if (address && typeof address === "object") resolve(address.port);
        else reject(new Error("failed to allocate test port"));
      });
    });
  });
}

const relayDir = dirname(dirname(fileURLToPath(import.meta.url)));
const port = await getFreePort();
const httpUrl = `http://127.0.0.1:${port}`;
const wsUrl = `ws://127.0.0.1:${port}/pinvou3/remote/ws`;
const enrollmentToken = "test-enrollment-token-at-least-24";
const adminPassword = "test-admin-password";
let relay;
let testRoot;
let telemetryDir;
let webDir;
let relayStatePath;

function waitForOutput(child, pattern, timeoutMs = 5000) {
  return new Promise((resolve, reject) => {
    let stdout = "";
    let stderr = "";
    const timer = setTimeout(() => reject(new Error(`relay startup timeout: ${pattern}`)), timeoutMs);
    const onData = (chunk) => {
      const text = String(chunk);
      stdout += text;
      if (!pattern.test(text)) return;
      clearTimeout(timer);
      child.stdout.off("data", onData);
      child.stderr.off("data", onErr);
      resolve(text);
    };
    const onErr = (chunk) => { stderr += String(chunk); };
    child.stdout.on("data", onData);
    child.stderr.on("data", onErr);
    child.once("exit", (code) => {
      clearTimeout(timer);
      child.stdout.off("data", onData);
      child.stderr.off("data", onErr);
      reject(new Error(`relay exited during startup: ${code}\nstdout:\n${stdout}\nstderr:\n${stderr}`));
    });
  });
}

// A relay log line is written by the relay process and delivered to this
// process through a pipe: socket close and stderr arrival are independent
// events, so assertions on the line must wait for it instead of assuming it
// landed by the time the close handshake finished.
function waitForError(isolated, pattern, timeoutMs = 5000) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const poll = () => {
      if (pattern.test(isolated.errors())) return resolve();
      if (Date.now() - started > timeoutMs) {
        return reject(new Error(`timeout waiting for stderr ${pattern}\nstderr:\n${isolated.errors()}`));
      }
      setTimeout(poll, 25);
    };
    poll();
  });
}

// Sends one hand-built HTTP request and returns the raw response text; used
// where the WebSocket and fetch clients refuse to send malformed headers.
function rawRequest(hostPort, requestText) {
  return new Promise((resolve, reject) => {
    const socket = connect(hostPort, "127.0.0.1", () => {
      socket.end(requestText);
    });
    // A server that accepts but never answers must fail the test, not hang CI.
    socket.setTimeout(5000, () => socket.destroy());
    let text = "";
    socket.on("data", (chunk) => { text += String(chunk); });
    socket.once("error", reject);
    socket.once("close", () => resolve(text));
  });
}

function openSocket(url = wsUrl, options = {}) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url, options);
    ws.once("open", () => resolve(ws));
    ws.once("error", reject);
  });
}

function openControllerSocket(url = wsUrl) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url, CONTROLLER_SUBPROTOCOL);
    ws.once("open", () => {
      if (ws.protocol === CONTROLLER_SUBPROTOCOL) resolve(ws);
      else reject(new Error("controller subprotocol was not negotiated"));
    });
    ws.once("error", reject);
  });
}

function rejectedUpgrade(url, options = {}) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url, options);
    ws.once("open", () => {
      ws.close();
      reject(new Error("websocket upgrade unexpectedly succeeded"));
    });
    ws.once("unexpected-response", (_request, response) => {
      response.resume();
      resolve(response.statusCode);
    });
    ws.once("error", (error) => {
      if (/Unexpected server response/.test(error.message)) return;
      reject(error);
    });
  });
}

function nextMessage(ws, type, timeoutMs = 3000) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      ws.off("message", onMessage);
      reject(new Error(`timeout waiting for ${type}`));
    }, timeoutMs);
    const onMessage = (data) => {
      const message = JSON.parse(String(data));
      if (message.type !== type) return;
      clearTimeout(timer);
      ws.off("message", onMessage);
      resolve(message);
    };
    ws.on("message", onMessage);
  });
}

function expectNoMessage(ws, type, timeoutMs = 250) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      ws.off("message", onMessage);
      resolve();
    }, timeoutMs);
    const onMessage = (data) => {
      const message = JSON.parse(String(data));
      if (message.type !== type) return;
      clearTimeout(timer);
      ws.off("message", onMessage);
      reject(new Error(`unexpected ${type}: ${String(data)}`));
    };
    ws.on("message", onMessage);
  });
}

function waitForClose(ws, timeoutMs = 3000) {
  if (ws.readyState === ws.CLOSED) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("timeout waiting for websocket close")), timeoutMs);
    ws.once("close", () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

async function registerDesktop(endpointId, accessToken, desktopSecret, url = wsUrl, timeoutMs = 3000,
  controllerCapable = false) {
  const ws = controllerCapable ? await openControllerSocket(url) : await openSocket(url);
  const registered = nextMessage(ws, "desktop_endpoint_registered", timeoutMs);
  ws.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: endpointId,
    access_token: accessToken,
    desktop_secret: desktopSecret,
  }));
  return { ws, registered: await registered };
}

async function joinWeb(endpointId, accessToken, options = {}, url = wsUrl) {
  const ws = await openSocket(url);
  const joined = nextMessage(ws, "web_client_joined");
  ws.send(JSON.stringify({
    v: 2,
    type: "web_client_join",
    endpoint_id: endpointId,
    access_token: accessToken,
    stream_epoch: options.streamEpoch || null,
    after_seq: options.afterSeq || 0,
  }));
  return { ws, joined: await joined };
}

function closeSocket(ws) {
  if (!ws) return;
  try { ws.close(); } catch {}
}

function waitForExit(child, timeoutMs = 3000) {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("timeout waiting for relay exit")), timeoutMs);
    child.once("exit", () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

async function stopRelay(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  child.kill("SIGTERM");
  await waitForExit(child);
}

async function spawnRelay(env = {}, { preload, ipc = false, awaitListening = true } = {}) {
  const isolatedPort = await getFreePort();
  const statePath = env.PINVOU_REMOTE_STATE_PATH
    || join(testRoot, `relay-state-${isolatedPort}-${Date.now()}-${Math.random().toString(16).slice(2)}.json`);
  const args = preload ? ["--import", preload, join(relayDir, "server.js")] : [join(relayDir, "server.js")];
  const child = spawn(process.execPath, args, {
    cwd: relayDir,
    env: {
      ...process.env,
      PORT: String(isolatedPort),
      PINVOU_REMOTE_PUBLIC_BASE_PATH: "/pinvou3/remote",
      PINVOU_REMOTE_WEB_DIR: webDir,
      HEARTBEAT_INTERVAL_MS: "5000",
      PINVOU_REMOTE_STATE_PATH: statePath,
      ...env,
    },
    stdio: ipc ? ["ignore", "pipe", "pipe", "ipc"] : ["ignore", "pipe", "pipe"],
  });
  let output = "";
  let errors = "";
  child.stdout.on("data", (chunk) => { output += String(chunk); });
  child.stderr.on("data", (chunk) => { errors += String(chunk); });
  if (awaitListening) {
    await waitForOutput(child, /pinvou remote relay listening/,
      env.PINVOU_DESKTOP_CONTROLLER_ENABLED === "1" ? 60_000 : 5000);
  }
  return {
    child,
    port: isolatedPort,
    httpUrl: `http://127.0.0.1:${isolatedPort}`,
    wsUrl: `ws://127.0.0.1:${isolatedPort}/pinvou3/remote/ws`,
    statePath,
    output: () => output,
    errors: () => errors,
  };
}

function controllerEnvironment(name, overrides = {}) {
  const signing = crypto.generateKeyPairSync("ed25519");
  return {
    NODE_ENV: "development",
    PINVOU_DESKTOP_CONTROLLER_ENABLED: "1",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "1",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(testRoot, `controller-${name}-${Date.now()}`, "state.json"),
    PINVOU_REMOTE_STATE_PATH: join(testRoot, `legacy-${name}-${Date.now()}.json`),
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "enrollment-regression-key",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "enrollment-regression-material-at-least-32-bytes",
    PINVOU_REMOTE_PAIRING_HMAC_KEY_ID: "pairing-regression-key",
    PINVOU_REMOTE_PAIRING_HMAC_KEY: "pairing-regression-independent-material-at-least-32-bytes",
    PINVOU_REMOTE_RELAY_SIGNING_KEY_ID: "signing-regression-key",
    PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64: signing.privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
    ...overrides,
  };
}

function selfRegistrationEnvironment(name, overrides = {}) {
  return controllerEnvironment(name, {
    NODE_ENV: "production",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "false",
    PINVOU_REMOTE_ENROLLMENT_SELF_REGISTRATION: "true",
    PINVOU_TELEMETRY_ENROLLMENT_TOKEN: enrollmentToken,
    PINVOU_TELEMETRY_DEVICE_PEPPER: "self-registration-test-pepper-at-least-24",
    PINVOU_STATS_ADMIN_PASSWORD: adminPassword,
    PINVOU_TELEMETRY_DATA_DIR: join(testRoot, `self-registration-telemetry-${name}`),
    ...overrides,
  });
}

async function enrollmentSnapshot(statePath) {
  const state = JSON.parse(await readFile(statePath, "utf8"));
  return Array.isArray(state.namespaces)
    ? state.namespaces.find(record => record.name === "endpoint_enrollment")
    : state.namespaces.endpoint_enrollment;
}

test("public production: first desktop receives a real pairing code and reconnects after Relay restart without admin enrollment", { timeout: 90_000 }, async () => {
  const env = selfRegistrationEnvironment("first-start");
  const endpointId = `ep_${crypto.randomBytes(12).toString("hex")}`;
  const access = crypto.randomBytes(32).toString("base64url");
  const desktopSecret = crypto.randomBytes(32).toString("base64url");
  let isolated = await spawnRelay(env), ws;
  try {
    ({ ws } = await registerDesktop(endpointId, access, desktopSecret, isolated.wsUrl, 15_000, true));
    const codeKey = crypto.generateKeyPairSync("x25519");
    const handshakeKey = crypto.generateKeyPairSync("x25519");
    const registered = nextMessageOneOf(ws, ["desktop_pairing_registered", "error"], 15_000);
    ws.send(JSON.stringify({ v: 1, type: "desktop_pairing_register", request_id: "request-self-registration",
      registration_request_id: "registration-self-registration", endpoint_id: endpointId,
      remote_control_generation: "generation-self-registration",
      code_wrap_public_key_b64: rawX25519Public(codeKey.publicKey).toString("base64"),
      handshake_public_key_b64: rawX25519Public(handshakeKey.publicKey).toString("base64"),
      key_algorithm: "x25519", capabilities: ["pairing_v1"] }));
    const response = await registered;
    assert.equal(response.type, "desktop_pairing_registered", response.code);
    assert.match(openPairingCode(codeKey.privateKey, response), /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u);
    const before = await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
    assert.equal(before.value.entries.length, 1);
    assert.equal(before.value.entries[0].desktop_credential.algorithm, "hmac-sha256-role-v1");
    assert.equal(JSON.stringify(before).includes(access), false);
    assert.equal(JSON.stringify(before).includes(desktopSecret), false);
    closeSocket(ws);
    await stopRelay(isolated.child);
    isolated = await spawnRelay(env);
    ({ ws } = await registerDesktop(endpointId, access, desktopSecret, isolated.wsUrl, 15_000, true));
    const after = await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
    assert.deepEqual(after, before, "restart and re-registration must preserve enrolled credentials and revision");
    const wrong = await openControllerSocket(isolated.wsUrl);
    try {
      const rejected = nextMessage(wrong, "error", 15_000);
      wrong.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: endpointId,
        access_token: access, desktop_secret: crypto.randomBytes(32).toString("base64url") }));
      assert.equal((await rejected).code, "invalid_enrollment_credential");
      assert.deepEqual(await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), before);
    } finally { closeSocket(wrong); }
  } finally {
    closeSocket(ws);
    await stopRelay(isolated.child);
  }
});

for (const [name, overrides, expected] of [
  ["rate", { ENDPOINT_CREATE_LIMIT: "1", ENDPOINT_CREATE_WINDOW_MS: "60000" }, "endpoint_creation_rate_limited"],
  ["capacity", { MAX_ENDPOINTS: "1", ENDPOINT_CREATE_LIMIT: "100" }, "endpoint_capacity_reached"],
]) {
  test(`public production: ${name} admission rejects new devices before durable registry writes`, { timeout: 90_000 }, async () => {
    const env = selfRegistrationEnvironment(name, overrides);
    const isolated = await spawnRelay(env);
    let first, denied;
    try {
      first = await registerDesktop(`ep_${crypto.randomBytes(12).toString("hex")}`,
        crypto.randomBytes(32).toString("base64url"), crypto.randomBytes(32).toString("base64url"), isolated.wsUrl, 15_000, true);
      const before = await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
      denied = await openControllerSocket(isolated.wsUrl);
      const rejected = nextMessage(denied, "error", 15_000);
      denied.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register",
        endpoint_id: `ep_${crypto.randomBytes(12).toString("hex")}`,
        access_token: crypto.randomBytes(32).toString("base64url"), desktop_secret: crypto.randomBytes(32).toString("base64url") }));
      assert.equal((await rejected).code, expected);
      assert.deepEqual(await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), before);
    } finally {
      closeSocket(first?.ws); closeSocket(denied);
      await stopRelay(isolated.child);
    }
  });
}

test("identifier contract: real Relay accepts pinned-trust signing IDs and preserves legacy leading-ID Web v2", { timeout: 90_000 }, async (t) => {
  const isolated = await spawnRelay(controllerEnvironment("identifier-contract", {
    PINVOU_REMOTE_RELAY_SIGNING_KEY_ID: "relay.signing-0001",
  }));
  t.after(() => stopRelay(isolated.child));
  for (const endpointId of ["_old", "-old"]) {
    const accessToken = `legacy-access-${endpointId}-credential-0000000000000001`;
    const desktopSecret = `legacy-desktop-${endpointId}-credential-000000000000001`;
    const desktop = await registerDesktop(endpointId, accessToken, desktopSecret, isolated.wsUrl);
    t.after(() => closeSocket(desktop.ws));
    const web = await joinWeb(endpointId, accessToken, {}, isolated.wsUrl);
    t.after(() => closeSocket(web.ws));
    assert.equal(web.joined.endpoint_id, endpointId);
    assert.equal(web.joined.desktop_connected, true);
  }
});

function waitForRegistrationProbe(child, type, endpointId) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      child.off("message", onMessage);
      reject(new Error(`timeout waiting for registration probe ${type}`));
    }, 15_000);
    const onMessage = (message) => {
      if (message?.type !== type || (endpointId && message.endpointId !== endpointId)) return;
      clearTimeout(timer);
      child.off("message", onMessage);
      resolve(message);
    };
    child.on("message", onMessage);
  });
}

test("public production: one pending socket cannot persist multiple anonymous devices", { timeout: 90_000 }, async () => {
  const env = selfRegistrationEnvironment("pending-registration");
  const isolated = await spawnRelay(env, {
    preload: new URL("./fixtures/desktop-registration-barrier.js", import.meta.url).href, ipc: true,
  });
  let ws;
  try {
    ws = await openControllerSocket(isolated.wsUrl);
    const ids = [0, 1].map(() => `ep_${crypto.randomBytes(12).toString("hex")}`);
    const ready = waitForRegistrationProbe(isolated.child, "authorization-ready", ids[0]);
    const rejected = nextMessage(ws, "error", 15_000);
    for (const endpoint_id of ids) {
      ws.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id,
        access_token: crypto.randomBytes(32).toString("base64url"),
        desktop_secret: crypto.randomBytes(32).toString("base64url") }));
    }
    await ready;
    assert.equal((await rejected).code, "bad_desktop_endpoint_register");
    const snapshot = await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
    assert.deepEqual(snapshot.value.entries.map(entry => entry.endpoint_id), [ids[0]]);
    const finished = waitForRegistrationProbe(isolated.child, "authorization-finished", ids[0]);
    isolated.child.send({ type: "release-registration" });
    await finished;
  } finally {
    closeSocket(ws);
    await stopRelay(isolated.child);
  }
});

for (const sameIdentity of [false, true]) {
  test(`public production: concurrent sockets ${sameIdentity ? "share one identity slot" : "reserve capacity before distinct identity writes"}`, { timeout: 90_000 }, async () => {
    const env = selfRegistrationEnvironment(`cross-socket-${sameIdentity}`, { MAX_ENDPOINTS: "1", ENDPOINT_CREATE_LIMIT: "100" });
    const isolated = await spawnRelay(env, {
      preload: new URL("./fixtures/desktop-registration-barrier.js", import.meta.url).href, ipc: true,
    });
    let first, second;
    try {
      first = await openControllerSocket(isolated.wsUrl);
      second = await openControllerSocket(isolated.wsUrl);
      const id = `ep_${crypto.randomBytes(12).toString("hex")}`;
      const credentials = { access_token: crypto.randomBytes(32).toString("base64url"), desktop_secret: crypto.randomBytes(32).toString("base64url") };
      const ready = waitForRegistrationProbe(isolated.child, "authorization-ready", id);
      const firstRegistered = nextMessage(first, "desktop_endpoint_registered", 15_000);
      first.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: id, ...credentials }));
      await ready;
      const before = await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
      const secondResponse = nextMessageOneOf(second, ["desktop_endpoint_registered", "error"], 15_000);
      const secondReady = sameIdentity ? waitForRegistrationProbe(isolated.child, "authorization-ready", id) : null;
      second.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register",
        endpoint_id: sameIdentity ? id : `ep_${crypto.randomBytes(12).toString("hex")}`, ...credentials }));
      if (sameIdentity) await secondReady;
      else assert.equal((await secondResponse).code, "endpoint_capacity_reached");
      assert.deepEqual(await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), before);
      isolated.child.send({ type: "release-registration" });
      assert.equal((await firstRegistered).type, "desktop_endpoint_registered");
      if (sameIdentity) assert.equal((await secondResponse).type, "desktop_endpoint_registered");
      const health = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
      assert.equal(health.endpoint_count, 1);
    } finally {
      closeSocket(first); closeSocket(second); await stopRelay(isolated.child);
    }
  });
}

test("public production: failed enrollment releases its reserved capacity", { timeout: 90_000 }, async () => {
  const isolated = await spawnRelay(selfRegistrationEnvironment("release-failed", { MAX_ENDPOINTS: "1", ENDPOINT_CREATE_LIMIT: "100" }));
  let denied, accepted;
  try {
    denied = await openControllerSocket(isolated.wsUrl);
    const response = nextMessage(denied, "error", 15_000);
    const sharedCredential = crypto.randomBytes(32).toString("base64url");
    denied.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: `ep_${crypto.randomBytes(12).toString("hex")}`,
      access_token: sharedCredential, desktop_secret: sharedCredential }));
    assert.equal((await response).code, "invalid_enrollment_credential");
    accepted = await registerDesktop(`ep_${crypto.randomBytes(12).toString("hex")}`,
      crypto.randomBytes(32).toString("base64url"), crypto.randomBytes(32).toString("base64url"), isolated.wsUrl, 15_000, true);
    assert.equal(accepted.registered.type, "desktop_endpoint_registered");
  } finally {
    closeSocket(denied); closeSocket(accepted?.ws); await stopRelay(isolated.child);
  }
});

test("public production: a closing socket holds capacity until pending authorization finishes, then releases it", { timeout: 90_000 }, async () => {
  const env = selfRegistrationEnvironment("release-closed", { MAX_ENDPOINTS: "1", ENDPOINT_CREATE_LIMIT: "100" });
  const isolated = await spawnRelay(env, {
    preload: new URL("./fixtures/desktop-registration-barrier.js", import.meta.url).href, ipc: true,
  });
  let closing, denied, accepted;
  try {
    const id = `ep_${crypto.randomBytes(12).toString("hex")}`;
    closing = await openControllerSocket(isolated.wsUrl);
    const ready = waitForRegistrationProbe(isolated.child, "authorization-ready", id);
    closing.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: id,
      access_token: crypto.randomBytes(32).toString("base64url"), desktop_secret: crypto.randomBytes(32).toString("base64url") }));
    await ready;
    const before = await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
    const closed = waitForRegistrationProbe(isolated.child, "socket-closed");
    closing.close(); await closed;
    denied = await openControllerSocket(isolated.wsUrl);
    const rejected = nextMessage(denied, "error", 15_000);
    denied.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: `ep_${crypto.randomBytes(12).toString("hex")}`,
      access_token: crypto.randomBytes(32).toString("base64url"), desktop_secret: crypto.randomBytes(32).toString("base64url") }));
    assert.equal((await rejected).code, "endpoint_capacity_reached");
    assert.deepEqual(await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), before);
    const finished = waitForRegistrationProbe(isolated.child, "authorization-finished", id);
    isolated.child.send({ type: "release-registration" }); await finished;
    const health = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(health.endpoint_count, 0);
    accepted = await openControllerSocket(isolated.wsUrl);
    const replacementId = `ep_${crypto.randomBytes(12).toString("hex")}`;
    const replacementReady = waitForRegistrationProbe(isolated.child, "authorization-ready", replacementId);
    const registered = nextMessage(accepted, "desktop_endpoint_registered", 15_000);
    accepted.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: replacementId,
      access_token: crypto.randomBytes(32).toString("base64url"), desktop_secret: crypto.randomBytes(32).toString("base64url") }));
    await replacementReady; isolated.child.send({ type: "release-registration" });
    assert.equal((await registered).type, "desktop_endpoint_registered");
  } finally {
    closeSocket(closing); closeSocket(denied); closeSocket(accepted); await stopRelay(isolated.child);
  }
});

test("public production: self-revocation permits offline quota recovery while retaining the permanent Relay denial", { timeout: 300_000 }, async () => {
  const env = selfRegistrationEnvironment("relay-revoked");
  let isolated = await spawnRelay(env);
  const id = `ep_${crypto.randomBytes(12).toString("hex")}`;
  const access = crypto.randomBytes(32).toString("base64url");
  const secret = crypto.randomBytes(32).toString("base64url");
  let desktop, denied, fresh;
  try {
    desktop = await registerDesktop(id, access, secret, isolated.wsUrl, 15_000, true);
    const before = await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH);
    const revoked = nextMessage(desktop.ws, "desktop_endpoint_revoked", 15_000);
    desktop.ws.send(JSON.stringify({ v: 2, type: "desktop_endpoint_revoke", endpoint_id: id, desktop_secret: secret }));
    await revoked;
    denied = await openControllerSocket(isolated.wsUrl);
    const response = nextMessage(denied, "error", 15_000);
    denied.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: id,
      access_token: access, desktop_secret: secret }));
    assert.equal((await response).code, "endpoint_revoked");
    assert.deepEqual(await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH), before);
    assert.equal(before.value.entries[0].status, "active", "socket self-revocation does not invoke registry revocation");
    closeSocket(desktop.ws); closeSocket(denied);
    await stopRelay(isolated.child);
    const legacyBefore = await readFile(env.PINVOU_REMOTE_STATE_PATH);
    const inventoryDir = join(testRoot, "self-revocation-cleanup");
    await mkdir(inventoryDir, { mode: 0o700 });
    if (process.platform === "win32") await protectWindowsPath(inventoryDir, "directory");
    const inventoryPath = join(inventoryDir, "selected.json");
    const output = [], errors = [];
    const io = { log: value => output.push(JSON.parse(value)), error: value => errors.push(JSON.parse(value)) };
    assert.equal(await migrationCli(["--list-self-registrations", "--output", inventoryPath], env, io), 0, JSON.stringify(errors));
    const inventory = JSON.parse(await readFile(inventoryPath, "utf8"));
    assert.deepEqual(inventory.entries.map(entry => [entry.endpoint_id, entry.status]), [[id, "active"]]);
    assert.equal(await migrationCli(["--check-self-registration-cleanup", "--input", inventoryPath], env, io), 0);
    assert.equal(output.at(-1).selected, 1);
    assert.equal(output.at(-1).removed, 0);
    assert.equal(await migrationCli(["--apply-self-registration-cleanup", "--input", inventoryPath], env, io), 0);
    assert.equal(output.at(-1).removed, 1);
    assert.equal(output.at(-1).capacity.entries, 0);
    assert.deepEqual(await readFile(env.PINVOU_REMOTE_STATE_PATH), legacyBefore, "cleanup must retain the separate Relay tombstone");
    isolated = await spawnRelay(env);
    denied = await openControllerSocket(isolated.wsUrl);
    const afterRestart = nextMessage(denied, "error", 15_000);
    denied.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: id,
      access_token: access, desktop_secret: secret }));
    assert.equal((await afterRestart).code, "endpoint_revoked");
    assert.equal((await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH)).value.entries.length, 0);
    fresh = await registerDesktop(`ep_${crypto.randomBytes(12).toString("hex")}`, access, secret, isolated.wsUrl, 15_000, true);
    assert.equal((await enrollmentSnapshot(env.PINVOU_REMOTE_CONTROLLER_STATE_PATH)).value.entries.length, 1);
  } finally {
    closeSocket(desktop?.ws); closeSocket(denied); closeSocket(fresh?.ws); await stopRelay(isolated.child);
  }
});

test("desktop authorization cannot attach one socket to two endpoints", async () => {
  const isolated = await spawnRelay(controllerEnvironment("concurrent-registration"), {
    preload: new URL("./fixtures/desktop-registration-barrier.js", import.meta.url).href, ipc: true,
  });
  let ws;
  try {
    ws = await openSocket(isolated.wsUrl);
    const ids = ["endpoint-concurrent-first", "endpoint-concurrent-second"];
    const ready = ids.map((id) => waitForRegistrationProbe(isolated.child, "authorization-ready", id));
    for (const endpoint_id of ids) {
      ws.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id,
        access_token: "concurrent-registration-access-00000001",
        desktop_secret: "concurrent-registration-secret-00000001" }));
    }
    await Promise.all(ready);
    const finished = ids.map((id) => waitForRegistrationProbe(isolated.child, "authorization-finished", id));
    const registered = [];
    const errors = [];
    ws.on("message", (data) => {
      const message = JSON.parse(String(data));
      if (message.type === "desktop_endpoint_registered") registered.push(message);
      if (message.type === "error") errors.push(message);
    });
    const responses = Promise.all([
      nextMessage(ws, "desktop_endpoint_registered", 15_000),
      nextMessage(ws, "error", 15_000),
    ]);
    isolated.child.send({ type: "release-registration" });
    await Promise.all([...finished, responses]);
    const health = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(health.endpoint_count, 1, "a second pending authorization must not create an endpoint");
    assert.equal(registered.length, 1);
    assert.equal(errors.length, 1);
    assert.equal(errors[0].code, "bad_desktop_endpoint_register");
  } finally {
    closeSocket(ws);
    await stopRelay(isolated.child);
  }
});

test("desktop authorization cannot attach a socket closed while authentication awaits", async () => {
  const isolated = await spawnRelay(controllerEnvironment("closed-registration"), {
    preload: new URL("./fixtures/desktop-registration-barrier.js", import.meta.url).href, ipc: true,
  });
  let ws;
  try {
    ws = await openSocket(isolated.wsUrl);
    const endpointId = "endpoint-closed-authorization";
    const ready = waitForRegistrationProbe(isolated.child, "authorization-ready", endpointId);
    ws.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: endpointId,
      access_token: "closed-registration-access-00000000001",
      desktop_secret: "closed-registration-secret-00000000001" }));
    await ready;
    const closed = waitForRegistrationProbe(isolated.child, "socket-closed");
    ws.close();
    await closed;
    const finished = waitForRegistrationProbe(isolated.child, "authorization-finished", endpointId);
    isolated.child.send({ type: "release-registration" });
    await finished;
    const health = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(health.endpoint_count, 0, "late authorization must not create an orphan endpoint");
    assert.equal(health.ws_connection_count, 0);
  } finally {
    closeSocket(ws);
    await stopRelay(isolated.child);
  }
});

function nextMessageOneOf(ws, types, timeoutMs = 3000) {
  const accepted = new Set(types);
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      ws.off("message", onMessage);
      reject(new Error(`timeout waiting for ${[...accepted].join(" or ")}`));
    }, timeoutMs);
    const onMessage = (data) => {
      const message = JSON.parse(String(data));
      if (!accepted.has(message.type)) return;
      clearTimeout(timer);
      ws.off("message", onMessage);
      resolve(message);
    };
    ws.on("message", onMessage);
  });
}

function rawX25519Public(key) {
  return key.export({ type: "spki", format: "der" }).subarray(-32);
}

function lengthPrefixed(value) {
  const bytes = Buffer.from(value, "utf8");
  const length = Buffer.alloc(4);
  length.writeUInt32BE(bytes.length);
  return Buffer.concat([length, bytes]);
}

function openPairingCode(privateKey, registered) {
  const binding = {
    registration_request_id: registered.registration_request_id,
    pairing_id: registered.pairing_id,
    remote_control_generation: registered.remote_control_generation,
    relay_epoch: registered.relay_epoch,
    expires_at_ms: registered.expires_at_ms,
  };
  const parsed = parseSealedCodeV1(registered.sealed_code_b64);
  const relayPublic = crypto.createPublicKey({
    key: Buffer.concat([X25519_SPKI_PREFIX, parsed.relay_ephemeral_public_key]),
    format: "der",
    type: "spki",
  });
  const shared = crypto.diffieHellman({ privateKey, publicKey: relayPublic });
  const info = Buffer.concat([
    Buffer.from("pinvou-code-wrap-key-v1", "ascii"),
    ...[
      binding.registration_request_id,
      binding.pairing_id,
      binding.remote_control_generation,
      binding.relay_epoch,
      binding.expires_at_ms,
    ].map(lengthPrefixed),
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

before(async () => {
  testRoot = await mkdtemp(join(await realpath(tmpdir()), "pinvou-relay-v2-test-"));
  telemetryDir = join(testRoot, "telemetry");
  webDir = join(testRoot, "web-dist");
  relayStatePath = join(testRoot, "relay-state.json");
  await mkdir(webDir, { recursive: true });
  await mkdir(join(webDir, "assets"), { recursive: true });
  await writeFile(join(webDir, "index.html"), "<!doctype html><title>PINVOU WebUI v2</title><main id=app></main>");
  await writeFile(join(webDir, "app.js"), "window.__pinvou_webui_v2__ = true;\n");
  await writeFile(join(webDir, "assets", "app-01234567.js"), "window.__hashed__ = true;\n");
  await writeFile(join(webDir, "assets", "h3c-banner.jpg"), "fixed-name-asset\n");
  relay = spawn(process.execPath, [join(relayDir, "server.js")], {
    cwd: relayDir,
    env: {
      ...process.env,
      PORT: String(port),
      PINVOU_REMOTE_PUBLIC_BASE_PATH: "/pinvou3/remote",
      PINVOU_REMOTE_WEB_DIR: webDir,
      PINVOU_REMOTE_STATE_PATH: relayStatePath,
      HEARTBEAT_INTERVAL_MS: "5000",
      PINVOU_TELEMETRY_DATA_DIR: telemetryDir,
      PINVOU_TELEMETRY_ENROLLMENT_TOKEN: enrollmentToken,
      PINVOU_TELEMETRY_DEVICE_PEPPER: "test-device-pepper-at-least-24-chars",
      PINVOU_STATS_ADMIN_PASSWORD: adminPassword,
      PINVOU_REMOTE_TRUSTED_PROXY_IPS: "127.0.0.1",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  await waitForOutput(relay, /pinvou remote relay listening/);
});

after(async () => {
  await stopRelay(relay);
  await rm(testRoot, { recursive: true, force: true });
});

test("serves the built WebUI SPA at the public base path", async () => {
  const root = await fetch(`${httpUrl}/pinvou3/remote/`);
  assert.equal(root.status, 200);
  assert.match(await root.text(), /PINVOU WebUI v2/);
  assert.equal(root.headers.get("cache-control"), "no-store");
  assert.match(root.headers.get("content-security-policy"), /connect-src 'self' ws: wss:/);
  assert.equal(root.headers.get("referrer-policy"), "no-referrer");

  const spa = await fetch(`${httpUrl}/pinvou3/remote/conversations/current`);
  assert.equal(spa.status, 200);
  assert.match(await spa.text(), /PINVOU WebUI v2/);

  const asset = await fetch(`${httpUrl}/pinvou3/remote/app.js`);
  assert.equal(asset.status, 200);
  assert.match(asset.headers.get("content-type"), /text\/javascript/);
  assert.equal(asset.headers.get("cache-control"), "no-cache");

  const hashedAsset = await fetch(`${httpUrl}/pinvou3/remote/assets/app-01234567.js`);
  assert.equal(hashedAsset.status, 200);
  assert.match(hashedAsset.headers.get("cache-control"), /immutable/);

  const fixedAsset = await fetch(`${httpUrl}/pinvou3/remote/assets/h3c-banner.jpg`);
  assert.equal(fixedAsset.status, 200);
  assert.equal(fixedAsset.headers.get("cache-control"), "no-cache");

  const missingAsset = await fetch(`${httpUrl}/pinvou3/remote/missing.css`);
  assert.equal(missingAsset.status, 404);
  const outside = await fetch(`${httpUrl}/not-the-webui`);
  assert.equal(outside.status, 404);
});

test("healthz exposes aggregate endpoint counters and no identifiers", async () => {
  const response = await fetch(`${httpUrl}/pinvou3/remote/healthz`);
  assert.equal(response.status, 200);
  const health = await response.json();
  assert.deepEqual(Object.keys(health).sort(), [
    "connected_endpoint_count",
    "desktop_controller_enabled",
    "desktop_offline_count",
    "desktop_open_count",
    "endpoint_count",
    "ok",
    "room_count",
    "unauthenticated_connection_count",
    "web_client_open_count",
    "ws_connection_count",
  ]);
  assert.equal(health.desktop_controller_enabled, false);
  assert.equal(JSON.stringify(health).includes("token"), false);
});

test("controller v1 fails with a specific error when its runtime is disabled", async (t) => {
  const socket = await openControllerSocket();
  t.after(() => closeSocket(socket));
  const unavailable = nextMessage(socket, "error");
  socket.send(JSON.stringify({
    v: 1,
    type: "desktop_pairing_register",
    request_id: "request-controller-disabled",
    registration_request_id: "registration-controller-disabled",
    endpoint_id: "endpoint-controller-disabled",
    remote_control_generation: "generation-controller-disabled",
    code_wrap_public_key_b64: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
    handshake_public_key_b64: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=",
    key_algorithm: "x25519",
    capabilities: ["pairing_v1"],
  }));
  assert.equal((await unavailable).code, "desktop_controller_unavailable");
  await waitForClose(socket);
});

test("disabled controller rejects v1 without opening or rewriting registry and tombstones", async () => {
  const controllerStatePath = join(testRoot, `disabled-controller-state-${Date.now()}.json`);
  const preserved = Buffer.from('{"opaque_registry_and_tombstones":"must-remain-byte-identical"}\n', "utf8");
  await writeFile(controllerStatePath, preserved);
  const isolated = await spawnRelay({
    PINVOU_DESKTOP_CONTROLLER_ENABLED: "0",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: controllerStatePath,
  });
  let socket;
  try {
    socket = await openControllerSocket(isolated.wsUrl);
    const unavailable = nextMessage(socket, "error");
    socket.send(JSON.stringify({
      v: 1,
      type: "desktop_pairing_register",
      request_id: "request-disabled-preserve-state",
      registration_request_id: "registration-disabled-preserve-state",
      endpoint_id: "endpoint-disabled-preserve-state",
      remote_control_generation: "generation-disabled-preserve-state",
      code_wrap_public_key_b64: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
      handshake_public_key_b64: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=",
      key_algorithm: "x25519",
      capabilities: ["pairing_v1"],
    }));
    assert.equal((await unavailable).code, "desktop_controller_unavailable");
    await waitForClose(socket);
  } finally {
    closeSocket(socket);
    await stopRelay(isolated.child);
  }
  assert.deepEqual(await readFile(controllerStatePath), preserved);
});

test("stats and telemetry keep their dedicated top-level paths", async () => {
  const stats = await fetch(`${httpUrl}/pinvou3/stats`);
  assert.equal(stats.status, 200);
  const telemetryHealth = await fetch(`${httpUrl}/pinvou3/telemetry/healthz`);
  assert.equal(telemetryHealth.status, 200);
  assert.deepEqual(await telemetryHealth.json(), { ok: true });
});

test("registers a persistent endpoint and blind-forwards only v2 message types", async (t) => {
  const endpointId = `endpoint_forward_${Date.now()}`;
  const accessToken = "access-token-forward-0000000000000001";
  const desktopSecret = "desktop-secret-forward-0000000000001";
  const desktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  const webConnected = nextMessage(desktop.ws, "web_client_connected");
  const web = await joinWeb(endpointId, accessToken, { streamEpoch: "epoch-a", afterSeq: 41 });
  t.after(() => { closeSocket(web.ws); closeSocket(desktop.ws); });

  assert.equal(desktop.registered.endpoint_id, endpointId);
  assert.equal(desktop.registered.web_client_connected, false);
  assert.equal(web.joined.desktop_connected, true);
  assert.match(web.joined.lease_id, /^lease_/);
  assert.deepEqual(await webConnected, {
    v: 2,
    type: "web_client_connected",
    endpoint_id: endpointId,
    lease_id: web.joined.lease_id,
    stream_epoch: "epoch-a",
    after_seq: 41,
  });

  const forwardedRpc = nextMessage(desktop.ws, "rpc_request");
  web.ws.send(JSON.stringify({
    v: 2,
    type: "rpc_request",
    lease_id: "attacker-controlled",
    id: "rpc-1",
    client_request_id: "rpc-1",
    command: "list_sessions",
    args: { offset: 3 },
    desktop_secret: "must-not-cross-relay",
  }));
  assert.deepEqual(await forwardedRpc, {
    v: 2,
    type: "rpc_request",
    endpoint_id: endpointId,
    lease_id: web.joined.lease_id,
    id: "rpc-1",
    client_request_id: "rpc-1",
    command: "list_sessions",
    args: { offset: 3 },
  });

  const response = nextMessage(web.ws, "rpc_response");
  desktop.ws.send(JSON.stringify({
    v: 2,
    type: "rpc_response",
    id: "rpc-1",
    ok: true,
    result: ["session-a"],
    access_token: "must-not-cross-relay",
  }));
  assert.deepEqual(await response, {
    v: 2,
    type: "rpc_response",
    endpoint_id: endpointId,
    lease_id: web.joined.lease_id,
    id: "rpc-1",
    ok: true,
    result: ["session-a"],
  });

  for (const type of ["event_subscribe", "event_unsubscribe", "client_ready"]) {
    const forwarded = nextMessage(desktop.ws, type);
    web.ws.send(JSON.stringify({ v: 2, type, event: "engine:event", after_seq: 4 }));
    assert.equal((await forwarded).lease_id, web.joined.lease_id);
  }
  for (const type of ["event", "stream_reset", "desktop_snapshot"]) {
    const forwarded = nextMessage(web.ws, type);
    desktop.ws.send(JSON.stringify({ v: 2, type, payload: { marker: type } }));
    const message = await forwarded;
    assert.equal(message.lease_id, web.joined.lease_id);
    assert.deepEqual(message.payload, { marker: type });
  }

  const unsupported = nextMessage(web.ws, "error");
  web.ws.send(JSON.stringify({ v: 2, type: "desktop_endpoint_revoke", endpoint_id: endpointId }));
  assert.equal((await unsupported).code, "invalid_desktop_secret");
});

test("a newer Web client takes over the endpoint lease", async (t) => {
  const endpointId = `endpoint_takeover_${Date.now()}`;
  const accessToken = "access-token-takeover-000000000000001";
  const desktopSecret = "desktop-secret-takeover-0000000000001";
  const desktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  const firstConnected = nextMessage(desktop.ws, "web_client_connected");
  const first = await joinWeb(endpointId, accessToken);
  await firstConnected;
  const replaced = nextMessage(first.ws, "endpoint_replaced");
  const secondConnected = nextMessage(desktop.ws, "web_client_connected");
  const second = await joinWeb(endpointId, accessToken, { afterSeq: 9 });
  t.after(() => { closeSocket(first.ws); closeSocket(second.ws); closeSocket(desktop.ws); });

  assert.equal((await replaced).lease_id, first.joined.lease_id);
  await waitForClose(first.ws);
  assert.notEqual(second.joined.lease_id, first.joined.lease_id);
  assert.equal((await secondConnected).lease_id, second.joined.lease_id);

  desktop.ws.send(JSON.stringify({
    v: 2,
    type: "rpc_response",
    lease_id: first.joined.lease_id,
    id: "stale-rpc",
    ok: true,
  }));
  await expectNoMessage(second.ws, "rpc_response");

  const current = nextMessage(second.ws, "rpc_response");
  desktop.ws.send(JSON.stringify({
    v: 2,
    type: "rpc_response",
    lease_id: second.joined.lease_id,
    id: "current-rpc",
    ok: true,
  }));
  assert.equal((await current).id, "current-rpc");
});

test("desktop can reconnect to the same endpoint within the offline TTL", async (t) => {
  const endpointId = `endpoint_reconnect_${Date.now()}`;
  const accessToken = "access-token-reconnect-00000000000001";
  const desktopSecret = "desktop-secret-reconnect-000000000001";
  const firstDesktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  const initialConnected = nextMessage(firstDesktop.ws, "web_client_connected");
  const web = await joinWeb(endpointId, accessToken, { streamEpoch: "epoch-r", afterSeq: 77 });
  await initialConnected;
  t.after(() => { closeSocket(web.ws); closeSocket(firstDesktop.ws); });

  const offline = nextMessage(web.ws, "desktop_connection_state");
  firstDesktop.ws.terminate();
  assert.equal((await offline).status, "offline");
  const offlineHealth = await (await fetch(`${httpUrl}/pinvou3/remote/healthz`)).json();
  assert.ok(offlineHealth.desktop_offline_count >= 1);

  const secondDesktopSocket = await openSocket();
  const registered = nextMessage(secondDesktopSocket, "desktop_endpoint_registered");
  const online = nextMessage(web.ws, "desktop_connection_state");
  const connected = nextMessage(secondDesktopSocket, "web_client_connected");
  secondDesktopSocket.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: endpointId,
    access_token: accessToken,
    desktop_secret: desktopSecret,
  }));
  t.after(() => closeSocket(secondDesktopSocket));
  assert.equal((await registered).web_client_connected, true);
  assert.equal((await online).status, "connected");
  assert.deepEqual(await connected, {
    v: 2,
    type: "web_client_connected",
    endpoint_id: endpointId,
    lease_id: web.joined.lease_id,
    stream_epoch: "epoch-r",
    after_seq: 77,
  });
});

test("invalid credentials cannot join or replace a desktop", async (t) => {
  const endpointId = `endpoint_auth_${Date.now()}`;
  const accessToken = "access-token-auth-000000000000000001";
  const desktopSecret = "desktop-secret-auth-000000000000001";
  const desktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  t.after(() => closeSocket(desktop.ws));

  const badWeb = await openSocket();
  const badWebError = nextMessage(badWeb, "error");
  badWeb.send(JSON.stringify({
    v: 2,
    type: "web_client_join",
    endpoint_id: endpointId,
    access_token: "wrong-token",
  }));
  assert.equal((await badWebError).code, "invalid_token");
  await waitForClose(badWeb);

  const badDesktop = await openSocket();
  const badDesktopError = nextMessage(badDesktop, "error");
  badDesktop.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: endpointId,
    access_token: accessToken,
    desktop_secret: "wrong-secret",
  }));
  assert.equal((await badDesktopError).code, "invalid_desktop_secret");
  await waitForClose(badDesktop);
  assert.equal(desktop.ws.readyState, desktop.ws.OPEN);
});

test("requires exact v2 messages and rejects legacy token aliases before authentication", async (t) => {
  const sockets = [];
  t.after(() => sockets.forEach(closeSocket));

  const missingVersion = await openSocket();
  sockets.push(missingVersion);
  const versionError = nextMessage(missingVersion, "error");
  missingVersion.send(JSON.stringify({
    type: "web_client_join",
    endpoint_id: "endpoint_missing_version",
    access_token: "access-token-missing-version-00000001",
  }));
  assert.equal((await versionError).code, "unsupported_protocol_version");
  await waitForClose(missingVersion);

  for (const alias of ["token", "pairing_token"]) {
    const socket = await openSocket();
    sockets.push(socket);
    const aliasError = nextMessage(socket, "error");
    socket.send(JSON.stringify({
      v: 2,
      type: "web_client_join",
      endpoint_id: `endpoint_legacy_${alias}`,
      [alias]: "legacy-token-must-not-authenticate",
    }));
    assert.equal((await aliasError).code, "legacy_token_alias_unsupported");
    await waitForClose(socket);
  }

  const mixedCredentials = await openSocket();
  sockets.push(mixedCredentials);
  const mixedError = nextMessage(mixedCredentials, "error");
  mixedCredentials.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: "endpoint_mixed_credentials",
    access_token: "access-token-current-0000000000000001",
    token: "legacy-token-must-not-be-tolerated",
    desktop_secret: "desktop-secret-current-0000000000001",
  }));
  assert.equal((await mixedError).code, "legacy_token_alias_unsupported");
  await waitForClose(mixedCredentials);
});

test("rejects non-v2 rpc, event, client_ready, and revoke after authentication", async (t) => {
  const endpointId = `endpoint_version_${Date.now()}`;
  const accessToken = "access-token-version-0000000000000001";
  const desktopSecret = "desktop-secret-version-0000000000001";
  const sockets = [];
  t.after(() => sockets.forEach(closeSocket));

  let desktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  sockets.push(desktop.ws);
  let connected = nextMessage(desktop.ws, "web_client_connected");
  let web = await joinWeb(endpointId, accessToken);
  sockets.push(web.ws);
  await connected;

  let notForwarded = expectNoMessage(desktop.ws, "rpc_request");
  let versionError = nextMessage(web.ws, "error");
  web.ws.send(JSON.stringify({
    type: "rpc_request",
    id: "rpc-without-version",
    command: "list_sessions",
    args: {},
  }));
  assert.equal((await versionError).code, "unsupported_protocol_version");
  await notForwarded;
  await waitForClose(web.ws);

  connected = nextMessage(desktop.ws, "web_client_connected");
  web = await joinWeb(endpointId, accessToken);
  sockets.push(web.ws);
  await connected;
  notForwarded = expectNoMessage(desktop.ws, "client_ready");
  versionError = nextMessage(web.ws, "error");
  web.ws.send(JSON.stringify({ v: 1, type: "client_ready" }));
  assert.equal((await versionError).code, "unsupported_protocol_version");
  await notForwarded;
  await waitForClose(web.ws);

  connected = nextMessage(desktop.ws, "web_client_connected");
  web = await joinWeb(endpointId, accessToken);
  sockets.push(web.ws);
  await connected;
  notForwarded = expectNoMessage(web.ws, "event");
  versionError = nextMessage(desktop.ws, "error");
  desktop.ws.send(JSON.stringify({
    v: "2",
    type: "event",
    payload: { must_not_cross: true },
  }));
  assert.equal((await versionError).code, "unsupported_protocol_version");
  await notForwarded;
  await waitForClose(desktop.ws);

  desktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  sockets.push(desktop.ws);
  versionError = nextMessage(desktop.ws, "error");
  desktop.ws.send(JSON.stringify({
    type: "desktop_endpoint_revoke",
    endpoint_id: endpointId,
    desktop_secret: desktopSecret,
  }));
  assert.equal((await versionError).code, "unsupported_protocol_version");
  await waitForClose(desktop.ws);

  const survivingEndpoint = await joinWeb(endpointId, accessToken);
  sockets.push(survivingEndpoint.ws);
  assert.equal(survivingEndpoint.joined.endpoint_id, endpointId);
});

test("desktop revoke invalidates the persistent link", async (t) => {
  const endpointId = `endpoint_revoke_${Date.now()}`;
  const accessToken = "access-token-revoke-00000000000001";
  const desktopSecret = "desktop-secret-revoke-000000000001";
  const desktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  const connected = nextMessage(desktop.ws, "web_client_connected");
  const web = await joinWeb(endpointId, accessToken);
  await connected;
  t.after(() => { closeSocket(web.ws); closeSocket(desktop.ws); });

  const revokedAck = nextMessage(desktop.ws, "desktop_endpoint_revoked");
  const revokedWeb = nextMessage(web.ws, "endpoint_revoked");
  desktop.ws.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_revoke",
    endpoint_id: endpointId,
    desktop_secret: desktopSecret,
    reason: "refreshed",
  }));
  assert.equal((await revokedAck).endpoint_id, endpointId);
  assert.equal((await revokedWeb).reason, "refreshed");

  const persistedState = JSON.parse(await readFile(relayStatePath, "utf8"));
  assert.equal(persistedState.version, 1);
  assert.ok(persistedState.revoked_endpoints.some((entry) => entry.endpoint_id === endpointId));

  const stale = await openSocket();
  const error = nextMessage(stale, "error");
  stale.send(JSON.stringify({
    v: 2,
    type: "web_client_join",
    endpoint_id: endpointId,
    access_token: accessToken,
  }));
  assert.equal((await error).code, "endpoint_not_found");
  closeSocket(stale);

  const staleDesktop = await openSocket();
  const staleDesktopError = nextMessage(staleDesktop, "error");
  staleDesktop.send(JSON.stringify({
    v: 2,
    type: "desktop_endpoint_register",
    endpoint_id: endpointId,
    access_token: accessToken,
    desktop_secret: desktopSecret,
  }));
  assert.equal((await staleDesktopError).code, "endpoint_revoked");
  await waitForClose(staleDesktop);
});

test("revoke tombstone survives Relay restart and permits a fresh endpoint id", async () => {
  const statePath = join(testRoot, `restart-revoke-${Date.now()}.json`);
  const endpointId = `endpoint_restart_revoke_${Date.now()}`;
  const accessToken = "access-token-restart-revoke-0000000001";
  const desktopSecret = "desktop-secret-restart-revoke-000000001";
  let isolated = await spawnRelay({ PINVOU_REMOTE_STATE_PATH: statePath });
  try {
    const desktop = await registerDesktop(
      endpointId,
      accessToken,
      desktopSecret,
      isolated.wsUrl,
    );
    const revoked = nextMessage(desktop.ws, "desktop_endpoint_revoked");
    desktop.ws.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: endpointId,
      desktop_secret: desktopSecret,
      reason: "restart-test",
    }));
    await revoked;
    await waitForClose(desktop.ws);
    await stopRelay(isolated.child);

    isolated = await spawnRelay({ PINVOU_REMOTE_STATE_PATH: statePath });
    const stale = await openSocket(isolated.wsUrl);
    const staleError = nextMessage(stale, "error");
    stale.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_register",
      endpoint_id: endpointId,
      access_token: accessToken,
      desktop_secret: desktopSecret,
    }));
    assert.equal((await staleError).code, "endpoint_revoked");
    await waitForClose(stale);

    const fresh = await registerDesktop(
      `${endpointId}_fresh`,
      "access-token-restart-fresh-00000000001",
      "desktop-secret-restart-fresh-000000001",
      isolated.wsUrl,
    );
    assert.match(fresh.registered.endpoint_id, /_fresh$/);
    const freshRevoked = nextMessage(fresh.ws, "desktop_endpoint_revoked");
    fresh.ws.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: `${endpointId}_fresh`,
      desktop_secret: "desktop-secret-restart-fresh-000000001",
    }));
    await freshRevoked;
    const updatedState = JSON.parse(await readFile(statePath, "utf8"));
    assert.deepEqual(
      updatedState.revoked_endpoints.map((entry) => entry.endpoint_id).sort(),
      [endpointId, `${endpointId}_fresh`].sort(),
    );
  } finally {
    await stopRelay(isolated.child);
  }
});

test("revoke fails closed without ACK when the tombstone cannot be persisted", async () => {
  const blockedParent = join(testRoot, `blocked-state-parent-${Date.now()}`);
  const statePath = join(blockedParent, "relay-state.json");
  const isolated = await spawnRelay({ PINVOU_REMOTE_STATE_PATH: statePath });
  const endpointId = `endpoint_revoke_failure_${Date.now()}`;
  const accessToken = "access-token-revoke-failure-000000001";
  const desktopSecret = "desktop-secret-revoke-failure-00000001";
  try {
    const desktop = await registerDesktop(
      endpointId,
      accessToken,
      desktopSecret,
      isolated.wsUrl,
    );
    await writeFile(blockedParent, "not-a-directory");
    const persistenceError = nextMessage(desktop.ws, "error");
    desktop.ws.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: endpointId,
      desktop_secret: desktopSecret,
    }));
    assert.equal((await persistenceError).code, "revoke_persistence_failed");
    await waitForClose(desktop.ws);

    await rm(blockedParent, { force: true });
    const reconnect = await registerDesktop(
      endpointId,
      accessToken,
      desktopSecret,
      isolated.wsUrl,
    );
    assert.equal(reconnect.registered.endpoint_id, endpointId);
    closeSocket(reconnect.ws);
  } finally {
    await stopRelay(isolated.child);
  }
});

test("tombstone capacity stays inside what the state limit can hold", async () => {
  // MAX_REVOKED_ENDPOINTS is clamped to what MAX_RELAY_STATE_BYTES can hold
  // (one tombstone per ~256 bytes): revocations past the clamp refuse through
  // the normal capacity path instead of persisting entries until the size
  // check fails every further revoke.
  const isolated = await spawnRelay({
    MAX_RELAY_STATE_BYTES: "2048",
    MAX_REVOKED_ENDPOINTS: "100",
    ENDPOINT_CREATE_LIMIT: "100",
  });
  try {
    for (let i = 0; i < 8; i += 1) {
      const endpointId = `endpoint_clamp_${String(i).padStart(2, "0")}`;
      const desktop = await registerDesktop(
        endpointId,
        `clamp-token-${String(i).padStart(22, "0")}`,
        `clamp-secret-${String(i).padStart(22, "0")}`,
        isolated.wsUrl,
      );
      assert.equal(desktop.registered.endpoint_id, endpointId);
      const revokedAck = nextMessage(desktop.ws, "desktop_endpoint_revoked");
      desktop.ws.send(JSON.stringify({
        v: 2,
        type: "desktop_endpoint_revoke",
        endpoint_id: endpointId,
      }));
      await revokedAck;
      await waitForClose(desktop.ws);
    }
    const over = await registerDesktop(
      "endpoint_clamp_over_capacity_x",
      "clamp-token-over-capacity-000000000x",
      "clamp-secret-over-capacity-00000000x",
      isolated.wsUrl,
    );
    assert.equal(over.registered.endpoint_id, "endpoint_clamp_over_capacity_x");
    const refusal = nextMessage(over.ws, "error");
    over.ws.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: "endpoint_clamp_over_capacity_x",
    }));
    // Capacity exhaustion gets its own code: it is an expected operational
    // condition, not a persistence failure, and must stay distinguishable
    // from a broken disk.
    assert.equal((await refusal).code, "revoked_endpoint_capacity_reached");
    await waitForClose(over.ws);

    const persisted = JSON.parse(await readFile(isolated.statePath, "utf8"));
    assert.equal(persisted.revoked_endpoints.length, 8);
    // The refused endpoint stays registered and usable; only the tombstone
    // is missing.
    const health = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(health.endpoint_count, 1);
  } finally {
    await stopRelay(isolated.child);
  }
});

test("a state above the clamped capacity loads and refuses only new revocations", async () => {
  // The tombstone clamp ties MAX_REVOKED_ENDPOINTS to MAX_RELAY_STATE_BYTES.
  // A state file accumulated above the clamp must still load — the size
  // limit is the real bound, every loaded tombstone stays enforced, and
  // refusing to start would brick the relay on a valid file. Only new
  // revocations are refused, with the capacity code instead of a persistence
  // failure.
  const statePath = join(testRoot, `clamped-state-${Date.now()}.json`);
  const entries = [];
  for (let i = 0; i < 9; i += 1) {
    entries.push({ endpoint_id: `clamp${i}`, revoked_at: "2026-01-01T00:00:00Z" });
  }
  await writeFile(statePath, JSON.stringify({ version: 1, revoked_endpoints: entries }));
  const isolated = await spawnRelay({
    PINVOU_REMOTE_STATE_PATH: statePath,
    MAX_RELAY_STATE_BYTES: "2048",
    ENDPOINT_CREATE_LIMIT: "100",
  });
  try {
    assert.match(isolated.output(), /revoked_endpoints=9/u);
    // A loaded tombstone stays enforced.
    const stale = await openSocket(isolated.wsUrl);
    const staleError = nextMessage(stale, "error");
    stale.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_register",
      endpoint_id: "clamp3",
      access_token: "clamped-stale-access-token-000001",
      desktop_secret: "clamped-stale-desktop-secret-0001",
    }));
    assert.equal((await staleError).code, "endpoint_revoked");
    await waitForClose(stale);

    const desktop = await registerDesktop(
      "endpoint_clamped_capacity_x",
      "clamped-token-0000000000000000000x",
      "clamped-secret-000000000000000000x",
      isolated.wsUrl,
    );
    assert.equal(desktop.registered.endpoint_id, "endpoint_clamped_capacity_x");
    const refusal = nextMessage(desktop.ws, "error");
    desktop.ws.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: "endpoint_clamped_capacity_x",
    }));
    assert.equal((await refusal).code, "revoked_endpoint_capacity_reached");
    await waitForClose(desktop.ws);
    const persisted = JSON.parse(await readFile(statePath, "utf8"));
    assert.equal(persisted.revoked_endpoints.length, 9);
  } finally {
    await stopRelay(isolated.child);
  }
});

test("forwards payloads larger than the previous 1 MiB limit", async (t) => {
  const endpointId = `endpoint_payload_${Date.now()}`;
  const accessToken = "access-token-payload-0000000000001";
  const desktopSecret = "desktop-secret-payload-00000000001";
  const desktop = await registerDesktop(endpointId, accessToken, desktopSecret);
  const connected = nextMessage(desktop.ws, "web_client_connected");
  const web = await joinWeb(endpointId, accessToken);
  await connected;
  t.after(() => { closeSocket(web.ws); closeSocket(desktop.ws); });
  const snapshot = nextMessage(web.ws, "desktop_snapshot", 5000);
  desktop.ws.send(JSON.stringify({
    v: 2,
    type: "desktop_snapshot",
    payload: { text: "x".repeat(1_200_000) },
  }));
  assert.equal((await snapshot).payload.text.length, 1_200_000);
});

test("closes a websocket that does not authenticate in time", async () => {
  const isolated = await spawnRelay({ WS_AUTH_TIMEOUT_MS: "1000" });
  try {
    const ws = await openSocket(isolated.wsUrl);
    const error = nextMessage(ws, "error", 2500);
    assert.equal((await error).code, "authentication_timeout");
    await waitForClose(ws);
  } finally {
    await stopRelay(isolated.child);
  }
});

test("a cross-endpoint revoker keeps its own session", async () => {
  const isolated = await spawnRelay({ WS_CONNECT_LIMIT: "20" });
  let desktopA;
  let desktopB;
  try {
    desktopA = await registerDesktop(
      "endpoint_revoke_self_a",
      "revoke-self-token-a-0000000000001",
      "revoke-self-secret-a-000000000001",
      isolated.wsUrl,
    );
    await desktopA.registered;
    desktopB = await registerDesktop(
      "endpoint_revoke_self_b",
      "revoke-self-token-b-0000000000001",
      "revoke-self-secret-b-000000000001",
      isolated.wsUrl,
    );
    await desktopB.registered;
    // A's desktop revokes endpoint B with B's secret.
    const revokedAck = nextMessage(desktopA.ws, "desktop_endpoint_revoked");
    desktopA.ws.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: "endpoint_revoke_self_b",
      desktop_secret: "revoke-self-secret-b-000000000001",
    }));
    await revokedAck;
    // A's own endpoint must survive with its desktop still connected.
    await new Promise((resolve) => setTimeout(resolve, 300));
    const health = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(health.endpoint_count, 1);
    assert.equal(health.desktop_offline_count, 0);
    // The surviving session must still work: B is gone, so an rpc_request
    // from A's desktop-role socket is refused as unsupported_message_type
    // (desktops may not originate rpc_request) — an answered error proving
    // the socket is alive, not a dead one.
    const alive = nextMessage(desktopA.ws, "error");
    desktopA.ws.send(JSON.stringify({
      v: 2,
      type: "rpc_request",
      request_id: "post-revoke-1",
      payload: { tool: "ping" },
    }));
    assert.equal((await alive).code, "unsupported_message_type");
  } finally {
    closeSocket(desktopA.ws);
    closeSocket(desktopB?.ws);
    await stopRelay(isolated.child);
  }
});

test("an unauthenticated cross-endpoint revoker stays bounded by the auth timeout", async () => {
  // The secret authorizes the revoke itself, but a revoker that never
  // authenticated keeps its socket after a cross-endpoint revoke — so the
  // authentication timer, cleared when the revoke started, must be re-armed.
  // Without that, the socket holds a connection (and a healthz
  // unauthenticated counter) forever.
  const isolated = await spawnRelay({ WS_AUTH_TIMEOUT_MS: "1500", WS_CONNECT_LIMIT: "20" });
  let desktopB;
  let revoker;
  try {
    desktopB = await registerDesktop(
      "endpoint_revoke_unk_b",
      "revoke-unk-token-b-0000000000001",
      "revoke-unk-secret-b-0000000000001",
      isolated.wsUrl,
    );
    await desktopB.registered;
    revoker = await openSocket(isolated.wsUrl);
    const revokedAck = nextMessage(revoker, "desktop_endpoint_revoked");
    revoker.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_revoke",
      endpoint_id: "endpoint_revoke_unk_b",
      desktop_secret: "revoke-unk-secret-b-0000000000001",
    }));
    await revokedAck;
    // The endpoint is gone; the revoker has no credentials left to
    // authenticate with and must be closed by the re-armed timeout.
    const timeoutError = nextMessage(revoker, "error", 5000);
    assert.equal((await timeoutError).code, "authentication_timeout");
    await waitForClose(revoker, 6000);
  } finally {
    closeSocket(revoker);
    closeSocket(desktopB?.ws);
    await stopRelay(isolated.child);
  }
});

test("expires offline endpoints after TTL and releases endpoint capacity", async () => {
  const isolated = await spawnRelay({
    MAX_ENDPOINTS: "1",
    ENDPOINT_CREATE_LIMIT: "10",
    ENDPOINT_OFFLINE_TTL_MS: "1000",
  });
  const first = await registerDesktop(
    "endpoint_ttl_one",
    "ttl-token-one-000000000000000000001",
    "ttl-secret-one-00000000000000000001",
    isolated.wsUrl,
  );
  const connected = nextMessage(first.ws, "web_client_connected");
  const web = await joinWeb(
    "endpoint_ttl_one",
    "ttl-token-one-000000000000000000001",
    {},
    isolated.wsUrl,
  );
  await connected;
  try {
    const offline = nextMessage(web.ws, "desktop_connection_state");
    first.ws.terminate();
    assert.equal((await offline).status, "offline");

    const retained = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(retained.endpoint_count, 1);
    assert.equal(retained.desktop_offline_count, 1);

    await new Promise((resolve) => setTimeout(resolve, 1100));
    const expired = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(expired.endpoint_count, 0);
    await waitForClose(web.ws);

    const second = await registerDesktop(
      "endpoint_ttl_two",
      "ttl-token-two-000000000000000000002",
      "ttl-secret-two-00000000000000000002",
      isolated.wsUrl,
    );
    assert.equal(second.registered.endpoint_id, "endpoint_ttl_two");
    closeSocket(second.ws);
  } finally {
    closeSocket(web.ws);
    closeSocket(first.ws);
    await stopRelay(isolated.child);
  }
});

test("allows native and approved browser origins while rejecting other browser origins", async () => {
  const isolated = await spawnRelay({
    PINVOU_REMOTE_ALLOWED_WEB_ORIGINS: "https://trusted.example",
    WS_CONNECT_LIMIT: "20",
  });
  const sockets = [];
  try {
    assert.equal(
      await rejectedUpgrade(isolated.wsUrl, { origin: "https://evil.example" }),
      403,
    );
    sockets.push(await openSocket(isolated.wsUrl, { origin: "https://trusted.example" }));
    sockets.push(await openSocket(isolated.wsUrl, { origin: isolated.httpUrl }));
    sockets.push(await openSocket(isolated.wsUrl));
  } finally {
    sockets.forEach(closeSocket);
    await stopRelay(isolated.child);
  }
});

test("rate limits websocket ingress by message count", async () => {
  const isolated = await spawnRelay({
    WS_INGRESS_MESSAGE_LIMIT: "2",
    WS_INGRESS_BYTE_LIMIT: "1048576",
  });
  const desktop = await registerDesktop(
    "endpoint_ingress_count",
    "ingress-count-token-0000000000000001",
    "ingress-count-secret-000000000000001",
    isolated.wsUrl,
  );
  try {
    const unsupported = nextMessage(desktop.ws, "error");
    desktop.ws.send(JSON.stringify({ v: 2, type: "unsupported-one" }));
    assert.equal((await unsupported).code, "unsupported_message_type");

    const limited = nextMessage(desktop.ws, "error");
    desktop.ws.send(JSON.stringify({ v: 2, type: "unsupported-two" }));
    assert.equal((await limited).code, "ingress_rate_limited");
    await waitForClose(desktop.ws);
  } finally {
    closeSocket(desktop.ws);
    await stopRelay(isolated.child);
  }
});

test("rate limits websocket ingress by byte count", async () => {
  const isolated = await spawnRelay({
    WS_INGRESS_MESSAGE_LIMIT: "100",
    WS_INGRESS_BYTE_LIMIT: "1024",
  });
  const desktop = await registerDesktop(
    "endpoint_ingress_bytes",
    "ingress-bytes-token-0000000000000001",
    "ingress-bytes-secret-000000000000001",
    isolated.wsUrl,
  );
  try {
    const limited = nextMessage(desktop.ws, "error");
    desktop.ws.send(JSON.stringify({
      v: 2,
      type: "unsupported-large",
      padding: "x".repeat(1024),
    }));
    assert.equal((await limited).code, "ingress_rate_limited");
    await waitForClose(desktop.ws);
  } finally {
    closeSocket(desktop.ws);
    await stopRelay(isolated.child);
  }
});

test("clamps payload and buffer floors so a legal 2 MiB response is forwarded", async () => {
  const isolated = await spawnRelay({
    MAX_PAYLOAD_BYTES: "1024",
    WS_MAX_BUFFERED_BYTES: "1024",
  });
  const desktop = await registerDesktop(
    "endpoint_buffer_high_water",
    "buffer-token-0000000000000000000001",
    "buffer-secret-000000000000000000001",
    isolated.wsUrl,
  );
  const connected = nextMessage(desktop.ws, "web_client_connected");
  const web = await joinWeb(
    "endpoint_buffer_high_water",
    "buffer-token-0000000000000000000001",
    {},
    isolated.wsUrl,
  );
  await connected;
  try {
    const snapshot = nextMessage(web.ws, "desktop_snapshot", 5000);
    desktop.ws.send(JSON.stringify({
      v: 2,
      type: "desktop_snapshot",
      payload: { text: "x".repeat(2 * 1024 * 1024) },
    }));
    assert.equal((await snapshot).payload.text.length, 2 * 1024 * 1024);
    assert.equal(web.ws.readyState, web.ws.OPEN);
    assert.equal(desktop.ws.readyState, desktop.ws.OPEN);
  } finally {
    closeSocket(web.ws);
    closeSocket(desktop.ws);
    await stopRelay(isolated.child);
  }
});

test("caps concurrent websocket upgrades", async () => {
  const isolated = await spawnRelay({ MAX_WS_CONNECTIONS: "2", WS_CONNECT_LIMIT: "20" });
  const first = await openSocket(isolated.wsUrl);
  const second = await openSocket(isolated.wsUrl);
  try {
    assert.equal(await rejectedUpgrade(isolated.wsUrl), 503);
  } finally {
    closeSocket(first);
    closeSocket(second);
    await stopRelay(isolated.child);
  }
});

test("rate limits websocket upgrades per client", async () => {
  const isolated = await spawnRelay({ WS_CONNECT_LIMIT: "1", WS_CONNECT_WINDOW_MS: "60000" });
  const first = await openSocket(isolated.wsUrl);
  try {
    assert.equal(await rejectedUpgrade(isolated.wsUrl), 429);
  } finally {
    closeSocket(first);
    await stopRelay(isolated.child);
  }
});

test("caps endpoint creation while allowing an existing endpoint to reconnect", async () => {
  const isolated = await spawnRelay({ MAX_ENDPOINTS: "1", ENDPOINT_CREATE_LIMIT: "10" });
  const first = await registerDesktop(
    "endpoint_capacity_one",
    "capacity-token-one-0000000000000001",
    "capacity-secret-one-000000000000001",
    isolated.wsUrl,
  );
  try {
    first.ws.terminate();
    const denied = await openSocket(isolated.wsUrl);
    const deniedError = nextMessage(denied, "error");
    denied.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_register",
      endpoint_id: "endpoint_capacity_two",
      access_token: "capacity-token-two-0000000000000002",
      desktop_secret: "capacity-secret-two-000000000000002",
    }));
    assert.equal((await deniedError).code, "endpoint_capacity_reached");

    const reconnect = await registerDesktop(
      "endpoint_capacity_one",
      "capacity-token-one-0000000000000001",
      "capacity-secret-one-000000000000001",
      isolated.wsUrl,
    );
    assert.equal(reconnect.registered.endpoint_id, "endpoint_capacity_one");
    closeSocket(reconnect.ws);
  } finally {
    closeSocket(first.ws);
    await stopRelay(isolated.child);
  }
});

test("rate limits new endpoint creation per client", async () => {
  const isolated = await spawnRelay({ ENDPOINT_CREATE_LIMIT: "1", ENDPOINT_CREATE_WINDOW_MS: "60000" });
  const first = await registerDesktop(
    "endpoint_rate_one",
    "rate-token-one-00000000000000000001",
    "rate-secret-one-000000000000000001",
    isolated.wsUrl,
  );
  try {
    const denied = await openSocket(isolated.wsUrl);
    const deniedError = nextMessage(denied, "error");
    denied.send(JSON.stringify({
      v: 2,
      type: "desktop_endpoint_register",
      endpoint_id: "endpoint_rate_two",
      access_token: "rate-token-two-00000000000000000002",
      desktop_secret: "rate-secret-two-000000000000000002",
    }));
    assert.equal((await deniedError).code, "endpoint_creation_rate_limited");
  } finally {
    closeSocket(first.ws);
    await stopRelay(isolated.child);
  }
});

test("proxy allowlist keeps local health checks available", async () => {
  const isolated = await spawnRelay({ PINVOU_REMOTE_ALLOWED_PROXY_IPS: "203.0.113.10" });
  try {
    const response = await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`);
    assert.equal(response.status, 200);
  } finally {
    await stopRelay(isolated.child);
  }
});

test("trusted proxy malformed sources never crash the enabled controller or bypass its peer limit", async () => {
  const isolated = await spawnRelay(controllerEnvironment("xff", {
    PINVOU_REMOTE_TRUSTED_PROXY_IPS: "127.0.0.1", WS_CONNECT_LIMIT: "3", WS_CONNECT_WINDOW_MS: "60000",
  }));
  const sockets = [];
  try {
    for (const forwarded of ["x".repeat(200), "198.51.100.5, invalid", "x".repeat(4097)]) {
      const socket = await openSocket(isolated.wsUrl, { headers: { "x-forwarded-for": forwarded } });
      sockets.push(socket);
    }
    assert.equal(await rejectedUpgrade(isolated.wsUrl, { headers: { "x-forwarded-for": "another-invalid" } }), 429);
    const ipv4 = await openSocket(isolated.wsUrl, { headers: { "x-forwarded-for": "invalid, 198.51.100.5" } });
    const ipv6 = await openSocket(isolated.wsUrl, { headers: { "x-forwarded-for": "2001:db8::1" } });
    sockets.push(ipv4, ipv6);
    const response = nextMessage(ipv6, "error");
    ipv6.send(JSON.stringify({ v: 2, type: "not_authenticated" }));
    assert.equal((await response).code, "authentication_required");
    assert.equal(isolated.child.exitCode, null);
    assert.equal((await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).status, 200);
  } finally {
    sockets.forEach(closeSocket);
    await stopRelay(isolated.child);
  }
});

test("survives a websocket protocol violation that emits an error event", async () => {
  const isolated = await spawnRelay({});
  let ws;
  let controller;
  try {
    ws = await openSocket(isolated.wsUrl);
    // A frame over maxPayload makes ws emit "error" on the socket; without a
    // listener the unhandled event crashed the whole relay process.
    ws.send("x".repeat(4 * 1024 * 1024 + 8));
    await waitForClose(ws);
    // The containment must stay observable: one fixed line, no frame content.
    // The line reaches this process through a pipe that drains independently
    // of the close handshake, so wait for the evidence before asserting it.
    await waitForError(
      isolated,
      /\[relay-error\] websocket_protocol_violation code=WS_ERR_UNSUPPORTED_MESSAGE_LENGTH/u,
    );
    // The controller lane shares the connection handler and its error guard.
    // Baseline the log count before the violation: the line can land while
    // the close handshake is still draining.
    const violations = () => isolated.errors().match(/websocket_protocol_violation/gu)?.length ?? 0;
    const loggedBefore = violations();
    controller = await openControllerSocket(isolated.wsUrl);
    controller.send("x".repeat(4 * 1024 * 1024 + 8));
    await waitForClose(controller, 5000);
    const deadline = Date.now() + 5000;
    while (violations() <= loggedBefore && Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
    assert.ok(violations() > loggedBefore, "controller lane protocol violation was not logged");
    // The relay is unharmed: a follower connects and the process stays up.
    const follower = await openSocket(isolated.wsUrl);
    closeSocket(follower);
    assert.equal(isolated.child.exitCode, null);
  } finally {
    closeSocket(ws);
    closeSocket(controller);
    await stopRelay(isolated.child);
  }
});

test("rejects malformed Host headers instead of crashing", async () => {
  const isolated = await spawnRelay({});
  try {
    const httpBanner = await rawRequest(
      isolated.port,
      "GET /pinvou3/remote/healthz HTTP/1.1\r\n"
      + "Host: ::1:99999\r\n"
      + "Connection: close\r\n\r\n",
    );
    assert.match(httpBanner, /400 Bad Request/u);
    assert.match(httpBanner, /bad request/u);
    const upgradeBanner = await rawRequest(
      isolated.port,
      "GET /pinvou3/remote/ws HTTP/1.1\r\n"
      + "Host: ::1:99999\r\n"
      + "Upgrade: websocket\r\nConnection: Upgrade\r\n"
      + "Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==\r\n"
      + "Sec-WebSocket-Version: 13\r\n\r\n",
    );
    assert.match(upgradeBanner, /400 Bad Request/u);
    assert.match(upgradeBanner, /malformed host header/u);
    const follower = await openSocket(isolated.wsUrl);
    closeSocket(follower);
    assert.equal(isolated.child.exitCode, null);
  } finally {
    await stopRelay(isolated.child);
  }
});

test("matches a mixed-case IPv4-mapped trust entry with the plain peer address", async () => {
  // The trust entry is spelled with an uppercase mapped prefix; the loopback
  // peer reports plain "127.0.0.1". Canonical reduction must make trust hold,
  // so the forwarded connection opens its own bucket (the second forwarded
  // attempt hits the 429) while the untouched peer bucket still admits a
  // plain connection.
  const isolated = await spawnRelay({
    WS_CONNECT_LIMIT: "1",
    PINVOU_REMOTE_TRUSTED_PROXY_IPS: "::FFFF:127.0.0.1",
  });
  let first;
  try {
    first = await openSocket(isolated.wsUrl, { headers: { "x-forwarded-for": "198.51.100.9" } });
    assert.equal(await rejectedUpgrade(isolated.wsUrl, {
      headers: { "x-forwarded-for": "198.51.100.9" },
    }), 429);
    const plain = await openSocket(isolated.wsUrl);
    closeSocket(plain);
  } finally {
    closeSocket(first);
    await stopRelay(isolated.child);
  }
});

test("expired Web endpoint releases its exact interaction slot before deletion", async () => {
  const isolated = await spawnRelay(controllerEnvironment("expired-slot", { ENDPOINT_OFFLINE_TTL_MS: "1000" }));
  const sockets = [];
  try {
    const endpointId = "endpoint-expired-slot";
    const accessToken = "expired-slot-access-token-at-least-32";
    const desktopSecret = "expired-slot-desktop-secret-at-least-32";
    const desktop = await registerDesktop(endpointId, accessToken, desktopSecret, isolated.wsUrl, 15000);
    const web = await joinWeb(endpointId, accessToken, {}, isolated.wsUrl);
    sockets.push(desktop.ws, web.ws);
    desktop.ws.terminate();
    await waitForClose(desktop.ws);
    // The real heartbeat performs TTL expiry; wait on the observable Web close,
    // not a timing assumption or direct call into the cleanup implementation.
    await waitForClose(web.ws, 10000);
    const reconnect = await registerDesktop(endpointId, accessToken, desktopSecret, isolated.wsUrl, 15000, true);
    sockets.push(reconnect.ws);
    const codeWrap = crypto.generateKeyPairSync("x25519");
    const handshakeB = crypto.generateKeyPairSync("x25519");
    const registered = nextMessageOneOf(reconnect.ws, ["desktop_pairing_registered", "error"], 15000);
    reconnect.ws.send(JSON.stringify({ v: 1, type: "desktop_pairing_register",
      request_id: "request-expired-register", registration_request_id: "expired-register-0001",
      endpoint_id: endpointId, remote_control_generation: "expired-generation-0001",
      code_wrap_public_key_b64: rawX25519Public(codeWrap.publicKey).toString("base64"),
      handshake_public_key_b64: rawX25519Public(handshakeB.publicKey).toString("base64"), key_algorithm: "x25519",
      capabilities: ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1"] }));
    const registration = await registered;
    assert.equal(registration.type, "desktop_pairing_registered", registration.code);
    const controller = await openControllerSocket(isolated.wsUrl);
    sockets.push(controller);
    const reserved = nextMessageOneOf(controller, ["controller_pairing_reserved", "error"], 10000);
    const handshakeA = crypto.generateKeyPairSync("x25519");
    controller.send(JSON.stringify({ v: 1, type: "controller_pairing_claim",
      request_id: "request-expired-claim", claim_request_id: "expired-claim-0001",
      pairing_code: openPairingCode(codeWrap.privateKey, registration),
      handshake_public_key_b64: rawX25519Public(handshakeA.publicKey).toString("base64"), key_algorithm: "x25519",
      capabilities: ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1"] }));
    const claimed = await reserved;
    assert.equal(claimed.type, "controller_pairing_reserved", claimed.code);
  } finally {
    sockets.forEach(closeSocket);
    await stopRelay(isolated.child);
  }
});

test("official controller fails startup until trusted live claim wiring exists", async () => {
  const isolatedPort = await getFreePort();
  const child = spawn(process.execPath, [join(relayDir, "server.js")], {
    cwd: relayDir, env: { ...process.env, PORT: String(isolatedPort), ...controllerEnvironment("official-disabled", {
      NODE_ENV: "production", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "official", PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "0",
    }) }, stdio: ["ignore", "pipe", "pipe"], windowsHide: true,
  });
  let output = "";
  let errors = "";
  child.stdout.on("data", (chunk) => { output += String(chunk); });
  child.stderr.on("data", (chunk) => { errors += String(chunk); });
  try {
    await waitForExit(child, 10000);
    assert.notEqual(child.exitCode, 0);
    assert.match(errors, /official_controller_claim_dispatcher_unavailable/u);
    assert.doesNotMatch(output, /pinvou remote relay listening/u);
  } finally {
    await stopRelay(child);
  }
});

test("actual controller startup rejects both public pairing HMAC examples before listening", { timeout: 150_000 }, async () => {
  for (const [index, key] of [
    "replace-with-at-least-32-random-bytes",
    "replace-with-at-least-32-independent-random-bytes",
  ].entries()) {
    const child = spawn(process.execPath, [join(relayDir, "server.js")], {
      cwd: relayDir,
      env: {
        ...process.env, PORT: String(await getFreePort()),
        ...controllerEnvironment(`pairing-example-${index}`, {
          NODE_ENV: "production",
          PINVOU_REMOTE_HOST: "127.0.0.1",
          PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted",
          PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "0",
          PINVOU_REMOTE_ENROLLMENT_HMAC_PREVIOUS_KEYS_JSON: "[]",
          PINVOU_REMOTE_PAIRING_HMAC_KEY: key,
        }),
      },
      stdio: ["ignore", "pipe", "pipe"], windowsHide: true,
    });
    let output = "";
    let errors = "";
    child.stdout.on("data", (chunk) => { output += String(chunk); });
    child.stderr.on("data", (chunk) => { errors += String(chunk); });
    try {
      await waitForExit(child, 60_000);
      assert.equal(child.exitCode, 1);
      assert.equal(child.signalCode, null, "failure must come from validation, not termination");
      assert.match(errors, /pairing_digest_key_invalid/u);
      assert.doesNotMatch(output, /pinvou remote relay listening/u);
      assert.ok(!output.includes(key) && !errors.includes(key), "startup diagnostics must not echo key material");
    } finally {
      await stopRelay(child);
    }
  }
});

test("invalid or partially invalid proxy IP configuration fails startup instead of opening the allowlist", async () => {
  for (const variable of ["PINVOU_REMOTE_ALLOWED_PROXY_IPS", "PINVOU_REMOTE_TRUSTED_PROXY_IPS"]) {
    // ",,," has visible content but yields no entries: a broken edit must
    // fail closed at startup, never silently mean an empty allowlist.
    for (const value of ["not-an-ip", "127.0.0.1,not-an-ip", ",,,"]) {
      const child = spawn(process.execPath, [join(relayDir, "server.js")], {
        cwd: relayDir, env: { ...process.env, PORT: String(await getFreePort()),
          PINVOU_REMOTE_STATE_PATH: join(testRoot, `proxy-invalid-${variable}-${Date.now()}.json`), [variable]: value },
        stdio: ["ignore", "pipe", "pipe"], windowsHide: true,
      });
      let output = "";
      let errors = "";
      child.stdout.on("data", (chunk) => { output += String(chunk); });
      child.stderr.on("data", (chunk) => { errors += String(chunk); });
      try {
        await waitForExit(child, 10000);
        assert.notEqual(child.exitCode, 0);
        assert.match(errors, /proxy_ip_configuration_invalid/u);
        assert.doesNotMatch(output, /pinvou remote relay listening/u);
      } finally { await stopRelay(child); }
    }
  }
});

test("an invalid PORT reports the fixed line instead of a stack", async () => {
  // listen() validates the port synchronously, so an invalid value would die
  // inside the call with a RangeError whose stack prints the module path.
  for (const value of ["abc", "99999", "-1", "1.5", "0"]) {
    const isolated = await spawnRelay({ PORT: value }, { awaitListening: false });
    try {
      await waitForExit(isolated.child, 10000);
      assert.notEqual(isolated.child.exitCode, 0);
      assert.equal(
        isolated.errors().trim(),
        "[relay-error] invalid PORT: use an integer from 1 to 65535",
      );
    } finally {
      await stopRelay(isolated.child);
    }
  }
});

test("a failed bind reports a fixed error without the bind address", async () => {
  const first = await spawnRelay({});
  try {
    const second = await spawnRelay({ PORT: String(first.port) }, { awaitListening: false });
    try {
      await waitForExit(second.child, 10000);
      assert.notEqual(second.child.exitCode, 0);
      // The relay may print operational notices (telemetry configuration);
      // only [relay-error] lines are pinned, and there must be exactly one.
      const errorLines = second.errors().split("\n").filter((line) => line.startsWith("[relay-error]"));
      assert.deepEqual(errorLines, ["[relay-error] listen_failed code=EADDRINUSE"]);
      assert.ok(!second.errors().includes(String(first.port)), "bind address echoed into stderr");
    } finally {
      await stopRelay(second.child);
    }
  } finally {
    await stopRelay(first.child);
  }
});

test("real Relay heartbeat retries failed reservation invalidation and releases the slot", async () => {
  const statePath = join(testRoot, `controller-timeout-${Date.now()}.json`);
  const controllerStatePath = join(testRoot, `controller-timeout-state-${Date.now()}`, "state.json");
  const signing = crypto.generateKeyPairSync("ed25519");
  const isolated = await spawnRelay({
    NODE_ENV: "development",
    PINVOU_DESKTOP_CONTROLLER_ENABLED: "1",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "1",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: controllerStatePath,
    PINVOU_REMOTE_STATE_PATH: statePath,
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "enrollment-key-timeout-1",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "enrollment-key-material-at-least-32-bytes",
    PINVOU_REMOTE_PAIRING_HMAC_KEY_ID: "pairing-key-timeout-1",
    PINVOU_REMOTE_PAIRING_HMAC_KEY: "pairing-key-material-independent-at-least-32-bytes",
    PINVOU_REMOTE_RELAY_SIGNING_KEY_ID: "relay-signing-key-timeout-1",
    PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64: signing.privateKey.export({
      type: "pkcs8", format: "der",
    }).toString("base64"),
  }, { preload: new URL("./fixtures/reservation-invalidation-failure.js", import.meta.url).href });
  const sockets = [];
  let failureTrace = "";
  isolated.child.stdout.on("data", (chunk) => { failureTrace += String(chunk); });
  try {
    const endpointId = "endpoint-controller-timeout";
    const accessToken = "controller-timeout-access-token-00000001";
    const desktopSecret = "controller-timeout-desktop-secret-000001";
    const desktop = await registerDesktop(endpointId, accessToken, desktopSecret,
      isolated.wsUrl, 15_000, true);
    sockets.push(desktop.ws);

    const codeWrap = crypto.generateKeyPairSync("x25519");
    const handshakeB = crypto.generateKeyPairSync("x25519");
    const registeredPromise = nextMessageOneOf(
      desktop.ws, ["desktop_pairing_registered", "error"], 15_000,
    );
    desktop.ws.send(JSON.stringify({
      v: 1,
      type: "desktop_pairing_register",
      request_id: "request-register-timeout",
      registration_request_id: "registration-timeout-0001",
      endpoint_id: endpointId,
      remote_control_generation: "generation-timeout-0001",
      code_wrap_public_key_b64: rawX25519Public(codeWrap.publicKey).toString("base64"),
      handshake_public_key_b64: rawX25519Public(handshakeB.publicKey).toString("base64"),
      key_algorithm: "x25519",
      capabilities: ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1"],
    }));
    const registered = await registeredPromise;
    if (registered.type === "error") throw new Error(`pairing registration failed: ${registered.code}`);
    const pairingCode = openPairingCode(codeWrap.privateKey, registered);

    const controller = await openControllerSocket(isolated.wsUrl);
    sockets.push(controller);
    const reservedA = nextMessage(controller, "controller_pairing_reserved", 10_000);
    const reservedB = nextMessage(desktop.ws, "controller_pairing_reserved", 10_000);
    const invalidated = nextMessage(desktop.ws, "error", 40_000);
    const handshakeA = crypto.generateKeyPairSync("x25519");
    controller.send(JSON.stringify({
      v: 1,
      type: "controller_pairing_claim",
      request_id: "request-claim-timeout",
      claim_request_id: "claim-timeout-0001",
      pairing_code: pairingCode,
      handshake_public_key_b64: rawX25519Public(handshakeA.publicKey).toString("base64"),
      key_algorithm: "x25519",
      capabilities: ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1"],
    }));
    await Promise.all([reservedA, reservedB]);
    assert.equal((await invalidated).code, "pairing_code_invalid");
    assert.match(failureTrace, /reservation-invalidation-probe: failed/u);
    assert.match(failureTrace, /reservation-invalidation-probe: retried/u);

    const web = await joinWeb(endpointId, accessToken, {}, isolated.wsUrl);
    sockets.push(web.ws);
    assert.equal(web.joined.lease_id.length > 0, true);
  } finally {
    sockets.forEach(closeSocket);
    await stopRelay(isolated.child);
  }
});

test("controller v1 completes a real three-party encrypted handshake beside legacy Web v2", async () => {
  const statePath = join(testRoot, `controller-legacy-${Date.now()}.json`);
  const controllerStatePath = join(testRoot, `controller-state-${Date.now()}`, "state.json");
  const signing = crypto.generateKeyPairSync("ed25519");
  const isolated = await spawnRelay({
    NODE_ENV: "development",
    PINVOU_DESKTOP_CONTROLLER_ENABLED: "1",
    PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
    PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "1",
    PINVOU_REMOTE_CONTROLLER_STATE_PATH: controllerStatePath,
    PINVOU_REMOTE_STATE_PATH: statePath,
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "enrollment-key-test-1",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "enrollment-key-material-at-least-32-bytes",
    PINVOU_REMOTE_PAIRING_HMAC_KEY_ID: "pairing-key-test-1",
    PINVOU_REMOTE_PAIRING_HMAC_KEY: "pairing-key-material-independent-at-least-32-bytes",
    PINVOU_REMOTE_RELAY_SIGNING_KEY_ID: "relay-signing-key-test-1",
    PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64: signing.privateKey.export({
      type: "pkcs8", format: "der",
    }).toString("base64"),
  });
  const sockets = [];
  try {
    const controllerHealth = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(controllerHealth.desktop_controller_enabled, true);
    const legacySocket = await openSocket(isolated.wsUrl);
    sockets.push(legacySocket);
    assert.equal(legacySocket.protocol, "");
    const mixedVersionError = nextMessage(legacySocket, "error");
    legacySocket.send('{"v":1,"v":2,"type":"web_client_join"}');
    assert.equal((await mixedVersionError).code, "bad_json");
    const unnegotiatedV1 = nextMessage(legacySocket, "error");
    legacySocket.send('{"v":1,"type":"controller_capabilities"}');
    assert.equal((await unnegotiatedV1).code, "unsupported_protocol_version");
    closeSocket(legacySocket);
    const duplicateSocket = await openControllerSocket(isolated.wsUrl);
    sockets.push(duplicateSocket);
    const duplicateError = nextMessage(duplicateSocket, "error");
    duplicateSocket.send('{"v":1,"type":"controller_pairing_claim","type":"controller_capabilities"}');
    assert.equal((await duplicateError).code, "bad_json");
    closeSocket(duplicateSocket);
    await smokeControllerRoute(isolated.wsUrl);
    const endpointId = "endpoint-controller-e2e";
    const accessToken = "controller-access-token-0000000000000001";
    const desktopSecret = "controller-desktop-secret-000000000001";
    const desktop = await registerDesktop(endpointId, accessToken, desktopSecret,
      isolated.wsUrl, 15_000, true);
    sockets.push(desktop.ws);

    const codeWrap = crypto.generateKeyPairSync("x25519");
    const handshakeB = crypto.generateKeyPairSync("x25519");
    const registeredPromise = nextMessageOneOf(
      desktop.ws, ["desktop_pairing_registered", "error"], 15_000,
    );
    desktop.ws.send(JSON.stringify({
      v: 1,
      type: "desktop_pairing_register",
      request_id: "request-register-e2e",
      registration_request_id: "registration-e2e-0001",
      endpoint_id: endpointId,
      remote_control_generation: "generation-e2e-0001",
      code_wrap_public_key_b64: rawX25519Public(codeWrap.publicKey).toString("base64"),
      handshake_public_key_b64: rawX25519Public(handshakeB.publicKey).toString("base64"),
      key_algorithm: "x25519",
      capabilities: ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1", "task_v1"],
    }));
    const registered = await registeredPromise;
    if (registered.type === "error") throw new Error(`pairing registration failed: ${registered.code}`);
    const pairingCode = openPairingCode(codeWrap.privateKey, registered);
    assert.match(pairingCode, /^[23456789ABCDEFGHJKMNPQRSTUVWXYZ]{7}$/u);

    const controller = await openControllerSocket(isolated.wsUrl);
    sockets.push(controller);
    const reservedA = nextMessage(controller, "controller_pairing_reserved", 10_000);
    const reservedB = nextMessage(desktop.ws, "controller_pairing_reserved", 10_000);
    const handshakeA = crypto.generateKeyPairSync("x25519");
    controller.send(JSON.stringify({
      v: 1,
      type: "controller_pairing_claim",
      request_id: "request-claim-e2e",
      claim_request_id: "claim-e2e-0001",
      pairing_code: pairingCode,
      handshake_public_key_b64: rawX25519Public(handshakeA.publicKey).toString("base64"),
      key_algorithm: "x25519",
      capabilities: ["pairing_v1", "encrypted_frames_v1", "finished_receipt_v1", "task_v1"],
    }));
    const [reserved, controlledReserved] = await Promise.all([reservedA, reservedB]);
    assert.equal(controlledReserved.transcript_hash, reserved.transcript_hash);
    assert.equal(crypto.verify(
      null,
      buildPairingTranscriptPreimage(reserved.transcript),
      signing.publicKey,
      Buffer.from(reserved.relay_signature_b64, "base64"),
    ), true);

    const common = {
      v: 1,
      pairing_id: reserved.pairing_id,
      reservation_id: reserved.reservation_id,
      remote_control_generation: reserved.remote_control_generation,
      relay_epoch: reserved.relay_epoch,
      transcript_hash: reserved.transcript_hash,
    };
    const secretA = crypto.randomBytes(32);
    const secretB = crypto.randomBytes(32);
    const bindingA = {
      relay_epoch: reserved.relay_epoch,
      pairing_id: reserved.pairing_id,
      reservation_id: reserved.reservation_id,
      transcript_hash: reserved.transcript_hash,
      sender_role: "A",
    };
    const bindingB = { ...bindingA, sender_role: "B" };

    let forwarded = nextMessage(desktop.ws, "finished_receipt_commit");
    let confirmed = nextMessage(controller, "finished_receipt_commit");
    controller.send(JSON.stringify({
      ...common,
      type: "finished_receipt_commit",
      request_id: "request-commit-A",
      sender_role: "A",
      commitment: receiptCommitmentHex(bindingA, secretA),
    }));
    assert.equal((await forwarded).sender_role, "A");
    assert.equal((await confirmed).sender_role, "A");
    forwarded = nextMessage(controller, "finished_receipt_commit");
    confirmed = nextMessage(desktop.ws, "finished_receipt_commit");
    desktop.ws.send(JSON.stringify({
      ...common,
      type: "finished_receipt_commit",
      request_id: "request-commit-B",
      sender_role: "B",
      commitment: receiptCommitmentHex(bindingB, secretB),
    }));
    assert.equal((await forwarded).sender_role, "B");
    assert.equal((await confirmed).sender_role, "B");

    forwarded = nextMessage(desktop.ws, "encrypted_handshake_finished");
    controller.send(JSON.stringify({
      ...common,
      type: "encrypted_handshake_finished",
      request_id: "request-finished-A",
      sender_role: "A",
      ciphertext_b64: "AQID",
      ciphertext_bytes: "3",
    }));
    await forwarded;
    forwarded = nextMessage(controller, "encrypted_handshake_finished");
    desktop.ws.send(JSON.stringify({
      ...common,
      type: "encrypted_handshake_finished",
      request_id: "request-finished-B",
      sender_role: "B",
      ciphertext_b64: "BAUG",
      ciphertext_bytes: "3",
    }));
    await forwarded;

    desktop.ws.send(JSON.stringify({
      ...common,
      type: "finished_receipt_ack",
      request_id: "request-receipt-A",
      sender_role: "A",
      receiver_role: "B",
      receipt_secret_b64: secretA.toString("base64"),
    }));
    const joinedA = nextMessage(controller, "controller_joined", 10_000);
    const joinedB = nextMessage(desktop.ws, "controller_joined", 10_000);
    controller.send(JSON.stringify({
      ...common,
      type: "finished_receipt_ack",
      request_id: "request-receipt-B",
      sender_role: "B",
      receiver_role: "A",
      receipt_secret_b64: secretB.toString("base64"),
    }));
    const [controllerJoined, desktopJoined] = await Promise.all([joinedA, joinedB]);
    assert.equal(controllerJoined.controller_lease_id, desktopJoined.controller_lease_id);
    assert.notEqual(controllerJoined.resume_token_b64, desktopJoined.resume_token_b64);

    const leaseContext = {
      connection_id: controllerJoined.connection_id,
      controller_lease_id: controllerJoined.controller_lease_id,
      lease_epoch: controllerJoined.lease_epoch,
      relay_epoch: controllerJoined.relay_epoch,
      remote_control_generation: controllerJoined.remote_control_generation,
    };
    const capabilityRequest = {
      v: 1,
      type: "encrypted_frame",
      ...leaseContext,
      frame_sequence: "1",
      direction: "a_to_b",
      payload_kind: "capability_snapshot_request",
      ciphertext_b64: "AQID",
      ciphertext_bytes: "3",
    };
    const requestAtDesktop = nextMessage(desktop.ws, "encrypted_frame", 10_000);
    controller.send(JSON.stringify(capabilityRequest));
    assert.deepEqual(await requestAtDesktop, capabilityRequest);

    const capabilityResponse = {
      ...capabilityRequest,
      frame_sequence: "1",
      direction: "b_to_a",
      payload_kind: "capability_snapshot",
      ciphertext_b64: "BAUG",
    };
    const responseAtController = nextMessage(controller, "encrypted_frame", 10_000);
    desktop.ws.send(JSON.stringify(capabilityResponse));
    assert.deepEqual(await responseAtController, capabilityResponse);

    const web = await openSocket(isolated.wsUrl);
    sockets.push(web);
    const conflict = nextMessage(web, "error");
    // Legacy v2 retains duplicate ordinary fields. The endpoint is occupied
    // by controller A, so routing still returns the existing conflict code.
    web.send(`{"v":2,"type":"web_client_join","type":"web_client_join",`
      + `"endpoint_id":"${endpointId}","access_token":"${accessToken}",`
      + '"stream_epoch":null,"after_seq":0}');
    assert.equal((await conflict).code, "endpoint_not_found");
  } finally {
    sockets.forEach(closeSocket);
    await stopRelay(isolated.child);
  }
});

test("a non-numeric heartbeat falls back instead of a 1 ms NaN tick", async (t) => {
  // setInterval(NaN) beats at 1 ms; the observable regression is the ping
  // cadence itself: a connected client must see no ping for a window that a
  // 1 ms ticker blows past immediately (the 15 s fallback's first ping lands
  // far outside it).
  const isolated = await spawnRelay({ HEARTBEAT_INTERVAL_MS: "abc" });
  const ws = await openSocket(isolated.wsUrl);
  t.after(() => { closeSocket(ws); return stopRelay(isolated.child); });
  let pings = 0;
  ws.on("ping", () => { pings += 1; });
  await new Promise((resolve) => setTimeout(resolve, 1500));
  assert.equal(pings, 0, `garbage heartbeat produced ${pings} pings (1 ms NaN tick)`);
  // And the relay stays silent on stderr — a clean fallback prints no
  // [relay-error] line (operational notices aside).
  assert.doesNotMatch(isolated.errors(), /\[relay-error\]/u, "heartbeat fallback produced an error line");
  const health = await (await fetch(`${isolated.httpUrl}/pinvou3/remote/healthz`)).json();
  assert.equal(health.endpoint_count, 0);

  // An oversized value clamps to the 24 h ceiling instead of passing the raw
  // number to setInterval; the relay must stay healthy either way.
  const oversized = await spawnRelay({ HEARTBEAT_INTERVAL_MS: "99999999999999" });
  try {
    const oversizeSocket = await openSocket(oversized.wsUrl);
    let oversizePings = 0;
    oversizeSocket.on("ping", () => { oversizePings += 1; });
    await new Promise((resolve) => setTimeout(resolve, 1200));
    assert.equal(oversizePings, 0);
    const oversizeHealth = await (await fetch(`${oversized.httpUrl}/pinvou3/remote/healthz`)).json();
    assert.equal(oversizeHealth.ok, true);
    closeSocket(oversizeSocket);
  } finally {
    await stopRelay(oversized.child);
  }
});

test("clamps a sub-floor heartbeat to the five second floor", async (t) => {
  // An empty value parses as 0 (the old parser took its default through the
  // falsy-string branch): measure the cadence between two pings, which is
  // phase-independent (the first ping can land anywhere in the cycle).
  const isolated = await spawnRelay({ HEARTBEAT_INTERVAL_MS: "" });
  try {
    const ws = await openSocket(isolated.wsUrl);
    t.after(() => closeSocket(ws));
    const pings = [];
    ws.on("ping", () => pings.push(performance.now()));
    await new Promise((resolve, reject) => {
      // Both timers must clear on either exit path: an interval left running
      // would hold the test process open after a rejection.
      const timer = setTimeout(() => {
        clearInterval(poll);
        reject(new Error("heartbeat pings never arrived"));
      }, 15000);
      const poll = setInterval(() => {
        if (pings.length >= 2) {
          clearInterval(poll);
          clearTimeout(timer);
          resolve();
        }
      }, 100);
    });
    const cadence = pings[1] - pings[0];
    assert.ok(cadence >= 4500 && cadence <= 9000,
      `heartbeat cadence ${Math.round(cadence)} ms — expected the 5 s floor`);
  } finally {
    await stopRelay(isolated.child);
  }
});

test("sweeps stranded state temp files at startup", async () => {
  const stateDirectory = join(testRoot, `tmp-sweep-${Date.now()}`);
  await mkdir(stateDirectory, { recursive: true });
  await writeFile(join(stateDirectory, "relay-state.json.999.deadbeef.tmp"), "partial write");
  await writeFile(join(stateDirectory, "unrelated.tmp"), "keep me");
  // A sibling state path extending this basename keeps its own writer's
  // temp files, and a bare basename.tmp is not a writer product either:
  // the sweep matches only basename.<pid>.<hex>.tmp.
  await writeFile(join(stateDirectory, "relay-state.json.backup.123.abcde.tmp"), "sibling partial write");
  await writeFile(join(stateDirectory, "relay-state.json.tmp"), "not a writer product");
  const isolated = await spawnRelay({
    PINVOU_REMOTE_STATE_PATH: join(stateDirectory, "relay-state.json"),
  });
  try {
    const survivors = new Set(await readdir(stateDirectory));
    assert.ok(!survivors.has("relay-state.json.999.deadbeef.tmp"), "stranded temp file not swept");
    assert.ok(survivors.has("unrelated.tmp"), "sweep removed an unrelated file");
    assert.ok(survivors.has("relay-state.json.backup.123.abcde.tmp"), "sweep removed a sibling path's temp file");
    assert.ok(survivors.has("relay-state.json.tmp"), "sweep removed a non-writer temp name");
  } finally {
    await stopRelay(isolated.child);
  }
});

test("serves 503 for the site root when the web build is missing entirely", async () => {
  const emptyWebDir = join(testRoot, `web-empty-${Date.now()}`);
  await mkdir(emptyWebDir, { recursive: true });
  const isolated = await spawnRelay({ PINVOU_REMOTE_WEB_DIR: emptyWebDir });
  try {
    // The site entry is the build: a missing index.html is "build is not
    // available" (503), not an ordinary 404. A missing asset with an
    // extension stays a plain 404 (a stale reference to a replaced build),
    // and an extensionless SPA route falls back to the same missing entry.
    const root = await fetch(`${isolated.httpUrl}/pinvou3/remote/`);
    assert.equal(root.status, 503);
    assert.equal(await root.text(), "WebUI build is not available");
    // HEAD reads the same site entry, so an uptime probe cannot disagree
    // with the GET that follows it.
    const rootHead = await fetch(`${isolated.httpUrl}/pinvou3/remote/`, { method: "HEAD" });
    assert.equal(rootHead.status, 503);
    assert.equal(
      (await fetch(`${isolated.httpUrl}/pinvou3/remote/staleAsset-12345678.js`)).status,
      404,
    );
    assert.equal(
      (await fetch(`${isolated.httpUrl}/pinvou3/remote/some/route`)).status,
      503,
    );
  } finally {
    await stopRelay(isolated.child);
  }
});
