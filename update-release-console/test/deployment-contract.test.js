import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

async function source(path) {
  return readFile(join(root, path), "utf8");
}

test("容器、Compose 与 Nginx 保持最小权限和可信代理契约", async () => {
  const [dockerfile, compose, nginx, nginxWrapper] = await Promise.all([
    source("Dockerfile"),
    source("deploy/compose.yaml"),
    source("deploy/nginx-location.conf"),
    source("deploy/nginx-test.conf"),
  ]);
  assert.match(dockerfile, /^FROM node:20-/mu);
  assert.match(dockerfile, /^USER node$/mu);
  assert.match(compose, /PINVOU_UPDATE_TRUSTED_PROXIES:\s+172\.16\.0\.0\/12/u);
  assert.match(compose, /- \/var\/www\/pinvou3:\/srv\/pinvou3/u);
  assert.match(compose, /console-state:\/var\/lib\/pinvou-update-console/u);
  assert.match(compose, /read_only:\s+true/u);
  assert.match(compose, /release-console\/healthz/u);
  assert.match(nginx, /proxy_pass http:\/\/127\.0\.0\.1:8792/u);
  assert.match(nginx, /proxy_set_header X-Real-IP \$remote_addr;/u);
  assert.match(nginx, /proxy_set_header X-Forwarded-For \$remote_addr;/u);
  assert.match(nginx, /client_max_body_size 4224m;/u);
  assert.match(nginxWrapper, /include \/etc\/nginx\/pinvou3-release-location\.conf;/u);
  assert.match(nginxWrapper, /events \{\}/u);
});

test("发布复制和目录持久化契约不依赖跨挂载 rename", async () => {
  const implementation = await source("src/release-service.js");
  assert.match(implementation, /\.pinvou3-release-\$\{randomUUID\(\)\}\.tmp/u);
  assert.match(implementation, /createWriteStream\(temporaryPath, \{ flags: "wx"/u);
  assert.match(implementation, /await handle\.chmod\(mode\)/u);
  assert.match(implementation, /await requirePublicFile\(this\.manifestPath, "latest\.json"\)/u);
  assert.match(implementation, /await syncFile\(temporaryPath\)/u);
  assert.match(implementation, /await rename\(temporaryPath, destinationPath\)/u);
  assert.match(implementation, /await syncDirectory\(publicDirectory\)/u);
  assert.doesNotMatch(implementation, /rename\(stagingPath,\s*destinationPath\)/u);
});

test("healthz 只读取初始化缓存且不回显内部错误", async () => {
  const [server, service] = await Promise.all([
    source("src/server.js"),
    source("src/release-service.js"),
  ]);
  assert.match(server, /const health = service\.getHealthStatus\(\)/u);
  assert.doesNotMatch(server, /healthz[\s\S]{0,300}checkReadiness/u);
  assert.doesNotMatch(server, /not_ready", message:/u);
  assert.match(service, /\[this\.stagingDir, "stagingDir"\]/u);
  assert.match(service, /\[this\.historyDir, "historyDir"\]/u);
  assert.match(service, /for \(const \[path, label\] of directories\) await verifyWritableDirectory/u);
});

test("管理员令牌输入禁止浏览器自动填充", async () => {
  const page = await source("public/index.html");
  assert.match(
    page,
    /id="tokenInput"[^>]*type="password"[^>]*autocomplete="off"[^>]*autocapitalize="none"[^>]*spellcheck="false"/u,
  );
});
