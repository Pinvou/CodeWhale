import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';

import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { invitationCreateBody, invitationSubmissionAttempt } from '../src/features/account-platform/invitation-client.js';
import { clearInvitationAcceptanceRetries, invitationAcceptanceMayBeCommitted, invitationRecoveryScope, readInvitationAcceptanceRetries, writeInvitationAcceptanceRetries } from '../src/features/account-platform/invitation-recovery.js';
import { PendingInvitations } from '../src/features/account-platform/pending-invitations.js';
import { accountPlatformCopy } from '../src/shared/account-platform-i18n.js';

test('invitation request adapter preserves legacy email calls and emits typed username or phone targets', () => {
  assert.deepEqual(invitationCreateBody('legacy@example.com', ['MEMBER']), { email: 'legacy@example.com', roles: ['MEMBER'], expires_in_hours: 72 });
  assert.deepEqual(invitationCreateBody({ type: 'USERNAME', value: 'login_name' }, ['MEMBER']), { target_type: 'USERNAME', target: 'login_name', roles: ['MEMBER'], expires_in_hours: 72 });
  assert.deepEqual(invitationCreateBody({ target_type: 'PHONE', target: '+8613800138000' }, ['MEMBER']), { target_type: 'PHONE', target: '+8613800138000', roles: ['MEMBER'], expires_in_hours: 72 });

  let sequence = 0;
  const createKey = () => `key-${++sequence}`;
  const first = invitationSubmissionAttempt(null, 'org-1', 'USERNAME', 'login_name', createKey);
  assert.equal(invitationSubmissionAttempt(first, 'org-1', 'USERNAME', 'login_name', createKey), first, 'retrying unchanged input must retain the same key');
  assert.equal(sequence, 1);
  const changed = invitationSubmissionAttempt(first, 'org-1', 'PHONE', '+8613800138000', createKey);
  assert.equal(changed.key, 'key-2', 'an explicitly changed invitation must receive a new key');
});

test('BFF invitation client sends explicit idempotency and never retries an acceptance failure', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AccountPlatformBffClient('/portal');
    client.csrfToken = 'c'.repeat(32);
    await client.invite('org/id', { type: 'USERNAME', value: 'login_name' }, ['MEMBER'], 'invite-key');
    await client.invite('org/id', 'legacy@example.com', ['MEMBER']);
    await client.myInvitations();
    await client.acceptInvitation('invitation-id');
    assert.equal(calls[0].url, '/portal/api/account/v1/organizations/org/id/invitations');
    assert.equal(calls[0].options.headers['Idempotency-Key'], 'invite-key');
    assert.deepEqual(JSON.parse(calls[0].options.body), { target_type: 'USERNAME', target: 'login_name', roles: ['MEMBER'], expires_in_hours: 72 });
    assert.deepEqual(JSON.parse(calls[1].options.body), { email: 'legacy@example.com', roles: ['MEMBER'], expires_in_hours: 72 });
    assert.equal(calls[2].url, '/portal/api/account/v1/me/invitations');
    assert.deepEqual(JSON.parse(calls[3].options.body), { invitation_id: 'invitation-id' });

    calls.length = 0;
    globalThis.fetch = async (url, options = {}) => {
      calls.push({ url, options });
      return new Response(JSON.stringify({ code: 'ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE', message: 'retry explicitly' }), { status: 503, headers: { 'Content-Type': 'application/json' } });
    };
    await assert.rejects(client.acceptInvitation('same-invitation-id'), (error) => error.status === 503);
    assert.equal(calls.length, 1);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('desktop invitation client forwards typed target, explicit key, list, and invitation-id acceptance', async () => {
  const calls = [];
  const client = new AccountPlatformDesktopClient({
    request: async (request) => {
      calls.push(request);
      return { status: 200, body: { items: [] } };
    },
  });
  await client.invite('org-1', { type: 'PHONE', value: '+8613800138000' }, ['MEMBER'], 'desktop-key');
  await client.invite('org-1', 'legacy@example.com', ['MEMBER']);
  await client.myInvitations();
  await client.acceptInvitation('invitation-id');
  assert.deepEqual(calls[0], { area: 'account', resource: '/v1/organizations/org-1/invitations', method: 'POST', body: { target_type: 'PHONE', target: '+8613800138000', roles: ['MEMBER'], expires_in_hours: 72 }, idempotencyKey: 'desktop-key' });
  assert.deepEqual(calls[1].body, { email: 'legacy@example.com', roles: ['MEMBER'], expires_in_hours: 72 });
  assert.equal(calls[2].resource, '/v1/me/invitations');
  assert.deepEqual(calls[3].body, { invitation_id: 'invitation-id' });
});

test('unknown acceptance recovery is scoped to the immutable user and stores only invitation ids', () => {
  const values = new Map();
  const storage = { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) };
  const accountA = { user_id: 'user-a' };
  const accountB = { user_id: 'user-b' };
  const invitationId = '01994d3a-0b0c-7000-8000-000000000001';
  writeInvitationAcceptanceRetries(accountA, [{ invitation_id: invitationId, organization_name: 'Private Org', target: '+8613800138000', token: 'secret' }], storage);
  assert.equal(values.get(invitationRecoveryScope(accountA)), `["${invitationId}"]`);
  assert.deepEqual(readInvitationAcceptanceRetries(accountA, storage), [{ invitation_id: invitationId }]);
  assert.deepEqual(readInvitationAcceptanceRetries(accountB, storage), []);
  assert.equal(invitationAcceptanceMayBeCommitted({ code: 'ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE', status: 503 }), true);
  assert.equal(invitationAcceptanceMayBeCommitted({ status: 502 }), true);
  assert.equal(invitationAcceptanceMayBeCommitted(new TypeError('network failed')), true);
  for (const error of [{ status: 403 }, { status: 404 }, { status: 409 }, { status: 410, code: 'INVITATION_EXPIRED' }]) assert.equal(invitationAcceptanceMayBeCommitted(error), false);

  values.set(invitationRecoveryScope(accountA), JSON.stringify(['not-an-id', ...Array.from({ length: 30 }, (_, index) => `01994d3a-0b0c-7000-8000-${String(index).padStart(12, '0')}`)]));
  assert.equal(readInvitationAcceptanceRetries(accountA, storage).length, 20);
  clearInvitationAcceptanceRetries(accountA, storage);
  assert.deepEqual(readInvitationAcceptanceRetries(accountA, storage), []);
});

test('pending invitation view renders all locales without target or token disclosure', async () => {
  for (const language of ['zh', 'en', 'ja']) {
    const html = renderToStaticMarkup(React.createElement(PendingInvitations, {
      copy: accountPlatformCopy[language], language, unavailable: false, busy: false, canAccept: true, onAccept: () => {},
      items: [{ invitation_id: 'invite-1', organization_id: 'org-1', organization_name: 'Example Org', roles: ['MEMBER'], expires_at: '2026-10-01T00:00:00Z', target: 'hidden-user', token: 'hidden-token' }, { invitation_id: 'retry-only' }],
    }));
    assert.match(html, /Example Org/);
    assert.match(html, new RegExp(accountPlatformCopy[language].acceptInvitation));
    assert.match(html, new RegExp(accountPlatformCopy[language].invitationAcceptanceRecovery));
    assert.doesNotMatch(html, /hidden-user|hidden-token/);
  }
});

test('organization invitation UI defaults to username and clears only after a successful result', async () => {
  const source = await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8');
  assert.match(source, /useState\('USERNAME'\)/);
  assert.match(source, /<option value="USERNAME">/);
  assert.match(source, /<option value="PHONE">/);
  assert.doesNotMatch(source, /<option value="EMAIL">/);
  assert.match(source, /const created = await onInvite[\s\S]*if \(!created\) return;[\s\S]*setInvitationTarget\(''\)/);
  assert.match(source, /surfaceAccess\.invitationsRead \? client\.myInvitations/);
  assert.doesNotMatch(source, /invite\.token|invitation\.token|target_hint/);
});
