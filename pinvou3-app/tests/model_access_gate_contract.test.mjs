import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { Linter } from 'eslint';

const root = path.resolve(import.meta.dirname, '..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');

test('model access gate is an opaque full-window choice with the requested action hierarchy', async () => {
  const [main, gate, account, isolation] = await Promise.all([
    read('src/app/main.jsx'),
    read('src/features/settings/ModelAccessGate.jsx'),
    read('src/features/account-platform/AccountPlatformPopover.jsx'),
    read('src/hooks/useModalIsolation.js'),
  ]);

  assert.match(main, /data-testid="model-access-layer"/);
  assert.match(main, /background: activeTheme === 'dark' \? '#111416' : '#F7FAF9'/);
  assert.doesNotMatch(main, /model-access-layer[\s\S]{0,240}rgba\(/);
  assert.match(gate, /data-testid="model-access-login"[\s\S]*onClick=\{onLogin\}/);
  assert.match(gate, /data-testid="model-access-external"[\s\S]*onClick=\{onUseExternalModels\}/);
  assert.ok(gate.indexOf('model-access-login') < gate.indexOf('model-access-external'));
  assert.match(main, /presentation="screen"/);
  assert.match(account, /presentation === 'screen'/);
  assert.match(main, /z-\[2147483647\]/);
  assert.match(main, /useModalIsolation\(modelAccessGateOpen && browserOverlayPublicationReady, modelAccessLayerRef\)/);
  assert.match(isolation, /element\.setAttribute\('aria-hidden', 'true'\)/);
  assert.match(isolation, /element\.setAttribute\('inert', ''\)/);
  assert.match(isolation, /event\.key !== 'Tab'/);
  assert.match(isolation, /previouslyFocused\.focus/);
  assert.match(main, /onKeyDown=\{\(event\) => event\.stopPropagation\(\)\}/);
  assert.match(account, /if \(event\.key !== 'Escape'\) return;[\s\S]{0,100}event\.stopPropagation\(\)/);
  assert.match(main, /onUseExternalModels=\{\(\) => \{[\s\S]{0,180}openSettingsSection\('model'\)/);
});

test('successful authentication refreshes both model snapshots before the gate can dismiss', async () => {
  const { refreshModelAccessAfterAuthentication } = await import('../src/features/settings/model-access.js');
  const calls = [];
  const state = {
    modelsLoaded: true,
    savedModels: [],
    effectiveModelConfig: { credential_state: 'missing', requires_user_api_key: true },
  };
  const refreshed = await refreshModelAccessAfterAuthentication({
    async loadModels() {
      calls.push('models');
      state.savedModels = [{ id: 'platform-default' }];
      return true;
    },
    async loadEffectiveModelConfig() {
      calls.push('effective');
      state.effectiveModelConfig = { model: 'platform-model', credential_state: 'available' };
      return true;
    },
  });

  assert.equal(refreshed, true);
  assert.deepEqual(calls, ['models', 'effective']);
  assert.equal(state.savedModels.length, 1);
  assert.equal(state.effectiveModelConfig.credential_state, 'available');

  const failedCalls = [];
  const failedRefresh = await refreshModelAccessAfterAuthentication({
    async loadModels() {
      failedCalls.push('models');
      return false;
    },
    async loadEffectiveModelConfig() {
      failedCalls.push('effective');
      return false;
    },
  });
  assert.deepEqual(failedCalls, ['models', 'effective']);
  assert.equal(failedRefresh, false);

  const [main, account] = await Promise.all([
    read('src/app/main.jsx'),
    read('src/features/account-platform/AccountPlatformPopover.jsx'),
  ]);
  assert.match(main, /onAuthenticated=\{refreshModelAccess\}/);
  assert.match(account, /await refreshAuthenticatedModelAccess\(\);[\s\S]{0,120}if \(!isCurrent\(\)\) return;/);
  assert.match(account, /if \(loginMode !== 'forgot'\) await loadAccount\(\);/);
  const submitLogin = account.slice(account.indexOf('const submitLogin'), account.indexOf('const sendSms'));
  assert.doesNotMatch(submitLogin, /setError\(copy\.requestFailed\)/);
});

test('authenticated screen refresh is single-flight per open and retries after reopen', async () => {
  const { createAuthenticatedRefreshCoordinator } = await import('../src/features/account-platform/authenticated-refresh.js');
  const coordinator = createAuthenticatedRefreshCoordinator();
  let calls = 0;
  let finishFirst;
  coordinator.reset();
  const first = coordinator.run(() => {
    calls += 1;
    return new Promise((resolve) => { finishFirst = resolve; });
  });
  const duplicate = coordinator.run(() => {
    calls += 1;
    return true;
  });
  assert.equal(calls, 0, 'refresh starts in a microtask');
  await Promise.resolve();
  assert.equal(calls, 1, 'existing-session and submit paths must share one refresh');
  finishFirst(false);
  assert.equal(await first, false);
  assert.equal(await duplicate, false);

  coordinator.reset();
  assert.equal(await coordinator.run(async () => {
    calls += 1;
    return true;
  }), true);
  assert.equal(calls, 2, 'reopening after a failed refresh must allow one recovery attempt');
});

test('modal background isolation covers every ancestor sibling and restores exact state', async () => {
  const originalHTMLElement = globalThis.HTMLElement;
  const originalMutationObserver = globalThis.MutationObserver;
  const originalDocument = globalThis.document;
  const observers = [];

  class FakeElement {
    constructor(name) {
      this.name = name;
      this.children = [];
      this.parentElement = null;
      this.attributes = new Map();
      this.inert = false;
    }
    append(...children) {
      children.forEach((child) => {
        child.parentElement = this;
        this.children.push(child);
      });
    }
    contains(target) {
      return this === target || this.children.some((child) => child.contains(target));
    }
    getAttribute(name) { return this.attributes.has(name) ? this.attributes.get(name) : null; }
    hasAttribute(name) { return this.attributes.has(name); }
    setAttribute(name, value) { this.attributes.set(name, String(value)); }
    removeAttribute(name) { this.attributes.delete(name); }
  }
  class FakeMutationObserver {
    constructor(callback) {
      this.callback = callback;
      observers.push(this);
    }
    observe() {}
    disconnect() { this.disconnected = true; }
  }

  try {
    globalThis.HTMLElement = FakeElement;
    globalThis.MutationObserver = FakeMutationObserver;
    const body = new FakeElement('body');
    globalThis.document = { body };
    const rootHost = new FakeElement('root-host');
    const portal = new FakeElement('portal');
    const appRoot = new FakeElement('app-root');
    const titleBar = new FakeElement('title-bar');
    const gateBranch = new FakeElement('gate-branch');
    const mainSurface = new FakeElement('main-surface');
    const gate = new FakeElement('gate');
    titleBar.setAttribute('aria-hidden', 'false');
    body.append(rootHost, portal);
    rootHost.append(appRoot);
    appRoot.append(titleBar, gateBranch);
    gateBranch.append(mainSurface, gate);

    const { isolateModalBackground } = await import('../src/hooks/useModalIsolation.js');
    const restore = isolateModalBackground(gate);
    for (const background of [mainSurface, titleBar, portal]) {
      assert.equal(background.inert, true, `${background.name} must be inert`);
      assert.equal(background.getAttribute('aria-hidden'), 'true');
    }
    for (const activeBranch of [gate, gateBranch, appRoot, rootHost]) {
      assert.equal(activeBranch.inert, false, `${activeBranch.name} must remain interactive`);
    }

    const lateSibling = new FakeElement('late-sibling');
    appRoot.append(lateSibling);
    observers[0].callback();
    assert.equal(lateSibling.inert, true, 'late-mounted ancestor siblings must also be isolated');

    restore();
    assert.equal(mainSurface.inert, false);
    assert.equal(mainSurface.hasAttribute('aria-hidden'), false);
    assert.equal(titleBar.getAttribute('aria-hidden'), 'false');
    assert.equal(portal.inert, false);
    assert.equal(lateSibling.hasAttribute('inert'), false);
    assert.equal(observers[0].disconnected, true);
  } finally {
    globalThis.HTMLElement = originalHTMLElement;
    globalThis.MutationObserver = originalMutationObserver;
    globalThis.document = originalDocument;
  }
});

test('modal isolation only reads the publication barrier after its declaration', async () => {
  const main = await read('src/app/main.jsx');
  const linter = new Linter();
  const messages = linter.verify(main, [{
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    plugins: {
      modelGate: {
        rules: {
          'publication-before-isolation': {
            create(context) {
              let publicationDeclared = false;
              return {
                VariableDeclarator(node) {
                  if (node.id?.name === 'browserOverlayPublicationReady') publicationDeclared = true;
                },
                CallExpression(node) {
                  if (node.callee?.name === 'useModalIsolation' && !publicationDeclared) {
                    context.report({
                      node,
                      message: 'useModalIsolation reads browserOverlayPublicationReady before declaration',
                    });
                  }
                },
              };
            },
          },
        },
      },
    },
    rules: { 'modelGate/publication-before-isolation': 'error' },
  }], { disableInlineConfig: true });
  const relevantMessages = messages.filter((message) => (
    message.fatal || message.ruleId === 'modelGate/publication-before-isolation'
  ));
  assert.deepEqual(relevantMessages, []);
});

test('model access copy is complete in Chinese, English, and Japanese', async () => {
  const [zh, en, ja] = await Promise.all([
    read('src/shared/i18n/zh.js'),
    read('src/shared/i18n/en.js'),
    read('src/shared/i18n/ja.js'),
  ]);
  for (const source of [zh, en, ja]) {
    for (const key of ['modelAccessTitle', 'modelAccessDesc', 'modelAccessLogin', 'modelAccessExternal']) {
      assert.match(source, new RegExp(`${key}:\\s*'[^']+'`));
    }
  }
  assert.match(zh, /modelAccessLogin: '登录'/);
  assert.match(zh, /modelAccessExternal: '使用本地\/第三方模型'/);
});
