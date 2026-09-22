import test from "node:test";
import assert from "node:assert/strict";
import { resolve } from "node:path";
import { loadConfig } from "../src/config.js";

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
