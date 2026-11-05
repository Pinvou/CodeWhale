import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import fs from 'node:fs';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
const pages = JSON.parse(fs.readFileSync(new URL('./fixtures/managed-price-pages.json', import.meta.url)));
test('real AdminClient walks producer pages without automatic fetching and reaches older ACTIVE', async () => {
  const calls=[];
  const server=createServer((req,res)=>{
    const u=new URL(req.url,'http://fixture.invalid');calls.push(u);
    assert.equal(u.searchParams.get('limit'),'100');
    res.setHeader('Content-Type','application/json');
    res.end(JSON.stringify(u.searchParams.has('cursor')?pages.second:pages.first));
  });
  await new Promise(done=>server.listen(0,'127.0.0.1',done));
  try {
    const client=new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`);
    const first=await client.managedPrices();
    assert.equal(first.items.length,100);assert.equal(calls.length,1);assert.equal(first.has_more,true);
    const second=await client.managedPrices({cursor:first.next_cursor});
    assert.equal(second.items.length,1);assert.equal(second.items[0].derived_status,'ACTIVE');
    assert.equal(first.items.some(item=>item.price_version_id===second.items[0].price_version_id),false);
    assert.equal(second.has_more,false);assert.equal(calls[1].searchParams.get('cursor'),first.next_cursor);
  } finally {await new Promise(done=>server.close(done));}
});
for(const payload of [{items:[]},{items:[],has_more:true},{items:[],has_more:false,next_cursor:'unexpected'}]) {
  test(`real AdminClient rejects malformed continuation ${JSON.stringify(payload)}`,async()=>{
    const old=globalThis.fetch;globalThis.fetch=async()=>new Response(JSON.stringify(payload),{status:200});
    try { await assert.rejects(()=>new AdminPlatformClient().managedPrices(),{code:'INVALID_MANAGED_PRICES_RESPONSE'}); } finally {globalThis.fetch=old;}
  });
}
test('real AdminClient retires a late page after session replacement', async () => {
  const original=globalThis.fetch;
  let release,started;
  const reached=new Promise(done=>started=done);
  globalThis.fetch=()=>{started();return new Promise(done=>release=()=>done(new Response(JSON.stringify(pages.second),{status:200})));};
  try {
    const client=new AdminPlatformClient();
    const pending=client.managedPrices({cursor:pages.first.next_cursor});
    await reached;client.retireSession();release();
    await assert.rejects(pending);
  } finally {globalThis.fetch=original;}
});
