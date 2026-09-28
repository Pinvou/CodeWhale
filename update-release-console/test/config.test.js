import test from "node:test";
import assert from "node:assert/strict";
import { resolve } from "node:path";
import { isTrustedProxy, loadConfig, parseTrustedProxies } from "../src/config.js";

function environment(overrides = {}) {
  return {
    PINVOU_UPDATE_PUBLIC_ROOT: resolve("test-public"),
    PINVOU_UPDATE_STATE_ROOT: resolve("test-state"),
    PINVOU_UPDATE_PUBLIC_BASE_URL: "https://updates.example.test/pinvou3",
    PINVOU_UPDATE_ADMIN_TOKEN: "a".repeat(32),
    ...overrides,
  };
}

test("配置默认只监听回环地址并规范化路径", () => {
  const config = loadConfig(environment({ PINVOU_UPDATE_CONSOLE_BASE_PATH: "/console/" }));
  assert.equal(config.host, "127.0.0.1");
  assert.equal(config.port, 8792);
  assert.equal(config.basePath, "/console");
  assert.equal(config.publicBaseUrl, "https://updates.example.test/pinvou3");
  assert.deepEqual(config.trustedProxies, []);
  assert.equal(config.authFailureLimit, 5);
  assert.equal(config.authFailureWindowMs, 300_000);
  assert.equal(config.authBlockMs, 900_000);
  assert.equal(config.authLimiterTtlMs, 3_600_000);
  assert.equal(config.authLimiterMaxEntries, 10_000);
  assert.equal(config.uploadIdleTimeoutMs, 120_000);
});

test("上传无进展超时必须处于安全范围", () => {
  assert.equal(
    loadConfig(environment({ PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS: "90000" })).uploadIdleTimeoutMs,
    90_000,
  );
  assert.throws(
    () => loadConfig(environment({ PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS: "999" })),
    /PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS/u,
  );
  assert.throws(
    () => loadConfig(environment({ PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS: "120001" })),
    /PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS/u,
  );
});

test("拒绝 HTTP 下载地址和公开目录内的状态目录", () => {
  assert.throws(
    () => loadConfig(environment({ PINVOU_UPDATE_PUBLIC_BASE_URL: "http://updates.example.test/pinvou3" })),
    /clean HTTPS URL/u,
  );
  const publicRoot = resolve("public-root");
  assert.throws(
    () => loadConfig(environment({
      PINVOU_UPDATE_PUBLIC_ROOT: publicRoot,
      PINVOU_UPDATE_STATE_ROOT: resolve(publicRoot, "private-state"),
    })),
    /must not be inside/u,
  );
});

test("仅显式可信代理可提供 X-Real-IP", () => {
  const rules = parseTrustedProxies("127.0.0.1,172.16.0.0/12,::1");
  assert.equal(isTrustedProxy("127.0.0.1", rules), true);
  assert.equal(isTrustedProxy("::ffff:172.20.5.6", rules), true);
  assert.equal(isTrustedProxy("172.32.0.1", rules), false);
  assert.equal(isTrustedProxy("::1", rules), true);
  assert.throws(() => parseTrustedProxies("not-an-ip"), /invalid address/u);
  assert.throws(() => parseTrustedProxies("2001:db8::/64"), /IPv6 hosts only/u);
});
