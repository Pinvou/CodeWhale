import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

function messageKeys(source) {
  return new Set(source.split(/\r?\n/).filter(line => line && !line.startsWith('#')).map(line => line.split('=', 1)[0]));
}

test('web login is rendered by the Identity boundary and Portal redirects guests into OIDC', async () => {
  const [authenticator, template, portal] = await Promise.all([
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/PinvouBrowserAuthenticator.java'),
    read('deploy/keycloak/themes/pinvou/login/pinvou-login.ftl'),
    read('../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
  ]);
  assert.match(portal, /portal && auth === 'guest'[\s\S]*location\.replace\('\/auth\/login'\)/);
  assert.match(authenticator, /CLIENT\.precheckPassword\(password\)/);
  assert.match(authenticator, /CLIENT\.createCaptcha\(mobile, purpose\(mode\)\)/);
  assert.match(authenticator, /CLIENT\.sendSMS\(/);
  assert.match(authenticator, /CLIENT\.verifySMS\(/);
  assert.match(authenticator, /CLIENT\.register\(/);
  assert.match(authenticator, /!"COMMITTED"\.equals\(status\)/);
  assert.match(authenticator, /result\.get\("user_id"\)/);
  assert.match(authenticator, /put\(context, "password_handle", handle\)/);
  assert.doesNotMatch(authenticator, /put\(context, "password",/);
  assert.match(template, /name="mode"/);
  assert.match(template, /data-mode="password"/);
  assert.match(template, /data-mode="sms"/);
  assert.match(template, /data-mode="register"/);
  assert.match(template, /name="sms_code"/);
  assert.match(template, /name="captcha_offset"/);
  assert.doesNotMatch(template, /type="hidden"[^>]+name="password"/);
});

test('Keycloak browser flow keeps cookie SSO and requires the custom Identity image assets', async () => {
  const [realmSource, dockerfile, composeSource, helm] = await Promise.all([
    read('deploy/keycloak/pinvou-dev-realm.json'),
    read('deploy/keycloak/Dockerfile'),
    read('deploy/compose/compose.yaml'),
    read('deploy/helm/account-platform/templates/infrastructure.yaml'),
  ]);
  const realm = JSON.parse(realmSource);
  const browser = realm.authenticationFlows.find(flow => flow.alias === 'pinvou-browser');
  assert.equal(realm.browserFlow, 'pinvou-browser');
  assert.equal(realm.loginTheme, 'pinvou');
  assert.ok(browser.authenticationExecutions.some(item => item.authenticator === 'auth-cookie' && item.requirement === 'ALTERNATIVE'));
  assert.ok(browser.authenticationExecutions.some(item => item.authenticator === 'pinvou-browser-authenticator' && item.requirement === 'ALTERNATIVE'));
  assert.match(dockerfile, /COPY themes\/pinvou \/opt\/keycloak\/themes\/pinvou/);
  const compose = YAML.parse(composeSource);
  assert.equal(compose.services.keycloak.environment.PINVOU_ACCOUNT_AUTH_URL, 'http://account-service:8080');
  assert.equal(compose.services.keycloak.environment.PINVOU_IDENTITY_AUTH_URL, 'http://identity-service:8080');
  assert.match(helm, /PINVOU_ACCOUNT_AUTH_URL/);
  assert.match(helm, /PINVOU_IDENTITY_AUTH_URL/);
});

test('the Identity-hosted login theme has exact Simplified Chinese, English, and Japanese coverage', async () => {
  const [zh, en, ja, theme] = await Promise.all([
    read('deploy/keycloak/themes/pinvou/login/messages/messages_zh_CN.properties'),
    read('deploy/keycloak/themes/pinvou/login/messages/messages_en.properties'),
    read('deploy/keycloak/themes/pinvou/login/messages/messages_ja.properties'),
    read('deploy/keycloak/themes/pinvou/login/theme.properties'),
  ]);
  const expected = [...messageKeys(zh)].sort();
  assert.deepEqual([...messageKeys(en)].sort(), expected);
  assert.deepEqual([...messageKeys(ja)].sort(), expected);
  assert.match(theme, /locales=zh-CN,en,ja/);
});
