import React, { useCallback, useEffect, useRef, useState } from 'react';
import { useAdminOperation } from './use-admin-operation.js';
import { newRequestUUID } from './admin-client.js';

const fixedRoles = new Set(['OWNER','MEMBER','PLATFORM_SUPER_ADMIN']);
function collection(value) { if (!Array.isArray(value?.items)) throw new TypeError('invalid collection'); return value.items; }
function date(value) { return value ? new Date(value).toLocaleString() : '?'; }
function iso(value) { return value ? new Date(value).toISOString() : null; }
function validID(value) { return typeof value === 'string' && /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i.test(value) && value !== '00000000-0000-0000-0000-000000000000'; }
function Feedback({ operation, copy }) { return <>{operation.failed && <p role="alert" className="adm-alert">{copy.failed}</p>}{operation.noticeKey && <p role="status">{copy[operation.noticeKey]}</p>}</>; }
function Grid({ columns, items, id, empty }) { return items.length ? <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{columns.map(([key,title]) => <th key={key}>{title}</th>)}</tr></thead><tbody>{items.map(item => <tr key={item[id]}>{columns.map(([key,,render]) => <td key={key}>{render ? render(item) : item[key] ?? '?'}</td>)}</tr>)}</tbody></table></div> : <p className="adm-empty">{empty}</p>; }

export function AdminRoleBindings({ client, copy:allCopy, onAuthorizationError }) {
  const copy = allCopy.roleBindings;
  const operation = useAdminOperation(onAuthorizationError, 'account');
  const [items, setItems] = useState([]);
  const [grantRoles, setGrantRoles] = useState([]);
  const [form, setForm] = useState({ user_id:'', role_code:'ORG_ADMIN', organization_id:'', unit_id:'', starts_at:'', expires_at:'' });
  const pending = useRef(null);
  const load = useCallback(async ({ signal, isCurrent }) => {
    const [bindings, definitions] = await Promise.all([client.roleBindings(signal), client.roles(signal)]);
    const next = collection(bindings);
    const roles = collection(definitions).filter(role => role.assignable === true && typeof role.code === 'string' && !fixedRoles.has(role.code)).map(role => role.code);
    if (isCurrent()) { setItems(next); setGrantRoles(roles); setForm(current => ({ ...current, role_code:roles.includes(current.role_code) ? current.role_code : roles[0] || '' })); }
  }, [client]);
  useEffect(() => { void operation.run(load); return operation.invalidate; }, [load, operation.completed, operation.run, operation.invalidate]);
  const grant = event => {
    event.preventDefault(); if (operation.busy || !grantRoles.includes(form.role_code)) return;
    void operation.run(async () => {
      const input = { ...form, user_id:form.user_id.trim(), organization_id:form.organization_id.trim() || null, unit_id:form.unit_id.trim() || null, starts_at:iso(form.starts_at), expires_at:iso(form.expires_at) };
      const facts = JSON.stringify(input);
      if (pending.current?.facts !== facts) pending.current = { facts, key:`admin-ui-${newRequestUUID()}` };
      const key = pending.current.key;
      const result = await client.grantRole({ ...input, idempotency_key:key });
      if (!validID(result?.binding_id) || result.user_id !== input.user_id || result.role_code !== input.role_code) throw new TypeError('invalid grant');
      if (pending.current?.key === key) pending.current = null;
    }, true);
  };
  const field = (name, label, type = 'text') => <label>{label}<input type={type} disabled={operation.busy} value={form[name]} onChange={event => setForm(current => ({ ...current, [name]:event.target.value }))} required={name === 'user_id'} /></label>;
  return <section className="adm-panel"><h2>{copy.title}</h2><p>{copy.lead}</p><form className="adm-create-row" onSubmit={grant}>{field('user_id',copy.user)}<label>{copy.role}<select disabled={operation.busy || !grantRoles.length} value={form.role_code} onChange={event => setForm(current => ({ ...current, role_code:event.target.value }))}>{grantRoles.map(role => <option key={role}>{role}</option>)}</select></label>{field('organization_id',copy.organization)}{field('unit_id',copy.unit)}{field('starts_at',copy.starts,'datetime-local')}{field('expires_at',copy.expires,'datetime-local')}<button disabled={operation.busy || !grantRoles.includes(form.role_code)}>{copy.grant}</button></form><button disabled={operation.busy} onClick={() => void operation.run(load)}>{copy.refresh}</button><Feedback operation={operation} copy={copy} /><Grid items={items} id="binding_id" empty={copy.none} columns={[
    ['binding_id',copy.binding],['user_id',copy.user],['role_code',copy.role],['scope',copy.scope],['organization_id',copy.organization],['unit_id',copy.unit],['validity',copy.validity,item => `${date(item.starts_at)} / ${item.expires_at ? date(item.expires_at) : copy.forever}`],['grant_source',copy.source],['status',copy.status],['actions',copy.actions,item => !fixedRoles.has(item.role_code) && ['ACTIVE','SCHEDULED'].includes(item.status) && <button disabled={operation.busy} onClick={() => void operation.run(() => client.revokeRole(item.binding_id),true)}>{copy.revoke}</button>],
  ]} /></section>;
}

export function AdminRegistrationRepair({ client, copy:allCopy, onAuthorizationError }) {
  const copy = allCopy.registrationRepair;
  const operation = useAdminOperation(onAuthorizationError, 'account');
  const [filters, setFilters] = useState({ status:'', stage:'', responsibility:'', sla:'', older_than_minutes:'' });
  const [submitted, setSubmitted] = useState(filters);
  const [items, setItems] = useState([]);
  const load = useCallback(({ signal,isCurrent }) => client.registrationOperations(submitted,signal).then(value => { const next = collection(value); if (isCurrent()) setItems(next); }),[client,submitted]);
  useEffect(() => { void operation.run(load); return operation.invalidate; },[load,operation.completed,operation.run,operation.invalidate]);
  const definitions = [['status',['REPAIR_REQUIRED','COMPENSATION_PENDING']],['stage',['IDENTITY_CREATING','ACCOUNT_COMMITTING','WALLET_PROVISIONING','COMPENSATION']],['responsibility',['IDENTITY','ACCOUNT','BILLING']],['sla',['OVERDUE','WITHIN']]];
  return <section className="adm-panel"><h2>{copy.title}</h2><p>{copy.description}</p><form className="adm-create-row" onSubmit={event => { event.preventDefault(); if (!operation.busy) setSubmitted({ ...filters }); }}>{definitions.map(([name,values]) => <label key={name}>{copy[name]}<select disabled={operation.busy} value={filters[name]} onChange={event => setFilters(current => ({ ...current,[name]:event.target.value }))}><option value="">{copy.all}</option>{values.map(value => <option key={value}>{value}</option>)}</select></label>)}<label>{copy.age}<input disabled={operation.busy} type="number" min="0" max="43200" value={filters.older_than_minutes} onChange={event => setFilters(current => ({ ...current,older_than_minutes:event.target.value }))} /></label><button disabled={operation.busy}>{copy.refresh}</button></form><Feedback operation={operation} copy={copy} /><Grid items={items} id="registration_operation_id" empty={copy.empty} columns={[
    ['registration_operation_id',copy.operation],['status',copy.status],['stage',copy.stage],['responsibility',copy.responsibility],['attempts',copy.attempts],['reason',copy.reason],['next_retry_at',copy.nextRetry,item => date(item.next_retry_at)],['sla_expires_at',copy.sla,item => date(item.sla_expires_at)],['updated_at',copy.updated,item => date(item.updated_at)],['actions',copy.actions,item => <>{item.status === 'REPAIR_REQUIRED' && <button disabled={operation.busy} onClick={() => void operation.run(() => client.repairRegistration(item.registration_operation_id,'retry'),true)}>{copy.retry}</button>}{item.compensation_allowed === true && <button disabled={operation.busy} onClick={() => void operation.run(() => client.repairRegistration(item.registration_operation_id,'compensate'),true)}>{copy.compensate}</button>}</>],
  ]} /></section>;
}

function safeAmount(value) {
  if (!/^-?[0-9]+$/.test(value)) throw new TypeError('invalid amount');
  const amount = BigInt(value);
  if (amount === 0n || amount > BigInt(Number.MAX_SAFE_INTEGER) || amount < BigInt(Number.MIN_SAFE_INTEGER)) throw new TypeError('invalid amount');
  return Number(amount);
}
export function AdminApprovals({ client, copy:allCopy, onAuthorizationError, initialTarget = '' }) {
  const copy = allCopy.approvals;
  const operation = useAdminOperation(onAuthorizationError, 'account');
  const [items,setItems] = useState([]);
  const [filters,setFilters] = useState({ command_type:'',status:'',risk_class:'',requester_user_id:'',target:initialTarget,submitted_from:'',submitted_to:'',expires_from:'',expires_to:'' });
  const [submitted,setSubmitted] = useState(filters);
  useEffect(() => { setFilters(current => ({ ...current,target:initialTarget })); setSubmitted(current => ({ ...current,target:initialTarget })); },[initialTarget]);
  const [form,setForm] = useState({ wallet_id:'', amount:'', reason:'' });
  const pending = useRef(null);
  const load = useCallback(({ signal,isCurrent }) => client.approvalRequests(submitted,signal).then(value => { const next = collection(value); if (isCurrent()) setItems(next); }),[client,submitted]);
  useEffect(() => { void operation.run(load); return operation.invalidate; },[load,operation.completed,operation.run,operation.invalidate]);
  const create = event => {
    event.preventDefault(); if (operation.busy) return;
    void operation.run(async ({ isCurrent }) => {
      const amount = safeAmount(form.amount);
      const facts = JSON.stringify([form.wallet_id.trim(),amount,form.reason.trim()]);
      if (pending.current?.facts !== facts) pending.current = { facts,key:newRequestUUID() };
      const attempt = pending.current;
      const key = attempt.key;
      if (!attempt.draft) {
        const adjustment = await client.createAdjustment({ wallet_id:form.wallet_id.trim(),amount_microcredits:amount,reason:form.reason.trim(),idempotency_key:key });
        if (!isCurrent()) return;
        if (adjustment.wallet_id !== form.wallet_id.trim() || adjustment.amount_microcredits !== amount || !validID(adjustment.adjustment_id) || !Number.isSafeInteger(adjustment.version) || adjustment.version < 1 || !validID(adjustment.requested_by)) throw new TypeError('invalid adjustment');
        // Preserve the authorized facts across lost create/submit acknowledgements.
        // A posted adjustment's current version must not replace this first snapshot.
        attempt.draft = { command_type:'WALLET_ADJUSTMENT',idempotency_key:`approval:${key}`,
          normalized_command:{ adjustment_id:adjustment.adjustment_id,wallet_id:form.wallet_id.trim(),amount_microcredits:amount,requested_by:adjustment.requested_by },
          target_resource_type:'MANUAL_ADJUSTMENT',target_resource_id:adjustment.adjustment_id,reason:form.reason.trim(),redacted_evidence:[],
          expected_impact:{ wallet_id:form.wallet_id.trim(),delta_microcredits:amount },critical_resource_versions:{ adjustment_version:adjustment.version },
          financial_snapshot:{ wallet_id:form.wallet_id.trim(),delta_microcredits:amount } };
      }
      const approval = await client.createApproval(attempt.draft);
      if (!isCurrent()) return;
      if (!validID(approval.approval_request_id) || !Number.isSafeInteger(approval.version) || approval.version < 1) throw new TypeError('invalid approval');
      const existing = approval.status !== 'DRAFT';
      if (approval.status === 'DRAFT') {
        const submittedResult = await client.approvalAction(approval.approval_request_id,'submit',{ expected_version:approval.version });
        if (submittedResult.approval_request_id !== approval.approval_request_id || submittedResult.status !== 'SUBMITTED' || !Number.isSafeInteger(submittedResult.version) || submittedResult.version < 1) throw new TypeError('invalid submitted approval');
      }
      else if (!['SUBMITTED','APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED','REJECTED','WITHDRAWN','EXPIRED','INVALIDATED'].includes(approval.status)) throw new TypeError('invalid approval status');
      if (pending.current?.key === key) pending.current = null;
      if (isCurrent()) setForm({ wallet_id:'',amount:'',reason:'' });
      return existing ? 'existing' : 'created';
    },true);
  };
  const act = (item, action) => {
    if (operation.busy) return;
    const reason = action === 'WITHDRAW' ? '' : window.prompt(copy.reason);
    if (reason === null || (action !== 'WITHDRAW' && !reason.trim())) return;
    void operation.run(() => client.approvalAction(item.approval_request_id,action === 'WITHDRAW' ? 'withdraw' : 'decide',action === 'WITHDRAW' ? { expected_version:item.version } : { expected_version:item.version,decision:action === 'APPROVE' ? 'APPROVED' : 'REJECTED',reason:reason.trim(),observed_versions:item.critical_resource_versions || {} }),true);
  };
  return <section className="adm-panel"><h2>{copy.title}</h2><p>{copy.lead}</p><form className="adm-create-row" onSubmit={event => { event.preventDefault(); if (!operation.busy) setSubmitted({ ...filters, ...Object.fromEntries(['submitted_from','submitted_to','expires_from','expires_to'].map(key => [key,iso(filters[key]) || ''])) }); }}>{Object.keys(filters).map(name => <label key={name}>{({command_type:copy.type,status:copy.status,risk_class:copy.risk,requester_user_id:copy.requester,target:copy.target,submitted_from:copy.submittedFrom,submitted_to:copy.submittedTo,expires_from:copy.expiresFrom,expires_to:copy.expiresTo})[name]}<input disabled={operation.busy} type={name.includes('_from') || name.includes('_to') ? 'datetime-local' : 'text'} value={filters[name]} onChange={event => setFilters(current => ({ ...current,[name]:event.target.value }))} /></label>)}<button disabled={operation.busy}>{copy.refresh}</button></form><form className="adm-create-row" onSubmit={create}><label>{copy.wallet}<input disabled={operation.busy} required value={form.wallet_id} onChange={event => setForm(current => ({ ...current,wallet_id:event.target.value }))} /></label><label>{copy.amount}<input disabled={operation.busy} required inputMode="numeric" value={form.amount} onChange={event => setForm(current => ({ ...current,amount:event.target.value }))} /></label><label>{copy.requestReason}<input disabled={operation.busy} required minLength={10} value={form.reason} onChange={event => setForm(current => ({ ...current,reason:event.target.value }))} /></label><button disabled={operation.busy}>{copy.create}</button></form><Feedback operation={operation} copy={copy} /><Grid items={items} id="approval_request_id" empty={copy.empty} columns={[
    ['command_type',copy.type],['status',copy.status],['risk_class',copy.risk],['requester_user_id',copy.requester],['target_resource_id',copy.target],['times',copy.submitted,item => `${date(item.submitted_at)} / ${date(item.expires_at)}`],['policy_version',copy.policy],['facts',copy.command,item => <details><summary>{copy.command}</summary>{[['expected_impact',copy.impact],['normalized_command',copy.command],['critical_resource_versions',copy.versions],['redacted_evidence',copy.evidence]].map(([key,label]) => <div key={key}><strong>{label}</strong><pre>{JSON.stringify(item[key] || {},null,2)}</pre></div>)}</details>],['actions',copy.actions,item => (Array.isArray(item.allowed_actions) ? item.allowed_actions : []).filter(action => ['APPROVE','REJECT','WITHDRAW'].includes(action)).map(action => <button key={action} disabled={operation.busy} onClick={() => act(item,action)}>{copy[action.toLowerCase()]}</button>)],
  ]} /></section>;
}
