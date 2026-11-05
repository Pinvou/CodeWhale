// Opt-in test fixture only. The production React view and client are real;
// identity, Account responses and timing are controlled public fictional data.
import './account-platform-f01-ui.jsx';

const originalFetch = globalThis.fetch;
const state = { calls: [], fail: false, delay: false, release: null, active: false, holdStatus: false, releaseStatus: null };
const controls = document.createElement('aside');
controls.setAttribute('aria-label', 'Controlled lookup fixture');
const output = document.createElement('pre');
output.id = 'lookup-fixture-calls';
const report = () => { output.textContent = JSON.stringify(state.calls); };
const button = (label, action) => {
  const element = document.createElement('button');
  element.type = 'button'; element.textContent = label;
  element.onclick = action; controls.append(element);
};
button('Fixture: success', () => { state.fail = false; });
button('Fixture: deny next lookups', () => { state.fail = true; });
button('Fixture: delay next lookup', () => { state.delay = true; });
button('Fixture: release delayed lookup', () => { state.release?.(); state.release = null; });
button('Fixture: ACTIVE lookup target', () => { state.active = true; state.fail = false; });
button('Fixture: hold next status write', () => { state.holdStatus = true; });
button('Fixture: release status write', () => { state.releaseStatus?.(); state.releaseStatus = null; });
controls.append(output); document.body.prepend(controls);

globalThis.fetch = (url, options) => {
  if (!String(url).includes('/user-code-lookups/')) {
    if (String(url).includes('/admin/v1/users')) {
      state.calls.push({ url, method: options?.method || 'GET' }); report();
      if (String(url).endsWith('/status') && state.holdStatus) {
        state.holdStatus = false;
        return new Promise(resolve => { state.releaseStatus = () => resolve(originalFetch(url, options)); });
      }
    }
    if (String(url).includes('/admin/v1/users?')) return Promise.resolve(originalFetch(url, options)).then(async response => {
      if (!response.ok) return response;
      const body = await response.json();
      return new Response(JSON.stringify({ ...body, items: body.items.map(item => ({ ...item, user_code: 'USR-1111-1111-1111-1111-11' })) }), { status: response.status, headers: { 'Content-Type': 'application/json' } });
    });
    return originalFetch(url, options);
  }
  const code = decodeURIComponent(String(url).split('/').at(-1));
  state.calls.push({ url, method: options?.method || 'GET' }); report();
  const response = name => new Response(JSON.stringify({ user_id: window.f01.id, user_code: code.toUpperCase(), username: name, status: state.active ? 'ACTIVE' : 'DELETED' }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  if (state.fail) return Promise.resolve(new Response(JSON.stringify({ code: 'USER_NOT_FOUND', message: 'the user lookup could not be completed' }), { status: 404 }));
  if (state.delay) {
    state.delay = false;
    return new Promise(resolve => { state.release = () => resolve(response('lookup-stale')); });
  }
  return Promise.resolve(response('lookup-current'));
};
window.f01.render('admin');
