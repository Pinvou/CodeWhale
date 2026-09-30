import assert from 'node:assert/strict';
import test from 'node:test';

import {
  canUseControllerTemporaryModel,
  clearResolvedRemoteModelRequiredError,
  modelSourceForLease,
  normalizeRemoteModelSource,
  remoteModelSelectionError,
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

test('a new capable connection defaults to the controller model source', () => {
  const capable = { temporaryModelAvailable: true };
  assert.equal(
    modelSourceForLease('controller_temporary', capable, 'connection-1:1', 'connection-1:1'),
    'controller_temporary',
  );
  assert.equal(
    modelSourceForLease('controller_temporary', capable, 'connection-1:1', 'connection-2:1'),
    'controller_temporary',
  );
  assert.equal(
    modelSourceForLease('controlled_device', capable, 'connection-1:1', 'connection-1:2'),
    'controller_temporary',
  );
  assert.equal(
    modelSourceForLease('controlled_device', capable, 'connection-1:1:controlled', 'connection-1:1:temporary'),
    'controller_temporary',
  );
  assert.equal(
    modelSourceForLease('controller_temporary', { temporaryModelAvailable: false }, 'connection-1:1', 'connection-2:1'),
    'controlled_device',
  );
});

test('controller model source requires a selected model at send time', () => {
  assert.equal(remoteModelSelectionError('controller_temporary', null), 'remote_model_required');
  assert.equal(remoteModelSelectionError('controller_temporary', ''), 'remote_model_required');
  assert.equal(remoteModelSelectionError('controller_temporary', 'model-1'), null);
  assert.equal(remoteModelSelectionError('controlled_device', null), null);
});

test('an asynchronously resolved default clears only its own required-model error', () => {
  const required = { identity: 'lease-1:revision-1', code: 'remote_model_required' };
  assert.equal(clearResolvedRemoteModelRequiredError(required, required.identity, null), null);
  assert.equal(
    clearResolvedRemoteModelRequiredError(required, required.identity, 'remote_model_required'),
    required,
  );
  assert.equal(
    clearResolvedRemoteModelRequiredError(required, 'lease-1:revision-2', null),
    required,
    'a late model list cannot clear an error owned by another send identity',
  );
  const unrelated = { identity: required.identity, code: 'remote_task_intent_invalid' };
  assert.equal(
    clearResolvedRemoteModelRequiredError(unrelated, required.identity, null),
    unrelated,
    'model readiness cannot overwrite another failure',
  );
});
