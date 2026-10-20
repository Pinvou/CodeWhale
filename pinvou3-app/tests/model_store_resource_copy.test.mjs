import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { renderToString } from 'react-dom/server';
import { loadJsx } from './fixtures/jsx-module-hooks.mjs';
import { dict } from './helpers/i18n-all.js';

const { ResourceManagerDialog } = await loadJsx(
  '../src/features/model-store/ResourceManagerDialog.jsx', import.meta.url,
);

for (const [language, label] of Object.entries({ zh: '存储', en: 'Storage', ja: 'ストレージ' })) {
  test(`${language} 资源管理存储入口保留现有文案且无重复键`, () => {
    const source = readFileSync(new URL(`../src/shared/i18n/${language}.js`, import.meta.url), 'utf8');
    assert.equal([...source.matchAll(/\bstorageButton\s*:/g)].length, 1);
    const copy = dict[language].uiModelStore;
    assert.equal(copy.storageButton, label);
    const noop = () => {};
    const html = renderToString(ResourceManagerDialog({
      open: true, copy, theme: 'light', onClose: noop,
      onOpenEngines: noop, onOpenLocalResources: noop, onOpenStorage: noop,
    }));
    const button = /<button\b[^>]*data-testid="model-store-resource-storage"[^>]*>([\s\S]*?)<\/button>/.exec(html);
    assert.ok(button, '资源管理缺少存储入口');
    assert.ok(button[1].includes(`>${label}</div>`));
  });
}
