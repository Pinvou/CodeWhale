import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createAppServer } from "../src/server.js";
import { ReleaseService } from "../src/release-service.js";
import { AppError } from "../src/errors.js";

const TOKEN = "test-admin-token-that-is-at-least-32-characters";

function fakePe(payload) {
  const content = Buffer.alloc(132 + Buffer.byteLength(payload));
  content.write("MZ", 0, "ascii");
  content.writeUInt32LE(128, 0x3c);
  content.write("PE\0\0", 128, "binary");
  content.write(payload, 132, "utf8");
  return content;
}

function requestHeaders(extra = {}) {
  return { Authorization: `Bearer ${TOKEN}`, ...extra };
}

async function json(response) {
  const payload = await response.json();
  if (!response.ok) throw new Error(`${response.status}: ${JSON.stringify(payload)}`);
  return payload;
}

async function startFixture(t, options = {}) {
  const root = await mkdtemp(join(tmpdir(), "pinvou-update-console-"));
  const publicRoot = join(root, "public");
  const stateRoot = join(root, "state");
  const initialManifest = {
    version: "0.8.0",
    url: "https://updates.example.test/linux.deb",
    platforms: {
      "linux-x86_64": { version: "0.8.0", marker: "keep-linux" },
      "macos-universal": { version: "0.9.0", marker: "keep-mac" },
      "windows-x86_64": {
        version: "0.10.3",
        url: "https://updates.example.test/pinvou3_0.10.3_x64-setup.exe",
        format: "nsis",
        sha256: "1".repeat(64),
        size: 42,
        restart_after_install: false,
        notes: "previous",
        pub_date: "2026-09-20T00:00:00.000Z",
      },
    },
  };
  await writeFile(join(root, "seed.json"), JSON.stringify(initialManifest));
  const config = {
    host: "127.0.0.1",
    port: 0,
    basePath: "/console",
    publicRoot,
    stateRoot,
    publicBaseUrl: "https://updates.example.test/pinvou3",
    adminToken: TOKEN,
    maxUploadBytes: 1024 * 1024,
    onlineVerifyTimeoutMs: 10_000,
  };
  const onlineVerifications = [];
  const service = new ReleaseService(config, {
    onlineVerifier: options.onlineVerifier || (async (url, expected) => {
      onlineVerifications.push({ url, expected });
      return expected;
    }),
  });
  const { server } = await createAppServer(config, { service });
  await writeFile(service.manifestPath, `${JSON.stringify(initialManifest, null, 2)}\n`);
  await new Promise((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
  const address = server.address();
  const baseUrl = `http://127.0.0.1:${address.port}/console/`;
  t.after(async () => {
    await new Promise((resolvePromise) => {
      server.close(resolvePromise);
      server.closeAllConnections();
    });
    await rm(root, { recursive: true, force: true });
  });
  return { baseUrl, service, initialManifest, onlineVerifications };
}

test("上传、草稿、发布和回滚形成完整闭环", async (t) => {
  const { baseUrl, service, initialManifest, onlineVerifications } = await startFixture(t);
  const pageResponse = await fetch(baseUrl);
  const page = await pageResponse.text();
  assert.equal(pageResponse.status, 200);
  assert.match(pageResponse.headers.get("content-security-policy"), /script-src 'nonce-[^']+'/u);
  assert.doesNotMatch(page, /__CSP_NONCE__/u);
  assert.match(page, /Windows 更新发布/u);

  const unauthorized = await fetch(`${baseUrl}api/status`);
  assert.equal(unauthorized.status, 401);

  const installer = fakePe("signed-nsis-fixture\0binary-content");
  const expectedSha256 = createHash("sha256").update(installer).digest("hex");
  const form = new FormData();
  form.set("version", "0.10.4");
  form.set("gitCommit", "abcdef1234567890");
  form.set("expectedSha256", expectedSha256);
  form.set("notes", "Windows 第一阶段发布台验证\n第二行更新说明");
  form.set("operator", "测试发布员");
  form.set("installer", new Blob([installer]), "pinvou3_0.10.4_x64-setup.exe");

  const draft = await json(await fetch(`${baseUrl}api/releases/windows/upload`, {
    method: "POST",
    headers: requestHeaders(),
    body: form,
  }));
  assert.equal(draft.status, "draft");
  assert.equal(draft.entry.sha256, expectedSha256);
  assert.equal(draft.entry.size, installer.length);
  assert.match(draft.entry.notes, /第二行更新说明/u);
  assert.equal(draft.preview_manifest.platforms["linux-x86_64"].marker, "keep-linux");
  assert.deepEqual(JSON.parse(await readFile(service.manifestPath, "utf8")), initialManifest);

  const changedManifest = JSON.parse(await readFile(service.manifestPath, "utf8"));
  changedManifest.platforms["macos-universal"].marker = "mac-changed-after-draft";
  await writeFile(service.manifestPath, `${JSON.stringify(changedManifest, null, 2)}\n`);

  const published = await json(await fetch(`${baseUrl}api/releases/windows/publish`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify({ draftId: draft.id, operator: "复核发布员" }),
  }));
  assert.equal(published.entry.version, "0.10.4");
  const onlineManifest = JSON.parse(await readFile(service.manifestPath, "utf8"));
  assert.equal(onlineManifest.platforms["windows-x86_64"].sha256, expectedSha256);
  assert.equal(onlineManifest.platforms["linux-x86_64"].marker, "keep-linux");
  assert.equal(onlineManifest.platforms["macos-universal"].marker, "mac-changed-after-draft");
  assert.deepEqual(await readFile(join(service.config.publicRoot, draft.file_name)), installer);
  assert.equal(await service.getDraft(), null);
  assert.deepEqual(onlineVerifications, [{
    url: "https://updates.example.test/pinvou3/pinvou3_0.10.4_x64-setup.exe",
    expected: { sha256: expectedSha256, size: installer.length },
  }]);

  const rollback = await json(await fetch(`${baseUrl}api/releases/windows/rollback`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify({ releaseId: published.id, operator: "值班发布员", reason: "真实环境验证发现阻断问题" }),
  }));
  assert.equal(rollback.type, "rollback");
  const rolledBackManifest = JSON.parse(await readFile(service.manifestPath, "utf8"));
  assert.equal(rolledBackManifest.platforms["windows-x86_64"].version, "0.10.3");
  assert.equal(rolledBackManifest.platforms["macos-universal"].marker, "mac-changed-after-draft");
  assert.deepEqual(await readFile(join(service.config.publicRoot, draft.file_name)), installer);

  const audit = await readFile(service.auditPath, "utf8");
  assert.match(audit, /"action":"draft_created"/u);
  assert.match(audit, /"action":"release_published"/u);
  assert.match(audit, /"action":"release_rolled_back"/u);
  assert.doesNotMatch(audit, new RegExp(TOKEN, "u"));
});

test("SHA-256 不一致时拒绝草稿且不残留暂存文件", async (t) => {
  const { baseUrl, service } = await startFixture(t);
  const form = new FormData();
  form.set("version", "0.10.4");
  form.set("gitCommit", "abcdef1");
  form.set("expectedSha256", "a".repeat(64));
  form.set("notes", "hash mismatch");
  form.set("operator", "测试发布员");
  form.set("installer", new Blob([fakePe("different")]), "pinvou3_0.10.4_x64-setup.exe");
  const response = await fetch(`${baseUrl}api/releases/windows/upload`, {
    method: "POST",
    headers: requestHeaders(),
    body: form,
  });
  assert.equal(response.status, 400);
  assert.equal((await response.json()).error, "sha256_mismatch");
  assert.equal(await service.getDraft(), null);
  assert.deepEqual(await readdir(service.stagingDir), []);
});

test("非 PE 文件即使摘要一致也不能生成草稿", async (t) => {
  const { baseUrl, service } = await startFixture(t);
  const content = Buffer.from("not-a-windows-executable", "utf8");
  const form = new FormData();
  form.set("version", "0.10.4");
  form.set("gitCommit", "abcdef1");
  form.set("expectedSha256", createHash("sha256").update(content).digest("hex"));
  form.set("notes", "invalid pe");
  form.set("operator", "测试发布员");
  form.set("installer", new Blob([content]), "pinvou3_0.10.4_x64-setup.exe");
  const response = await fetch(`${baseUrl}api/releases/windows/upload`, {
    method: "POST",
    headers: requestHeaders(),
    body: form,
  });
  assert.equal(response.status, 400);
  assert.equal((await response.json()).error, "invalid_installer");
  assert.equal(await service.getDraft(), null);
  assert.deepEqual(await readdir(service.stagingDir), []);
});

test("HTTPS 回读失败时不切换清单，并可在网络恢复后重试", async (t) => {
  let shouldFail = true;
  const { baseUrl, service, initialManifest } = await startFixture(t, {
    onlineVerifier: async (_url, expected) => {
      if (shouldFail) throw new AppError(502, "online_asset_unavailable", "模拟线上回读失败");
      return expected;
    },
  });
  const installer = fakePe("retryable-signed-installer");
  const sha256 = createHash("sha256").update(installer).digest("hex");
  const form = new FormData();
  form.set("version", "0.10.4");
  form.set("gitCommit", "abcdef123");
  form.set("expectedSha256", sha256);
  form.set("notes", "online retry");
  form.set("operator", "测试发布员");
  form.set("installer", new Blob([installer]), "pinvou3_0.10.4_x64-setup.exe");
  const draft = await json(await fetch(`${baseUrl}api/releases/windows/upload`, {
    method: "POST", headers: requestHeaders(), body: form,
  }));

  const failedPublish = await fetch(`${baseUrl}api/releases/windows/publish`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify({ draftId: draft.id, operator: "测试发布员" }),
  });
  assert.equal(failedPublish.status, 502);
  assert.deepEqual(JSON.parse(await readFile(service.manifestPath, "utf8")), initialManifest);
  assert.equal((await service.getDraft()).id, draft.id);
  assert.deepEqual(await readFile(join(service.config.publicRoot, draft.file_name)), installer);

  shouldFail = false;
  const published = await json(await fetch(`${baseUrl}api/releases/windows/publish`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify({ draftId: draft.id, operator: "重试发布员" }),
  }));
  assert.equal(published.entry.version, "0.10.4");
  assert.equal((JSON.parse(await readFile(service.manifestPath, "utf8"))).platforms["windows-x86_64"].sha256, sha256);
});
