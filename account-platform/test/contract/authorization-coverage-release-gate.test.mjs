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
  // The six existing Portal transports use Any, alongside the 396 verb-specific
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
  assert.deepEqual(await runAuthorizationCoverage(), { routes: 410, tasks: 33, events: 18, cli: 27 });
});

function assertLedgerRepairProofAuthorization(service, handler) {
  const source = 'account-platform/internal/account/service.go';
  const routePath = '/ledger-repair/authorization-proofs:batch';
  const routes = authorizationRoutes(source, service).filter(route => route.path === routePath);
  assert.equal(routes.length, 1, 'the ledger repair proof route must exist exactly once');
  assert.equal(routes[0].receiver, 'internal');
  assert.equal(routes[0].method, 'POST');
  assert.equal(routes[0].registration.trim(), 'internal.POST("/ledger-repair/authorization-proofs:batch", httpserver.RequirePathSuffix("/authorization-proofs:batch"), serviceauth.Require("internal.account.ledger-repair-proof.read"), httpserver.AdaptFunc(s.ledgerRepairAuthorizationProofBatch))');
  const {code, masked} = goSource(service);
  const groups = [...code.matchAll(/\binternal\s*(?::=|=)\s*router\.Group\("([^"]+)"\)/g)]
    .filter(match => masked[match.index] !== ' ');
  assert.deepEqual(groups.map(match => match[1]), ['/internal/v1']);
  assert.match(handler, /caller, ok := serviceauth\.FromContext\(r\.Context\(\)\)/);
  assert.match(handler, /if !ok \|\| caller\.ActorType != "SERVICE" \|\| caller\.Service != "billing-service"/);
  // Gin treats the colon in :batch as a parameter; the handler must constrain
  // the resolved path and reject alternate query-bearing URLs as well.
  assert.match(handler, /if r\.URL\.Path != "\/internal\/v1\/ledger-repair\/authorization-proofs:batch" \|\| r\.URL\.RawQuery != "" \|\| r\.URL\.ForceQuery/);
}

test('ledger repair history proof is one exact Billing-only workload route', async () => {
  const source = 'account-platform/internal/account/service.go';
  const service = await readFile(new URL('../../internal/account/service.go', import.meta.url), 'utf8');
  const handler = await readFile(new URL('../../internal/account/approval_ledger_repair_proofs.go', import.meta.url), 'utf8');
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.ok(catalog.http_sources.includes(source));
  assert.equal(catalog.http_policies.some(route => route.source === source && route.path === '/ledger-repair/authorization-proofs:batch'), false);
  assertLedgerRepairProofAuthorization(service, handler);
  const registration = 'internal.POST("/ledger-repair/authorization-proofs:batch", httpserver.RequirePathSuffix("/authorization-proofs:batch"), serviceauth.Require("internal.account.ledger-repair-proof.read"), httpserver.AdaptFunc(s.ledgerRepairAuthorizationProofBatch))';
  for (const mutation of [
    service.replace(registration, ''),
    service.replace(registration, registration.replace('httpserver.RequirePathSuffix("/authorization-proofs:batch"), ', '')),
    service.replace(registration, registration.replace('"/authorization-proofs:batch")', '"/authorization-proofs:batchOther")')),
    service.replace(registration, registration.replace('httpserver.RequirePathSuffix("/authorization-proofs:batch"), ', 'httpserver.RequirePathSuffix("/authorization-proofs:batch"), httpserver.RequirePathSuffix("/authorization-proofs:batch"), ')),
    service.replace(registration, registration.replace('httpserver.RequirePathSuffix("/authorization-proofs:batch")', 'middleware.Require("path")')),
    service.replace(registration, registration.replace('internal.POST', 'internal.GET')),
    service.replace(registration, registration.replace('internal.account.ledger-repair-proof.read', 'internal.account.other.read')),
    service.replace(registration, registration.replace('s.ledgerRepairAuthorizationProofBatch', 's.otherHandler')),
    service.replace('internal := router.Group("/internal/v1")', 'internal := router.Group("/other")'),
  ]) {
    assert.notEqual(mutation, service, 'each negative mutation must actually change the source');
    assert.throws(() => assertLedgerRepairProofAuthorization(mutation, handler));
  }
  for (const mutation of [
    handler.replace('caller.ActorType != "SERVICE"', 'caller.ActorType != "USER"'),
    handler.replace('caller.Service != "billing-service"', 'caller.Service != "other-service"'),
    handler.replace('r.URL.Path != "/internal/v1/ledger-repair/authorization-proofs:batch"', 'r.URL.Path != "/internal/v1/ledger-repair/other"'),
  ]) {
    assert.notEqual(mutation, handler, 'each negative mutation must actually change the handler');
    assert.throws(() => assertLedgerRepairProofAuthorization(service, mutation));
  }
  await rejectedMutation(catalog => {
    catalog.http_sources = catalog.http_sources.filter(item => item !== source);
  }, /unregistered_http_source:account-platform\/internal\/account\/service\.go/);
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

test('evidence validator has an exact required authorization entry', async () => {
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assert.deepEqual(catalog.cli.filter(item => item.name === 'evidence-image-validator'), [{
    name: 'evidence-image-validator', source: 'account-platform/cmd/evidence-image-validator/main.go',
    strategy: 'workload.default-disabled-unix-peer-uid-verified-evidence-validator',
  }]);
  await rejectedMutation(catalog => {
    catalog.cli = catalog.cli.filter(item => item.name !== 'evidence-image-validator');
  }, /unclassified_cli:evidence-image-validator/);
  await rejectedMutation(catalog => {
    catalog.cli.find(item => item.name === 'evidence-image-validator').source = 'account-platform/cmd/healthcheck/main.go';
  }, /cli_source_mismatch:evidence-image-validator/);
  await rejectedMutation(catalog => {
    catalog.cli.find(item => item.name === 'evidence-image-validator').strategy = '';
  }, /unstable_cli_strategy:evidence-image-validator/);
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
const restoredReadEntries = [
  { source: 'account-platform/internal/account/service.go', receiver: 'admin', method: 'GET', path: '/overview',
    registration: 'admin.GET("/overview", s.requireAnyPermission(PermissionPlatformUsersRead, PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminOverview))' },
  { source: 'account-platform/internal/account/service.go', receiver: 'admin', method: 'GET', path: '/users/:user_id',
    registration: 'admin.GET("/users/:user_id", s.requirePermission(PermissionPlatformUsersRead), httpserver.AdaptFunc(s.adminUserDetail))' },
  { source: 'account-platform/internal/account/organization_routes.go', receiver: 'admin', method: 'GET', path: '/organizations/:organization_id',
    registration: 'admin.GET("/organizations/:organization_id", s.requirePermission(PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminOrganizationDetail))' },
];
const organizationDetailPolicy = {
  source: 'account-platform/internal/account/organization_routes.go', receiver: 'admin', method: 'GET',
  path: '/organizations/:organization_id', scope: 'platform', permissions: ['platform.organizations.read'],
  strategy: 'domain.platform-organization-detail-read',
};

// Require live Go expressions, not comments or strings which happen to quote one.
function assertLiveOnce(text, expression) {
  const {code, masked} = goSource(text);
  const offsets = [];
  for (let offset = code.indexOf(expression); offset !== -1; offset = code.indexOf(expression, offset + 1)) {
    if (masked[offset] !== ' ') offsets.push(offset);
  }
  assert.equal(offsets.length, 1, expression);
  return offsets[0];
}

async function restoredReadSources() {
  const paths = {
    service: 'service.go', organizations: 'organization_routes.go', authorization: 'authorization.go',
    organizationPermissions: 'organization_permissions.go', overview: 'admin_overview.go',
    user: 'admin_user_detail.go', organization: 'organization_admin_detail.go',
  };
  return Object.fromEntries(await Promise.all(Object.entries(paths).map(async ([name, file]) =>
    [name, (await readFile(new URL(`../../internal/account/${file}`, import.meta.url), 'utf8')).replace(/\r\n/g, '\n')])));
}

function assertRestoredReadAuthorization(sources, catalog) {
  for (const entry of restoredReadEntries) {
    assert.equal(catalog.http_sources.filter(source => source === entry.source).length, 1);
    const text = entry.source.endsWith('/service.go') ? sources.service : sources.organizations;
    // Match the entire tuple; other legitimate verbs use the same resource path.
    const matches = authorizationRoutes(entry.source, text).filter(route => route.receiver === entry.receiver && route.method === entry.method && route.path === entry.path);
    assert.equal(matches.length, 1, entry.path);
    assert.equal(matches[0].method, entry.method);
    assert.equal(matches[0].registration.trim(), entry.registration);
    const policies = catalog.http_policies.filter(policy => policy.source === entry.source &&
      policy.receiver === entry.receiver && policy.method === entry.method && policy.path === entry.path);
    // Overview and user detail are inline; duplicating their fallback would be stale.
    assert.deepEqual(policies, entry.path.startsWith('/organizations/') ? [organizationDetailPolicy] : []);
  }
  assertLiveOnce(sources.service, 'protected := router.Group("")');
  assertLiveOnce(sources.service, 'protected.Use(identity.GinMiddleware(verify), s.contextVersionHeaders())');
  assertLiveOnce(sources.service, 'admin := protected.Group("/admin/v1")');
  assertLiveOnce(sources.service, 's.registerOrganizationRoutes(protected, admin)');
  const enabledGroup = assertLiveOnce(sources.organizations, 'admin = admin.Group("", s.requireOrganizationsEnabled())');
  const detailRoute = assertLiveOnce(sources.organizations, restoredReadEntries[2].registration);
  assert.ok(enabledGroup < detailRoute, 'the live organization enabled group must precede the live detail route');
  assert.match(goSource(sources.authorization).code, /\bPermissionPlatformUsersRead\s*=\s*"platform\.users\.read"/);
  assert.match(goSource(sources.organizationPermissions).code, /\bPermissionPlatformOrganizationsRead\s*=\s*"platform\.organizations\.read"/);
  // Both permissions are platform-scoped, regardless of a detail's path parameter.
  const anyPermission = goSource(sources.authorization).code.split('func (s *Service) requireAnyPermission(')[1]?.split('\nfunc ')[0];
  assert.ok(anyPermission, 'the real OR authorization middleware must exist');
  assertLiveOnce(anyPermission, 'repository.Authorize(c.Request.Context(), principal, permission, nil, nil)');
  assertLiveOnce(anyPermission, 'platformauthorization.WithGrantedPermissions(c.Request.Context(), granted...)');
  assertLiveOnce(sources.overview, 'if platformauthorization.GrantedPermission(r.Context(), PermissionPlatformUsersRead) {\n\t\tresponse["users"], response["active_users"], response["disabled_users"] = result.Users, result.ActiveUsers, result.DisabledUsers\n\t}');
  assertLiveOnce(sources.overview, 'if platformauthorization.GrantedPermission(r.Context(), PermissionPlatformOrganizationsRead) {\n\t\tresponse["organizations"], response["active_memberships"], response["pending_invitations"] = result.Organizations, result.ActiveMemberships, result.PendingInvitations\n\t}');
  for (const [key, handler, parameter, reader] of [
    ['user', 'adminUserDetail', 'user_id', 'AdminUserDetail'],
    ['organization', 'adminOrganizationDetail', 'organization_id', 'AdminOrganizationDetail'],
  ]) {
    assertLiveOnce(sources[key], `func (s *Service) ${handler}(w http.ResponseWriter, r *http.Request) {`);
    assertLiveOnce(sources[key], `id, valid := pathUUID(w, r, "${parameter}")`);
    assertLiveOnce(sources[key], `item, err := repository.${reader}(r.Context(), id)`);
    assertLiveOnce(sources[key], 'w.Header().Set("Cache-Control", "no-store")');
  }
}

test('three restored admin reads retain exact tuples, platform permissions, grouping and domain projection', async () => {
  const sources = await restoredReadSources();
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  assertRestoredReadAuthorization(sources, catalog);
  for (const entry of restoredReadEntries) {
    const key = entry.source.endsWith('/service.go') ? 'service' : 'organizations';
    for (const replacement of [
      '', `// ${entry.registration}`, `var quotedRoute = \`${entry.registration}\``,
      entry.registration.replace('admin.GET', 'admin.POST'),
      entry.registration.replace('admin.GET', 'protected.GET'),
      entry.registration.replace(entry.path, `${entry.path}/other`),
      entry.registration.replace(/s\.admin(?:Overview|UserDetail|OrganizationDetail)/, 's.otherHandler'),
      entry.registration.replace('PermissionPlatformUsersRead', 'PermissionPlatformAuditRead').replace('PermissionPlatformOrganizationsRead', 'PermissionPlatformUsersRead'),
    ]) {
      const mutated = sources[key].replace(entry.registration, replacement);
      assert.notEqual(mutated, sources[key]);
      assert.throws(() => assertRestoredReadAuthorization({...sources, [key]: mutated}, catalog));
    }
  }
  const mutations = [
    ['service', 's.requireAnyPermission(PermissionPlatformUsersRead, PermissionPlatformOrganizationsRead)', 's.requirePermission(PermissionPlatformUsersRead)'],
    ['service', 's.requireAnyPermission(PermissionPlatformUsersRead, PermissionPlatformOrganizationsRead)', 's.requireAnyPermission(PermissionPlatformOrganizationsRead)'],
    ['service', 's.requireAnyPermission(PermissionPlatformUsersRead, PermissionPlatformOrganizationsRead)', 's.requireAnyPermission(PermissionPlatformUsersRead)'],
    ['service', 'admin := protected.Group("/admin/v1")', 'admin := router.Group("/other")'],
    ['service', 'protected.Use(identity.GinMiddleware(verify), s.contextVersionHeaders())', ''],
    ['service', 's.registerOrganizationRoutes(protected, admin)', 's.registerOrganizationRoutes(admin, protected)'],
    ['organizations', 'admin = admin.Group("", s.requireOrganizationsEnabled())', ''],
    ['authorization', '"platform.users.read"', '"platform.audit.read"'],
    ['organizationPermissions', '"platform.organizations.read"', '"org.members.read"'],
    ['overview', 'GrantedPermission(r.Context(), PermissionPlatformUsersRead)', 'GrantedPermission(r.Context(), PermissionPlatformOrganizationsRead)'],
    ['overview', 'GrantedPermission(r.Context(), PermissionPlatformOrganizationsRead)', 'GrantedPermission(r.Context(), PermissionPlatformUsersRead)'],
    ['user', 'pathUUID(w, r, "user_id")', 'pathUUID(w, r, "organization_id")'],
    ['organization', 'repository.AdminOrganizationDetail(r.Context(), id)', 'repository.AdminUserDetail(r.Context(), id)'],
  ];
  for (const [key, original, replacement] of mutations) {
    const mutated = sources[key].replace(original, replacement);
    assert.notEqual(mutated, sources[key]);
    assert.throws(() => assertRestoredReadAuthorization({...sources, [key]: mutated}, catalog));
  }
});

test('organization grouping order uses live positions despite comment decoys', async () => {
  const sources = await restoredReadSources();
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  const group = 'admin = admin.Group("", s.requireOrganizationsEnabled())';
  const route = restoredReadEntries[2].registration;
  // An earlier route comment must not make a correctly ordered live pair fail.
  const commentedRoute = `// ${route}\n${sources.organizations}`;
  assertRestoredReadAuthorization({...sources, organizations: commentedRoute}, catalog);
  // Keep a comment at the old group location, but move the actual middleware
  // registration after the real route. A raw indexOf comparison misses this.
  const movedGroup = sources.organizations.replace(group, `// ${group}`)
    .replace(route, `${route}\n\t${group}`);
  assert.notEqual(movedGroup, sources.organizations);
  assert.ok(assertLiveOnce(movedGroup, group) > assertLiveOnce(movedGroup, route));
  assert.throws(() => assertRestoredReadAuthorization({...sources, organizations: movedGroup}, catalog),
    /the live organization enabled group must precede the live detail route/);
});

test('organization detail fallback rejects missing, incorrect, duplicate and inline-stale policies', async () => {
  const isDetail = row => row.source === organizationDetailPolicy.source && row.receiver === 'admin' && row.method === 'GET' && row.path === organizationDetailPolicy.path;
  for (const [change, expected] of [
    [catalog => { catalog.http_policies = catalog.http_policies.filter(row => !isDetail(row)); }, /unclassified_http_entrypoint:.*organization_routes\.go\|admin\|GET\|\/organizations\/:organization_id/],
    [catalog => { catalog.http_policies.find(isDetail).scope = 'organization'; }, /permission_scope_mismatch/],
    [catalog => { catalog.http_policies.find(isDetail).permissions = ['platform.unknown.read']; }, /unknown_human_permission/],
    [catalog => { catalog.http_policies.push({...catalog.http_policies.find(isDetail)}); }, /duplicate_http_policy/],
    [catalog => { catalog.http_policies.find(isDetail).method = 'POST'; }, /unclassified_http_entrypoint/],
    ...restoredReadEntries.slice(0, 2).map(({registration, ...tuple}) => [catalog => {
      catalog.http_policies.push({...tuple, scope: 'platform', permissions: ['platform.users.read'], strategy: 'domain.incorrect-inline-fallback'});
    }, /stale_http_policy/]),
  ]) {
    await rejectedMutation(catalog => {
      const original = JSON.stringify(catalog);
      change(catalog);
      assert.notEqual(JSON.stringify(catalog), original);
    }, expected);
  }
  // A different known platform permission is syntactically legal to the general
  // checker; the exact contract above must still reject the wrong domain binding.
  const sources = await restoredReadSources();
  const catalog = JSON.parse(await readFile(catalogURL, 'utf8'));
  catalog.http_policies.find(isDetail).permissions = ['platform.users.read'];
  assert.throws(() => assertRestoredReadAuthorization(sources, catalog));
});


const colonActionRegistrations = [
  ['account/service.go', 'internal', '/ledger-repair/authorization-proofs:batch', '/authorization-proofs:batch'],
  ['billing/organization_routes.go', 'protected', '/internal/v1/organizations/:organization_id/quota-grants:batch', '/quota-grants:batch'],
  ['token/management_routes.go', 'admin', '/commands:precheck', '/commands:precheck'],
  ['token/organization_finance_routes.go', 'routes', '/organizations/:organization_id/account-closed-grants:reclaim-batch', '/account-closed-grants:reclaim-batch'],
  ['token/organization_finance_routes.go', 'routes', '/organizations/:organization_id/quota-grants:batch', '/quota-grants:batch'],
];

function assertColonActionGuard(source, file, receiver, path, suffix) {
  const routes = authorizationRoutes(file, source).filter(route => route.path === path);
  assert.equal(routes.length, 1, path);
  const route = routes[0];
  assert.equal(route.receiver, receiver, path);
  assert.equal(route.method, 'POST', path);
  const prefix = `${receiver}.POST("${path}", `;
  const guard = `httpserver.RequirePathSuffix("${suffix}")`;
  assert.ok(route.registration.trim().startsWith(prefix + guard + ', '), path);
  const {code, masked} = goSource(route.registration);
  const liveGuards = [...code.matchAll(/\bhttpserver\.RequirePathSuffix\(/g)]
    .filter(match => masked[match.index] !== ' ');
  assert.equal(liveGuards.length, 1, path);
}

test('all five production colon action registrations keep their first exact path guard', async () => {
  for (const [file, receiver, path, suffix] of colonActionRegistrations) {
    const source = await readFile(new URL(`../../internal/${file}`, import.meta.url), 'utf8');
    const check = text => assertColonActionGuard(text, file, receiver, path, suffix);
    check(source);
    const route = authorizationRoutes(file, source).find(route => route.path === path).registration;
    const guard = `httpserver.RequirePathSuffix("${suffix}")`;
    const prefix = `${receiver}.POST("${path}", `;
    const mutations = [
      route.replace(guard + ', ', ''),
      route.replace(guard, `httpserver.RequirePathSuffix("${suffix}Other")`),
      route.replace(prefix + guard + ', ', prefix).replace(/\)$/, ', ' + guard + ')'),
      route.replace(guard + ', ', guard + ', ' + guard + ', '),
    ];
    for (const registration of mutations) {
      assert.notEqual(registration, route, path);
      const mutated = source.replace(route, registration);
      assert.notEqual(mutated, source, path);
      assert.throws(() => check(mutated), undefined, path);
    }
    // Comments and literals cannot replace a missing executable guard.
    const absent = mutations[0];
    for (const decoy of ['// ' + route, `var guardExample = \`${route}\``]) {
      assert.throws(() => check(source.replace(route, absent) + '\n' + decoy), undefined, path);
    }
  }
});
