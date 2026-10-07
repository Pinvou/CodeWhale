import test from 'node:test';
import assert from 'node:assert/strict';
import { requireSession, requirePersonalAccount, requireProfileChange } from '../src/features/account-platform/personal-contract.js';
import { parseBalanceJSON, formatBalance } from '../src/features/account-platform/balance-view.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { consumePortalSessionHandoff } from '../src/features/account-platform/portal-session-handoff.js';
import { takePortalSessionHandoffTicket } from '../src/features/account-platform/portal-session-handoff-url.js';
import { accountPlatformCopy, adminPlatformCopy } from '../src/shared/i18n.js';
import { localizedAccountPlatformError } from '../src/features/account-platform/error-localization.js';

const id = 'a7f21df2-d2ef-4d55-99c8-a5470f9968b9';
const json = (payload, status = 200) => new Response(status === 204 ? null : JSON.stringify(payload), { status, headers: { 'Content-Type': 'application/json' } });
const response = (bodyText, body = { available_microcredits: 42, reserved_microcredits: 0 }) => ({ status: 200, body, bodyText });

for (const [amount, expected] of [['9223372036854775807','922,337,203,685,477.5807'],['-9223372036854775808','-922,337,203,685,477.5808'],['0','0'],['-10001','-1.0001']]) {
  test(`balance preserves int64 ${amount}`, () => {
    const result = parseBalanceJSON(`{"available_microcredits":${amount},"reserved_microcredits":0}`);
    assert.equal(result.available_microcredits, amount);
    assert.equal(formatBalance(result.available_microcredits, 'en'), expected);
  });
}
test('balance lexer leaves similar field names in strings and nested unknown fields alone', () => {
  const text = '{"available_microcredits":9007199254740993,"reserved_microcredits":0,"message":"\\\"available_microcredits\\\":999","nested":{"available_microcredits":7},"extension":["reserved_microcredits",true]}';
  const result = parseBalanceJSON(text);
  assert.equal(result.available_microcredits, '9007199254740993');
  assert.equal(result.nested.available_microcredits, 7);
  assert.deepEqual(result.extension, ['reserved_microcredits', true]);
});
for (const text of ['{}','[]','null','{"available_microcredits":1}', '{"available_microcredits":1,"reserved_microcredits":0,"available_microcredits":2}', '{"available_microcredits":1,"reserved_microcredits":0,"availab\\u006ce_microcredits":2}', '{"available_microcredits":"1","reserved_microcredits":0}', '{"available_microcredits":9223372036854775808,"reserved_microcredits":0}', '{"available_microcredits":-9223372036854775809,"reserved_microcredits":0}', '{"available_microcredits":1e3,"reserved_microcredits":0}', '{"available_microcredits":1.5,"reserved_microcredits":0}', '{"available_microcredits":1,"reserved_microcredits":0}junk']) {
  test(`invalid balance rejects ${text}`, () => assert.throws(() => parseBalanceJSON(text)));
}
test('Desktop balance prefers exact raw bytes over an already rounded body', async () => {
  const client = new AccountPlatformDesktopClient({ request: async () => response('{"available_microcredits":9007199254740993,"reserved_microcredits":0}', { available_microcredits: 9007199254740992, reserved_microcredits: 0 }) });
  assert.equal((await client.balance()).available_microcredits, '9007199254740993');
});
test('Desktop balance refuses unsafe parsed money when raw body is unavailable', async () => {
  const client = new AccountPlatformDesktopClient({ request: async () => response(undefined, { available_microcredits: 9007199254740992, reserved_microcredits: 0 }) });
  await assert.rejects(client.balance(), /INVALID_BALANCE/);
});
test('Desktop balance permits exact safe numeric responses', async () => {
  const client = new AccountPlatformDesktopClient({ request: async () => response(undefined) });
  assert.equal((await client.balance()).available_microcredits, '42');
});
test('BFF balance reads text rather than rounded response.json', async () => {
  const previous = globalThis.fetch;
  globalThis.fetch = async () => new Response('{"available_microcredits":-9223372036854775808,"reserved_microcredits":0}', { status: 200 });
  try { assert.equal((await new AccountPlatformBffClient().balance()).available_microcredits, '-9223372036854775808'); } finally { globalThis.fetch = previous; }
});
test('profile risk requests carry one client device ID, not a caller authority header', async () => {
  const previous = globalThis.fetch; const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return json(url.endsWith('/actions/complete') ? { profile_change_request_id: id, status: 'COMPLETED' } : { status: 'SENT' }); };
  try {
    const client = new AccountPlatformBffClient(); client.csrfToken = 'csrf';
    await client.sendProfileMobileChallenge(id, id, 5);
    await client.completeProfileChange(id, '000000');
    assert.equal(calls.length, 2); assert.ok(calls[0].options.headers['X-Device-ID']);
    assert.equal(calls[0].options.headers['X-Device-ID'], calls[1].options.headers['X-Device-ID']);
    assert.equal(calls[0].options.headers['X-Pinvou-Client-Source'], undefined);
    assert.equal(calls[0].options.headers['X-CSRF-Token'], 'csrf');
  } finally { globalThis.fetch = previous; }
});
test('admin exposes only ACTIVE/DISABLED user status and accepts the actual 204 ACK', async () => {
  const previous = globalThis.fetch; const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return json(null, 204); };
  try {
    const client = new AdminPlatformClient(); client.csrfToken = 'csrf';
    assert.equal(client.deleteUser, undefined);
    assert.equal(await client.setUserStatus(id, 'DISABLED'), null);
    assert.equal(await client.setUserStatus(id, 'ACTIVE'), null);
    await assert.rejects(client.setUserStatus(id, 'DELETED'));
    assert.deepEqual(calls.map(call => JSON.parse(call.options.body).status), ['DISABLED', 'ACTIVE']);
    assert.ok(calls.every(call => call.options.method === 'PATCH'));
  } finally { globalThis.fetch = previous; }
});
test('admin retries consume exact 202 resource receipts and compensation 204', async () => {
  const previous = globalThis.fetch;
  globalThis.fetch = async url => url.endsWith('/compensate') ? json(null, 204) : url.includes('registration-operations') ? json({ registration_operation_id: id, status: 'IDENTITY_CREATING' }, 202) : json({ request_id: id, resource_type: 'profile_change', action: 'RETRY_SCHEDULED', status: 'RETRY' }, 202);
  try {
    const client = new AdminPlatformClient();
    assert.equal((await client.retryRegistration(id)).registration_operation_id, id);
    assert.equal((await client.retryProfileChange(id)).request_id, id);
    assert.equal(await client.compensateRegistration(id), null);
    globalThis.fetch = async () => json({ request_id: id, status: 'RETRY', action: 'RETRY_SCHEDULED', resource_type: 'profile_change' });
    await assert.rejects(client.retryProfileChange(id), /INVALID_MUTATION_ACK/);
    globalThis.fetch = async () => json({ registration_operation_id: 'foreign', status: 'ACTIVE' }, 202);
    await assert.rejects(client.retryRegistration(id), /INVALID_MUTATION_ACK/);
  } finally { globalThis.fetch = previous; }
});
test('handoff clears ticket/locale from history before its only request', async () => {
  const events = []; const ticket = 'one-time-control-ticket';
  const taken = takePortalSessionHandoffTicket(`https://portal.example/portal-session-handoff.html?locale=ja#ticket=${ticket}`, (_state, _title, path) => events.push(['clear', path]));
  assert.equal(await consumePortalSessionHandoff(taken, async (url, options) => { events.push(['send', url, options]); return json({ redirect_to: '/' }); }), '/');
  assert.equal(events[0][0], 'clear'); assert.equal(events[0][1], '/portal-session-handoff.html'); assert.equal(events.length, 2);
  assert.equal(events[1][2].credentials, 'same-origin'); assert.equal(JSON.parse(events[1][2].body).ticket, ticket);
});
test('handoff invalid/failed/untrusted redirect cannot navigate or automatically retry', async () => {
  let calls = 0;
  await assert.rejects(consumePortalSessionHandoff('', async () => { calls++; })); assert.equal(calls, 0);
  await assert.rejects(consumePortalSessionHandoff('control-ticket-123', async () => { calls++; return json({ redirect_to: 'https://elsewhere.example' }); })); assert.equal(calls, 1);
  await assert.rejects(consumePortalSessionHandoff('control-ticket-123', async () => { calls++; return json({ code: 'SESSION_HANDOFF_UNAVAILABLE' }, 503); }), /SESSION_HANDOFF_UNAVAILABLE/); assert.equal(calls, 2);
});
test('all installed languages expose personal, repair, SMS and safe handoff messages', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(accountPlatformCopy[language].sessionHandoff.invalid);
    assert.ok(accountPlatformCopy[language].showPassword);
    assert.ok(adminPlatformCopy[language].userManagement.profileRepair);
    assert.ok(adminPlatformCopy[language].settings.sms.templateLabels.PASSWORD_RESET);
    assert.equal(localizedAccountPlatformError(accountPlatformCopy[language], new Error('private http://internal.example?secret=control')), accountPlatformCopy[language].requestFailed);
  }
});

test('personal authority and profile receipts reject missing facts without stripping extensions', () => {
  assert.throws(() => requireSession({})); assert.deepEqual(requireSession({ authenticated: false, extension: true }), { authenticated: false, extension: true });
  const me = { user_id: id, status: 'ACTIVE', personal_permissions: [], platform_permissions: [], extension: true };
  assert.equal(requirePersonalAccount(me), me); assert.throws(() => requirePersonalAccount({ ...me, user_id: 'foreign' })); assert.throws(() => requirePersonalAccount({ ...me, personal_permissions: null }));
  const profile = { profile_change_request_id: id, field: 'USERNAME', status: 'PENDING', extension: true };
  assert.equal(requireProfileChange(profile, id, 'USERNAME'), profile); assert.throws(() => requireProfileChange({ ...profile, profile_change_request_id: 'foreign' }, id)); assert.throws(() => requireProfileChange({ ...profile, field: 'MOBILE' }, '', 'USERNAME')); assert.throws(() => requireProfileChange({ ...profile, status: '' }));
});
