import assert from "node:assert/strict";
import { once } from "node:events";
import test from "node:test";
import { WebSocket, WebSocketServer } from "ws";
import { createEnrollmentGateway } from "../enrollment-gateway.js";

// Real gateway and WebSocket transport over loopback. This file proves the
// egress projected-queue guard only; it does not prove durable enrollment or
// Relay E2E (covered by the rollback-live and registry suites).
async function egressFixture(t, { egressBufferLimitBytes }) {
  let gateway;
  let client;
  const upstream = new WebSocketServer({ host: "127.0.0.1", port: 0 });
  t.after(async () => {
    client?.terminate();
    if (gateway) await gateway.close();
    for (const socket of upstream.clients) socket.terminate();
    await new Promise((resolve) => upstream.close(resolve));
  });
  let forwardToClient = () => {};
  const upstreamReceived = [];
  upstream.on("connection", (socket) => {
    socket.once("message", () => {
      socket.send(JSON.stringify({
        v: 2, type: "desktop_endpoint_registered", endpoint_id: "egress-contract-endpoint",
      }));
      forwardToClient = (message) => {
        if (socket.readyState === WebSocket.OPEN) socket.send(message);
      };
    });
    socket.on("message", (message) => { upstreamReceived.push(String(message)); });
  });
  await once(upstream, "listening");
  gateway = createEnrollmentGateway({
    registry: {
      authorizeDesktopRegistration({ endpointId, accessToken, desktopSecret }) {
        assert.equal(endpointId, "egress-contract-endpoint");
        assert.equal(accessToken, "test-egress-contract-access-00000001");
        assert.equal(desktopSecret, "test-egress-contract-desktop-0000001");
      },
      authorizeWebRegistration() { assert.fail("unexpected web authorization"); },
    },
    upstreamUrl: `ws://127.0.0.1:${upstream.address().port}/ws`,
    egressBufferLimitBytes,
  });
  const address = await gateway.listen({ host: "127.0.0.1", port: 0 });
  client = new WebSocket(`ws://127.0.0.1:${address.port}/pinvou3/remote/ws`);
  await once(client, "open");
  const registered = once(client, "message");
  client.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register",
    endpoint_id: "egress-contract-endpoint", access_token: "test-egress-contract-access-00000001",
    desktop_secret: "test-egress-contract-desktop-0000001" }));
  assert.equal(JSON.parse(String((await registered)[0])).type, "desktop_endpoint_registered");
  return {
    client,
    sendFromUpstream: (message) => forwardToClient(message),
    upstreamReceived,
  };
}

test("an upstream frame larger than the egress budget closes the client instead of unbounded buffering",
  { timeout: 10_000 }, async (t) => {
  const fixture = await egressFixture(t, { egressBufferLimitBytes: 64 * 1024 });
  const closed = new Promise((resolve) => fixture.client.once("close", resolve));
  // The payload must be wire-valid JSON: the legacy parse validates every
  // text frame before the egress budget check, so a raw string would trip the
  // parse rejection instead of the guard this file exists to pin.
  fixture.sendFromUpstream(JSON.stringify({ v: 2, type: "desktop_event", pad: "x".repeat(256 * 1024) }));
  assert.equal(await closed, 1008);
});

test("a client frame whose upstream queue would exceed the egress budget closes the client",
  { timeout: 10_000 }, async (t) => {
  const fixture = await egressFixture(t, { egressBufferLimitBytes: 64 * 1024 });
  const closed = new Promise((resolve) => fixture.client.once("close", resolve));
  fixture.client.send(JSON.stringify({ v: 2, type: "web_relay", pad: "y".repeat(256 * 1024) }));
  assert.equal(await closed, 1008);
});

test("frames below the egress budget keep flowing in both directions",
  { timeout: 10_000 }, async (t) => {
  const fixture = await egressFixture(t, { egressBufferLimitBytes: 64 * 1024 });
  const echoed = new Promise((resolve) => fixture.client.once("message", resolve));
  fixture.sendFromUpstream(JSON.stringify({ v: 2, type: "desktop_event", pad: "z".repeat(1024) }));
  assert.equal(JSON.parse(String(await echoed)).type, "desktop_event");
  fixture.client.send(JSON.stringify({ v: 2, type: "web_event", pad: "w".repeat(1024) }));
  // The client→upstream leg is asserted too: the projected queue toward the
  // upstream socket must forward sub-budget frames unchanged.
  const forwarded = await new Promise((resolve) => {
    const deadline = Date.now() + 5_000;
    const poll = () => {
      const hit = fixture.upstreamReceived.map((message) => {
        try { return JSON.parse(message); } catch { return null; }
      }).find((message) => message?.type === "web_event");
      if (hit) return resolve(hit);
      if (Date.now() > deadline) return resolve(null);
      setTimeout(poll, 20);
    };
    poll();
  });
  assert.notEqual(forwarded, null, "sub-budget client frames must reach the upstream socket");
});
