// Isolated process: an uncaught upstream-close error must fail the parent test.
import assert from "node:assert/strict";
import { once } from "node:events";
import http from "node:http";
import { join } from "node:path";
import { WebSocket, WebSocketServer } from "ws";
import { createEnrollmentGateway } from "../../enrollment-gateway.js";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../../enrollment-registry.js";
import { RelayStateStore } from "../../relay-state-store.js";

const [scenario, root] = process.argv.slice(2);
assert.ok(["abnormal", "valid"].includes(scenario) && root);
const accessToken = "gateway-close-test-access-token-at-least-32-bytes";
const desktopSecret = "gateway-close-test-desktop-secret-at-least-32-bytes";
const config = loadEnrollmentRegistryConfig({
  NODE_ENV: "test", PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "development",
  PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "0",
  PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID: "gateway-close-test-key",
  PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "gateway-close-test-hmac-material-at-least-32-bytes",
  PINVOU_REMOTE_CONTROLLER_STATE_PATH: join(root, "controller", "state.json"),
  PINVOU_REMOTE_STATE_PATH: join(root, "legacy.json"),
});
// This process tests gateway close propagation, not the already-covered OS ACL.
const store = await RelayStateStore.open({ statePath: config.controller_state_path,
  legacyStatePath: config.legacy_web_state_path,
  ...(process.platform === "win32" ? { protectPath: async () => {}, verifyPath: async () => {},
    verifyAncestors: async () => {} } : {}),
  processIdentity: async (pid) => `gateway-close-test:${pid}` });
const registry = EnrollmentRegistry.open({ stateStore: store, config });
await registry.enroll({ endpointId: "gateway-close-endpoint", credential: accessToken, desktopCredential: desktopSecret });
const upstreamServer = http.createServer();
const upstreamSockets = new WebSocketServer({ server: upstreamServer });
const validReason = "断".repeat(41); // Exactly 123 UTF-8 bytes: a legal wire reason.
upstreamSockets.on("connection", (socket) => socket.once("message", () => {
  if (scenario === "abnormal") socket.terminate();
  else socket.close(4001, validReason);
}));
upstreamServer.listen(0, "127.0.0.1");
await once(upstreamServer, "listening");
const gateway = createEnrollmentGateway({ registry,
  upstreamUrl: `ws://127.0.0.1:${upstreamServer.address().port}` });
const address = await gateway.listen({ port: 0, host: "127.0.0.1" });
const client = new WebSocket(`ws://127.0.0.1:${address.port}/pinvou3/remote/ws`);
await once(client, "open");
const closed = once(client, "close");
client.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", request_id: "gateway-close-register",
  endpoint_id: "gateway-close-endpoint", access_token: accessToken, desktop_secret: desktopSecret }));
const [code, reason] = await closed;
assert.equal(code, scenario === "abnormal" ? 1011 : 4001);
assert.equal(reason.toString(), scenario === "abnormal" ? "" : validReason);
assert.equal((await fetch(`http://127.0.0.1:${address.port}/healthz`)).status, 200);
// A second authenticated connection proves the event loop/listener still work.
const second = new WebSocket(`ws://127.0.0.1:${address.port}/pinvou3/remote/ws`);
await once(second, "open");
const secondClosed = once(second, "close");
second.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", request_id: "gateway-close-register-2",
  endpoint_id: "gateway-close-endpoint", access_token: accessToken, desktop_secret: desktopSecret }));
assert.equal((await secondClosed)[0], code);
await gateway.close();
await new Promise((resolve, reject) => upstreamSockets.close((error) => error ? reject(error) : resolve()));
await new Promise((resolve, reject) => upstreamServer.close((error) => error ? reject(error) : resolve()));
await store.close();
process.send?.({ ok: true, code, reasonBytes: reason.length });
