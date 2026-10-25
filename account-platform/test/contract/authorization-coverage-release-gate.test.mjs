import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { authorizationRoutes, goSource, runAuthorizationCoverage } from '../../../scripts/check-account-platform-authorization-coverage.mjs';

const catalogURL = new URL('../../deploy/authorization-entrypoints.json', import.meta.url);

async function rejectedMutation(mutate, expected) {
  const directory = await mkdtemp(path.join(tmpdir(), 'pinvou-authorization-coverage-'));
  try {
    const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
    mutate(catalog);
    const target = path.join(directory, 'authorization-entrypoints.json');
    await writeFile(target, `${JSON.stringify(catalog, null, 2)}\n`);
    await assert.rejects(runAuthorizationCoverage(target), expected);
  } finally {
    const owned = path.resolve(directory);
    assert.equal(path.dirname(owned), path.resolve(tmpdir()));
    assert.ok(path.basename(owned).startsWith('pinvou-authorization-coverage-'));
    await rm(owned, { recursive: true, force: true });
  }
}

test('authorization release gate classifies every HTTP task event callback and CLI entrypoint', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  // The six existing Portal transports use Any, alongside the 392 verb-specific
  // registrations. Keep the expected complete graph independent of its parser.
  const wildcardPaths = [
    '/api/account/*resource', '/api/token/*resource',
    '/api/admin/account/*resource', '/api/admin/billing/*resource',
    '/api/admin/token/*resource', '/api/admin/approvals/requests/*resource',
  ];
  const wildcardSource = 'account-platform/internal/portal/service.go';
  assert.deepEqual(catalog.http_policies.filter(item => item.method === 'ANY').map(({source, receiver, method, path}) => ({source, receiver, method, path})),
    wildcardPaths.map(path => ({source: wildcardSource, receiver: 'router', method: 'ANY', path})));
  const portal = await readFile(new URL('../../internal/portal/service.go', import.meta.url), 'utf8');
  for (const route of wildcardPaths) assert.ok(portal.includes(`router.Any("${route}",`), `missing existing Portal transport ${route}`);
  // The real checker still rejects missing, stale, duplicate or mis-scoped
  // entries before returning these exact inventory sizes.
  assert.deepEqual(await runAuthorizationCoverage(), { routes: 398, tasks: 32, events: 18, cli: 26 });
});

test('password preparation adds only the captcha admission and two exact internal authority routes', async () => {
  const account = await readFile(new URL('../../internal/account/service.go', import.meta.url), 'utf8');
  const identity = await readFile(new URL('../../internal/identityadapter/service.go', import.meta.url), 'utf8');
  const expected = [
    [account, '/v1/auth/password-preparation-authorizations', 'router.POST("/v1/auth/password-preparation-authorizations", httpserver.AdaptFunc(s.createPasswordPreparation))'],
    [account, '/auth/password-preparation-authorizations/claim', 'internal.POST("/auth/password-preparation-authorizations/claim", serviceauth.Require("internal.auth.password-preparation.claim"), httpserver.AdaptFunc(s.claimPasswordPreparation))'],
    [identity, '/registration/password-references/preparation', 'internal.POST("/registration/password-references/preparation", serviceauth.Require("internal.identity.password-reference.validate"), httpserver.AdaptFunc(s.passwordReferencePreparation))'],
  ];
  for (const [source, route, registration] of expected) {
    assert.equal(source.split(`POST("${route}"`).length - 1, 1);
    assert.ok(source.includes(registration));
  }
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.http_policies.filter(item => item.path === '/v1/auth/password-preparation-authorizations').map(item => item.strategy), ['protocol.registration.captcha-password-preparation']);
});


test('platform role management retains four exact human permission and AuditOnly bindings', async () => {
  const source = 'account-platform/internal/account/platform_roles_http.go';
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.equal(catalog.http_sources.filter(item => item === source).length, 1);
  assert.equal(catalog.http_policies.some(item => item.source === source), false);
  const text = await readFile(new URL(`../../../${source}`, import.meta.url), 'utf8');
  const routes = authorizationRoutes(source, text);
  const expected = [
    ['GET', '/roles', 'PermissionPlatformRolesRead', 'adminPlatformRoles'],
    ['GET', '/role-bindings', 'PermissionPlatformRolesRead', 'adminPlatformRoleBindings'],
    ['POST', '/role-bindings', 'PermissionPlatformRolesAssign', 'adminGrantPlatformRoleBinding', 'ACCOUNT_ROLE_BINDING_GRANT'],
    ['DELETE', '/role-bindings/:binding_id', 'PermissionPlatformRolesAssign', 'adminRevokePlatformRoleBinding', 'ACCOUNT_ROLE_BINDING_REVOKE'],
  ];
  assert.equal(routes.length, expected.length);
  for (const [method, routePath, permission, handler, policy] of expected) {
    const matches = routes.filter(item => item.method === method && item.path === routePath);
    assert.equal(matches.length, 1, `${method} ${routePath}`);
    assert.equal(matches[0].receiver, 'admin');
    const approval = policy ? `approvalpolicy.Require("${policy}", approvalpolicy.AuditOnly), ` : '';
    assert.equal(matches[0].registration.trim(), `admin.${method}("${routePath}", s.requirePermission(${permission}), ${approval}httpserver.AdaptFunc(s.${handler}))`);
  }
  const service = await readFile(new URL('../../internal/account/service.go', import.meta.url), 'utf8');
  const {code, masked} = goSource(service);
  const groups = [...code.matchAll(/\badmin\s*:=\s*protected\.Group\("([^"]+)"\)/g)]
    .filter(match => masked[match.index] !== ' ');
  assert.deepEqual(groups.map(match => match[1]), ['/admin/v1']);
  const registrations = [...code.matchAll(/\bs\.registerPlatformRoleRoutes\(admin\)/g)]
    .filter(match => masked[match.index] !== ' ');
  assert.equal(registrations.length, 1);
  assert.ok(registrations[0].index > groups[0].index);
  const definitions = await readFile(new URL('../../internal/account/platform_roles.go', import.meta.url), 'utf8');
  assert.match(definitions, /\bPermissionPlatformRolesRead\s*=\s*"platform\.roles\.read"/);
  assert.match(definitions, /\bPermissionPlatformRolesAssign\s*=\s*"platform\.roles\.assign"/);
  await rejectedMutation(catalog => {
    catalog.http_sources = catalog.http_sources.filter(item => item !== source);
  }, /unregistered_http_source:account-platform\/internal\/account\/platform_roles_http\.go/);
});

test('usage waiver restores one human audit-only command and two distinct workload routes', async () => {
  const bindings = [
    {source: 'account-platform/internal/billing/service.go', receiver: 'admin', path: '/usage-holds/:hold_id/release',
      registration: 'admin.POST("/usage-holds/:hold_id/release", s.authorizer.Require("platform.billing.holds.release"), approvalpolicy.Require("BILLING_USAGE_HOLD_SAFE_RELEASE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUsageWaiver))'},
    {source: 'account-platform/internal/billing/service.go', receiver: 'protected', path: '/internal/v1/holds/waiver',
      registration: 'protected.POST("/internal/v1/holds/waiver", serviceauth.Require("internal.billing.hold.waive"), httpserver.AdaptFunc(s.executeUsageWaiver))'},
    {source: 'account-platform/internal/token/management_routes.go', receiver: 'internal', path: '/usage-waivers',
      registration: 'internal.POST("/usage-waivers", serviceauth.Require("internal.token.usage.waive"), httpserver.AdaptFunc(s.internalUsageWaiver))'},
  ];
  for (const entry of bindings) {
    const text = await readFile(new URL(`../../../${entry.source}`, import.meta.url), 'utf8');
    const routes = authorizationRoutes(entry.source, text).filter(route => route.path === entry.path);
    assert.equal(routes.length, 1);
    assert.equal(routes[0].receiver, entry.receiver);
    assert.equal(routes[0].method, 'POST');
    assert.equal(routes[0].registration.trim(), entry.registration);
    if (entry.receiver !== 'protected') {
      const {code, masked} = goSource(text);
      const groups = [...code.matchAll(new RegExp(`\\b${entry.receiver}\\s*(?::=|=)\\s*router\\.Group\\("([^"]+)"\\)`, 'g'))]
        .filter(match => masked[match.index] !== ' ');
      assert.deepEqual(groups.map(match => match[1]), [entry.receiver === 'admin' ? '/admin/v1' : '/internal/v1']);
    }
  }
});

test('history recovery routes retain exact workload permissions and handlers without human fallback', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  const expected = [
    {source: 'account-platform/internal/identityadapter/service.go', receiver: 'internal', method: 'POST',
      path: '/users/:subject/profile-revocations', permission: 'internal.identity.sessions.revoke', handler: 'revokeProfileSessions'},
    {source: 'account-platform/internal/billing/service.go', receiver: 'protected', method: 'POST',
      path: '/internal/v1/token-account-wallet-provisioning-receipts', permission: 'internal.billing.token-account-wallet-provision.execute', handler: 'provisionTokenAccountWallet'},
    {source: 'account-platform/internal/billing/service.go', receiver: 'protected', method: 'GET',
      path: '/internal/v1/token-account-wallet-provisioning-receipts/:command_reference', permission: 'internal.billing.token-account-wallet-provision.read', handler: 'getTokenAccountWalletProvisioningReceipt'},
  ];
  for (const entry of expected) {
    assert.ok(catalog.http_sources.includes(entry.source));
    const text = await readFile(new URL(`../../../${entry.source}`, import.meta.url), 'utf8');
    const routes = authorizationRoutes(entry.source, text).filter(route => route.path === entry.path);
    assert.equal(routes.length, 1, `missing or duplicate recovery route ${entry.path}`);
    assert.equal(routes[0].receiver, entry.receiver);
    assert.equal(routes[0].method, entry.method);
    assert.equal(routes[0].registration.match(/serviceauth\.Require\("([^"]+)"\)/)?.[1], entry.permission);
    assert.equal(routes[0].registration.trim(), `${entry.receiver}.${entry.method}("${entry.path}", serviceauth.Require("${entry.permission}"), httpserver.AdaptFunc(s.${entry.handler}))`);
    assert.equal(catalog.http_policies.some(route => route.source === entry.source && route.path === entry.path), false);
    if (entry.receiver === 'internal') {
      const {code, masked} = goSource(text);
      const groups = [...code.matchAll(/\binternal\s*(?::=|=)\s*router\.Group\("([^"]+)"\)/g)]
        .filter(match => masked[match.index] !== ' ');
      assert.deepEqual(groups.map(match => match[1]), ['/internal/v1']);
    }
  }
});

test('original refund query has one read-only workload route bound to both original resource keys', async () => {
  const source = 'account-platform/internal/paymentsim/service.go';
  const text = await readFile(new URL('../../internal/paymentsim/service.go', import.meta.url), 'utf8');
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.ok(catalog.http_sources.includes(source));
  const routes = authorizationRoutes(source, text).filter(route => route.path === '/v1/orders/:merchant_order_no/refunds/:merchant_refund_no');
  assert.equal(routes.length, 1);
  assert.equal(routes[0].receiver, 'management');
  assert.equal(routes[0].method, 'GET');
  assert.match(routes[0].registration, /serviceauth\.Require\("internal\.payment-simulator\.refund\.read"\).*s\.getRefund/);
  assert.match(text, /QueryRefund\(r\.Context\(\), r\.PathValue\("merchant_order_no"\), r\.PathValue\("merchant_refund_no"\)\)/);
  // Workload routes are classified from their exact Require declarations by
  // the real coverage scanner, so there is no public/human policy fallback.
  assert.equal(catalog.http_policies.some(route => route.source === source && route.path === routes[0].path), false);
});

test('Token proof endpoint requires a dedicated Account-only permission', async () => {
  const source = await readFile(new URL('../../internal/token/management_routes.go', import.meta.url), 'utf8');
  assert.match(source, /internal\.POST\("\/approval-command-proofs", serviceauth\.Require\("internal\.token\.approval-proof\.read"\)/);
});

test('installed approval recovery has an exact bounded workload entry', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.cli.filter(item => item.name === 'account-approval-recovery'), [{
    name: 'account-approval-recovery', source: 'account-platform/cmd/account-approval-recovery/main.go',
    strategy: 'workload.dedicated-account-approval-recovery-role',
  }]);
  const runtime = await readFile(new URL('../../cmd/account-approval-recovery/runtime.go', import.meta.url), 'utf8');
  assert.match(runtime, /n < 1 \|\| n > 200/);
  assert.match(runtime, /verifyRecoveryActor\(ctx, pool\)/);
  assert.match(runtime, /account\.NewApprovalRecoveryRuntime/);
  await rejectedMutation(catalog => {
    catalog.cli = catalog.cli.filter(item => item.name !== 'account-approval-recovery');
  }, /unclassified_cli:account-approval-recovery/);
});

test('Billing approval activation projection requires its exact workload permission', async () => {
  const source = await readFile(new URL('../../internal/billing/service.go', import.meta.url), 'utf8');
  assert.match(source, /protected\.GET\("\/internal\/v1\/approval-command-activations", serviceauth\.Require\("internal\.billing\.approval-activation\.read"\)/);
  assert.match(source, /protected\.POST\("\/internal\/v1\/approval-command-proofs", serviceauth\.Require\("internal\.billing\.approval-proof\.read"\)/);
});

test('installed approval reconciliation retains its exact bounded effect-verified workload entry', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.cli.filter(item => item.name === 'approval-reconciliation'), [{
    name: 'approval-reconciliation', source: 'account-platform/cmd/approval-reconciliation/main.go',
    strategy: 'workload.bounded-effect-verified-approval-reconciliation',
  }]);
  await rejectedMutation(catalog => {
    catalog.cli = catalog.cli.filter(item => item.name !== 'approval-reconciliation');
  }, /unclassified_cli:approval-reconciliation/);
});

test('fixed NATS resource operator has an exact required authorization entry', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.cli.filter(item => item.name === 'event-resources'), [{
    name: 'event-resources', source: 'account-platform/cmd/event-resources/main.go',
    strategy: 'protocol.offline-fixed-nats-authorization-or-workload-fixed-resources',
  }]);
  await rejectedMutation(catalog => {
    catalog.cli = catalog.cli.filter(item => item.name !== 'event-resources');
  }, /unclassified_cli:event-resources/);
});

test('single-client workload token projector has an exact required authorization entry', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.cli.filter(item => item.name === 'workload-token-agent'), [{
    name: 'workload-token-agent', source: 'account-platform/cmd/workload-token-agent/main.go',
    strategy: 'workload.single-client-verified-token-projection',
  }]);
  await rejectedMutation(catalog => {
    catalog.cli = catalog.cli.filter(item => item.name !== 'workload-token-agent');
  }, /unclassified_cli:workload-token-agent/);
});

test('authorization release gate fails closed for removed unknown and scope-mismatched policies', async () => {
  await rejectedMutation((catalog) => catalog.http_policies.shift(), /unclassified_http_entrypoint/);
  await rejectedMutation((catalog) => {
    const policy = catalog.http_policies.find((item) => item.scope === 'organization');
    policy.permissions = ['org.unknown.injected'];
  }, /unknown_human_permission/);
  await rejectedMutation((catalog) => {
    const policy = catalog.http_policies.find((item) => item.scope === 'organization');
    policy.scope = 'platform';
  }, /permission_scope_mismatch/);
});

test('authorization release gate rejects unregistered tasks events and command surfaces', async () => {
  await rejectedMutation((catalog) => catalog.tasks.pop(), /unclassified_task/);
  await rejectedMutation((catalog) => catalog.events.pop(), /unclassified_event/);
  await rejectedMutation((catalog) => catalog.cli.pop(), /unclassified_cli/);
});

test('authorization gate discovers all three unresolved Token approval subscriptions', async () => {
  for (const durable of ['token-model-lifecycle-executor-v1', 'token-catalog-rollback-executor-v1', 'token-credential-revoke-executor-v1']) {
    await rejectedMutation((catalog) => {
      catalog.events = catalog.events.filter((entry) => entry.durable !== durable);
    }, new RegExp(`unclassified_event:.*${durable}`));
  }
});

test('late review query keeps exactly two sync-authorized audit-only POST registrations', async () => {
  const source = 'account-platform/internal/billing/service.go';
  const text = await readFile(new URL(`../../../${source}`, import.meta.url), 'utf8');
  const expected = [
    ['/late-payment-reviews/:case_id/query-commands', 'adminLatePaymentQueryCommand'],
    ['/late-payment-reviews/:case_id/query-command-results', 'adminLatePaymentQueryResult'],
  ];
  const routes = authorizationRoutes(source, text).filter(item => expected.some(([route]) => route === item.path));
  assert.equal(routes.length, expected.length);
  for (const [route, handler] of expected) {
    const matches = routes.filter(item => item.path === route);
    assert.equal(matches.length, 1);
    assert.equal(matches[0].receiver, 'admin');
    assert.equal(matches[0].method, 'POST');
    assert.equal(matches[0].registration.trim(), `admin.POST("${route}", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_LATE_PAYMENT_QUERY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.${handler}))`);
  }
});

test('withdrawal result exposes exactly two platform original-receipt GET bindings', async () => {
  const source = 'account-platform/internal/account/service.go';
  const routePath = '/approval-requests/:approval_request_id/actions/withdraw-result';
  const text = await readFile(new URL(`../../../${source}`, import.meta.url), 'utf8');
  const routes = authorizationRoutes(source, text).filter(item => item.path === routePath);
  assert.deepEqual(routes.map(({ receiver, method, path: route }) => ({ receiver, method, path: route })),
    ['admin', 'adminV2'].map(receiver => ({ receiver, method: 'GET', path: routePath })));
  for (const item of routes) {
    assert.equal(item.registration.trim(), `${item.receiver}.GET("${routePath}", httpserver.AdaptFunc(s.adminApprovalWithdrawalResult))`);
  }
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  const bindings = catalog.http_policies.filter(item => item.source === source && item.path === routePath);
  assert.deepEqual(bindings, ['admin', 'adminV2'].map(receiver => ({ source, receiver, method: 'GET', path: routePath,
    scope: 'platform', strategy: 'domain.approval-policy-withdrawal-original-receipt' })));
  await rejectedMutation(value => {
    value.http_policies = value.http_policies.filter(item => !(item.source === source && item.receiver === 'adminV2' && item.path === routePath));
  }, /unclassified_http_entrypoint/);
});

test('decision receipt adds exactly two versioned GETs with the existing current-authority domain reader', async () => {
  const source = 'account-platform/internal/account/service.go';
  const text = await readFile(new URL('../../internal/account/service.go', import.meta.url), 'utf8');
  const routePath = '/approval-requests/:approval_request_id/actions/decision-result';
  const expected = ['admin', 'adminV2'];
  const routes = authorizationRoutes(source, text).filter(item => item.path === routePath);
  assert.equal(routes.length, 2);
  for (const receiver of expected) {
    assert.equal(routes.filter(item => item.receiver === receiver && item.method === 'GET' && item.registration.trim() === `${receiver}.GET("${routePath}", httpserver.AdaptFunc(s.adminApprovalDecisionResult))`).length, 1);
  }
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  const policies = catalog.http_policies.filter(item => item.path === routePath);
  assert.deepEqual(policies.map(item => [item.receiver, item.method, item.scope, item.strategy]), expected.map(receiver => [receiver, 'GET', 'platform', 'domain.approval-policy-decision-original-receipt']));
  const reader = await readFile(new URL('../../internal/account/approval_decision_receipts.go', import.meta.url), 'utf8');
  for (const bound of ['humanApprovalPrincipal(principal)', 'approvalRequestVisibilityForActor', 'requireApprovalDraftCloseControl', 'item.RequesterUserID == *actor', 'policy.ApproverPermission', 'readApprovalDecisionReceipt']) assert.ok(reader.includes(bound), `missing existing decision receipt boundary ${bound}`);
  await rejectedMutation(catalog => { catalog.http_policies = catalog.http_policies.filter(item => !(item.path === routePath && item.receiver === 'adminV2')); }, /unclassified_http_entrypoint:account-platform\/internal\/account\/service\.go\|adminV2\|GET\|\/approval-requests\/:approval_request_id\/actions\/decision-result/);
});
