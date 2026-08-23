import { registrationRepairCopies as copies } from './registration-repair-i18n.js';

const root = document.querySelector('#registration-repair-root');
const state = { language: navigator.language.startsWith('ja') ? 'ja' : navigator.language.startsWith('en') ? 'en' : 'zh', items: [], filters: {}, busy: false, error: '' };

function element(name, text, className) {
  const node = document.createElement(name);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}

function field(kind, values, value) {
  const node = document.createElement(kind);
  if (kind === 'select') {
    for (const [key, label] of values) {
      const option = element('option', label);
      option.value = key;
      node.append(option);
    }
  }
  node.value = value;
  return node;
}

async function request(path, method = 'GET') {
  const session = await fetch('/api/session', { credentials: 'same-origin', headers: { Accept: 'application/json' } });
  if (!session.ok) throw new Error('authentication_required');
  const sessionBody = await session.json();
  const response = await fetch(path, {
    method, credentials: 'same-origin', headers: { Accept: 'application/json', ...(method === 'POST' ? { 'X-CSRF-Token': sessionBody.csrf_token } : {}) },
  });
  if (!response.ok) {
    const problem = await response.json().catch(() => ({}));
    throw new Error(problem.code || `HTTP_${response.status}`);
  }
  return response.status === 204 ? null : response.json();
}

async function load() {
  if (state.busy) return;
  const parameters = new URLSearchParams();
  for (const id of ['status', 'stage', 'responsibility', 'sla', 'older_than_minutes']) {
    const value = root.querySelector(`[name="${id}"]`)?.value ?? state.filters[id] ?? '';
    state.filters[id] = value;
    if (value) parameters.set(id, value);
  }
  state.busy = true; state.error = ''; render();
  try {
    const body = await request(`/api/admin/account/admin/v1/registration-operations?${parameters}`);
    state.items = body.items || [];
  } catch (error) { state.error = error.message; }
  finally { state.busy = false; render(); }
}

async function act(operationID, action) {
  if (state.busy) return;
  state.busy = true; state.error = ''; render();
  let actionError = '';
  try { await request(`/api/admin/account/admin/v1/registration-operations/${encodeURIComponent(operationID)}/actions/${action}`, 'POST'); }
  catch (error) { actionError = error.message; }
  state.busy = false;
  await load();
  if (actionError) { state.error = actionError; render(); }
}

function render() {
  const copy = copies[state.language];
  root.replaceChildren();
  const section = element('section', undefined, 'registration-repair');
  const header = element('header');
  header.append(element('h2', copy.title));
  const language = field('select', [['zh', '简体中文'], ['en', 'English'], ['ja', '日本語']], state.language);
  language.setAttribute('aria-label', copy.language);
  language.onchange = () => { state.language = language.value; render(); };
  header.append(language); section.append(header, element('p', copy.description, 'muted'));

  const filters = element('div', undefined, 'filters');
  const definitions = [
    ['status', copy.status, [['', copy.all], ['REPAIR_REQUIRED', 'REPAIR_REQUIRED'], ['COMPENSATION_PENDING', 'COMPENSATION_PENDING']]],
    ['stage', copy.stage, [['', copy.all], ...['IDENTITY_CREATING', 'ACCOUNT_COMMITTING', 'WALLET_PROVISIONING', 'COMPENSATION'].map((value) => [value, value])]],
    ['responsibility', copy.responsibility, [['', copy.all], ...['IDENTITY', 'ACCOUNT', 'BILLING'].map((value) => [value, value])]],
    ['sla', copy.sla, [['', copy.all], ['OVERDUE', copy.overdue], ['WITHIN', copy.within]]],
  ];
  for (const [name, label, values] of definitions) {
    const select = field('select', values, state.filters[name] || ''); select.name = name; select.setAttribute('aria-label', label); filters.append(select);
  }
  const age = field('input', [], state.filters.older_than_minutes || ''); age.name = 'older_than_minutes'; age.type = 'number'; age.min = '0'; age.max = '43200'; age.placeholder = copy.age; filters.append(age);
  const refresh = element('button', state.busy ? copy.loading : copy.refresh); refresh.disabled = state.busy; refresh.onclick = load; filters.append(refresh); section.append(filters);
  if (state.error) section.append(element('p', `${copy.failed}: ${state.error}`, 'error'));
  if (!state.items.length) section.append(element('p', state.busy ? copy.loading : copy.empty, 'muted'));
  else {
    const table = element('table');
    const head = element('tr');
    for (const label of [copy.operation, copy.status, copy.stage, copy.responsibility, copy.attempts, copy.reason, copy.nextRetry, copy.sla, copy.updated, copy.actions]) head.append(element('th', label));
    table.append(element('thead')); table.tHead.append(head); const body = element('tbody');
    for (const item of state.items) {
      const row = element('tr');
      row.append(element('td', item.registration_operation_id, 'operation-id'), element('td', item.status), element('td', item.stage), element('td', item.responsibility), element('td', String(item.attempts)), element('td', item.reason || '—'), element('td', item.next_retry_at || '—'), element('td', item.sla_expires_at || '—'), element('td', item.updated_at || '—'));
      const actions = element('td', undefined, 'actions');
      if (item.status === 'REPAIR_REQUIRED') { const retry = element('button', copy.retry); retry.disabled = state.busy; retry.onclick = () => act(item.registration_operation_id, 'retry'); actions.append(retry); }
      if (item.compensation_allowed) { const compensate = element('button', copy.compensate); compensate.disabled = state.busy; compensate.onclick = () => act(item.registration_operation_id, 'compensate'); actions.append(compensate); }
      row.append(actions); body.append(row);
    }
    table.append(body); section.append(table);
  }
  root.append(section);
}

render();
load();
