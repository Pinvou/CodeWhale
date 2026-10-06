import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { performLogicalOperation, quotaBatchAcknowledged } from '../src/features/account-platform/logical-operation.js';

test('mixed quota acknowledgement retries the same batch, replays successful child and completes only once', async () => {
  const cache = new Map();
  let sequence = 0;
  let attempts = 0;
  const created = new Map();
  const dispatchKeys = [];
  const items = [{ member_user_id:'member-a', amount_microcredits:10000 }, { member_user_id:'member-b', amount_microcredits:20000 }];
  const call = () => performLogicalOperation(cache, {
    kind:'quota-batch', identity:JSON.stringify(['user','org',items]), createKey:kind=>`${kind}-${++sequence}`,
    acknowledged:quotaBatchAcknowledged,
    action:async key => {
      dispatchKeys.push(key); attempts += 1;
      return { items:items.map((item,index) => {
        const childKey = `${key}:${index}`;
        if (index === 1 && attempts === 1) return {index,status:'FAILED',code:'BILLING_CONFLICT'};
        const replay = created.has(childKey);
        if (!replay) created.set(childKey,item);
        return {index,status:'SUCCEEDED',created:!replay};
      }) };
    },
  });
  assert.equal((await call()).items[1].status,'FAILED');
  assert.equal(cache.size,1); assert.equal(created.size,1);
  const recovered = await call();
  assert.equal(dispatchKeys[0],dispatchKeys[1]);
  assert.equal(recovered.items[0].created,false); assert.equal(recovered.items[1].created,true);
  assert.equal(created.size,2); assert.equal(cache.size,0);
  await call(); assert.notEqual(dispatchKeys[1],dispatchKeys[2]); assert.equal(created.size,4);
});

test('unknown acknowledgement retains exact key and facts, while different identity/facts use a new call', async () => {
  const cache = new Map(); let sequence = 0; const seen = [];
  const unknown = identity => performLogicalOperation(cache, {kind:'unit-create',identity,createKey:()=>`key-${++sequence}`,action:async key=>{seen.push(key);throw new Error('controlled unknown response');}});
  await assert.rejects(unknown(JSON.stringify(['user-a','org-a',{name:'unit'}])));
  await assert.rejects(unknown(JSON.stringify(['user-a','org-a',{name:'unit'}])));
  assert.equal(seen[0],seen[1]);
  for (const identity of [['user-b','org-a',{name:'unit'}],['user-b','org-b',{name:'unit'}],['user-b','org-b',{name:'other'}]]) await assert.rejects(unknown(JSON.stringify(identity)));
  assert.equal(new Set(seen).size,4);
  await performLogicalOperation(cache,{kind:'unit-create',identity:'confirmed',createKey:()=>`key-${++sequence}`,action:async()=>({unit:'controlled valid receipt'})});
  assert.equal(cache.size,0);
});

test('late acknowledgement cannot remove a newer logical operation', async () => {
  const cache = new Map(); let resolve; let sequence = 0;
  const old = performLogicalOperation(cache,{kind:'quota-batch',identity:'user-a/org-a',createKey:()=>`key-${++sequence}`,action:()=>new Promise(done=>{resolve=done;})});
  await assert.rejects(performLogicalOperation(cache,{kind:'quota-batch',identity:'user-b/org-b',createKey:()=>`key-${++sequence}`,action:async()=>{throw new Error('controlled unknown');}}));
  resolve({items:[{status:'SUCCEEDED'}]}); await old;
  assert.equal(cache.get('quota-batch').identity,'user-b/org-b');
});

test('quota acknowledgement rejects empty/unknown/failed batches, and View uses the same product helper', async () => {
  for (const value of [null,{}, {items:[]}, {items:[null]}, {items:[{status:'FAILED'}]}, {items:[{status:'SUCCEEDED'},{status:'FAILED'}]}]) assert.equal(quotaBatchAcknowledged(value),false);
  assert.equal(quotaBatchAcknowledged({items:[{status:'SUCCEEDED'},{status:'SUCCEEDED'}]}),true);
  const source=await readFile(new URL('../src/features/account-platform/AccountPlatformView.jsx',import.meta.url),'utf8');
  assert.match(source,/return performLogicalOperation\(operationKeysRef\.current,/u);
  const quota=source.slice(source.indexOf('const allocateQuota ='),source.indexOf('const revokeQuota ='));
  assert.match(quota,/\}, quotaBatchAcknowledged\)/u);
});
