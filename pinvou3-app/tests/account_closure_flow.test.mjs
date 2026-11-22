import assert from 'node:assert/strict';
import test from 'node:test';

import {
  accountClosureBlockerLabel,
  accountClosurePresentation,
  accountClosureQueryStatus,
  accountClosureResumeAction,
  queryAccountClosure,
  submitAccountClosure,
} from '../src/features/account-platform/account-closure-flow.js';
import { accountClosureCopy } from '../src/shared/account-closure-i18n.js';

function domainError(status, code = '') {
  return Object.assign(new Error(code || `HTTP ${status}`), { status, code });
}

test('account closure 401 starts recent-authentication recovery with the exact return location', async () => {
  const calls = { create: 0, reauthenticate: [] };
  const client = {
    async createAccountClosure() {
      calls.create += 1;
      throw domainError(401, 'RECENT_AUTHENTICATION_REQUIRED');
    },
    reauthenticate(returnTo) {
      calls.reauthenticate.push(returnTo);
    },
  };

  const result = await submitAccountClosure({
    client,
    confirmation: 'CLOSE_MY_ACCOUNT',
    key: 'account-closure-key',
    returnTo: '/?section=accountSettings&closure=confirm',
  });

  assert.equal(result.status, 'reauthenticating');
  assert.equal(calls.create, 1);
  assert.deepEqual(calls.reauthenticate, ['/?section=accountSettings&closure=confirm']);
});

test('account closure resume prompts once only after an explicit 404', async () => {
  const lookup = await queryAccountClosure({
    async accountClosure() {
      throw domainError(404);
    },
  });
  assert.deepEqual(lookup, { status: accountClosureQueryStatus.ready, closure: null });

  const first = accountClosureResumeAction({
    requestedClosure: 'confirm',
    queryStatus: lookup.status,
    closure: lookup.closure,
    resumed: false,
  });
  const second = accountClosureResumeAction({
    requestedClosure: 'confirm',
    queryStatus: lookup.status,
    closure: lookup.closure,
    resumed: first.clearMarker,
  });
  assert.deepEqual(first, { clearMarker: true, prompt: true });
  assert.deepEqual(second, { clearMarker: false, prompt: false });
});

test('account closure resume clears the marker without prompting when a closure exists', async () => {
  const closure = { closure_request_id: 'closure-1', stage: 'CLOSURE_REQUESTED' };
  const lookup = await queryAccountClosure({ async accountClosure() { return closure; } });
  const action = accountClosureResumeAction({
    requestedClosure: 'confirm',
    queryStatus: lookup.status,
    closure: lookup.closure,
    resumed: false,
  });

  assert.deepEqual(lookup, { status: accountClosureQueryStatus.ready, closure });
  assert.deepEqual(action, { clearMarker: true, prompt: false });
});

test('account closure lookup failures retain the resume marker and cannot prompt or create', async () => {
  const failures = [
    domainError(401, 'UNAUTHENTICATED'),
    domainError(403, 'ACCOUNT_FORBIDDEN'),
    domainError(503, 'ACCOUNT_CLOSURE_UNAVAILABLE'),
    new TypeError('network unavailable'),
    Object.assign(new Error('aborted'), { name: 'AbortError' }),
  ];

  for (const failure of failures) {
    let creates = 0;
    const lookup = await queryAccountClosure({ async accountClosure() { throw failure; } });
    const action = accountClosureResumeAction({
      requestedClosure: 'confirm',
      queryStatus: lookup.status,
      closure: lookup.closure,
      resumed: false,
    });
    if (action.prompt) {
      await submitAccountClosure({
        client: { async createAccountClosure() { creates += 1; } },
        confirmation: 'CLOSE_MY_ACCOUNT',
        key: 'account-closure-key',
        returnTo: '/',
      });
    }

    assert.equal(lookup.status, failure.name === 'AbortError' ? accountClosureQueryStatus.loading : accountClosureQueryStatus.error);
    assert.deepEqual(action, { clearMarker: false, prompt: false });
    assert.equal(creates, 0);
  }
});

test('account closure confirmation mismatch does not call the create endpoint', async () => {
  let creates = 0;
  const result = await submitAccountClosure({
    client: { async createAccountClosure() { creates += 1; } },
    confirmation: 'CLOSE_MY_ORGANIZATION',
    key: 'account-closure-key',
    returnTo: '/',
  });

  assert.equal(result.status, 'confirmation-mismatch');
  assert.equal(creates, 0);
});

test('account closure presentation permits a request only after an authoritative empty result', () => {
  assert.deepEqual(
    accountClosurePresentation({ queryStatus: accountClosureQueryStatus.loading, closure: null }),
    { state: accountClosureQueryStatus.loading, canRequest: false, canRetry: false },
  );
  assert.deepEqual(
    accountClosurePresentation({ queryStatus: accountClosureQueryStatus.error, closure: null }),
    { state: accountClosureQueryStatus.error, canRequest: false, canRetry: true },
  );
  assert.deepEqual(
    accountClosurePresentation({ closure: null }),
    { state: accountClosureQueryStatus.ready, canRequest: true, canRetry: false },
  );
  assert.deepEqual(
    accountClosurePresentation({ queryStatus: accountClosureQueryStatus.error, closure: { closure_request_id: 'closure-1' } }),
    { state: 'existing', canRequest: false, canRetry: true },
  );
});

test('account closure blocker labels preserve known translations and unknown code/action fallbacks', () => {
  for (const copy of Object.values(accountClosureCopy)) {
    assert.equal(accountClosureBlockerLabel({ code: 'PERSONAL_BALANCE_NONZERO', user_action: 'CLEAR_PERSONAL_BALANCE', domain: 'BILLING' }, copy),
      `${copy.blockerCodes.PERSONAL_BALANCE_NONZERO} — ${copy.actionCodes.CLEAR_PERSONAL_BALANCE}`);
    assert.equal(accountClosureBlockerLabel({ code: 'UNKNOWN_CODE', user_action: 'UNKNOWN_ACTION', domain: 'TOKEN' }, copy),
      'UNKNOWN_CODE — UNKNOWN_ACTION');
    for (const action of ['', null, undefined]) {
      assert.equal(accountClosureBlockerLabel({ code: 'UNKNOWN_CODE', user_action: action, domain: 'TOKEN' }, copy),
        'UNKNOWN_CODE — TOKEN');
    }
  }
});

test('account closure blocker labels never render inherited prototype members', () => {
  for (const copy of Object.values(accountClosureCopy)) {
    for (const code of ['constructor', 'toString', '__proto__']) {
      assert.equal(accountClosureBlockerLabel({ code, user_action: 'CLEAR_PERSONAL_BALANCE', domain: 'BILLING' }, copy),
        `${code} — ${copy.actionCodes.CLEAR_PERSONAL_BALANCE}`);
      assert.equal(accountClosureBlockerLabel({ code: 'PERSONAL_BALANCE_NONZERO', user_action: code, domain: 'BILLING' }, copy),
        `${copy.blockerCodes.PERSONAL_BALANCE_NONZERO} — ${code}`);
      assert.equal(accountClosureBlockerLabel({ code, user_action: code, domain: 'BILLING' }, copy), `${code} — ${code}`);
      assert.equal(accountClosureBlockerLabel({ code, user_action: '', domain: 'BILLING' }, copy), `${code} — BILLING`);
    }
  }
});

test('account closure blocker labels work without Object.hasOwn', () => {
  const descriptor = Object.getOwnPropertyDescriptor(Object, 'hasOwn');
  try {
    Object.defineProperty(Object, 'hasOwn', { configurable: true, writable: true, value: undefined });
    assert.equal(accountClosureBlockerLabel({ code: '__proto__', user_action: 'constructor', domain: 'ACCOUNT' }, accountClosureCopy.en),
      '__proto__ — constructor');
    assert.equal(accountClosureBlockerLabel({ code: 'PERSONAL_BALANCE_NONZERO', user_action: '', domain: 'BILLING' }, accountClosureCopy.en),
      `${accountClosureCopy.en.blockerCodes.PERSONAL_BALANCE_NONZERO} — BILLING`);
  } finally {
    if (descriptor) Object.defineProperty(Object, 'hasOwn', descriptor);
    else delete Object.hasOwn;
  }
});
