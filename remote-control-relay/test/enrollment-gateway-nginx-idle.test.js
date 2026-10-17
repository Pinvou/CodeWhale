import assert from "node:assert/strict";
import crypto from "node:crypto";
import { spawn, execFile } from "node:child_process";
import { once } from "node:events";
import { lstat, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import { basename, dirname, isAbsolute, join, relative, resolve } from "node:path";
import { promisify } from "node:util";
import test from "node:test";
import { WebSocket, WebSocketServer } from "ws";
import { createEnrollmentGateway } from "../enrollment-gateway.js";
import { DEFAULT_GATEWAY_HEARTBEAT_INTERVAL_MS } from "../enrollment-gateway-heartbeat.js";

const execute = promisify(execFile);
const nginxBin = process.env.PINVOU_TEST_NGINX_BIN;
const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

async function unusedLoopbackPort() {
  const probe = net.createServer();
  probe.listen({ host: "127.0.0.1", port: 0 });
  await once(probe, "listening");
  const port = probe.address().port;
  await new Promise((done, reject) => probe.close((error) => error ? reject(error) : done()));
  return port;
}

async function bounded(promise, ms, code) {
  let timer;
  try {
    return await Promise.race([promise,
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(code)), ms); })]);
  } finally { clearTimeout(timer); }
}

async function removeOwnedDirectory(root, canonicalRoot, canonicalParent) {
  const info = await lstat(root);
  assert.ok(info.isDirectory() && !info.isSymbolicLink(), "nginx fixture root must remain a real directory");
  assert.equal(await realpath(root), canonicalRoot);
  assert.equal(dirname(canonicalRoot), canonicalParent);
  assert.match(basename(canonicalRoot), /^pinvou-nginx-idle-[A-Za-z0-9]+$/u);
  assert.equal(relative(canonicalParent, canonicalRoot), basename(canonicalRoot));
  // These checks apply before recursive removal on Windows too. No external
  // binary/tooling directory or shared cache is ever a cleanup target.
  await rm(root, { recursive: true, force: false });
}

test("real nginx keeps the authenticated public gateway hop idle beyond two proxy budgets", {
  skip: nginxBin ? false : "PINVOU_TEST_NGINX_BIN is not supplied; real nginx was not executed",
  timeout: 25_000,
}, async (t) => {
  assert.ok(isAbsolute(nginxBin), "explicit nginx executable must use an absolute path");
  const binary = await realpath(nginxBin);
  assert.ok((await lstat(binary)).isFile());
  const version = await execute(binary, ["-v"], { windowsHide: true, timeout: 5000 });
  assert.match(version.stdout + version.stderr, /nginx version: nginx\/[0-9.]+/u);
  assert.equal(DEFAULT_GATEWAY_HEARTBEAT_INTERVAL_MS, 15_000);
  const sample = await readFile(new URL("../nginx-enrollment-gateway.conf", import.meta.url), "utf8");
  const websocketLocation = sample.match(/location = \/pinvou3\/remote\/ws\s*\{([^}]+)\}/u)?.[1];
  assert.ok(websocketLocation, "the deployed sample must use the exact WebSocket route");
  assert.match(websocketLocation, /proxy_read_timeout\s+75s;/u);
  assert.match(websocketLocation, /proxy_send_timeout\s+75s;/u);
  assert.match(websocketLocation, /proxy_set_header X-Forwarded-For \$remote_addr;/u);

  const parent = await realpath(tmpdir());
  const root = await mkdtemp(join(parent, "pinvou-nginx-idle-"));
  const canonicalRoot = await realpath(root);
  const prefix = canonicalRoot.replaceAll("\\", "/") + "/";
  const config = join(root, "nginx.conf");
  const nonce = crypto.randomUUID();
  const upstreamTimers = new Set();
  let upstream;
  let gateway;
  let client;
  let nginx;
  let nginxExit;
  let nginxError = "";
  let upstreamPongs = 0;
  let authorizations = 0;
  let upstreamRegistrations = 0;
  const pingTimes = [];

  t.after(async () => {
    for (const timer of upstreamTimers) clearInterval(timer);
    client?.terminate();
    let shutdownError;
    if (nginx && nginx.exitCode === null && nginx.signalCode === null) {
      try {
        const pid = Number((await readFile(join(root, "nginx.pid"), "utf8")).trim());
        assert.equal(pid, nginx.pid, "quit must address only the child spawned by this fixture");
        await execute(binary, ["-p", prefix, "-c", config, "-s", "stop"],
          { windowsHide: true, timeout: 5000 });
        assert.equal((await bounded(nginxExit, 5000, "owned_nginx_quit_timeout"))[0], 0);
      } catch (error) {
        shutdownError = error;
        // Only this exact ChildProcess handle can be terminated. Never locate
        // processes by name or signal an instance using an unverified pidfile.
        if (nginx.exitCode === null && nginx.signalCode === null) nginx.kill();
        await bounded(nginxExit, 5000, "owned_nginx_termination_timeout");
      }
    }
    if (gateway) await bounded(gateway.close(), 5000, "owned_gateway_close_timeout");
    if (upstream) {
      for (const socket of upstream.clients) socket.terminate();
      await bounded(new Promise((done) => upstream.close(done)), 5000, "owned_upstream_close_timeout");
    }
    await removeOwnedDirectory(root, canonicalRoot, parent);
    if (shutdownError) throw shutdownError;
  });

  upstream = new WebSocketServer({ host: "127.0.0.1", port: 0 });
  upstream.on("connection", (socket) => {
    socket.on("error", () => {});
    socket.on("pong", () => { upstreamPongs += 1; });
    const timer = setInterval(() => {
      if (socket.readyState === WebSocket.OPEN) socket.ping();
    }, 50);
    timer.unref();
    upstreamTimers.add(timer);
    socket.once("close", () => { clearInterval(timer); upstreamTimers.delete(timer); });
    socket.once("message", (raw) => {
      const message = JSON.parse(String(raw));
      assert.equal(message.type, "desktop_endpoint_register");
      assert.equal(message.endpoint_id, "nginx-idle-fixture");
      upstreamRegistrations += 1;
      socket.send(JSON.stringify({ v: 2, type: "desktop_endpoint_registered",
        endpoint_id: "nginx-idle-fixture" }));
    });
  });
  await once(upstream, "listening");
  // Only Gateway and nginx are the production implementations here. This
  // strict credential adapter and socket upstream do not prove durable
  // enrollment, the Legacy Relay, TLS, or deployed two-device E2E.
  gateway = createEnrollmentGateway({
    registry: {
      authorizeDesktopRegistration({ endpointId, accessToken, desktopSecret }) {
        assert.equal(endpointId, "nginx-idle-fixture");
        assert.equal(accessToken, "test-nginx-idle-access-00000000001");
        assert.equal(desktopSecret, "test-nginx-idle-desktop-000000001");
        authorizations += 1;
      },
      authorizeWebRegistration() { assert.fail("unexpected web authorization"); },
    },
    heartbeatIntervalMs: 100,
    trustedProxyIps: ["127.0.0.1"],
    upstreamUrl: `ws://127.0.0.1:${upstream.address().port}/ws`,
  });
  const gatewayAddress = await gateway.listen({ host: "127.0.0.1", port: 0 });
  const proxyPort = await unusedLoopbackPort();
  await mkdir(join(root, "logs"));
  await mkdir(join(root, "temp"));
  const wrappedSample = `daemon off;
master_process off;
pid nginx.pid;
error_log logs/error.log notice;
events { worker_connections 64; }
http {
  access_log off;
  server {
    listen 127.0.0.1:${proxyPort};
    location = /owned-${nonce} {
      add_header X-Pinvou-Nginx-Test "${nonce}" always;
      return 204;
    }
    ${sample.replaceAll("http://127.0.0.1:8790", `http://127.0.0.1:${gatewayAddress.port}`)}
  }
}
`;
  await writeFile(config, wrappedSample, "utf8");
  await execute(binary, ["-p", prefix, "-c", config, "-t"], { windowsHide: true, timeout: 5000 });
  // Preserve the actual sample directives and headers; only scale its two
  // transport budgets for a bounded regression. This is not a 75s live soak.
  await writeFile(config, wrappedSample.replace(/(proxy_(?:read|send)_timeout\s+)75s;/gu, "$11s;"), "utf8");
  await execute(binary, ["-p", prefix, "-c", config, "-t"], { windowsHide: true, timeout: 5000 });
  nginx = spawn(binary, ["-p", prefix, "-c", config], {
    windowsHide: true, stdio: ["ignore", "pipe", "pipe"], cwd: root,
  });
  nginx.stdout.resume();
  nginx.stderr.on("data", (raw) => { nginxError = (nginxError + raw.toString()).slice(-4096); });
  nginxExit = once(nginx, "exit");
  nginxExit.catch(() => {});
  await once(nginx, "spawn");
  const readyDeadline = Date.now() + 5000;
  let ready = false;
  while (Date.now() < readyDeadline) {
    assert.equal(nginx.exitCode, null, nginxError);
    try {
      const response = await fetch(`http://127.0.0.1:${proxyPort}/owned-${nonce}`,
        { signal: AbortSignal.timeout(200) });
      ready = response.status === 204 && response.headers.get("x-pinvou-nginx-test") === nonce;
    } catch { /* readiness only; no fixture credential is sent to this probe */ }
    if (ready) break;
    await sleep(25);
  }
  assert.ok(ready, "owned nginx did not expose its exact readiness nonce");
  assert.equal(Number((await readFile(join(root, "nginx.pid"), "utf8")).trim()), nginx.pid);

  client = new WebSocket(`ws://127.0.0.1:${proxyPort}/pinvou3/remote/ws`);
  client.on("error", () => {});
  client.on("ping", () => { pingTimes.push(performance.now()); });
  await bounded(once(client, "open"), 5000, "owned_nginx_websocket_open_timeout");
  const registered = once(client, "message");
  client.send(JSON.stringify({ v: 2, type: "desktop_endpoint_register", endpoint_id: "nginx-idle-fixture",
    access_token: "test-nginx-idle-access-00000000001", desktop_secret: "test-nginx-idle-desktop-000000001" }));
  assert.equal(JSON.parse(String((await bounded(registered, 5000, "owned_nginx_registration_timeout"))[0])).type,
    "desktop_endpoint_registered");
  const started = performance.now();
  await new Promise((done, reject) => {
    const closed = () => { clearTimeout(timer); reject(new Error("gateway_closed_before_two_nginx_read_budgets")); };
    const timer = setTimeout(() => { client.off("close", closed); done(); }, 2600);
    client.once("close", closed);
  });
  assert.ok(performance.now() - started >= 2500);
  assert.equal(client.readyState, WebSocket.OPEN);
  assert.equal(authorizations, 1);
  assert.equal(upstreamRegistrations, 1);
  assert.ok(upstreamPongs >= 2, "independent upstream heartbeat must be active");
  assert.ok(pingTimes.length >= 5, "Gateway pings must traverse the real nginx hop");
  assert.ok(pingTimes.at(-1) - started >= 2000, "public-hop heartbeat must continue past two read budgets");
  t.diagnostic(`real_nginx_idle_elapsed_ms=${Math.floor(performance.now() - started)} public_pings=${pingTimes.length} upstream_pongs=${upstreamPongs}`);
});
