import test from 'node:test';
import assert from 'node:assert/strict';
import { registrationManualReviewReason } from '../src/features/account-platform/registration-recovery-state.js';
import { accountPlatformCopy } from '../src/shared/account-platform-i18n.js';

test('only explicit same-operation deterministic repair retires automatic registration polling', () => {
  for (const reason of ['USERNAME_ALREADY_REGISTERED', 'MOBILE_ALREADY_REGISTERED']) {
    const receipt = { status: 'REPAIR_REQUIRED', manual_review_required: true, reason };
    assert.equal(registrationManualReviewReason(receipt), reason);
    for (const status of ['IDENTITY_CREATING', 'ACTIVE', 'COMPENSATED']) assert.equal(registrationManualReviewReason({ ...receipt, status }), '');
    assert.equal(registrationManualReviewReason({ ...receipt, manual_review_required: false }), '');
    assert.equal(registrationManualReviewReason({ ...receipt, manual_review_required: 'true' }), '');
  }
  for (const receipt of [null, {}, { status: 'REPAIR_REQUIRED' }, { status: 'REPAIR_REQUIRED', manual_review_required: true, reason: 'IDENTITY_UNAVAILABLE' }]) assert.equal(registrationManualReviewReason(receipt), '');
});

test('manual-repair messages in all three languages do not promise automatic completion', () => {
  for (const language of ['zh', 'en', 'ja']) {
    const copy = accountPlatformCopy[language];
    assert.ok(copy.registrationManualReview?.trim());
    assert.notEqual(copy.registrationManualReview, copy.registrationDoNotRepeat);
    assert.ok(copy.usernameAlreadyRegistered?.trim());
    assert.ok(copy.mobileAlreadyRegistered?.trim());
  }
});
