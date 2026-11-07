import assert from 'node:assert/strict';
import test from 'node:test';
import { dict } from './helpers/i18n-all.js';
import { relayError } from '../src/features/team-relay/team-relay-errors.js';

test('enrollment configuration failures have a distinct message in every language, including older Hubs', () => {
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].uiTeamRelay;
    const expected = copy.errorMessages.self_service_enrollment_disabled;
    assert.ok(expected);
    assert.notEqual(expected, copy.errorMessages.action_not_allowed);
    for (const kind of ['account', 'team']) {
      const legacy = `action_not_allowed: Self-service ${kind} enrollment is disabled on this Hub. (request req_test)`;
      assert.equal(relayError(copy, legacy), expected);
      assert.equal(relayError(copy, legacy.replace(' (request req_test)', '')), expected);
    }
    assert.equal(relayError(copy, 'action_not_allowed: Changed server wording (request req_test) [details={"reason":"self_service_enrollment_disabled"}]'), expected);
    assert.equal(relayError(copy, 'action_not_allowed: A transfer is pending. (request req_test)'), copy.errorMessages.action_not_allowed);
    assert.equal(relayError(copy, 'action_not_allowed: Other error [details={broken}]'), copy.errorMessages.action_not_allowed);
    assert.equal(relayError(copy, 'hub_unreachable: Offline'), copy.errorMessages.hub_unreachable);
    assert.equal(relayError(copy, 'unknown_error: Diagnostic (request req_test) [details={"reason":"other"}]'), 'unknown_error: Diagnostic (request req_test)');
  }
});

test('work blocking details retain their formatting', () => {
  const copy = {
    errorMessages: { member_has_open_work: 'Work remains' },
    blockingThread: item => item.title,
    blockingWork: (message, titles) => `${message}: ${titles.join(', ')}`,
  };
  assert.equal(relayError(copy, 'member_has_open_work: Blocked [details={"blocking_threads":[{"title":"Task A"},{"title":"Task B"}]}]'), 'Work remains: Task A, Task B');
  assert.equal(relayError(copy, 'member_has_open_work: Blocked [details={"blocking_threads":[null]}]'), 'Work remains');
});
