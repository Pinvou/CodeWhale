import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { desktopBridgeApi, desktopOnlyBridgeDomains, expectedWebBridgeApi } from './bridge_domain_contract.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = relative => fs.readFileSync(path.join(root, relative), 'utf8');
const fragment = read('src/platform/tauri/bridge/account-platform.js');
const id = '11111111-1111-4111-8111-111111111111';
const methods = [
  ['session', 'account_platform_session', [], undefined],
  ['openPortal', 'account_platform_open_portal', [{ language: 'ja' }], { input: { language: 'ja' } }],
  ['login', 'account_platform_login', [{ username: 'public-fixture-user', password: 'PUBLIC_SYNTHETIC_PASSWORD' }], { credentials: { username: 'public-fixture-user', password: 'PUBLIC_SYNTHETIC_PASSWORD' } }],
  ['createSliderCaptcha', 'account_platform_create_slider_captcha', [{ mobile: '13800000000', purpose: 'REGISTER' }], { input: { mobile: '13800000000', purpose: 'REGISTER' } }],
  ['precheckMobileRegistration', 'account_platform_precheck_mobile_registration', [{ mobile: '13800000000', username: 'public-fixture-user' }], { input: { mobile: '13800000000', username: 'public-fixture-user' } }],
  ['sendSms', 'account_platform_send_sms', [{ mobile: '13800000000', captcha_id: id, captcha_offset: 12, purpose: 'REGISTER' }], { input: { mobile: '13800000000', captcha_id: id, captcha_offset: 12, purpose: 'REGISTER' } }],
  ['registerMobile', 'account_platform_register_mobile', [{ mobile: '13800000000', challenge_id: id, code: '000000', password_handle: 'PUBLIC_SYNTHETIC_HANDLE' }], { input: { mobile: '13800000000', challenge_id: id, code: '000000', password_handle: 'PUBLIC_SYNTHETIC_HANDLE' } }],
  ['registrationStatus', 'account_platform_registration_status', [], undefined],
  ['loginSms', 'account_platform_login_sms', [{ mobile: '13800000000', challenge_id: id, code: '000000' }], { input: { mobile: '13800000000', challenge_id: id, code: '000000' } }],
  ['resetPassword', 'account_platform_reset_password', [{ mobile: '13800000000', challenge_id: id, code: '000000', password_handle: 'PUBLIC_SYNTHETIC_HANDLE' }], { input: { mobile: '13800000000', challenge_id: id, code: '000000', password_handle: 'PUBLIC_SYNTHETIC_HANDLE' } }],
  ['logout', 'account_platform_logout', [], undefined],
  ['modelCatalog', 'account_platform_model_catalog', [], undefined],
  ['selectCatalogModel', 'account_platform_select_catalog_model', [{ modelId: id }], { input: { modelId: id } }],
  ['selectSessionCatalogModel', 'account_platform_select_session_model', [{ sessionId: 'public-session', modelId: id }], { input: { sessionId: 'public-session', modelId: id } }],
  ['branchReplacementSession', 'account_platform_branch_replacement_session', [{ sourceSessionId: 'public-session', replacementModelId: id }], { input: { sourceSessionId: 'public-session', replacementModelId: id } }],
  ['request', 'account_platform_request', [{ service: 'account', method: 'GET', path: '/v1/me' }], { request: { service: 'account', method: 'GET', path: '/v1/me' } }],
];
function productionFeature(invoke) {
  const window = {};
  vm.runInNewContext(fragment, { window }, { filename: 'bridge/account-platform.js' });
  return window.__PINVOU_TAURI_BRIDGE_FEATURES__['account-platform']({ invoke });
}
for (const [method, command, args, expected] of methods) {
  test(`personal bridge ${method} dispatches actual command and complete argument envelope`, async () => {
    const calls = []; const receipt = { public_receipt: method };
    const feature = productionFeature(async (...call) => { calls.push(call); return receipt; });
    assert.equal(await feature[method](...args), receipt);
    assert.equal(calls.length, 1); assert.equal(calls[0][0], command);
    assert.deepEqual(calls[0][1] === undefined ? undefined : JSON.parse(JSON.stringify(calls[0][1])), expected);
    const originalError = new Error('PUBLIC_SYNTHETIC_NATIVE_FAILURE');
    const failing = productionFeature(async () => { throw originalError; });
    await assert.rejects(failing[method](...args), error => error === originalError);
  });
}
test('personal/model bridge stays finite and desktop-only after mature LLMap retirement', () => {
  const feature = productionFeature(async () => null);
  assert.deepEqual(Object.keys(feature).sort(), desktopBridgeApi.accountPlatform.toSorted());
  assert.equal(desktopOnlyBridgeDomains.includes('accountPlatform'), true);
  assert.equal(expectedWebBridgeApi().accountPlatform, undefined);
  assert.equal(desktopBridgeApi.llmapi, undefined);
  const main = read('src/app/main.jsx');
  assert.doesNotMatch(main, /getLlmApiStatus|isLlmApiAccountVisible|llmAccountVisible/);
  assert.match(main, /shouldShowModelAccessGate/);
  assert.match(main, /openSettingsSection\('model'\)/);
  assert.match(read('src/platform/tauri/bridge.js'), /switchModelRef:\s*settingsFeature\.switchModelRef/);
});

test('actual host visibility expression requires semantic platform support and installed Native bridge', () => {
  const main = read('src/app/main.jsx');
  const expressions = [...main.matchAll(/const personalAccountAvailable = ([^;]+);/g)];
  assert.equal(expressions.length, 1);
  for (const [allowed, supported, client, expected] of [[true,true,{},true],[false,true,{},false],[true,false,{},false],[true,true,null,false],[true,undefined,{},false]]) {
    const actual = vm.runInNewContext(expressions[0][1], { can: capability => { assert.equal(capability, 'accountPlatform'); return allowed; }, platformCapabilities: { accountPlatformSupported: supported }, personalAccountClient: client });
    assert.equal(actual, expected);
  }
  const capabilitiesSource = read('src/shared/platform.js');
  for (const [window, expected] of [[{},true],[{ PinvouPlatform: { kind: 'web', capabilities: {} } },false],[{ PinvouPlatform: { kind: 'web', capabilities: { accountPlatform: true }, can: () => false } },false]]) {
    const executable = capabilitiesSource.replace(/export /g, '') + '\ncan("accountPlatform")';
    assert.equal(vm.runInNewContext(executable, { window }), expected);
  }
  assert.match(read('src-tauri/src/app/commands/runtime.rs'), /account_platform_supported:\s*crate::features::account_platform::account_platform_enabled\(\s*\)/);
});
test('index loads the production personal fragment before bridge installation and retains original domains', () => {
  const index = read('src/index.html');
  const fragmentAt = index.indexOf('%BASE_URL%platform/tauri/bridge/account-platform.js');
  const bridgeAt = index.indexOf('%BASE_URL%platform/tauri/bridge.js');
  assert.ok(fragmentAt >= 0 && fragmentAt < bridgeAt);
  assert.match(index, /platform\/tauri\/bridge\/chat\.js/);
  assert.match(index, /platform\/tauri\/bridge\/settings\.js/);
  assert.match(read('src/platform/tauri/bridge.js'), /accountPlatform:\s*accountPlatformFeature/);
});
const require = createRequire(import.meta.url);
const { composeEffectiveConfig, buildResourceManifest } = require('../scripts/tauri/effective-config.js');
for (const [platform, overlay] of [['win32','windows'],['darwin','macos'],['linux','linux']]) {
  test(`actual ${overlay} effective resource manifest installs account configuration without losing OS resources`, () => {
    const configPath = `src-tauri/config/platforms/${overlay}/tauri.conf.json`;
    const original = JSON.parse(read(configPath)).bundle.resources;
    const { effectiveConfig } = composeEffectiveConfig([configPath]);
    const resources = effectiveConfig.bundle.resources;
    const source = 'resources/common/config/account-platform.json';
    assert.equal(resources[source], 'config/account-platform.json');
    for (const [name, destination] of Object.entries(original)) assert.equal(resources[name], destination);
    const manifest = buildResourceManifest(effectiveConfig, { platform });
    const installed = manifest.files.filter(file => file.destination === 'config/account-platform.json');
    assert.equal(installed.length, 1); assert.equal(installed[0].source, source); assert.equal(installed[0].kind, 'file');
    assert.equal(installed[0].bytes, fs.statSync(path.join(root, 'src-tauri', source)).size);
  });
}
