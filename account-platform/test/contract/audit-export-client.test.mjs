import assert from 'node:assert/strict';
import test from 'node:test';
import {AdminPlatformClient} from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import {newAuditExportAttempt,submitAuditExportAttempt} from '../../../pinvou3-app/src/features/account-platform/admin-audit-export.js';
import {auditWorkspaceCopy} from '../../../pinvou3-app/src/shared/i18n.js';
const id='ec4df7c4-2bd1-4cbb-a267-2018abf90f63';
test('real export client keeps facts/key after lost ACK and validates definite create ACK',async()=>{
 const original=globalThis.fetch;const calls=[];const client=new AdminPlatformClient();client.csrfToken='synthetic-session-binding';
 try{
  globalThis.fetch=async(url,options)=>{calls.push({url,options});if(calls.length===1)throw new TypeError('controlled transport loss');return new Response(JSON.stringify({export_id:id,status:'PENDING',decryption_key:'synthetic-recovered-key'}),{status:202})};
  const attempt=newAuditExportAttempt({from:'2026-10-06T00:00:00Z',to:'2026-10-07T00:00:00Z'},'controlled audit investigation',false,'synthetic-export-key');
  await assert.rejects(submitAuditExportAttempt(client,attempt));const result=await submitAuditExportAttempt(client,attempt);
  assert.equal(result.export_id,id);assert.equal(result.decryption_key,'synthetic-recovered-key');
  assert.equal(calls[0].options.body,calls[1].options.body);assert.equal(calls[0].options.headers['Idempotency-Key'],calls[1].options.headers['Idempotency-Key']);
  globalThis.fetch=async()=>new Response(JSON.stringify({export_id:id,status:'PENDING'}),{status:202});
  await assert.rejects(submitAuditExportAttempt(client,attempt),/INVALID_AUDIT_EXPORT_ACK/);
 }finally{globalThis.fetch=original}
});
test('download ticket response must bind exact export and opaque token',async()=>{
 const original=globalThis.fetch;const client=new AdminPlatformClient();try{
  globalThis.fetch=async()=>new Response(JSON.stringify({export_id:id,download_token:'a'.repeat(43),expires_at:'2026-10-08T00:00:00Z'}),{status:201});
  assert.equal((await client.issueAuditExportDownloadTicket(id)).export_id,id);
  globalThis.fetch=async()=>new Response(JSON.stringify({export_id:'e5447cdb-c62f-49f7-a72e-613ae1d56d35',download_token:'a'.repeat(43),expires_at:'2026-10-08T00:00:00Z'}),{status:201});
  await assert.rejects(client.issueAuditExportDownloadTicket(id),/INVALID_AUDIT_EXPORT_TICKET_ACK/);
 }finally{globalThis.fetch=original}
});
test('three original locales disclose one-time ticket recovery and no key logging',()=>{
 for(const locale of ['zh','en','ja'])for(const key of ['ticketIssueUnknown','ticketUnavailable','retrySameExport','saveKey','includePII'])assert.ok(auditWorkspaceCopy[locale].audit[key]);
});

test('OpenAPI service and BFF server bases match all six actual client transports', async () => {
 const {readFile}=await import('node:fs/promises');
 const contract=JSON.parse(await readFile(new URL('../../contracts/openapi/audit-export.json',import.meta.url),'utf8'));
 const original=globalThis.fetch;const calls=[];const client=new AdminPlatformClient();client.csrfToken='controlled-csrf';
 const token='t'.repeat(43);
 try {
  globalThis.fetch=async(url,options={})=>{
   calls.push({url:String(url),options});
   if(String(url).endsWith('/download'))return new Response('controlled-envelope',{status:200,headers:{'Content-Type':'application/vnd.pinvou.audit-envelope+json','Content-Length':'19'}});
   if(String(url).endsWith('/download-tickets'))return new Response(JSON.stringify({export_id:id,download_token:token,expires_at:'2099-01-01T00:00:00Z',download_url:'https://untrusted.invalid/do-not-follow'}),{status:201});
   if(String(url).includes('/audit-exports'))return new Response(JSON.stringify({export_id:id,status:'PENDING',decryption_key:'controlled-key'}),{status:options.method==='POST'?202:200});
   return new Response(JSON.stringify({items:[],audit_id:id}),{status:200});
  };
  await client.audits();await client.audit(id);
  await client.createAuditExport({},'controlled audit review',false,'controlled-key');
  await client.auditExport(id);const ticket=await client.issueAuditExportDownloadTicket(id);
  await client.downloadAuditExport(id,ticket.download_token);
  const paths=['/admin/v1/audit-logs','/admin/v1/audit-logs/{audit_id}','/admin/v1/audit-exports','/admin/v1/audit-exports/{export_id}','/admin/v1/audit-exports/{export_id}/download-tickets','/admin/v1/audit-exports/{export_id}/download'];
  assert.deepEqual(Object.keys(contract.paths),paths);
  assert.deepEqual(contract.servers.map(server=>server.url),['/','/api/admin/account']);
  for(let index=0;index<paths.length;index++){
   const concrete=paths[index].replace(/\{(?:audit_id|export_id)\}/g,id);
   assert.equal(contract.servers[0].url.replace(/\/$/,'')+concrete,concrete);
   assert.equal(contract.servers[1].url+concrete,calls[index].url);
   assert.equal(calls[index].options.credentials,'same-origin');assert.equal(calls[index].options.cache,'no-store');
  }
  assert.equal(calls.length,6);assert.equal(calls[5].options.headers['X-Pinvou-Download-Token'],token);
  assert.equal(calls[5].options.redirect,'error');assert.equal(calls[2].options.headers['X-CSRF-Token'],'controlled-csrf');
  assert.equal(contract.components.schemas.AuditExportDownloadTicket.properties.download_url.deprecated,true);
 }finally{globalThis.fetch=original}
});
