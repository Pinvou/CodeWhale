import test from 'node:test';
import assert from 'node:assert/strict';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../src/shared/admin-platform-i18n.js';
const id='00000000-0000-4000-8000-000000000001';
const code='USR-1111-1111-1111-1111-11';
test('actual code client calls the dedicated BFF owner route and accepts deleted projection',async()=>{
 const original=globalThis.fetch;let actual;
 globalThis.fetch=async(url,options)=>{actual={url,options};return new Response(JSON.stringify({user_id:id,user_code:code,status:'DELETED'}),{status:200})};
 try{const c=new AdminPlatformClient();assert.deepEqual(await c.userByCode(code.toLowerCase()),{user_id:id,user_code:code,status:'DELETED'});assert.equal(actual.url,'/api/admin/account/admin/v1/user-code-lookups/'+code.toLowerCase());assert.equal(actual.options.credentials,'same-origin');assert.equal(actual.options.cache,'no-store');assert.equal(actual.options.body,undefined)}finally{globalThis.fetch=original}
});
test('code client never substitutes fuzzy directory search and rejects a mismatching or PII response',async()=>{
 const original=globalThis.fetch;let calls=0;try{
  const c=new AdminPlatformClient();for(const bad of [{user_id:id,user_code:code,status:'ACTIVE',email:'private.invalid'},{user_id:id,user_code:code.replace('1111','2222'),status:'ACTIVE'},{user_id:'bad',user_code:code,status:'ACTIVE'},{user_id:id,user_code:code}]){globalThis.fetch=async()=>{calls++;return new Response(JSON.stringify(bad),{status:200})};await assert.rejects(c.userByCode(code),/INVALID_USER_CODE_LOOKUP_ACK/)}
  for(const raw of ['',code+'/extra',code+'?q=prefix'])await assert.rejects(c.userByCode(raw),/USER_CODE_LOOKUP_FAILED/);assert.equal(calls,4)
 }finally{globalThis.fetch=original}
});
test('lookup forwards uniform owner errors without direct retry or alternate search',async()=>{
 const original=globalThis.fetch;try{for(const status of [404,429,503]){let calls=0;globalThis.fetch=async()=>{calls++;return new Response(JSON.stringify({code:'USER_NOT_FOUND',message:'the user lookup could not be completed'}),{status})};await assert.rejects(new AdminPlatformClient().userByCode(code),e=>e.status===status);assert.equal(calls,1)}}finally{globalThis.fetch=original}
});
test('exact lookup has explicit Simplified Chinese English and Japanese copy',()=>{
 for(const language of ['zh','en','ja'])for(const key of ['exactCode','codePlaceholder','lookup','codeLookupFailed'])assert.equal(typeof adminPlatformCopy[language].userManagement[key],'string');
 assert.notEqual(adminPlatformCopy.ja.userManagement.codeLookupFailed,adminPlatformCopy.en.userManagement.codeLookupFailed);
});
