import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import YAML from 'yaml';

const read = (path) => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

const [up, down, authorization, directory, admin, cursor, integration, accountService, portal, openapiRaw, nginx, helm] = await Promise.all([
  read('migrations/account/000043_token_owner_candidates.up.sql'),
  read('migrations/account/000043_token_owner_candidates.down.sql'),
  read('internal/account/authorization.go'),
  read('internal/account/directory.go'),
  read('internal/account/admin.go'),
  read('internal/account/token_owner_candidate_cursor.go'),
  read('internal/account/token_owner_candidates_integration_test.go'),
  read('internal/account/service.go'),
  read('internal/portal/service.go'),
  read('contracts/openapi/account.yaml'),
  read('deploy/nginx/account-platform.conf.template'),
  read('deploy/helm/account-platform/templates/app-deployments.yaml'),
]);

test('owner candidate permission migration grants exactly the two approved platform roles', () => {
  assert.match(up, /INSERT INTO permissions\(code,scope,description\)[\s\S]*'platform\.tokens\.owner-candidates\.read','PLATFORM'/);
  assert.match(up, /permission_code='platform\.tokens\.owner-candidates\.read'[\s\S]*role_code NOT IN \('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN'\)/);
  const grants = [...up.matchAll(/\('((?:PLATFORM_)[A-Z_]+)'\)/g)].map((match) => match[1]).sort();
  assert.deepEqual(grants, ['PLATFORM_MODEL_ADMIN', 'PLATFORM_SUPER_ADMIN']);
  assert.match(up, /inserted_grants AS \([\s\S]*SELECT approved_role\.role_code,repaired_permission\.code[\s\S]*CROSS JOIN repaired_permission[\s\S]*ON CONFLICT DO NOTHING[\s\S]*RETURNING role_code[\s\S]*UNION SELECT role_code FROM inserted_grants/);
  assert.match(up, /existing_permission AS MATERIALIZED[\s\S]*scope_repair_roles AS MATERIALIZED[\s\S]*changed_roles AS[\s\S]*UNION SELECT role_code FROM revoked_roles[\s\S]*UNION SELECT role_code FROM inserted_grants/);
  assert.match(up, /changed_bindings AS[\s\S]*JOIN roles role_definition ON role_definition\.code=assignment\.role_code[\s\S]*affected_scopes AS/);
  assert.match(up, /FROM changed_bindings WHERE role_scope='PERSONAL'[\s\S]*FROM changed_bindings WHERE role_scope='PLATFORM'/);
  assert.match(up, /role_scope IN \('ORGANIZATION','UNIT'\)[\s\S]*role_scope='UNIT'/);
  assert.match(up, /SELECT increment_authorization_scope_version\(user_id,scope,scope_id\)[\s\S]*FROM affected_scopes/);
  assert.match(up, /CREATE TABLE IF NOT EXISTS token_owner_candidate_query_attempts/);
  assert.match(up, /CREATE INDEX IF NOT EXISTS token_owner_candidate_attempt_service_window_idx/);
});

test('down migration revokes grants, invalidates assignments without a permission join, then deletes the definition', () => {
  const revoke = down.indexOf("DELETE FROM role_permissions");
  const bump = down.indexOf('SELECT increment_authorization_scope_version');
  const remove = down.indexOf("DELETE FROM permissions WHERE code='platform.tokens.owner-candidates.read'");
  assert.ok(revoke >= 0 && bump > revoke && remove > bump);
  assert.doesNotMatch(down.slice(revoke, remove), /JOIN role_permissions/);
  assert.match(down, /role_code IN \('PLATFORM_SUPER_ADMIN','PLATFORM_MODEL_ADMIN'\)/);
  assert.match(down, /JOIN roles role_definition ON role_definition\.code=assignment\.role_code[\s\S]*SELECT increment_authorization_scope_version\(user_id,scope,scope_id\)/);
});

test('Account exposes the minimal directory only to the exact Token workload permission', () => {
  assert.match(authorization, /PermissionPlatformTokenOwnerCandidatesRead\s*= "platform\.tokens\.owner-candidates\.read"/);
  assert.match(accountService, /internal\.GET\("\/token-owner-candidates", serviceauth\.Require\("internal\.token-owner-candidates\.read"\)/);
  assert.doesNotMatch(accountService, /admin\.GET\("\/token-owner-candidates"/);
  assert.doesNotMatch(portal, /"\/admin\/v1\/token-owner-candidates"/);
  assert.match(helm, /"token-gateway"[\s\S]*"internal\.token-owner-candidates\.read"/);
  assert.match(directory, /JOIN role_permissions permission[\s\S]*permission\.permission_code=\$2/);
  assert.match(directory, /account_user\.display_name[\s\S]*account_user\.username[\s\S]*account_user\.user_code/);
  assert.doesNotMatch(directory.slice(0, directory.indexOf('func (r *PostgresRepository) ListUnits')), /account_user\.email|account_user\.mobile/);
  assert.match(admin, /Cache-Control", "no-store"/);
});

test('candidate collection uses immutable order, trusted-service limiting, and bound signed cursors', () => {
  assert.match(directory, /candidate\.created_at<=\$2/);
  assert.match(directory, /SELECT transaction_timestamp\(\)/);
  assert.match(directory, /ORDER BY candidate\.owner_type,candidate\.owner_id/);
  assert.match(directory, /count\(\*\)>=30[\s\S]*count\(\*\)>=60/);
  assert.match(directory, /caller_service=\$2/);
  assert.doesNotMatch(directory, /X-Forwarded-For|X-Real-IP|X-Pinvou-Client-Source|sourceHash/);
  assert.match(directory, /Cursor\.AuthorizationVersion != authorizationVersion[\s\S]*ErrTokenOwnerCandidateCursorExpired/);
  assert.match(cursor, /hmac\.New\(sha256\.New, key\)/);
  assert.match(cursor, /CallerService != actor\.CallerService[\s\S]*ActorSubject != actor\.Subject[\s\S]*FilterFingerprint/);
  assert.match(cursor, /tokenOwnerCandidateCursorTTL\s*= 15 \* time\.Minute/);
  assert.match(directory, /filter\.Cursor != nil[\s\S]*result='STARTED'[\s\S]*completed_at IS NULL[\s\S]*expires_at>now\(\)/);
  assert.match(directory, /count\(\*\)>=3[\s\S]*count\(\*\)>=10/);
  assert.match(admin, /http\.StatusGone, "CURSOR_EXPIRED"/);
  assert.match(admin, /http\.StatusForbidden, "PERMISSION_DENIED"/);
});

test('internal OpenAPI freezes a four-field no-email projection and stable error distinction', () => {
  const openapi = YAML.parse(openapiRaw);
  const operation = openapi.paths['/internal/v1/token-owner-candidates'].get;
  assert.ok(operation);
  assert.deepEqual(operation.security, [{ serviceToken: [] }]);
  assert.deepEqual(Object.keys(openapi.components.schemas.TokenOwnerCandidate.properties).sort(), ['display_label', 'id', 'status', 'type']);
  assert.equal(openapi.components.schemas.TokenOwnerCandidate.additionalProperties, false);
  assert.doesNotMatch(JSON.stringify(openapi.components.schemas.TokenOwnerCandidate), /email|mobile|subject/i);
  for (const status of ['200', '400', '403', '410', '429']) assert.ok(operation.responses[status], status);
  const limit = operation.parameters.find((parameter) => parameter.name === 'limit');
  assert.equal(limit.schema.default, 20000);
  assert.equal(limit.schema.maximum, 50000);
});

test('the public edge discards spoofable source headers and overwrites the trusted hop', () => {
  assert.match(nginx, /location \^~ \/account\/internal\/ \{[\s\S]*return 404;/);
  const locations = nginx.split(/\n\s*location /).slice(1).filter((location) => location.includes('proxy_pass'));
  assert.ok(locations.length >= 10);
  for (const location of locations) {
    assert.match(location, /proxy_set_header X-Pinvou-Client-Source "";/);
    assert.match(location, /proxy_set_header X-Real-IP \$remote_addr;/);
    assert.match(location, /proxy_set_header X-Forwarded-For \$remote_addr;/);
    assert.doesNotMatch(location, /\$proxy_add_x_forwarded_for/);
  }
});

test('real PostgreSQL gate covers migration idempotence, authorization, cursors, and shared concurrency limits', () => {
  assert.match(integration, /os\.Getenv\("ACCOUNT_DATABASE_URL"\)/);
  assert.match(integration, /freshPermissionScope[\s\S]*assertTokenOwnerCandidateGrants[\s\S]*migrations\.DownOne/);
  assert.match(integration, /migration\.UpSQL[\s\S]*migration\.DownSQL/);
  assert.match(integration, /'PERSONAL_USER',\$1[\s\S]*beforePersonal[\s\S]*afterPersonal != beforePersonal\+1/);
  assert.match(integration, /beforeDown[\s\S]*downVersions\[0\] != beforeDown\[0\]\+1[\s\S]*downVersions\[1\] != beforeDown\[1\]\+1/);
  assert.match(integration, /unprivileged HTTP status/);
  assert.match(integration, /tampered cursor status[\s\S]*cross-subject cursor status[\s\S]*authorization-changed cursor status/);
  assert.match(integration, /assertTokenOwnerCandidateRateLimit\([\s\S]*31, 30\)/);
  assert.match(integration, /for index := 0; index < 61; index\+\+/);
  assert.match(integration, /seedTokenOwnerCandidateOrganizations\([\s\S]*1601/);
  assert.match(integration, /10\*time\.Second[\s\S]*logical scan attempts/);
  assert.match(integration, /fourth concurrent scan error/);
  assert.match(integration, /activeServiceActors := make\(\[\]TokenOwnerCandidateActor, 0, 11\)[\s\S]*index < 10[\s\S]*eleventh service active scan error/);
  assert.match(integration, /ffffffff-ffff-ffff-ffff-ffffffffffff[\s\S]*clock_timestamp\(\)/);
});
