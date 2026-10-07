import assert from 'node:assert/strict';
import test from 'node:test';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
const A='PUBLIC_SYNTHETIC_CSRF_A', B='PUBLIC_SYNTHETIC_CSRF_B';
const json=(data,status=200)=>new Response(JSON.stringify(data),{status,headers:{'Content-Type':'application/json'}});
async function check(name,{logout=false,initial=A,refreshed=A,accepted=true,mutateDuringRefresh=false}) {
  const posts=[];let sessions=0;let client;
  globalThis.fetch=async(url,options={})=>{
    if(url==='/api/session') {sessions++; return json({authenticated:true,csrf_token:refreshed});}
    const expected=logout?'/api/logout':'/api/account/v1/me/profile-change-requests';
    assert.equal(url,expected);posts.push({csrf:options.headers['X-CSRF-Token'],body:options.body});
    if(posts.length===1)return json({code:'CSRF_REJECTED'},403);
    return logout?new Response(null,{status:204}):json({profile_change_request_id:'11111111-1111-4111-8111-111111111111',field:'USERNAME',status:'PENDING'});
  };
  client=new AccountPlatformBffClient();client.csrfToken=initial;
  if (mutateDuringRefresh) {
    const originalSession = client.session.bind(client);
    client.session = async () => {
      const receipt = await originalSession();
      // Another login completes after the refresh receipt, before retry sends.
      client.csrfToken = B;
      return receipt;
    };
  }
  let success=false;let code;
  try {if(logout)await client.logout();else await client.createProfileChange('USERNAME','public-user-a-newname');success=true;}catch(e){code=e.code;}
  assert.equal(success,accepted);assert.equal(sessions,1);assert.equal(posts.length,accepted?2:1);
  assert.equal(posts[0].csrf,initial);
  if(accepted){assert.equal(posts[1].csrf,initial);assert.equal(posts[1].body,posts[0].body);}
  else {assert.equal(code,'CSRF_REJECTED');assert.equal(client.csrfToken,refreshed);}
}

test('original-cross-login-mutation-counterexample', async () => {
  const originalFetch = globalThis.fetch;
  try { await check('original-cross-login-mutation-counterexample',{refreshed:B,accepted:false}); } finally { globalThis.fetch = originalFetch; }
});
test('ordinary-same-CSRF-refresh-mutation', async () => {
  const originalFetch = globalThis.fetch;
  try { await check('ordinary-same-CSRF-refresh-mutation',{refreshed:A}); } finally { globalThis.fetch = originalFetch; }
});
test('cross-login-logout-does-not-log-out-new-user', async () => {
  const originalFetch = globalThis.fetch;
  try { await check('cross-login-logout-does-not-log-out-new-user',{logout:true,refreshed:B,accepted:false}); } finally { globalThis.fetch = originalFetch; }
});
test('ordinary-same-CSRF-refresh-logout', async () => {
  const originalFetch = globalThis.fetch;
  try { await check('ordinary-same-CSRF-refresh-logout',{logout:true,refreshed:A}); } finally { globalThis.fetch = originalFetch; }
});
test('empty-initial-CSRF-mutation-does-not-adopt-session', async () => {
  const originalFetch = globalThis.fetch;
  try { await check('empty-initial-CSRF-mutation-does-not-adopt-session',{initial:'',refreshed:B,accepted:false}); } finally { globalThis.fetch = originalFetch; }
});
test('empty-initial-CSRF-logout-does-not-adopt-session', async () => {
  const originalFetch = globalThis.fetch;
  try { await check('empty-initial-CSRF-logout-does-not-adopt-session',{logout:true,initial:'',refreshed:B,accepted:false}); } finally { globalThis.fetch = originalFetch; }
});
test('retry-uses-frozen-original-CSRF', async () => {
  const originalFetch = globalThis.fetch;
  try { await check('retry-uses-frozen-original-CSRF',{refreshed:A,mutateDuringRefresh:true}); } finally { globalThis.fetch = originalFetch; }
});
