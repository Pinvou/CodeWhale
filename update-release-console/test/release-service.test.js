import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  compareVersions,
  mergeWindowsEntry,
  parseVersion,
  verifyOnlineAsset,
  ReleaseService,
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

test("初始化要求有效 publicRoot/latest.json 并清理自有陈旧临时文件", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-console-readiness-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const publicRoot = join(root, "public");
  const stateRoot = join(root, "state");
  const config = {
    publicRoot,
    stateRoot,
    publicBaseUrl: "https://updates.example.test/pinvou3",
    onlineVerifyTimeoutMs: 5000,
  };
  const service = new ReleaseService(config);
  assert.deepEqual(service.getHealthStatus(), { status: "not_ready" });
  await assert.rejects(service.initialize(), (error) => error.code === "not_ready");

  await mkdir(publicRoot);
  await writeFile(join(publicRoot, "latest.json"), "not-json");
  await assert.rejects(service.initialize(), (error) => error.code === "invalid_json");

  await writeFile(join(publicRoot, "latest.json"), "{}\n");
  const ownedTemporary = ".pinvou3-release-123e4567-e89b-42d3-a456-426614174000.tmp";
  const manifestTemporary = "latest.json.123e4567-e89b-42d3-a456-426614174001.tmp";
  const draftTemporary = "windows-draft.json.123e4567-e89b-42d3-a456-426614174002.tmp";
  const historyTemporary = "1-123e4567-e89b-42d3-a456-426614174003.json.123e4567-e89b-42d3-a456-426614174004.tmp";
  await writeFile(join(publicRoot, ownedTemporary), "stale");
  await writeFile(join(publicRoot, manifestTemporary), "stale");
  await writeFile(join(stateRoot, draftTemporary), "stale");
  await writeFile(join(service.historyDir, historyTemporary), "stale");
  await writeFile(join(publicRoot, "customer.tmp"), "keep");
  await service.initialize();
  assert.deepEqual(service.getHealthStatus(), { status: "ok" });
  assert.equal((await readdir(publicRoot)).includes(ownedTemporary), false);
  assert.equal((await readdir(publicRoot)).includes(manifestTemporary), false);
  assert.equal((await readdir(stateRoot)).includes(draftTemporary), false);
  assert.equal((await readdir(service.historyDir)).includes(historyTemporary), false);
  assert.equal(await readFile(join(publicRoot, "customer.tmp"), "utf8"), "keep");

  await rm(service.stagingDir, { recursive: true });
  await writeFile(service.stagingDir, "not-a-directory");
  await assert.rejects(service.initialize());
  assert.deepEqual(service.getHealthStatus(), { status: "not_ready" });
});
