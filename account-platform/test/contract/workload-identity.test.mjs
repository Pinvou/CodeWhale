import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');

test('production services use short-lived workload identities and default-deny route grants', async () => {
  const middleware = await readFile(path.join(root, 'internal/platform/serviceauth/middleware.go'), 'utf8');
  const apps = await readFile(path.join(root, 'deploy/helm/account-platform/templates/app-deployments.yaml'), 'utf8');
  const accounts = await readFile(path.join(root, 'deploy/helm/account-platform/templates/serviceaccount.yaml'), 'utf8');
  const keycloak = await readFile(path.join(root, 'deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/MobileDirectGrantAuthenticator.java'), 'utf8');
  assert.match(middleware, /defaultMaximumLifetime = 15 \* time\.Minute/);
  assert.match(middleware, /WORKLOAD_IDENTITY_RULES_JSON/);
  assert.match(middleware, /exact permissions/);
  assert.match(middleware, /func Require\(permission string\)/);
  assert.match(middleware, /environment != "development" && environment != "test"/);
  assert.match(apps, /serviceAccountToken: \{ path: token, audience:/);
  assert.match(apps, /WORKLOAD_IDENTITY_TOKEN_FILE/);
  assert.match(accounts, /"account-service" "identity-service" "billing-service" "token-gateway"/);
  assert.match(keycloak, /Files\.readString\(Path\.of\(tokenFile\)/);
  assert.doesNotMatch(apps, /key: internal-service-token/);
  assert.match(apps, /"permissions" \(list "internal\./);
  assert.doesNotMatch(apps, /"paths" \(list/);
  assert.doesNotMatch(apps, /"methods" \(list/);
});

test('every authenticated internal HTTP route declares one exact workload permission', async () => {
  for (const [relative, group] of [
    ['internal/account/service.go', 'internal'],
    ['internal/identityadapter/service.go', 'internal'],
    ['internal/billing/service.go', 'protected'],
    ['internal/token/service.go', 'internal'],
    ['internal/paymentsim/service.go', 'management'],
  ]) {
    const source = await readFile(path.join(root, relative), 'utf8');
    const routes = source.split(/\r?\n/).filter(line => new RegExp(`\\b${group}\\.(?:GET|POST|PUT|PATCH|DELETE)\\(`).test(line));
    assert.ok(routes.length > 0, `${relative} contains no classified internal routes`);
    for (const route of routes) {
      assert.match(route, /serviceauth\.Require\("internal\.[a-z0-9.-]+"\)/, `${relative}: ${route.trim()}`);
    }
  }
});

test('Token Gateway wallet resolution is granted the exact Billing ensure permission', async () => {
  const [resolver, compose, helm] = await Promise.all([
    readFile(path.join(root, 'internal/token/service.go'), 'utf8'),
    readFile(path.join(root, 'deploy/compose/compose.model-token.yaml'), 'utf8'),
    readFile(path.join(root, 'deploy/helm/account-platform/templates/app-deployments.yaml'), 'utf8'),
  ]);
  assert.match(resolver, /HTTPWalletResolver[\s\S]*http\.MethodPost[\s\S]*\/internal\/v1\/wallets\/ensure[\s\S]*serviceauth\.SetAuthorization/);
  assert.match(compose, /"permissions":\["internal\.billing\.wallet\.ensure"[^\]]*\],"service":"token-gateway"/);
  assert.match(helm, /"service" "token-gateway" "permissions" \(list "internal\.billing\.wallet\.ensure"/);
});

test('each relay has an independent identity and cannot publish another source subject', async () => {
  const relay = await readFile(path.join(root, 'internal/platform/events/relay.go'), 'utf8');
  const main = await readFile(path.join(root, 'cmd/event-relay/main.go'), 'utf8');
  const deployment = await readFile(path.join(root, 'deploy/helm/account-platform/templates/event-relays.yaml'), 'utf8');
  assert.match(relay, /AllowedSubjects/);
  assert.match(main, /AllowedSubjects: \[\]string\{source \+ "\.>"\}/);
  assert.match(main, /"actor_type", "SERVICE"/);
  assert.match(main, /"job_run_id", jobRunID/);
  assert.match(deployment, /serviceAccountName: \{\{ \$fullname \}\}-\{\{ \$source \}\}-event-relay/);
  assert.match(deployment, /NATS_CREDENTIALS_FILE/);
  assert.match(deployment, /%s-event-relay\.creds/);
});
