import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

function validator(contract, schemaName) {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${schemaName}`, $defs: contract.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/').replaceAll('token-gateway.yaml#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

const id = digit => `${digit.repeat(8)}-${digit.repeat(4)}-7${digit.repeat(3)}-8${digit.repeat(3)}-${digit.repeat(12)}`;

test('APT-133 exposes only closed credential metadata and result projections', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const credentialPath = contract.paths['/token/admin/v1/token-accounts/{token_account_id}/credentials']?.get;
  for (const status of ['200', '400', '403', '410', '422', '429', '503', '504']) assert.ok(credentialPath.responses[status], status);
  for (const suffix of ['enroll', 'rotate']) {
    const operation = contract.paths[`/token/admin/v1/token-accounts/{token_account_id}/credential-launches/${suffix}`]?.post;
    assert.ok(operation);
    assert.ok(operation.parameters.some(parameter => parameter.$ref === '#/components/parameters/IdempotencyKey'));
  }
  assert.ok(contract.paths['/token/admin/v1/credential-results/{result_ref}']?.get);
  assert.ok(contract.paths['/token/admin/v1/credential-results/{result_ref}/revoke-window']?.post);
  const safeDetailPath = contract.paths['/token/admin/v2/token-accounts/{token_account_id}']?.get;
  for (const status of ['200', '400', '403', '404', '503', '504']) assert.ok(safeDetailPath.responses[status], `safe detail ${status}`);
  const validateSafeDetail = validator(contract, 'TokenAdminAccountSafeDetail');
  const safeDetail = {
    token_account_id: id('4'), owner_type: 'USER', owner_id: id('2'), wallet_id: id('5'), name: 'safe account',
    status: 'ACTIVE', share_scope: 'OWNER_UNIT_ONLY', management_mode: 'PLATFORM', active_credential_count: 1,
    route_count: 1, account_version: 2, updated_at: '2026-09-04T12:00:00Z', as_of: '2026-09-04T12:00:00Z',
  };
  assert.equal(validateSafeDetail(safeDetail), true, JSON.stringify(validateSafeDetail.errors));
  for (const inlineResource of [{ credentials: [] }, { routes: [] }, { provider_url: 'https://private.example' }]) {
    assert.equal(validateSafeDetail({ ...safeDetail, ...inlineResource }), false, JSON.stringify(inlineResource));
  }

  const validateCredential = validator(contract, 'TokenAdminCredentialMetadata');
  const credential = {
    credential_id: id('1'), credential_type: 'NEW_API', fingerprint: 'a'.repeat(64), version: 2,
    status: 'ACTIVE', validation_status: 'VALID', allowed_models: ['model-a'], validated_at: '2026-09-04T12:00:00Z',
    responsible_actor_id: id('2'), created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-04T12:00:00Z',
  };
  assert.equal(validateCredential(credential), true, JSON.stringify(validateCredential.errors));
  for (const privateFact of [
    { secret: 'private' }, { ciphertext: 'private' }, { key_id: 'private' },
    { base_url: 'https://private.example' }, { raw_error: 'private' }, { provider_key_id: 'private' },
  ]) assert.equal(validateCredential({ ...credential, ...privateFact }), false, JSON.stringify(privateFact));

  const validateResult = validator(contract, 'CredentialIsolationResult');
  const result = { result_ref: id('3'), token_account_id: id('4'), action: 'ROTATE', status: 'SUCCEEDED', credential_id: id('1'), credential_version: 2, credential_status: 'ACTIVE', validation_status: 'VALID', fingerprint: 'a'.repeat(64), audit_request_id: 'request-1', window_confirmed: true, updated_at: '2026-09-04T12:00:00Z' };
  assert.equal(validateResult(result), true, JSON.stringify(validateResult.errors));
  assert.equal(validateResult({ ...result, secret: 'private' }), false);
  assert.equal(validateResult({ ...result, error_detail: 'private' }), false);
  assert.equal(validateResult({ ...result, status: 'PENDING' }), false, 'pending cannot carry success metadata');
  assert.equal(validateResult({ ...result, status: 'FAILED', error_code: 'CREDENTIAL_VALIDATION_FAILED' }), false, 'failure cannot carry success metadata');
  const pending = { result_ref: id('3'), token_account_id: id('4'), action: 'ROTATE', status: 'PENDING', audit_request_id: 'request-1', window_confirmed: true, updated_at: '2026-09-04T12:00:00Z' };
  assert.equal(validateResult(pending), true, JSON.stringify(validateResult.errors));
  assert.equal(validateResult({ ...pending, status: 'FAILED' }), false, 'terminal failure requires a stable code');
  assert.equal(validateResult({ ...pending, status: 'FAILED', error_code: 'CREDENTIAL_VALIDATION_FAILED' }), true, JSON.stringify(validateResult.errors));
});

test('APT-133 closes the governed credential validation contract', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const precheck = contract.components.schemas.TokenCommandPrecheckRequest;
  assert.ok(precheck.properties.command_type.enum.includes('TOKEN_CREDENTIAL_VALIDATE'));
  const validationBranch = precheck.allOf.find(branch => branch.if?.properties?.command_type?.const === 'TOKEN_CREDENTIAL_VALIDATE');
  assert.equal(validationBranch.then.properties.target_resource_type.const, 'PROVIDER_CREDENTIAL');
  assert.equal(validationBranch.then.properties.command.additionalProperties, false);
  assert.deepEqual(validationBranch.then.properties.command.required, ['token_account_id', 'credential_id']);
  const expected = contract.components.schemas.TokenCredentialValidateExpectedVersions;
  assert.equal(expected.additionalProperties, false);
  assert.deepEqual(expected.required, ['token_account_id', 'expected_account_version', 'credential_id', 'credential_version', 'precheck_fingerprint', 'request_fingerprint']);

  const validate = validator(contract, 'TokenCredentialValidationGovernedResult');
  const value = {
    validation_status: 'INVALID', error_code: 'CREDENTIAL_CHECK_FAILED', credential: {
      credential_id: id('1'), credential_type: 'NEW_API', fingerprint: 'a'.repeat(64), version: 2,
      status: 'ACTIVE', validation_status: 'INVALID', allowed_models: ['model-a'], responsible_actor_id: id('2'),
      created_at: '2026-09-03T12:00:00Z', updated_at: '2026-09-04T12:00:00Z',
    },
  };
  assert.equal(validate(value), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...value, credential: { ...value.credential, provider_url: 'https://private.example' } }), false);
});

test('APT-133 keeps Portal and credential origin responsibilities separate', async () => {
  const [portal, handoffHandler, handoffScript, generatedHash, handoffClient, launchState, origin, originIntegration, realBrowser, isolation, ui, navigation] = await Promise.all([
    read('internal/portal/service.go'), read('internal/portal/credential_handoff.go'), read('internal/portal/credential-handoff.v1.js'), read('internal/portal/credential_handoff_hash_generated.go'),
    read('../pinvou3-app/src/features/account-platform/admin-credential-handoff.js'),
    read('../pinvou3-app/src/features/account-platform/admin-credential-launch-state.js'),
    read('internal/token/credential_origin.go'), read('internal/token/credential_origin_integration_test.go'),
    read('../pinvou3-app/tests/admin_token_credential_origin_real_smoke.mjs'), read('internal/token/credential_isolation.go'),
    read('../pinvou3-app/src/features/account-platform/AdminTokenAccountDetailWorkspace.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-navigation.js'),
  ]);
  assert.match(portal, /tokenCredentialIsolationPath/);
  assert.match(portal, /form-action 'self'/);
  assert.match(portal, /r\.URL\.Path == "\/credential-handoff"/);
  assert.match(origin, /prompt", "login"/);
  assert.match(origin, /max_age", "900"/);
  assert.match(origin, /http\.StatusSeeOther/);
  assert.match(origin, /SameSiteLaxMode/);
  assert.match(origin, /credential-submit\.js/);
  assert.match(origin, /fetch\('\/credentials'/);
  assert.match(origin, /credentials:'same-origin'/);
  assert.match(origin, /referrerPolicy:'no-referrer'/);
  assert.match(origin, /session\.Status == "COMPLETED"/);
  assert.match(origin, /http\.Redirect\(w, r, "\/session", http\.StatusSeeOther\)/);
  assert.match(isolation, /item\.Status == "COMPLETED"/);
  assert.match(isolation, /launchStatus != "COMPLETED"/);
  assert.doesNotMatch(origin, /console\.(?:log|error)|sendBeacon/);
  assert.match(isolation, /CREDENTIAL_WINDOW_BLOCKED/);
  assert.match(isolation, /authorization_version/);
  assert.match(isolation, /gate_epoch/);
  assert.match(isolation, /CheckCredentialIsolationSessionAuthority/);
  assert.match(isolation, /heartbeatDeadline\.Before\(heartbeatAt\)/);
  assert.doesNotMatch(isolation, /now\.Sub\(heartbeatAt\)\s*>\s*10\*time\.Second/);
  for (const tab of ['overview', 'credentials', 'routes', 'provisionings', 'audit']) assert.match(ui, new RegExp(`'${tab}'`));
  assert.match(portal, /Referrer-Policy", "origin"/);
  assert.match(handoffHandler, /script-src 'sha256-"\+credentialHandoffScriptSHA256\+"'/);
  assert.match(handoffHandler, /form-action "\+origin/);
  assert.match(handoffHandler, /worker-src 'none'/);
  assert.match(handoffHandler, /Referrer-Policy", "origin"/);
  assert.match(handoffScript, /navigator\.serviceWorker\?\.controller !== null/);
  assert.match(handoffScript, /navigator\.serviceWorker\.getRegistrations/);
  assert.match(handoffScript, /HANDOFF_READY/);
  assert.match(handoffScript, /HANDOFF_BOUND/);
  assert.match(handoffScript, /HANDOFF_SUBMIT/);
  assert.match(handoffScript, /HANDOFF_ACCEPTED/);
  assert.match(handoffScript, /window\.opener = null/);
  assert.doesNotMatch(handoffScript, /localStorage|sessionStorage|document\.cookie|navigator\.sendBeacon|fetch\(/);
  assert.match(generatedHash, /credentialHandoffScriptSHA256 = "[A-Za-z0-9+/]{43}="/);
  assert.match(handoffClient, /windowObject\.open\('\/credential-handoff', '_blank'\)/);
  assert.match(handoffClient, /new MessageChannelClass/);
  assert.match(handoffClient, /CREDENTIAL_HANDOFF_OUTCOME_UNKNOWN/);
  assert.match(originIntegration, /adminportal\.NewService/);
  assert.match(realBrowser, /window\.open\('\/credential-handoff', '_blank'\)/);
  assert.match(realBrowser, /new MessageChannel\(\)/);
  assert.match(realBrowser, /launch_id: opaqueLaunchID/);
  assert.doesNotMatch(realBrowser, /\?launch_id=|target="_blank"|action=.*\/launch/);
  assert.match(ui, /settleCredentialLaunch\(client, current/);
  assert.match(ui, /revocationAttempted/);
  assert.match(ui, /cancelLaunch\('CREDENTIAL_LAUNCH_CANCELLED'\)/);
  assert.match(launchState, /credentialIsolationResult\(launch\.result_ref/);
  assert.doesNotMatch(launchState, /createCredentialIsolationLaunch|openCredentialHandoff/);
  assert.doesNotMatch(ui, /target="_blank"|referrerPolicy=|name="launch_id"/);
  assert.doesNotMatch(ui + handoffClient, /noreferrer/);
  assert.doesNotMatch(ui, /name="secret"|base_url|provider_key_id|ciphertext/);
  assert.match(navigation, /token_account_id/);
  assert.match(navigation, /token_detail_tab/);
});
