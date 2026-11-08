import assert from 'node:assert/strict';
import test from 'node:test';
import http from 'node:http';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

test('explicit acceptance script follows current admin, context-version, catalog and exact-money HTTP contracts', { timeout: 15000 }, async () => {
  // HTTP contract simulation only: no real OIDC, PostgreSQL, provider or funds.
  const user = '11111111-1111-4111-8111-111111111111', organization = '22222222-2222-4222-8222-222222222222';
  const model = '33333333-3333-4333-8333-333333333333';
  let contextVersion = 7, activeOrganization = null, balanceReads = 0, completed = false;
  const observed = new Set(), keys = new Set(), failures = [];
  const server = http.createServer(async (request, response) => {
    try {
      const chunks = []; for await (const chunk of request) chunks.push(chunk);
      const text = Buffer.concat(chunks).toString(), path = new URL(request.url, 'http://fixture.invalid').pathname;
      observed.add(`${request.method} ${path}`);
      const body = text && request.headers['content-type'] === 'application/json' ? JSON.parse(text) : null;
      const send = (value, status = 200) => { response.writeHead(status, {'content-type':'application/json'}); response.end(JSON.stringify(value)); };
      if (path === '/oidc/token') {
        assert.equal(request.method, 'POST'); assert.equal(new URLSearchParams(text).get('grant_type'), 'password');
        send({access_token:`fixture.${Buffer.from(JSON.stringify({sid:'controlled-session',aud:'fixture-account'})).toString('base64url')}.fixture`}); return;
      }
      if (path === '/fixture-payment/complete') { assert.equal(request.headers.authorization, undefined); completed = true; send({ok:true}); return; }
      assert.match(request.headers.authorization, /^Bearer fixture\./);
      if (path === '/v1/account/bootstrap') { send({user_id:user,status:'ACTIVE',mobile_verified:true,platform_permissions:['platform.organizations.create']}); return; }
      if (path === '/admin/v1/organizations') {
        assert.equal(body.owner_user_id, user); assert.ok(body.name);
        keys.add(request.headers['idempotency-key']); send({organization_id:organization},201); return;
      }
      if (path === `/v1/organizations/${organization}/units`) { assert.equal(body.parent_id,null); send({unit:{unit_id:'44444444-4444-4444-8444-444444444444'}},201); return; }
      if (path === '/v1/me/authorization-context') { send({context_version:contextVersion,organization_id:activeOrganization}); return; }
      if (path === '/v1/sessions/current/active-context') {
        assert.equal(body.expected_context_version, contextVersion);
        activeOrganization=body.organization_id;contextVersion++;
        send({context_version:contextVersion,organization_id:activeOrganization}); return;
      }
      if (path === '/v1/recharge/products') { assert.equal(activeOrganization,null); send({items:[{product_id:'fixture-product',availability_status:'AVAILABLE',channels:['ALIPAY']}]}); return; }
      if (path === '/v1/models') { send({items:[{model_id:model,context_eligible:true}]}); return; }
      if (path === '/v1/recharge/orders') {
        assert.deepEqual(body,{product_id:'fixture-product',channel:'ALIPAY'});
        keys.add(request.headers['idempotency-key']);send({order_id:'55555555-5555-4555-8555-555555555555',payment_url:'http://127.0.0.1/fixture-payment'},201); return;
      }
      if (path === '/v1/recharge/orders/55555555-5555-4555-8555-555555555555') { assert.equal(completed,true);send({fulfillment_status:'CREDITED'}); return; }
      if (path === '/v1/me/balance') { balanceReads++;send({available_microcredits:balanceReads===1?'9007199254740993':'9007199254740992'}); return; }
      if (path === '/v1/chat/completions') {
        assert.equal(body.model,model);keys.add(request.headers['idempotency-key']);
        response.setHeader('X-Pinvou-Usage-Request-ID','66666666-6666-4666-8666-666666666666');send({model:'fixture-upstream'});return;
      }
      throw new Error('unexpected legacy contract');
    } catch (error) { failures.push(error); response.writeHead(500); response.end('{}'); }
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  try {
    const origin=`http://127.0.0.1:${server.address().port}`;
    const result=await new Promise((resolve,reject)=>{
      const child=spawn(process.execPath,[fileURLToPath(new URL('../integration/auth-business-acceptance.mjs',import.meta.url)),'--execute-disposable-fixture'],{shell:false,windowsHide:true,stdio:['ignore','pipe','pipe'],env:{...process.env,PINVOU_TEST_ACCOUNT_ORIGIN:origin,PINVOU_TEST_TOKEN_ORIGIN:origin,PINVOU_TEST_PAYMENT_REQUEST_ORIGIN:origin,PINVOU_TEST_OIDC_TOKEN_URL:`${origin}/oidc/token`,PINVOU_TEST_OIDC_CLIENT_ID:'fixture-client',PINVOU_TEST_OIDC_AUDIENCE:'fixture-account',PINVOU_TEST_OIDC_USERNAME:'fixture-user',PINVOU_TEST_OIDC_PASSWORD:'fixture-only-not-a-credential',PINVOU_TEST_PRODUCT_ID:'fixture-product',PINVOU_TEST_MODEL_ID:model,PINVOU_TEST_RESPONSE_MODEL:'fixture-upstream'}});
      let stdout='',stderr='';const timer=setTimeout(()=>child.kill(),10000);
      child.stdout.on('data',chunk=>{stdout+=chunk;});child.stderr.on('data',chunk=>{stderr+=chunk;});
      child.once('error',error=>{clearTimeout(timer);reject(error);});child.once('close',code=>{clearTimeout(timer);resolve({code,stdout,stderr});});
    });
    assert.deepEqual(failures,[]);assert.equal(result.code,0,result.stderr);assert.match(result.stdout,/authenticated business acceptance passed/);
    assert.ok(observed.has('POST /admin/v1/organizations'));assert.ok(!observed.has('POST /v1/organizations'));
    assert.equal(keys.size,3);assert.ok([...keys].every(key=>typeof key==='string'&&key.length>=16));
    assert.equal(balanceReads,2);assert.equal(contextVersion,9);assert.equal(activeOrganization,null);
  } finally { server.closeAllConnections();await new Promise(resolve=>server.close(resolve)); }
});
