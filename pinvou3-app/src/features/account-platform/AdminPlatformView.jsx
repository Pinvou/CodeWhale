import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { buildUnitTree } from './admin-organization-tree.js';
import { formatPoints } from './credit-units.js';
import './admin-platform.css';

const NAV_ICONS = { overview:'⌂', organizations:'▦', users:'◎', billing:'◇', tokens:'↗', audit:'≡' };
const okStatuses = new Set(['ACTIVE', 'PAID', 'SETTLED', 'HEALTHY', 'APPROVED', 'SUCCESS']);
const warnStatuses = new Set(['PENDING', 'CREATED', 'SETTLEMENT_PENDING', 'REVIEW', 'MANUAL_REVIEW', 'PENDING_APPROVAL']);

function formatNumber(value) { return Number(value || 0).toLocaleString(); }
function formatMoney(value) { return `¥${(Number(value || 0) / 100).toLocaleString(undefined, { minimumFractionDigits:2, maximumFractionDigits:2 })}`; }
function formatDate(value) { return value ? new Date(value).toLocaleString() : '—'; }
function shortID(value) { const text = String(value || ''); return text.length > 16 ? `${text.slice(0, 8)}…${text.slice(-5)}` : text || '—'; }

function Status({ value }) {
  const status = String(value || '—');
  const tone = okStatuses.has(status) ? 'ok' : warnStatuses.has(status) ? 'warn' : status === '—' ? '' : 'neutral';
  return <span className={`adm-status ${tone}`}>{status}</span>;
}

function Metric({ label, value, tone = '', formatter = formatNumber }) {
  return <article className={`adm-metric ${tone}`}><span>{label}</span><strong>{formatter(value)}</strong></article>;
}

function Empty({ copy }) { return <div className="adm-empty">{copy.noData}</div>; }

function Table({ columns, items, copy }) {
  if (!items?.length) return <Empty copy={copy} />;
  return <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{columns.map(column => <th key={column.key}>{column.label}</th>)}</tr></thead><tbody>{items.map((item, index) => <tr key={item.user_id || item.organization_id || item.wallet_id || item.order_id || item.adjustment_id || item.hold_id || item.token_account_id || item.usage_request_id || item.audit_id || index}>{columns.map(column => <td key={column.key}>{column.render ? column.render(item) : item[column.key] ?? '—'}</td>)}</tr>)}</tbody></table></div>;
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
  const [data, setData] = useState({});
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [updatedAt, setUpdatedAt] = useState(null);

  const load = useCallback(async (target, signal) => {
    setBusy(true); setError('');
    try {
      let next = {};
      if (target === 'overview') {
        const [account, billing, token] = await Promise.all([client.accountOverview(signal), client.billingOverview(signal), client.tokenOverview(signal)]);
        next = { accountOverview:account, billingOverview:billing, tokenOverview:token };
      } else if (target === 'organizations') {
        const [users, organizations] = await Promise.all([client.users('', signal), client.organizations('', signal)]);
        next = { users:users.items || [], organizations:organizations.items || [] };
      } else if (target === 'users') {
        const users = await client.users('', signal);
        next = { users:users.items || [] };
      } else if (target === 'billing') {
        const [wallets, orders, adjustments, holds] = await Promise.all([client.wallets(signal), client.paymentOrders(signal), client.adjustments(signal), client.holds(signal)]);
        next = { wallets:wallets.items || [], orders:orders.items || [], adjustments:adjustments.items || [], holds:holds.items || [] };
      } else if (target === 'tokens') {
        const [accounts, usage] = await Promise.all([client.tokenAccounts(signal), client.usageRequests(signal)]);
        next = { tokenAccounts:accounts.items || [], usage:usage.items || [] };
      } else {
        const audits = await client.audits(signal); next = { audits:audits.items || [] };
      }
      setData(current => ({ ...current, ...next })); setUpdatedAt(new Date()); setAuth('ready');
    } catch (reason) {
      if (reason?.name === 'AbortError') return;
      if (reason?.status === 401) setAuth('guest');
      else if (reason?.status === 403) setAuth('denied');
      else { setAuth(current => current === 'loading' ? 'error' : current); setError(copy.failed); }
    } finally { setBusy(false); }
  }, [client, copy.failed]);

  useEffect(() => {
    const controller = new globalThis.AbortController();
    client.session(controller.signal).then(session => {
      if (!session.authenticated) { setAuth('guest'); return; }
      load('overview', controller.signal);
    }).catch(reason => { if (reason?.name !== 'AbortError') { setAuth('error'); setError(copy.failed); } });
    return () => controller.abort();
  }, [client, copy.failed, load]);

  const navigate = key => { setSection(key); if (!sectionHasData(key, data)) load(key); };
  const logout = async () => { await client.logout(); window.location.assign('/admin.html'); };

  if (auth === 'loading') return <StatePage copy={copy} kind="loading" />;
  if (auth === 'guest') return <StatePage copy={copy} kind="guest" />;
  if (auth === 'denied') return <StatePage copy={copy} kind="denied" />;
  if (auth === 'error') return <StatePage copy={copy} kind="error" onRetry={() => window.location.reload()} />;

  return <div className="adm-shell">
    <aside className="adm-sidebar">
      <div className="adm-brand"><div className="adm-mark">P</div><div><strong>{copy.title}</strong><span>{copy.product}</span></div></div>
      <nav>{Object.keys(copy.nav).map(key => <button key={key} className={section === key ? 'active' : ''} onClick={() => navigate(key)}><span>{NAV_ICONS[key]}</span>{copy.nav[key]}</button>)}</nav>
      <div className="adm-sidebar-foot"><span>{copy.readOnly}</span><select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}><option value="zh">简体中文</option><option value="en">English</option><option value="ja">日本語</option></select></div>
    </aside>
    <main className="adm-main">
      <header className="adm-topbar"><div><span className="adm-eyebrow">{copy.product}</span><h1>{copy.nav[section]}</h1></div><div className="adm-actions">{updatedAt && <small>{copy.updated} {updatedAt.toLocaleTimeString()}</small>}<button onClick={() => load(section)} disabled={busy}>{copy.refresh}</button><div className="adm-admin"><span>PA</span><div><strong>{copy.administrator}</strong><button onClick={logout}>{copy.logout}</button></div></div></div></header>
      {error && <div role="alert" className="adm-alert">{error}</div>}
      {busy && <div className="adm-progress" />}
      {section === 'overview' && <Overview copy={copy} language={language} data={data} />}
      {section === 'organizations' && <AccountDirectory section="organizations" copy={copy} data={data} client={client} setData={setData} />}
      {section === 'users' && <AccountDirectory section="users" copy={copy} data={data} client={client} setData={setData} />}
      {section === 'billing' && <Billing copy={copy} language={language} data={data} />}
      {section === 'tokens' && <Tokens copy={copy} language={language} data={data} />}
      {section === 'audit' && <Audit copy={copy} data={data} />}
    </main>
  </div>;
}

function sectionHasData(section, data) {
  return section === 'overview' ? data.accountOverview : section === 'organizations' ? data.organizations : section === 'users' ? data.users : section === 'billing' ? data.wallets : section === 'tokens' ? data.tokenAccounts : data.audits;
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
  return <section><div className="adm-hero"><div><h2>{copy.overview.title}</h2><p>{copy.overview.lead}</p></div><Status value="ACTIVE" /></div><div className="adm-metrics"><Metric label={copy.overview.users} value={a.users} /><Metric label={copy.overview.organizations} value={a.organizations} /><Metric label={copy.overview.available} value={Number(b.posted_microcredits || 0) - Number(b.reserved_microcredits || 0)} tone="accent" formatter={value => formatPoints(value, language)} /><Metric label={copy.overview.reserved} value={b.reserved_microcredits} formatter={value => formatPoints(value, language)} /><Metric label={copy.overview.tokenAccounts} value={t.token_accounts} /><Metric label={copy.overview.usageRequests} value={t.usage_requests} /></div><div className="adm-overview-grid"><article className="adm-panel"><SectionHeader title={copy.overview.attention} copy={copy} /><div className="adm-attention">{alerts.map(([label,value]) => <div key={label}><span>{label}</span><strong className={Number(value) ? 'has-value' : ''}>{formatNumber(value)}</strong></div>)}</div></article><article className="adm-panel adm-health"><span>{copy.overview.healthyRoutes}</span><strong>{formatNumber(t.healthy_routes)}</strong><small>/ {formatNumber(t.token_accounts)} {copy.overview.tokenAccounts}</small><div><i style={{ width:`${Math.min(100, Number(t.token_accounts) ? Number(t.healthy_routes) / Number(t.token_accounts) * 100 : 0)}%` }} /></div></article></div></section>;
}

function AccountDirectory({ section, copy, data, client, setData }) {
  const [search, setSearch] = useState('');
  const [actionKey, setActionKey] = useState('');
  const [notice, setNotice] = useState('');
  const [actionError, setActionError] = useState('');
  const [selectedOrganization, setSelectedOrganization] = useState(null);
  const [organizationName, setOrganizationName] = useState('');
  const [ownerUserID, setOwnerUserID] = useState('');
  const isUserSection = section === 'users';
  const items = isUserSection ? data.users || [] : data.organizations || [];
  const activeUsers = useMemo(() => (data.users || []).filter(user => user.status === 'ACTIVE'), [data.users]);
  useEffect(() => {
    if (!ownerUserID && activeUsers[0]) setOwnerUserID(activeUsers[0].user_id);
  }, [activeUsers, ownerUserID]);
  const reload = async () => {
    const result = isUserSection ? await client.users(search) : await client.organizations(search);
    setData(current => ({ ...current, [section]:result.items || [], accountOverview:undefined }));
  };
  const perform = async (item, action) => {
    const isUser = isUserSection;
    const id = isUser ? item.user_id : item.organization_id;
    const label = isUser ? item.display_name || item.email || shortID(id) : item.name;
    if (action === 'delete' && !window.confirm(isUser ? copy.accounts.confirmUser(label) : copy.accounts.confirmOrganization(label))) return;
    setActionKey(`${section}:${id}:${action}`); setNotice(''); setActionError('');
    try {
      if (isUser) {
        if (action === 'delete') await client.deleteUser(id);
        else await client.setUserStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
      } else if (action === 'delete') await client.deleteOrganization(id);
      else await client.setOrganizationStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
      await reload(); setNotice(copy.accounts.updated);
    } catch (reason) {
      setActionError(reason?.code === 'LAST_OWNER_REQUIRED' ? copy.accounts.ownerRequired : reason?.code === 'ACCOUNT_CONFLICT' ? copy.accounts.conflict : copy.accounts.actionFailed);
    } finally { setActionKey(''); }
  };
  const actionButtons = (item, isUser) => {
    const id = isUser ? item.user_id : item.organization_id;
    const prefix = `${isUser ? 'users' : 'organizations'}:${id}:`;
    const buttons = [];
    if (item.status === 'DISABLED') buttons.push(<button key="enable" disabled={Boolean(actionKey)} onClick={() => perform(item, 'enable')}>{copy.accounts.enable}</button>);
    if (item.status === 'ACTIVE' || (isUser && item.status === 'PROVISIONING')) buttons.push(<button key="disable" disabled={Boolean(actionKey)} onClick={() => perform(item, 'disable')}>{copy.accounts.disable}</button>);
    buttons.push(<button key="delete" className="danger" disabled={Boolean(actionKey)} onClick={() => perform(item, 'delete')}>{actionKey === `${prefix}delete` ? '…' : copy.accounts.delete}</button>);
    return <div className="adm-row-actions">{buttons}</div>;
  };
  const columns = isUserSection ? [
    { key:'display_name', label:copy.accounts.displayName, render:item => <strong>{item.display_name || '—'}</strong> }, { key:'email', label:copy.accounts.email }, { key:'status', label:copy.accounts.status, render:item => <Status value={item.status} /> }, { key:'organization_count', label:copy.accounts.memberships }, { key:'last_seen_at', label:copy.accounts.lastSeen, render:item => formatDate(item.last_seen_at) }, { key:'actions', label:copy.accounts.actions, render:item => actionButtons(item, true) },
  ] : [
    { key:'name', label:copy.accounts.organization, render:item => <button className="adm-name-link" onClick={() => setSelectedOrganization(item)}>{item.name}</button> }, { key:'organization_id', label:'ID', render:item => <code title={item.organization_id}>{shortID(item.organization_id)}</code> }, { key:'status', label:copy.accounts.status, render:item => <Status value={item.status} /> }, { key:'member_count', label:copy.accounts.members }, { key:'created_at', label:copy.accounts.created, render:item => formatDate(item.created_at) }, { key:'actions', label:copy.accounts.actions, render:item => actionButtons(item, false) },
  ];
  const submit = async event => { event.preventDefault(); await reload(); };
  const createOrganization = async event => {
    event.preventDefault();
    if (!organizationName.trim() || !ownerUserID || actionKey) return;
    setActionKey('organization:create'); setNotice(''); setActionError('');
    try {
      await client.createOrganization(organizationName.trim(), ownerUserID);
      setOrganizationName('');
      await reload();
      setNotice(copy.accounts.organizationCreated);
    } catch {
      setActionError(copy.accounts.actionFailed);
    } finally { setActionKey(''); }
  };
  if (selectedOrganization) return <OrganizationDetail copy={copy} organization={selectedOrganization} client={client} onBack={() => setSelectedOrganization(null)} />;
  return <section className="adm-panel"><SectionHeader title={copy.accounts[section]} count={items.length} copy={copy}><form className="adm-search" onSubmit={submit}><input value={search} onChange={event => setSearch(event.target.value)} placeholder={copy.search} /><button>{copy.searchAction}</button></form></SectionHeader>{!isUserSection && <form className="adm-create-row" onSubmit={createOrganization}><input value={organizationName} onChange={event => setOrganizationName(event.target.value)} placeholder={copy.accounts.organizationNamePlaceholder} maxLength={120} /><select value={ownerUserID} onChange={event => setOwnerUserID(event.target.value)} aria-label={copy.accounts.initialOwner}><option value="">{copy.accounts.chooseOwner}</option>{activeUsers.map(user => <option key={user.user_id} value={user.user_id}>{user.display_name || user.email || shortID(user.user_id)}</option>)}</select><button disabled={!organizationName.trim() || !ownerUserID || Boolean(actionKey)}>{copy.accounts.createOrganization}</button>{activeUsers.length === 0 && <small>{copy.accounts.noActiveOwner}</small>}</form>}{notice && <div className="adm-inline-message ok" role="status">{notice}</div>}{actionError && <div className="adm-inline-message error" role="alert">{actionError}</div>}<Table columns={columns} items={items} copy={copy} /></section>;
}

function OrganizationDetail({ copy, organization, client, onBack }) {
  const [units, setUnits] = useState([]);
  const [name, setName] = useState('');
  const [parentID, setParentID] = useState('');
  const [busy, setBusy] = useState(true);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const load = useCallback(async signal => {
    setBusy(true); setError('');
    try {
      const result = await client.organizationUnits(organization.organization_id, signal);
      setUnits(result.items || []);
    } catch (reason) {
      if (reason?.name !== 'AbortError') setError(copy.accounts.actionFailed);
    } finally { setBusy(false); }
  }, [client, copy.accounts.actionFailed, organization.organization_id]);
  useEffect(() => {
    const controller = new globalThis.AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);
  const roots = useMemo(() => buildUnitTree(units), [units]);
  const createUnit = async event => {
    event.preventDefault();
    if (!name.trim() || busy || organization.status !== 'ACTIVE') return;
    setBusy(true); setNotice(''); setError('');
    try {
      await client.createOrganizationUnit(organization.organization_id, name.trim(), parentID || null);
      setName(''); setParentID('');
      const result = await client.organizationUnits(organization.organization_id);
      setUnits(result.items || []); setNotice(copy.accounts.unitCreated);
    } catch { setError(copy.accounts.actionFailed); }
    finally { setBusy(false); }
  };
  return <section className="adm-panel adm-org-detail"><button className="adm-back" onClick={onBack}>← {copy.accounts.backToOrganizations}</button><div className="adm-org-title"><div><span className="adm-eyebrow">{copy.accounts.organizationManagement}</span><h2>{organization.name}</h2></div><Status value={organization.status} /></div>{organization.status !== 'ACTIVE' && <div className="adm-inline-message error">{copy.accounts.organizationDisabled}</div>}<form className="adm-unit-form" onSubmit={createUnit}><input value={name} onChange={event => setName(event.target.value)} placeholder={copy.accounts.unitName} maxLength={120} /><select value={parentID} onChange={event => setParentID(event.target.value)} aria-label={copy.accounts.parentUnit}><option value="">{copy.accounts.organizationRoot}</option>{units.filter(unit => unit.status === 'ACTIVE').map(unit => <option key={unit.unit_id} value={unit.unit_id}>{unit.name}</option>)}</select><button disabled={busy || organization.status !== 'ACTIVE' || !name.trim()}>{copy.accounts.createUnit}</button></form>{notice && <div className="adm-inline-message ok" role="status">{notice}</div>}{error && <div className="adm-inline-message error" role="alert">{error}</div>}<div className="adm-unit-head"><h3>{copy.accounts.unitHierarchy}</h3><span>{copy.items(units.length)}</span></div>{busy && units.length === 0 ? <div className="adm-empty">{copy.accounts.loadingUnits}</div> : roots.length === 0 ? <Empty copy={copy} /> : <div className="adm-unit-tree">{roots.map(unit => <UnitBranch key={unit.unit_id} unit={unit} copy={copy} disabled={busy || organization.status !== 'ACTIVE'} onAddChild={id => { setParentID(id); setName(''); }} />)}</div>}</section>;
}

function UnitBranch({ unit, copy, disabled, onAddChild }) {
  return <div className="adm-unit-branch"><div className="adm-unit-node"><div><strong>{unit.name}</strong><Status value={unit.status} /></div><button disabled={disabled || unit.status !== 'ACTIVE'} onClick={() => onAddChild(unit.unit_id)}>{copy.accounts.addChild}</button></div>{unit.children.length > 0 && <div className="adm-unit-children">{unit.children.map(child => <UnitBranch key={child.unit_id} unit={child} copy={copy} disabled={disabled} onAddChild={onAddChild} />)}</div>}</div>;
}

function Billing({ copy, language, data }) {
  const [tab, setTab] = useState('wallets'); const items = data[tab] || [];
  const columns = useMemo(() => ({
    wallets:[{key:'owner_id',label:copy.billing.owner,render:item => <code title={item.owner_id}>{item.owner_type} · {shortID(item.owner_id)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'available_microcredits',label:copy.billing.available,render:item => formatPoints(item.available_microcredits, language)},{key:'posted_microcredits',label:copy.billing.posted,render:item => formatPoints(item.posted_microcredits, language)},{key:'reserved_microcredits',label:copy.billing.reserved,render:item => formatPoints(item.reserved_microcredits, language)}],
    orders:[{key:'merchant_order_no',label:copy.billing.order,render:item => <code>{item.merchant_order_no}</code>},{key:'channel',label:copy.billing.channel},{key:'amount_fen',label:copy.billing.amount,render:item => formatMoney(item.amount_fen)},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'fulfillment_status',label:copy.billing.fulfillment,render:item => <Status value={item.fulfillment_status} />},{key:'updated_at',label:copy.billing.updated,render:item => formatDate(item.updated_at)}],
    adjustments:[{key:'wallet_id',label:copy.tokens.wallet,render:item => <code title={item.wallet_id}>{shortID(item.wallet_id)}</code>},{key:'amount_microcredits',label:copy.billing.amount,render:item => formatPoints(item.amount_microcredits, language)},{key:'reason',label:copy.billing.reason},{key:'requested_by',label:copy.billing.requestedBy,render:item => <code title={item.requested_by}>{shortID(item.requested_by)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />}],
    holds:[{key:'usage_request_id',label:'Usage ID',render:item => <code title={item.usage_request_id}>{shortID(item.usage_request_id)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'max_microcredits',label:copy.billing.maximum,render:item => formatPoints(item.max_microcredits, language)},{key:'actual_microcredits',label:copy.billing.actual,render:item => formatPoints(item.actual_microcredits, language)},{key:'expires_at',label:copy.billing.expires,render:item => formatDate(item.expires_at)}],
  }), [copy, language]);
  return <section className="adm-panel"><SectionHeader title={copy.billing.title} count={items.length} copy={copy} /><Tabs active={tab} onChange={setTab} items={['wallets','orders','adjustments','holds'].map(key => ({key,label:copy.billing[key]}))} /><Table columns={columns[tab]} items={items} copy={copy} /></section>;
}

function Tokens({ copy, language, data }) {
  const [tab, setTab] = useState('tokenAccounts'); const items = data[tab] || [];
  const columns = tab === 'tokenAccounts' ? [
    {key:'name',label:copy.tokens.name,render:item => <strong>{item.name}</strong>},{key:'owner_id',label:copy.tokens.owner,render:item => <code title={item.owner_id}>{item.owner_type} · {shortID(item.owner_id)}</code>},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'share_scope',label:copy.tokens.sharing},{key:'active_credential_count',label:copy.tokens.credentials},{key:'route_count',label:copy.tokens.routes},{key:'updated_at',label:copy.billing.updated,render:item => formatDate(item.updated_at)},
  ] : [
    {key:'id',label:'Usage ID',render:item => <code title={item.usage_request_id || item.id}>{shortID(item.usage_request_id || item.id)}</code>},{key:'model',label:copy.tokens.model},{key:'status',label:copy.accounts.status,render:item => <Status value={item.status} />},{key:'maximum_microcredits',label:copy.tokens.maximum,render:item => formatPoints(item.maximum_microcredits, language)},{key:'actual_microcredits',label:copy.tokens.actual,render:item => formatPoints(item.actual_microcredits, language)},{key:'error_class',label:copy.tokens.error},{key:'started_at',label:copy.tokens.started,render:item => formatDate(item.started_at)},
  ];
  return <section className="adm-panel"><SectionHeader title={copy.tokens.title} count={items.length} copy={copy} /><Tabs active={tab} onChange={setTab} items={[{key:'tokenAccounts',label:copy.tokens.accounts},{key:'usage',label:copy.tokens.usage}]} /><Table columns={columns} items={items} copy={copy} /></section>;
}

function Audit({ copy, data }) {
  const items = data.audits || []; const columns = [
    {key:'action',label:copy.audit.action,render:item => <strong>{item.action}</strong>},{key:'resource_type',label:copy.audit.resource,render:item => <span>{item.resource_type}<br /><code title={item.resource_id}>{shortID(item.resource_id)}</code></span>},{key:'actor_user_id',label:copy.audit.actor,render:item => <code title={item.actor_user_id}>{shortID(item.actor_user_id)}</code>},{key:'result',label:copy.audit.result,render:item => <Status value={item.result} />},{key:'request_id',label:copy.audit.request,render:item => <code title={item.request_id}>{shortID(item.request_id)}</code>},{key:'occurred_at',label:copy.audit.occurred,render:item => formatDate(item.occurred_at)},
  ];
  return <section className="adm-panel"><SectionHeader title={copy.audit.title} count={items.length} copy={copy} /><Table columns={columns} items={items} copy={copy} /></section>;
}
