import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToString } from 'react-dom/server';
import { loadJsx } from './fixtures/jsx-module-hooks.mjs';
import { startCardScripts } from '../src/features/model-store/model-store-api.js';
const { CardScriptsActions } = await loadJsx('../src/features/model-store/CardScriptsActions.jsx', import.meta.url);
const { dict } = await import('./helpers/i18n-all.js');

test('script changes require explicit boolean confirmation and rollback choice', async () => {
  const previous = globalThis.__TAURI__;
  const calls = [];
  globalThis.__TAURI__ = { core: { invoke: async (name, args) => { calls.push([name, args]); return {}; } } };
  try {
    for (const value of [undefined, null, false, 'true', 'false', {}, true]) {
      await startCardScripts('card', value, value);
      assert.deepEqual(calls.at(-1), ['model_store_start_card_scripts', { cardId: 'card', rollback: value === true, confirmed: value === true }]);
    }
  } finally {
    if (previous === undefined) delete globalThis.__TAURI__;
    else globalThis.__TAURI__ = previous;
  }
});

for (const language of ['zh', 'en', 'ja']) {
  const copy = dict[language].uiModelStore;
  for (const [running, busy] of [[false, false], [true, false], [false, true]]) {
    test(`${language}: script update actions respect running=${running} busy=${busy}`, () => {
      const html = renderToString(React.createElement(CardScriptsActions, { record: { card_id: 'card', running }, copy, busy, onAction() {} }));
      assert.ok(html.includes(copy.cardScriptsUpdate));
      assert.ok(html.includes(copy.cardScriptsRollback));
      assert.ok(html.includes(running ? copy.cardScriptsStopFirst : copy.cardScriptsHint));
      assert.equal((html.match(/disabled=""/g) || []).length, running || busy ? 2 : 0);
      assert.doesNotMatch(html, /undefined|NaN/);
    });
  }
}
