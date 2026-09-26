import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { request as httpRequest } from "node:http";
import { chmod, mkdir, mkdtemp, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { AuthFailureLimiter, createAppServer } from "../src/server.js";
import { ReleaseService } from "../src/release-service.js";
import { AppError } from "../src/errors.js";
import { peFixture } from "./pe-fixture.js";

const TOKEN = "test-admin-token-that-is-at-least-32-characters";

test("认证失败限流器具有 TTL 和容量上限", () => {
  let now = 1_000;
  const limiter = new AuthFailureLimiter({
    authFailureLimit: 2,
    authFailureWindowMs: 1_000,
    authBlockMs: 2_000,
    authLimiterTtlMs: 3_000,
    authLimiterMaxEntries: 2,
  }, () => now);
  limiter.recordFailure("192.0.2.1");
  now += 1;
  limiter.recordFailure("192.0.2.2");
  now += 1;
  limiter.recordFailure("192.0.2.3");
  assert.ok(limiter.entries.size <= 2);
  now += 4_000;
  assert.equal(limiter.retryAfter("192.0.2.3"), 0);
  assert.equal(limiter.entries.size, 0);
});

function requestHeaders(extra = {}) {
  return { Authorization: `Bearer ${TOKEN}`, ...extra };
}

async function json(response) {
  const payload = await response.json();
  if (!response.ok) throw new Error(`${response.status}: ${JSON.stringify(payload)}`);
  return payload;
}

async function uploadInstaller(baseUrl, content, version = "0.10.4", extraHeaders = {}) {
  const form = new FormData();
  form.set("version", version);
  form.set("gitCommit", "abcdef1234567890");
  form.set("expectedSha256", createHash("sha256").update(content).digest("hex"));
  form.set("notes", "fixture release");
  form.set("operator", "测试发布员");
  form.set("installer", new Blob([content]), `pinvou3_${version}_x64-setup.exe`);
  return fetch(`${baseUrl}api/releases/windows/upload`, {
    method: "POST",
    headers: requestHeaders(extraHeaders),
    body: form,
  });
}

async function startFixture(t, options = {}) {
  const root = await mkdtemp(join(tmpdir(), "pinvou-update-console-"));
  const publicRoot = join(root, "public");
  const stateRoot = options.stateRoot || join(root, "state");
  const previousInstaller = peFixture({ productVersion: "0.10.3.0" });
  const previousSha256 = createHash("sha256").update(previousInstaller).digest("hex");
  const initialManifest = {
    version: "0.8.0",
    url: "https://updates.example.test/linux.deb",
    platforms: {
      "linux-x86_64": { version: "0.8.0", marker: "keep-linux" },
      "macos-universal": { version: "0.9.0", marker: "keep-mac" },
      "windows-x86_64": {
        version: "0.10.3",
        url: "https://updates.example.test/pinvou3/pinvou3_0.10.3_x64-setup.exe",
        format: "nsis",
        sha256: previousSha256,
        size: previousInstaller.length,
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
    trustedProxies: options.trustedProxies || [],
    ...(options.config || {}),
  };
  await mkdir(publicRoot, { recursive: true });
  await writeFile(join(publicRoot, "pinvou3_0.10.3_x64-setup.exe"), previousInstaller);
  const manifestPath = join(publicRoot, "latest.json");
  await writeFile(manifestPath, `${JSON.stringify(initialManifest, null, 2)}\n`);
  await chmod(manifestPath, 0o644);
  const onlineVerifications = [];
  const service = new ReleaseService(config, {
    onlineVerifier: options.onlineVerifier || (async (url, expected) => {
      onlineVerifications.push({ url, expected });
      return expected;
    }),
  });
  const { server } = await createAppServer(config, { service });
  await new Promise((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
  const address = server.address();
  const baseUrl = `http://127.0.0.1:${address.port}/console/`;
  t.after(async () => {
    await new Promise((resolvePromise) => {
      server.close(resolvePromise);
      server.closeAllConnections();
    });
    await rm(root, { recursive: true, force: true });
    if (options.stateRoot) await rm(options.stateRoot, { recursive: true, force: true });
  });
  return { baseUrl, service, initialManifest, onlineVerifications };
}

test("真实 HTTP 闲置上传收到结构化 408 并关闭连接", async (t) => {
  const { baseUrl, service } = await startFixture(t, {
    config: { uploadIdleTimeoutMs: 50 },
  });
  const boundary = "pinvou-real-idle-timeout";
  const result = await new Promise((resolvePromise, rejectPromise) => {
    let settled = false;
    const request = httpRequest(new URL("api/releases/windows/upload", baseUrl), {
      method: "POST",
      headers: requestHeaders({
        "Content-Type": `multipart/form-data; boundary=${boundary}`,
        "Transfer-Encoding": "chunked",
      }),
    }, (response) => {
      const chunks = [];
      response.on("data", (chunk) => chunks.push(chunk));
      response.on("end", () => {
        settled = true;
        resolvePromise({
          status: response.statusCode,
          headers: response.headers,
          payload: JSON.parse(Buffer.concat(chunks).toString("utf8")),
        });
      });
    });
    request.on("error", (error) => {
      if (!settled) rejectPromise(error);
    });
    request.write(
      `--${boundary}\r\nContent-Disposition: form-data; name="installer"; filename="pinvou3_0.10.4_x64-setup.exe"\r\nContent-Type: application/octet-stream\r\n\r\npartial`,
    );
  });

  assert.equal(result.status, 408);
  assert.equal(result.headers.connection, "close");
  assert.equal(result.payload.error, "upload_idle_timeout");
  assert.deepEqual(await readdir(service.stagingDir), []);
  assert.equal(await service.getDraft(), null);
});

test("上传、草稿、发布和回滚形成完整闭环", async (t) => {
  const previousUmask = process.platform === "win32" ? null : process.umask(0o077);
  if (previousUmask !== null) t.after(() => process.umask(previousUmask));
  const { baseUrl, service, initialManifest, onlineVerifications } = await startFixture(t);
  if (process.platform !== "win32") {
    assert.equal((await stat(service.manifestPath)).mode & 0o777, 0o644);
  }
  const pageResponse = await fetch(baseUrl);
  const page = await pageResponse.text();
  assert.equal(pageResponse.status, 200);
  assert.match(pageResponse.headers.get("content-security-policy"), /script-src 'nonce-[^']+'/u);
  assert.doesNotMatch(page, /__CSP_NONCE__/u);
  assert.match(page, /Windows 更新发布/u);

  const unauthorized = await fetch(`${baseUrl}api/status`);
  assert.equal(unauthorized.status, 401);

  const installer = peFixture({ productVersion: "0.10.4.0" });
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
  if (process.platform !== "win32") {
    assert.equal((await stat(service.draftPath)).mode & 0o777, 0o600);
  }

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
  if (process.platform !== "win32") {
    assert.equal((await stat(service.manifestPath)).mode & 0o777, 0o644);
    assert.equal(
      (await stat(join(service.config.publicRoot, draft.file_name))).mode & 0o777,
      0o644,
    );
    const [historyName] = await readdir(service.historyDir);
    assert.equal((await stat(join(service.historyDir, historyName))).mode & 0o777, 0o600);
  }
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
  if (process.platform !== "win32") {
    assert.equal((await stat(service.auditPath)).mode & 0o777, 0o600);
  }
});

test("healthz 使用初始化缓存且不会按请求创建写探针", async (t) => {
  const { baseUrl, service } = await startFixture(t);
  await writeFile(service.manifestPath, "{broken-json");
  const beforePublic = await stat(service.config.publicRoot, { bigint: true });
  const beforeState = await stat(service.config.stateRoot, { bigint: true });
  for (let request = 0; request < 5; request += 1) {
    const response = await fetch(`${baseUrl}healthz`);
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { status: "ok" });
  }
  const afterPublic = await stat(service.config.publicRoot, { bigint: true });
  const afterState = await stat(service.config.stateRoot, { bigint: true });
  assert.equal(afterPublic.mtimeNs, beforePublic.mtimeNs);
  assert.equal(afterState.mtimeNs, beforeState.mtimeNs);
});

test("healthz 未就绪时仅返回通用状态", async (t) => {
  const config = {
    host: "127.0.0.1",
    port: 0,
    basePath: "/console",
    adminToken: TOKEN,
    trustedProxies: [],
  };
  const service = {
    async initialize() {},
    getHealthStatus() { return { status: "not_ready" }; },
  };
  const { server } = await createAppServer(config, { service });
  await new Promise((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
  t.after(() => new Promise((resolvePromise) => server.close(resolvePromise)));
  const response = await fetch(`http://127.0.0.1:${server.address().port}/console/healthz`);
  assert.equal(response.status, 503);
  assert.deepEqual(await response.json(), { status: "not_ready" });
});

test("仅从显式可信反向代理记录 X-Real-IP", async (t) => {
  const trusted = await startFixture(t, {
    trustedProxies: [{ address: "127.0.0.1", family: 4, prefix: 32, network: 0x7f000001 }],
  });
  const installer = peFixture({ productVersion: "0.10.4.0" });
  const draft = await json(await uploadInstaller(
    trusted.baseUrl,
    installer,
    "0.10.4",
    { "X-Real-IP": "203.0.113.9" },
  ));
  await json(await fetch(`${trusted.baseUrl}api/releases/windows/draft`, {
    method: "DELETE",
    headers: requestHeaders({ "Content-Type": "application/json", "X-Real-IP": "203.0.113.9" }),
    body: JSON.stringify({ draftId: draft.id, operator: "测试发布员" }),
  }));
  const audit = await readFile(trusted.service.auditPath, "utf8");
  assert.match(audit, /"remote_address":"203\.0\.113\.9"/u);

  const untrusted = await startFixture(t);
  const untrustedDraft = await json(await uploadInstaller(
    untrusted.baseUrl,
    installer,
    "0.10.4",
    { "X-Real-IP": "198.51.100.7" },
  ));
  assert.ok(untrustedDraft.id);
  const untrustedAudit = await readFile(untrusted.service.auditPath, "utf8");
  assert.doesNotMatch(untrustedAudit, /198\.51\.100\.7/u);
});

test("认证失败按可信代理解析后的客户端 IP 限流且成功后清除", async (t) => {
  const fixture = await startFixture(t, {
    trustedProxies: [{ address: "127.0.0.1", family: 4, prefix: 32, network: 0x7f000001 }],
    config: {
      authFailureLimit: 2,
      authFailureWindowMs: 60_000,
      authBlockMs: 60_000,
      authLimiterTtlMs: 120_000,
      authLimiterMaxEntries: 100,
    },
  });
  const invalidHeaders = {
    Authorization: `Bearer ${"x".repeat(32)}`,
    "X-Real-IP": "203.0.113.8",
  };
  const first = await fetch(`${fixture.baseUrl}api/status`, { headers: invalidHeaders });
  assert.equal(first.status, 401);
  const blocked = await fetch(`${fixture.baseUrl}api/status`, { headers: invalidHeaders });
  assert.equal(blocked.status, 429);
  assert.equal(blocked.headers.get("retry-after"), "60");

  const otherClient = await fetch(`${fixture.baseUrl}api/status`, {
    headers: { ...invalidHeaders, "X-Real-IP": "203.0.113.9" },
  });
  assert.equal(otherClient.status, 401);

  const success = await fetch(`${fixture.baseUrl}api/status`, {
    headers: requestHeaders({ "X-Real-IP": "203.0.113.8" }),
  });
  assert.equal(success.status, 200);
  const afterSuccess = await fetch(`${fixture.baseUrl}api/status`, { headers: invalidHeaders });
  assert.equal(afterSuccess.status, 401);
});

test("SHA-256 不一致时拒绝草稿且不残留暂存文件", async (t) => {
  const { baseUrl, service } = await startFixture(t);
  const form = new FormData();
  form.set("version", "0.10.4");
  form.set("gitCommit", "abcdef1");
  form.set("expectedSha256", "a".repeat(64));
  form.set("notes", "hash mismatch");
  form.set("operator", "测试发布员");
  form.set("installer", new Blob([peFixture({ productVersion: "0.10.4.0" })]), "pinvou3_0.10.4_x64-setup.exe");
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

test("接受 AMD64 stub，并拒绝不支持的 stub、版本不匹配和损坏 PE", async (t) => {
  const { baseUrl, service } = await startFixture(t);
  const amd64 = await json(await uploadInstaller(
    baseUrl,
    peFixture({ machine: 0x8664, imageFormat: "pe32+" }),
  ));
  assert.equal(amd64.status, "draft");
  await json(await fetch(`${baseUrl}api/releases/windows/draft`, {
    method: "DELETE",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify({ draftId: amd64.id, operator: "测试发布员" }),
  }));
  const cases = [
    {
      content: peFixture({ productVersion: "0.10.4.0", machine: 0xaa64 }),
      error: "invalid_installer_arch",
    },
    {
      content: peFixture({ productVersion: "0.10.5.0" }),
      error: "installer_version_mismatch",
    },
    {
      content: (() => {
        const value = peFixture({ productVersion: "0.10.4.0" });
        value.writeUInt32LE(0xfffffff0, 0x400 + 20);
        return value;
      })(),
      error: "invalid_installer",
    },
  ];
  for (const fixture of cases) {
    const response = await uploadInstaller(baseUrl, fixture.content);
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error, fixture.error);
    assert.equal(await service.getDraft(), null);
    assert.deepEqual(await readdir(service.stagingDir), []);
  }
});

test("HTTPS 回读失败时不切换清单，并可在网络恢复后重试", async (t) => {
  let shouldFail = true;
  const { baseUrl, service, initialManifest } = await startFixture(t, {
    onlineVerifier: async (_url, expected) => {
      if (shouldFail) throw new AppError(502, "online_asset_unavailable", "模拟线上回读失败");
      return expected;
    },
  });
  const installer = peFixture({ productVersion: "0.10.4.0" });
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

test("回滚目标本地或 HTTPS 复验失败时不修改 manifest", async (t) => {
  let rejectPreviousOnline = false;
  const { baseUrl, service } = await startFixture(t, {
    onlineVerifier: async (url, expected) => {
      if (rejectPreviousOnline && url.includes("0.10.3")) {
        throw new AppError(502, "online_asset_unavailable", "模拟旧版本线上不可用");
      }
      return expected;
    },
  });
  const draft = await json(await uploadInstaller(baseUrl, peFixture({ productVersion: "0.10.4.0" })));
  const published = await json(await fetch(`${baseUrl}api/releases/windows/publish`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify({ draftId: draft.id, operator: "发布员" }),
  }));
  const publishedManifest = await readFile(service.manifestPath, "utf8");
  const rollbackBody = {
    releaseId: published.id,
    operator: "值班发布员",
    reason: "验证失败不应切换清单",
  };

  const previousPath = join(service.config.publicRoot, "pinvou3_0.10.3_x64-setup.exe");
  const previousBytes = await readFile(previousPath);
  await writeFile(previousPath, Buffer.from("tampered"));
  let response = await fetch(`${baseUrl}api/releases/windows/rollback`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify(rollbackBody),
  });
  assert.equal(response.status, 409);
  assert.equal((await response.json()).error, "rollback_asset_changed");
  assert.equal(await readFile(service.manifestPath, "utf8"), publishedManifest);

  await writeFile(previousPath, previousBytes);
  rejectPreviousOnline = true;
  response = await fetch(`${baseUrl}api/releases/windows/rollback`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify(rollbackBody),
  });
  assert.equal(response.status, 502);
  assert.equal((await response.json()).error, "online_asset_unavailable");
  assert.equal(await readFile(service.manifestPath, "utf8"), publishedManifest);
});

test("stateRoot 与 publicRoot 跨文件系统时仍通过同目录临时文件发布", async (t) => {
  if (process.platform === "win32") {
    t.skip("Windows 测试机没有稳定的独立挂载点");
    return;
  }
  let stateRoot;
  try {
    const sharedMemory = await stat("/dev/shm");
    const temporary = await stat(tmpdir());
    if (sharedMemory.dev === temporary.dev) {
      t.skip("/dev/shm 与临时目录位于同一文件系统");
      return;
    }
    stateRoot = await mkdtemp("/dev/shm/pinvou-update-console-state-");
  } catch {
    t.skip("当前环境没有可用的 /dev/shm 独立挂载");
    return;
  }
  const { baseUrl, service } = await startFixture(t, { stateRoot });
  const installer = peFixture({ productVersion: "0.10.4.0" });
  const draft = await json(await uploadInstaller(baseUrl, installer));
  await json(await fetch(`${baseUrl}api/releases/windows/publish`, {
    method: "POST",
    headers: requestHeaders({ "Content-Type": "application/json" }),
    body: JSON.stringify({ draftId: draft.id, operator: "跨挂载测试" }),
  }));
  assert.deepEqual(await readFile(join(service.config.publicRoot, draft.file_name)), installer);
  assert.deepEqual(await readdir(service.stagingDir), []);
  assert.equal(
    (await readdir(service.config.publicRoot)).some((name) => /^\.pinvou3-release-.+\.tmp$/u.test(name)),
    false,
  );
});
