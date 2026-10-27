import assert from 'node:assert/strict';
import test from 'node:test';
import { assertDeferredStartupUpdates } from '../vite.config.mjs';

function bundle() {
  return {
    main: { type: 'chunk', name: 'main', fileName: 'main.js', isEntry: true, modules: {}, imports: ['shared.js'], dynamicImports: ['reminder.js'] },
    shared: { type: 'chunk', name: 'shared', fileName: 'shared.js', modules: {}, imports: [] },
    reminder: { type: 'chunk', name: 'reminder', fileName: 'reminder.js', modules: {
      '/repo/src/features/model-store/startup-update-runtime.js': {},
      'C:\\repo\\src\\features\\model-store\\startup-update-read-state.js': {},
    }, imports: ['shared.js'] },
  };
}

test('the reminder gate permits a real dynamic boundary and shared dependencies', () => {
  assert.doesNotThrow(() => assertDeferredStartupUpdates(bundle()));
});

test('static extraction cannot disguise an eager reminder as a lazy chunk', () => {
  const outputs = bundle();
  outputs.shared.imports.push('reminder.js');
  outputs.reminder.imports.push('main.js'); // Traversal must also terminate on cycles.
  assert.throws(() => assertDeferredStartupUpdates(outputs), /outside the main static dependency graph/u);
});

test('missing, duplicated and entry reminder modules fail closed', () => {
  const missing = bundle();
  delete missing.reminder.modules['/repo/src/features/model-store/startup-update-runtime.js'];
  assert.throws(() => assertDeferredStartupUpdates(missing), /startup-update-runtime/u);
  const duplicated = bundle();
  duplicated.shared.modules = { ...duplicated.reminder.modules };
  assert.throws(() => assertDeferredStartupUpdates(duplicated), /must be emitted once/u);
  const entry = bundle();
  entry.reminder.isEntry = true;
  assert.throws(() => assertDeferredStartupUpdates(entry), /must be emitted once/u);
  assert.throws(() => assertDeferredStartupUpdates({}), /did not find the main entry/u);
});
