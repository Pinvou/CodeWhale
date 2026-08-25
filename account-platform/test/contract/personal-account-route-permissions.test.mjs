import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import YAML from 'yaml';

const service = await readFile(new URL('../../internal/account/service.go', import.meta.url), 'utf8');
const authorization = await readFile(new URL('../../internal/account/authorization.go', import.meta.url), 'utf8');
const openapi = YAML.parse(await readFile(new URL('../../contracts/openapi/account.yaml', import.meta.url), 'utf8'));
const personalHandlers = await Promise.all([
  '../../internal/account/service.go',
  '../../internal/account/account_closure_http.go',
  '../../internal/account/profile_changes_http.go',
].map(relative => readFile(new URL(relative, import.meta.url), 'utf8'))).then(items => items.join('\n'));

const routePermissions = [
  ['GET', '/v1/me', 'PermissionPersonalProfileRead'],
  ['GET', '/v1/me/commerce-customer-mapping', 'PermissionPersonalProfileRead'],
  ['POST', '/v1/me/profile-change-requests', 'PermissionPersonalProfileUpdate'],
  ['POST', '/v1/me/profile-change-requests/:request_id/mobile-challenges', 'PermissionPersonalProfileUpdate'],
  ['POST', '/v1/me/profile-change-requests/:request_id/actions/complete', 'PermissionPersonalProfileUpdate'],
  ['GET', '/v1/me/profile-change-requests/:request_id', 'PermissionPersonalProfileUpdate'],
  ['GET', '/v1/me/authorization-context', 'PermissionPersonalProfileRead'],
  ['GET', '/v1/me/role-bindings', 'PermissionPersonalProfileRead'],
  ['GET', '/v1/me/organizations', 'PermissionPersonalProfileRead'],
  ['POST', '/v1/sessions/current/active-context', 'PermissionPersonalContextSwitch'],
  ['POST', '/v1/me/account-closure-requests', 'PermissionPersonalAccountClosureRequest'],
  ['GET', '/v1/me/account-closure-request', 'PermissionPersonalAccountClosureRead'],
  ['GET', '/v1/me/invitations', 'PermissionPersonalInvitationsRead'],
  ['POST', '/v1/invitations/accept', 'PermissionPersonalInvitationsAccept'],
];

test('every self-service Account route has its exact personal permission middleware', () => {
  for (const [method, route, permission] of routePermissions) {
    assert.match(service, new RegExp(`protected\\.${method}\\("${route.replaceAll('/', '\\/')}"\\s*,\\s*s\\.requirePermission\\(${permission}\\)`), `${method} ${route}`);
    const contractRoute = route.replace(/^\/v1/, '').replaceAll(':request_id', '{request_id}');
    assert.ok(openapi.paths[contractRoute][method.toLowerCase()].responses['403'], `${method} ${contractRoute} must publish permission denial`);
  }
  for (const permission of new Set(routePermissions.map(([, , value]) => value))) {
    assert.match(authorization, new RegExp(`${permission}\\s*=\\s*"personal\\.`));
  }
});

test('self-service targets come only from the authenticated principal and lifecycle remains fail closed', () => {
  assert.doesNotMatch(personalHandlers, /target_user_id|user_id"\s*`json/);
  for (const handler of [
    'GetMe', 'ResolveAuthorizationContext', 'ListOrganizations', 'SetActiveContext',
    'CreateAccountClosure', 'AccountClosureForPrincipal', 'ListMyInvitations', 'AcceptInvitation',
  ]) assert.match(personalHandlers, new RegExp(`${handler}\\([^\\n]*mustPrincipal\\(r\\.Context\\(\\)\\)`));
  assert.match(authorization, /case "PROVISIONING", "REPAIR_REQUIRED":[\s\S]*case "CLOSING":[\s\S]*return false/);
});
