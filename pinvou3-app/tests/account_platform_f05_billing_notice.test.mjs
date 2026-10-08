import test from 'node:test';
import assert from 'node:assert/strict';
import { timelineUserError,pairDeepSeekTimeline } from '../src/features/conversation/deepseek-conversation.js';
import { canOpenPersonalRecharge,walletRechargeTarget } from '../src/features/conversation/billing-notice.js';
const notice={schema_version:1,code:'WALLET_INSUFFICIENT',stage:'ADDITIONAL_RESERVATION',turn_outcome:'COMPLETED',settlement_status:'SETTLED',action:'OPEN_RECHARGE',context_type:'PERSONAL'};
test('persisted completed personal billing notice keeps successful outcome and actionable recharge facts',()=>{ const events=[{event:'user_start',turn_id:'public-turn',timestamp:1,ui_turn_index:0},{event:'assistant_done',turn_id:'public-turn',timestamp:2,status:'Completed',billing_notice:notice}];const rows=pairDeepSeekTimeline(events);assert.equal(rows[0].status,'Completed');assert.equal(rows[0].userError.severity,'warning');assert.equal(rows[0].userError.settlementStatus,'SETTLED');assert.deepEqual(walletRechargeTarget(rows[0].userError),{contextType:'PERSONAL',view:'recharge'}); });
test('non personal or unrelated notice cannot dispatch personal recharge',()=>{for(const input of [{...notice,context_type:'ORGANIZATION'},{...notice,action:'CONTACT_ORG_ADMIN'},{...notice,code:'UNRELATED'}]) assert.equal(canOpenPersonalRecharge(timelineUserError({billing_notice:input})),false);});
