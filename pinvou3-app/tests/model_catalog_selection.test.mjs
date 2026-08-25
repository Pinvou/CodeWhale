import test from 'node:test';
import assert from 'node:assert/strict';

import {
  ACCOUNT_PLATFORM_SOURCE,
  LOCAL_CONFIG_SOURCE,
  composerPlatformModels,
  platformModelSelectableForNewSession,
  sameModelSelectionRef,
} from '../src/features/account-platform/model-catalog-selection.js';

test('platform model selection follows context and new-session lifecycle cutoffs', () => {
  const now = Date.parse('2026-08-25T08:00:00Z');
  assert.equal(platformModelSelectableForNewSession({ context_eligible: true, lifecycle_status: 'ACTIVE' }, now), true);
  assert.equal(platformModelSelectableForNewSession({ context_eligible: false, lifecycle_status: 'ACTIVE' }, now), false);
  assert.equal(platformModelSelectableForNewSession({ context_eligible: true, lifecycle_status: 'REMOVED' }, now), false);
  assert.equal(platformModelSelectableForNewSession({
    context_eligible: true,
    lifecycle_status: 'DEPRECATED',
    new_session_cutoff_at: '2026-08-25T08:00:01Z',
  }, now), true);
  assert.equal(platformModelSelectableForNewSession({
    context_eligible: true,
    lifecycle_status: 'DEPRECATED',
    new_session_cutoff_at: '2026-08-25T08:00:00Z',
  }, now), false);
  assert.equal(platformModelSelectableForNewSession({ context_eligible: true, lifecycle_status: 'DEPRECATED' }, now), false);
});

test('composer lists context-visible models while selection references remain source-aware', () => {
  const visible = composerPlatformModels([
    { model_id: 'active', context_eligible: true, lifecycle_status: 'ACTIVE' },
    { model_id: 'deprecated', context_eligible: true, lifecycle_status: 'DEPRECATED' },
    { model_id: 'removed', context_eligible: true, lifecycle_status: 'REMOVED' },
    { model_id: 'denied', context_eligible: false, lifecycle_status: 'ACTIVE' },
  ]);
  assert.deepEqual(visible.map(item => item.model_id), ['active', 'deprecated']);
  assert.equal(sameModelSelectionRef(
    { source: ACCOUNT_PLATFORM_SOURCE, model_id: 'active' },
    { source: ACCOUNT_PLATFORM_SOURCE, model_id: 'active' },
  ), true);
  assert.equal(sameModelSelectionRef(
    { source: ACCOUNT_PLATFORM_SOURCE, model_id: 'active' },
    { source: LOCAL_CONFIG_SOURCE, saved_model_id: 'active' },
  ), false);
  assert.equal(sameModelSelectionRef(
    { source: LOCAL_CONFIG_SOURCE, saved_model_id: 'local-a' },
    { source: LOCAL_CONFIG_SOURCE, saved_model_id: 'local-b' },
  ), false);
});
