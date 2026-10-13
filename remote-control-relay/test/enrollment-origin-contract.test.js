import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { WebSocket } from "ws";
import { createEnrollmentGateway } from "../enrollment-gateway.js";

const PUBLIC_ORIGIN = "https://origin-contract.example.invalid";
const PUBLIC_HOST = "origin-contract.example.invalid";
const ENDPOINT = "origin-contract-endpoint";
const ACCESS = "test-origin-contract-access-0000000001";
const SECRET = "test-origin-contract-desktop-00000001";
const relayDir = fileURLToPath(new URL("../", import.meta.url));

async function freePort() {
  const server = createServer();
  await new Promise((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

async function startRelay(t, origins = "") {
  const root = await mkdtemp(join(tmpdir(), "pinvou-origin-contract-"));
  const port = await freePort();
  const env = { PATH: process.env.PATH, SystemRoot: process.env.SystemRoot, TEMP: process.env.TEMP,
    TMP: process.env.TMP, NODE_ENV: "test", PORT: String(port), PINVOU_REMOTE_HOST: "127.0.0.1",
    PINVOU_DESKTOP_CONTROLLER_ENABLED: "0", PINVOU_REMOTE_ALLOWED_WEB_ORIGINS: origins,
    PINVOU_REMOTE_TRUSTED_PROXY_IPS: "127.0.0.1", PINVOU_REMOTE_PUBLIC_BASE_PATH: "/pinvou3/remote",
    PINVOU_REMOTE_STATE_PATH: join(root, "legacy-state.json"),
    PINVOU_REMOTE_TELEMETRY_DIR: join(root, "telemetry"), PINVOU_REMOTE_WEB_DIR: root };
  const child = spawn(process.execPath, [join(relayDir, "server.js")],
    { cwd: relayDir, env, shell: false, windowsHide: true, stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  const ready = new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("origin_contract_relay_startup_timeout")), 5000);
    child.stdout.on("data", (chunk) => {
      output += String(chunk);
      if (output.includes("pinvou remote relay listening")) { clearTimeout(timer); resolve(); }
    });
    child.stderr.on("data", () => {});
    child.once("error", (error) => { clearTimeout(timer); reject(error); });
    child.once("exit", () => { clearTimeout(timer); reject(new Error("origin_contract_relay_startup_exit")); });
  });
  let stopped = false;
  const stop = async () => {
    if (stopped) return;
    stopped = true;
    if (child.exitCode === null && child.signalCode === null) {
      const ended = once(child, "exit");
      child.kill("SIGTERM");
      await ended;
    }
    await rm(root, { recursive: true, force: true });
  };
  t.after(stop);
  await ready;
  return { wsUrl: `ws://127.0.0.1:${port}/pinvou3/remote/ws`, stop };
}

function openSocket(t, url, headers = {}) {
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(url, { headers, handshakeTimeout: 5000 });
    t.after(() => socket.terminate());
    socket.once("open", () => resolve(socket));
    socket.once("error", reject);
  });
}

function nextMessage(socket, type) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { socket.off("message", receive); reject(new Error("origin_contract_message_timeout")); }, 5000);
    const receive = (raw) => {
      const value = JSON.parse(String(raw));
      if (value.type !== type) return;
      clearTimeout(timer); socket.off("message", receive); resolve(value);
    };
    socket.on("message", receive);
  });
}

function closedCode(socket) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("origin_contract_close_timeout")), 5000);
    socket.once("close", (code) => { clearTimeout(timer); resolve(code); });
  });
}

function rejectedUpgrade(t, url, headers) {
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(url, { headers, handshakeTimeout: 5000 });
    t.after(() => socket.terminate());
    socket.once("open", () => reject(new Error("origin_contract_unexpected_upgrade")));
    socket.once("unexpected-response", (_request, response) => { response.resume(); resolve(response.statusCode); });
    socket.once("error", (error) => { if (!/Unexpected server response/u.test(error.message)) reject(error); });
  });
}

async function gateway(t, relay, origins = [], trusted = ["127.0.0.1"]) {
  let authorizationCalls = 0;
  // Production gateway and Relay are real. This exact-credential adapter is
  // deliberately not a durable EnrollmentRegistry or an enrollment E2E proof.
  const authorize = ({ endpointId, accessToken, desktopSecret }) => {
    authorizationCalls++;
    assert.equal(endpointId, ENDPOINT); assert.equal(accessToken, ACCESS);
    if (desktopSecret !== undefined) assert.equal(desktopSecret, SECRET);
  };
  const runtime = createEnrollmentGateway({
    registry: { authorizeDesktopRegistration: authorize, authorizeWebRegistration: authorize },
    upstreamUrl: relay.wsUrl, allowedWebOrigins: origins, trustedProxyIps: trusted,
  });
  const address = await runtime.listen({ host: "127.0.0.1", port: 0 });
  t.after(() => runtime.close());
  return { wsUrl: `ws://127.0.0.1:${address.port}/pinvou3/remote/ws`, calls: () => authorizationCalls };
}

async function registerDesktop(t, url, headers = {}) {
  const socket = await openSocket(t, url, headers);
  const response = nextMessage(socket, "desktop_endpoint_registered");
  socket.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: ENDPOINT,
    access_token: ACCESS, desktop_secret: SECRET }));
  assert.equal((await response).endpoint_id, ENDPOINT);
  return socket;
}

async function joinWeb(t, url, headers) {
  const socket = await openSocket(t, url, headers);
  const response = nextMessage(socket, "web_client_joined");
  socket.send(JSON.stringify({ v: 2, type: "web_client_join", endpoint_id: ENDPOINT,
    access_token: ACCESS, after_seq: 0, stream_epoch: null }));
  const joined = await response;
  assert.equal(joined.endpoint_id, ENDPOINT); assert.equal(joined.desktop_connected, true);
  return joined;
}

const publicHeaders = { Host: PUBLIC_HOST, Origin: PUBLIC_ORIGIN, "X-Forwarded-For": "198.51.100.20" };

test("public same-origin at gateway does not become loopback same-origin at Relay", async (t) => {
  const relay = await startRelay(t);
  await registerDesktop(t, relay.wsUrl);
  const guarded = await gateway(t, relay);
  const socket = await openSocket(t, guarded.wsUrl, publicHeaders);
  const closed = closedCode(socket);
  socket.send(JSON.stringify({ v: 2, type: "web_client_join", endpoint_id: ENDPOINT, access_token: ACCESS }));
  assert.equal(await closed, 1008);
  assert.equal(guarded.calls(), 1, "first-hop same-origin admitted and authorized before upstream rejection");
  assert.equal(await rejectedUpgrade(t, relay.wsUrl, { Origin: PUBLIC_ORIGIN }), 403);
});

test("both exact allowlists preserve public browser Origin through the real two-hop join", async (t) => {
  const relay = await startRelay(t, PUBLIC_ORIGIN);
  await registerDesktop(t, relay.wsUrl);
  const guarded = await gateway(t, relay, [PUBLIC_ORIGIN]);
  await joinWeb(t, guarded.wsUrl, publicHeaders);
  assert.equal(guarded.calls(), 1);
});

test("wrong browser Origin is rejected on each real hop before gateway authorization", async (t) => {
  const relay = await startRelay(t, PUBLIC_ORIGIN);
  const guarded = await gateway(t, relay, [PUBLIC_ORIGIN]);
  const headers = { ...publicHeaders, Origin: "https://wrong.example.invalid" };
  assert.equal(await rejectedUpgrade(t, guarded.wsUrl, headers), 403);
  assert.equal(await rejectedUpgrade(t, relay.wsUrl, { Origin: headers.Origin }), 403);
  assert.equal(guarded.calls(), 0);
});

test("native no-Origin invokes credential authorization and preserves two-hop legacy registration and join", async (t) => {
  const relay = await startRelay(t);
  const guarded = await gateway(t, relay);
  const headers = { "X-Forwarded-For": "198.51.100.20" };
  await registerDesktop(t, guarded.wsUrl, headers);
  await joinWeb(t, guarded.wsUrl, headers);
  assert.equal(guarded.calls(), 2, "both no-Origin first frames pass through the exact credential adapter");
});

test("bad or missing immediate proxy source rejects without enrollment calls", async (t) => {
  const relay = await startRelay(t, PUBLIC_ORIGIN);
  const guarded = await gateway(t, relay, [PUBLIC_ORIGIN]);
  for (const source of [undefined, "198.51.100.20, 127.0.0.1", "not-an-ip"]) {
    const headers = { Host: PUBLIC_HOST, Origin: PUBLIC_ORIGIN };
    if (source !== undefined) headers["X-Forwarded-For"] = source;
    assert.equal(await rejectedUpgrade(t, guarded.wsUrl, headers), 403);
  }
  assert.equal(guarded.calls(), 0);
});

test("forwarded Host cannot repair or bypass the loopback upstream Origin boundary", async (t) => {
  const relay = await startRelay(t);
  assert.equal(await rejectedUpgrade(t, relay.wsUrl,
    { Origin: PUBLIC_ORIGIN, "X-Forwarded-Host": PUBLIC_HOST }), 403);
  const guarded = await gateway(t, relay);
  const socket = await openSocket(t, guarded.wsUrl, { ...publicHeaders, "X-Forwarded-Host": PUBLIC_HOST });
  const closed = closedCode(socket);
  socket.send(JSON.stringify({ v: 2, type: "web_client_join", endpoint_id: ENDPOINT, access_token: ACCESS }));
  assert.equal(await closed, 1008); assert.equal(guarded.calls(), 1);
});

test("restoring the explicit Relay allowlist recovers the same public topology and credentials", async (t) => {
  const before = await startRelay(t);
  assert.equal(await rejectedUpgrade(t, before.wsUrl, { Origin: PUBLIC_ORIGIN }), 403);
  await before.stop();
  const restored = await startRelay(t, PUBLIC_ORIGIN);
  await registerDesktop(t, restored.wsUrl);
  const guarded = await gateway(t, restored, [PUBLIC_ORIGIN]);
  await joinWeb(t, guarded.wsUrl, publicHeaders);
  assert.equal(guarded.calls(), 1);
});
