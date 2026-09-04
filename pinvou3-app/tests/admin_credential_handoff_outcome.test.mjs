import assert from 'node:assert/strict';
import test from 'node:test';

import { openCredentialHandoff } from '../src/features/account-platform/admin-credential-handoff.js';
import { settleCredentialLaunch } from '../src/features/account-platform/admin-credential-launch-state.js';

function harness({ dropAccepted = false, throwSubmit = false } = {}) {
  const listeners = new Set();
  const popup = {
    closed: false,
    close() { this.closed = true; },
    postMessage(message, origin, transfer) {
      const remote = transfer[0];
      remote.onmessage = ({ data }) => {
        if (throwSubmit) throw new Error('not submitted');
        if (!dropAccepted) remote.postMessage({ version: 1, type: 'HANDOFF_ACCEPTED', nonce: data.nonce });
      };
      queueMicrotask(() => remote.postMessage({ version: 1, type: 'HANDOFF_BOUND', nonce: message.nonce }));
    },
  };
  class Port {
    close() { this.closed = true; }
    start() {}
    postMessage(data) {
      if (this.closed) throw new Error('closed');
      this.peer?.onmessage?.({ data });
    }
  }
  class Channel {
    constructor() {
      this.port1 = new Port(); this.port2 = new Port();
      this.port1.peer = this.port2; this.port2.peer = this.port1;
    }
  }
  const windowObject = {
    location: { origin: 'https://account.pinvou.com' },
    open: () => popup,
    addEventListener: (_type, listener) => listeners.add(listener),
    removeEventListener: (_type, listener) => listeners.delete(listener),
    setTimeout, clearTimeout, setInterval, clearInterval,
  };
  const cryptoObject = { getRandomValues(value) { value.fill(7); return value; } };
  const ready = openCredentialHandoff({ windowObject, cryptoObject, MessageChannelClass: Channel, timeoutMs: 20 });
  queueMicrotask(() => { for (const listener of listeners) listener({ source: popup, origin: windowObject.location.origin, data: { version: 1, type: 'HANDOFF_READY' } }); });
  return { ready };
}

test('a lost HANDOFF_ACCEPTED is an unknown submitted outcome', async () => {
  const handoff = await harness({ dropAccepted: true }).ready;
  await assert.rejects(handoff.submit('0199a200-0001-7000-8000-000000000201'), (error) => error.code === 'CREDENTIAL_HANDOFF_OUTCOME_UNKNOWN' && error.outcomeUnknown === true);
});

test('a synchronous MessagePort rejection is proven not submitted', async () => {
  const handoff = await harness({ throwSubmit: true }).ready;
  await assert.rejects(handoff.submit('0199a200-0001-7000-8000-000000000201'), (error) => error.code === 'CREDENTIAL_HANDOFF_INVALID' && error.outcomeUnknown === false);
});

test('a lost revoke response is recovered only through the immutable result reference', async () => {
  const calls = [];
  const launch = { result_ref: '0199a200-0001-7000-8000-000000000202' };
  const expected = { result_ref: launch.result_ref, status: 'REVOKED' };
  const client = {
    async revokeCredentialWindow(reference, reason) { calls.push(['revoke', reference, reason]); throw Object.assign(new Error('lost response'), { status: 504 }); },
    async credentialIsolationResult(reference) { calls.push(['result', reference]); return expected; },
    async createCredentialLaunch() { throw new Error('recovery must never create a launch'); },
  };
  assert.equal(await settleCredentialLaunch(client, launch, 'CREDENTIAL_LAUNCH_CANCELLED', (value) => value === expected), expected);
  assert.deepEqual(calls, [['revoke', launch.result_ref, 'CREDENTIAL_LAUNCH_CANCELLED'], ['result', launch.result_ref]]);
});

test('subsequent unknown recovery only queries after the fixed revoke attempt', async () => {
  const calls = [];
  const launch = { result_ref: '0199a200-0001-7000-8000-000000000203' };
  const pending = { result_ref: launch.result_ref, status: 'PENDING' };
  const terminal = { result_ref: launch.result_ref, status: 'REVOKED' };
  let result = pending;
  const client = {
    async revokeCredentialWindow(reference, reason) { calls.push(['revoke', reference, reason]); throw Object.assign(new Error('lost response'), { status: 504 }); },
    async credentialIsolationResult(reference) { calls.push(['result', reference]); return result; },
    async createCredentialLaunch() { throw new Error('recovery must never create a launch'); },
  };
  assert.equal(await settleCredentialLaunch(client, launch, 'CREDENTIAL_LAUNCH_CANCELLED', () => false), null);
  result = terminal;
  assert.equal(await settleCredentialLaunch(client, launch, 'CREDENTIAL_LAUNCH_CANCELLED', (value) => value === terminal, undefined, false), terminal);
  assert.deepEqual(calls, [
    ['revoke', launch.result_ref, 'CREDENTIAL_LAUNCH_CANCELLED'],
    ['result', launch.result_ref],
    ['result', launch.result_ref],
  ]);
});
