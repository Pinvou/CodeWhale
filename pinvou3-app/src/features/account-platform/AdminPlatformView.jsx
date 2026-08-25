import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { buildUnitTree } from './admin-organization-tree.js';
import { formatPoints, MICROCREDITS_PER_POINT } from './credit-units.js';
import { adminSectionReadOnly, adminVisibleSections, authorizationVersionChanged, authorizationVersions, hasPermission } from './authorization-ui.js';
import { adminPlatformCopy } from '../../shared/admin-platform-i18n.js';
import { localeNames, supportedLocales } from '../../shared/supported-locales.js';
import './admin-platform.css';

const NAV_ICONS = {
  overview: '⌂',
  organizations: '▦',
  users: '◎',
  billing: '◇',
  tokens: '↗',
  system: '◉',
  audit: '≡',
  identity: '⚿',
};
const okStatuses = new Set(['ACTIVE', 'PAID', 'SETTLED', 'HEALTHY', 'APPROVED', 'SUCCESS']);
const warnStatuses = new Set(['PENDING', 'CREATED', 'SETTLEMENT_PENDING', 'REVIEW', 'MANUAL_REVIEW', 'PENDING_APPROVAL']);

function formatNumber(value) {
  return Number(value || 0).toLocaleString();
}
function formatMoney(value) {
  return `¥${(Number(value || 0) / 100).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}
function formatDate(value) {
  return value ? new Date(value).toLocaleString() : '—';
}
function shortID(value) {
  const text = String(value || '');
  return text.length > 16 ? `${text.slice(0, 8)}…${text.slice(-5)}` : text || '—';
}
function formatSummary(value) {
  return value && typeof value === 'object'
    ? Object.entries(value)
        .map(([key, item]) => `${key}: ${item}`)
        .join(' · ')
    : String(value || '—');
}
function userLabel(user) {
  return user?.username || user?.display_name || user?.email || shortID(user?.user_id);
}

function Status({ value }) {
  const status = String(value || '—');
  const tone = okStatuses.has(status) ? 'ok' : warnStatuses.has(status) ? 'warn' : status === '—' ? '' : 'neutral';
  return <span className={`adm-status ${tone}`}>{status}</span>;
}

function Metric({ label, value, tone = '', formatter = formatNumber }) {
  return (
    <article className={`adm-metric ${tone}`}>
      <span>{label}</span>
      <strong>{formatter(value)}</strong>
    </article>
  );
}

function Empty({ copy }) {
  return <div className="adm-empty">{copy.noData}</div>;
}

function Table({ columns, items, copy }) {
  if (!items?.length) return <Empty copy={copy} />;
  return (
    <div className="adm-table-wrap">
      <table className="adm-table">
        <thead>
          <tr>
            {columns.map((column) => (
              <th key={column.key}>{column.label}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {items.map((item, index) => (
            <tr key={item.user_id || item.organization_id || item.model_id || item.provisioning_id || item.receivable_id || item.wallet_id || item.order_id || item.operation_id || item.package_id || item.adjustment_id || item.hold_id || item.case_id || item.event_id || item.evidence_id || item.token_account_id || item.usage_request_id || item.audit_id || index}>
              {columns.map((column) => (
                <td key={column.key}>{column.render ? column.render(item) : (item[column.key] ?? '—')}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function SectionHeader({ title, count, copy, children }) {
  return (
    <header className="adm-section-head">
      <div>
        <h2>{title}</h2>
        {count !== undefined && <span>{copy.items(count)}</span>}
      </div>
      {children}
    </header>
  );
}

function Tabs({ items, active, onChange }) {
  const receivablesLabel = Object.values(adminPlatformCopy).find((candidate) => candidate.billing.wallets === items[0]?.label)?.billing.receivables;
  const visibleItems = items.some((item) => item.key === 'wallets') && !items.some((item) => item.key === 'receivables') ? [items[0], { key: 'receivables', label: receivablesLabel }, ...items.slice(1)] : items;
  return (
    <div className="adm-tabs">
      {visibleItems.map((item) => (
        <button key={item.key} className={active === item.key ? 'active' : ''} onClick={() => onChange(item.key)}>
          {item.label}
        </button>
      ))}
    </div>
  );
}

export function AdminPlatformView({ client, copy, language, onLanguageChange }) {
  const [auth, setAuth] = useState('loading');
  const [section, setSection] = useState('overview');
  const [authorization, setAuthorization] = useState(null);
  const [data, setData] = useState({});
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [updatedAt, setUpdatedAt] = useState(null);
  const authorizationRef = useRef(null);
  const authorizationRefreshRef = useRef(false);

  const load = useCallback(
    async (target, signal, granted = authorizationRef.current) => {
      setBusy(true);
      setError('');
      try {
        const allowed = (permission) => hasPermission(granted, permission);
        const read = (permission, action, fallback = { items: [] }) => (allowed(permission) ? action() : Promise.resolve(fallback));
        let next = {};
        if (target === 'overview') {
          const [account, billing, token] = await Promise.all([client.accountOverview(signal), client.billingOverview(signal), client.tokenOverview(signal)]);
          next = {
            accountOverview: account,
            billingOverview: billing,
            tokenOverview: token,
          };
        } else if (target === 'organizations') {
          const [users, organizations, roles] = await Promise.all([read('platform.users.read', () => client.users('', signal)), read('platform.organizations.read', () => client.organizations('', signal)), read('platform.roles.read', () => client.roles(signal))]);
          next = {
            users: users.items || [],
            organizations: organizations.items || [],
            roles: roles.items || [],
          };
        } else if (target === 'users') {
          const [users, roles] = await Promise.all([client.users('', signal), read('platform.roles.read', () => client.roles(signal))]);
          next = { users: users.items || [], roles: roles.items || [] };
        } else if (target === 'billing') {
          const [wallets, receivables, prices, orders, refunds, adjustments, holds, reconciliations, lateReviews, anomalies] = await Promise.all([read('platform.billing.wallets.read', () => client.wallets(signal)), read('platform.billing.wallets.read', () => client.receivables(signal)), read('platform.pricing.read', () => client.priceVersions(signal)), read('platform.billing.wallets.read', () => client.paymentOrders({}, signal)), read('platform.billing.wallets.read', () => client.refunds(signal)), read('platform.billing.wallets.read', () => client.adjustments(signal)), read('platform.usage.read', () => client.holds(signal)), read('platform.usage.read', () => client.reconciliationRuns(signal)), read('platform.billing.wallets.read', () => client.latePaymentReviews({}, signal)), read('platform.billing.wallets.read', () => client.paymentAnomalies({}, signal))]);
          next = {
            wallets: wallets.items || [],
            receivables: receivables.items || [],
            prices: prices.items || [],
            orders: orders.items || [],
            refunds: refunds.items || [],
            adjustments: adjustments.items || [],
            holds: holds.items || [],
            reconciliations: reconciliations.items || [],
            lateReviews: lateReviews.items || [],
            anomalies: anomalies.items || [],
          };
        } else if (target === 'tokens') {
          const [accounts, usage, models, provisionings] = await Promise.all([read('platform.tokens.read', () => client.tokenAccounts(signal)), read('platform.usage.read', () => client.usageRequests(signal)), read('platform.models.read', () => client.catalogModels(signal)), read('platform.tokens.read', () => client.provisionings(signal))]);
          next = {
            tokenAccounts: accounts.items || [],
            usage: usage.items || [],
            models: models.items || [],
            provisionings: provisionings.items || [],
            catalogVersion: models.catalog_version || 0,
          };
        } else if (target === 'system') {
          next = { systemStatus: await client.systemStatus(signal) };
        } else if (target === 'audit') {
          const audits = await client.audits({}, signal);
          next = { audits: audits.items || [] };
        } else if (target === 'identity') {
          const closures = await client.accountClosures(signal);
          next = { accountClosures: closures.items || [] };
        } else {
          return;
        }
        setData((current) => ({ ...current, ...next }));
        setUpdatedAt(new Date());
        setAuth('ready');
      } catch (reason) {
        if (reason?.name === 'AbortError') return;
        if (reason?.status === 401) setAuth('guest');
        else if (reason?.status === 403) setAuth('denied');
        else {
          setAuth((current) => (current === 'loading' ? 'error' : current));
          setError(copy.failed);
        }
      } finally {
        setBusy(false);
      }
    },
    [client, copy.failed],
  );

  useEffect(() => {
    const controller = new globalThis.AbortController();
    void (async () => {
      try {
        const session = await client.session(controller.signal);
        if (!session.authenticated) {
          setAuth('guest');
          return;
        }
        const granted = await client.authorization(controller.signal);
        const visible = adminVisibleSections(granted);
        if (visible.length === 0) {
          setAuth('denied');
          return;
        }
        authorizationRef.current = granted;
        setAuthorization(granted);
        const target = visible[0];
        setSection(target);
        await load(target, controller.signal, granted);
      } catch (reason) {
        if (reason?.name !== 'AbortError') {
          if (reason?.status === 401) setAuth('guest');
          else if (reason?.status === 403) setAuth('denied');
          else {
            setAuth('error');
            setError(copy.failed);
          }
        }
      }
    })();
    return () => controller.abort();
  }, [client, copy.failed, load]);

  useEffect(() => {
    if (auth !== 'ready') return undefined;
    let disposed = false;
    const refreshAuthorization = async () => {
      if (authorizationRefreshRef.current) return;
      authorizationRefreshRef.current = true;
      try {
        const next = await client.authorization();
        if (disposed) return;
        const current = authorizationRef.current;
        const permissionsChanged = JSON.stringify(current?.platform_permissions || []) !== JSON.stringify(next?.platform_permissions || []);
        if (permissionsChanged || authorizationVersionChanged(authorizationVersions(current), authorizationVersions(next))) {
          const visible = adminVisibleSections(next);
          authorizationRef.current = next;
          setAuthorization(next);
          setData({});
          if (visible.length === 0) {
            setAuth('denied');
            return;
          }
          const target = visible.includes(section) ? section : visible[0];
          setSection(target);
          await load(target, undefined, next);
        }
      } catch (reason) {
        if (!disposed && reason?.name !== 'AbortError') {
          if (reason?.status === 401) setAuth('guest');
          else setError(copy.failed);
        }
      } finally {
        authorizationRefreshRef.current = false;
      }
    };
    const unsubscribe = client.onAuthorizationVersion?.(() => {
      void refreshAuthorization();
    });
    const onFocus = () => void refreshAuthorization();
    globalThis.addEventListener?.('focus', onFocus);
    return () => {
      disposed = true;
      unsubscribe?.();
      globalThis.removeEventListener?.('focus', onFocus);
    };
  }, [auth, client, copy.failed, load, section]);

  const navigate = (key) => {
    if (!adminVisibleSections(authorization).includes(key)) return;
    setSection(key);
    if (!sectionHasData(key, data)) load(key, undefined, authorization);
  };
  const logout = async () => {
    await client.logout();
    window.location.assign('/admin.html');
  };

  if (auth === 'loading') return <StatePage copy={copy} kind="loading" />;
  if (auth === 'guest') return <StatePage copy={copy} kind="guest" />;
  if (auth === 'denied') return <StatePage copy={copy} kind="denied" />;
  if (auth === 'error') return <StatePage copy={copy} kind="error" onRetry={() => window.location.reload()} />;

  const visibleSections = adminVisibleSections(authorization);
  const platformPermissions = authorization?.platform_permissions || [];
  const sectionIsReadOnly = adminSectionReadOnly(authorization, section);

  return (
    <div className="adm-shell">
      <aside className="adm-sidebar">
        <div className="adm-brand">
          <div className="adm-mark">P</div>
          <div>
            <strong>{copy.title}</strong>
            <span>{copy.product}</span>
          </div>
        </div>
        <nav>
          {visibleSections.map((key) => (
            <button key={key} className={section === key ? 'active' : ''} onClick={() => navigate(key)}>
              <span>{NAV_ICONS[key]}</span>
              {copy.nav[key]}
            </button>
          ))}
        </nav>
        <div className="adm-sidebar-foot">
          <span>{sectionIsReadOnly ? copy.permissionReadOnly : copy.permissionControlled}</span>
          <select aria-label={copy.language} value={language} onChange={(event) => onLanguageChange(event.target.value)}>
            {supportedLocales.map((locale) => (
              <option key={locale} value={locale}>
                {localeNames[locale]}
              </option>
            ))}
          </select>
        </div>
      </aside>
      <main className="adm-main">
        <header className="adm-topbar">
          <div>
            <span className="adm-eyebrow">{copy.product}</span>
            <h1>{copy.nav[section]}</h1>
          </div>
          <div className="adm-actions">
            {updatedAt && section !== 'identity' && (
              <small>
                {copy.updated} {updatedAt.toLocaleTimeString()}
              </small>
            )}
            {section !== 'identity' && (
              <button onClick={() => load(section, undefined, authorization)} disabled={busy}>
                {copy.refresh}
              </button>
            )}
            <div className="adm-admin">
              <span>PA</span>
              <div>
                <strong>{copy.administrator}</strong>
                <button onClick={logout}>{copy.logout}</button>
              </div>
            </div>
          </div>
        </header>
        {error && (
          <div role="alert" className="adm-alert">
            {error}
          </div>
        )}
        {busy && <div className="adm-progress" />}
        {sectionIsReadOnly && (
          <div className="adm-inline-message" role="status">
            {copy.permissionReadOnlyReason}
          </div>
        )}
        {section === 'overview' && <Overview copy={copy} language={language} data={data} />}
        {section === 'organizations' && <AccountDirectory section="organizations" copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} />}
        {section === 'users' && <AccountDirectory section="users" copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} />}
        {section === 'billing' && <Billing copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} />}
        {section === 'tokens' && <Tokens copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} />}
        {section === 'system' && <SystemStatus copy={copy} data={data} />}
        {section === 'audit' && <Audit copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} />}
        {section === 'identity' && <IdentitySecurity copy={copy} closures={data.accountClosures || []} client={client} setData={setData} permissions={platformPermissions} />}
      </main>
    </div>
  );
}

function sectionHasData(section, data) {
  return section === 'identity' ? data.accountClosures : section === 'overview' ? data.accountOverview : section === 'organizations' ? data.organizations : section === 'users' ? data.users : section === 'billing' ? data.wallets : section === 'tokens' ? data.tokenAccounts : section === 'system' ? data.systemStatus : data.audits;
}

function StatePage({ copy, kind, onRetry }) {
  const loading = kind === 'loading';
  const title = kind === 'guest' ? copy.loginTitle : kind === 'denied' ? copy.deniedTitle : kind === 'error' ? copy.failed : copy.loading;
  const body = kind === 'guest' ? copy.loginIntro : kind === 'denied' ? copy.denied : '';
  return (
    <main className="adm-state">
      <div className="adm-state-mark">P</div>
      <span className="adm-eyebrow">{copy.product}</span>
      <h1>{title}</h1>
      {body && <p>{body}</p>}
      {loading && <div className="adm-spinner" />}
      {kind === 'guest' && <a href="/auth/login?return_to=/admin.html">{copy.login}</a>}
      {kind === 'error' && <button onClick={onRetry}>{copy.retry}</button>}
    </main>
  );
}

function Overview({ copy, language, data }) {
  const a = data.accountOverview || {},
    b = data.billingOverview || {},
    t = data.tokenOverview || {};
  const alerts = [
    [copy.overview.disabledUsers, a.disabled_users],
    [copy.overview.invitations, a.pending_invitations],
    [copy.overview.paymentReview, b.review_payment_orders],
    [copy.overview.pendingAdjustments, b.pending_adjustments],
    [copy.overview.usageReview, t.review_usage_requests],
  ];
  return (
    <section>
      <div className="adm-hero">
        <div>
          <h2>{copy.overview.title}</h2>
          <p>{copy.overview.lead}</p>
        </div>
        <Status value="ACTIVE" />
      </div>
      <div className="adm-metrics">
        <Metric label={copy.overview.users} value={a.users} />
        <Metric label={copy.overview.organizations} value={a.organizations} />
        <Metric label={copy.overview.available} value={Number(b.posted_microcredits || 0) - Number(b.reserved_microcredits || 0)} tone="accent" formatter={(value) => formatPoints(value, language)} />
        <Metric label={copy.overview.reserved} value={b.reserved_microcredits} formatter={(value) => formatPoints(value, language)} />
        <Metric label={copy.overview.tokenAccounts} value={t.token_accounts} />
        <Metric label={copy.overview.usageRequests} value={t.usage_requests} />
      </div>
      <div className="adm-overview-grid">
        <article className="adm-panel">
          <SectionHeader title={copy.overview.attention} copy={copy} />
          <div className="adm-attention">
            {alerts.map(([label, value]) => (
              <div key={label}>
                <span>{label}</span>
                <strong className={Number(value) ? 'has-value' : ''}>{formatNumber(value)}</strong>
              </div>
            ))}
          </div>
        </article>
        <article className="adm-panel adm-health">
          <span>{copy.overview.healthyRoutes}</span>
          <strong>{formatNumber(t.healthy_routes)}</strong>
          <small>
            / {formatNumber(t.token_accounts)} {copy.overview.tokenAccounts}
          </small>
          <div>
            <i
              style={{
                width: `${Math.min(100, Number(t.token_accounts) ? (Number(t.healthy_routes) / Number(t.token_accounts)) * 100 : 0)}%`,
              }}
            />
          </div>
        </article>
      </div>
    </section>
  );
}

function AccountDirectory({ section, copy, data, client, setData, permissions }) {
  const [search, setSearch] = useState('');
  const [actionKey, setActionKey] = useState('');
  const [notice, setNotice] = useState('');
  const [actionError, setActionError] = useState('');
  const [selectedOrganization, setSelectedOrganization] = useState(null);
  const [selectedUser, setSelectedUser] = useState(null);
  const [organizationName, setOrganizationName] = useState('');
  const [ownerUserID, setOwnerUserID] = useState('');
  const isUserSection = section === 'users';
  const permissionSet = new Set(permissions || []);
  const canManageUserStatus = permissionSet.has('platform.users.status.manage');
  const canCreateOrganization = permissionSet.has('platform.organizations.create');
  const canManageOrganizationStatus = permissionSet.has('platform.organizations.status.manage');
  const canDeleteOrganization = permissionSet.has('platform.organizations.delete');
  const items = isUserSection ? data.users || [] : data.organizations || [];
  const activeUsers = useMemo(() => (data.users || []).filter((user) => user.status === 'ACTIVE'), [data.users]);
  useEffect(() => {
    if (!ownerUserID && activeUsers[0]) setOwnerUserID(activeUsers[0].user_id);
  }, [activeUsers, ownerUserID]);
  const reload = async () => {
    const compactCode = search.trim().replaceAll('-', '');
    const userCodeLookup = isUserSection && /^USR/i.test(compactCode);
    const result = userCodeLookup ? { items: [await client.userByCode(search.trim())] } : isUserSection ? await client.users(search) : await client.organizations(search);
    setData((current) => ({
      ...current,
      [section]: result.items || [],
      accountOverview: undefined,
    }));
  };
  const perform = async (item, action) => {
    const isUser = isUserSection;
    const id = isUser ? item.user_id : item.organization_id;
    const label = isUser ? userLabel(item) : item.name;
    if (action === 'delete' && !window.confirm(copy.accounts.confirmOrganization(label))) return;
    setActionKey(`${section}:${id}:${action}`);
    setNotice('');
    setActionError('');
    try {
      if (isUser) {
        await client.setUserStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
      } else if (action === 'delete') await client.deleteOrganization(id);
      else await client.setOrganizationStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
      await reload();
      setNotice(copy.accounts.updated);
    } catch (reason) {
      setActionError(reason?.code === 'LAST_OWNER_REQUIRED' ? copy.accounts.ownerRequired : reason?.code === 'ACCOUNT_CONFLICT' ? copy.accounts.conflict : copy.accounts.actionFailed);
    } finally {
      setActionKey('');
    }
  };
  const actionButtons = (item, isUser) => {
    const id = isUser ? item.user_id : item.organization_id;
    const prefix = `${isUser ? 'users' : 'organizations'}:${id}:`;
    const buttons = [];
    const canManageStatus = isUser ? canManageUserStatus : canManageOrganizationStatus;
    if (canManageStatus && item.status === 'DISABLED')
      buttons.push(
        <button key="enable" disabled={Boolean(actionKey)} onClick={() => perform(item, 'enable')}>
          {copy.accounts.enable}
        </button>,
      );
    if (canManageStatus && (item.status === 'ACTIVE' || (isUser && item.status === 'PROVISIONING')))
      buttons.push(
        <button key="disable" disabled={Boolean(actionKey)} onClick={() => perform(item, 'disable')}>
          {copy.accounts.disable}
        </button>,
      );
    if (!isUser && canDeleteOrganization)
      buttons.push(
        <button key="delete" className="danger" disabled={Boolean(actionKey)} onClick={() => perform(item, 'delete')}>
          {actionKey === `${prefix}delete` ? '…' : copy.accounts.delete}
        </button>,
      );
    return <div className="adm-row-actions">{buttons}</div>;
  };
  const columns = isUserSection
    ? [
        {
          key: 'username',
          label: copy.username,
          render: (item) => (
            <button className="adm-name-link" onClick={() => setSelectedUser(item)}>
              {userLabel(item)}
            </button>
          ),
        },
        {
          key: 'user_code',
          label: copy.accounts.userCode,
          render: (item) => <code>{item.user_code || '—'}</code>,
        },
        { key: 'display_name', label: copy.accounts.displayName },
        { key: 'email', label: copy.accounts.email },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        { key: 'organization_count', label: copy.accounts.memberships },
        {
          key: 'last_seen_at',
          label: copy.accounts.lastSeen,
          render: (item) => formatDate(item.last_seen_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => actionButtons(item, true),
        },
      ]
    : [
        {
          key: 'name',
          label: copy.accounts.organization,
          render: (item) => (
            <button className="adm-name-link" onClick={() => setSelectedOrganization(item)}>
              {item.name}
            </button>
          ),
        },
        {
          key: 'organization_id',
          label: 'ID',
          render: (item) => <code title={item.organization_id}>{shortID(item.organization_id)}</code>,
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        { key: 'member_count', label: copy.accounts.members },
        {
          key: 'created_at',
          label: copy.accounts.created,
          render: (item) => formatDate(item.created_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => actionButtons(item, false),
        },
      ];
  const submit = async (event) => {
    event.preventDefault();
    setActionError('');
    try {
      await reload();
    } catch {
      setActionError(copy.accounts.lookupUnavailable || copy.accounts.actionFailed);
    }
  };
  const createOrganization = async (event) => {
    event.preventDefault();
    if (!organizationName.trim() || !ownerUserID || actionKey) return;
    setActionKey('organization:create');
    setNotice('');
    setActionError('');
    try {
      await client.createOrganization(organizationName.trim(), ownerUserID);
      setOrganizationName('');
      await reload();
      setNotice(copy.accounts.organizationCreated);
    } catch {
      setActionError(copy.accounts.actionFailed);
    } finally {
      setActionKey('');
    }
  };
  if (selectedUser) return <UserMembershipDetail copy={copy} user={selectedUser} roles={data.roles || []} client={client} permissions={permissions} onBack={() => setSelectedUser(null)} />;
  if (selectedOrganization)
    return (
      <OrganizationDetail
        copy={copy}
        organization={selectedOrganization}
        users={data.users || []}
        roles={data.roles || []}
        client={client}
        permissions={permissions}
        onBack={() => setSelectedOrganization(null)}
        onRenamed={(name) => {
          setSelectedOrganization((current) => ({ ...current, name }));
          setData((current) => ({
            ...current,
            organizations: (current.organizations || []).map((item) => (item.organization_id === selectedOrganization.organization_id ? { ...item, name } : item)),
          }));
        }}
      />
    );
  return (
    <section className="adm-panel">
      <SectionHeader title={copy.accounts[section]} count={items.length} copy={copy}>
        <form className="adm-search" onSubmit={submit}>
          <input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={isUserSection ? copy.search : copy.accounts.organizationSearch} />
          <button>{copy.searchAction}</button>
        </form>
      </SectionHeader>
      {!isUserSection && canCreateOrganization && (
        <form className="adm-create-row" onSubmit={createOrganization}>
          <input value={organizationName} onChange={(event) => setOrganizationName(event.target.value)} placeholder={copy.accounts.organizationNamePlaceholder} maxLength={120} />
          <select value={ownerUserID} onChange={(event) => setOwnerUserID(event.target.value)} aria-label={copy.accounts.initialOwner}>
            <option value="">{copy.accounts.chooseOwner}</option>
            {activeUsers.map((user) => (
              <option key={user.user_id} value={user.user_id}>
                {userLabel(user)}
              </option>
            ))}
          </select>
          <button disabled={!organizationName.trim() || !ownerUserID || Boolean(actionKey)}>{copy.accounts.createOrganization}</button>
          {activeUsers.length === 0 && <small>{copy.accounts.noActiveOwner}</small>}
        </form>
      )}
      {notice && (
        <div className="adm-inline-message ok" role="status">
          {notice}
        </div>
      )}
      {actionError && (
        <div className="adm-inline-message error" role="alert">
          {actionError}
        </div>
      )}
      <Table columns={columns} items={items} copy={copy} />
      <RoleDefinitions copy={copy} roles={data.roles || []} />
    </section>
  );
}

function RoleDefinitions({ copy, roles }) {
  if (!roles.length) return null;
  return (
    <details className="adm-role-definitions">
      <summary>{copy.roles.definitions}</summary>
      <div className="adm-role-grid">
        {roles.map((role) => (
          <article key={role.code}>
            <strong>{role.code}</strong>
            <span>{role.scope === 'UNIT' ? copy.roles.unitScope : copy.roles.organizationScope}</span>
            <p>{role.description}</p>
          </article>
        ))}
      </div>
      <small>
        {copy.roles.ownerRoleHint} {copy.roles.baseRoleHint}
      </small>
    </details>
  );
}

function UserMembershipDetail({ copy, user, roles, client, onBack }) {
  const [items, setItems] = useState([]);
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState('');
  useEffect(() => {
    const controller = new globalThis.AbortController();
    client
      .userMemberships(user.user_id, controller.signal)
      .then((result) => setItems(result.items || []))
      .catch((reason) => {
        if (reason?.name !== 'AbortError') setError(copy.accounts.actionFailed);
      })
      .finally(() => setBusy(false));
    return () => controller.abort();
  }, [client, copy.accounts.actionFailed, user.user_id]);
  return (
    <section className="adm-panel adm-org-detail">
      <button className="adm-back" onClick={onBack}>
        ← {copy.roles.backToUsers}
      </button>
      <div className="adm-org-title">
        <div>
          <span className="adm-eyebrow">{copy.roles.userMemberships}</span>
          <h2>{userLabel(user)}</h2>
          <code>
            {copy.accounts.userCode}: {user.user_code || '—'}
          </code>
        </div>
        <Status value={user.status} />
      </div>
      {error && <div className="adm-inline-message error">{error}</div>}
      {busy ? (
        <div className="adm-empty">{copy.loading}</div>
      ) : items.length ? (
        <div className="adm-membership-list">
          {items.map((item) => (
            <article key={item.organization_id}>
              <div>
                <strong>{item.organization_name}</strong>
                <Status value={item.membership_status} />
              </div>
              <div className="adm-role-badges">
                {item.roles.map((role) => (
                  <span key={role}>{role}</span>
                ))}
              </div>
              {item.unit_admin_unit_ids?.length > 0 && (
                <small>
                  {copy.roles.unitAdminNodes}: {item.unit_admin_unit_ids.map(shortID).join(' · ')}
                </small>
              )}
            </article>
          ))}
        </div>
      ) : (
        <Empty copy={{ noData: copy.roles.noMemberships }} />
      )}
      <RoleDefinitions copy={copy} roles={roles} />
    </section>
  );
}

function OrganizationDetail({ copy, organization, users, roles, client, permissions, onBack, onRenamed }) {
  const [units, setUnits] = useState([]);
  const [members, setMembers] = useState([]);
  const [name, setName] = useState('');
  const [organizationName, setOrganizationName] = useState(organization.name);
  const [editingOrganizationName, setEditingOrganizationName] = useState(false);
  const [parentID, setParentID] = useState('');
  const [newOwnerUserID, setNewOwnerUserID] = useState('');
  const [newMemberUserID, setNewMemberUserID] = useState('');
  const [busy, setBusy] = useState(true);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const permissionSet = new Set(permissions || []);
  const canManageOrganization = permissionSet.has('platform.organizations.status.manage');
  const canTransferOwnership = permissionSet.has('platform.organizations.owner.transfer');
  const load = useCallback(
    async (signal) => {
      setBusy(true);
      setError('');
      try {
        const [unitResult, memberResult] = await Promise.all([client.organizationUnits(organization.organization_id, signal), client.organizationMembers(organization.organization_id, signal)]);
        setUnits(unitResult.items || []);
        setMembers(memberResult.items || []);
      } catch (reason) {
        if (reason?.name !== 'AbortError') setError(copy.accounts.actionFailed);
      } finally {
        setBusy(false);
      }
    },
    [client, copy.accounts.actionFailed, organization.organization_id],
  );
  useEffect(() => {
    const controller = new globalThis.AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);
  useEffect(() => setOrganizationName(organization.name), [organization.name]);
  const roots = useMemo(() => buildUnitTree(units), [units]);
  const owners = useMemo(() => members.filter((member) => (member.roles || []).includes('OWNER')), [members]);
  const transferCandidates = useMemo(() => members.filter((member) => member.status === 'ACTIVE' && !(member.roles || []).includes('OWNER')), [members]);
  const addCandidates = useMemo(() => users.filter((user) => user.status === 'ACTIVE' && !members.some((member) => member.user_id === user.user_id)), [members, users]);
  const memberLabel = (member) => member.display_name || member.email || shortID(member.user_id);
  const refreshUnits = async () => {
    const result = await client.organizationUnits(organization.organization_id);
    setUnits(result.items || []);
  };
  const refreshMembers = async () => {
    const result = await client.organizationMembers(organization.organization_id);
    setMembers(result.items || []);
  };
  const renameOrganization = async (event) => {
    event.preventDefault();
    const nextName = organizationName.trim();
    if (!nextName || nextName === organization.name || busy) return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.renameOrganization(organization.organization_id, nextName);
      onRenamed(nextName);
      setEditingOrganizationName(false);
      setNotice(copy.accounts.organizationRenamed);
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const createUnit = async (event) => {
    event.preventDefault();
    if (!name.trim() || busy || organization.status !== 'ACTIVE') return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.createOrganizationUnit(organization.organization_id, name.trim(), parentID || null);
      setName('');
      setParentID('');
      await refreshUnits();
      setNotice(copy.accounts.unitCreated);
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const transferOwnership = async (event) => {
    event.preventDefault();
    const target = transferCandidates.find((member) => member.user_id === newOwnerUserID);
    if (!target || busy || organization.status !== 'ACTIVE' || !window.confirm(copy.accounts.confirmOwnership(memberLabel(target)))) return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.transferOrganizationOwnership(organization.organization_id, target.user_id);
      await refreshMembers();
      setNewOwnerUserID('');
      setNotice(copy.accounts.ownershipTransferred);
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const addMember = async (event) => {
    event.preventDefault();
    if (!newMemberUserID || busy || organization.status !== 'ACTIVE') return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.addOrganizationMember(organization.organization_id, newMemberUserID, ['MEMBER'], []);
      await refreshMembers();
      setNewMemberUserID('');
      setNotice(copy.roles.memberAdded);
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const saveMemberRoles = async (member, selectedRoles, unitIDs) => {
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.replaceOrganizationMemberRoles(organization.organization_id, member.user_id, selectedRoles, unitIDs);
      await refreshMembers();
      setNotice(copy.roles.rolesUpdated);
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const removeMember = async (member) => {
    if (!window.confirm(copy.roles.confirmRemove(memberLabel(member)))) return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.removeOrganizationMember(organization.organization_id, member.user_id);
      await refreshMembers();
      setNotice(copy.roles.memberRemoved);
    } catch (reason) {
      setError(reason?.code === 'LAST_OWNER_REQUIRED' ? copy.accounts.ownerRequired : copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const renameUnit = async (unit, nextName) => {
    const trimmed = nextName.trim();
    if (!trimmed || trimmed === unit.name || busy) return trimmed === unit.name;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.renameOrganizationUnit(organization.organization_id, unit.unit_id, trimmed);
      await refreshUnits();
      setNotice(copy.accounts.unitRenamed);
      return true;
    } catch {
      setError(copy.accounts.actionFailed);
      return false;
    } finally {
      setBusy(false);
    }
  };
  const deleteUnit = async (unit) => {
    if (busy || !window.confirm(copy.accounts.confirmUnit(unit.name))) return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.deleteOrganizationUnit(organization.organization_id, unit.unit_id);
      await refreshUnits();
      setNotice(copy.accounts.unitDeleted);
    } catch (reason) {
      setError(reason?.code === 'ORG_UNIT_HIERARCHY_INVALID' ? copy.accounts.deleteUnitBlocked : copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="adm-panel adm-org-detail">
      <button className="adm-back" onClick={onBack}>
        ← {copy.accounts.backToOrganizations}
      </button>
      <div className="adm-org-title">
        <div>
          <span className="adm-eyebrow">{copy.accounts.organizationManagement}</span>
          {canManageOrganization && editingOrganizationName ? (
            <form className="adm-org-rename" onSubmit={renameOrganization}>
              <input value={organizationName} onChange={(event) => setOrganizationName(event.target.value)} maxLength={120} aria-label={copy.accounts.organizationName} autoFocus />
              <button disabled={busy || !organizationName.trim() || organizationName.trim() === organization.name}>{copy.accounts.save}</button>
              <button
                type="button"
                className="secondary"
                disabled={busy}
                onClick={() => {
                  setOrganizationName(organization.name);
                  setEditingOrganizationName(false);
                }}
              >
                {copy.accounts.cancel}
              </button>
            </form>
          ) : (
            <div className="adm-org-name-display">
              <h2>{organization.name}</h2>
              {canManageOrganization && (
                <button type="button" disabled={busy} onClick={() => setEditingOrganizationName(true)}>
                  {copy.accounts.rename}
                </button>
              )}
            </div>
          )}
        </div>
        <Status value={organization.status} />
      </div>
      {organization.status !== 'ACTIVE' && <div className="adm-inline-message error">{copy.accounts.organizationDisabled}</div>}
      <div className="adm-owner-card">
        <div>
          <span>{copy.accounts.organizationOwner}</span>
          <strong>{owners.length ? owners.map(memberLabel).join(' · ') : '—'}</strong>
        </div>
        {canTransferOwnership && (
          <form onSubmit={transferOwnership}>
            <select value={newOwnerUserID} onChange={(event) => setNewOwnerUserID(event.target.value)} aria-label={copy.accounts.chooseNewOwner}>
              <option value="">{copy.accounts.chooseNewOwner}</option>
              {transferCandidates.map((member) => (
                <option key={member.user_id} value={member.user_id}>
                  {memberLabel(member)}
                </option>
              ))}
            </select>
            <button disabled={busy || organization.status !== 'ACTIVE' || !newOwnerUserID}>{copy.accounts.transferOwner}</button>
          </form>
        )}
        {!busy && transferCandidates.length === 0 && <small>{copy.accounts.noTransferCandidate}</small>}
      </div>
      <div className="adm-unit-head">
        <h3>{copy.roles.organizationMembers}</h3>
        <span>{copy.items(members.length)}</span>
      </div>
      {canManageOrganization && (
        <form className="adm-create-row" onSubmit={addMember}>
          <select value={newMemberUserID} onChange={(event) => setNewMemberUserID(event.target.value)} aria-label={copy.roles.chooseMember}>
            <option value="">{copy.roles.chooseMember}</option>
            {addCandidates.map((user) => (
              <option key={user.user_id} value={user.user_id}>
                {user.display_name || user.email || shortID(user.user_id)}
              </option>
            ))}
          </select>
          <button disabled={busy || !newMemberUserID}>{copy.roles.addMember}</button>
        </form>
      )}
      <div className="adm-member-access-list">
        {members.map((member) => (
          <MemberAccessEditor key={member.user_id} copy={copy} member={member} units={units} roles={roles} disabled={!canManageOrganization || busy || organization.status !== 'ACTIVE'} onSave={saveMemberRoles} onRemove={removeMember} />
        ))}
      </div>
      <RoleDefinitions copy={copy} roles={roles} />
      {canManageOrganization && (
        <form className="adm-unit-form" onSubmit={createUnit}>
          <input value={name} onChange={(event) => setName(event.target.value)} placeholder={copy.accounts.unitName} maxLength={120} />
          <select value={parentID} onChange={(event) => setParentID(event.target.value)} aria-label={copy.accounts.parentUnit}>
            <option value="">{copy.accounts.organizationRoot}</option>
            {units
              .filter((unit) => unit.status === 'ACTIVE')
              .map((unit) => (
                <option key={unit.unit_id} value={unit.unit_id}>
                  {unit.name}
                </option>
              ))}
          </select>
          <button disabled={busy || organization.status !== 'ACTIVE' || !name.trim()}>{copy.accounts.createUnit}</button>
        </form>
      )}
      {notice && (
        <div className="adm-inline-message ok" role="status">
          {notice}
        </div>
      )}
      {error && (
        <div className="adm-inline-message error" role="alert">
          {error}
        </div>
      )}
      <div className="adm-unit-head">
        <h3>{copy.accounts.unitHierarchy}</h3>
        <span>{copy.items(units.length)}</span>
      </div>
      {busy && units.length === 0 ? (
        <div className="adm-empty">{copy.accounts.loadingUnits}</div>
      ) : roots.length === 0 ? (
        <Empty copy={copy} />
      ) : (
        <div className="adm-unit-tree">
          {roots.map((unit) => (
            <UnitBranch
              key={unit.unit_id}
              unit={unit}
              copy={copy}
              disabled={busy || !canManageOrganization}
              addDisabled={!canManageOrganization || organization.status !== 'ACTIVE'}
              onAddChild={(id) => {
                setParentID(id);
                setName('');
              }}
              onRename={renameUnit}
              onDelete={deleteUnit}
            />
          ))}
        </div>
      )}
    </section>
  );
}

function MemberAccessEditor({ copy, member, units, roles, disabled, onSave, onRemove }) {
  const editableRoles = roles.filter((role) => role.assignable && role.scope === 'ORGANIZATION').map((role) => role.code);
  const [selected, setSelected] = useState(() => (member.roles || []).filter((role) => editableRoles.includes(role)));
  const [unitIDs, setUnitIDs] = useState(() => member.unit_admin_unit_ids || []);
  useEffect(() => {
    setSelected((member.roles || []).filter((role) => editableRoles.includes(role)));
    setUnitIDs(member.unit_admin_unit_ids || []);
  }, [member, editableRoles.join('|')]);
  const toggle = (value, values, setter) => setter(values.includes(value) ? values.filter((item) => item !== value) : [...values, value]);
  return (
    <article className="adm-member-access">
      <header>
        <div>
          <strong>{member.display_name || member.email || shortID(member.user_id)}</strong>
          <small>{member.email}</small>
        </div>
        <Status value={member.status} />
      </header>
      <div className="adm-role-options">
        {editableRoles.map((role) => (
          <label key={role}>
            <input type="checkbox" checked={selected.includes(role)} disabled={disabled} onChange={() => toggle(role, selected, setSelected)} />
            {role}
          </label>
        ))}
        <label>
          <input type="checkbox" checked disabled readOnly />
          MEMBER
        </label>
        {(member.roles || []).includes('OWNER') && (
          <label>
            <input type="checkbox" checked disabled readOnly />
            OWNER
          </label>
        )}
      </div>
      <div className="adm-unit-role-options">
        <span>{copy.roles.unitAdminNodes}</span>
        {units
          .filter((unit) => unit.status === 'ACTIVE')
          .map((unit) => (
            <label key={unit.unit_id}>
              <input type="checkbox" checked={unitIDs.includes(unit.unit_id)} disabled={disabled} onChange={() => toggle(unit.unit_id, unitIDs, setUnitIDs)} />
              {unit.name}
            </label>
          ))}
      </div>
      <footer>
        <button disabled={disabled} onClick={() => onSave(member, selected, unitIDs)}>
          {copy.roles.saveRoles}
        </button>
        <button className="danger" disabled={disabled || (member.roles || []).includes('OWNER')} onClick={() => onRemove(member)}>
          {copy.roles.removeMember}
        </button>
      </footer>
    </article>
  );
}

function UnitBranch({ unit, copy, disabled, addDisabled, onAddChild, onRename, onDelete }) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(unit.name);
  useEffect(() => setName(unit.name), [unit.name]);
  const submit = async (event) => {
    event.preventDefault();
    if (await onRename(unit, name)) setEditing(false);
  };
  return (
    <div className="adm-unit-branch">
      <div className="adm-unit-node">
        {editing ? (
          <form className="adm-unit-rename" onSubmit={submit}>
            <input value={name} onChange={(event) => setName(event.target.value)} maxLength={120} aria-label={copy.accounts.unitName} autoFocus />
            <button disabled={disabled || !name.trim()}>{copy.accounts.save}</button>
            <button
              type="button"
              onClick={() => {
                setName(unit.name);
                setEditing(false);
              }}
            >
              {copy.accounts.cancel}
            </button>
          </form>
        ) : (
          <div>
            <strong>{unit.name}</strong>
            <Status value={unit.status} />
          </div>
        )}
        <div className="adm-unit-actions">
          <button type="button" disabled={disabled || addDisabled || unit.status !== 'ACTIVE'} onClick={() => onAddChild(unit.unit_id)}>
            {copy.accounts.addChild}
          </button>
          <button type="button" disabled={disabled || unit.status !== 'ACTIVE'} onClick={() => setEditing(true)}>
            {copy.accounts.rename}
          </button>
          <button type="button" className="danger" title={unit.children.length ? copy.accounts.deleteUnitBlocked : undefined} disabled={disabled || unit.status !== 'ACTIVE' || unit.children.length > 0} onClick={() => onDelete(unit)}>
            {copy.accounts.delete}
          </button>
        </div>
      </div>
      {unit.children.length > 0 && (
        <div className="adm-unit-children">
          {unit.children.map((child) => (
            <UnitBranch key={child.unit_id} unit={child} copy={copy} disabled={disabled} addDisabled={addDisabled} onAddChild={onAddChild} onRename={onRename} onDelete={onDelete} />
          ))}
        </div>
      )}
    </div>
  );
}

function Billing({ copy, language, data, client, setData, permissions }) {
  const [tab, setTab] = useState('wallets');
  const items = data[tab] || [];
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [ledger, setLedger] = useState(null);
  const [cases, setCases] = useState(null);
  const [orderDetail, setOrderDetail] = useState(null);
  const [receivableDetail, setReceivableDetail] = useState(null);
  const [holdDetail, setHoldDetail] = useState(null);
  const [lateReviewDetail, setLateReviewDetail] = useState(null);
  const [anomalyDetail, setAnomalyDetail] = useState(null);
  const [price, setPrice] = useState({
    model: '',
    input: '',
    output: '',
    cached: '',
  });
  const [reconciliation, setReconciliation] = useState({
    channel: 'ALIPAY',
    businessDate: new Date().toISOString().slice(0, 10),
  });
  const [orderFilter, setOrderFilter] = useState({ q: '', status: '' });
  const [lateFilter, setLateFilter] = useState({
    status: '',
    provider_id: '',
    owner_id: '',
    entered_from: '',
    entered_to: '',
    next_review_before: '',
    overdue: false,
  });
  const [anomalyFilter, setAnomalyFilter] = useState({
    anomaly_type: '',
    status: 'OPEN',
    severity: '',
  });
  const permissionSet = new Set(permissions || []);
  const canReadWallets = permissionSet.has('platform.billing.wallets.read');
  const canReadPricing = permissionSet.has('platform.pricing.read');
  const canReadUsage = permissionSet.has('platform.usage.read');
  const canSyncOrders = permissionSet.has('platform.billing.orders.sync');
  const canRequestRepair = permissionSet.has('platform.billing.repairs.request');
  const canApproveAdjustment = permissionSet.has('platform.billing.adjustments.approve');
  const canReleaseHold = permissionSet.has('platform.billing.holds.release');
  const canManagePricing = permissionSet.has('platform.pricing.manage');
  const canReconcile = permissionSet.has('platform.usage.reconcile.retry');
  const visibleTabs = [...(canReadWallets ? ['wallets', 'orders', 'refunds', 'adjustments', 'lateReviews', 'anomalies'] : []), ...(canReadPricing ? ['prices'] : []), ...(canReadUsage ? ['holds', 'reconciliations'] : [])];
  useEffect(() => {
    if (!visibleTabs.includes(tab)) setTab(visibleTabs[0] || 'wallets');
  }, [tab, visibleTabs.join('|')]);
  const paymentCapabilities = data.billingOverview?.payment_capabilities || {};
  const refundSupported = paymentCapabilities.refund_supported === true;
  const reconciliationSupported = paymentCapabilities.bill_supported === true;
  const reconciliationImportSupported = paymentCapabilities.bill_file_import_supported === true;
  const repairSupported = paymentCapabilities.manual_repair_supported === true;
  const hasLimitedPaymentCapabilities = paymentCapabilities.provider_id && (!paymentCapabilities.refund_supported || !paymentCapabilities.close_supported || !paymentCapabilities.notification_supported || paymentCapabilities.reconciliation_mode !== 'API');
  const reload = async () => {
    const [wallets, receivables, prices, orders, refunds, adjustments, holds, reconciliations, lateReviews, anomalies] = await Promise.all([canReadWallets ? client.wallets() : Promise.resolve({ items: [] }), canReadWallets ? client.receivables() : Promise.resolve({ items: [] }), canReadPricing ? client.priceVersions() : Promise.resolve({ items: [] }), canReadWallets ? client.paymentOrders(orderFilter) : Promise.resolve({ items: [] }), canReadWallets ? client.refunds() : Promise.resolve({ items: [] }), canReadWallets ? client.adjustments() : Promise.resolve({ items: [] }), canReadUsage ? client.holds() : Promise.resolve({ items: [] }), canReadUsage ? client.reconciliationRuns() : Promise.resolve({ items: [] }), canReadWallets ? client.latePaymentReviews(lateFilter) : Promise.resolve({ items: [] }), canReadWallets ? client.paymentAnomalies(anomalyFilter) : Promise.resolve({ items: [] })]);
    setData((current) => ({
      ...current,
      wallets: wallets.items || [],
      receivables: receivables.items || [],
      prices: prices.items || [],
      orders: orders.items || [],
      refunds: refunds.items || [],
      adjustments: adjustments.items || [],
      holds: holds.items || [],
      reconciliations: reconciliations.items || [],
      lateReviews: lateReviews.items || [],
      anomalies: anomalies.items || [],
    }));
  };
  const perform = async (action) => {
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await action();
      await reload();
      setNotice(copy.billing.completed);
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const promptReason = (label) => String(window.prompt(label) || '').trim();
  const createPrice = (event) => {
    event.preventDefault();
    const input = {
      model: price.model.trim(),
      input_per_million: Math.round(Number(price.input) * MICROCREDITS_PER_POINT),
      output_per_million: Math.round(Number(price.output) * MICROCREDITS_PER_POINT),
      cached_input_per_million: Math.round(Number(price.cached) * MICROCREDITS_PER_POINT),
    };
    if (
      !input.model ||
      Object.values(input)
        .slice(1)
        .some((value) => !Number.isFinite(value) || value < 0)
    )
      return;
    void perform(async () => {
      await client.createPriceVersion(input);
      setPrice({ model: '', input: '', output: '', cached: '' });
    });
  };
  const showLedger = async (wallet) => {
    setBusy(true);
    setError('');
    try {
      const result = await client.walletLedger(wallet.wallet_id);
      setLedger({ wallet, items: result.items || [] });
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const showReceivable = async (receivable) => {
    setBusy(true);
    setError('');
    try {
      const result = await client.receivableAllocations(receivable.receivable_id);
      setReceivableDetail({ receivable, items: result.items || [] });
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const showHold = async (hold) => {
    setBusy(true);
    setError('');
    try {
      setHoldDetail(await client.hold(hold.hold_id));
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const decide = (item, decision) => {
    const reason = promptReason(copy.billing.decisionReason);
    if (reason) void perform(() => client.decideAdjustment(item.adjustment_id, decision, reason));
  };
  const refund = (item) => {
    const reason = promptReason(copy.billing.refundReason);
    if (reason && orderDetail)
      void perform(async () => {
        await client.refundPaymentOrder(orderDetail.order_id, item.source_payment_transaction_id, item.package_id, reason);
        setOrderDetail(await client.paymentOrder(orderDetail.order_id));
      });
  };
  const repair = (item) => {
    const reason = promptReason(copy.billing.repairReason);
    if (reason) void perform(() => client.repairPaymentOrder(item.order_id, reason));
  };
  const searchOrders = async (event) => {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      const result = await client.paymentOrders(orderFilter);
      setData((current) => ({ ...current, orders: result.items || [] }));
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const release = (item) => {
    const reason = promptReason(copy.billing.releaseReason);
    if (reason && window.confirm(copy.billing.confirmRelease)) void perform(() => client.releaseHold(item.hold_id, reason));
  };
  const showCases = async (run) => {
    setBusy(true);
    setError('');
    try {
      const result = await client.reconciliationCases(run.reconciliation_run_id);
      setCases({ run, items: result.items || [] });
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const updateReconciliationCase = (item, action) => {
    const command = { action };
    if (action === 'ASSIGN') command.owner_id = String(window.prompt(copy.billing.ownerPrompt, item.owner_id || '') || '').trim();
    if (action === 'SET_REPAIR_ACTION') command.repair_action = promptReason(copy.billing.repairActionPrompt);
    if (action === 'RESOLVE' || action === 'IGNORE') command.review_result = promptReason(copy.billing.reviewResultPrompt);
    command.reason = promptReason(copy.billing.caseCommandReason);
    if ([...command.reason].length < 10 || (action === 'ASSIGN' && !command.owner_id) || (action === 'SET_REPAIR_ACTION' && !command.repair_action) || ((action === 'RESOLVE' || action === 'IGNORE') && !command.review_result)) return;
    void perform(async () => {
      await client.updateReconciliationCase(item.case_id, command);
      const result = await client.reconciliationCases(cases.run.reconciliation_run_id);
      setCases((current) => ({ ...current, items: result.items || [] }));
    });
  };
  const showOrder = async (order) => {
    setBusy(true);
    setError('');
    try {
      setOrderDetail(await client.paymentOrder(order.order_id));
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const searchLateReviews = async (event) => {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      const result = await client.latePaymentReviews(lateFilter);
      setData((current) => ({ ...current, lateReviews: result.items || [] }));
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const showLateReview = async (item) => {
    setBusy(true);
    setError('');
    try {
      setLateReviewDetail(await client.latePaymentReview(item.case_id));
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const searchAnomalies = async (event) => {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      const result = await client.paymentAnomalies(anomalyFilter);
      setData((current) => ({ ...current, anomalies: result.items || [] }));
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const showAnomaly = async (item) => {
    setBusy(true);
    setError('');
    try {
      setAnomalyDetail(await client.paymentAnomaly(item.case_id));
    } catch {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const performAnomaly = (action) =>
    perform(async () => {
      await action();
      if (anomalyDetail) setAnomalyDetail(await client.paymentAnomaly(anomalyDetail.case.case_id));
    });
  const resolveAnomaly = (item) => {
    const resolution = String(window.prompt(copy.billing.anomalyResolutionPrompt, 'ESCALATE_FINANCIAL_REPAIR') || '')
      .trim()
      .toUpperCase();
    const reason = promptReason(copy.billing.anomalyResolutionReason);
    if (resolution && [...reason].length >= 10) void performAnomaly(() => client.resolvePaymentAnomaly(item.case_id, resolution, reason));
  };
  const performLate = (action) =>
    perform(async () => {
      await action();
      if (lateReviewDetail) setLateReviewDetail(await client.latePaymentReview(lateReviewDetail.case.case_id));
    });
  const scheduleLate = (item) => {
    const next = window.prompt(copy.billing.nextReviewPrompt, new Date(Date.now() + 86400000).toISOString().slice(0, 16));
    const reason = promptReason(copy.billing.reviewReason);
    if (next && reason) void performLate(() => client.scheduleLatePaymentReview(item.case_id, next, reason));
  };
  const markProviderBlind = (item) => {
    const reason = promptReason(copy.billing.providerBlindReason);
    if (reason) void performLate(() => client.escalateProviderBlind(item.case_id, reason));
  };
  const importBill = (event) => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file) return;
    void perform(async () => {
      const parsed = JSON.parse(await file.text());
      const rows = Array.isArray(parsed) ? parsed : parsed.items;
      await client.importProviderBill(reconciliation.channel, reconciliation.businessDate, rows);
    });
  };
  const columns = useMemo(
    () => ({
      wallets: [
        {
          key: 'owner_id',
          label: copy.billing.owner,
          render: (item) => (
            <code title={item.owner_id}>
              {item.owner_type} · {shortID(item.owner_id)}
            </code>
          ),
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => (
            <span>
              <Status value={item.status} /> <Status value={item.financial_status} />
            </span>
          ),
        },
        {
          key: 'available_microcredits',
          label: copy.billing.available,
          render: (item) => formatPoints(item.available_microcredits, language),
        },
        {
          key: 'posted_microcredits',
          label: copy.billing.posted,
          render: (item) => formatPoints(item.posted_microcredits, language),
        },
        {
          key: 'reserved_microcredits',
          label: copy.billing.reserved,
          render: (item) => formatPoints(item.reserved_microcredits, language),
        },
        {
          key: 'receivable_microcredits',
          label: copy.billing.receivable,
          render: (item) => formatPoints(item.receivable_microcredits, language),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => <button onClick={() => showLedger(item)}>{copy.billing.viewLedger}</button>,
        },
      ],
      receivables: [
        {
          key: 'wallet_id',
          label: copy.tokens.wallet,
          render: (item) => <code title={item.wallet_id}>{shortID(item.wallet_id)}</code>,
        },
        { key: 'source_type', label: copy.billing.source },
        {
          key: 'original_microcredits',
          label: copy.billing.originalReceivable,
          render: (item) => formatPoints(item.original_microcredits, language),
        },
        {
          key: 'settled_microcredits',
          label: copy.billing.settledReceivable,
          render: (item) => formatPoints(item.settled_microcredits, language),
        },
        {
          key: 'outstanding_microcredits',
          label: copy.billing.outstandingReceivable,
          render: (item) => formatPoints(item.outstanding_microcredits, language),
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => <button onClick={() => showReceivable(item)}>{copy.billing.viewAllocations}</button>,
        },
      ],
      prices: [
        {
          key: 'model',
          label: copy.tokens.model,
          render: (item) => <strong>{item.model}</strong>,
        },
        {
          key: 'input_per_million',
          label: copy.billing.inputPrice,
          render: (item) => formatPoints(item.input_per_million, language),
        },
        {
          key: 'output_per_million',
          label: copy.billing.outputPrice,
          render: (item) => formatPoints(item.output_per_million, language),
        },
        {
          key: 'cached_input_per_million',
          label: copy.billing.cachedPrice,
          render: (item) => formatPoints(item.cached_input_per_million, language),
        },
        {
          key: 'effective_from',
          label: copy.billing.effectiveFrom,
          render: (item) => formatDate(item.effective_from),
        },
        {
          key: 'effective_to',
          label: copy.billing.effectiveTo,
          render: (item) => (item.effective_to ? formatDate(item.effective_to) : <Status value="ACTIVE" />),
        },
      ],
      orders: [
        {
          key: 'merchant_order_no',
          label: copy.billing.order,
          render: (item) => (
            <span>
              <button className="adm-link-button" onClick={() => showOrder(item)}>
                {item.merchant_order_no}
              </button>
              <br />
              <small>
                {copy.billing.pinvouOrder}: {shortID(item.order_id)}
              </small>
            </span>
          ),
        },
        {
          key: 'provider_sku',
          label: copy.billing.product,
          render: (item) => (
            <span>
              {item.product_id}
              <br />
              <code>{item.provider_sku || '—'}</code>
            </span>
          ),
        },
        {
          key: 'primary_provider_transaction_id',
          label: copy.billing.paymentReference,
          render: (item) => (
            <span>
              <code>{item.primary_provider_transaction_id || '—'}</code>
              <br />
              <small>{item.mobile_masked || '—'}</small>
            </span>
          ),
        },
        {
          key: 'amount_fen',
          label: copy.billing.amount,
          render: (item) => formatMoney(item.amount_fen),
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        {
          key: 'fulfillment_status',
          label: copy.billing.fulfillment,
          render: (item) => <Status value={item.fulfillment_status} />,
        },
        {
          key: 'updated_at',
          label: copy.billing.updated,
          render: (item) => formatDate(item.updated_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => (
            <div className="adm-row-actions">
              <button disabled={busy || !canSyncOrders} title={!canSyncOrders ? copy.permissionDenied : undefined} onClick={() => perform(() => client.syncPaymentOrder(item.order_id))}>
                {copy.billing.sync}
              </button>
              <button title={!repairSupported ? copy.billing.providerUnsupported : undefined} disabled={busy || !canRequestRepair || !repairSupported || item.status !== 'MANUAL_REVIEW' || item.fulfillment_status !== 'MANUAL_REVIEW'} onClick={() => repair(item)}>
                {copy.billing.repair}
              </button>
            </div>
          ),
        },
      ],
      refunds: [
        { key: 'operation_type', label: copy.billing.refundType },
        {
          key: 'payment_order_id',
          label: copy.billing.order,
          render: (item) => (
            <button className="adm-link-button" onClick={() => showOrder({ order_id: item.payment_order_id })}>
              {shortID(item.payment_order_id)}
            </button>
          ),
        },
        {
          key: 'payment_transaction_id',
          label: copy.billing.paymentReference,
          render: (item) => <code title={item.payment_transaction_id}>{shortID(item.payment_transaction_id)}</code>,
        },
        {
          key: 'entitlement_package_id',
          label: copy.billing.package,
          render: (item) => <code title={item.entitlement_package_id}>{shortID(item.entitlement_package_id)}</code>,
        },
        {
          key: 'amount_fen',
          label: copy.billing.refundCash,
          render: (item) => formatMoney(item.amount_fen),
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        { key: 'reason', label: copy.billing.reason },
        {
          key: 'updated_at',
          label: copy.billing.updated,
          render: (item) => formatDate(item.updated_at),
        },
      ],
      adjustments: [
        {
          key: 'wallet_id',
          label: copy.tokens.wallet,
          render: (item) => <code title={item.wallet_id}>{shortID(item.wallet_id)}</code>,
        },
        {
          key: 'amount_microcredits',
          label: copy.billing.amount,
          render: (item) => formatPoints(item.amount_microcredits, language),
        },
        { key: 'reason', label: copy.billing.reason },
        {
          key: 'requested_by',
          label: copy.billing.requestedBy,
          render: (item) => <code title={item.requested_by}>{shortID(item.requested_by)}</code>,
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) =>
            canApproveAdjustment && item.status === 'PENDING_APPROVAL' ? (
              <div className="adm-row-actions">
                <button disabled={busy} onClick={() => decide(item, 'approve')}>
                  {copy.billing.approve}
                </button>
                <button className="danger" disabled={busy} onClick={() => decide(item, 'reject')}>
                  {copy.billing.reject}
                </button>
              </div>
            ) : (
              '—'
            ),
        },
      ],
      holds: [
        {
          key: 'usage_request_id',
          label: copy.tokens.usageID,
          render: (item) => (
            <button className="adm-link-button" onClick={() => showHold(item)} title={item.usage_request_id}>
              {shortID(item.usage_request_id)}
            </button>
          ),
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => (
            <span>
              <Status value={item.status} /> <Status value={item.anomaly_state} />
            </span>
          ),
        },
        {
          key: 'max_microcredits',
          label: copy.billing.maximum,
          render: (item) => formatPoints(item.max_microcredits, language),
        },
        {
          key: 'actual_microcredits',
          label: copy.billing.actual,
          render: (item) => formatPoints(item.actual_microcredits, language),
        },
        {
          key: 'expires_at',
          label: copy.billing.expires,
          render: (item) => formatDate(item.expires_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => (
            <div className="adm-row-actions">
              <button onClick={() => showHold(item)}>{copy.billing.viewEvidence}</button>
              {canReleaseHold && ['LEASE_EXPIRED', 'REVIEW_REQUIRED'].includes(item.anomaly_state) && (
                <button className="danger" disabled={busy} onClick={() => release(item)}>
                  {copy.billing.release}
                </button>
              )}
            </div>
          ),
        },
      ],
      reconciliations: [
        { key: 'business_date', label: copy.billing.businessDate },
        { key: 'channel', label: copy.billing.channel },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        { key: 'provider_row_count', label: copy.billing.providerRows },
        { key: 'case_count', label: copy.billing.cases },
        {
          key: 'started_at',
          label: copy.billing.updated,
          render: (item) => formatDate(item.started_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => <button onClick={() => showCases(item)}>{copy.billing.viewCases}</button>,
        },
      ],
      lateReviews: [
        {
          key: 'merchant_order_no',
          label: copy.billing.order,
          render: (item) => (
            <button className="adm-link-button" onClick={() => showLateReview(item)}>
              {item.merchant_order_no}
            </button>
          ),
        },
        {
          key: 'provider_id',
          label: copy.billing.provider,
          render: (item) => (
            <span>
              {item.provider_id}
              <br />
              <code>{item.provider_version}</code>
            </span>
          ),
        },
        {
          key: 'review_status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.review_status} />,
        },
        {
          key: 'owner_id',
          label: copy.billing.owner,
          render: (item) => <code>{shortID(item.owner_id)}</code>,
        },
        {
          key: 'next_review_at',
          label: copy.billing.nextReview,
          render: (item) => formatDate(item.next_review_at),
        },
        {
          key: 'resolution_due_at',
          label: copy.billing.resolutionDue,
          render: (item) => formatDate(item.resolution_due_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => <button onClick={() => showLateReview(item)}>{copy.billing.reviewDetail}</button>,
        },
      ],
      anomalies: [
        {
          key: 'anomaly_type',
          label: copy.billing.anomalyType,
          render: (item) => (
            <button className="adm-link-button" onClick={() => showAnomaly(item)}>
              {item.anomaly_type}
            </button>
          ),
        },
        { key: 'severity', label: copy.billing.severity },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        {
          key: 'owner_id',
          label: copy.billing.owner,
          render: (item) => <code>{shortID(item.owner_id)}</code>,
        },
        {
          key: 'payment_order_id',
          label: copy.billing.order,
          render: (item) => <code>{shortID(item.payment_order_id)}</code>,
        },
        {
          key: 'resolution_due_at',
          label: copy.billing.resolutionDue,
          render: (item) => formatDate(item.resolution_due_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => <button onClick={() => showAnomaly(item)}>{copy.billing.reviewDetail}</button>,
        },
      ],
    }),
    [busy, client, copy, language, refundSupported, repairSupported],
  );
  if (ledger)
    return (
      <BillingDetail
        copy={copy}
        title={`${copy.billing.ledger} · ${shortID(ledger.wallet.wallet_id)}`}
        items={ledger.items}
        columns={[
          { key: 'entry_type', label: copy.billing.type },
          {
            key: 'business_reference',
            label: copy.billing.reference,
            render: (item) => <code>{item.business_reference}</code>,
          },
          {
            key: 'component_type',
            label: copy.billing.component,
            render: (item) => (
              <span>
                {item.component_type || '—'}
                <br />
                <code title={item.entitlement_component_id}>{shortID(item.entitlement_component_id)}</code>
              </span>
            ),
          },
          { key: 'ledger_account_type', label: copy.billing.account },
          {
            key: 'delta_microcredits',
            label: copy.billing.amount,
            render: (item) => formatPoints(item.delta_microcredits, language),
          },
          {
            key: 'journal_balance_microcredits',
            label: copy.billing.balanceProof,
            render: (item) => formatPoints(item.journal_balance_microcredits, language),
          },
          {
            key: 'created_at',
            label: copy.accounts.created,
            render: (item) => formatDate(item.created_at),
          },
        ]}
        onBack={() => setLedger(null)}
      />
    );
  if (receivableDetail)
    return (
      <BillingDetail
        copy={copy}
        title={`${copy.billing.allocations} · ${shortID(receivableDetail.receivable.receivable_id)}`}
        items={receivableDetail.items}
        columns={[
          { key: 'source_type', label: copy.billing.source },
          {
            key: 'source_reference',
            label: copy.billing.reference,
            render: (item) => <code>{item.source_reference}</code>,
          },
          {
            key: 'amount_microcredits',
            label: copy.billing.amount,
            render: (item) => formatPoints(item.amount_microcredits, language),
          },
          {
            key: 'journal_entry_id',
            label: copy.billing.journal,
            render: (item) => <code>{shortID(item.journal_entry_id)}</code>,
          },
          {
            key: 'created_at',
            label: copy.accounts.created,
            render: (item) => formatDate(item.created_at),
          },
        ]}
        onBack={() => setReceivableDetail(null)}
      />
    );
  if (holdDetail)
    return (
      <section className="adm-panel">
        <button className="adm-back" onClick={() => setHoldDetail(null)}>
          ← {copy.billing.back}
        </button>
        <SectionHeader title={`${copy.billing.holdEvidence} · ${shortID(holdDetail.hold.hold_id)}`} count={(holdDetail.component_allocations || []).length} copy={copy} />
        <div className="adm-detail-grid">
          <div>
            <small>{copy.accounts.status}</small>
            <strong>
              <Status value={holdDetail.hold.status} /> <Status value={holdDetail.hold.anomaly_state} />
            </strong>
          </div>
          <div>
            <small>{copy.billing.expires}</small>
            <strong>{formatDate(holdDetail.hold.expires_at)}</strong>
          </div>
          <div>
            <small>{copy.billing.modelProtocol}</small>
            <strong>
              {holdDetail.hold.model_id || '—'} · {holdDetail.hold.protocol_code || '—'}
            </strong>
          </div>
          <div>
            <small>{copy.billing.maximum}</small>
            <strong>{formatPoints(holdDetail.hold.max_microcredits, language)}</strong>
          </div>
        </div>
        <h3>{copy.billing.componentAllocations}</h3>
        <Table
          columns={[
            { key: 'component_type', label: copy.billing.component },
            {
              key: 'package_id',
              label: copy.billing.package,
              render: (item) => <code>{shortID(item.package_id)}</code>,
            },
            {
              key: 'reserved_microcredits',
              label: copy.billing.reserved,
              render: (item) => formatPoints(item.reserved_microcredits, language),
            },
            {
              key: 'settled_microcredits',
              label: copy.billing.settledReceivable,
              render: (item) => formatPoints(item.settled_microcredits, language),
            },
            {
              key: 'released_microcredits',
              label: copy.billing.released,
              render: (item) => formatPoints(item.released_microcredits, language),
            },
          ]}
          items={holdDetail.component_allocations || []}
          copy={copy}
        />
        <h3>{copy.billing.leaseHistory}</h3>
        <Table
          columns={[
            { key: 'event_type', label: copy.billing.type },
            {
              key: 'additional_microcredits',
              label: copy.billing.additional,
              render: (item) => formatPoints(item.additional_microcredits, language),
            },
            {
              key: 'expires_at',
              label: copy.billing.expires,
              render: (item) => formatDate(item.expires_at),
            },
            {
              key: 'created_at',
              label: copy.accounts.created,
              render: (item) => formatDate(item.created_at),
            },
          ]}
          items={holdDetail.lease_events || []}
          copy={copy}
        />
        <h3>{copy.billing.adminActions}</h3>
        <Table
          columns={[
            { key: 'action', label: copy.billing.type },
            { key: 'reason', label: copy.billing.reason },
            { key: 'actor', label: copy.billing.operator },
            {
              key: 'created_at',
              label: copy.accounts.created,
              render: (item) => formatDate(item.created_at),
            },
          ]}
          items={holdDetail.admin_actions || []}
          copy={copy}
        />
      </section>
    );
  if (cases)
    return (
      <BillingDetail
        copy={copy}
        title={`${copy.billing.cases} · ${cases.run.business_date}`}
        items={cases.items}
        columns={[
          { key: 'merchant_order_no', label: copy.billing.order },
          { key: 'case_type', label: copy.billing.caseType },
          { key: 'severity', label: copy.billing.severity },
          {
            key: 'status',
            label: copy.accounts.status,
            render: (item) => <Status value={item.status} />,
          },
          {
            key: 'owner_id',
            label: copy.billing.owner,
            render: (item) => <code>{shortID(item.owner_id)}</code>,
          },
          {
            key: 'provider_snapshot',
            label: copy.billing.providerSnapshot,
            render: (item) => <code title={formatSummary(item.provider_snapshot)}>{formatSummary(item.provider_snapshot)}</code>,
          },
          {
            key: 'internal_snapshot',
            label: copy.billing.internalSnapshot,
            render: (item) => <code title={formatSummary(item.internal_snapshot)}>{formatSummary(item.internal_snapshot)}</code>,
          },
          {
            key: 'refund_snapshot',
            label: copy.billing.refundSnapshot,
            render: (item) => <code title={formatSummary(item.refund_snapshot)}>{formatSummary(item.refund_snapshot)}</code>,
          },
          {
            key: 'ledger_snapshot',
            label: copy.billing.ledgerSnapshot,
            render: (item) => <code title={formatSummary(item.ledger_snapshot)}>{formatSummary(item.ledger_snapshot)}</code>,
          },
          { key: 'repair_action', label: copy.billing.repairAction },
          { key: 'review_result', label: copy.billing.reviewResult },
          {
            key: 'actions',
            label: copy.accounts.actions,
            render: (item) => (
              <div className="adm-row-actions">
                {!item.owner_id && (
                  <button disabled={busy} onClick={() => updateReconciliationCase(item, 'CLAIM')}>
                    {copy.billing.claimReview}
                  </button>
                )}
                <button disabled={busy || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'ASSIGN')}>
                  {copy.billing.assignOwner}
                </button>
                <button disabled={busy || !item.owner_id || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'SET_REPAIR_ACTION')}>
                  {copy.billing.setRepairAction}
                </button>
                <button disabled={busy || !item.owner_id || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'RESOLVE')}>
                  {copy.billing.resolveCase}
                </button>
                <button disabled={busy || !item.owner_id || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'IGNORE')}>
                  {copy.billing.ignoreCase}
                </button>
              </div>
            ),
          },
        ]}
        onBack={() => setCases(null)}
      />
    );
  if (anomalyDetail) {
    const item = anomalyDetail.case;
    return (
      <section className="adm-panel">
        <button className="adm-back" onClick={() => setAnomalyDetail(null)}>
          ← {copy.billing.back}
        </button>
        <SectionHeader title={`${copy.billing.anomalyDetail} · ${item.anomaly_type}`} copy={copy}>
          <div className="adm-row-actions">
            {canSyncOrders && !item.owner_id && item.status === 'OPEN' && (
              <button disabled={busy} onClick={() => performAnomaly(() => client.claimPaymentAnomaly(item.case_id))}>
                {copy.billing.claimReview}
              </button>
            )}
            <button disabled={busy || !canRequestRepair || item.status !== 'OPEN' || !item.owner_id} title={!canRequestRepair ? copy.permissionDenied : undefined} onClick={() => resolveAnomaly(item)}>
              {copy.billing.resolveCase}
            </button>
          </div>
        </SectionHeader>
        {notice && <div className="adm-inline-message ok">{notice}</div>}
        {error && <div className="adm-inline-message error">{error}</div>}
        <div className="adm-detail-grid">
          <div>
            <small>{copy.accounts.status}</small>
            <strong>
              <Status value={item.status} /> · {item.severity}
            </strong>
          </div>
          <div>
            <small>{copy.billing.owner}</small>
            <strong>{shortID(item.owner_id)}</strong>
          </div>
          <div>
            <small>{copy.billing.financialAction}</small>
            <Status value={item.financial_action_status} />
          </div>
          <div>
            <small>{copy.billing.resolutionDue}</small>
            <strong>{formatDate(item.resolution_due_at)}</strong>
          </div>
          <div>
            <small>{copy.billing.resourceSnapshot}</small>
            <code>{formatSummary(item.resource_snapshot)}</code>
          </div>
          <div>
            <small>{copy.billing.reviewResult}</small>
            <strong>
              {item.resolution_code || '—'} {item.resolution_reason || ''}
            </strong>
          </div>
        </div>
        <h3>{copy.billing.operatorTimeline}</h3>
        <Table
          columns={[
            { key: 'event_type', label: copy.billing.type },
            {
              key: 'actor_id',
              label: copy.billing.operator,
              render: (event) => <code>{shortID(event.actor_id)}</code>,
            },
            {
              key: 'details',
              label: copy.billing.description,
              render: (event) => <code>{formatSummary(event.details)}</code>,
            },
            {
              key: 'created_at',
              label: copy.accounts.created,
              render: (event) => formatDate(event.created_at),
            },
          ]}
          items={anomalyDetail.events || []}
          copy={copy}
        />
      </section>
    );
  }
  if (lateReviewDetail) {
    const item = lateReviewDetail.case;
    const active = ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND'].includes(item.review_status);
    return (
      <section className="adm-panel">
        <button className="adm-back" onClick={() => setLateReviewDetail(null)}>
          ← {copy.billing.back}
        </button>
        <SectionHeader title={`${copy.billing.reviewDetail} · ${item.merchant_order_no}`} copy={copy}>
          <div className="adm-row-actions">
            {canSyncOrders && !item.owner_id && (
              <button disabled={busy} onClick={() => performLate(() => client.claimLatePaymentReview(item.case_id))}>
                {copy.billing.claimReview}
              </button>
            )}
            <button disabled={busy || !canSyncOrders || !active || !item.owner_id} onClick={() => performLate(() => client.queryLatePaymentReview(item.case_id))}>
              {copy.billing.queryProvider}
            </button>
            <button disabled={busy || !canSyncOrders || !active || !item.owner_id} onClick={() => scheduleLate(item)}>
              {copy.billing.scheduleReview}
            </button>
            <button disabled={busy || !canSyncOrders || item.review_status !== 'REVIEW_REQUIRED_LATE_PAYMENT' || !item.owner_id} onClick={() => performLate(() => client.archiveLatePaymentReview(item.case_id))}>
              {copy.billing.archiveUnpaid}
            </button>
            <button className="danger" disabled={busy || !canSyncOrders || !active || !item.owner_id} onClick={() => markProviderBlind(item)}>
              {copy.billing.providerBlind}
            </button>
          </div>
        </SectionHeader>
        {notice && <div className="adm-inline-message ok">{notice}</div>}
        {error && <div className="adm-inline-message error">{error}</div>}
        <div className="adm-detail-grid">
          <div>
            <small>{copy.accounts.status}</small>
            <strong>
              <Status value={item.review_status} /> · {item.severity}
            </strong>
          </div>
          <div>
            <small>{copy.billing.owner}</small>
            <strong>{shortID(item.owner_id)}</strong>
          </div>
          <div>
            <small>{copy.billing.nextReview}</small>
            <strong>{formatDate(item.next_review_at)}</strong>
          </div>
          <div>
            <small>{copy.billing.lastQuery}</small>
            <strong>
              {formatDate(item.last_query_at)} · {item.last_query_result || '—'}
            </strong>
          </div>
          <div>
            <small>{copy.billing.providerSnapshot}</small>
            <code>{formatSummary(item.provider_capability_snapshot)}</code>
          </div>
          <div>
            <small>{copy.billing.policySnapshot}</small>
            <code>{formatSummary(item.policy_snapshot)}</code>
          </div>
        </div>
        <h3>{copy.billing.evidenceTimeline}</h3>
        <Table
          columns={[
            { key: 'evidence_type', label: copy.billing.type },
            {
              key: 'payload_digest',
              label: copy.billing.evidenceKey,
              render: (evidence) => <code>{shortID(evidence.payload_digest)}</code>,
            },
            {
              key: 'payload_snapshot',
              label: copy.billing.description,
              render: (evidence) => <code>{formatSummary(evidence.payload_snapshot)}</code>,
            },
            {
              key: 'created_at',
              label: copy.accounts.created,
              render: (evidence) => formatDate(evidence.created_at),
            },
          ]}
          items={lateReviewDetail.evidence || []}
          copy={copy}
        />
        <h3>{copy.billing.operatorTimeline}</h3>
        <Table
          columns={[
            { key: 'event_type', label: copy.billing.type },
            {
              key: 'actor_id',
              label: copy.billing.operator,
              render: (event) => <code>{shortID(event.actor_id)}</code>,
            },
            {
              key: 'details',
              label: copy.billing.description,
              render: (event) => <code>{formatSummary(event.details)}</code>,
            },
            {
              key: 'created_at',
              label: copy.accounts.created,
              render: (event) => formatDate(event.created_at),
            },
          ]}
          items={lateReviewDetail.events || []}
          copy={copy}
        />
      </section>
    );
  }
  if (orderDetail)
    return (
      <section className="adm-panel">
        <button className="adm-back" onClick={() => setOrderDetail(null)}>
          ← {copy.billing.back}
        </button>
        <SectionHeader title={`${copy.billing.orderDetail} · ${orderDetail.merchant_order_no}`} count={(orderDetail.transactions || []).length} copy={copy} />
        <div className="adm-detail-grid">
          <div>
            <small>{copy.billing.payer}</small>
            <strong>{shortID(orderDetail.payer_user_id)}</strong>
          </div>
          <div>
            <small>{copy.billing.beneficiary}</small>
            <strong>{orderDetail.beneficiary_organization_id ? shortID(orderDetail.beneficiary_organization_id) : copy.billing.personalBeneficiary}</strong>
          </div>
          <div>
            <small>{copy.billing.paymentSummary}</small>
            <code>{formatSummary(orderDetail.payment_summary)}</code>
          </div>
          <div>
            <small>{copy.billing.refundSummary}</small>
            <code>{formatSummary(orderDetail.refund_summary)}</code>
          </div>
          <div>
            <small>{copy.billing.chargebackSummary}</small>
            <code>{formatSummary(orderDetail.chargeback_summary)}</code>
          </div>
          <div>
            <small>{copy.billing.attentionSummary}</small>
            <Status value={orderDetail.attention_summary} />
          </div>
        </div>
        <h3>{copy.billing.transactions}</h3>
        <Table
          columns={[
            {
              key: 'provider_transaction_id',
              label: copy.billing.paymentReference,
              render: (item) => <code>{item.provider_transaction_id}</code>,
            },
            { key: 'provider_id', label: copy.billing.provider },
            {
              key: 'status',
              label: copy.accounts.status,
              render: (item) => <Status value={item.status} />,
            },
            {
              key: 'fulfillment_status',
              label: copy.billing.fulfillment,
              render: (item) => <Status value={item.fulfillment_status} />,
            },
            {
              key: 'amount_fen',
              label: copy.billing.amount,
              render: (item) => formatMoney(item.amount_fen),
            },
            {
              key: 'verified_at',
              label: copy.billing.updated,
              render: (item) => formatDate(item.verified_at),
            },
          ]}
          items={orderDetail.transactions || []}
          copy={copy}
        />
        <h3>{copy.billing.entitlementPackages}</h3>
        <Table
          columns={[
            {
              key: 'package_id',
              label: copy.billing.package,
              render: (item) => <code title={item.package_id}>{shortID(item.package_id)}</code>,
            },
            {
              key: 'source_payment_transaction_id',
              label: copy.billing.paymentReference,
              render: (item) => <code title={item.source_payment_transaction_id}>{shortID(item.source_payment_transaction_id)}</code>,
            },
            {
              key: 'points_remaining_microcredits',
              label: copy.billing.available,
              render: (item) => formatPoints(item.points_remaining_microcredits, language),
            },
            {
              key: 'status',
              label: copy.accounts.status,
              render: (item) => <Status value={item.status} />,
            },
            {
              key: 'actions',
              label: copy.accounts.actions,
              render: (item) =>
                canRequestRepair && refundSupported && item.status === 'ACTIVE' ? (
                  <button className="danger" disabled={busy} onClick={() => refund(item)}>
                    {copy.billing.refundWholePackage}
                  </button>
                ) : (
                  '—'
                ),
            },
          ]}
          items={orderDetail.entitlement_packages || []}
          copy={copy}
        />
        <h3>{copy.billing.evidence}</h3>
        <Table
          columns={[
            { key: 'evidence_type', label: copy.billing.type },
            {
              key: 'provider_transaction_id',
              label: copy.billing.paymentReference,
              render: (item) => <code>{item.provider_transaction_id}</code>,
            },
            {
              key: 'evidence_key',
              label: copy.billing.evidenceKey,
              render: (item) => <code>{item.evidence_key}</code>,
            },
            {
              key: 'merge_result',
              label: copy.billing.mergeResult,
              render: (item) => <Status value={item.merge_result} />,
            },
            {
              key: 'received_at',
              label: copy.accounts.created,
              render: (item) => formatDate(item.received_at),
            },
          ]}
          items={orderDetail.evidence || []}
          copy={copy}
        />
      </section>
    );
  return (
    <section className="adm-panel">
      <SectionHeader title={copy.billing.title} count={items.length} copy={copy} />
      {hasLimitedPaymentCapabilities && (
        <div className="adm-inline-message">
          {copy.billing.capabilityLimitations} {copy.billing.reconciliationMode}: <strong>{paymentCapabilities.reconciliation_mode || '—'}</strong>
        </div>
      )}
      <Tabs active={tab} onChange={setTab} items={visibleTabs.map((key) => ({ key, label: copy.billing[key] }))} />
      {tab === 'orders' && (
        <form className="adm-ops-form" onSubmit={searchOrders}>
          <input
            value={orderFilter.q}
            onChange={(event) =>
              setOrderFilter((current) => ({
                ...current,
                q: event.target.value,
              }))
            }
            maxLength={160}
            placeholder={copy.billing.orderSearch}
          />
          <select
            value={orderFilter.status}
            onChange={(event) =>
              setOrderFilter((current) => ({
                ...current,
                status: event.target.value,
              }))
            }
          >
            <option value="">{copy.billing.allStatuses}</option>
            {['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'MANUAL_REVIEW'].map((status) => (
              <option key={status}>{status}</option>
            ))}
          </select>
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'lateReviews' && (
        <form className="adm-ops-form" onSubmit={searchLateReviews}>
          <select
            value={lateFilter.status}
            onChange={(event) =>
              setLateFilter((current) => ({
                ...current,
                status: event.target.value,
              }))
            }
          >
            <option value="">{copy.billing.allStatuses}</option>
            {['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID'].map((status) => (
              <option key={status}>{status}</option>
            ))}
          </select>
          <input
            value={lateFilter.provider_id}
            onChange={(event) =>
              setLateFilter((current) => ({
                ...current,
                provider_id: event.target.value,
              }))
            }
            placeholder={copy.billing.provider}
          />
          <input
            value={lateFilter.owner_id}
            onChange={(event) =>
              setLateFilter((current) => ({
                ...current,
                owner_id: event.target.value,
              }))
            }
            placeholder={copy.billing.ownerID}
          />
          <label>
            {copy.billing.enteredFrom}
            <input
              type="datetime-local"
              value={lateFilter.entered_from}
              onChange={(event) =>
                setLateFilter((current) => ({
                  ...current,
                  entered_from: event.target.value,
                }))
              }
            />
          </label>
          <label>
            {copy.billing.enteredTo}
            <input
              type="datetime-local"
              value={lateFilter.entered_to}
              onChange={(event) =>
                setLateFilter((current) => ({
                  ...current,
                  entered_to: event.target.value,
                }))
              }
            />
          </label>
          <label>
            {copy.billing.nextReviewBefore}
            <input
              type="datetime-local"
              value={lateFilter.next_review_before}
              onChange={(event) =>
                setLateFilter((current) => ({
                  ...current,
                  next_review_before: event.target.value,
                }))
              }
            />
          </label>
          <label>
            <input
              type="checkbox"
              checked={lateFilter.overdue}
              onChange={(event) =>
                setLateFilter((current) => ({
                  ...current,
                  overdue: event.target.checked,
                }))
              }
            />
            {copy.billing.overdueOnly}
          </label>
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'anomalies' && (
        <form className="adm-ops-form" onSubmit={searchAnomalies}>
          <select
            value={anomalyFilter.anomaly_type}
            onChange={(event) =>
              setAnomalyFilter((current) => ({
                ...current,
                anomaly_type: event.target.value,
              }))
            }
          >
            <option value="">{copy.billing.allAnomalyTypes}</option>
            {['CHARGEBACK', 'FORCED_REFUND', 'DUPLICATE_PAYMENT', 'LATE_PAYMENT_CONFLICT', 'PAYMENT_FACT_CONFLICT', 'REFUND_FAILURE', 'ILLEGAL_TRANSITION'].map((type) => (
              <option key={type}>{type}</option>
            ))}
          </select>
          <select
            value={anomalyFilter.status}
            onChange={(event) =>
              setAnomalyFilter((current) => ({
                ...current,
                status: event.target.value,
              }))
            }
          >
            <option value="">{copy.billing.allStatuses}</option>
            <option>OPEN</option>
            <option>RESOLVED</option>
          </select>
          <select
            value={anomalyFilter.severity}
            onChange={(event) =>
              setAnomalyFilter((current) => ({
                ...current,
                severity: event.target.value,
              }))
            }
          >
            <option value="">{copy.billing.allSeverities}</option>
            <option>P0</option>
            <option>P1</option>
            <option>P2</option>
          </select>
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'prices' && canManagePricing && (
        <form className="adm-ops-form" onSubmit={createPrice}>
          <input placeholder={copy.tokens.model} value={price.model} onChange={(event) => setPrice((current) => ({ ...current, model: event.target.value }))} />
          <input type="number" min="0" step="0.0001" placeholder={copy.billing.inputPrice} value={price.input} onChange={(event) => setPrice((current) => ({ ...current, input: event.target.value }))} />
          <input
            type="number"
            min="0"
            step="0.0001"
            placeholder={copy.billing.outputPrice}
            value={price.output}
            onChange={(event) =>
              setPrice((current) => ({
                ...current,
                output: event.target.value,
              }))
            }
          />
          <input
            type="number"
            min="0"
            step="0.0001"
            placeholder={copy.billing.cachedPrice}
            value={price.cached}
            onChange={(event) =>
              setPrice((current) => ({
                ...current,
                cached: event.target.value,
              }))
            }
          />
          <button disabled={busy}>{copy.billing.activatePrice}</button>
        </form>
      )}
      {tab === 'reconciliations' && canReconcile && (
        <form
          className="adm-ops-form"
          onSubmit={(event) => {
            event.preventDefault();
            if (reconciliationSupported) void perform(() => client.runReconciliation(reconciliation.channel, reconciliation.businessDate));
          }}
        >
          <select
            value={reconciliation.channel}
            onChange={(event) =>
              setReconciliation((current) => ({
                ...current,
                channel: event.target.value,
              }))
            }
          >
            <option>ALIPAY</option>
            <option>WECHAT</option>
            <option>UNIONPAY</option>
          </select>
          <input
            type="date"
            value={reconciliation.businessDate}
            onChange={(event) =>
              setReconciliation((current) => ({
                ...current,
                businessDate: event.target.value,
              }))
            }
          />
          {reconciliationSupported && <button disabled={busy}>{copy.billing.runReconciliation}</button>}
          {reconciliationImportSupported && (
            <label>
              {copy.billing.importBill}
              <input type="file" accept="application/json,.json" disabled={busy} onChange={importBill} />
            </label>
          )}
          {!reconciliationSupported && !reconciliationImportSupported && <span>{copy.billing.queryDegraded}</span>}
        </form>
      )}
      {notice && <div className="adm-inline-message ok">{notice}</div>}
      {error && <div className="adm-inline-message error">{error}</div>}
      <Table columns={columns[tab]} items={items} copy={copy} />
    </section>
  );
}

function BillingDetail({ copy, title, items, columns, onBack }) {
  return (
    <section className="adm-panel">
      <button className="adm-back" onClick={onBack}>
        ← {copy.billing.back}
      </button>
      <SectionHeader title={title} count={items.length} copy={copy} />
      <Table columns={columns} items={items} copy={copy} />
    </section>
  );
}

function Tokens({ copy, language, data, client, setData, permissions }) {
  const [tab, setTab] = useState('tokenAccounts');
  const [account, setAccount] = useState(null);
  const [usageDetail, setUsageDetail] = useState(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [route, setRoute] = useState({
    model: '',
    protocol_code: 'CHAT',
    priority: 100,
    default_scope: 'NONE',
    health_status: 'HEALTHY',
  });
  const [newAccount, setNewAccount] = useState({
    owner_type: 'ORG',
    owner_id: '',
    name: '',
    default_priority: 100,
  });
  const permissionSet = new Set(permissions || []);
  const canReadTokens = permissionSet.has('platform.tokens.read');
  const canManageTokens = permissionSet.has('platform.tokens.manage');
  const canValidateCredentials = permissionSet.has('platform.tokens.credentials.validate');
  const canReadModels = permissionSet.has('platform.models.read');
  const canManageModels = permissionSet.has('platform.models.manage');
  const canSyncModels = permissionSet.has('platform.models.sync');
  const canReadUsage = permissionSet.has('platform.usage.read');
  const canRetryUsage = permissionSet.has('platform.usage.reconcile.retry');
  const visibleTabs = [...(canReadTokens ? ['tokenAccounts', 'provisionings'] : []), ...(canReadModels ? ['models'] : []), ...(canReadUsage ? ['usage'] : [])];
  useEffect(() => {
    if (!visibleTabs.includes(tab)) setTab(visibleTabs[0] || 'tokenAccounts');
  }, [tab, visibleTabs.join('|')]);
  const items = data[tab] || [];
  const perform = async (action) => {
    setBusy(true);
    setNotice('');
    setError('');
    try {
      const message = await action();
      setNotice(message || copy.tokens.completed);
    } catch {
      setError(copy.tokens.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const openAccount = (item) =>
    perform(async () => {
      setAccount(await client.tokenAccount(item.token_account_id));
      setUsageDetail(null);
    });
  const openUsage = (item) =>
    perform(async () => {
      setUsageDetail(await client.usageRequest(item.usage_request_id));
      setAccount(null);
    });
  const refreshAccount = async () => setAccount(await client.tokenAccount(account.token_account_id));
  const updateStatus = () => {
    const status = account.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE';
    const reason = window.prompt(copy.tokens.statusReason);
    if (!reason?.trim()) return;
    void perform(async () => {
      await client.setTokenAccountStatus(account.token_account_id, status, reason.trim());
      await refreshAccount();
      setData((current) => ({
        ...current,
        tokenAccounts: (current.tokenAccounts || []).map((item) => (item.token_account_id === account.token_account_id ? { ...item, status } : item)),
      }));
    });
  };
  const saveRoute = (event) => {
    event.preventDefault();
    if (!route.model.trim()) return;
    void perform(async () => {
      await client.upsertTokenRoute(account.token_account_id, {
        ...route,
        model: route.model.trim(),
        priority: Number(route.priority),
      });
      await refreshAccount();
      setRoute((current) => ({ ...current, model: '' }));
    });
  };
  const deleteRoute = (model) => {
    if (!window.confirm(copy.tokens.confirmDeleteRoute(model))) return;
    void perform(async () => {
      await client.deleteTokenRoute(account.token_account_id, model);
      await refreshAccount();
    });
  };
  const validateCredential = (credentialID) =>
    perform(async () => {
      const result = await client.validateTokenCredential(account.token_account_id, credentialID);
      await refreshAccount();
      return result.validation_status === 'VALID' ? copy.tokens.credentialValid : copy.tokens.credentialInvalid;
    });
  const createTokenAccount = (event) => {
    event.preventDefault();
    const ownerID = newAccount.owner_id.trim();
    if (!ownerID || !newAccount.name.trim()) return;
    const isOrganization = newAccount.owner_type === 'ORG';
    void perform(async () => {
      await client.createTokenAccount({
        owner_type: newAccount.owner_type,
        owner_id: ownerID,
        owner_organization_id: isOrganization ? ownerID : null,
        owner_unit_id: null,
        name: newAccount.name.trim(),
        share_scope: isOrganization ? 'ORG_ALL' : 'OWNER_UNIT_ONLY',
        default_priority: Number(newAccount.default_priority),
      });
      const result = await client.tokenAccounts();
      setData((current) => ({ ...current, tokenAccounts: result.items || [] }));
      setNewAccount((current) => ({ ...current, owner_id: '', name: '' }));
      return copy.tokens.accountCreated;
    });
  };
  const retryReconciliation = () => {
    const reason = window.prompt(copy.tokens.retryReason);
    if (!reason?.trim()) return;
    void perform(async () => {
      setUsageDetail(await client.retryUsageReconciliation(usageDetail.usage_request_id, reason.trim()));
    });
  };
  const refreshModels = async () => {
    const result = await client.catalogModels();
    setData((current) => ({
      ...current,
      models: result.items || [],
      catalogVersion: result.catalog_version || 0,
    }));
  };
  const syncModels = () =>
    perform(async () => {
      await client.syncModelCatalog();
      await refreshModels();
      return copy.tokens.catalogSynced;
    });
  const editModel = (item) => {
    const displayName = window.prompt(copy.tokens.displayName, item.draft?.display_name || item.display_name);
    if (!displayName?.trim()) return;
    const aliases = window.prompt(copy.tokens.aliases, (item.draft?.aliases || item.aliases || []).join(', '));
    void perform(async () => {
      await client.saveModelDraft(item.model_id, {
        expected_catalog_version: data.catalogVersion,
        expected_draft_version: item.draft?.draft_version || 0,
        display_name: displayName.trim(),
        description: item.draft?.description ?? item.description ?? '',
        aliases: String(aliases || '')
          .split(',')
          .map((value) => value.trim())
          .filter(Boolean),
        protocol_code: item.protocol_code,
        protocol_version: item.protocol_version,
        context_length: item.draft?.context_length || item.context_length,
        maximum_output_tokens: item.draft?.maximum_output_tokens ?? item.maximum_output_tokens,
        supported_modalities: item.draft?.supported_modalities || item.supported_modalities,
        tokenizer_version: item.draft?.tokenizer_version || item.tokenizer_version,
      });
      await refreshModels();
      return copy.tokens.draftSaved;
    });
  };
  const publishModel = (item) => {
    const reason = window.prompt(copy.tokens.changeReason);
    if (!reason?.trim() || !item.draft) return;
    void perform(async () => {
      await client.publishModelDraft(item.model_id, {
        expected_catalog_version: data.catalogVersion,
        expected_draft_version: item.draft.draft_version,
        idempotency_key: `model-publish-${crypto.randomUUID()}`,
        reason: reason.trim(),
      });
      await refreshModels();
      return copy.tokens.modelPublished;
    });
  };
  const transitionModel = (item, target_status) => {
    const reason = window.prompt(copy.tokens.changeReason);
    if (!reason?.trim()) return;
    void perform(async () => {
      await client.transitionModel(item.model_id, {
        expected_catalog_version: data.catalogVersion,
        target_status,
        effective_at: new Date().toISOString(),
        new_session_cutoff_at: new Date().toISOString(),
        continuation_cutoff_at: new Date().toISOString(),
        force_cancel_inflight: false,
        idempotency_key: `model-lifecycle-${crypto.randomUUID()}`,
        reason: reason.trim(),
      });
      await refreshModels();
      return copy.tokens.lifecycleChanged;
    });
  };
  const rollbackCatalog = () => {
    const source = Number(window.prompt(copy.tokens.rollbackVersion));
    const reason = window.prompt(copy.tokens.changeReason);
    if (!Number.isInteger(source) || source < 1 || !reason?.trim()) return;
    void perform(async () => {
      await client.rollbackModelCatalog({
        source_catalog_version: source,
        expected_catalog_version: data.catalogVersion,
        idempotency_key: `model-rollback-${crypto.randomUUID()}`,
        reason: reason.trim(),
      });
      await refreshModels();
      return copy.tokens.catalogRolledBack;
    });
  };
  const refreshProvisionings = async () => {
    const result = await client.provisionings();
    setData((current) => ({ ...current, provisionings: result.items || [] }));
  };
  const repairProvisioning = (item) => {
    const reason = window.prompt(copy.tokens.repairReason);
    if (!reason?.trim()) return;
    void perform(async () => {
      await client.repairProvisioning(item.provisioning_id, reason.trim());
      await refreshProvisionings();
      return copy.tokens.repairStarted;
    });
  };
  if (account) return <TokenAccountDetail copy={copy} account={account} route={route} setRoute={setRoute} busy={busy} notice={notice} error={error} onBack={() => setAccount(null)} onStatus={updateStatus} onSaveRoute={saveRoute} onDeleteRoute={deleteRoute} onValidate={validateCredential} canManage={canManageTokens} canValidate={canValidateCredentials} />;
  if (usageDetail) return <UsageRequestDetail copy={copy} language={language} item={usageDetail} busy={busy} notice={notice} error={error} onBack={() => setUsageDetail(null)} onRetry={retryReconciliation} canRetry={canRetryUsage} />;
  const columns =
    tab === 'tokenAccounts'
      ? [
          {
            key: 'name',
            label: copy.tokens.name,
            render: (item) => (
              <button className="adm-name-link" onClick={() => openAccount(item)}>
                {item.name}
              </button>
            ),
          },
          {
            key: 'owner_id',
            label: copy.tokens.owner,
            render: (item) => (
              <code title={item.owner_id}>
                {item.owner_type} · {shortID(item.owner_id)}
              </code>
            ),
          },
          {
            key: 'status',
            label: copy.accounts.status,
            render: (item) => <Status value={item.status} />,
          },
          { key: 'share_scope', label: copy.tokens.sharing },
          { key: 'active_credential_count', label: copy.tokens.credentials },
          { key: 'route_count', label: copy.tokens.routes },
          {
            key: 'updated_at',
            label: copy.billing.updated,
            render: (item) => formatDate(item.updated_at),
          },
        ]
      : tab === 'usage'
        ? [
            {
              key: 'id',
              label: copy.tokens.usageID,
              render: (item) => (
                <button className="adm-name-link" title={item.usage_request_id} onClick={() => openUsage(item)}>
                  {shortID(item.usage_request_id)}
                </button>
              ),
            },
            { key: 'model', label: copy.tokens.model },
            {
              key: 'status',
              label: copy.accounts.status,
              render: (item) => <Status value={item.status} />,
            },
            {
              key: 'maximum_microcredits',
              label: copy.tokens.maximum,
              render: (item) => formatPoints(item.maximum_microcredits, language),
            },
            {
              key: 'actual_microcredits',
              label: copy.tokens.actual,
              render: (item) => formatPoints(item.actual_microcredits, language),
            },
            {
              key: 'upstream_request_id',
              label: copy.tokens.upstreamReference,
              render: (item) => <code title={item.upstream_request_id}>{shortID(item.upstream_request_id)}</code>,
            },
            { key: 'error_class', label: copy.tokens.error },
            {
              key: 'started_at',
              label: copy.tokens.started,
              render: (item) => formatDate(item.started_at),
            },
          ]
        : tab === 'models'
          ? [
              {
                key: 'display_name',
                label: copy.tokens.displayName,
                render: (item) => <strong>{item.display_name}</strong>,
              },
              {
                key: 'model_id',
                label: copy.tokens.modelID,
                render: (item) => <code title={item.model_id}>{shortID(item.model_id)}</code>,
              },
              {
                key: 'lifecycle_status',
                label: copy.accounts.status,
                render: (item) => <Status value={item.lifecycle_status} />,
              },
              { key: 'catalog_version', label: copy.tokens.catalogVersion },
              {
                key: 'capability_version',
                label: copy.tokens.capabilityVersion,
              },
              { key: 'protocol_code', label: copy.tokens.protocol },
              {
                key: 'context_length',
                label: copy.tokens.contextLength,
                render: (item) => formatNumber(item.context_length),
              },
              {
                key: 'source_available',
                label: copy.tokens.sourceAvailable,
                render: (item) => <Status value={item.source_available ? 'ACTIVE' : 'UNAVAILABLE'} />,
              },
              {
                key: 'actions',
                label: copy.accounts.actions,
                render: (item) =>
                  canManageModels ? (
                    <span className="adm-row-actions">
                      <button disabled={busy || item.lifecycle_status === 'REMOVED'} onClick={() => editModel(item)}>
                        {copy.tokens.editDraft}
                      </button>
                      {item.draft && (
                        <button disabled={busy} onClick={() => publishModel(item)}>
                          {copy.tokens.publish}
                        </button>
                      )}
                      {item.lifecycle_status === 'PUBLISHED' && (
                        <button disabled={busy} onClick={() => transitionModel(item, 'DEPRECATED')}>
                          {copy.tokens.deprecate}
                        </button>
                      )}
                      {item.lifecycle_status !== 'REMOVED' && (
                        <button className="danger" disabled={busy} onClick={() => transitionModel(item, 'REMOVED')}>
                          {copy.tokens.removeModel}
                        </button>
                      )}
                    </span>
                  ) : (
                    '—'
                  ),
              },
            ]
          : [
              {
                key: 'provisioning_id',
                label: copy.tokens.provisioningID,
                render: (item) => <code title={item.provisioning_id}>{shortID(item.provisioning_id)}</code>,
              },
              {
                key: 'scope_type',
                label: copy.tokens.scope,
                render: (item) => `${item.scope_type} · ${shortID(item.scope_id)}`,
              },
              {
                key: 'model_id',
                label: copy.tokens.modelID,
                render: (item) => <code title={item.model_id}>{shortID(item.model_id)}</code>,
              },
              {
                key: 'status',
                label: copy.accounts.status,
                render: (item) => <Status value={item.status} />,
              },
              { key: 'stage', label: copy.tokens.stage },
              {
                key: 'desired_generation',
                label: copy.tokens.generation,
                render: (item) => `${item.actual_generation || '—'} / ${item.desired_generation}`,
              },
              {
                key: 'attempt_count',
                label: copy.tokens.attempts,
                render: (item) => `${item.attempt_count} / ${item.max_attempts}`,
              },
              { key: 'last_error_code', label: copy.tokens.lastError },
              {
                key: 'updated_at',
                label: copy.billing.updated,
                render: (item) => formatDate(item.updated_at),
              },
              {
                key: 'actions',
                label: copy.accounts.actions,
                render: (item) =>
                  canManageTokens ? (
                    <button disabled={busy || !['FAILED', 'REPAIR_REQUIRED'].includes(item.status)} onClick={() => repairProvisioning(item)}>
                      {copy.tokens.repair}
                    </button>
                  ) : (
                    '—'
                  ),
              },
            ];
  return (
    <section className="adm-panel">
      <SectionHeader title={copy.tokens.title} count={items.length} copy={copy}>
        {tab === 'models' && (canSyncModels || canManageModels) && (
          <span className="adm-row-actions">
            <button disabled={busy || !canSyncModels} onClick={syncModels}>
              {copy.tokens.syncCatalog}
            </button>
            <button disabled={busy || !canManageModels} onClick={rollbackCatalog}>
              {copy.tokens.rollbackCatalog}
            </button>
          </span>
        )}
        {tab === 'provisionings' && (
          <button disabled={busy} onClick={() => void perform(refreshProvisionings)}>
            {copy.refresh}
          </button>
        )}
      </SectionHeader>
      <Tabs
        active={tab}
        onChange={setTab}
        items={visibleTabs.map((key) => ({
          key,
          label: key === 'tokenAccounts' ? copy.tokens.accounts : key === 'models' ? copy.tokens.modelCatalog : copy.tokens[key],
        }))}
      />
      {tab === 'tokenAccounts' && canManageTokens && (
        <form className="adm-ops-form" onSubmit={createTokenAccount}>
          <select
            value={newAccount.owner_type}
            onChange={(event) =>
              setNewAccount((current) => ({
                ...current,
                owner_type: event.target.value,
              }))
            }
          >
            <option value="ORG">{copy.tokens.organizationScope}</option>
            <option value="USER">{copy.tokens.personalScope}</option>
          </select>
          <input
            placeholder={copy.tokens.ownerID}
            value={newAccount.owner_id}
            onChange={(event) =>
              setNewAccount((current) => ({
                ...current,
                owner_id: event.target.value,
              }))
            }
          />
          <input
            placeholder={copy.tokens.accountName}
            value={newAccount.name}
            onChange={(event) =>
              setNewAccount((current) => ({
                ...current,
                name: event.target.value,
              }))
            }
          />
          <input
            type="number"
            min="1"
            max="10000"
            value={newAccount.default_priority}
            onChange={(event) =>
              setNewAccount((current) => ({
                ...current,
                default_priority: event.target.value,
              }))
            }
          />
          <button disabled={busy || !newAccount.owner_id.trim() || !newAccount.name.trim()}>{copy.tokens.createAccount}</button>
        </form>
      )}
      {notice && <div className="adm-inline-message ok">{notice}</div>}
      {error && <div className="adm-inline-message error">{error}</div>}
      <Table columns={columns} items={items} copy={copy} />
    </section>
  );
}

function TokenAccountDetail({ copy, account, route, setRoute, busy, notice, error, onBack, onStatus, onSaveRoute, onDeleteRoute, onValidate, canManage, canValidate }) {
  const credentialColumns = [
    { key: 'provider', label: copy.tokens.provider },
    {
      key: 'status',
      label: copy.accounts.status,
      render: (item) => <Status value={item.status} />,
    },
    {
      key: 'validation_status',
      label: copy.tokens.validation,
      render: (item) => <Status value={item.validation_status} />,
    },
    {
      key: 'fingerprint',
      label: copy.tokens.fingerprint,
      render: (item) => <code>{item.fingerprint}</code>,
    },
    {
      key: 'allowed_models',
      label: copy.tokens.models,
      render: (item) => item.allowed_models.join(' · '),
    },
    {
      key: 'updated_at',
      label: copy.billing.updated,
      render: (item) => formatDate(item.updated_at),
    },
    {
      key: 'actions',
      label: copy.accounts.actions,
      render: (item) =>
        canValidate ? (
          <button disabled={busy || item.status !== 'ACTIVE'} onClick={() => onValidate(item.credential_id)}>
            {copy.tokens.validate}
          </button>
        ) : (
          '—'
        ),
    },
  ];
  const routeColumns = [
    {
      key: 'model',
      label: copy.tokens.model,
      render: (item) => <strong>{item.model}</strong>,
    },
    { key: 'protocol_code', label: copy.tokens.protocol },
    { key: 'priority', label: copy.tokens.priority },
    { key: 'default_scope', label: copy.tokens.defaultScope },
    {
      key: 'health_status',
      label: copy.tokens.health,
      render: (item) => <Status value={item.health_status} />,
    },
    {
      key: 'updated_at',
      label: copy.billing.updated,
      render: (item) => formatDate(item.updated_at),
    },
    {
      key: 'actions',
      label: copy.accounts.actions,
      render: (item) =>
        canManage ? (
          <button className="danger" disabled={busy} onClick={() => onDeleteRoute(item.model)}>
            {copy.accounts.delete}
          </button>
        ) : (
          '—'
        ),
    },
  ];
  return (
    <section className="adm-panel">
      <button className="adm-back" onClick={onBack}>
        ← {copy.tokens.back}
      </button>
      <SectionHeader title={`${copy.tokens.accountDetail} · ${account.name}`} copy={copy}>
        {canManage && (
          <button disabled={busy || account.status === 'FROZEN'} onClick={onStatus}>
            {account.status === 'ACTIVE' ? copy.accounts.disable : copy.accounts.enable}
          </button>
        )}
      </SectionHeader>
      {notice && <div className="adm-inline-message ok">{notice}</div>}
      {error && <div className="adm-inline-message error">{error}</div>}
      <h3>{copy.tokens.credentialMetadata}</h3>
      <p className="adm-help">{copy.tokens.credentialSafety}</p>
      <Table columns={credentialColumns} items={account.credentials} copy={copy} />
      <h3>{copy.tokens.routes}</h3>
      {canManage && (
        <form className="adm-ops-form" onSubmit={onSaveRoute}>
          <input placeholder={copy.tokens.model} value={route.model} onChange={(event) => setRoute((current) => ({ ...current, model: event.target.value }))} />
          <select
            value={route.protocol_code}
            onChange={(event) =>
              setRoute((current) => ({
                ...current,
                protocol_code: event.target.value,
              }))
            }
          >
            <option>CHAT</option>
          </select>
          <input
            type="number"
            min="0"
            max="10000"
            value={route.priority}
            onChange={(event) =>
              setRoute((current) => ({
                ...current,
                priority: event.target.value,
              }))
            }
          />
          <select
            value={route.default_scope}
            onChange={(event) =>
              setRoute((current) => ({
                ...current,
                default_scope: event.target.value,
              }))
            }
          >
            <option>NONE</option>
            <option>UNIT</option>
            <option>ORG</option>
          </select>
          <select
            value={route.health_status}
            onChange={(event) =>
              setRoute((current) => ({
                ...current,
                health_status: event.target.value,
              }))
            }
          >
            <option>HEALTHY</option>
            <option>DEGRADED</option>
            <option>UNHEALTHY</option>
          </select>
          <button disabled={busy || !route.model.trim()}>{copy.tokens.saveRoute}</button>
        </form>
      )}
      <Table columns={routeColumns} items={account.routes} copy={copy} />
    </section>
  );
}

function UsageRequestDetail({ copy, language, item, busy, notice, error, onBack, onRetry, canRetry }) {
  const rows = [
    [copy.tokens.model, item.model],
    [copy.accounts.status, <Status value={item.status} />],
    [copy.tokens.executionStatus, <Status value={item.execution_status} />],
    [copy.tokens.deliveryStatus, <Status value={item.delivery_status} />],
    [copy.tokens.cancelStatus, <Status value={item.cancel_status} />],
    [copy.tokens.settlementStatus, <Status value={item.settlement_status} />],
    [copy.tokens.maximum, formatPoints(item.maximum_microcredits, language)],
    [copy.tokens.actual, formatPoints(item.actual_microcredits, language)],
    [copy.tokens.provider, item.provider_id || '—'],
    [copy.tokens.adapterVersion, item.adapter_version || '—'],
    [copy.tokens.upstreamReference, <code>{item.upstream_request_id || '—'}</code>],
    [copy.tokens.attemptStatus, item.attempt_status || '—'],
    [copy.tokens.tokens, `${formatNumber(item.input_tokens)} / ${formatNumber(item.output_tokens)} / ${formatNumber(item.cached_input_tokens)}`],
    [copy.tokens.persistedSequence, formatNumber(item.last_persisted_sequence)],
    [copy.tokens.confirmedSequence, formatNumber(item.last_client_confirmed_sequence)],
    [copy.tokens.reconciliationAttempts, formatNumber(item.reconciliation_attempts)],
    [copy.tokens.lastError, item.reconciliation_last_error || item.error_class || '—'],
    [copy.tokens.cancellationEvidence, <pre className="adm-json-detail">{JSON.stringify(item.cancellation_evidence || [], null, 2)}</pre>],
    [copy.tokens.started, formatDate(item.started_at)],
    [copy.billing.updated, formatDate(item.updated_at)],
  ];
  const retryAvailable = ['SETTLEMENT_PENDING', 'REVIEW'].includes(item.status);
  return (
    <section className="adm-panel">
      <button className="adm-back" onClick={onBack}>
        ← {copy.tokens.back}
      </button>
      <SectionHeader title={`${copy.tokens.usageDetail} · ${shortID(item.usage_request_id)}`} copy={copy}>
        {canRetry && retryAvailable && (
          <button disabled={busy} onClick={onRetry}>
            {copy.tokens.retry}
          </button>
        )}
      </SectionHeader>
      {notice && <div className="adm-inline-message ok">{notice}</div>}
      {error && <div className="adm-inline-message error">{error}</div>}
      <dl className="adm-detail-list">
        {rows.map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

function SystemStatus({ copy, data }) {
  const status = data.systemStatus || {
    services: [],
    relays: [],
    reconciliation: {},
    recent_errors: [],
  };
  const serviceColumns = [
    {
      key: 'key',
      label: copy.system.service,
      render: (item) => <strong>{copy.system.names[item.key] || item.key}</strong>,
    },
    {
      key: 'status',
      label: copy.accounts.status,
      render: (item) => <Status value={item.status} />,
    },
    {
      key: 'latency_ms',
      label: copy.system.latency,
      render: (item) => `${formatNumber(item.latency_ms)} ms`,
    },
    { key: 'message', label: copy.system.message },
  ];
  const relayColumns = [
    {
      key: 'key',
      label: copy.system.service,
      render: (item) => <strong>{copy.system.names[item.key] || item.key}</strong>,
    },
    {
      key: 'status',
      label: copy.accounts.status,
      render: (item) => <Status value={item.status} />,
    },
    { key: 'pending_events', label: copy.system.pending },
    { key: 'dead_lettered_events', label: copy.system.deadLetters },
    {
      key: 'oldest_pending_seconds',
      label: copy.system.oldest,
      render: (item) => `${formatNumber(item.oldest_pending_seconds)} s`,
    },
    { key: 'message', label: copy.system.message },
  ];
  const errorColumns = [
    {
      key: 'source',
      label: copy.system.source,
      render: (item) => <strong>{copy.system.names[item.source] || item.source}</strong>,
    },
    {
      key: 'resource',
      label: copy.system.resource,
      render: (item) => <code title={item.resource}>{shortID(item.resource)}</code>,
    },
    { key: 'message', label: copy.system.message },
    {
      key: 'updated_at',
      label: copy.billing.updated,
      render: (item) => formatDate(item.updated_at),
    },
  ];
  return (
    <section>
      <div className="adm-metrics">
        <Metric label={copy.system.healthyServices} value={status.services.filter((item) => item.status === 'HEALTHY').length} />
        <Metric label={copy.system.pendingReconciliation} value={status.reconciliation.pending} />
        <Metric label={copy.system.stoppedReconciliation} value={status.reconciliation.stopped} tone={Number(status.reconciliation.stopped) ? 'accent' : ''} />
      </div>
      <article className="adm-panel">
        <SectionHeader title={copy.system.services} count={status.services.length} copy={copy} />
        <Table columns={serviceColumns} items={status.services} copy={copy} />
        <h3>{copy.system.relays}</h3>
        <Table columns={relayColumns} items={status.relays} copy={copy} />
        <h3>{copy.system.recentErrors}</h3>
        <Table columns={errorColumns} items={status.recent_errors} copy={copy} />
      </article>
    </section>
  );
}

function IdentitySecurity({ copy, closures, client, setData, permissions }) {
  const [copied, setCopied] = useState('');
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const canRepairClosures = (permissions || []).includes('platform.account-closures.repair');
  const commands = {
    ssh: 'ssh -N -T -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -L 18081:127.0.0.1:18080 <ssh-user>@<server>',
    kubernetes: 'kubectl -n <namespace> port-forward service/<release>-account-platform-keycloak 18081:8080 --address 127.0.0.1',
  };
  const copyCommand = async (key) => {
    try {
      await navigator.clipboard.writeText(commands[key]);
      setCopied(key);
    } catch {
      setCopied('');
    }
  };
  const retryClosure = async (item) => {
    const reason = window.prompt(copy.closure.retryReason);
    if (!reason?.trim()) return;
    setBusy(item.closure_request_id);
    setError('');
    try {
      const updated = await client.retryAccountClosure(item.closure_request_id, reason.trim());
      setData((current) => ({
        ...current,
        accountClosures: (current.accountClosures || []).map((candidate) => (candidate.closure_request_id === updated.closure_request_id ? updated : candidate)),
      }));
    } catch {
      setError(copy.closure.retryFailed);
    } finally {
      setBusy('');
    }
  };
  const closureColumns = [
    {
      key: 'closure_request_id',
      label: copy.closure.request,
      render: (item) => <code title={item.closure_request_id}>{shortID(item.closure_request_id)}</code>,
    },
    {
      key: 'user_id',
      label: copy.closure.user,
      render: (item) => <code title={item.user_id}>{shortID(item.user_id)}</code>,
    },
    {
      key: 'stage',
      label: copy.closure.stage,
      render: (item) => <Status value={item.stage} />,
    },
    {
      key: 'status',
      label: copy.accounts.status,
      render: (item) => <Status value={item.status} />,
    },
    {
      key: 'blockers',
      label: copy.closure.blockers,
      render: (item) => (item.blockers || []).map((blocker) => blocker.code).join(' · ') || copy.closure.none,
    },
    {
      key: 'updated_at',
      label: copy.billing.updated,
      render: (item) => formatDate(item.updated_at),
    },
    {
      key: 'actions',
      label: copy.accounts.actions,
      render: (item) =>
        canRepairClosures ? (
          <button disabled={Boolean(busy)} onClick={() => retryClosure(item)}>
            {busy === item.closure_request_id ? '…' : copy.closure.retry}
          </button>
        ) : (
          '—'
        ),
    },
  ];
  return (
    <section className="adm-security-grid">
      <article className="adm-panel adm-security-intro">
        <span className="adm-eyebrow">KEYCLOAK</span>
        <h2>{copy.identity.title}</h2>
        <p>{copy.identity.lead}</p>
        <div className="adm-security-warning">
          <strong>{copy.identity.publicBlocked}</strong>
          <span>{copy.identity.publicBlockedHelp}</span>
        </div>
        <a className="adm-security-open" href="http://127.0.0.1:18081/admin/" target="_blank" rel="noreferrer">
          {copy.identity.openLocal}
        </a>
        <small>{copy.identity.openHelp}</small>
      </article>
      <article className="adm-panel">
        <SectionHeader title={copy.identity.accessMethods} copy={copy} />
        <SecureCommand title={copy.identity.ssh} help={copy.identity.sshHelp} command={commands.ssh} copied={copied === 'ssh'} copy={copy} onCopy={() => copyCommand('ssh')} />
        <SecureCommand title={copy.identity.kubernetes} help={copy.identity.kubernetesHelp} command={commands.kubernetes} copied={copied === 'kubernetes'} copy={copy} onCopy={() => copyCommand('kubernetes')} />
        <ol className="adm-security-steps">
          <li>{copy.identity.stepTunnel}</li>
          <li>{copy.identity.stepOpen}</li>
          <li>{copy.identity.stepClose}</li>
        </ol>
      </article>
      <article className="adm-panel" style={{ gridColumn: '1 / -1' }}>
        <SectionHeader title={copy.closure.title} count={closures.length} copy={copy} />
        <p>{copy.closure.lead}</p>
        {error && <div className="adm-inline-message error">{error}</div>}
        <Table columns={closureColumns} items={closures} copy={copy} />
      </article>
    </section>
  );
}

function SecureCommand({ title, help, command, copied, copy, onCopy }) {
  return (
    <div className="adm-secure-command">
      <div>
        <strong>{title}</strong>
        <span>{help}</span>
      </div>
      <code>{command}</code>
      <button type="button" onClick={onCopy}>
        {copied ? copy.identity.copied : copy.identity.copy}
      </button>
    </div>
  );
}

function Audit({ copy, data, client, setData, permissions }) {
  const [filters, setFilters] = useState({
    actor: '',
    action: '',
    resource: '',
    result: '',
    from: '',
    to: '',
  });
  const [detail, setDetail] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [includePII, setIncludePII] = useState(false);
  const [exportJob, setExportJob] = useState(null);
  const [exportKey, setExportKey] = useState('');
  const canExport = (permissions || []).includes('platform.audit.export');
  const canExportPII = (permissions || []).includes('platform.audit.pii.export');
  const items = data.audits || [];
  const queryFilters = () => ({
    ...filters,
    from: filters.from ? new Date(filters.from).toISOString() : '',
    to: filters.to ? new Date(filters.to).toISOString() : '',
  });
  const search = async (event) => {
    event?.preventDefault();
    setBusy(true);
    setError('');
    try {
      const result = await client.audits(queryFilters());
      setData((current) => ({ ...current, audits: result.items || [] }));
    } catch {
      setError(copy.audit.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const showDetail = async (item) => {
    setBusy(true);
    setError('');
    try {
      setDetail(await client.audit(item.audit_id));
    } catch {
      setError(copy.audit.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const createExport = async () => {
    const filter = queryFilters();
    if (!filter.from || !filter.to) {
      setError(copy.audit.exportRangeRequired);
      return;
    }
    const reason = window.prompt(copy.audit.exportReason);
    if (!reason) return;
    setBusy(true);
    setError('');
    try {
      const job = await client.createAuditExport(filter, reason, includePII);
      setExportJob(job);
      setExportKey(job.decryption_key || '');
    } catch {
      setError(copy.audit.exportFailed);
    } finally {
      setBusy(false);
    }
  };
  const refreshExport = async () => {
    if (!exportJob) return;
    setBusy(true);
    setError('');
    try {
      setExportJob(await client.auditExport(exportJob.export_id));
    } catch {
      setError(copy.audit.exportFailed);
    } finally {
      setBusy(false);
    }
  };
  const downloadExport = async () => {
    setBusy(true);
    setError('');
    try {
      const ticket = await client.issueAuditExportDownloadTicket(exportJob.export_id);
      const blob = await client.downloadAuditExport(exportJob.export_id, ticket.download_token);
      const href = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = href;
      link.download = exportJob.filename || `pinvou-audit-${exportJob.export_id}.pae`;
      link.click();
      URL.revokeObjectURL(href);
      setExportJob((current) => ({ ...current, status: 'DOWNLOADED' }));
    } catch {
      setError(copy.audit.exportFailed);
    } finally {
      setBusy(false);
    }
  };
  if (detail) return <AuditDetail copy={copy} item={detail} onBack={() => setDetail(null)} />;
  const columns = [
    {
      key: 'action',
      label: copy.audit.action,
      render: (item) => (
        <button className="adm-name-link" onClick={() => showDetail(item)}>
          {item.action}
        </button>
      ),
    },
    {
      key: 'resource_type',
      label: copy.audit.resource,
      render: (item) => (
        <span>
          {item.resource_type}
          <br />
          <code title={item.resource_id}>{shortID(item.resource_id)}</code>
        </span>
      ),
    },
    {
      key: 'actor_user_id',
      label: copy.audit.actor,
      render: (item) => (
        <span>
          {item.actor_display_name || item.actor_email || '—'}
          <br />
          <code title={item.actor_user_id}>{shortID(item.actor_user_id)}</code>
        </span>
      ),
    },
    {
      key: 'result',
      label: copy.audit.result,
      render: (item) => <Status value={item.result} />,
    },
    {
      key: 'request_id',
      label: copy.audit.request,
      render: (item) => <code title={item.request_id}>{shortID(item.request_id)}</code>,
    },
    {
      key: 'occurred_at',
      label: copy.audit.occurred,
      render: (item) => formatDate(item.occurred_at),
    },
  ];
  return (
    <section className="adm-panel">
      <SectionHeader title={copy.audit.title} count={items.length} copy={copy}>
        <button disabled={busy || !canExport} title={!canExport ? copy.permissionDenied : undefined} onClick={createExport}>
          {copy.audit.export}
        </button>
      </SectionHeader>
      <form className="adm-ops-form" onSubmit={search}>
        <input placeholder={copy.audit.actorFilter} value={filters.actor} onChange={(event) => setFilters((current) => ({ ...current, actor: event.target.value }))} />
        <input
          placeholder={copy.audit.actionFilter}
          value={filters.action}
          onChange={(event) =>
            setFilters((current) => ({
              ...current,
              action: event.target.value,
            }))
          }
        />
        <input
          placeholder={copy.audit.resourceFilter}
          value={filters.resource}
          onChange={(event) =>
            setFilters((current) => ({
              ...current,
              resource: event.target.value,
            }))
          }
        />
        <select
          value={filters.result}
          onChange={(event) =>
            setFilters((current) => ({
              ...current,
              result: event.target.value,
            }))
          }
        >
          <option value="">{copy.audit.allResults}</option>
          <option>SUCCEEDED</option>
          <option>DENIED</option>
          <option>FAILED</option>
        </select>
        <label className="adm-filter-date">
          <span>{copy.audit.from}</span>
          <input
            type="datetime-local"
            value={filters.from}
            onChange={(event) =>
              setFilters((current) => ({
                ...current,
                from: event.target.value,
              }))
            }
          />
        </label>
        <label className="adm-filter-date">
          <span>{copy.audit.to}</span>
          <input type="datetime-local" value={filters.to} onChange={(event) => setFilters((current) => ({ ...current, to: event.target.value }))} />
        </label>
        <label>
          <input type="checkbox" checked={includePII} disabled={!canExportPII} title={!canExportPII ? copy.permissionDenied : undefined} onChange={(event) => setIncludePII(event.target.checked)} />
          {copy.audit.includePII}
        </label>
        <button disabled={busy}>{copy.searchAction}</button>
      </form>
      <p>{copy.audit.exportLimits}</p>
      {exportJob && (
        <div className="adm-inline-message">
          <strong>
            {copy.audit.exportStatus}: {exportJob.status}
          </strong>
          {exportKey && (
            <>
              <br />
              <span>{copy.audit.saveKey}</span>
              <br />
              <code>{exportKey}</code>
            </>
          )}
          <br />
          <button disabled={busy} onClick={refreshExport}>
            {copy.refresh}
          </button>
          {exportJob.status === 'READY' && (
            <button disabled={busy} onClick={downloadExport}>
              {copy.audit.download}
            </button>
          )}
        </div>
      )}
      {error && <div className="adm-inline-message error">{error}</div>}
      <Table columns={columns} items={items} copy={copy} />
    </section>
  );
}

function AuditDetail({ copy, item, onBack }) {
  const rows = [
    [copy.audit.source, 'Account Service'],
    [copy.audit.actor, item.actor_display_name || item.actor_email || item.actor_user_id || '—'],
    [copy.audit.action, item.action],
    [copy.audit.resource, `${item.resource_type} · ${item.resource_id}`],
    [copy.audit.result, <Status value={item.result} />],
    [copy.audit.request, item.request_id],
    [copy.audit.organization, item.active_organization_id || '—'],
    [copy.audit.occurred, formatDate(item.occurred_at)],
  ];
  return (
    <section className="adm-panel">
      <button className="adm-back" onClick={onBack}>
        ← {copy.audit.back}
      </button>
      <SectionHeader title={copy.audit.detail} copy={copy} />
      <dl className="adm-detail-list">
        {rows.map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <h3>{copy.audit.changeSummary}</h3>
      <pre className="adm-json-detail">{JSON.stringify(item.change_summary || {}, null, 2)}</pre>
    </section>
  );
}
