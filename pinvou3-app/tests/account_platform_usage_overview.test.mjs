import assert from 'node:assert/strict';
import test from 'node:test';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { validateModelOperationsPageOverview,validateModelOperationsOverview,modelTokenPageOverviewPermissionView,modelTokenPageOverviewDrilldown,usageOverviewRoute } from '../src/features/account-platform/admin-usage-overview.js';
import { planAdminModelTokenPageOverview,modelTokenPageOverviewCacheKey,createModelTokenPageOverviewRequestTracker } from '../src/features/account-platform/admin-usage-overview-state.js';
import { adminModelTokenQueryUpdates } from '../src/features/account-platform/admin-navigation.js';
import { overviewDTO } from './fixtures/account-platform-f05-overview-data.js';

test('Three installed permission views exactly bind plan/window and real client GET',async()=>{
 const oldFetch=globalThis.fetch;const calls=[];globalThis.fetch=async(url,options)=>{calls.push({url,options});return new Response('{}',{status:200});};
 try{const client=new AdminPlatformClient();for(const [permissions,view,window]of[[['platform.tokens.read'],'TOKEN_ONLY',''],[['platform.usage.read'],'USAGE_ONLY','7d'],[['platform.tokens.read','platform.usage.read'],'TOKEN_AND_USAGE','7d']]){
  const plan=planAdminModelTokenPageOverview('modelOperations',permissions,true,'7d');assert.equal(plan.permissionView,view);assert.equal(plan.window,window);const controller=new AbortController();await plan.load(client,controller.signal);const call=calls.at(-1);assert.equal(new URL(call.url,'https://portal.example').searchParams.get('window'),window||null);assert.equal(call.options.credentials,'same-origin');assert.equal(call.options.cache,'no-store');assert.ok(call.options.signal instanceof AbortSignal);
 }
 assert.equal(planAdminModelTokenPageOverview('modelOperations',[],true),null);assert.equal(planAdminModelTokenPageOverview('modelOperations',['platform.usage.read'],false),null);
 await client.tokenOperationsOverview('30d');assert.match(calls.at(-1).url,/operations-overview\?window=30d$/);assert.throws(()=>client.tokenOperationsOverview('future'));
 }finally{globalThis.fetch=oldFetch;}
});
test('Owner shape/counts and trend chronology fail closed without unsafe integer coercion',()=>{
 for(const view of['TOKEN_ONLY','USAGE_ONLY','TOKEN_AND_USAGE']){const raw=overviewDTO(view,'24h');assert.ok(validateModelOperationsPageOverview(raw,view));assert.equal(validateModelOperationsPageOverview(raw,view==='TOKEN_ONLY'?'USAGE_ONLY':'TOKEN_ONLY'),null);for(const invalid of[9007199254740992,-1,1.5,'2']){const bad=structuredClone(raw);(bad.usage?.metrics||bad.provisioning.metrics)[0].value=invalid;assert.equal(validateModelOperationsPageOverview(bad,view),null);}}
 for(const duration of['24h','7d','30d']){const raw=overviewDTO('TOKEN_AND_USAGE',duration,true);assert.ok(validateModelOperationsOverview(raw));raw.trend.points[0].usage_request_count=2;assert.equal(validateModelOperationsOverview(raw),null);}
});
test('Card and bucket routes reset detail/evidence/cursor with half-open source filters',()=>{
 const raw=overviewDTO('TOKEN_AND_USAGE');const bucket=raw.usage.trend.points[0];for(const key of['usage_request_count','usage_trend','reconciliation_pending_count','provisioning_pending_count']){
  const target=modelTokenPageOverviewDrilldown('modelOperations',key,raw.usage.window,bucket);const next=usageOverviewRoute(target,adminModelTokenQueryUpdates(target.source,{...target.query,cursor:''}));assert.equal(next.query.usage_request_id,null);assert.equal(next.query.token_account_id,null);assert.equal(next.query.model_id,null);assert.equal(next.query.usage_evidence_cursor,null);
  if(key==='usage_trend'){assert.equal(next.query.usage_started_from,bucket.from);assert.equal(next.query.usage_started_to,bucket.to);assert.equal(next.query.usage_cursor,null);}
  if(key==='provisioning_pending_count')assert.equal(next.query.provisioning_status,'PENDING');
 }
});
test('Lineage cache and request generations isolate actor/version/permission/window',()=>{
 const base={userID:'controlled-owner',authorizationVersion:1,section:'modelOperations',permissionView:'TOKEN_AND_USAGE',window:'24h'};const key=modelTokenPageOverviewCacheKey(base);for(const change of[{userID:'other'},{authorizationVersion:2},{permissionView:'USAGE_ONLY'},{window:'7d'}])assert.notEqual(modelTokenPageOverviewCacheKey({...base,...change}),key);
 assert.equal(modelTokenPageOverviewCacheKey({...base,authorizationVersion:0}),'');assert.equal(modelTokenPageOverviewPermissionView([]),'');const tracker=createModelTokenPageOverviewRequestTracker();const old=tracker.begin(key);tracker.invalidate(key);assert.equal(tracker.isCurrent(key,old),false);
});
