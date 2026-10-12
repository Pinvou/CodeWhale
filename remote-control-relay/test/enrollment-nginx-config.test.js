import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { WebSocket } from "ws";
import { createEnrollmentGateway } from "../enrollment-gateway.js";
import { loadEnrollmentNginxConfig, renderEnrollmentNginxConfig } from "../enrollment-nginx-config.js";
import { main } from "../scripts/enrollment-nginx-config.js";

const execFileAsync = promisify(execFile);

function env(overrides = {}) {
  return {
    PINVOU_REMOTE_GATEWAY_HOST: "127.0.0.1", PINVOU_REMOTE_GATEWAY_PORT: "8790",
    PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST: "127.0.0.1", PINVOU_REMOTE_HOST: "127.0.0.1",
    PINVOU_REMOTE_LEGACY_RELAY_URL: "ws://127.0.0.1:8787/pinvou3/remote/ws",
    PINVOU_REMOTE_GATEWAY_TRUSTED_PROXY_IPS: "127.0.0.1",
    PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: '["https://example.invalid/pinvou3/remote"]',
    ...overrides,
  };
}

test("checked-in nginx sample is exactly the default offline contract", async () => {
  const actual = await readFile(new URL("../nginx-enrollment-gateway.conf", import.meta.url), "utf8");
  assert.equal(actual.replaceAll("\r\n", "\n"), renderEnrollmentNginxConfig(env()));
});

test("custom routes and independent ports target only the guarded gateway", () => {
  for (const host of ["127.0.0.1", "::1"]) {
    const authority = host === "::1" ? "[::1]" : host;
    const config = env({ PINVOU_REMOTE_PUBLIC_BASE_PATH: "/custom/remote-v2", PORT: "9321",
      PINVOU_REMOTE_GATEWAY_HOST: host, PINVOU_REMOTE_GATEWAY_PORT: "9322",
      PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST: host, PINVOU_REMOTE_HOST: host,
      PINVOU_REMOTE_LEGACY_RELAY_URL: `ws://${authority}:9321/custom/remote-v2/ws`,
      PINVOU_REMOTE_GATEWAY_TRUSTED_PROXY_IPS: host,
      PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: '["https://example.invalid/custom/remote-v2"]' });
    const rendered = renderEnrollmentNginxConfig(config);
    assert.deepEqual([...rendered.matchAll(/^location = (\S+) \{/gmu)].map((m) => m[1]),
      ["/custom/remote-v2/ws", "/custom/remote-v2/healthz"]);
    assert.deepEqual([...rendered.matchAll(/^\s+proxy_pass (\S+);$/gmu)].map((m) => m[1]),
      [`http://${authority}:9322`, `http://${authority}:9322`]);
    assert.doesNotMatch(rendered, /9321|pinvou3\/remote|location\s+\//u);
    assert.match(rendered, /proxy_set_header X-Forwarded-For \$remote_addr;/u);
    assert.match(rendered, /proxy_set_header Host \$host;/u);
  }
});

test("unsafe or inconsistent deployment inputs reject without emitting configuration", async () => {
  for (const override of [
    { PINVOU_REMOTE_PUBLIC_BASE_PATH: "/x;proxy_pass" },
    { PINVOU_REMOTE_PUBLIC_BASE_PATH: "/x\n}" },
    { PINVOU_REMOTE_GATEWAY_PORT: "8790;" }, { PINVOU_REMOTE_GATEWAY_PORT: "80" },
    { PINVOU_REMOTE_GATEWAY_HOST: "0.0.0.0" },
    { PINVOU_REMOTE_LEGACY_RELAY_URL: "ws://localhost:8787/pinvou3/remote/ws" },
    { PINVOU_REMOTE_LEGACY_RELAY_URL: "ws://127.0.0.1:8787/old/ws" },
    { PINVOU_REMOTE_LEGACY_RELAY_URL: "ws://private-material@127.0.0.1:8787/pinvou3/remote/ws" },
    { PORT: "9321" }, { PINVOU_REMOTE_HOST: "0.0.0.0" },
    { PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST: "::1" },
    { PINVOU_REMOTE_GATEWAY_PORT: "8787" },
    { PINVOU_REMOTE_GATEWAY_TRUSTED_PROXY_IPS: "" },
    { PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: "[]" },
    { PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: '["https://example.invalid/stale"]' },
  ]) {
    const logs = [], errors = [];
    assert.equal(await main([], env(override), { log: (x) => logs.push(x), error: (x) => errors.push(x) }), 1);
    assert.equal(logs.length, 0);
    assert.equal(errors.length, 1);
    assert.match(JSON.parse(errors[0]).code, /^gateway_[a-z_]+$/u);
    assert.doesNotMatch(errors[0], /private-material|proxy_pass|localhost/u);
  }
});

test("nginx-normalized ambiguous paths reject even when every env route agrees", () => {
  for (const basePath of ["//custom", "/custom//remote", "/custom///remote"]) {
    const values = env({ PINVOU_REMOTE_PUBLIC_BASE_PATH: basePath,
      PINVOU_REMOTE_LEGACY_RELAY_URL: `ws://127.0.0.1:8787${basePath}/ws`,
      PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: JSON.stringify([`https://example.invalid${basePath}`]) });
    assert.throws(() => renderEnrollmentNginxConfig(values), { code: "gateway_base_path_invalid" });
  }
});

test("offline check rejects stale routes, widened locations and direct legacy bypass", async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "pinvou-nginx-contract-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const file = join(dir, "gateway.conf");
  const original = renderEnrollmentNginxConfig(env());
  const logs = [], errors = [];
  const io = { log: (x) => logs.push(x), error: (x) => errors.push(x) };
  for (const valid of [original, original.replaceAll("\n", "\r\n")]) {
    await writeFile(file, valid);
    assert.equal(await main(["--check", file], env(), io), 0);
  }
  for (const invalid of [original.replaceAll("8790", "8787"),
    original.replaceAll("/pinvou3/remote", "/stale"),
    original.replace("location =", "location"), original + "location / { proxy_pass http://127.0.0.1:8787; }\n"]) {
    await writeFile(file, invalid);
    assert.equal(await main(["--check", file], env(), io), 1);
    assert.equal(JSON.parse(errors.at(-1)).code, "gateway_nginx_config_drift");
  }
  assert.equal(await main(["--unknown"], env(), io), 1);
  assert.equal(JSON.parse(errors.at(-1)).code, "gateway_nginx_arguments_invalid");
});

test("actual CLI generates and checks without disclosing unrelated credential inputs", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "pinvou-nginx-cli-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const file = join(directory, "gateway.conf");
  const cli = fileURLToPath(new URL("../scripts/enrollment-nginx-config.js", import.meta.url));
  const values = { ...process.env, ...env(), PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "test-only-sensitive-marker" };
  const generated = await execFileAsync(process.execPath, [cli], { env: values, timeout: 10_000 });
  assert.equal(generated.stdout, renderEnrollmentNginxConfig(values));
  assert.equal(generated.stderr, "");
  assert.doesNotMatch(generated.stdout, /test-only-sensitive-marker/u);
  await writeFile(file, generated.stdout);
  const checked = await execFileAsync(process.execPath, [cli, "--check", file], { env: values, timeout: 10_000 });
  assert.deepEqual(JSON.parse(checked.stdout), { ok: true, gateway_nginx_config: "consistent" });
  await writeFile(file, generated.stdout.replaceAll("8790", "8787"));
  await assert.rejects(execFileAsync(process.execPath, [cli, "--check", file], { env: values, timeout: 10_000 }),
    (error) => error.code === 1 && error.stdout === ""
      && JSON.parse(error.stderr).code === "gateway_nginx_config_drift"
      && !error.stderr.includes("test-only-sensitive-marker"));
});

test("generated custom health and WS routes reach the real gateway and retain enrollment denial", async (t) => {
  const upstream = http.createServer();
  let upstreamConnections = 0;
  upstream.on("upgrade", (_request, socket) => { upstreamConnections++; socket.destroy(); });
  await new Promise((resolve) => upstream.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise((resolve) => upstream.close(resolve)));
  let authorizationCalls = 0;
  const deny = () => { authorizationCalls++; throw new Error("test_registry_denied"); };
  const basePath = "/contract/custom";
  const gateway = createEnrollmentGateway({
    registry: { authorizeWebRegistration: deny, authorizeDesktopRegistration: deny },
    upstreamUrl: `ws://127.0.0.1:${upstream.address().port}${basePath}/ws`, basePath,
    allowedWebOrigins: ["https://example.invalid"], trustedProxyIps: ["127.0.0.1"],
  });
  t.after(() => gateway.close());
  const address = await gateway.listen({ host: "127.0.0.1", port: 0 });
  const values = env({ PINVOU_REMOTE_PUBLIC_BASE_PATH: basePath,
    PINVOU_REMOTE_GATEWAY_PORT: String(address.port), PORT: String(upstream.address().port),
    PINVOU_REMOTE_LEGACY_RELAY_URL: `ws://127.0.0.1:${upstream.address().port}${basePath}/ws`,
    PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON: '["https://example.invalid/contract/custom"]' });
  const config = loadEnrollmentNginxConfig(values);
  const rendered = renderEnrollmentNginxConfig(values);
  assert.match(rendered, /location = \/contract\/custom\/healthz/u);
  const health = await fetch(`http://${config.host}:${config.gatewayPort}${config.basePath}/healthz`);
  assert.equal(health.status, 200);
  assert.equal(health.headers.get("x-pinvou-enrollment-gateway"), "v1");
  const stale = await fetch(`http://${config.host}:${config.gatewayPort}/pinvou3/remote/healthz`);
  assert.equal(stale.status, 404);
  const socket = new WebSocket(`ws://${config.host}:${config.gatewayPort}${config.basePath}/ws`,
    { origin: "https://example.invalid", headers: { "X-Forwarded-For": "198.51.100.20" } });
  t.after(() => socket.terminate());
  await once(socket, "open");
  const closed = once(socket, "close");
  socket.send(JSON.stringify({ v: 2, type: "web_client_join", endpoint_id: "endpoint-test", access_token: "test-invalid-token" }));
  const [code] = await closed;
  assert.equal(code, 1008);
  assert.equal(authorizationCalls, 1);
  assert.equal(upstreamConnections, 0);
});
