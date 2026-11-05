import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const read = (path) => readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8');
const oldRef = '828242031c42911099792ec78c1f746ed8620c49';

function directives(text, section) {
  const result = new Map();
  let active = false;
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith('#') || line.startsWith(';')) continue;
    if (line.startsWith('[')) { active = line === `[${section}]`; continue; }
    if (!active) continue;
    const split = line.indexOf('=');
    assert.ok(split > 0, `invalid directive in ${section}`);
    const key = line.slice(0, split);
    assert.ok(!result.has(key), `duplicate ${key}`);
    result.set(key, line.slice(split + 1));
  }
  return Object.fromEntries(result);
}

test('expiry oneshot invokes the retained offline checker with its private state boundary', () => {
  const service = directives(read('account-platform/deploy/production/pinvou-release-expiry.service'), 'Service');
  assert.deepEqual(service, {
    Type: 'oneshot',
    ExecStart: '/usr/bin/python3 -I /opt/pinvou-release-expiry/account-platform-release-expiry.py --config /opt/pinvou-release-expiry/config/check.json --state /opt/pinvou-release-expiry/state/latest.json',
    UMask: '0077', LimitCORE: '0', TimeoutStartSec: '30', MemoryMax: '64M', TasksMax: '8',
    NoNewPrivileges: 'true', PrivateTmp: 'true', PrivateNetwork: 'true', ProtectHome: 'true',
    ProtectSystem: 'strict', ReadWritePaths: '/opt/pinvou-release-expiry/state',
    InaccessiblePaths: '-/var/run/docker.sock -/run/docker.sock', RestrictAddressFamilies: 'AF_UNIX',
  });
  // The actual checker consumes the flags used by this unit, with no network/DB entry.
  const checker = read('scripts/account-platform-release-expiry.py');
  assert.match(checker, /parser\.add_argument\('--config', type=Path, required=True\)/);
  assert.match(checker, /parser\.add_argument\('--state', type=Path, required=True\)/);
  assert.match(checker, /'evidence': 'RECORDED_EXPIRY_ONLY', 'live_database_checked': False/);
  assert.match(checker, /stat\.S_IMODE\(info\.st_mode\) == \(0o700 if directory else 0o600\)/);
  assert.doesNotMatch(checker, /(?:import|from)\s+(?:socket|requests|urllib|psycopg|subprocess)\b/);
});

test('expiry timer retains original daily schedule without adding repair or retry policy', () => {
  const timer = read('account-platform/deploy/production/pinvou-release-expiry.timer');
  assert.deepEqual(directives(timer, 'Timer'), {
    OnCalendar: '*-*-* 08:00:00', Persistent: 'true', RandomizedDelaySec: '5min', Unit: 'pinvou-release-expiry.service',
  });
  assert.deepEqual(directives(timer, 'Install'), { WantedBy: 'timers.target' });
});

test('all five dashboard queries bind current HTTP and relay metric families', () => {
  const dashboard = JSON.parse(read('account-platform/deploy/dashboards/account-platform-overview.json'));
  assert.equal(dashboard.uid, 'pinvou-account-platform');
  assert.equal(dashboard.panels.length, 5);
  assert.deepEqual(dashboard.panels.map((panel) => panel.id), [1, 2, 3, 4, 5]);
  assert.equal(dashboard.templating.list[0].query, 'label_values(pinvou_http_requests_total, service)');
  assert.deepEqual(dashboard.panels.map((panel) => panel.targets.map((target) => target.expr)), [
    ['sum by(service) (rate(pinvou_http_requests_total{service=~"$service"}[5m]))'],
    ['histogram_quantile(0.95, sum by(service,le) (rate(pinvou_http_request_duration_seconds_bucket{service=~"$service"}[5m])))'],
    ['pinvou_outbox_pending_events'], ['pinvou_outbox_oldest_pending_age_seconds'], ['sum(pinvou_outbox_dead_lettered_events)'],
  ]);
  const http = read('account-platform/internal/platform/httpserver/metrics.go');
  for (const family of ['pinvou_http_requests_total', 'pinvou_http_request_duration_seconds']) {
    assert.ok(http.includes(`Name: "${family}"`), `current HTTP emitter missing ${family}`);
  }
  assert.match(http, /"service": service/);
  const relay = read('account-platform/cmd/event-relay/metrics.go');
  for (const family of ['pinvou_outbox_pending_events', 'pinvou_outbox_oldest_pending_age_seconds', 'pinvou_outbox_dead_lettered_events']) {
    assert.ok(relay.includes(`Name: "${family}"`), `current relay emitter missing ${family}`);
  }
  assert.match(relay, /"source": source/);
});

test('twelve broken references become explicit immutable historical sources', () => {
  const files = [
    ['account-platform/docs/implementation-status.md', 2],
    ['account-platform/docs/implementation-tasks.md', 1],
    ['account-platform/docs/ledger-repair-design.md', 2],
    ['account-platform/docs/payment-financial-repair-design.md', 2],
    ['account-platform/docs/receivable-write-off-design.md', 2],
    ['account-platform/docs/refund-exception-repair-design.md', 2],
    ['account-platform/docs/requirements.md', 1],
  ];
  const targets = new Set([
    'account-platform/docs/admin-governance-security-tasks.md',
    'account-platform/docs/admin-governance-security-command-baseline.md',
    'account-platform/docs/admin-governance-security-redesign.md',
    'account-platform/deploy/runbooks/model-token-dns.md',
  ]);
  let total = 0;
  for (const [path, expected] of files) {
    const text = read(path);
    const restored = [...text.matchAll(/\[([^\]\n]+（历史原文）)\]\((https:\/\/github\.com\/Pinvou\/pinvou3\/blob\/([a-f0-9]{40})\/([^\s)]+))\)/g)]
      .filter((match) => targets.has(match[4]));
    assert.equal(restored.length, expected, `historical reference count in ${path}`);
    for (const match of restored) assert.equal(match[3], oldRef);
    for (const target of targets) {
      assert.ok(!text.includes(`](${target.split('/').at(-1)})`), `relative missing target in ${path}`);
      assert.ok(!text.includes(`](../deploy/runbooks/${target.split('/').at(-1)})`), `relative missing runbook in ${path}`);
    }
    total += restored.length;
  }
  assert.equal(total, 12);
});

test('operator documentation distinguishes offline evidence and current lineage policy', () => {
  const doc = read('account-platform/deploy/runbooks/offline-expiry-and-dashboard.md');
  for (const boundary of ['RECORDED_EXPIRY_ONLY', 'live_database_checked=false', '不安装、启用或部署服务', 'Session-lineage 版本与回滚', '不改变会话运行语义']) {
    assert.ok(doc.includes(boundary), `missing boundary ${boundary}`);
  }
  assert.ok(doc.includes('没有自动导入或修改已有数据源'));
});

test('deployment inputs no longer advertise a switch for always-enabled lineage', () => {
  for (const path of [
    'account-platform/deploy/compose/compose.yaml',
    'account-platform/deploy/helm/account-platform/templates/app-deployments.yaml',
    'account-platform/deploy/helm/account-platform/values.yaml',
    'account-platform/deploy/helm/account-platform/values.schema.json',
  ]) {
    assert.doesNotMatch(read(path), /PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED|sessionLineageLogoutEnabled|Phase 2 only|keep false during the capable-binary rollout/, path);
  }
  const portal = JSON.parse(read('account-platform/deploy/helm/account-platform/values.schema.json')).properties.portal;
  assert.equal(portal.additionalProperties, false, 'stale per-release values must fail schema validation');
  assert.ok(!portal.required.includes('sessionLineageLogoutEnabled'));
  assert.ok(!Object.hasOwn(portal.properties, 'sessionLineageLogoutEnabled'));
  assert.ok(portal.required.includes('adminModelTokenIAEnabled'), 'neighbor gates are retained');
  const ajv = new Ajv({ strict: true });
  addFormats(ajv);
  const validate = ajv.compile(JSON.parse(read('account-platform/deploy/helm/account-platform/values.schema.json')));
  const defaults = YAML.parse(read('account-platform/deploy/helm/account-platform/values.yaml'));
  assert.equal(validate(defaults), true, JSON.stringify(validate.errors));
  for (const staleValue of [false, true, 'false']) {
    const stale = structuredClone(defaults);
    stale.portal.sessionLineageLogoutEnabled = staleValue;
    assert.equal(validate(stale), false, 'obsolete release overrides must be removed explicitly');
    assert.ok(validate.errors.some((error) => error.keyword === 'additionalProperties'
      && error.params.additionalProperty === 'sessionLineageLogoutEnabled'));
  }
  const compose = YAML.parse(read('account-platform/deploy/compose/compose.yaml'), { merge: true });
  assert.ok(compose.services['token-portal'].environment);
  assert.ok(!Object.hasOwn(compose.services['token-portal'].environment, 'PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED'));
  const service = read('account-platform/internal/portal/service.go');
  assert.match(service, /func \(s \*Service\) sessionLineageStatus[^]*?"schema_version": 1, "capable": true, "enabled": true/);
  assert.match(service, /s\.store\.CommitLoginSession\(r\.Context\(\), flow\.PreviousSession, session\)/);
  assert.match(service, /s\.store\.LogoutSession\(r\.Context\(\), sessionID, lineageRoot, s\.logoutMarkerExpiry\(session\)\)/);
  assert.doesNotMatch(read('account-platform/cmd/token-portal/main.go'), /SESSION_LINEAGE_LOGOUT/);
});

test('rollback guide requires current protection and store compatibility instead of flag or elapsed time', () => {
  const doc = read('account-platform/deploy/runbooks/session-lineage-version-rollback.md');
  for (const boundary of ['同等保护且兼容当前存储的版本', '不能退回只删除单一 session', 'portal:lineage-members:*', 'portal:logout:*', 'portal:refresh:*', 'PreviousSession', '严格 schema 会拒绝', '没有可证明同等保护的目标版本时停止', '不新增替代等待窗口', '--reuse-values', '只改仓库默认值不会清除']) {
    assert.ok(doc.includes(boundary), `missing rollback boundary ${boundary}`);
  }
  const store = read('account-platform/internal/portal/store.go');
  for (const primitive of ['CommitLoginSession', 'LogoutSession', 'ReplaceSessionFromHandoff', 'SaveRefreshedSession']) {
    assert.ok(store.includes(`func (s *RedisStore) ${primitive}(`));
  }
  assert.ok(read('account-platform/docs/production-security-infrastructure.md').includes('../deploy/runbooks/session-lineage-version-rollback.md'));
});

test('original Docker context exclusions retain Go COPY and do not govern the separate Keycloak context', () => {
  assert.deepEqual(read('account-platform/.dockerignore').trim().split(/\r?\n/), ['.git', 'deploy', 'test', 'web', '**/*_test.go']);
  const dockerfile = read('account-platform/Dockerfile');
  const localCopySources = [...dockerfile.matchAll(/^COPY (?!.*--from=)(.+)$/gm)]
    .flatMap((match) => match[1].trim().split(/\s+/).slice(0, -1));
  assert.deepEqual(localCopySources, ['go.mod', 'go.sum', 'cmd', 'internal', 'migrations']);
  assert.doesNotMatch(dockerfile, /^COPY (?:deploy|test|web)(?:\s|\/)/m);
  const compose = YAML.parse(read('account-platform/deploy/compose/compose.yaml'), { merge: true });
  assert.deepEqual(compose['x-service-build'], { context: '../..', dockerfile: 'Dockerfile' });
  assert.equal(compose.services.keycloak.build.context, '../keycloak');
  const keycloak = read('account-platform/deploy/keycloak/Dockerfile');
  assert.match(keycloak, /COPY mobile-authenticator\/pom[.]xml/);
  assert.match(keycloak, /COPY themes\/pinvou/);
  // These are separate tool/test Dockerfiles. Neither consumes an application
  // root COPY source that would now be filtered; this does not execute Docker.
  for (const path of ['account-platform/deploy/production/Dockerfile.postgres-backup', 'account-platform/test/deployment/nginx.Dockerfile']) {
    assert.doesNotMatch(read(path), /^COPY /m);
  }
  const embedded = [
    ['account-platform/internal/portal/static.go', 'static/*'],
    ['account-platform/internal/portal/credential_handoff.go', 'credential-handoff.v1.js'],
    ['account-platform/internal/billingschema/legacy_entitlement_admission.go', 'retained/000016_entitlement_components.up.sql'],
    ['account-platform/internal/billingschema/usage_waiver_history.go', 'retained/000026_hold_operations_evidence_oldfix.up.sql'],
    ['account-platform/internal/billingschema/wallet_receipts.go', 'retained/000053_token_account_wallet_provisioning_receipts.up.sql'],
  ];
  for (const [path, pattern] of embedded) {
    assert.ok(read(path).includes(`//go:embed ${pattern}`));
    assert.ok(!/^(deploy|test|web)(\/|$)/.test(pattern) && !pattern.endsWith('_test.go'));
  }
});
