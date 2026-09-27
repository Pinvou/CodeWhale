import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('password and SMS credentials require strong personal login authorization before Keycloak success', async () => {
  const [client, browser, direct, passwordGate, policy, authorization, registrationSaga, accountView, bffClient, accountService] = await Promise.all([
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PinvouAuthClient.java'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PinvouBrowserAuthenticator.java'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/MobileDirectGrantAuthenticator.java'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PasswordLoginGate.java'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/RegistrationLoginPolicy.java'),
    read('internal/account/authorization.go'),
    read('internal/account/registration_saga.go'),
    read('../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/bff-client.js'),
    read('internal/account/service.go'),
  ]);

  assert.match(client, /\/internal\/v1\/authorization\/check/);
  assert.match(client, /"issuer", issuer[\s\S]*"subject", subject[\s\S]*"permission", "personal\.session\.login"/);
  assert.match(client, /"Authorization", "Bearer " \+ workloadToken\(\)/);
  assert.match(client, /Boolean\.TRUE\.equals\(result\.body\(\)\.get\("allowed"\)\)/);
  assert.match(client, /"PERSONAL"\.equals\(string\(result\.body\(\)\.get\("permission_scope"\)\)\)/);
  assert.match(client, /String userID = string\(result\.body\(\)\.get\("user_id"\)\)/);
  assert.match(client, /return new LoginAuthorization\(allowed, userID\)/);

  for (const authenticator of [browser, direct]) {
    const authorizationGate = authenticator.indexOf('CLIENT.authorizeLogin(user.getId())');
    const success = authenticator.indexOf('context.success()', authorizationGate);
    assert.ok(authorizationGate >= 0 && success > authorizationGate, 'authorization must precede Keycloak success');
  }
  assert.match(browser, /authenticatePassword[\s\S]*PasswordLoginGate\.allows\([\s\S]*credentialManager\(\)\.isValid[\s\S]*CLIENT\.authorizeLogin\(user\.getId\(\)\)[\s\S]*complete\(context, user\)/);
  assert.match(browser, /authenticatePassword[\s\S]*if \(user == null\) AuthenticatorUtils\.dummyHash\(context\);[\s\S]*PasswordLoginGate\.allows/);
  assert.match(passwordGate, /if \(!verifier\.verify\(password\)\) return false;[\s\S]*if \(!userEnabled\.getAsBoolean\(\)\) return false;[\s\S]*if \(bruteForceBlocked\.getAsBoolean\(\)\) return false;[\s\S]*authorizer\.authorize\(\)[\s\S]*authorization\.allowed\(\)/);
  assert.match(browser, /succeed\(AuthenticationFlowContext context, UserModel user\)[\s\S]*authorizeLogin\(user\.getId\(\)\)\.allowed\(\)[\s\S]*complete\(context, user\)/);
  assert.match(direct, /boolean smsLogin[\s\S]*credentialManager\(\)\.isValid[\s\S]*CLIENT\.authorizeLogin\(user\.getId\(\)\)\.allowed\(\)/);
  assert.match(authorization, /roleGranted && permissionAllowedForUserStatus\(decision\.UserStatus, permission\)/);
  assert.match(authorization, /case "PROVISIONING", "REPAIR_REQUIRED":[\s\S]*PermissionPersonalSessionLogin/);
  assert.match(registrationSaga, /ensureDefaultPersonalRole\(ctx, tx, operation\.CandidateUserID, now\)[\s\S]*INSERT INTO identities[\s\S]*status='PROVISIONING'/);
  assert.match(policy, /sameID\(registrationAccountUserID, authorizedAccountUserID\)/);
  assert.match(policy, /&& authorizationAllowed\s*&& keycloakUserPresent/);
  assert.match(accountView, /if \(!session\.authenticated\)[\s\S]*const account = await client\.bootstrap\(\)[\s\S]*setAuth\('ready'\)/);
  assert.match(bffClient, /bootstrap\(signal\)[\s\S]*'\/v1\/account\/bootstrap'[\s\S]*method: 'POST'/);
  assert.match(accountService, /protected\.POST\("\/v1\/account\/bootstrap"[\s\S]*func \(s \*Service\) bootstrap/);
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
  assert.equal(remote.services['account-service'].environment.MOBILE_AUTH_ENABLED, 'true');
  assert.equal(remote.services['identity-service'].environment.MOBILE_AUTH_ENABLED, 'true');
  assert.match(infrastructure, /PINVOU_OIDC_ISSUER[\s\S]*\.Values\.oidc\.issuer/);
  assert.match(deployments, /keycloak-mobile-authenticator[\s\S]*internal\.auth\.sms\.verify[\s\S]*internal\.authorization\.check/);
});
