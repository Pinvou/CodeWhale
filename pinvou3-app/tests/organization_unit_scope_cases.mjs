import assert from 'node:assert/strict';

export async function runOrganizationUnitScopeCases(page, origin) {
 const errors = [], results = [];
 const errorListener = error => errors.push(error.message); page.on('pageerror', errorListener);
 const fresh = async (options = {}) => {
  errors.length = 0; await page.goto(`${origin}/tests/fixtures/account-platform-unit-scope.html?configuration=${encodeURIComponent(JSON.stringify(options))}`);
  await page.waitForFunction(() => window.unitScope);
 };
 const ready = () => page.waitForFunction(() => [...document.querySelectorAll('.ap-org-unit-scope form')].find(form => form.querySelector('h4')?.textContent === window.unitScope.copy().replaceMemberUnits)?.querySelector('select')?.options.length > 1);
 const writes = () => page.evaluate(() => window.unitScope.calls.filter(call => call.method !== 'GET'));
 const run = async (name, test) => { await test(); assert.deepEqual(errors, []); results.push({ name, pass: true }); };
 const form = key => page.evaluateHandle(key => [...document.querySelectorAll('.ap-org-unit-scope form')].find(element => element.querySelector('h4')?.textContent === window.unitScope.copy()[key]), key);
 const fill = async (key, fields) => {
  const handle = await form(key);
  await handle.evaluate((element, fields) => {
   if (!element) throw new Error('Missing scoped action');
   for (const [index, value] of fields) {
    const input = element.querySelectorAll('input,select')[index];
    if (input.tagName === 'SELECT') { for (const option of input.options) option.selected = (Array.isArray(value) ? value : [value]).includes(option.value); input.dispatchEvent(new Event('change', { bubbles: true })); }
    else { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, value); input.dispatchEvent(new Event('input', { bubbles: true })); }
   }
  }, fields); await handle.dispose();
 };
 const submit = async key => { const handle = await form(key); await handle.evaluate(element => element.requestSubmit()); await handle.dispose(); };
 try {
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: real UNIT DTO opens only an explicit authorized subtree`, async () => {
   await fresh({ language }); await ready();
   assert.equal(await page.evaluate(() => document.querySelector('.ap-org-unit-scope h3').textContent === window.unitScope.copy().unitScopeTitle), true);
   assert.equal(await page.evaluate(() => window.unitScope.calls.filter(call => /\/(units|members)(\?|$)/.test(call.path)).every(call => call.path.includes('root_unit_id=') || call.path.includes('unit_id='))), true);
   assert.equal(await page.evaluate(() => document.querySelector('.ap-org-unit-scope').textContent.includes('Outside B')), false);
   assert.equal(await page.evaluate(() => [...document.querySelectorAll('.ap-org-unit-scope h4')].some(item => item.textContent === window.unitScope.copy().replaceRoles || item.textContent === window.unitScope.copy().allocate)), false);
  });
  await run('Auditor plus Unit Admin keeps global read and limits writes to selected subtree', async () => {
   await fresh({ kind: 'mixed' }); await ready(); await page.waitForFunction(() => document.body.textContent.includes('Outside B'));
   assert.equal(await page.evaluate(() => window.unitScope.calls.some(call => /\/units$/.test(call.path))), true);
   assert.equal(await page.evaluate(() => window.unitScope.calls.some(call => call.path.includes('root_unit_id='))), true);
   assert.equal(await page.evaluate(() => [...document.querySelectorAll('form')].filter(element => element.querySelector('h4')?.textContent === window.unitScope.copy().createUnit).every(element => element.closest('.ap-org-unit-scope'))), true);
  });
  await run('Disjoint bindings switch query scope without merging or retaining targets', async () => {
   await fresh(); await ready();
   await page.evaluate(() => { const select = document.querySelector('.ap-org-unit-scope > label select'); select.value = window.unitScope.dto.root_c; select.dispatchEvent(new Event('change', { bubbles: true })); });
   await page.waitForFunction(() => [...document.querySelectorAll('.ap-org-unit-scope select')].some(select => select.options.length === 2 && select.options[1].value === window.unitScope.dto.root_c));
   assert.equal(await page.evaluate(() => { const forms = [...document.querySelectorAll('.ap-org-unit-scope form')]; return forms.every(form => [...form.querySelectorAll('option')].every(option => ![window.unitScope.dto.root_a, window.unitScope.dto.child_a].includes(option.value))); }), true);
  });
  await run('Cross-subtree member facts are preserved but never become mutation targets', async () => {
   await fresh({ forgedCrossMember: true }); await ready();
   await page.waitForFunction(() => document.querySelector('.ap-org-unit-scope').textContent.includes('Cross subtree member'));
   assert.equal(await page.evaluate(() => [...document.querySelectorAll('.ap-org-unit-scope form option')].some(option => option.value === window.unitScope.dto.cross_id)), false);
  });
  await run('Scoped create requires a real in-subtree parent and never an organization root', async () => {
   await fresh(); await ready();
   await fill('createUnit', [[0, 'Scoped child']]); await submit('createUnit');
   assert.equal((await writes()).length, 0);
   const parent = await page.evaluate(() => window.unitScope.dto.child_a);
   await fill('createUnit', [[1, parent]]); await submit('createUnit');
   await page.waitForFunction(() => window.unitScope.calls.some(call => call.method === 'POST'));
   const mutation = (await writes())[0]; assert.equal(mutation.body.parent_id, parent); assert.equal(mutation.body.name, 'Scoped child');
  });
  await run('Scoped invitation is MEMBER-only, requires attachment, and has no forbidden revoke', async () => {
   await fresh(); await ready();
   await fill('invite', [[0, 'LINK']]); await submit('invite'); assert.equal((await writes()).length, 0);
   const unit = await page.evaluate(() => window.unitScope.dto.child_a);
   await fill('invite', [[2, [unit]]]); await submit('invite');
   await page.waitForFunction(() => document.querySelector('.ap-org-unit-scope input[type=password]'));
   const mutation = (await writes())[0]; assert.deepEqual(mutation.body.roles, ['MEMBER']); assert.deepEqual(mutation.body.unit_ids, [unit]);
   assert.equal(await page.evaluate(() => [...document.querySelectorAll('.ap-org-unit-scope button')].some(button => button.textContent === window.unitScope.copy().revoke)), false);
  });
  await run('A root switch retires an already displayed member confirmation', async () => {
   await fresh(); await ready(); const target = await page.evaluate(() => window.unitScope.dto.target_id);
   await fill('replaceMemberUnits', [[0, target], [1, [await page.evaluate(() => window.unitScope.dto.child_a)]]]); await submit('replaceMemberUnits');
   await page.waitForSelector('[role=alertdialog]');
   await page.evaluate(() => { const button = document.querySelector('[role=alertdialog] button:last-child'); window.unitScope.oldConfirm = button[Object.keys(button).find(key => key.startsWith('__reactProps$'))].onClick; const select = document.querySelector('.ap-org-unit-scope > label select'); select.value = window.unitScope.dto.root_c; select.dispatchEvent(new Event('change', { bubbles: true })); window.unitScope.oldConfirm(); });
   assert.equal((await writes()).length, 0); await page.waitForFunction(() => !document.querySelector('[role=alertdialog]'));
  });
  await run('Revocation notification retires confirmation synchronously before parent refresh', async () => {
   await fresh(); await ready(); const target = await page.evaluate(() => window.unitScope.dto.target_id);
   await fill('replaceMemberUnits', [[0, target], [1, [await page.evaluate(() => window.unitScope.dto.child_a)]]]); await submit('replaceMemberUnits'); await page.waitForSelector('[role=alertdialog]');
   await page.evaluate(() => { const button = document.querySelector('[role=alertdialog] button:last-child'); const confirm = button[Object.keys(button).find(key => key.startsWith('__reactProps$'))].onClick; window.unitScope.revoked = true; for (const fn of window.unitScope.listeners.authorization) fn({ organization_authorization_version: window.unitScope.dto.unit.context.organization_authorization_version + 1 }); confirm(); });
   assert.equal((await writes()).length, 0); await page.waitForFunction(() => !document.querySelector('.ap-org-unit-scope form'));
  });
  await run('A fresh Me snapshot rechecks scheduled binding expiry even without a version increment', async () => {
   await fresh(); await ready();
   await page.evaluate(() => { window.unitScope.expired = true; window.unitScope.currentMe = null; window.unitScope.render(); });
   await page.waitForFunction(() => window.unitScope.calls.filter(call => call.path.endsWith('/authorization-context')).length === 2 && !document.querySelector('.ap-org-unit-scope form'));
   assert.equal((await writes()).length, 0);
  });
  await run('Member replacement binds the full selected scope and exact current member version', async () => {
   await fresh(); await ready();
   const values = await page.evaluate(() => ({ user: window.unitScope.dto.target_id, unit: window.unitScope.dto.root_a, version: window.unitScope.dto.unit.members_a.items.find(member => member.user_id === window.unitScope.dto.target_id).version }));
   await fill('replaceMemberUnits', [[0, values.user], [1, [values.unit]]]); await submit('replaceMemberUnits'); await page.waitForSelector('[role=alertdialog]');
   await page.evaluate(() => { const button = document.querySelector('[role=alertdialog] button:last-child'); button.click(); button.click(); });
   await page.waitForFunction(() => window.unitScope.calls.some(call => call.method === 'PUT'));
   const mutations = await writes(); assert.equal(mutations.length, 1); assert.deepEqual(mutations[0].body, { unit_ids: [values.unit], expected_version: values.version });
  });
  await run('Unknown scoped writes retain original facts and key across language change', async () => {
   await fresh({ unknown: true }); await ready(); const parent = await page.evaluate(() => window.unitScope.dto.child_a);
   await fill('createUnit', [[0, 'Unknown scoped child'], [1, parent]]); await submit('createUnit');
   await page.waitForFunction(() => [...document.querySelectorAll('.ap-org-unit-scope [role=alert]')].some(element => element.textContent === window.unitScope.copy().unknown));
   await page.evaluate(() => { window.unitScope.unknown = false; window.unitScope.language = 'ja'; window.unitScope.render(); });
   await submit('createUnit'); await page.waitForFunction(() => window.unitScope.calls.filter(call => call.method === 'POST').length === 2);
   const mutations = await writes(); assert.equal(mutations[0].key, mutations[1].key); assert.deepEqual(mutations[0].body, mutations[1].body);
  });
  await run('Pagination retains selected unit scope and aborts the old page after switching roots', async () => {
   await fresh({ pagination: true }); await ready();
   await page.evaluate(() => { window.unitScope.delayed = 'page'; [...document.querySelectorAll('.ap-org-unit-scope button')].find(button => button.textContent === window.unitScope.copy().loadMoreMembers).click(); });
   await page.waitForFunction(() => window.unitScope.release);
   await page.evaluate(() => { window.unitScope.oldPage = window.unitScope.release; window.unitScope.delayed = ''; const select = document.querySelector('.ap-org-unit-scope > label select'); select.value = window.unitScope.dto.root_c; select.dispatchEvent(new Event('change', { bubbles: true })); });
   await page.waitForFunction(() => window.unitScope.calls.some(call => call.path.includes(`unit_id=${window.unitScope.dto.root_c}`)));
   await page.evaluate(() => window.unitScope.oldPage());
   assert.equal(await page.evaluate(() => window.unitScope.calls.find(call => call.path.includes('cursor=controlled-scope-page-2'))?.aborted()), true);
   assert.equal(await page.evaluate(() => [...document.querySelectorAll('.ap-org-unit-scope form option')].some(option => option.value === window.unitScope.dto.target_id)), false);
  });
  await run('A cancelled old subtree response cannot populate a newly selected root', async () => {
   await fresh({ delayed: 'tree' }); await page.waitForFunction(() => window.unitScope.release && document.querySelector('.ap-org-unit-scope > label select'));
   await page.evaluate(() => { window.unitScope.oldTree = window.unitScope.release; window.unitScope.delayed = ''; const select = document.querySelector('.ap-org-unit-scope > label select'); select.value = window.unitScope.dto.root_c; select.dispatchEvent(new Event('change', { bubbles: true })); });
   await page.waitForFunction(() => window.unitScope.calls.some(call => call.path.includes(`unit_id=${window.unitScope.dto.root_c}`)));
   await page.evaluate(() => window.unitScope.oldTree());
   assert.equal(await page.evaluate(() => [...document.querySelectorAll('.ap-org-unit-scope form option')].some(option => option.value === window.unitScope.dto.child_a)), false);
  });
  for (const failed of [403, 503]) await run(`Authority read ${failed} fails closed without a flat-permission fallback`, async () => {
   await fresh({ failed }); await page.waitForFunction(() => document.querySelector('.ap-org-unit-scope [role=alert]'));
   assert.equal(await page.evaluate(() => window.unitScope.calls.some(call => call.path.includes('root_unit_id='))), false);
  });
  for (const mismatch of ['context_version', 'organization_authorization_version', 'organization_id']) await run(`Mismatched authority ${mismatch} cannot issue scope reads`, async () => {
   await fresh({ mismatch }); await page.waitForFunction(() => document.querySelector('.ap-org-unit-scope [role=alert]'));
   assert.equal(await page.evaluate(() => window.unitScope.calls.some(call => call.path.includes('root_unit_id=') || call.path.includes('unit_id='))), false);
  });
  await run('Session retirement ignores an authority response even when transport resolves late', async () => {
   await fresh({ delayed: 'context' }); await page.waitForFunction(() => window.unitScope.release);
   await page.evaluate(() => { window.unitScope.client.financeSessionEpoch++; window.unitScope.release(); });
   await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
   assert.equal(await page.evaluate(() => window.unitScope.calls.some(call => call.path.includes('root_unit_id='))), false);
  });
  for (const kind of ['member', 'auditor', 'admin']) await run(`${kind}: organization-wide original behavior does not acquire unit writes`, async () => {
   await fresh({ kind }); await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
   assert.equal(await page.evaluate(() => Boolean(document.querySelector('.ap-org-unit-scope form'))), false);
   if (kind === 'admin') assert.equal(await page.evaluate(() => window.unitScope.calls.some(call => call.path.endsWith('/authorization-context'))), false);
  });
  return results;
 } finally { page.off('pageerror', errorListener); }
}
