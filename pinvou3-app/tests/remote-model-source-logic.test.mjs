import assert from 'node:assert/strict';
import test from 'node:test';

import {
  canUseControllerTemporaryModel,
  modelSourceForLease,
  normalizeRemoteModelSource,
} from '../src/features/remote-control/remote-model-source.mjs';

test('temporary model selection fails closed when the lease did not negotiate it', () => {
  for (const connection of [undefined, {}, { temporaryModelAvailable: false }]) {
    assert.equal(canUseControllerTemporaryModel(connection), false);
    assert.equal(normalizeRemoteModelSource('controller_temporary', connection), 'controlled_device');
  }
});

test('temporary model selection is preserved only for a capable lease', () => {
  const connection = { temporaryModelAvailable: true };
  assert.equal(canUseControllerTemporaryModel(connection), true);
  assert.equal(normalizeRemoteModelSource('controller_temporary', connection), 'controller_temporary');
  assert.equal(normalizeRemoteModelSource('unexpected', connection), 'controlled_device');
});

test('a new connection or lease always resets the model source', () => {
  const capable = { temporaryModelAvailable: true };
  assert.equal(
    modelSourceForLease('controller_temporary', capable, 'connection-1:1', 'connection-1:1'),
    'controller_temporary',
  );
  assert.equal(
    modelSourceForLease('controller_temporary', capable, 'connection-1:1', 'connection-2:1'),
    'controlled_device',
  );
  assert.equal(
    modelSourceForLease('controller_temporary', capable, 'connection-1:1', 'connection-1:2'),
    'controlled_device',
  );
});
