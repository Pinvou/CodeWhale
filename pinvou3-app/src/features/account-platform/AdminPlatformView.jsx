import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createContextRequestGuard } from './context-guard.js';
import { buildUnitTree } from './admin-organization-tree.js';
import { formatMoney, formatPoints, subtractMicrocredits, parseScaledAmount } from './credit-units.js';
import { useAdminOperation } from './use-admin-operation.js';
import { createAdminMutationOwner } from './admin-operation-scope.js';
import { AdminMutationContext, useAdminMutationOwner } from './admin-mutation-context.js';
import { auditCSV } from './audit-csv.js';
import './admin-platform.css';
import { AdminRoleBindings, AdminRegistrationRepair, AdminApprovals } from './AdminOperations.jsx';
import { AdminPricing } from './AdminPricing.jsx';
import { AdminFinancialOperations, AdminPaymentOrderDetail, AdminUsageHoldDetail } from './AdminFinancialOperations.jsx';
import { AdminReconciliationCases } from './AdminReconciliation.jsx';
import { requireAdminRecord, requireAdminFacts } from './admin-acknowledgements.js';

const NAV_ICONS = { overview:'⌂', organizations:'▦', users:'◎', billing:'◇', tokens:'↗', system:'◉', audit:'≡', identity:'⚿' };
const okStatuses = new Set(['ACTIVE', 'PAID', 'SETTLED', 'HEALTHY', 'APPROVED', 'SUCCESS']);
const warnStatuses = new Set(['PENDING', 'CREATED', 'SETTLEMENT_PENDING', 'REVIEW', 'MANUAL_REVIEW', 'PENDING_APPROVAL']);

function formatNumber(value) { return Number(value || 0).toLocaleString(); }
function formatDate(value) { return value ? new Date(value).toLocaleString() : '—'; }
function shortID(value) { const text = String(value || ''); return text.length > 16 ? `${text.slice(0, 8)}…${text.slice(-5)}` : text || '—'; }
function formatSummary(value) { return value && typeof value === 'object' ? Object.entries(value).map(([key, item]) => `${key}: ${item}`).join(' · ') : String(value || '—'); }
function userLabel(user) { return user?.username || user?.display_name || user?.email || shortID(user?.user_id); }

function Status({ value }) {
  const status = String(value || '—');
  const tone = okStatuses.has(status) ? 'ok' : warnStatuses.has(status) ? 'warn' : status === '—' ? '' : 'neutral';
  return <span className={`adm-status ${tone}`}>{status}</span>;
}

function Metric({ label, value, tone = '', formatter = formatNumber }) {
  return <article className={`adm-metric ${tone}`}><span>{label}</span><strong>{formatter(value)}</strong></article>;
}

function Empty({ copy }) { return <div className="adm-empty">{copy.noData}</div>; }

function Table({ columns, items, copy, rowKey }) {
  if (!items?.length) return <Empty copy={copy} />;
  return <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{columns.map(column => <th key={column.key}>{column.label}</th>)}</tr></thead><tbody>{items.map((item, index) => <tr key={item[rowKey] ?? index}>{columns.map(column => <td key={column.key}>{column.render ? column.render(item) : item[column.key] ?? '—'}</td>)}</tr>)}</tbody></table></div>;
}

function SectionHeader({ title, count, copy, children }) {
  return <header className="adm-section-head"><div><h2>{title}</h2>{count !== undefined && <span>{copy.items(count)}</span>}</div>{children}</header>;
}

function Tabs({ items, active, onChange }) {
  return <div className="adm-tabs">{items.map(item => <button key={item.key} className={active === item.key ? 'active' : ''} onClick={() => onChange(item.key)}>{item.label}</button>)}</div>;
}

export function AdminPlatformView({ client, copy, language, onLanguageChange }) {
  const [auth, setAuth] = useState('loading');
  const [section, setSection] = useState('overview');
  const [approvalTarget, setApprovalTarget] = useState('');
  const [data, setData] = useState({});
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const busy = loading || loggingOut;
  const [updatedAt, setUpdatedAt] = useState(null);
  const guardRef = useRef(createContextRequestGuard());
  const mutationOwner = useMemo(() => createAdminMutationOwner(), [client]);
  // Parent refreshes and remounted billing views share the last submitted query.
  const billingFilters = useMemo(() => ({ current: { q:'', status:'' } }), [client]);
  const authorizationRef = useRef(false);
  authorizationRef.current = auth === 'ready' && !loggingOut;
  const handleAuthorizationError = useCallback(reason => {
    if (reason?.status !== 401 && reason?.status !== 403) return false;
    if (reason.status === 403 && reason.code !== 'CSRF_REJECTED') { setError(copy.denied); return true; }
    authorizationRef.current = false;
    guardRef.current.cancel();
    setData({}); setUpdatedAt(null); setLoading(false);
    setAuth(reason.status === 401 ? 'guest' : reason.code === 'CSRF_REJECTED' ? 'error' : 'denied');
    return true;
  }, [copy.denied]);

  const load = useCallback(async (target, signal, preservePermissionDenial = false) => {
    const request = guardRef.current.begin();
    const isCurrent = () => guardRef.current.isCurrent(request.generation) && !request.signal.aborted && !signal?.aborted;
    setLoading(true);
    setError(current => preservePermissionDenial && current === copy.denied ? current : '');
    try {
      const resources = target === 'overview' ? {
        accountOverview:client.accountOverview(request.signal), billingOverview:client.billingOverview(request.signal), tokenOverview:client.tokenOverview(request.signal),
      } : target === 'organizations' ? {
        users:client.users('', request.signal), organizations:client.organizations('', request.signal), roles:client.roles(request.signal),
      } : target === 'users' ? {
        users:client.users('', request.signal), roles:client.roles(request.signal),
      } : target === 'billing' ? billingResources(client, request.signal, billingFilters.current) : target === 'tokens' ? {
        tokenAccounts:client.tokenAccounts(request.signal), usage:client.usageRequests(request.signal),
      } : target === 'system' ? { systemStatus:client.systemStatus(request.signal) } : target === 'audit' ? {
        audits:client.audits({}, request.signal),
      } : null;
      if (!resources) return;
      const next = await readAdminResources(resources);
      if (!isCurrent()) return;
      setData(current => mergeAdminResources(current, next));
      const resourceError = Object.values(next.resourceErrors).includes('denied') ? copy.denied : Object.keys(next.resourceErrors).length ? copy.failed : '';
      setError(current => resourceError || (preservePermissionDenial && current === copy.denied ? current : ''));
      setUpdatedAt(Object.keys(next).some(key => key !== 'resourceErrors' && next[key] !== undefined) ? new Date() : null); setAuth('ready');
    } catch (reason) {
      if (reason?.name === 'AbortError' || !isCurrent()) return;
      if (!handleAuthorizationError(reason)) { setAuth(current => current === 'loading' ? 'error' : current); setError(copy.failed); }
    } finally { if (isCurrent()) setLoading(false); }
  }, [client, copy.failed, copy.denied, handleAuthorizationError, billingFilters]);

  const currentViewRef = useRef(null);
  currentViewRef.current = { section, load, mutationOwner };
  useEffect(() => mutationOwner.subscribe(event => {
    if (!event.completed || !authorizationRef.current || currentViewRef.current.mutationOwner !== mutationOwner) return;
    const fields = event.area === 'billing' ? ['wallets','prices','orders','adjustments','holds','reconciliations','billingOverview'] : event.area === 'token' ? ['tokenAccounts','usage','tokenOverview'] : event.area === 'account' ? ['users','organizations','roles','accountOverview'] : [];
    if (!fields.length) return;
    setData(current => ({ ...current, ...Object.fromEntries(fields.map(key => [key, undefined])) }));
    const target = currentViewRef.current.section;
    if ((event.area === 'billing' && target === 'billing') || (event.area === 'token' && target === 'tokens') || (event.area === 'account' && ['organizations','users'].includes(target))) void currentViewRef.current.load(target, undefined, true);
  }), [mutationOwner]);

  useEffect(() => {
    const controller = new globalThis.AbortController();
    client.session(controller.signal).then(session => {
      if (controller.signal.aborted) return;
      if (!session.authenticated) { setAuth('guest'); return; }
      setAuth('ready');
      load('overview', controller.signal);
    }).catch(reason => { if (!controller.signal.aborted && reason?.name !== 'AbortError') { setAuth('error'); setError(copy.failed); } });
    return () => { controller.abort(); guardRef.current.cancel(); };
  }, [client, copy.failed, load]);

  const navigate = (key, target = '') => {
    setApprovalTarget(key === 'approvals' ? target : '');
    setSection(key); setError('');
    if (!sectionHasData(key, data)) void load(key);
    else { guardRef.current.cancel(); setLoading(false); }
  };
  const logout = async () => {
    authorizationRef.current = false;
    guardRef.current.cancel(); setLoggingOut(true); setError('');
    try {
      await client.logout();
      guardRef.current.cancel(); setLoading(false);
      setData({}); setUpdatedAt(null); setAuth('guest');
      window.location.assign('/admin.html');
    } catch (reason) {
      if (!handleAuthorizationError(reason)) setError(copy.failed);
    } finally { setLoggingOut(false); }
  };

  if (auth === 'loading') return <StatePage copy={copy} kind="loading" />;
  if (auth === 'guest') return <StatePage copy={copy} kind="guest" />;
  if (auth === 'denied') return <StatePage copy={copy} kind="denied" />;
  if (auth === 'error') return <StatePage copy={copy} kind="error" onRetry={() => window.location.reload()} />;

  return <AdminMutationContext.Provider value={mutationOwner}><div className="adm-shell">
    <aside className="adm-sidebar">
      <div className="adm-brand"><div className="adm-mark">P</div><div><strong>{copy.title}</strong><span>{copy.product}</span></div></div>
      <nav>{Object.keys(copy.nav).map(key => <button key={key} className={section === key ? 'active' : ''} onClick={() => navigate(key)}><span>{NAV_ICONS[key]}</span>{copy.nav[key]}</button>)}</nav>
      <div className="adm-sidebar-foot"><span>{copy.readOnly}</span><select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}><option value="zh">简体中文</option><option value="en">English</option><option value="ja">日本語</option></select></div>
    </aside>
    <main className="adm-main">
      <header className="adm-topbar"><div><span className="adm-eyebrow">{copy.product}</span><h1>{copy.nav[section]}</h1></div><div className="adm-actions">{updatedAt && !['identity','roleBindings','registrationRepair','approvals'].includes(section) && <small>{copy.updated} {updatedAt.toLocaleTimeString()}</small>}{!['identity','roleBindings','registrationRepair','approvals'].includes(section) && <button onClick={() => load(section)} disabled={busy}>{copy.refresh}</button>}<div className="adm-admin"><span>PA</span><div><strong>{copy.administrator}</strong><button onClick={logout} disabled={loggingOut}>{copy.logout}</button></div></div></div></header>
      {error && <div role="alert" className="adm-alert">{error}</div>}
      {busy && <div className="adm-progress" />}
      {section === 'overview' && !data.resourceErrors?.accountOverview && !data.resourceErrors?.billingOverview && !data.resourceErrors?.tokenOverview && !loading && <Overview copy={copy} language={language} data={data} />}
      {section === 'organizations' && !data.resourceErrors?.organizations && !loading && <AccountDirectory section="organizations" copy={copy} data={data} client={client} setData={setData} onAuthorizationError={handleAuthorizationError} />}
      {section === 'users' && !data.resourceErrors?.users && !loading && <AccountDirectory section="users" copy={copy} data={data} client={client} setData={setData} onAuthorizationError={handleAuthorizationError} />}
      {section === 'billing' && <Billing copy={copy} language={language} data={data} client={client} filters={billingFilters} setData={setData} onAuthorizationError={handleAuthorizationError} onApproval={item => navigate('approvals', item.adjustment_id)} />}
      {section === 'tokens' && <Tokens copy={copy} language={language} data={data} client={client} setData={setData} onAuthorizationError={handleAuthorizationError} />}
      {section === 'system' && !data.resourceErrors?.systemStatus && !loading && <SystemStatus copy={copy} data={data} />}
      {section === 'audit' && !data.resourceErrors?.audits && !loading && <Audit copy={copy} data={data} client={client} setData={setData} onAuthorizationError={handleAuthorizationError} />}
      {section === 'roleBindings' && <AdminRoleBindings client={client} copy={copy} onAuthorizationError={handleAuthorizationError} />}
      {section === 'registrationRepair' && <AdminRegistrationRepair client={client} copy={copy} onAuthorizationError={handleAuthorizationError} />}
      {section === 'approvals' && <AdminApprovals client={client} copy={copy} onAuthorizationError={handleAuthorizationError} initialTarget={approvalTarget} />}
      {section === 'identity' && <IdentitySecurity copy={copy} />}
    </main>
  </div></AdminMutationContext.Provider>;
}

// An independently authorized resource must remain usable when another resource
// in the same screen is denied. Session and CSRF failures still end the whole operation.
async function readAdminResources(resources) {
  const entries = Object.entries(resources);
  const results = await Promise.allSettled(entries.map(([, value]) => value));
  const next = { resourceErrors:{} };
  for (let index = 0; index < entries.length; index += 1) {
    const [key] = entries[index]; const result = results[index];
    if (result.status === 'rejected') {
      const reason = result.reason;
      if (reason?.name === 'AbortError' || reason?.status === 401 || (reason?.status === 403 && reason.code === 'CSRF_REJECTED')) throw reason;
      next[key] = undefined; next.resourceErrors[key] = reason?.status === 403 ? 'denied' : 'failed';
    } else if (key.endsWith('Overview') || key === 'systemStatus') next[key] = result.value;
    else if (Array.isArray(result.value?.items)) next[key] = result.value.items;
    else { next[key] = undefined; next.resourceErrors[key] = 'failed'; }
  }
  return next;
}
function mergeAdminResources(current, next) {
  const resourceErrors = { ...current.resourceErrors };
  for (const key of Object.keys(next)) if (key !== 'resourceErrors') delete resourceErrors[key];
  return { ...current, ...next, resourceErrors:{ ...resourceErrors, ...next.resourceErrors } };
}
function billingResources(client, signal, filters) {
  return { wallets:client.wallets(signal), prices:client.priceVersions(signal), orders:client.paymentOrders(filters, signal), adjustments:client.adjustments(signal), holds:client.holds(signal), reconciliations:client.reconciliationRuns(signal), billingOverview:client.billingOverview(signal) };
}

function sectionHasData(section, data) {
  return ['identity','roleBindings','registrationRepair','approvals'].includes(section) || (section === 'overview' ? data.accountOverview : section === 'organizations' ? data.organizations : section === 'users' ? data.users : section === 'billing' ? data.wallets : section === 'tokens' ? data.tokenAccounts : section === 'system' ? data.systemStatus : data.audits);
}

function StatePage({ copy, kind, onRetry }) {
  const loading = kind === 'loading';
  const title = kind === 'guest' ? copy.loginTitle : kind === 'denied' ? copy.deniedTitle : kind === 'error' ? copy.failed : copy.loading;
  const body = kind === 'guest' ? copy.loginIntro : kind === 'denied' ? copy.denied : '';
  return <main className="adm-state"><div className="adm-state-mark">P</div><span className="adm-eyebrow">{copy.product}</span><h1>{title}</h1>{body && <p>{body}</p>}{loading && <div className="adm-spinner" />}{kind === 'guest' && <a href="/auth/login?return_to=/admin.html">{copy.login}</a>}{kind === 'error' && <button onClick={onRetry}>{copy.retry}</button>}</main>;
}

function Overview({ copy, language, data }) {
  const a = data.accountOverview || {}, b = data.billingOverview || {}, t = data.tokenOverview || {};
  const alerts = [[copy.overview.disabledUsers,a.disabled_users],[copy.overview.invitations,a.pending_invitations],[copy.overview.paymentReview,b.review_payment_orders],[copy.overview.pendingAdjustments,b.pending_adjustments],[copy.overview.usageReview,t.review_usage_requests]];
  return <section><div className="adm-hero"><div><h2>{copy.overview.title}</h2><p>{copy.overview.lead}</p></div><Status value="ACTIVE" /></div><div className="adm-metrics"><Metric label={copy.overview.users} value={a.users} /><Metric label={copy.overview.organizations} value={a.organizations} /><Metric label={copy.overview.available} value={subtractMicrocredits(b.posted_microcredits, b.reserved_microcredits)} tone="accent" formatter={value => formatPoints(value, language)} /><Metric label={copy.overview.reserved} value={b.reserved_microcredits} formatter={value => formatPoints(value, language)} /><Metric label={copy.overview.tokenAccounts} value={t.token_accounts} /><Metric label={copy.overview.usageRequests} value={t.usage_requests} /></div><div className="adm-overview-grid"><article className="adm-panel"><SectionHeader title={copy.overview.attention} copy={copy} /><div className="adm-attention">{alerts.map(([label,value]) => <div key={label}><span>{label}</span><strong className={Number(value) ? 'has-value' : ''}>{formatNumber(value)}</strong></div>)}</div></article><article className="adm-panel adm-health"><span>{copy.overview.healthyRoutes}</span><strong>{formatNumber(t.healthy_routes)}</strong><small>/ {formatNumber(t.token_accounts)} {copy.overview.tokenAccounts}</small><div><i style={{ width:`${Math.min(100, Number(t.token_accounts) ? Number(t.healthy_routes) / Number(t.token_accounts) * 100 : 0)}%` }} /></div></article></div></section>;
}

function AccountDirectory({ section, copy, data, client, setData, onAuthorizationError }) {
  const mutationOwner = useAdminMutationOwner('account');
  const [search, setSearch] = useState('');
  const [actionKey, setActionKey] = useState('');
  const [notice, setNotice] = useState('');
  const [actionError, setActionError] = useState('');
  const [selectedOrganization, setSelectedOrganization] = useState(null);
  const [selectedUser, setSelectedUser] = useState(null);
  const [organizationName, setOrganizationName] = useState('');
  const [ownerUserID, setOwnerUserID] = useState('');
  const searchGuardRef = useRef(createContextRequestGuard());
  const lifecycleRef = useRef(0);
  useEffect(() => {
    setActionKey(''); setNotice(''); setActionError('');
    return () => { lifecycleRef.current += 1; searchGuardRef.current.cancel(); };
  }, [section, selectedOrganization?.organization_id, selectedUser?.user_id]);
  const captureContext = () => {
    const generation = lifecycleRef.current;
    return () => generation === lifecycleRef.current;
  };
  const isUserSection = section === 'users';
  const items = isUserSection ? data.users || [] : data.organizations || [];
  const activeUsers = useMemo(() => (data.users || []).filter(user => user.status === 'ACTIVE'), [data.users]);
  useEffect(() => {
    if (!activeUsers.some(user => user.user_id === ownerUserID)) setOwnerUserID(activeUsers[0]?.user_id || '');
  }, [activeUsers, ownerUserID]);
  const reload = async () => {
    const request = searchGuardRef.current.begin();
    const isCurrent = () => searchGuardRef.current.isCurrent(request.generation) && !request.signal.aborted;
    try {
      const result = isUserSection
        ? (/^USR/i.test(search.trim().replaceAll('-', '')) ? { items:[await client.userByCode(search.trim(), request.signal)] } : await client.users(search, request.signal))
        : await client.organizations(search, request.signal);
      if (!isCurrent()) return;
      setData(current => ({ ...current, [section]:result.items || [], accountOverview:undefined }));
    } catch (reason) {
      if (isCurrent() && reason?.name !== 'AbortError') throw reason;
    }
  };
  const perform = async (item, action) => {
    const isUser = isUserSection;
    const id = isUser ? item.user_id : item.organization_id;
    const label = isUser ? userLabel(item) : item.name;
    if (action === 'delete' && !window.confirm(isUser ? copy.accounts.confirmUser(label) : copy.accounts.confirmOrganization(label))) return;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    setActionKey(`${section}:${id}:${action}`); setNotice(''); setActionError('');
    try {
      if (isUser) {
        if (action === 'delete') await client.deleteUser(id);
        else await client.setUserStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
      } else if (action === 'delete') await client.deleteOrganization(id);
      else await client.setOrganizationStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
      if (!isCurrent()) return;
      await reload(); if (isCurrent()) setNotice(copy.accounts.updated);
    } catch (reason) {
      if (!isCurrent()) return;
      onAuthorizationError(reason);
      setActionError(reason?.code === 'LAST_OWNER_REQUIRED' ? copy.accounts.ownerRequired : reason?.code === 'ACCOUNT_CONFLICT' ? copy.accounts.conflict : copy.accounts.actionFailed);
    } finally { mutationOwner.finish(ownership); if (isCurrent()) setActionKey(''); }
  };
  const actionButtons = (item, isUser) => {
    const id = isUser ? item.user_id : item.organization_id;
    const prefix = `${isUser ? 'users' : 'organizations'}:${id}:`;
    const buttons = [];
    if (item.status === 'DISABLED') buttons.push(<button key="enable" disabled={(Boolean(actionKey) || mutationOwner.busy)} onClick={() => perform(item, 'enable')}>{copy.accounts.enable}</button>);
    if (item.status === 'ACTIVE' || (isUser && item.status === 'PROVISIONING')) buttons.push(<button key="disable" disabled={(Boolean(actionKey) || mutationOwner.busy)} onClick={() => perform(item, 'disable')}>{copy.accounts.disable}</button>);
    buttons.push(<button key="delete" className="danger" disabled={(Boolean(actionKey) || mutationOwner.busy)} onClick={() => perform(item, 'delete')}>{actionKey === `${prefix}delete` ? '…' : copy.accounts.delete}</button>);
    return <div className="adm-row-actions">{buttons}</div>;
  };
  const columns = isUserSection ? [
    { key:'username', label:copy.username, render:item => <button className="adm-name-link" onClick={() => setSelectedUser(item)}>{userLabel(item)}</button> }, { key:'user_code', label:copy.accounts.userCode, render:item => <code>{item.user_code || '—'}</code> }, { key:'display_name', label:copy.accounts.displayName }, { key:'email', label:copy.accounts.email }, { key:'status', label:copy.accounts.status, render:item => <Status value={item.status} /> }, { key:'organization_count', label:copy.accounts.memberships }, { key:'last_seen_at', label:copy.accounts.lastSeen, render:item => formatDate(item.last_seen_at) }, { key:'actions', label:copy.accounts.actions, render:item => actionButtons(item, true) },
  ] : [
    { key:'name', label:copy.accounts.organization, render:item => <button className="adm-name-link" onClick={() => setSelectedOrganization(item)}>{item.name}</button> }, { key:'organization_id', label:'ID', render:item => <code title={item.organization_id}>{shortID(item.organization_id)}</code> }, { key:'status', label:copy.accounts.status, render:item => <Status value={item.status} /> }, { key:'member_count', label:copy.accounts.members }, { key:'created_at', label:copy.accounts.created, render:item => formatDate(item.created_at) }, { key:'actions', label:copy.accounts.actions, render:item => actionButtons(item, false) },
  ];
  const submit = async event => {
    event.preventDefault(); setActionError('');
    try { await reload(); } catch (reason) {
      onAuthorizationError(reason); setActionError(copy.accounts.actionFailed);
    }
  };
  const createOrganization = async event => {
    event.preventDefault();
    if (!organizationName.trim() || !ownerUserID || actionKey || mutationOwner.busy) return;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    setActionKey('organization:create'); setNotice(''); setActionError('');
    try {
      await client.createOrganization(organizationName.trim(), ownerUserID);
      if (!isCurrent()) return;
      setOrganizationName('');
      await reload();
      if (isCurrent()) setNotice(copy.accounts.organizationCreated);
    } catch (reason) {
      if (isCurrent()) { onAuthorizationError(reason); setActionError(copy.accounts.actionFailed); }
    } finally { mutationOwner.finish(ownership); if (isCurrent()) setActionKey(''); }
  };
  if (selectedUser) return <UserMembershipDetail key={selectedUser.user_id} copy={copy} user={selectedUser} roles={data.roles || []} client={client} onAuthorizationError={onAuthorizationError} onBack={() => setSelectedUser(null)} />;
  if (selectedOrganization) return <OrganizationDetail key={selectedOrganization.organization_id} copy={copy} organization={selectedOrganization} users={data.users || []} roles={data.roles || []} client={client} onBack={() => setSelectedOrganization(null)} onAuthorizationError={onAuthorizationError} onRenamed={name => { setSelectedOrganization(current => current?.organization_id === selectedOrganization.organization_id ? { ...current, name } : current); setData(current => ({ ...current, organizations:(current.organizations || []).map(item => item.organization_id === selectedOrganization.organization_id ? { ...item, name } : item) })); }} />;
  return <section className="adm-panel"><SectionHeader title={copy.accounts[section]} count={items.length} copy={copy}><form className="adm-search" onSubmit={submit}><input value={search} onChange={event => setSearch(event.target.value)} placeholder={isUserSection ? copy.search : copy.accounts.organizationSearch} /><button>{copy.searchAction}</button></form></SectionHeader>{!isUserSection && <form className="adm-create-row" onSubmit={createOrganization}><input value={organizationName} onChange={event => setOrganizationName(event.target.value)} placeholder={copy.accounts.organizationNamePlaceholder} maxLength={120} /><select value={ownerUserID} onChange={event => setOwnerUserID(event.target.value)} aria-label={copy.accounts.initialOwner}><option value="">{copy.accounts.chooseOwner}</option>{activeUsers.map(user => <option key={user.user_id} value={user.user_id}>{userLabel(user)}</option>)}</select><button disabled={!organizationName.trim() || !ownerUserID || (Boolean(actionKey) || mutationOwner.busy)}>{copy.accounts.createOrganization}</button>{activeUsers.length === 0 && <small>{copy.accounts.noActiveOwner}</small>}</form>}{notice && <div className="adm-inline-message ok" role="status">{notice}</div>}{actionError && <div className="adm-inline-message error" role="alert">{actionError}</div>}<Table columns={columns} items={items} copy={copy} rowKey={isUserSection ? 'user_id' : 'organization_id'} /><RoleDefinitions copy={copy} roles={data.roles || []} /></section>;
}

function RoleDefinitions({ copy, roles }) {
  if (!roles.length) return null;
  return <details className="adm-role-definitions"><summary>{copy.roles.definitions}</summary><div className="adm-role-grid">{roles.map(role => <article key={role.code}><strong>{role.code}</strong><span>{role.scope === 'UNIT' ? copy.roles.unitScope : copy.roles.organizationScope}</span><p>{copy.roles.descriptions[role.code] || copy.roles.unknownDescription}</p></article>)}</div><small>{copy.roles.ownerRoleHint} {copy.roles.baseRoleHint}</small></details>;
}

function UserMembershipDetail({ copy, user, roles, client, onBack, onAuthorizationError }) {
  const [items, setItems] = useState([]);
  const { busy, completed, failed, run, invalidate } = useAdminOperation(onAuthorizationError, 'account');
  useEffect(() => {
    void run(async ({ signal, isCurrent }) => {
      const result = await client.userMemberships(user.user_id, signal);
      if (isCurrent()) setItems(result.items || []);
    });
    return invalidate;
  }, [client, user.user_id, completed, run, invalidate]);
  const error = failed ? copy.accounts.actionFailed : '';
  return <section className="adm-panel adm-org-detail"><button className="adm-back" onClick={onBack}>← {copy.roles.backToUsers}</button><div className="adm-org-title"><div><span className="adm-eyebrow">{copy.roles.userMemberships}</span><h2>{userLabel(user)}</h2></div><Status value={user.status} /></div>{error && <div className="adm-inline-message error">{error}</div>}{busy ? <div className="adm-empty">{copy.loading}</div> : items.length ? <div className="adm-membership-list">{items.map(item => <article key={item.organization_id}><div><strong>{item.organization_name}</strong><Status value={item.membership_status} /></div><div className="adm-role-badges">{item.roles.map(role => <span key={role}>{role}</span>)}</div>{item.unit_admin_unit_ids?.length > 0 && <small>{copy.roles.unitAdminNodes}: {item.unit_admin_unit_ids.map(shortID).join(' · ')}</small>}</article>)}</div> : <Empty copy={{ noData:copy.roles.noMemberships }} />}<RoleDefinitions copy={copy} roles={roles} /></section>;
}

function OrganizationDetail({ copy, organization, users, roles, client, onBack, onRenamed, onAuthorizationError }) {
  const mutationOwner = useAdminMutationOwner('account');
  const [units, setUnits] = useState([]);
  const [members, setMembers] = useState([]);
  const [name, setName] = useState('');
  const [organizationName, setOrganizationName] = useState(organization.name);
  const [editingOrganizationName, setEditingOrganizationName] = useState(false);
  const [parentID, setParentID] = useState('');
  const [newOwnerUserID, setNewOwnerUserID] = useState('');
  const [loading, setLoading] = useState(true);
  const [mutating, setMutating] = useState(false);
  const [newMemberUserID, setNewMemberUserID] = useState('');
  const busy = loading || mutating || mutationOwner.busy;
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const loadGuardRef = useRef(createContextRequestGuard());
  const lifecycleRef = useRef(0);
  const mutationLockRef = useRef(false);
  useEffect(() => {
    setMutating(false);
    return () => { lifecycleRef.current += 1; };
  }, [organization.organization_id]);
  const captureContext = () => {
    const generation = lifecycleRef.current;
    return () => generation === lifecycleRef.current;
  };
  const load = useCallback(async signal => {
    const request = loadGuardRef.current.begin();
    const isCurrent = () => loadGuardRef.current.isCurrent(request.generation) && !request.signal.aborted && !signal?.aborted;
    setLoading(true); setError('');
    try {
      const [unitResult, memberResult] = await Promise.all([
        client.organizationUnits(organization.organization_id, request.signal),
        client.organizationMembers(organization.organization_id, request.signal),
      ]);
      if (!isCurrent()) return;
      setUnits(unitResult.items || []);
      setMembers(memberResult.items || []);
    } catch (reason) {
      if (reason?.name !== 'AbortError' && isCurrent()) { onAuthorizationError(reason); setError(copy.accounts.actionFailed); }
    } finally { if (isCurrent()) setLoading(false); }
  }, [client, copy.accounts.actionFailed, organization.organization_id, onAuthorizationError]);
  useEffect(() => {
    const controller = new globalThis.AbortController();
    void load(controller.signal);
    return () => { controller.abort(); loadGuardRef.current.cancel(); };
  }, [load, mutationOwner.completed]);
  useEffect(() => setOrganizationName(organization.name), [organization.name]);
  const roots = useMemo(() => buildUnitTree(units), [units]);
  const owners = useMemo(() => members.filter(member => (member.roles || []).includes('OWNER')), [members]);
  const transferCandidates = useMemo(() => members.filter(member => member.status === 'ACTIVE' && !(member.roles || []).includes('OWNER')), [members]);
  const addCandidates = useMemo(() => users.filter(user => user.status === 'ACTIVE' && !members.some(member => member.user_id === user.user_id)), [members, users]);
  const memberLabel = member => member.display_name || member.email || shortID(member.user_id);
  const refreshUnits = async isCurrent => {
    const result = await client.organizationUnits(organization.organization_id);
    if (isCurrent()) setUnits(result.items || []);
  };
  const refreshMembers = async isCurrent => {
    const result = await client.organizationMembers(organization.organization_id);
    if (isCurrent()) setMembers(result.items || []);
  };
  const renameOrganization = async event => {
    event.preventDefault();
    const nextName = organizationName.trim();
    if (!nextName || nextName === organization.name || busy || mutationLockRef.current) return;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    mutationLockRef.current = true;
    setMutating(true); setNotice(''); setError('');
    try {
      await client.renameOrganization(organization.organization_id, nextName);
      if (!isCurrent()) return;
      onRenamed(nextName); setEditingOrganizationName(false); setNotice(copy.accounts.organizationRenamed);
    } catch (reason) { if (!isCurrent()) return; onAuthorizationError(reason); setError(copy.accounts.actionFailed); }
    finally { mutationOwner.finish(ownership); mutationLockRef.current = false; if (isCurrent()) setMutating(false); }
  };
  const createUnit = async event => {
    event.preventDefault();
    if (!name.trim() || busy || mutationLockRef.current || organization.status !== 'ACTIVE') return;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    mutationLockRef.current = true;
    setMutating(true); setNotice(''); setError('');
    try {
      await client.createOrganizationUnit(organization.organization_id, name.trim(), parentID || null);
      if (!isCurrent()) return;
      setName(''); setParentID('');
      await refreshUnits(isCurrent); if (!isCurrent()) return; setNotice(copy.accounts.unitCreated);
    } catch (reason) { if (!isCurrent()) return; onAuthorizationError(reason); setError(copy.accounts.actionFailed); }
    finally { mutationOwner.finish(ownership); mutationLockRef.current = false; if (isCurrent()) setMutating(false); }
  };
  const transferOwnership = async event => {
    event.preventDefault();
    const target = transferCandidates.find(member => member.user_id === newOwnerUserID);
    if (!target || busy || mutationLockRef.current || organization.status !== 'ACTIVE' || !window.confirm(copy.accounts.confirmOwnership(memberLabel(target)))) return;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    mutationLockRef.current = true;
    setMutating(true); setNotice(''); setError('');
    try {
      await client.transferOrganizationOwnership(organization.organization_id, target.user_id);
      if (!isCurrent()) return;
      await refreshMembers(isCurrent); if (!isCurrent()) return; setNewOwnerUserID(''); setNotice(copy.accounts.ownershipTransferred);
    } catch (reason) { if (!isCurrent()) return; onAuthorizationError(reason); setError(copy.accounts.actionFailed); }
    finally { mutationOwner.finish(ownership); mutationLockRef.current = false; if (isCurrent()) setMutating(false); }
  };
  const changeMember = async (action, noticeKey, afterSuccess) => {
    if (busy || mutationLockRef.current || organization.status !== 'ACTIVE') return;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    mutationLockRef.current = true; setMutating(true); setNotice(''); setError('');
    try {
      await action();
      if (!isCurrent()) return;
      await refreshMembers(isCurrent);
      if (!isCurrent()) return;
      afterSuccess?.(); setNotice(copy.roles[noticeKey]);
    } catch (reason) {
      if (!isCurrent()) return;
      onAuthorizationError(reason);
      setError(reason?.code === 'LAST_OWNER_REQUIRED' ? copy.accounts.ownerRequired : copy.accounts.actionFailed);
    } finally { mutationOwner.finish(ownership); mutationLockRef.current = false; if (isCurrent()) setMutating(false); }
  };
  const addMember = event => {
    event.preventDefault();
    if (!addCandidates.some(user => user.user_id === newMemberUserID)) return;
    void changeMember(() => client.addOrganizationMember(organization.organization_id, newMemberUserID, ['MEMBER'], []), 'memberAdded', () => setNewMemberUserID(''));
  };
  const saveMemberRoles = (member, selectedRoles, unitIDs) => changeMember(() => client.replaceOrganizationMemberRoles(organization.organization_id, member.user_id, selectedRoles, unitIDs), 'rolesUpdated');
  const removeMember = member => {
    if (busy || mutationLockRef.current || (member.roles || []).includes('OWNER') || !window.confirm(copy.roles.confirmRemove(memberLabel(member)))) return;
    void changeMember(() => client.removeOrganizationMember(organization.organization_id, member.user_id), 'memberRemoved');
  };
  const renameUnit = async (unit, nextName) => {
    const trimmed = nextName.trim();
    if (!trimmed || trimmed === unit.name || busy || mutationLockRef.current) return trimmed === unit.name;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    mutationLockRef.current = true;
    setMutating(true); setNotice(''); setError('');
    try {
      await client.renameOrganizationUnit(organization.organization_id, unit.unit_id, trimmed);
      if (!isCurrent()) return;
      await refreshUnits(isCurrent); if (!isCurrent()) return; setNotice(copy.accounts.unitRenamed); return true;
    } catch (reason) { if (!isCurrent()) return; onAuthorizationError(reason); setError(copy.accounts.actionFailed); return false; }
    finally { mutationOwner.finish(ownership); mutationLockRef.current = false; if (isCurrent()) setMutating(false); }
  };
  const deleteUnit = async unit => {
    if (busy || mutationLockRef.current || !window.confirm(copy.accounts.confirmUnit(unit.name))) return;
    const isCurrent = captureContext();
    const ownership = mutationOwner.begin(); if (!ownership) return;
    mutationLockRef.current = true;
    setMutating(true); setNotice(''); setError('');
    try {
      await client.deleteOrganizationUnit(organization.organization_id, unit.unit_id);
      if (!isCurrent()) return;
      await refreshUnits(isCurrent); if (!isCurrent()) return; setNotice(copy.accounts.unitDeleted);
    } catch (reason) {
      if (!isCurrent()) return;
      onAuthorizationError(reason);
      setError(reason?.code === 'ORG_UNIT_HIERARCHY_INVALID' ? copy.accounts.deleteUnitBlocked : copy.accounts.actionFailed);
    } finally { mutationOwner.finish(ownership); mutationLockRef.current = false; if (isCurrent()) setMutating(false); }
  };
  return <section className="adm-panel adm-org-detail"><button className="adm-back" onClick={onBack}>← {copy.accounts.backToOrganizations}</button><div className="adm-org-title"><div><span className="adm-eyebrow">{copy.accounts.organizationManagement}</span>{editingOrganizationName ? <form className="adm-org-rename" onSubmit={renameOrganization}><input value={organizationName} onChange={event => setOrganizationName(event.target.value)} maxLength={120} aria-label={copy.accounts.organizationName} autoFocus /><button disabled={busy || !organizationName.trim() || organizationName.trim() === organization.name}>{copy.accounts.save}</button><button type="button" className="secondary" disabled={busy} onClick={() => { setOrganizationName(organization.name); setEditingOrganizationName(false); }}>{copy.accounts.cancel}</button></form> : <div className="adm-org-name-display"><h2>{organization.name}</h2><button type="button" disabled={busy} onClick={() => setEditingOrganizationName(true)}>{copy.accounts.rename}</button></div>}</div><Status value={organization.status} /></div>{organization.status !== 'ACTIVE' && <div className="adm-inline-message error">{copy.accounts.organizationDisabled}</div>}<div className="adm-owner-card"><div><span>{copy.accounts.organizationOwner}</span><strong>{owners.length ? owners.map(memberLabel).join(' · ') : '—'}</strong></div><form onSubmit={transferOwnership}><select value={newOwnerUserID} onChange={event => setNewOwnerUserID(event.target.value)} aria-label={copy.accounts.chooseNewOwner}><option value="">{copy.accounts.chooseNewOwner}</option>{transferCandidates.map(member => <option key={member.user_id} value={member.user_id}>{memberLabel(member)}</option>)}</select><button disabled={busy || organization.status !== 'ACTIVE' || !newOwnerUserID}>{copy.accounts.transferOwner}</button></form>{!busy && transferCandidates.length === 0 && <small>{copy.accounts.noTransferCandidate}</small>}</div><div className="adm-unit-head"><h3>{copy.roles.organizationMembers}</h3><span>{copy.items(members.length)}</span></div><form className="adm-create-row" onSubmit={addMember}><select value={newMemberUserID} onChange={event => setNewMemberUserID(event.target.value)} aria-label={copy.roles.chooseMember}><option value="">{copy.roles.chooseMember}</option>{addCandidates.map(user => <option key={user.user_id} value={user.user_id}>{userLabel(user)}</option>)}</select><button disabled={busy || !newMemberUserID}>{copy.roles.addMember}</button></form><div className="adm-member-access-list">{members.map(member => <MemberAccessEditor key={member.user_id} copy={copy} member={member} units={units} roles={roles} disabled={busy || organization.status !== 'ACTIVE'} onSave={saveMemberRoles} onRemove={removeMember} />)}</div><RoleDefinitions copy={copy} roles={roles} /><form className="adm-unit-form" onSubmit={createUnit}><input value={name} onChange={event => setName(event.target.value)} placeholder={copy.accounts.unitName} maxLength={120} /><select value={parentID} onChange={event => setParentID(event.target.value)} aria-label={copy.accounts.parentUnit}><option value="">{copy.accounts.organizationRoot}</option>{units.filter(unit => unit.status === 'ACTIVE').map(unit => <option key={unit.unit_id} value={unit.unit_id}>{unit.name}</option>)}</select><button disabled={busy || organization.status !== 'ACTIVE' || !name.trim()}>{copy.accounts.createUnit}</button></form>{notice && <div className="adm-inline-message ok" role="status">{notice}</div>}{error && <div className="adm-inline-message error" role="alert">{error}</div>}<div className="adm-unit-head"><h3>{copy.accounts.unitHierarchy}</h3><span>{copy.items(units.length)}</span></div>{busy && units.length === 0 ? <div className="adm-empty">{copy.accounts.loadingUnits}</div> : roots.length === 0 ? <Empty copy={copy} /> : <div className="adm-unit-tree">{roots.map(unit => <UnitBranch key={unit.unit_id} unit={unit} copy={copy} disabled={busy} addDisabled={organization.status !== 'ACTIVE'} onAddChild={id => { setParentID(id); setName(''); }} onRename={renameUnit} onDelete={deleteUnit} />)}</div>}</section>;
}

function MemberAccessEditor({ copy, member, units, roles, disabled, onSave, onRemove }) {
  const editableRoles = roles.filter(role => role.assignable && role.scope === 'ORGANIZATION').map(role => role.code);
  const [selected, setSelected] = useState(() => (member.roles || []).filter(role => editableRoles.includes(role)));
  const [unitIDs, setUnitIDs] = useState(() => member.unit_admin_unit_ids || []);
  useEffect(() => { setSelected((member.roles || []).filter(role => editableRoles.includes(role))); setUnitIDs(member.unit_admin_unit_ids || []); }, [member, editableRoles.join('|')]);
  const toggle = (value, values, setter) => setter(values.includes(value) ? values.filter(item => item !== value) : [...values, value]);
  return <article className="adm-member-access"><header><div><strong>{member.display_name || member.email || shortID(member.user_id)}</strong><small>{member.email}</small></div><Status value={member.status} /></header><div className="adm-role-options">{editableRoles.map(role => <label key={role}><input type="checkbox" checked={selected.includes(role)} disabled={disabled} onChange={() => toggle(role, selected, setSelected)} />{role}</label>)}<label><input type="checkbox" checked disabled readOnly />MEMBER</label>{(member.roles || []).includes('OWNER') && <label><input type="checkbox" checked disabled readOnly />OWNER</label>}</div><div className="adm-unit-role-options"><span>{copy.roles.unitAdminNodes}</span>{units.filter(unit => unit.status === 'ACTIVE').map(unit => <label key={unit.unit_id}><input type="checkbox" checked={unitIDs.includes(unit.unit_id)} disabled={disabled} onChange={() => toggle(unit.unit_id, unitIDs, setUnitIDs)} />{unit.name}</label>)}</div><footer><button disabled={disabled} onClick={() => onSave(member, selected, unitIDs)}>{copy.roles.saveRoles}</button><button className="danger" disabled={disabled || (member.roles || []).includes('OWNER')} onClick={() => onRemove(member)}>{copy.roles.removeMember}</button></footer></article>;
}

function UnitBranch({ unit, copy, disabled, addDisabled, onAddChild, onRename, onDelete }) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(unit.name);
  useEffect(() => setName(unit.name), [unit.name]);
  const submit = async event => {
    event.preventDefault();
    if (await onRename(unit, name)) setEditing(false);
  };
  return <div className="adm-unit-branch"><div className="adm-unit-node">{editing ? <form className="adm-unit-rename" onSubmit={submit}><input value={name} onChange={event => setName(event.target.value)} maxLength={120} aria-label={copy.accounts.unitName} autoFocus /><button disabled={disabled || !name.trim()}>{copy.accounts.save}</button><button type="button" onClick={() => { setName(unit.name); setEditing(false); }}>{copy.accounts.cancel}</button></form> : <div><strong>{unit.name}</strong><Status value={unit.status} /></div>}<div className="adm-unit-actions"><button type="button" disabled={disabled || addDisabled || unit.status !== 'ACTIVE'} onClick={() => onAddChild(unit.unit_id)}>{copy.accounts.addChild}</button><button type="button" disabled={disabled || unit.status !== 'ACTIVE'} onClick={() => setEditing(true)}>{copy.accounts.rename}</button><button type="button" className="danger" title={unit.children.length ? copy.accounts.deleteUnitBlocked : undefined} disabled={disabled || unit.status !== 'ACTIVE' || unit.children.length > 0} onClick={() => onDelete(unit)}>{copy.accounts.delete}</button></div></div>{unit.children.length > 0 && <div className="adm-unit-children">{unit.children.map(child => <UnitBranch key={child.unit_id} unit={child} copy={copy} disabled={disabled} addDisabled={addDisabled} onAddChild={onAddChild} onRename={onRename} onDelete={onDelete} />)}</div>}</div>;
}

function Billing({ copy, language, data, client, filters, setData, onAuthorizationError, onApproval }) {
  const [tab, setTab] = useState('wallets'); const items = data[tab] || [];
  const ops = useAdminOperation(onAuthorizationError, 'billing');
  const { busy } = ops;
  const [invalidAmount, setInvalidAmount] = useState(false);
  const notice = ops.noticeKey ? copy.billing[ops.noticeKey] : '';
  const error = invalidAmount ? copy.billing.invalidAmount : ops.failed ? copy.billing.actionFailed : '';
  const [ledger, setLedger] = useState(null); const [cases, setCases] = useState(null);
  const [orderDetail, setOrderDetail] = useState(null); const [holdDetail, setHoldDetail] = useState(null);
  const [price, setPrice] = useState({ model:'', input:'', output:'', cached:'' });
  const [reconciliation, setReconciliation] = useState({ channel:'ALIPAY', businessDate:new Date().toISOString().slice(0,10) });
  const [importBillJSON, setImportBillJSON] = useState('[]');
  const [orderFilter, setOrderFilter] = useState(() => ({ ...filters.current }));
  useEffect(() => { setOrderFilter({ ...filters.current }); }, [filters]);
  const paymentCapabilities = data.billingOverview?.payment_capabilities || {};
  const refundSupported = paymentCapabilities.refund_supported === true;
  const reconciliationSupported = paymentCapabilities.bill_supported === true;
  const reconciliationImportSupported = paymentCapabilities.bill_file_import_supported === true;
  const repairSupported = paymentCapabilities.manual_repair_supported === true;
  const hasLimitedPaymentCapabilities = paymentCapabilities.provider_id && (!paymentCapabilities.refund_supported || !paymentCapabilities.close_supported || !paymentCapabilities.notification_supported || !paymentCapabilities.bill_supported);
  const changeTab = key => { ops.invalidate(); setInvalidAmount(false); setLedger(null); setCases(null); setOrderDetail(null); setHoldDetail(null); setTab(key); };
  const reload = async ({ signal, isCurrent }) => {
    const next = await readAdminResources(billingResources(client, signal, filters.current));
    if (isCurrent()) setData(current => mergeAdminResources(current, next));
  };
  const perform = action => {
    setInvalidAmount(false);
    return ops.run(async context => {
      await action(context);
      if (!context.isCurrent()) return;
      await reload(context);
      return 'completed';
    }, true);
  };
  const searchOrders = event => {
    event.preventDefault();
    if (busy) return;
    void ops.run(async context => { filters.current = { ...orderFilter }; await reload(context); });
  };
  const promptReason = label => String(window.prompt(label) || '').trim();
  const createPrice = event => {
    event.preventDefault();
    const input = { model:price.model.trim(), input_per_million:parseScaledAmount(price.input || '0', 4), output_per_million:parseScaledAmount(price.output || '0', 4), cached_input_per_million:parseScaledAmount(price.cached || '0', 4) };
    if (!input.model) return;
    if (Object.values(input).slice(1).some(value => value === null)) { setInvalidAmount(true); return; }
    void perform(async ({ isCurrent }) => { await client.createPriceVersion(input); if (isCurrent()) setPrice({ model:'', input:'', output:'', cached:'' }); });
  };
  const showLedger = wallet => ops.run(async ({ signal, isCurrent }) => { const result = await client.walletLedger(wallet.wallet_id, signal); if (isCurrent()) setLedger({ wallet, items:result.items || [] }); });
  const reject = item => { if (busy) return; const reason = promptReason(copy.billing.decisionReason); if (reason) void perform(() => client.decideAdjustment(item.adjustment_id, 'reject', reason)); };
  const showOrder = item => ops.run(async ({ signal, isCurrent }) => { const value = requireAdminRecord(await client.paymentOrder(item.order_id, signal), 'order_id', item.order_id); if (isCurrent()) setOrderDetail(value); });
  const showHold = item => ops.run(async ({ signal, isCurrent }) => { const value = await client.hold(item.hold_id, signal); requireAdminRecord(value.hold, 'hold_id', item.hold_id); if (isCurrent()) setHoldDetail(value); });
  const refund = (transactionID, packageID, reason) => {
    if (busy || !refundSupported || !orderDetail) return;
    const key = client.refundRequestKey(orderDetail.order_id, transactionID, packageID, reason);
    return perform(async context => {
      await client.refundPaymentOrder(orderDetail.order_id, transactionID, packageID, reason, key);
      if (!context.isCurrent()) return;
      const value = requireAdminRecord(await client.paymentOrder(orderDetail.order_id, context.signal), 'order_id', orderDetail.order_id);
      if (context.isCurrent()) setOrderDetail(value);
    });
  };
  const repair = item => { if (busy || !repairSupported || item.fulfillment_status !== 'MANUAL_REVIEW') return; const reason = promptReason(copy.billing.repairReason); if (reason) void perform(() => client.repairPaymentOrder(item.order_id, reason)); };
  const release = item => { if (busy) return; const reason = promptReason(copy.billing.releaseReason); if (reason && window.confirm(copy.billing.confirmRelease)) void perform(() => client.releaseHold(item.hold_id, reason)); };
  const showCases = run => ops.run(async ({ signal, isCurrent }) => { const result = await client.reconciliationCases(run.reconciliation_run_id, signal); if (isCurrent()) setCases({ run, items:result.items || [] }); });
  const columns = {
    wallets:[{key:'owner_id',label:copy.billing.owner,render:item => <code title={item.owner_id}>{item.owner_type} · {shortID(item.owner_id)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'available_microcredits',label:copy.billing.available,render:item => formatPoints(item.available_microcredits, language)},{key:'posted_microcredits',label:copy.billing.posted,render:item => formatPoints(item.posted_microcredits, language)},{key:'reserved_microcredits',label:copy.billing.reserved,render:item => formatPoints(item.reserved_microcredits, language)},{key:'actions',label:copy.accounts.actions,render:item => <button disabled={busy} onClick={() => showLedger(item)}>{copy.billing.viewLedger}</button>}],
    prices:[{key:'model',label:copy.tokens.model,render:item => <strong>{item.model}</strong>},{key:'input_per_million',label:copy.billing.inputPrice,render:item => formatPoints(item.input_per_million, language)},{key:'output_per_million',label:copy.billing.outputPrice,render:item => formatPoints(item.output_per_million, language)},{key:'cached_input_per_million',label:copy.billing.cachedPrice,render:item => formatPoints(item.cached_input_per_million, language)},{key:'effective_from',label:copy.billing.effectiveFrom,render:item => formatDate(item.effective_from)},{key:'effective_to',label:copy.billing.effectiveTo,render:item => item.effective_to ? formatDate(item.effective_to) : <Status value="ACTIVE" />}],
    orders:[{key:'merchant_order_no',label:copy.billing.order,render:item => <span><code>{item.merchant_order_no}</code><br/><small>{copy.billing.pinvouOrder}: {shortID(item.order_id)}</small></span>},{key:'provider_sku',label:copy.billing.product,render:item => <span>{item.product_id}<br/><code>{item.provider_sku || '—'}</code></span>},{key:'primary_provider_transaction_id',label:copy.billing.paymentReference,render:item => <span><code>{item.primary_provider_transaction_id || '—'}</code><br/><small>{item.mobile_masked || '—'}</small></span>},{key:'channel',label:copy.billing.channel},{key:'amount_fen',label:copy.billing.paymentAmount,render:item => formatMoney(item.amount_fen, language)},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'fulfillment_status',label:copy.billing.fulfillment,render:item => <Status value={item.fulfillment_status} />},{key:'updated_at',label:copy.billing.updated,render:item => formatDate(item.updated_at)},{key:'actions',label:copy.accounts.actions,render:item => <div className="adm-row-actions"><button disabled={busy} onClick={() => perform(() => client.syncPaymentOrder(item.order_id))}>{copy.billing.sync}</button><button title={!repairSupported ? copy.billing.providerUnsupported : undefined} disabled={busy || !repairSupported || item.status !== 'MANUAL_REVIEW' || item.fulfillment_status !== 'MANUAL_REVIEW'} onClick={() => repair(item)}>{copy.billing.repair}</button><button title={!refundSupported ? copy.billing.providerUnsupported : undefined} disabled={busy || !refundSupported || !['PAID','PAID_LATE','PARTIALLY_REFUNDED'].includes(item.status)} onClick={() => showOrder(item)}>{copy.billing.refund}</button><button disabled={busy} onClick={() => showOrder(item)}>{copy.billing.orderDetail}</button></div>}],
    adjustments:[{key:'wallet_id',label:copy.tokens.wallet,render:item => <code title={item.wallet_id}>{shortID(item.wallet_id)}</code>},{key:'amount_microcredits',label:copy.billing.amount,render:item => formatPoints(item.amount_microcredits, language)},{key:'reason',label:copy.billing.reason},{key:'requested_by',label:copy.billing.requestedBy,render:item => <code title={item.requested_by}>{shortID(item.requested_by)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'actions',label:copy.accounts.actions,render:item => item.status === 'PENDING_APPROVAL' ? <div className="adm-row-actions"><button disabled={busy} onClick={() => onApproval(item)}>{copy.billing.reviewApproval}</button><button className="danger" disabled={busy} onClick={() => reject(item)}>{copy.billing.reject}</button></div> : '—'}],
    holds:[{key:'usage_request_id',label:copy.tokens.usageID,render:item => <code title={item.usage_request_id}>{shortID(item.usage_request_id)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'max_microcredits',label:copy.billing.maximum,render:item => formatPoints(item.max_microcredits, language)},{key:'actual_microcredits',label:copy.billing.actual,render:item => formatPoints(item.actual_microcredits, language)},{key:'expires_at',label:copy.billing.expires,render:item => formatDate(item.expires_at)},{key:'actions',label:copy.accounts.actions,render:item => <div className="adm-row-actions"><button disabled={busy} onClick={() => showHold(item)}>{copy.billing.viewEvidence}</button>{['ACTIVE','REVIEW'].includes(item.status) && <button className="danger" disabled={busy} onClick={() => release(item)}>{copy.billing.release}</button>}</div>}],
    reconciliations:[{key:'business_date',label:copy.billing.businessDate},{key:'channel',label:copy.billing.channel},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'provider_row_count',label:copy.billing.providerRows},{key:'case_count',label:copy.billing.cases},{key:'started_at',label:copy.billing.updated,render:item => formatDate(item.started_at)},{key:'actions',label:copy.accounts.actions,render:item => <button disabled={busy} onClick={() => showCases(item)}>{copy.billing.viewCases}</button>}],
  };
  if (orderDetail) return <><AdminPaymentOrderDetail order={orderDetail} copy={copy} language={language} busy={busy} refundSupported={refundSupported} onBack={() => { ops.invalidate(); setOrderDetail(null); }} onRefund={refund} />{notice && <p role="status">{notice}</p>}{error && <p role="alert">{error}</p>}</>;
  if (holdDetail) return <AdminUsageHoldDetail value={holdDetail} copy={copy} busy={busy} onBack={() => { ops.invalidate(); setHoldDetail(null); }} />;
  if (ledger) return <BillingDetail copy={copy} title={`${copy.billing.ledger} · ${shortID(ledger.wallet.wallet_id)}`} items={ledger.items} columns={[{key:'entry_type',label:copy.billing.type},{key:'business_reference',label:copy.billing.reference,render:item => <code>{item.business_reference}</code>},{key:'description',label:copy.billing.description},{key:'delta_microcredits',label:copy.billing.amount,render:item => formatPoints(item.delta_microcredits, language)},{key:'ledger_account_code',label:copy.billing.account,render:item => <span>{item.ledger_account_code} · {item.ledger_account_type}<br/><code>{item.ledger_account_id}</code></span>},{key:'entitlement_component_id',label:copy.billing.component,render:item => <span>{item.component_type || '—'}<br/><code>{item.entitlement_component_id || '—'}</code></span>},{key:'entitlement_package_id',label:copy.billing.package},{key:'journal_balance_microcredits',label:copy.billing.balanceProof,render:item => formatPoints(item.journal_balance_microcredits, language)},{key:'created_at',label:copy.accounts.created,render:item => formatDate(item.created_at)}]} rowKey="posting_id" onBack={() => { ops.invalidate(); setLedger(null); }} />;
  if (cases) return <AdminReconciliationCases initial={cases} client={client} copy={copy} onAuthorizationError={onAuthorizationError} onBack={() => { ops.invalidate(); setCases(null); }} />;
  const resourceError = data.resourceErrors?.[tab];
  if (['metering','receivables','refunds','lateReviews','anomalies'].includes(tab)) return <section className="adm-panel"><Tabs active={tab} onChange={changeTab} items={['wallets','prices','orders','adjustments','holds','reconciliations','metering','receivables','refunds','lateReviews','anomalies'].map(key => ({ key, label:copy.billing[key] }))} />{tab === 'metering' ? <AdminPricing client={client} copy={copy} onAuthorizationError={onAuthorizationError} /> : <AdminFinancialOperations key={tab} tab={tab} client={client} copy={copy} language={language} onAuthorizationError={onAuthorizationError} />}</section>;
  return <section className="adm-panel"><SectionHeader title={copy.billing.title} count={items.length} copy={copy} /><Tabs active={tab} onChange={changeTab} items={['wallets','prices','orders','adjustments','holds','reconciliations','metering','receivables','refunds','lateReviews','anomalies'].map(key => ({key,label:copy.billing[key]}))} />{!resourceError && tab === 'orders' && <form className="adm-ops-form" onSubmit={searchOrders}><input value={orderFilter.q} onChange={event => setOrderFilter(current => ({ ...current, q:event.target.value }))} maxLength={160} placeholder={copy.billing.orderSearch} /><select value={orderFilter.status} onChange={event => setOrderFilter(current => ({ ...current, status:event.target.value }))}><option value="">{copy.billing.allStatuses}</option>{['CREATED','PENDING','PAID','PAID_LATE','FAILED','CLOSED','PARTIALLY_REFUNDED','REFUNDED','MANUAL_REVIEW'].map(status => <option key={status}>{status}</option>)}</select><button disabled={busy}>{copy.searchAction}</button></form>}{!resourceError && tab === 'prices' && <form className="adm-ops-form" onSubmit={createPrice}><input placeholder={copy.tokens.model} value={price.model} onChange={event => setPrice(current => ({ ...current, model:event.target.value }))} /><input type="number" min="0" step="0.0001" placeholder={copy.billing.inputPrice} value={price.input} onChange={event => setPrice(current => ({ ...current, input:event.target.value }))} /><input type="number" min="0" step="0.0001" placeholder={copy.billing.outputPrice} value={price.output} onChange={event => setPrice(current => ({ ...current, output:event.target.value }))} /><input type="number" min="0" step="0.0001" placeholder={copy.billing.cachedPrice} value={price.cached} onChange={event => setPrice(current => ({ ...current, cached:event.target.value }))} /><button disabled={busy}>{copy.billing.activatePrice}</button></form>}{hasLimitedPaymentCapabilities && <div className="adm-inline-message">{copy.billing.capabilityLimitations}</div>}{tab === 'reconciliations' && paymentCapabilities.reconciliation_mode && <p>{copy.billing.reconciliationMode}: {paymentCapabilities.reconciliation_mode}</p>}{tab === 'reconciliations' && paymentCapabilities.reconciliation_mode === 'QUERY_DEGRADED' && <p>{copy.billing.queryDegraded}</p>}{!resourceError && tab === 'reconciliations' && reconciliationSupported && <form className="adm-ops-form" onSubmit={event => { event.preventDefault(); void perform(() => client.runReconciliation(reconciliation.channel, reconciliation.businessDate)); }}><select value={reconciliation.channel} onChange={event => setReconciliation(current => ({ ...current, channel:event.target.value }))}><option>ALIPAY</option><option>WECHAT</option><option>UNIONPAY</option></select><input type="date" value={reconciliation.businessDate} onChange={event => setReconciliation(current => ({ ...current, businessDate:event.target.value }))} /><button disabled={busy}>{copy.billing.runReconciliation}</button></form>}{!resourceError && tab === 'reconciliations' && reconciliationImportSupported && <form className="adm-ops-form" onSubmit={event => {
    event.preventDefault();
    void perform(async ({ isCurrent }) => {
      const billItems = JSON.parse(importBillJSON);
      if (!Array.isArray(billItems) || billItems.some(item => !item || !Number.isSafeInteger(item.amount_fen) || item.amount_fen < 0)) throw new Error('REQUEST_FAILED');
      requireAdminFacts(requireAdminRecord(await client.importProviderBill(reconciliation.channel, reconciliation.businessDate, billItems), 'reconciliation_run_id'), { channel:reconciliation.channel, business_date:reconciliation.businessDate, source_mode:'FILE_IMPORT' });
      if (isCurrent()) setImportBillJSON('[]');
    });
  }}><label>{copy.billing.channel}<select value={reconciliation.channel} onChange={event => setReconciliation(current => ({ ...current, channel:event.target.value }))}><option>ALIPAY</option><option>WECHAT</option><option>UNIONPAY</option></select></label><label>{copy.billing.businessDate}<input type="date" required value={reconciliation.businessDate} onChange={event => setReconciliation(current => ({ ...current, businessDate:event.target.value }))} /></label><label>{copy.billing.importBill}<textarea required value={importBillJSON} onChange={event => setImportBillJSON(event.target.value)} /></label><button disabled={busy}>{copy.billing.importBill}</button></form>}{notice && <div className="adm-inline-message ok">{notice}</div>}{error && <div className="adm-inline-message error">{error}</div>}{resourceError ? <p role="alert">{resourceError === 'denied' ? copy.denied : copy.failed}</p> : <Table columns={columns[tab]} items={items} copy={copy} rowKey={{ wallets:'wallet_id', prices:'price_version_id', orders:'order_id', adjustments:'adjustment_id', holds:'hold_id', reconciliations:'reconciliation_run_id' }[tab]} />}</section>;
}

function BillingDetail({ copy, title, items, columns, rowKey, onBack }) {
  return <section className="adm-panel"><button className="adm-back" onClick={onBack}>← {copy.billing.back}</button><SectionHeader title={title} count={items.length} copy={copy} /><Table columns={columns} items={items} copy={copy} rowKey={rowKey} /></section>;
}

function Tokens({ copy, language, data, client, setData, onAuthorizationError }) {
  const [tab, setTab] = useState('tokenAccounts');
  const [account, setAccount] = useState(null);
  const [usageDetail, setUsageDetail] = useState(null);
  const ops = useAdminOperation(onAuthorizationError, 'token');
  const { busy } = ops;
  const notice = ops.noticeKey ? copy.tokens[ops.noticeKey] : '';
  const error = ops.failed ? copy.tokens.actionFailed : '';
  const [route, setRoute] = useState({ model:'', priority:100, default_scope:'NONE', health_status:'HEALTHY' });
  const items = data[tab] || [];
  const back = () => { ops.invalidate(); setAccount(null); setUsageDetail(null); };
  const changeTab = key => { back(); setTab(key); };
  const openAccount = item => ops.run(async ({ signal, isCurrent }) => {
    const result = await client.tokenAccount(item.token_account_id, signal);
    if (isCurrent()) { setAccount(result); setUsageDetail(null); setRoute({ model:'', priority:100, default_scope:'NONE', health_status:'HEALTHY' }); }
  });
  const openUsage = item => ops.run(async ({ signal, isCurrent }) => { const result = await client.usageRequest(item.usage_request_id, signal); if (isCurrent()) { setUsageDetail(result); setAccount(null); } });
  const refreshAccount = async ({ signal, isCurrent }) => {
    const result = await client.tokenAccount(account.token_account_id, signal);
    if (isCurrent()) {
      setAccount(result);
      setData(current => ({ ...current, tokenAccounts:(current.tokenAccounts || []).map(item => item.token_account_id === result.token_account_id ? { ...item, status:result.status, route_count:result.routes?.length, active_credential_count:result.credentials?.filter(credential => credential.status === 'ACTIVE').length } : item), tokenOverview:undefined }));
    }
  };
  const perform = action => ops.run(async context => {
    const key = await action(context);
    if (!context.isCurrent()) return;
    await refreshAccount(context);
    return key || 'completed';
  }, true);
  const updateStatus = () => {
    if (busy || !account || account.status === 'FROZEN') return;
    const status = account.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE';
    const reason = window.prompt(copy.tokens.statusReason);
    if (reason?.trim()) void perform(() => client.setTokenAccountStatus(account.token_account_id, status, reason.trim()).then(() => 'completed'));
  };
  const saveRoute = event => {
    event.preventDefault();
    const priority = Number(route.priority);
    if (!account || !route.model.trim() || !String(route.priority).trim() || !Number.isSafeInteger(priority) || priority < 0 || priority > 10000) return;
    void perform(async ({ isCurrent }) => { await client.upsertTokenRoute(account.token_account_id, { ...route, model:route.model.trim(), priority }); if (isCurrent()) setRoute(current => ({ ...current, model:'' })); });
  };
  const deleteRoute = model => {
    if (busy || !account || !window.confirm(copy.tokens.confirmDeleteRoute(model))) return;
    void perform(() => client.deleteTokenRoute(account.token_account_id, model).then(() => 'completed'));
  };
  const validateCredential = credentialID => perform(async () => { const result = await client.validateTokenCredential(account.token_account_id, credentialID); return result.valid ? 'credentialValid' : 'credentialInvalid'; });
  const retryReconciliation = () => {
    if (busy || !usageDetail) return;
    const reason = window.prompt(copy.tokens.retryReason);
    if (!reason?.trim()) return;
    void ops.run(async ({ isCurrent }) => {
      const result = await client.retryUsageReconciliation(usageDetail.usage_request_id, reason.trim());
      if (isCurrent()) {
        setUsageDetail(result);
        setData(current => ({ ...current, usage:(current.usage || []).map(item => item.usage_request_id === result.usage_request_id ? result : item), tokenOverview:undefined }));
      }
      return 'completed';
    }, true);
  };
  if (account) return <TokenAccountDetail copy={copy} account={account} route={route} setRoute={setRoute} busy={busy} notice={notice} error={error} onBack={back} onStatus={updateStatus} onSaveRoute={saveRoute} onDeleteRoute={deleteRoute} onValidate={validateCredential} />;
  if (usageDetail) return <UsageRequestDetail copy={copy} language={language} item={usageDetail} busy={busy} notice={notice} error={error} onBack={back} onRetry={retryReconciliation} />;
  const columns = tab === 'tokenAccounts' ? [
    {key:'name',label:copy.tokens.name,render:item => <button className="adm-name-link" disabled={busy} onClick={() => openAccount(item)}>{item.name}</button>},{key:'owner_id',label:copy.tokens.owner,render:item => <code title={item.owner_id}>{item.owner_type} · {shortID(item.owner_id)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'share_scope',label:copy.tokens.sharing},{key:'active_credential_count',label:copy.tokens.credentials},{key:'route_count',label:copy.tokens.routes},{key:'updated_at',label:copy.billing.updated,render:item => formatDate(item.updated_at)},
  ] : [
    {key:'id',label:copy.tokens.usageID,render:item => <button className="adm-name-link" title={item.usage_request_id} disabled={busy} onClick={() => openUsage(item)}>{shortID(item.usage_request_id)}</button>},{key:'model',label:copy.tokens.model},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'maximum_microcredits',label:copy.tokens.maximum,render:item => formatPoints(item.maximum_microcredits, language)},{key:'actual_microcredits',label:copy.tokens.actual,render:item => formatPoints(item.actual_microcredits, language)},{key:'newapi_request_id',label:copy.tokens.newAPIRequest,render:item => <code title={item.newapi_request_id}>{shortID(item.newapi_request_id)}</code>},{key:'error_class',label:copy.tokens.error},{key:'started_at',label:copy.tokens.started,render:item => formatDate(item.started_at)},
  ];
  return <section className="adm-panel"><SectionHeader title={copy.tokens.title} count={items.length} copy={copy} /><Tabs active={tab} onChange={changeTab} items={[{key:'tokenAccounts',label:copy.tokens.accounts},{key:'usage',label:copy.tokens.usage}]} />{notice && <div className="adm-inline-message ok">{notice}</div>}{error && <div className="adm-inline-message error">{error}</div>}{data.resourceErrors?.[tab] ? <p role="alert">{data.resourceErrors[tab] === 'denied' ? copy.denied : copy.failed}</p> : <Table columns={columns} items={items} copy={copy} rowKey={tab === 'tokenAccounts' ? 'token_account_id' : 'usage_request_id'} />}</section>;
}

function TokenAccountDetail({ copy, account, route, setRoute, busy, notice, error, onBack, onStatus, onSaveRoute, onDeleteRoute, onValidate }) {
  const credentialColumns = [{key:'provider',label:copy.tokens.provider},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'validation_status',label:copy.tokens.validation,render:item => <Status value={item.validation_status} />},{key:'fingerprint',label:copy.tokens.fingerprint,render:item => <code>{item.fingerprint}</code>},{key:'allowed_models',label:copy.tokens.models,render:item => item.allowed_models.join(' · ')},{key:'updated_at',label:copy.billing.updated,render:item => formatDate(item.updated_at)},{key:'actions',label:copy.accounts.actions,render:item => <button disabled={busy || item.status !== 'ACTIVE'} onClick={() => onValidate(item.credential_id)}>{copy.tokens.validate}</button>}];
  const routeColumns = [{key:'model',label:copy.tokens.model,render:item => <strong>{item.model}</strong>},{key:'priority',label:copy.tokens.priority},{key:'default_scope',label:copy.tokens.defaultScope},{key:'health_status',label:copy.tokens.health,render:item => <Status value={item.health_status} />},{key:'updated_at',label:copy.billing.updated,render:item => formatDate(item.updated_at)},{key:'actions',label:copy.accounts.actions,render:item => <button className="danger" disabled={busy} onClick={() => onDeleteRoute(item.model)}>{copy.accounts.delete}</button>}];
  return <section className="adm-panel"><button className="adm-back" onClick={onBack}>← {copy.tokens.back}</button><SectionHeader title={`${copy.tokens.accountDetail} · ${account.name}`} copy={copy}><button disabled={busy || account.status === 'FROZEN'} onClick={onStatus}>{account.status === 'ACTIVE' ? copy.accounts.disable : copy.accounts.enable}</button></SectionHeader>{notice && <div className="adm-inline-message ok">{notice}</div>}{error && <div className="adm-inline-message error">{error}</div>}<h3>{copy.tokens.credentialMetadata}</h3><p className="adm-help">{copy.tokens.credentialSafety}</p><Table columns={credentialColumns} items={account.credentials} copy={copy} rowKey="credential_id" /><h3>{copy.tokens.routes}</h3><form className="adm-ops-form" onSubmit={onSaveRoute}><input placeholder={copy.tokens.model} value={route.model} onChange={event => setRoute(current => ({ ...current, model:event.target.value }))} /><input type="number" min="0" max="10000" value={route.priority} onChange={event => setRoute(current => ({ ...current, priority:event.target.value }))} /><select value={route.default_scope} onChange={event => setRoute(current => ({ ...current, default_scope:event.target.value }))}><option>NONE</option><option>UNIT</option><option>ORG</option></select><select value={route.health_status} onChange={event => setRoute(current => ({ ...current, health_status:event.target.value }))}><option>HEALTHY</option><option>DEGRADED</option><option>UNHEALTHY</option></select><button disabled={busy || !route.model.trim()}>{copy.tokens.saveRoute}</button></form><Table columns={routeColumns} items={account.routes} copy={copy} rowKey="model" /></section>;
}

function UsageRequestDetail({ copy, language, item, busy, notice, error, onBack, onRetry }) {
  const rows = [
    [copy.tokens.model,item.model],[copy.accounts.status,<Status key="status" value={item.status} />],[copy.tokens.maximum,formatPoints(item.maximum_microcredits, language)],[copy.tokens.actual,formatPoints(item.actual_microcredits, language)],[copy.tokens.newAPIRequest,<code key="newapi">{item.newapi_request_id || '—'}</code>],[copy.tokens.providerRequest,<code key="provider">{item.provider_request_id || '—'}</code>],[copy.tokens.attemptStatus,item.attempt_status || '—'],[copy.tokens.tokens,`${formatNumber(item.input_tokens)} / ${formatNumber(item.output_tokens)} / ${formatNumber(item.cached_input_tokens)}`],[copy.tokens.reconciliationAttempts,formatNumber(item.reconciliation_attempts)],[copy.tokens.lastError,item.reconciliation_last_error || item.error_class || '—'],[copy.tokens.started,formatDate(item.started_at)],[copy.billing.updated,formatDate(item.updated_at)],
  ];
  const canRetry = ['SETTLEMENT_PENDING','REVIEW'].includes(item.status);
  return <section className="adm-panel"><button className="adm-back" onClick={onBack}>← {copy.tokens.back}</button><SectionHeader title={`${copy.tokens.usageDetail} · ${shortID(item.usage_request_id)}`} copy={copy}>{canRetry && <button disabled={busy} onClick={onRetry}>{copy.tokens.retry}</button>}</SectionHeader>{notice && <div className="adm-inline-message ok">{notice}</div>}{error && <div className="adm-inline-message error">{error}</div>}<dl className="adm-detail-list">{rows.map(([label,value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl></section>;
}

function SystemStatus({ copy, data }) {
  const status = data.systemStatus || { services:[], relays:[], reconciliation:{}, recent_errors:[] };
  const serviceColumns = [{key:'key',label:copy.system.service,render:item => <strong>{copy.system.names[item.key] || item.key}</strong>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'latency_ms',label:copy.system.latency,render:item => `${formatNumber(item.latency_ms)} ms`},{key:'message',label:copy.system.message}];
  const relayColumns = [{key:'key',label:copy.system.service,render:item => <strong>{copy.system.names[item.key] || item.key}</strong>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'pending_events',label:copy.system.pending},{key:'dead_lettered_events',label:copy.system.deadLetters},{key:'oldest_pending_seconds',label:copy.system.oldest,render:item => `${formatNumber(item.oldest_pending_seconds)} s`},{key:'message',label:copy.system.message}];
  const errorColumns = [{key:'source',label:copy.system.source,render:item => <strong>{copy.system.names[item.source] || item.source}</strong>},{key:'resource',label:copy.system.resource,render:item => <code title={item.resource}>{shortID(item.resource)}</code>},{key:'message',label:copy.system.message},{key:'updated_at',label:copy.billing.updated,render:item => formatDate(item.updated_at)}];
  return <section><div className="adm-metrics"><Metric label={copy.system.healthyServices} value={status.services.filter(item => item.status === 'HEALTHY').length} /><Metric label={copy.system.pendingReconciliation} value={status.reconciliation.pending} /><Metric label={copy.system.stoppedReconciliation} value={status.reconciliation.stopped} tone={Number(status.reconciliation.stopped) ? 'accent' : ''} /></div><article className="adm-panel"><SectionHeader title={copy.system.services} count={status.services.length} copy={copy} /><Table columns={serviceColumns} items={status.services} copy={copy} rowKey="key" /><h3>{copy.system.relays}</h3><Table columns={relayColumns} items={status.relays} copy={copy} rowKey="key" /><h3>{copy.system.recentErrors}</h3><Table columns={errorColumns} items={status.recent_errors} copy={copy} /></article></section>;
}

function IdentitySecurity({ copy }) {
  const [copied, setCopied] = useState('');
  const commands = {
    ssh:'ssh -N -T -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -L 127.0.0.1:18081:127.0.0.1:18080 <ssh-user>@<server>',
    kubernetes:'kubectl -n <namespace> port-forward service/<release>-account-platform-keycloak 18081:8080 --address 127.0.0.1',
  };
  const copyCommand = async key => {
    try { await navigator.clipboard.writeText(commands[key]); setCopied(key); }
    catch { setCopied(''); }
  };
  return <section className="adm-security-grid">
    <article className="adm-panel adm-security-intro">
      <span className="adm-eyebrow">KEYCLOAK</span><h2>{copy.identity.title}</h2><p>{copy.identity.lead}</p>
      <div className="adm-security-warning"><strong>{copy.identity.publicBlocked}</strong><span>{copy.identity.publicBlockedHelp}</span></div>
      <a className="adm-security-open" href="http://127.0.0.1:18081/admin/" target="_blank" rel="noreferrer">{copy.identity.openLocal}</a>
      <small>{copy.identity.openHelp}</small>
    </article>
    <article className="adm-panel">
      <SectionHeader title={copy.identity.accessMethods} copy={copy} />
      <SecureCommand title={copy.identity.ssh} help={copy.identity.sshHelp} command={commands.ssh} copied={copied === 'ssh'} copy={copy} onCopy={() => copyCommand('ssh')} />
      <SecureCommand title={copy.identity.kubernetes} help={copy.identity.kubernetesHelp} command={commands.kubernetes} copied={copied === 'kubernetes'} copy={copy} onCopy={() => copyCommand('kubernetes')} />
      <ol className="adm-security-steps"><li>{copy.identity.stepTunnel}</li><li>{copy.identity.stepOpen}</li><li>{copy.identity.stepClose}</li></ol>
    </article>
  </section>;
}

function SecureCommand({ title, help, command, copied, copy, onCopy }) {
  return <div className="adm-secure-command"><div><strong>{title}</strong><span>{help}</span></div><code>{command}</code><button type="button" onClick={onCopy}>{copied ? copy.identity.copied : copy.identity.copy}</button></div>;
}

function Audit({ copy, data, client, setData, onAuthorizationError }) {
  const [filters, setFilters] = useState({ actor:'', action:'', resource:'', result:'', from:'', to:'' });
  const [detail, setDetail] = useState(null);
  const ops = useAdminOperation(onAuthorizationError, 'account');
  const { busy } = ops;
  const error = ops.failed ? copy.audit.actionFailed : '';
  const items = data.audits || [];
  const queryFilters = () => ({ ...filters, from:filters.from ? new Date(filters.from).toISOString() : '', to:filters.to ? new Date(filters.to).toISOString() : '' });
  const search = event => {
    event?.preventDefault();
    void ops.run(async ({ signal, isCurrent }) => { const result = await client.audits(queryFilters(), signal); if (isCurrent()) setData(current => ({ ...current, audits:result.items || [] })); });
  };
  const showDetail = item => ops.run(async ({ signal, isCurrent }) => { const result = await client.audit(item.audit_id, signal); if (isCurrent()) setDetail(result); });
  const exportItems = () => exportAuditCSV(items, copy.audit.exportFile);
  if (detail) return <AuditDetail copy={copy} item={detail} onBack={() => { ops.invalidate(); setDetail(null); }} />;
  const columns = [
    {key:'action',label:copy.audit.action,render:item => <button className="adm-name-link" disabled={busy} onClick={() => showDetail(item)}>{item.action}</button>},{key:'resource_type',label:copy.audit.resource,render:item => <span>{item.resource_type}<br /><code title={item.resource_id}>{shortID(item.resource_id)}</code></span>},{key:'actor_user_id',label:copy.audit.actor,render:item => <span>{item.actor_display_name || item.actor_email || '—'}<br /><code title={item.actor_user_id}>{shortID(item.actor_user_id)}</code></span>},{key:'result',label:copy.audit.result,render:item => <Status value={item.result} />},{key:'request_id',label:copy.audit.request,render:item => <code title={item.request_id}>{shortID(item.request_id)}</code>},{key:'occurred_at',label:copy.audit.occurred,render:item => formatDate(item.occurred_at)},
  ];
  return <section className="adm-panel"><SectionHeader title={copy.audit.title} count={items.length} copy={copy}><button disabled={!items.length} onClick={exportItems}>{copy.audit.export}</button></SectionHeader><form className="adm-ops-form" onSubmit={search}><input placeholder={copy.audit.actorFilter} value={filters.actor} onChange={event => setFilters(current => ({ ...current, actor:event.target.value }))} /><input placeholder={copy.audit.actionFilter} value={filters.action} onChange={event => setFilters(current => ({ ...current, action:event.target.value }))} /><input placeholder={copy.audit.resourceFilter} value={filters.resource} onChange={event => setFilters(current => ({ ...current, resource:event.target.value }))} /><select value={filters.result} onChange={event => setFilters(current => ({ ...current, result:event.target.value }))}><option value="">{copy.audit.allResults}</option><option>SUCCEEDED</option><option>DENIED</option><option>FAILED</option></select><label className="adm-filter-date"><span>{copy.audit.from}</span><input type="datetime-local" value={filters.from} onChange={event => setFilters(current => ({ ...current, from:event.target.value }))} /></label><label className="adm-filter-date"><span>{copy.audit.to}</span><input type="datetime-local" value={filters.to} onChange={event => setFilters(current => ({ ...current, to:event.target.value }))} /></label><button disabled={busy}>{copy.searchAction}</button></form>{error && <div className="adm-inline-message error">{error}</div>}<Table columns={columns} items={items} copy={copy} rowKey="audit_id" /></section>;
}

function AuditDetail({ copy, item, onBack }) {
  const rows = [[copy.audit.source,copy.system.names.account],[copy.audit.actor,item.actor_display_name || item.actor_email || item.actor_user_id || '—'],[copy.audit.action,item.action],[copy.audit.resource,`${item.resource_type} · ${item.resource_id}`],[copy.audit.result,<Status key="result" value={item.result} />],[copy.audit.request,item.request_id],[copy.audit.organization,item.active_organization_id || '—'],[copy.audit.occurred,formatDate(item.occurred_at)]];
  return <section className="adm-panel"><button className="adm-back" onClick={onBack}>← {copy.audit.back}</button><SectionHeader title={copy.audit.detail} copy={copy} /><dl className="adm-detail-list">{rows.map(([label,value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl><h3>{copy.audit.changeSummary}</h3><pre className="adm-json-detail">{JSON.stringify(item.change_summary || {}, null, 2)}</pre></section>;
}

function exportAuditCSV(items, filename) {
  const blob = new Blob([auditCSV(items)], { type:'text/csv;charset=utf-8' });
  const href = URL.createObjectURL(blob); const link = document.createElement('a'); link.href = href; link.download = filename; link.click(); URL.revokeObjectURL(href);
}
