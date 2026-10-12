import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import { checkCoverage, loadInputs } from '../check-token-management-authorization-coverage.mjs';

const inputs = loadInputs();
const anchors = {
  routeSource: inputs.routeSource.split(/\r?\n/).find(line => /^\s*internal\.POST\("\/holds\/expiry-proof"/.test(line)),
  credentialSource: inputs.credentialSource.split(/\r?\n/).find(line => /^\s*router\.GET\("\/assets\/credential\.css"/.test(line)),
};
for (const anchor of Object.values(anchors)) assert.ok(anchor);
const declarations = {
  routeSource: { receiver: 'internal', args: '"/coverage-unlisted", serviceauth.Require("internal.token.hold-expiry.prove"), httpserver.AdaptFunc(s.expiredHoldProof)' },
  credentialSource: { receiver: 'router', args: '"/coverage-unlisted", httpserver.AdaptFunc(s.stylesheet)' },
};
function insert(field, extra) {
  // Add within the real receiver's scope, preserving all original declarations,
  // manifest, OpenAPI, policies and DTO inputs. This does not execute Go routes.
  const anchor = anchors[field];
  const indent = /^\s*/.exec(anchor)[0];
  return { ...inputs, [field]: inputs[field].replace(anchor, `${anchor}\n${extra.map(line => indent + line).join('\n')}`) };
}

test('actual complete F04 inputs retain exact declared routes and policies', () => {
  assert.deepEqual(checkCoverage(inputs), {
    managementRoutes: inputs.manifest.routes.length,
    credentialRoutes: inputs.manifest.credentialRoutes.length,
    commands: inputs.manifest.commands.length,
    organizationFinanceRoutes: inputs.manifest.organizationFinanceRoutes.length,
  });
});

const unsupported = {
  HEAD: ({ receiver, args }) => [`${receiver}.HEAD(${args})`],
  OPTIONS: ({ receiver, args }) => [`${receiver}.OPTIONS(${args})`],
  Handle: ({ receiver, args }) => [`${receiver}.Handle("GET", ${args})`],
  Any: ({ receiver, args }) => [`${receiver}.Any(${args})`],
  Match: ({ receiver, args }) => [`${receiver}.Match([]string{"GET", "HEAD"}, ${args})`],
  Static: ({ receiver }) => [`${receiver}.Static("/coverage-unlisted", ".")`],
  StaticFS: ({ receiver }) => [`${receiver}.StaticFS("/coverage-unlisted", nil)`],
  StaticFile: ({ receiver }) => [`${receiver}.StaticFile("/coverage-unlisted", "fixture.json")`],
  StaticFileFS: ({ receiver }) => [`${receiver}.StaticFileFS("/coverage-unlisted", "fixture.json", nil)`],
  assignment: ({ receiver, args }) => [`_ = ${receiver}.GET(${args})`],
  conditional: ({ receiver, args }) => [`if true { ${receiver}.GET(${args}) }`],
  selector: ({ receiver, args }) => [`${receiver}.${receiver === 'router' ? 'Engine' : 'RouterGroup'}.GET(${args})`],
  methodValue: ({ receiver, args }) => [`coverageRegister := ${receiver}.GET`, `coverageRegister(${args})`],
  splitSelector: ({ receiver, args }) => [`${receiver}.`, `GET(${args})`],
  unicodeOffset: ({ receiver, args }) => ['_ = "\u{10000}"', `${receiver}.HEAD(${args})`],
  sameLineMultiple: ({ receiver, args }) => [`if true { ${receiver}.GET(${args}); ${receiver}.HEAD(${args}) }`],
};
for (const field of Object.keys(declarations)) {
  for (const [name, declaration] of Object.entries(unsupported)) {
    test(`${field} refuses an unclassified ${name} declaration without changing expected inventory`, () => {
      assert.throws(() => checkCoverage(insert(field, declaration(declarations[field]))), /unrecognized .*route/);
    });
  }
  test(`${field} preserves existing multiline declaration rejection`, () => {
    const { receiver, args } = declarations[field];
    assert.throws(() => checkCoverage(insert(field, [`${receiver}.GET(`, `${args})`])), /unrecognized .*route/);
  });
  test(`${field} preserves existing nonliteral path rejection`, () => {
    const { receiver, args } = declarations[field];
    assert.throws(() => checkCoverage(insert(field, ['coveragePath := "/coverage-unlisted"', `${receiver}.GET(${args.replace('"/coverage-unlisted"', 'coveragePath')})`])), /unrecognized .*route/);
  });
  test(`${field} ignores method-looking line/block comments and quoted/raw strings`, () => {
    const { receiver, args } = declarations[field];
    assert.deepEqual(checkCoverage(insert(field, [
      `// ${receiver}.HEAD(${args})`,
      '/*', `${receiver}.GET(${args})`, '*/',
      `_ = "${receiver}.GET(\\\"/not-a-route\\\")"`,
      '_ = `', `${receiver}.GET(${args})`, '`',
      "_ = '\"'",
    ])), checkCoverage(inputs));
  });
}

test('real CRLF route sources preserve declaration offsets and complete inventory', () => {
  assert.deepEqual(checkCoverage({
    ...inputs,
    routeSource: inputs.routeSource.replace(/\r?\n/g, '\r\n'),
    credentialSource: inputs.credentialSource.replace(/\r?\n/g, '\r\n'),
  }), checkCoverage(inputs));
});

test('organization finance registration cannot disappear outside the management inventory', () => {
  const result = checkCoverage(inputs);
  assert.equal(result.organizationFinanceRoutes, 13);
});

test('closed-grant authority helper cannot disappear or change permission', () => {
  for (const source of [
    inputs.organizationHandlerSource.replaceAll('s.closedGrantClient(w, r)', 's.unclassifiedClosedGrantClient(w, r)'),
    inputs.organizationHandlerSource.replace('func (s *Service) closedGrantClient(', 'func (s *Service) unclassifiedClosedGrantClient('),
    inputs.organizationHandlerSource.replace('s.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")', 's.resolveOrganizationBilling(w, r, "org.grants.allocate")'),
  ]) assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), /closed grant authority/);
});

test('comments and strings cannot stand in for the closed-grant authority chain', () => {
  const caller = 'resolved, org, client, ok := s.closedGrantClient(w, r)';
  const authority = 'resolved, org, _, ok := s.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")';
  for (const [original, replacement, selector, error] of [
    [caller, 'resolved, org, client, ok := s.unclassifiedClosedGrantClient(w, r)', 's.closedGrantClient(w, r)', /closed grant authority helper/],
    [authority, 'resolved, org, _, ok := s.resolveOrganizationBilling(w, r, "org.grants.allocate")', 's.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")', /closed grant authority changed/],
  ]) {
    for (const comment of [`// ${selector}`, `/* ${original} */`, `_ = \`${original}\``, `_ = \`\n${original}\n\``]) {
      const source = inputs.organizationHandlerSource.replaceAll(original, `${replacement}\n\t${comment}`);
      assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: source }), error);
    }
  }
});

test('a commented or stringified whole helper cannot hide changed real authority', () => {
  const helper = inputs.organizationHandlerSource.match(/func \(s \*Service\) closedGrantClient\([^\n]*\) [^\n]*\{[\s\S]*?\n\}/)?.[0];
  assert.ok(helper);
  const changed = inputs.organizationHandlerSource.replace('s.resolveOrganizationBilling(w, r, "org.grants.reclaim_closed")', 's.resolveOrganizationBilling(w, r, "org.grants.allocate")');
  for (const standIn of [`/*\n${helper}\n*/\n`, `var fakeClosedGrantHelper = \`\n${helper}\n\`\n`]) {
    assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: standIn + changed }), /closed grant authority changed/);
  }
});

test('both closed-grant paths must be present in the manifest and contract', () => {
  const paths = [
    '/v1/organizations/{organization_id}/account-closed-grants/{grant_id}/reclaim',
    '/v1/organizations/{organization_id}/account-closed-grants:reclaim-batch',
  ];
  for (const path of paths) {
    const manifest = structuredClone(inputs.manifest);
    manifest.organizationFinanceRoutes = manifest.organizationFinanceRoutes.filter(route => route.path !== path);
    assert.throws(() => checkCoverage({ ...inputs, manifest }), /organization route authority/);
    const organizationFinance = structuredClone(inputs.organizationFinance);
    delete organizationFinance.paths[path];
    assert.throws(() => checkCoverage({ ...inputs, organizationFinance }), /organization OpenAPI/);
  }
});

test('closed-grant routes cannot bypass their declared management gate', () => {
  for (const handler of ['reclaimClosedGrant', 'reclaimClosedGrantsBatch']) {
    const line = inputs.organizationFinanceSource.split(/\r?\n/).find(value => value.includes(`httpserver.AdaptFunc(s.${handler})`));
    assert.ok(line);
    const source = inputs.organizationFinanceSource.replace(line, '')
      .replace('if !s.managementEnabled {', `${line.trim()}\n\tif !s.managementEnabled {`);
    assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: source }), /precedes management assembly gate/);
  }
});

test('organization finance path and authority drift are rejected against frozen manifest', () => {
  assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: fs.readFileSync(new URL('../../account-platform/internal/token/organization_finance_routes.go', import.meta.url), 'utf8').replace('/balance', '/balance-unclassified') }), /organization/);
});

for (const [name, change] of Object.entries({
 omitted: value => value.replace(/.*routes.GET\("\/organizations\/:organization_id\/balance".*\r?\n/, ''),
 method: value => value.replace('routes.GET("/organizations/:organization_id/balance"', 'routes.POST("/organizations/:organization_id/balance"'),
 gate: value => value.replace('!s.managementEnabled', 's.managementEnabled'),
 conditional: value => value.replace('routes.GET("/organizations/:organization_id/balance",', 'if true { routes.GET("/organizations/:organization_id/balance",'),
 methodValue: value => value + '\n register := routes.GET\n',
})) test(`organization finance rejects ${name} registration drift`, () => {
 assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: change(inputs.organizationFinanceSource) }));
});
test('organization finance verifies permission inside the installed handler, not only declarations', () => {
 assert.throws(() => checkCoverage({ ...inputs, organizationHandlerSource: inputs.organizationHandlerSource.replace('resolveOrganizationBilling(w, r, "org.wallet.read")', 'resolveOrganizationBilling(w, r, "org.units.read")') }), /organization/);
});
for (const part of ['path', 'authority', 'key', 'approval']) test(`organization finance OpenAPI ${part} drift fails closed`, () => {
 const contract = structuredClone(inputs.organizationFinance), op = contract.paths['/v1/organizations/{organization_id}/model-policy'].put;
 if (part === 'path') delete contract.paths['/v1/organizations/{organization_id}/balance'];
 if (part === 'authority') op['x-authority'].all = ['org.members.manage'];
 if (part === 'key') op.parameters = op.parameters.filter(p => p.name !== 'Idempotency-Key');
 if (part === 'approval') delete op['x-approval-policy'];
 assert.throws(() => checkCoverage({ ...inputs, organizationFinance: contract }), /organization/);
});

test('organization request properties match the actual handler body decoder', () => {
 const contract = structuredClone(inputs.organizationFinance);
 delete contract.components.schemas.ModelPolicyWrite.properties.expected_version;
 assert.throws(() => checkCoverage({ ...inputs, organizationFinance: contract }), /organization request fields/);
});

test('a commented-out organization assembly gate does not satisfy the checker', () => {
 const source = inputs.organizationFinanceSource.replace('if !s.managementEnabled {', '/* if !s.managementEnabled { return } */ if s.managementEnabled {');
 assert.throws(() => checkCoverage({ ...inputs, organizationFinanceSource: source }), /organization management/);
});
