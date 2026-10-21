import assert from 'node:assert/strict';
import path from 'node:path';
import test from 'node:test';
import { matchesViteModule } from './helpers/vite-fixture-module.mjs';

test('settings form fixture injection matches POSIX and Windows Vite module IDs', () => {
  const suffix = 'src/features/settings/SettingsView.jsx';
  for (const root of ['/tmp/pinvou3-app', 'C:/workspace/pinvou3-app']) {
    const viteId = `${root}/${suffix}`;
    const filename = root.startsWith('C:')
      ? path.win32.join(root, suffix) : path.posix.join(root, suffix);
    assert.equal(matchesViteModule(viteId, filename), true);
    assert.equal(matchesViteModule(`${viteId}?v=fixture`, filename), true);
    assert.equal(matchesViteModule(filename, viteId), true);
    assert.equal(matchesViteModule(`${root}/src/features/settings/Other.jsx`, filename), false);
    assert.equal(matchesViteModule(`${viteId}.backup`, filename), false);
  }
});
