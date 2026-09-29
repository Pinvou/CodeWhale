import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');

test('model access gate is an opaque full-window choice with the requested action hierarchy', async () => {
  const [main, gate, account] = await Promise.all([
    read('src/app/main.jsx'),
    read('src/features/settings/ModelAccessGate.jsx'),
    read('src/features/account-platform/AccountPlatformPopover.jsx'),
  ]);

  assert.match(main, /data-testid="model-access-layer"/);
  assert.match(main, /background: activeTheme === 'dark' \? '#111416' : '#F7FAF9'/);
  assert.doesNotMatch(main, /model-access-layer[\s\S]{0,240}rgba\(/);
  assert.match(gate, /data-testid="model-access-login"[\s\S]*onClick=\{onLogin\}/);
  assert.match(gate, /data-testid="model-access-external"[\s\S]*onClick=\{onUseExternalModels\}/);
  assert.ok(gate.indexOf('model-access-login') < gate.indexOf('model-access-external'));
  assert.match(main, /presentation="screen"/);
  assert.match(account, /presentation === 'screen'/);
  assert.match(main, /onUseExternalModels=\{\(\) => \{[\s\S]{0,180}openSettingsSection\('model'\)/);
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
