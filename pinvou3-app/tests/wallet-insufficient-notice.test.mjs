import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

import { canOpenPersonalRecharge, walletRechargeTarget } from '../src/features/conversation/billing-notice.js';
import {
  createRechargeForTarget,
  loadRechargeTarget,
  rechargeBalanceForTarget,
  rechargeOrderForTarget,
  rechargeTargetAllowed,
} from '../src/features/account-platform/recharge-target.js';
import { pairDeepSeekTimeline } from '../src/features/conversation/deepseek-conversation.js';
import {
  applyNativeChatEvent,
  createNativeLane,
  hydrateNativeLane,
  projectNativeLane,
} from '../src/features/codex/code-native-lane.js';
import { dictEn } from '../src/shared/i18n/en.js';
import { dictJa } from '../src/shared/i18n/ja.js';
import { dictZh } from '../src/shared/i18n/zh.js';

const source = async relative => readFile(new URL(`../${relative}`, import.meta.url), 'utf8');

test('structured wallet status remains attached to a completed conversation turn', () => {
  const userError = {
    kind: 'wallet_insufficient',
    severity: 'warning',
    stage: 'ADDITIONAL_RESERVATION',
    turnOutcome: 'COMPLETED',
    settlementStatus: 'SETTLED',
    action: 'OPEN_RECHARGE',
    errorCode: 'WALLET_INSUFFICIENT',
  };
  const records = pairDeepSeekTimeline([
    { event: 'user_start', turn_id: 'turn-1', timestamp: 1, ui_turn_index: 0 },
    { event: 'assistant_done', turn_id: 'turn-1', timestamp: 2, status: 'Completed', user_error: userError },
  ]);

  assert.equal(records.length, 1);
  assert.equal(records[0].status, 'Completed');
  assert.deepEqual(records[0].userError, userError);
});

test('hydrated timeline restores a persisted organization notice without a personal recharge action', () => {
  const organizationId = '0199a100-0001-7000-8000-000000000201';
  const timeline = [
    { event: 'user_start', turn_id: 'turn-org', timestamp: 1, ui_turn_index: 0 },
    {
      event: 'assistant_done',
      turn_id: 'turn-org',
      timestamp: 2,
      status: 'Failed',
      billing_notice: {
        schema_version: 1,
        code: 'WALLET_INSUFFICIENT',
        stage: 'INITIAL_RESERVATION',
        turn_outcome: 'REJECTED',
        settlement_status: 'NOT_STARTED',
        action: 'CONTACT_ORG_ADMIN',
        context_type: 'ORGANIZATION',
        organization_id: organizationId,
      },
    },
  ];
  const records = pairDeepSeekTimeline(timeline);

  assert.equal(records[0].userError.kind, 'wallet_insufficient');
  assert.equal(records[0].userError.contextType, 'ORGANIZATION');
  assert.equal(records[0].userError.action, 'CONTACT_ORG_ADMIN');
  assert.equal(records[0].userError.organizationId, organizationId);
  assert.notEqual(records[0].userError.action, 'OPEN_RECHARGE');

  const lane = createNativeLane();
  hydrateNativeLane(lane, {
    messages: [
      { role: 'user', content: [{ type: 'text', text: 'organization request' }] },
      { role: 'assistant', content: [{ type: 'text', text: 'not started' }] },
    ],
  }, timeline);
  const codeTurn = projectNativeLane(lane, 'organization-session').turns[0];
  assert.equal(codeTurn.userError.kind, 'wallet_insufficient');
  assert.equal(codeTurn.userError.contextType, 'ORGANIZATION');
  assert.equal(codeTurn.userError.action, 'CONTACT_ORG_ADMIN');

  const unsafe = pairDeepSeekTimeline([
    { event: 'user_start', turn_id: 'turn-unsafe', timestamp: 3, ui_turn_index: 0 },
    {
      event: 'assistant_done',
      turn_id: 'turn-unsafe',
      timestamp: 4,
      status: 'Failed',
      billing_notice: { ...timeline[1].billing_notice, action: 'OPEN_RECHARGE' },
    },
  ])[0];
  assert.equal(unsafe.userError.contextType, 'ORGANIZATION');
  assert.equal(unsafe.userError.action, '', 'malformed organization notices must fail closed');
  assert.equal(canOpenPersonalRecharge(records[0].userError), false);
  assert.equal(canOpenPersonalRecharge({
    kind: 'wallet_insufficient',
    contextType: 'ORGANIZATION',
    action: 'OPEN_RECHARGE',
  }), false, 'organization context must never route into personal recharge');
  assert.equal(canOpenPersonalRecharge({
    kind: 'wallet_insufficient',
    contextType: 'PERSONAL',
    action: 'OPEN_RECHARGE',
  }), true);
});

test('organization recharge action is permission-bound and never calls personal wallet APIs', async () => {
  const organizationId = '0199a100-0001-7000-8000-000000000202';
  const userError = {
    kind: 'wallet_insufficient',
    contextType: 'ORGANIZATION',
    action: 'OPEN_ORGANIZATION_RECHARGE',
    organizationId,
  };
  const target = walletRechargeTarget(userError);
  assert.deepEqual(target, { contextType: 'ORGANIZATION', organizationId, view: 'organizationRecharge' });
  const account = { active_context: { organization_id: organizationId, permissions: ['org.entitlements.purchase', 'org.wallet.read'] } };
  assert.equal(rechargeTargetAllowed(account, target), true);
  assert.equal(rechargeTargetAllowed({ active_context: { organization_id: organizationId, permissions: [] } }, target), false);

  const calls = [];
  const client = {
    products: async () => ({ items: [{ product_id: 'product-1' }] }),
    organizationBalance: async id => { calls.push(['organizationBalance', id]); return { available_microcredits: 12 }; },
    purchaseOrganizationEntitlements: async (...args) => { calls.push(['purchaseOrganizationEntitlements', ...args]); return { order_id: 'order-1' }; },
    organizationRechargeOrder: async (...args) => { calls.push(['organizationRechargeOrder', ...args]); return { order_id: 'order-1', status: 'PENDING' }; },
    balance: async () => { throw new Error('personal balance must not be called'); },
    createRecharge: async () => { throw new Error('personal recharge must not be called'); },
    rechargeOrder: async () => { throw new Error('personal order must not be called'); },
  };
  const loaded = await loadRechargeTarget(client, account, target);
  await createRechargeForTarget(client, account, target, 'product-1', 'ALIPAY', 'key-1');
  await rechargeOrderForTarget(client, account, target, 'order-1');
  await rechargeBalanceForTarget(client, account, target);
  assert.equal(loaded.balance.available_microcredits, 12);
  assert.deepEqual(calls.map(call => call[0]), [
    'organizationBalance',
    'purchaseOrganizationEntitlements',
    'organizationRechargeOrder',
    'organizationBalance',
  ]);
  const callCount = calls.length;
  const unauthorized = { active_context: { organization_id: organizationId, permissions: [] } };
  await assert.rejects(
    createRechargeForTarget(client, unauthorized, target, 'product-1', 'ALIPAY', 'key-2'),
    /PERMISSION_DENIED/,
  );
  await assert.rejects(
    loadRechargeTarget(client, account, { ...target, organizationId: '0199a100-0001-7000-8000-000000000203' }),
    /PERMISSION_DENIED/,
  );
  assert.equal(calls.length, callCount, 'unauthorized or mismatched organization targets must not call any purchase API');
});

test('native code conversation matches billing notices to the exact turn generation', () => {
  const lane = createNativeLane();
  applyNativeChatEvent(lane, 'chat:user_message', { content: 'hello' });
  applyNativeChatEvent(lane, 'chat:billing_notice', {
    turn_generation: 7,
    billing_notice: {
      code: 'WALLET_INSUFFICIENT',
      stage: 'ADDITIONAL_RESERVATION',
      turn_outcome: 'COMPLETED',
      settlement_status: 'SETTLED',
      action: 'OPEN_RECHARGE',
    },
  });
  applyNativeChatEvent(lane, 'chat:done', { generation: 6, status: 'Completed' });
  assert.equal(Boolean(lane.timeline.at(-1).user_error), false);

  applyNativeChatEvent(lane, 'chat:user_message', { content: 'again' });
  applyNativeChatEvent(lane, 'chat:done', { generation: 7, status: 'Completed' });
  assert.equal(lane.timeline.at(-1).user_error.kind, 'wallet_insufficient');
  assert.equal(lane.timeline.at(-1).user_error.turnOutcome, 'COMPLETED');

  const organizationId = '0199a100-0001-7000-8000-000000000204';
  applyNativeChatEvent(lane, 'chat:user_message', { content: 'organization task' });
  applyNativeChatEvent(lane, 'chat:billing_notice', {
    turn_generation: 8,
    billing_notice: {
      code: 'WALLET_INSUFFICIENT',
      stage: 'ADDITIONAL_RESERVATION',
      turn_outcome: 'COMPLETED',
      settlement_status: 'SETTLED',
      context_type: 'ORGANIZATION',
      organization_id: organizationId,
      action: 'OPEN_ORGANIZATION_RECHARGE',
    },
  });
  applyNativeChatEvent(lane, 'chat:done', { generation: 8, status: 'Completed' });
  const organizationError = lane.timeline.at(-1).user_error;
  assert.equal(organizationError.action, 'OPEN_ORGANIZATION_RECHARGE');
  assert.equal(organizationError.organizationId, organizationId);
  assert.deepEqual(walletRechargeTarget(organizationError), {
    contextType: 'ORGANIZATION',
    organizationId,
    view: 'organizationRecharge',
  });
});

test('desktop billing notice is generation-scoped and opens the in-app recharge view', async () => {
  const [events, timeline, chatView, codexView, nativeLane, main, popover] = await Promise.all([
    source('src/platform/tauri/bridge/chat-events.js'),
    source('src/features/conversation/ConversationTimeline.jsx'),
    source('src/features/chat/ChatView.jsx'),
    source('src/features/codex/CodexAcpView.jsx'),
    source('src/features/codex/code-native-lane.js'),
    source('src/app/main.jsx'),
    source('src/features/account-platform/AccountPlatformPopover.jsx'),
  ]);

  assert.match(events, /billingNoticeKey\(sid, payload\.turn_generation\)/);
  assert.match(events, /billingNoticeKey\(sid, e\.payload && e\.payload\.generation\)/);
  assert.match(events, /mergeLocalTimelineBusinessState/);
  assert.match(timeline, /walletRechargeTarget\(turn\.userError\)/);
  assert.match(timeline, /onUserErrorAction\(turn\.userError, turn\)/);
  assert.match(chatView, /onUserErrorAction=\{onOpenAccountRecharge\}/);
  assert.match(codexView, /'chat:billing_notice'/);
  assert.match(codexView, /onUserErrorAction=\{onOpenAccountRecharge\}/);
  assert.match(nativeLane, /pendingBillingNotices\[String\(Number\(p\.turn_generation \|\| 0\)\)\]/);
  assert.match(main, /setAccountPopoverRequest\(\{ \.\.\.target, requestId: Date\.now\(\) \}\)/);
  assert.match(popover, /organizationRecharge/);
  assert.match(popover, /setView\('recharge'\)/);
  assert.match(popover, /void loadRecharge\(target\)/);
});

test('wallet notice copy is complete in all supported UI languages', () => {
  for (const dictionary of [dictZh, dictEn, dictJa]) {
    for (const key of [
      'walletInsufficientTitle',
      'walletInsufficientMessage',
      'walletInsufficientCompletedTitle',
      'walletInsufficientCompletedMessage',
      'organizationWalletInsufficientMessage',
      'organizationWalletPurchaserMessage',
      'rechargeNow',
      'organizationRechargeNow',
    ]) {
      assert.equal(typeof dictionary.uiConversation[key], 'string');
      assert.ok(dictionary.uiConversation[key].trim());
    }
  }
});
