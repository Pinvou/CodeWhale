import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createServer } from "node:http";
import { PassThrough } from "node:stream";
import { chmod, mkdir, mkdtemp, readFile, readdir, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  compareVersions,
  mergeWindowsEntry,
  parseVersion,
  verifyOnlineAsset,
  receiveUpload,
  ReleaseService,
} from "../src/release-service.js";
import { peFixture } from "./pe-fixture.js";

function multipartRequest(content, version = "0.10.4", boundary = `pinvou-${Date.now()}-${Math.random()}`) {
  const sha256 = createHash("sha256").update(content).digest("hex");
  const fields = [
    ["version", version],
    ["gitCommit", "abcdef1234567890"],
    ["expectedSha256", sha256],
    ["notes", "fixture release"],
    ["operator", "测试发布员"],
  ];
  const chunks = [];
  for (const [name, value] of fields) {
    chunks.push(Buffer.from(
      `--${boundary}\r\nContent-Disposition: form-data; name="${name}"\r\n\r\n${value}\r\n`,
    ));
  }
  chunks.push(Buffer.from(
    `--${boundary}\r\nContent-Disposition: form-data; name="installer"; filename="pinvou3_${version}_x64-setup.exe"\r\nContent-Type: application/octet-stream\r\n\r\n`,
  ));
  chunks.push(content);
  chunks.push(Buffer.from(`\r\n--${boundary}--\r\n`));
  const body = Buffer.concat(chunks);
  const request = new PassThrough();
  request.headers = {
    "content-type": `multipart/form-data; boundary=${boundary}`,
    "content-length": String(body.length),
  };
  request.aborted = false;
  return { body, request };
}

async function waitFor(check, timeoutMs = 1000) {
  const deadline = Date.now() + timeoutMs;
  while (!(await check())) {
    if (Date.now() >= deadline) throw new Error("timed out waiting for condition");
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 10));
  }
}

test("客户端中断 multipart 时返回 upload_aborted 并清理暂存文件", async (t) => {
  const stagingDir = await mkdtemp(join(tmpdir(), "pinvou-aborted-upload-"));
  t.after(() => rm(stagingDir, { recursive: true, force: true }));
  const request = new PassThrough();
  request.headers = { "content-type": "multipart/form-data; boundary=pinvou-boundary" };
  request.aborted = false;
  const receiving = receiveUpload(request, stagingDir, 1024 * 1024);
  request.write("--pinvou-boundary\r\nContent-Disposition: form-data; name=\"version\"\r\n\r\n0.10.4\r\n");
  request.aborted = true;
  request.emit("aborted");
  request.destroy();
  await assert.rejects(
    receiving,
    (error) => error.status === 400 && error.code === "upload_aborted",
  );
  assert.deepEqual(await readdir(stagingDir), []);
});

test("上传长时间无数据时返回 upload_idle_timeout 并清理暂存文件", async (t) => {
  const stagingDir = await mkdtemp(join(tmpdir(), "pinvou-idle-upload-"));
  t.after(() => rm(stagingDir, { recursive: true, force: true }));
  const stalled = multipartRequest(Buffer.alloc(1024), "0.10.4", "pinvou-idle");
  const receiving = receiveUpload(stalled.request, stagingDir, 1024 * 1024, 100);
  stalled.request.write(stalled.body.subarray(0, stalled.body.length - 8));
  await waitFor(async () => (await readdir(stagingDir)).length === 1);
  await assert.rejects(
    receiving,
    (error) => error.status === 408 && error.code === "upload_idle_timeout",
  );
  assert.deepEqual(await readdir(stagingDir), []);
});

test("未完成上传不占发布锁，其他完整上传仍可生成草稿", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-unlocked-upload-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const publicRoot = join(root, "public");
  const stateRoot = join(root, "state");
  await mkdir(publicRoot);
  await writeFile(join(publicRoot, "latest.json"), "{}\n");
  if (process.platform !== "win32") await chmod(join(publicRoot, "latest.json"), 0o644);
  const service = new ReleaseService({
    publicRoot,
    stateRoot,
    publicBaseUrl: "https://updates.example.test/pinvou3",
    maxUploadBytes: 1024 * 1024,
    uploadIdleTimeoutMs: 5000,
    onlineVerifyTimeoutMs: 5000,
  });
  await service.initialize();

  const installer = peFixture({ productVersion: "0.10.4.0" });
  const stalled = multipartRequest(installer, "0.10.4", "pinvou-stalled");
  const stalledResult = service.createDraft(stalled.request, "192.0.2.1").then(
    (value) => ({ value }),
    (error) => ({ error }),
  );
  stalled.request.write(stalled.body.subarray(0, stalled.body.length - 8));
  await waitFor(async () => (await readdir(service.stagingDir)).length === 1);
  await assert.rejects(stat(service.lockPath), (error) => error.code === "ENOENT");

  const complete = multipartRequest(installer, "0.10.4", "pinvou-complete");
  const completedDraft = service.createDraft(complete.request, "192.0.2.2");
  complete.request.end(complete.body);
  const draft = await completedDraft;
  assert.equal(draft.entry.version, "0.10.4");

  stalled.request.aborted = true;
  stalled.request.emit("aborted");
  stalled.request.destroy();
  const stalledOutcome = await stalledResult;
  assert.equal(stalledOutcome.error?.code, "upload_aborted");
  assert.equal((await readdir(service.stagingDir)).length, 1);
  await assert.rejects(stat(service.lockPath), (error) => error.code === "ENOENT");
});

test("草稿审计失败时在发布锁内回滚草稿", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-draft-audit-failure-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const publicRoot = join(root, "public");
  const stateRoot = join(root, "state");
  await mkdir(publicRoot);
  await writeFile(join(publicRoot, "latest.json"), "{}\n");
  if (process.platform !== "win32") await chmod(join(publicRoot, "latest.json"), 0o644);
  const service = new ReleaseService({
    publicRoot,
    stateRoot,
    publicBaseUrl: "https://updates.example.test/pinvou3",
    maxUploadBytes: 1024 * 1024,
    uploadIdleTimeoutMs: 5000,
    onlineVerifyTimeoutMs: 5000,
  });
  await service.initialize();

  let auditStartedResolve;
  let rejectAudit;
  const auditStarted = new Promise((resolvePromise) => {
    auditStartedResolve = resolvePromise;
  });
  const auditResult = new Promise((_resolvePromise, rejectPromise) => {
    rejectAudit = () => rejectPromise(new Error("injected audit failure"));
  });
  service.audit = async () => {
    auditStartedResolve();
    return auditResult;
  };

  const installer = peFixture({ productVersion: "0.10.4.0" });
  const upload = multipartRequest(installer, "0.10.4", "pinvou-audit-failure");
  const creationResult = service.createDraft(upload.request, "192.0.2.1").then(
    (value) => ({ value }),
    (error) => ({ error }),
  );
  upload.request.end(upload.body);
  await auditStarted;

  const transientDraft = await service.getDraft();
  assert.ok(transientDraft);
  assert.ok(await stat(service.lockPath));
  await assert.rejects(
    service.cancelDraft(transientDraft.id, "并发操作员", "192.0.2.2"),
    (error) => error.code === "publish_locked",
  );

  rejectAudit();
  const creationOutcome = await creationResult;
  assert.match(creationOutcome.error?.message || "", /injected audit failure/u);
  assert.equal(await service.getDraft(), null);
  assert.deepEqual(await readdir(service.stagingDir), []);
  await assert.rejects(stat(service.lockPath), (error) => error.code === "ENOENT");
});

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

test("初始化拒绝非公开权限和符号链接清单", {
  skip: process.platform === "win32" ? "Windows 不提供稳定的 Unix 权限语义" : false,
}, async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-console-manifest-mode-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const publicRoot = join(root, "public");
  const stateRoot = join(root, "state");
  const manifestPath = join(publicRoot, "latest.json");
  const symlinkTarget = join(root, "manifest-target.json");
  const config = {
    publicRoot,
    stateRoot,
    publicBaseUrl: "https://updates.example.test/pinvou3",
    onlineVerifyTimeoutMs: 5000,
  };
  await mkdir(publicRoot);
  await writeFile(manifestPath, "{}\n");
  await chmod(manifestPath, 0o600);
  await assert.rejects(
    new ReleaseService(config).initialize(),
    (error) => error.code === "not_ready" && /0644/u.test(error.message),
  );

  await rm(manifestPath);
  await writeFile(symlinkTarget, "{}\n");
  await chmod(symlinkTarget, 0o644);
  await symlink(symlinkTarget, manifestPath);
  await assert.rejects(
    new ReleaseService(config).initialize(),
    (error) => error.code === "not_ready" && /实际普通文件/u.test(error.message),
  );
});
