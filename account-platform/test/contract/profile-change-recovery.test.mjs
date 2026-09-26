import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const migration = (name) => readFile(path.join(root, 'migrations/account', name), 'utf8');
const source = (name) => readFile(path.join(root, 'internal/account', name), 'utf8');

test('profile-change supersession uses a validated forward-only status migration', async () => {
  const [expand, validate, validateDown, activate, activateDown] = await Promise.all([
    migration('000081_expand_profile_change_statuses.up.sql'),
    migration('000082_validate_profile_change_statuses.up.sql'),
    migration('000082_validate_profile_change_statuses.down.sql'),
    migration('000083_activate_profile_change_statuses.up.sql'),
    migration('000083_activate_profile_change_statuses.down.sql'),
  ]);

  assert.match(expand, /profile_change_requests_status_v2_check/);
  assert.match(expand, /'SUPERSEDED'/);
  assert.match(expand, /NOT VALID/);
  assert.match(validate, /VALIDATE CONSTRAINT profile_change_requests_status_v2_check/);
  assert.match(validateDown, /DROP CONSTRAINT profile_change_requests_status_v2_check/);
  assert.match(validateDown, /ADD CONSTRAINT profile_change_requests_status_v2_check[\s\S]*NOT VALID/);
  assert.match(activate, /AND convalidated/);
  assert.match(activate, /DROP CONSTRAINT profile_change_requests_status_check/);
  assert.match(activate, /RENAME CONSTRAINT profile_change_requests_status_v2_check/);
  assert.match(activateDown, /ACCOUNT_PROFILE_CHANGE_SUPERSEDED_FORWARD_ONLY/);
  assert.match(activateDown, /ERRCODE = '55000'/);
});

test('profile SMS delivery lease is persistent and account closure treats SUPERSEDED as terminal', async () => {
  const [leaseUp, leaseDown, profileChanges, closureCleanup] = await Promise.all([
    migration('000084_profile_sms_delivery_lease.up.sql'),
    migration('000084_profile_sms_delivery_lease.down.sql'),
    source('profile_changes.go'),
    source('account_closure_cleanup.go'),
  ]);

  assert.match(leaseUp, /delivery_lease_id UUID/);
  assert.match(leaseUp, /delivery_lease_until TIMESTAMPTZ/);
  assert.match(leaseUp, /delivery_started_at TIMESTAMPTZ/);
  assert.match(leaseUp, /delivery_uncertain_at TIMESTAMPTZ/);
  assert.match(leaseUp, /sms_challenges_delivery_lease_pair_check/);
  assert.match(leaseDown, /DROP COLUMN delivery_lease_until/);
  assert.match(profileChanges, /challenge\.delivery_lease_id=\$3/);
  assert.equal((profileChanges.match(/request\.status='PENDING' AND request\.sms_challenge_id=challenge\.id/g) || []).length, 6);
  assert.match(profileChanges, /func \(r \*PostgresRepository\) CommitProfileSMSVerification[\s\S]*status='CONSUMED'[\s\S]*status='VERIFIED'/);
  assert.match(profileChanges, /status IN \('VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'\)/);
  assert.doesNotMatch(profileChanges, /ScheduleProfileChangeRetry[\s\S]{0,1000}status<>'COMPLETED'/);
  assert.equal((closureCleanup.match(/'COMPLETED','SUPERSEDED','CANCELLED_ACCOUNT_CLOSURE'/g) || []).length, 2);
});
