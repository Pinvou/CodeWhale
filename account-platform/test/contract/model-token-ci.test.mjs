import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import vm from 'node:vm';
import YAML from 'yaml';
import { renderNginxFixtures } from '../../../scripts/account-platform-render-nginx-fixtures.mjs';

const workflow = YAML.parse(await readFile(new URL('../../../.github/workflows/pr-check.yml', import.meta.url), 'utf8'));
const job = workflow.jobs['account-platform-test'];
const nginxBuild = await readFile(new URL('../deployment/nginx.Dockerfile', import.meta.url), 'utf8');
const step = name => {
  const found = job.steps.find(item => item.name === name);
  assert.ok(found, `required named gate ${name}`); return found;
};

test('release CI routes all direct sources and retains old heavy gates within a bounded job', () => {
  assert.equal(job['timeout-minutes'], 45);
  const filter = workflow.jobs.changes.steps.find(item => item.with?.filters)?.with.filters;
  const paths = YAML.parse(filter).account_platform;
  for (const required of ['account-platform/**', 'scripts/account-platform-*.mjs', 'pinvou3-app/src/features/account-platform/**', 'pinvou3-app/src/shared/i18n.js', 'pinvou3-app/tests/credential_origin_scope_probe.test.mjs', 'pinvou3-app/tests/admin_token_credential_origin_real_smoke.mjs', 'pinvou3-app/package.json', 'pinvou3-app/package-lock.json']) assert.ok(paths.includes(required), required);
  const oldGo = step('Go 格式、测试与静态检查').run;
  for (const required of ['TestTokenAdmin(Pagination(PostgresIntegration|PostgresMillionMemberBoundary', 'TestTokenOperationsOverviewPerformancePostgresIntegration', 'TestCredentialOriginServicePostgresIntegration', 'go test -short -race ./...', 'go vet ./...']) assert.ok(oldGo.includes(required), required);
  assert.ok(step('Compose 配置和服务镜像构建').run.includes('build account-service billing-service token-gateway token-portal payment-simulator account-event-relay account-migrate'));
});

test('release role suite owns a second loopback cluster and executes real candidate CLI artifacts without SKIP', () => {
  assert.ok(!Object.hasOwn(job.env, 'PINVOU_TEST_RELEASE_DATABASE_URL'));
  const gate = step('专属集群的发布角色与原 CLI 实际闭环');
  assert.equal(gate['timeout-minutes'], 8);
  for (const required of ['127.0.0.1:5433:5432', 'POSTGRES_HOST_AUTH_METHOD=trust', 'pinvou.ci=apt137-release', 'PINVOU_TEST_RELEASE_DATABASE_URL', 'PINVOU_TEST_RELEASE_CLI_DIRECTORY', 'migrate token-governance token-policy-bootstrap', 'go build -tags=nomsgpack -trimpath', 'TestTokenReleasePreparationPostgresIntegration', 'TestTokenReleaseOperationsPostgresIntegration', '--- PASS:', "'--- SKIP:'", 'count(*) FROM pg_roles', 'count(*) FROM pg_database', 'trap', 'EXIT']) assert.ok(gate.run.includes(required), required);
  assert.ok(gate.run.indexOf('go build') < gate.run.indexOf('go test'));
  const cleanup = step('精确清理发布角色测试实例');
  assert.equal(cleanup.if, '${{ always() }}');
  for (const required of ['apt137-release.cid', '^[a-f0-9]{64}$', 'pinvou.ci', 'apt137-release']) assert.ok(cleanup.run.includes(required));
  assert.ok(!cleanup.run.includes('docker ps'));
});

test('browser and render gates call real engines and never claim fixture environment readiness', () => {
  assert.match(nginxBuild, /^FROM nginx:1\.18-alpine$/m);
  assert.ok(nginxBuild.includes(String.raw`sed -i 's|^http://dl-cdn\.alpinelinux\.org/|https://dl-cdn.alpinelinux.org/|' /etc/apk/repositories`));
  assert.ok(nginxBuild.includes('&& apk add --no-cache python3'));
  assert.doesNotMatch(nginxBuild, /allow-untrusted|no-check-certificate|--insecure|trusted\.alpine/);
  const browser = step('真实 Chromium 的只读 scope 与恶意响应门禁');
  assert.equal(browser['timeout-minutes'], 3);
  for (const required of ['test -x "$CHROME"', 'credential_origin_scope_probe.test.mjs', 'real Chromium scope gate', '# skipped 0', '# SKIP|^not ok']) assert.ok(browser.run.includes(required), required);
  const configuration = step('新治理 profile 的真实配置与镜像门禁').run;
  for (const required of ['model-token.env.example', 'compose.model-token.yaml', '--profile model-token', '--profile governance-operations', '--profile real-newapi', 'config --format json', '--compose-render', '--helm-render', 'values-staging.yaml values-production-new.yaml values-model-token-test.yaml', 'build token-credential-portal token-command-executor token-governance-check']) assert.ok(configuration.includes(required), required);
  const nginx = step('nginx 1.18 真实解析和隔离响应矩阵');
  assert.equal(nginx['timeout-minutes'], 5);
  for (const required of ['openssl req -x509', '--network none', '--read-only', '--cap-drop ALL', 'nginx -t', 'nginx-loopback-test.py', 'cloud_uat-loopback cloud_uat-encrypted_tunnel', 'owned_nginx_stopped', 'owned_mocks_stopped', 'tunnel_header_spoofed', '--exercise-cleanup', 'for fault in failure cancel', 'exercise_mode!==mode', 'exercise_reached!==true', 'sigterm_received!==(mode==="cancel")', 'trap']) assert.ok(nginx.run.includes(required), required);
  assert.equal(step('精确清理 nginx 测试实例').if, '${{ always() }}');
  assert.ok(nginx.run.includes('-parser-$profile'));
  for (const profile of ['cloud_uat-loopback', 'cloud_uat-encrypted_tunnel', 'cloud_uat-verified_tls', 'production-loopback', 'production-encrypted_tunnel', 'production-verified_tls']) assert.ok(step('精确清理 nginx 测试实例').run.includes(`parser-${profile}`));
  assert.doesNotMatch(nginx.run, /nginx\s+-s\s+reload|docker compose.*\bup\b|ssh\s/);
});

test('real fixture renderer emits six profiles only into an empty controlled directory', async t => {
  const root = await mkdtemp(path.join(tmpdir(), 'pinvou-ci-render-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const result = await renderNginxFixtures(root, '/fixture');
  assert.equal(result.files.length, 6); assert.equal(result.environment_status, 'ENV_PENDING');
  assert.equal(result.deployment_status, 'NOT_DEPLOYED');
  assert.equal((await readdir(root)).length, 7);
  for (const file of result.files) {
    const source = await readFile(path.join(root, file), 'utf8');
    assert.ok(source.includes('ssl_certificate /fixture/fixture.crt'));
    assert.ok(source.includes('client_body_temp_path /fixture/client_body-temp'));
    assert.ok(source.includes('proxy_temp_path /fixture/proxy-temp'));
    assert.ok(source.includes('location = /credential-handoff'));
    assert.ok(source.includes('proxy_hide_header Strict-Transport-Security'));
    assert.doesNotMatch(source, /pinvou-apt137-ingress-|C:\\Users|reload/);
  }
  await assert.rejects(renderNginxFixtures(root, '/fixture'), /DIRECTORY_NOT_EMPTY/);
  for (const invalid of ['relative', '/fixture;include', '/fixture/../site', '/fixture\nhttp', '/']) await assert.rejects(renderNginxFixtures(root, invalid), /PATH_INVALID/);
  assert.equal(await readFile(path.join(root, '.pinvou-nginx-fixture'), 'utf8'), 'PUBLIC_LOOPBACK_FIXTURE_ONLY\n');
  await writeFile(path.join(root, 'preserved.txt'), 'unchanged');
  await assert.rejects(renderNginxFixtures(root, '/fixture'), /DIRECTORY_NOT_EMPTY/);
  assert.equal(await readFile(path.join(root, 'preserved.txt'), 'utf8'), 'unchanged');
});

test('actual workflow cleanup verdict rejects pre-readiness failure and ignored signals', async t => {
  const command = step('nginx 1.18 真实解析和隔离响应矩阵').run.split('\n').find(line => line.includes('const mode=process.argv[2]'));
  const evaluator = command?.match(/node -e '([^']+)'/)[1];
  assert.ok(evaluator);
  const root = await mkdtemp(path.join(tmpdir(), 'pinvou-ci-cleanup-verdict-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const file = path.join(root, 'result.json');
  for (const mode of ['failure', 'cancel']) {
    const valid = { result: 'FAIL', owned_nginx_stopped: true, owned_mocks_stopped: true, exercise_mode: mode, exercise_reached: true, sigterm_received: mode === 'cancel' };
    for (const [patch, expected] of [[{}, 0], [{ exercise_reached: false }, 1], [{ exercise_mode: null }, 1], [{ exercise_mode: mode === 'failure' ? 'cancel' : 'failure' }, 1], [{ owned_nginx_stopped: false }, 1], [{ owned_mocks_stopped: false }, 1], [{ sigterm_received: mode !== 'cancel' }, 1], [{ result: 'PASS' }, 1]]) {
      await writeFile(file, JSON.stringify({ ...valid, ...patch }));
      // Execute the actual workflow verdict with real fs reads. No process startup
      // timing is part of this pure verdict contract; real SIGTERM has its own gate.
      let status = 0;
      const exited = new Error('controlled process.exit');
      try {
        vm.runInNewContext(evaluator, { require: createRequire(import.meta.url), process: { argv: [process.execPath, file, mode], exit(code) { status = code; throw exited; } } }, { timeout: 1000 });
      } catch (error) {
        if (error !== exited) throw error;
      }
      assert.equal(status, expected, `${mode}: ${JSON.stringify(patch)}`);
    }
  }
});
