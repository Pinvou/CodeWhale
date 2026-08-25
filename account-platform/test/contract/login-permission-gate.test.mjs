import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('password and SMS credentials require strong personal login authorization before Keycloak success', async () => {
  const [client, browser, direct, authorization] = await Promise.all([
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PinvouAuthClient.java'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PinvouBrowserAuthenticator.java'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/MobileDirectGrantAuthenticator.java'),
    read('internal/account/authorization.go'),
  ]);

  assert.match(client, /\/internal\/v1\/authorization\/check/);
  assert.match(client, /"issuer", issuer[\s\S]*"subject", subject[\s\S]*"permission", "personal\.session\.login"/);
  assert.match(client, /"Authorization", "Bearer " \+ workloadToken\(\)/);
  assert.match(client, /Boolean\.TRUE\.equals\(result\.body\(\)\.get\("allowed"\)\)/);
  assert.match(client, /"PERSONAL"\.equals\(string\(result\.body\(\)\.get\("permission_scope"\)\)\)/);

  for (const authenticator of [browser, direct]) {
    const authorizationGate = authenticator.indexOf('CLIENT.authorizeLogin(user.getId())');
    const success = authenticator.indexOf('context.success()', authorizationGate);
    assert.ok(authorizationGate >= 0 && success > authorizationGate, 'authorization must precede Keycloak success');
  }
  assert.match(browser, /authenticatePassword[\s\S]*credentialManager\(\)\.isValid[\s\S]*succeed\(context, user\)/);
  assert.match(direct, /boolean smsLogin[\s\S]*credentialManager\(\)\.isValid[\s\S]*CLIENT\.authorizeLogin\(user\.getId\(\)\)/);
  assert.match(authorization, /roleGranted && permissionAllowedForUserStatus\(decision\.UserStatus, permission\)/);
});

test('Keycloak has an exact issuer and least-privilege Account authorization route in every deployment', async () => {
  const [composeSource, remoteSource, infrastructure, deployments] = await Promise.all([
    read('deploy/compose/compose.yaml'),
    read('deploy/compose/compose.remote.yaml'),
    read('deploy/helm/account-platform/templates/infrastructure.yaml'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
  ]);
  const compose = YAML.parse(composeSource);
  const remote = YAML.parse(remoteSource);
  assert.equal(compose.services.keycloak.environment.PINVOU_OIDC_ISSUER, 'http://127.0.0.1:18080/realms/pinvou');
  assert.equal(remote.services.keycloak.environment.PINVOU_OIDC_ISSUER, '${PUBLIC_BASE_URL:?set PUBLIC_BASE_URL}/realms/pinvou');
  assert.match(infrastructure, /PINVOU_OIDC_ISSUER[\s\S]*\.Values\.oidc\.issuer/);
  assert.match(deployments, /keycloak-mobile-authenticator[\s\S]*\/internal\/v1\/auth\/sms\/verify[\s\S]*\/internal\/v1\/authorization\/check/);
});
