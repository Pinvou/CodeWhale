import test from 'node:test';
import assert from 'node:assert/strict';
import { timelineUserError,pairDeepSeekTimeline } from '../src/features/conversation/deepseek-conversation.js';
import { canOpenPersonalRecharge,walletRechargeTarget } from '../src/features/conversation/billing-notice.js';
const notice={schema_version:1,code:'WALLET_INSUFFICIENT',stage:'ADDITIONAL_RESERVATION',turn_outcome:'COMPLETED',settlement_status:'SETTLED',action:'OPEN_RECHARGE',context_type:'PERSONAL'};
test('persisted completed personal billing notice keeps successful outcome and actionable recharge facts',()=>{ const events=[{event:'user_start',turn_id:'public-turn',timestamp:1,ui_turn_index:0},{event:'assistant_done',turn_id:'public-turn',timestamp:2,status:'Completed',billing_notice:notice}];const rows=pairDeepSeekTimeline(events);assert.equal(rows[0].status,'Completed');assert.equal(rows[0].userError.severity,'warning');assert.equal(rows[0].userError.settlementStatus,'SETTLED');assert.deepEqual(walletRechargeTarget(rows[0].userError),{contextType:'PERSONAL',view:'recharge'}); });
test('non personal or unrelated notice cannot dispatch personal recharge',()=>{for(const input of [{...notice,context_type:'ORGANIZATION'},{...notice,action:'CONTACT_ORG_ADMIN'},{...notice,code:'UNRELATED'}]) assert.equal(canOpenPersonalRecharge(timelineUserError({billing_notice:input})),false);});
test('organization notices retain exact scope and never authorize personal recharge', () => {
  const organization_id = '0199a100-0001-7000-8000-000000000201';
  for (const code of ['WALLET_INSUFFICIENT', 'MEMBER_QUOTA_INSUFFICIENT']) {
    const input = {...notice, code, context_type:'ORGANIZATION', organization_id, action:code === 'MEMBER_QUOTA_INSUFFICIENT' ? 'CONTACT_ORG_ADMIN' : 'OPEN_ORGANIZATION_RECHARGE'};
    const result = timelineUserError({billing_notice:input});
    assert.equal(result.errorCode, code);
    assert.equal(result.organizationId, organization_id);
    assert.equal(result.severity, 'warning');
    assert.equal(walletRechargeTarget(result), null);
    assert.equal(timelineUserError({billing_notice:{...input, organization_id:'foreign text'}}), null);
    assert.equal(timelineUserError({billing_notice:{...input, schema_version:2}}), null);
  }
});
