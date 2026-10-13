import assert from "node:assert/strict";
import { once } from "node:events";
import test from "node:test";
import { WebSocket, WebSocketServer } from "ws";
import { createEnrollmentGateway } from "../enrollment-gateway.js";

async function idleFixture(t, options = {}, gatewayOptions = {}, authenticate = true) {
  let gateway;
  let client;
  let upstreamTimer;
  let upstreamPongs = 0;
  const upstream = new WebSocketServer({ host: "127.0.0.1", port: 0 });
  t.after(async () => {
    clearInterval(upstreamTimer);
    client?.terminate();
    if (gateway) await gateway.close();
    for (const socket of upstream.clients) socket.terminate();
    await new Promise((resolve) => upstream.close(resolve));
  });
  upstream.on("connection", (socket) => {
    socket.on("pong", () => upstreamPongs++);
    upstreamTimer = setInterval(() => {
      if (socket.readyState === WebSocket.OPEN) socket.ping();
    }, 50);
    upstreamTimer.unref();
    socket.once("message", () => socket.send(JSON.stringify({
      v: 2, type: "desktop_endpoint_registered", endpoint_id: "idle-contract-endpoint",
    })));
  });
  await once(upstream, "listening");
  // The gateway and WebSocket transport are production implementations. This
  // adapter and upstream fixture do not prove durable enrollment or Relay E2E.
  gateway = createEnrollmentGateway({
    registry: {
      authorizeDesktopRegistration({ endpointId, accessToken, desktopSecret }) {
        assert.equal(endpointId, "idle-contract-endpoint");
        assert.equal(accessToken, "test-idle-contract-access-000000001");
        assert.equal(desktopSecret, "test-idle-contract-desktop-00000001");
      },
      authorizeWebRegistration() { assert.fail("unexpected web authorization"); },
    },
    upstreamUrl: `ws://127.0.0.1:${upstream.address().port}/ws`,
    heartbeatIntervalMs: 100,
    ...gatewayOptions,
  });
  const address = await gateway.listen({ host: "127.0.0.1", port: 0 });
  client = new WebSocket(`ws://127.0.0.1:${address.port}/pinvou3/remote/ws`, options);
  await once(client, "open");
  if (authenticate) {
    const registered = once(client, "message");
    client.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register",
      endpoint_id: "idle-contract-endpoint", access_token: "test-idle-contract-access-000000001",
      desktop_secret: "test-idle-contract-desktop-00000001" }));
    assert.equal(JSON.parse(String((await registered)[0])).type, "desktop_endpoint_registered");
  }
  return { client, gateway, upstreamPongs: () => upstreamPongs,
    async stopGateway() { const runtime = gateway; gateway = null; await runtime.close(); } };
}

test("authenticated idle gateway sends its own public-hop ping while upstream ping remains independent",
  { timeout: 10_000 }, async (t) => {
    const fixture = await idleFixture(t);
    let publicPings = 0;
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("gateway_public_idle_heartbeat_missing")), 2000);
      const ping = () => {
        if (++publicPings < 2) return;
        clearTimeout(timer);
        fixture.client.off("ping", ping);
        resolve();
      };
      fixture.client.on("ping", ping);
    }).catch((error) => {
      assert.ok(fixture.upstreamPongs() >= 2, "upstream heartbeat was active despite missing public-hop ping");
      throw error;
    });
    assert.ok(fixture.upstreamPongs() >= 2, "upstream pings are independently answered");
    assert.equal(fixture.client.readyState, WebSocket.OPEN);
  });

test("missing public-hop pong closes authenticated gateway transport without keeping it alive via upstream pong",
  { timeout: 10_000 }, async (t) => {
    const fixture = await idleFixture(t, { autoPong: false });
    await once(fixture.client, "close", { signal: AbortSignal.timeout(2000) });
    assert.equal(fixture.client.readyState, WebSocket.CLOSED);
    assert.ok(fixture.upstreamPongs() >= 1);
  });

test("unauthenticated connection receives no transport heartbeat and retains its original authentication deadline",
  { timeout: 10_000 }, async (t) => {
    const fixture = await idleFixture(t, {}, { authTimeoutMs: 400 }, false);
    let pings = 0;
    fixture.client.on("ping", () => pings++);
    const [code] = await once(fixture.client, "close", { signal: AbortSignal.timeout(2000) });
    assert.equal(code, 1008);
    assert.equal(pings, 0);
    assert.equal(fixture.upstreamPongs(), 0);
  });

test("closing the gateway terminates its authenticated transport and heartbeat lifecycle",
  { timeout: 10_000 }, async (t) => {
    const fixture = await idleFixture(t);
    const closed = once(fixture.client, "close", { signal: AbortSignal.timeout(2000) });
    await fixture.stopGateway();
    await closed;
    assert.equal(fixture.client.readyState, WebSocket.CLOSED);
  });

test("heartbeat intervals reject non-finite, fractional and out-of-budget configuration before opening listeners", () => {
  for (const heartbeatIntervalMs of [NaN, Infinity, 0, -1, 99, 60_001, 1.5, "15000"]) {
    assert.throws(() => createEnrollmentGateway({
      registry: { authorizeDesktopRegistration() {}, authorizeWebRegistration() {} },
      upstreamUrl: "ws://127.0.0.1:8787/ws", heartbeatIntervalMs,
    }), { code: "gateway_heartbeat_interval_invalid" });
  }
});
