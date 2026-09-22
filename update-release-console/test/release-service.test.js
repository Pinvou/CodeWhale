import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import {
  compareVersions,
  mergeWindowsEntry,
  parseVersion,
  verifyOnlineAsset,
} from "../src/release-service.js";

test("版本比较支持三段和四段数字版本", () => {
  assert.equal(compareVersions("0.10.5", "0.10.4"), 1);
  assert.equal(compareVersions("0.10.4.1", "0.10.4"), 1);
  assert.equal(compareVersions("1.0.0", "1.0.0.0"), 0);
  assert.equal(compareVersions("0.9.99", "0.10.0"), -1);
  assert.deepEqual(parseVersion("12.3.4.5"), [12, 3, 4, 5]);
  assert.throws(() => parseVersion("1.2.3-beta"), /三段或四段/);
});

test("合并 Windows 节点不会修改或丢失其他平台", () => {
  const manifest = {
    version: "0.8.0",
    platforms: {
      "linux-x86_64": { version: "0.8.0", url: "https://example.test/linux.deb" },
      "macos-universal": { version: "0.9.0", url: "https://example.test/mac.dmg" },
    },
    custom: { retained: true },
  };
  const entry = { version: "0.10.0", sha256: "a".repeat(64) };
  const merged = mergeWindowsEntry(manifest, entry);

  assert.deepEqual(merged.platforms["linux-x86_64"], manifest.platforms["linux-x86_64"]);
  assert.deepEqual(merged.platforms["macos-universal"], manifest.platforms["macos-universal"]);
  assert.deepEqual(merged.platforms["windows-x86_64"], entry);
  assert.deepEqual(merged.custom, { retained: true });
  assert.equal(manifest.platforms["windows-x86_64"], undefined);
});

test("线上安装器通过 HTTP 响应体重新计算大小和 SHA-256", async (t) => {
  const content = Buffer.from("online-installer", "utf8");
  const server = createServer((_request, response) => {
    response.writeHead(200, { "Content-Length": content.length });
    response.end(content);
  });
  await new Promise((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
  t.after(() => new Promise((resolvePromise) => {
    server.close(resolvePromise);
    server.closeAllConnections();
  }));
  const { port } = server.address();
  const sha256 = createHash("sha256").update(content).digest("hex");
  assert.deepEqual(
    await verifyOnlineAsset(`http://127.0.0.1:${port}/installer.exe`, { size: content.length, sha256 }, 5000),
    { size: content.length, sha256 },
  );
  await assert.rejects(
    verifyOnlineAsset(
      `http://127.0.0.1:${port}/installer.exe`,
      { size: content.length, sha256: "0".repeat(64) },
      5000,
    ),
    (error) => error.code === "online_sha256_mismatch",
  );
});
