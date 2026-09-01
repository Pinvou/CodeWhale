import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { buildUnitTree } from './admin-organization-tree.js';
import { formatPoints, formatUsagePoints, MICROCREDITS_PER_POINT } from './credit-units.js';
import { adminSectionReadOnly, adminVisibleSections, authorizationVersionChanged, authorizationVersions, hasPermission } from './authorization-ui.js';
import { adminNavigationGroups, readAdminOrganizationDirectory, readAdminOrganizationRoute, readAdminSection, writeAdminSection } from './admin-navigation.js';
import { AdminRolesWorkspace } from './AdminRolesWorkspace.jsx';
import { adminPlatformCopy } from '../../shared/admin-platform-i18n.js';
import { localeNames, supportedLocales } from '../../shared/supported-locales.js';
import './admin-platform.css';

const NAV_ICONS = {
  overview: '⌂',
  organizations: '▦',
  users: '◎',
  roles: '◆',
  billing: '◇',
  payments: '¤',
  products: '□',
  tokens: '↗',
  approvals: '✓',
  system: '◉',
  audit: '≡',
  settings: '⚙',
};
const okStatuses = new Set(['ACTIVE', 'PAID', 'SETTLED', 'HEALTHY', 'APPROVED', 'SUCCESS']);
const warnStatuses = new Set(['PENDING', 'CREATED', 'SETTLEMENT_PENDING', 'REVIEW', 'MANUAL_REVIEW', 'PENDING_APPROVAL', 'REPAIR_REQUIRED', 'RETRY', 'COMPENSATION_PENDING', 'BLOCKED']);

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
  return user?.username || user?.display_name || shortID(user?.user_id);
}

function readAuditQueryFilters(locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  return {
    actor: '',
    action: search.get('audit_action') || '',
    resource: search.get('audit_resource') || '',
    result: '',
    from: '',
    to: '',
  };
}

function Status({ value }) {
  const status = String(value || '—');
  const tone = okStatuses.has(status) ? 'ok' : warnStatuses.has(status) ? 'warn' : status === '—' ? '' : 'neutral';
  return <span className={`adm-status ${tone}`} aria-label={status}>{status}</span>;
}

function TechnicalID({ value, copy, compact = true }) {
  const [copied, setCopied] = useState(false);
  const text = String(value || '');
  if (!text) return '—';
  const copyValue = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setCopied(false);
    }
  };
  return (
    <span className="adm-technical-id">
      <code title={text}>{compact ? shortID(text) : text}</code>
      <button type="button" onClick={copyValue} aria-label={`${copy.copyIdentifier}: ${text}`}>
        {copied ? copy.copiedIdentifier : copy.copyIdentifier}
      </button>
    </span>
  );
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
  return <div className="adm-empty" role="status">{copy.noData}</div>;
}

function Table({ columns, items, copy }) {
  if (!items?.length) return <Empty copy={copy} />;
  return (
    <div className="adm-table-wrap" tabIndex="0">
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
            <tr key={item.registration_operation_id || item.profile_change_request_id || item.closure_request_id || item.user_id || item.organization_id || item.model_id || item.provisioning_id || item.receivable_id || item.wallet_id || item.order_id || item.operation_id || item.package_id || item.adjustment_id || item.hold_id || item.case_id || item.event_id || item.evidence_id || item.token_account_id || item.usage_request_id || item.audit_id || index}>
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

function PageHeader({ eyebrow, title, subtitle, updatedAt, canRefresh, busy, copy, onRefresh }) {
  return (
    <header className="adm-page-head">
      <div>
        <span className="adm-eyebrow">{eyebrow}</span>
        <h2>{title}</h2>
        <p>{subtitle}</p>
      </div>
      <div className="adm-page-head-actions">
        {updatedAt && <small>{copy.updated} {updatedAt.toLocaleTimeString()}</small>}
        {canRefresh && <button type="button" onClick={onRefresh} disabled={busy}>{copy.refresh}</button>}
      </div>
    </header>
  );
}

function DetailDrawer({ label, onClose, children }) {
  const drawerRef = useRef(null);
  useEffect(() => {
    const previousFocus = document.activeElement;
    drawerRef.current?.focus();
    const onKeyDown = (event) => {
      if (event.key === 'Escape') onClose();
    };
    globalThis.addEventListener?.('keydown', onKeyDown);
    return () => {
      globalThis.removeEventListener?.('keydown', onKeyDown);
      previousFocus?.focus?.();
    };
  }, [onClose]);
  const trapFocus = (event) => {
    if (event.key !== 'Tab') return;
    const focusable = [...(drawerRef.current?.querySelectorAll('button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])') || [])];
    if (!focusable.length) {
      event.preventDefault();
      drawerRef.current?.focus();
      return;
    }
    const first = focusable[0];
    const last = focusable.at(-1);
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };
  return (
    <div className="adm-drawer-backdrop" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <aside ref={drawerRef} className="adm-detail-drawer" role="dialog" aria-modal="true" aria-label={label} tabIndex="-1" onKeyDown={trapFocus}>
        {children}
      </aside>
    </div>
  );
}

function Tabs({ items, active, onChange }) {
  const receivablesLabel = Object.values(adminPlatformCopy).find((candidate) => candidate.billing.wallets === items[0]?.label)?.billing.receivables;
  const visibleItems = items.some((item) => item.key === 'wallets') && !items.some((item) => item.key === 'receivables') ? [items[0], { key: 'receivables', label: receivablesLabel }, ...items.slice(1)] : items;
  return (
    <div className="adm-tabs" role="tablist">
      {visibleItems.map((item) => (
        <button key={item.key} type="button" role="tab" aria-selected={active === item.key} className={active === item.key ? 'active' : ''} onClick={() => onChange(item.key)}>
          {item.label}
        </button>
      ))}
    </div>
  );
}

function AdminExtensionSlot({ section, authorization }) {
  const hostRef = useRef(null);
  const platformPermissions = new Set(authorization?.platform_permissions || []);
  const visible = {
    'approval-requests-root': section === 'approvals',
    'product-admin-root': section === 'products' && platformPermissions.has('platform.products.read'),
    'pricing-admin-root': false,
  };

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return undefined;
    const nodes = Object.keys(visible).map((id) => document.getElementById(id)).filter(Boolean);
    for (const node of nodes) host.append(node);
    return () => {
      for (const node of nodes) document.body.append(node);
    };
  }, []);

  useEffect(() => {
    for (const [id, isVisible] of Object.entries(visible)) {
      const node = document.getElementById(id);
      if (node) node.hidden = !isVisible;
    }
  }, [section, authorization]);

  return <div ref={hostRef} className="adm-extension-stack" />;
}

export function AdminPlatformView({ client, copy, language, onLanguageChange }) {
  const [auth, setAuth] = useState('loading');
  const [section, setSection] = useState('overview');
  const [authorization, setAuthorization] = useState(null);
  const [data, setData] = useState({});
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [updatedAt, setUpdatedAt] = useState(null);
  const [routeVersion, setRouteVersion] = useState(0);
  const authorizationRef = useRef(null);
  const authorizationRefreshRef = useRef(false);

  const load = useCallback(
    async (target, signal, granted = authorizationRef.current) => {
      setBusy(true);
      setError('');
      try {
        const allowed = (permission) => hasPermission(granted, permission);
        const read = (permission, action, fallback = { items: [] }) => (allowed(permission) ? action() : Promise.resolve(fallback));
        const readAny = (permissions, action, fallback = { items: [] }) => (permissions.some(allowed) ? action() : Promise.resolve(fallback));
        const readOptional = async (permission, action, fallback = { items: [] }) => {
          if (!allowed(permission)) return fallback;
          try {
            return await action();
          } catch (reason) {
            if (reason?.name === 'AbortError' || reason?.status === 401 || reason?.status === 403) throw reason;
            return { ...fallback, unavailable: true };
          }
        };
        let next = {};
        if (target === 'overview') {
          const sources = [
            ['identity', ['platform.users.read', 'platform.organizations.read'].some(allowed), () => client.accountOverview(signal)],
            ['finance', allowed('platform.billing.wallets.read'), () => client.billingOverview(signal)],
            ['modelToken', ['platform.tokens.read', 'platform.usage.read'].some(allowed), () => client.tokenOverview(signal)],
            ['system', allowed('platform.system.status.read'), () => client.systemStatus(signal)],
          ];
          const results = await Promise.allSettled(sources.map(([, enabled, action]) => (enabled ? action() : Promise.resolve(null))));
          const overviewAvailability = {};
          const values = {};
          results.forEach((result, index) => {
            const [key, enabled] = sources[index];
            if (!enabled) {
              overviewAvailability[key] = 'hidden';
              values[key] = null;
            } else if (result.status === 'fulfilled') {
              overviewAvailability[key] = 'ready';
              values[key] = result.value;
            } else {
              if (result.reason?.name === 'AbortError' || result.reason?.status === 401) throw result.reason;
              overviewAvailability[key] = 'error';
              values[key] = null;
            }
          });
          next = {
            accountOverview: values.identity || {},
            billingOverview: values.finance || {},
            tokenOverview: values.modelToken || {},
            systemStatus: values.system,
            overviewAvailability,
          };
        } else if (target === 'organizations') {
          const organizationDirectory = readAdminOrganizationDirectory();
          const [users, organizations, roles] = await Promise.all([read('platform.users.read', () => client.users('', signal)), read('platform.organizations.read', () => client.organizations(organizationDirectory, signal)), read('platform.roles.read', () => client.roles(signal))]);
          next = {
            users: users.items || [],
            organizations: organizations.items || [],
            organizationNextCursor: organizations.next_cursor || '',
            roles: roles.items || [],
          };
        } else if (target === 'users') {
          const [users, overview, roles, registrations, profileChanges, closures] = await Promise.all([
            read('platform.users.read', () => client.users('', signal)),
            read('platform.users.read', () => client.accountOverview(signal), {}),
            read('platform.roles.read', () => client.roles(signal)),
            readOptional('platform.registrations.repair', () => client.registrationRepairs({}, signal)),
            readOptional('platform.profile-changes.repair', () => client.profileChangeRepairs(signal)),
            readOptional('platform.account-closures.read', () => client.accountClosures(signal)),
          ]);
          next = {
            users: users.items || [],
            userNextCursor: users.next_cursor || '',
            usersLoadVersion: Date.now(),
            accountOverview: overview,
            roles: roles.items || [],
            registrationRepairs: registrations.items || [],
            profileChangeRepairs: profileChanges.items || [],
            accountClosures: closures.items || [],
            userRepairAvailability: {
              registrations: registrations.unavailable ? 'error' : 'ready',
              profileChanges: profileChanges.unavailable ? 'error' : 'ready',
              closures: closures.unavailable ? 'error' : 'ready',
            },
          };
        } else if (target === 'roles') {
          const [roles, bindings, users, organizations] = await Promise.all([
            client.roles(signal),
            client.roleBindings({}, signal),
            read('platform.users.read', () => client.users('', signal)),
            read('platform.organizations.read', () => client.organizations('', signal)),
          ]);
          next = {
            roles: roles.items || [],
            roleBindings: bindings.items || [],
            roleUsers: users.items || [],
            roleOrganizations: organizations.items || [],
          };
        } else if (target === 'billing') {
          const [overview, wallets, receivables, prices, adjustments, holds, reconciliations] = await Promise.all([read('platform.billing.wallets.read', () => client.billingOverview(signal), {}), read('platform.billing.wallets.read', () => client.wallets(signal)), read('platform.billing.wallets.read', () => client.receivables(signal)), read('platform.pricing.read', () => client.priceVersions(signal)), read('platform.billing.wallets.read', () => client.adjustments(signal)), read('platform.usage.read', () => client.holds(signal)), read('platform.usage.read', () => client.reconciliationRuns(signal))]);
          next = {
            billingOverview: overview,
            wallets: wallets.items || [],
            receivables: receivables.items || [],
            prices: prices.items || [],
            adjustments: adjustments.items || [],
            holds: holds.items || [],
            reconciliations: reconciliations.items || [],
          };
        } else if (target === 'payments') {
          const [overview, orders, refunds, lateReviews, anomalies] = await Promise.all([client.billingOverview(signal), client.paymentOrders({}, signal), client.refunds(signal), client.latePaymentReviews({}, signal), client.paymentAnomalies({}, signal)]);
          next = {
            billingOverview: overview,
            orders: orders.items || [],
            refunds: refunds.items || [],
            lateReviews: lateReviews.items || [],
            anomalies: anomalies.items || [],
          };
        } else if (target === 'products' || target === 'approvals' || target === 'settings') {
          next = {};
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
          next = { systemStatus: await read('platform.system.status.read', () => client.systemStatus(signal), null) };
        } else if (target === 'audit') {
          const audits = await client.audits(readAuditQueryFilters(), signal);
          next = { audits: audits.items || [] };
        } else {
          return;
        }
        setData((current) => ({ ...current, ...next, loadedSections: { ...(current.loadedSections || {}), [target]: true } }));
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
        const requested = readAdminSection();
        const target = visible.includes(requested) ? requested : visible[0];
        setSection(target);
        writeAdminSection(target, 'replace');
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
    let retryTimer = null;
    const refreshAuthorization = async (isRetry = false) => {
      if (authorizationRefreshRef.current) return;
      authorizationRefreshRef.current = true;
      try {
        const next = await client.authorization();
        if (disposed) return;
        if (retryTimer !== null) {
          globalThis.clearTimeout?.(retryTimer);
          retryTimer = null;
        }
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
          writeAdminSection(target, 'replace');
          await load(target, undefined, next);
        }
      } catch (reason) {
        if (!disposed && reason?.name !== 'AbortError') {
          if (reason?.status === 401) setAuth('guest');
          else if (reason?.status === 403) setAuth('denied');
          else if (!isRetry && retryTimer === null) {
            retryTimer = globalThis.setTimeout?.(() => {
              retryTimer = null;
              void refreshAuthorization(true);
            }, 1500);
          } else {
            console.warn('[admin-platform] background authorization refresh failed', {
              name: reason?.name || 'Error',
              code: reason?.code || '',
              status: reason?.status || 0,
            });
          }
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
      if (retryTimer !== null) globalThis.clearTimeout?.(retryTimer);
      unsubscribe?.();
      globalThis.removeEventListener?.('focus', onFocus);
    };
  }, [auth, client, load, section]);

  useEffect(() => {
    if (auth !== 'ready' || !authorization) return undefined;
    const onPopState = () => {
      const requested = readAdminSection();
      const visible = adminVisibleSections(authorization);
      if (!visible.includes(requested)) {
        writeAdminSection(section, 'replace');
        return;
      }
      if (requested === section) {
        setRouteVersion((value) => value + 1);
        return;
      }
      setSection(requested);
      setRouteVersion((value) => value + 1);
      if (!sectionHasData(requested, data)) void load(requested, undefined, authorization);
    };
    globalThis.addEventListener?.('popstate', onPopState);
    return () => globalThis.removeEventListener?.('popstate', onPopState);
  }, [auth, authorization, data, load, section]);

  const navigate = (key, query = {}) => {
    if (!adminVisibleSections(authorization).includes(key)) return;
    setSection(key);
    writeAdminSection(key, 'push', undefined, undefined, {
      role: null,
      role_scope: null,
      role_view: null,
      audit_action: null,
      audit_resource: null,
      organization_id: null,
      organization_tab: null,
      ...query,
    });
    setRouteVersion((value) => value + 1);
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
  const visibleGroups = adminNavigationGroups
    .map((group) => ({ ...group, sections: group.sections.filter((key) => visibleSections.includes(key)) }))
    .filter((group) => group.sections.length > 0);
  const platformPermissions = authorization?.platform_permissions || [];
  const sectionIsReadOnly = section === 'settings' || section === 'overview' ? false : adminSectionReadOnly(authorization, section);
  const currentGroup = visibleGroups.find((group) => group.sections.includes(section));
  const organizationRoute = section === 'organizations' ? readAdminOrganizationRoute() : { organizationID: '', tab: 'overview' };
  const organizationDirectory = section === 'organizations' ? readAdminOrganizationDirectory() : { q: '', status: '', cursor: '' };
  const currentPageTitle = copy.nav[section];
  const currentPageLead = section === 'overview' ? copy.overview.lead : copy.pageLeads?.[section] || copy.subtitle;

  return (
    <div className="adm-shell">
      <a className="adm-skip-link" href="#admin-content">{copy.skipToContent}</a>
      <aside className="adm-sidebar">
        <div className="adm-brand">
          <div className="adm-mark">P</div>
          <div>
            <strong>{copy.title}</strong>
            <span>{copy.product}</span>
          </div>
        </div>
        <nav aria-label={copy.primaryNavigation}>
          {visibleGroups.map((group) => (
            <div className="adm-nav-group" key={group.key}>
              <span className="adm-nav-label">{copy.navGroups[group.key]}</span>
              {group.sections.map((key) => (
                <button key={key} type="button" aria-current={section === key ? 'page' : undefined} className={section === key ? 'active' : ''} onClick={() => navigate(key)}>
                  <span aria-hidden="true">{NAV_ICONS[key]}</span>
                  {copy.nav[key]}
                </button>
              ))}
            </div>
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
      <main className="adm-main" id="admin-content" tabIndex="-1">
        <header className="adm-topbar">
          <div>
            <span className="adm-eyebrow">{copy.product}</span>
            <h1>{copy.nav[section]}</h1>
          </div>
          <div className="adm-actions">
            <div className="adm-admin">
              <span>PA</span>
              <div>
                <strong>{copy.administrator}</strong>
                <button onClick={logout}>{copy.logout}</button>
              </div>
            </div>
          </div>
        </header>
        {busy && <div className="adm-progress" role="progressbar" aria-label={copy.loading} />}
        <div className="adm-page">
          {!organizationRoute.organizationID && <PageHeader
            eyebrow={copy.navGroups[currentGroup?.key] || copy.product}
            title={currentPageTitle}
            subtitle={currentPageLead}
            updatedAt={['products', 'approvals', 'settings'].includes(section) ? null : updatedAt}
            canRefresh={!['products', 'approvals', 'settings'].includes(section)}
            busy={busy}
            copy={copy}
            onRefresh={() => {
              void load(section, undefined, authorization);
              if (organizationRoute.organizationID) setRouteVersion((value) => value + 1);
            }}
          />}
          {error && (
            <div role="alert" className="adm-alert">
              {error}
            </div>
          )}
          {sectionIsReadOnly && (
            <div className="adm-inline-message" role="status">
              {copy.permissionReadOnlyReason}
            </div>
          )}
          {section === 'overview' && <Overview copy={copy} language={language} data={data} permissions={platformPermissions} onNavigate={navigate} />}
          {section === 'organizations' && <AccountDirectory section="organizations" copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} onNavigate={navigate} organizationRoute={organizationRoute} organizationDirectory={organizationDirectory} routeVersion={routeVersion} />}
          {section === 'users' && (
            <UsersWorkspace copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} currentUserID={authorization?.user_id || ''} onNavigate={navigate} />
          )}
          {section === 'roles' && <AdminRolesWorkspace copy={copy} roles={data.roles || []} bindings={data.roleBindings || []} users={data.roleUsers || []} organizations={data.roleOrganizations || []} client={client} setData={setData} permissions={platformPermissions} onNavigate={navigate} />}
          {section === 'billing' && <Billing key="billing" surface="billing" copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} />}
          {section === 'payments' && <Billing key="payments" surface="payments" copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} />}
          {section === 'tokens' && <Tokens copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} />}
          {section === 'system' && (
            <>
              {hasPermission(authorization, 'platform.system.status.read') && <SystemStatus copy={copy} data={data} permissions={platformPermissions} />}
              {hasPermission(authorization, 'platform.users.identity.repair') && <KeycloakSecurityAccess copy={copy} />}
            </>
          )}
          {section === 'audit' && <Audit copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} />}
          {section === 'settings' && <SettingsWorkspace copy={copy} language={language} onLanguageChange={onLanguageChange} />}
          <AdminExtensionSlot section={section} authorization={authorization} />
        </div>
      </main>
    </div>
  );
}

function sectionHasData(section, data) {
  return data.loadedSections?.[section] === true;
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

function Overview({ copy, language, data, permissions, onNavigate }) {
  const a = data.accountOverview || {},
    b = data.billingOverview || {},
    t = data.tokenOverview || {};
  const availability = data.overviewAvailability || {};
  const permissionSet = new Set(permissions || []);
  const canUsers = permissionSet.has('platform.users.read');
  const canOrganizations = permissionSet.has('platform.organizations.read');
  const canBilling = permissionSet.has('platform.billing.wallets.read');
  const canTokens = permissionSet.has('platform.tokens.read');
  const canUsage = permissionSet.has('platform.usage.read');
  const available = Number(b.posted_microcredits || 0) - Number(b.reserved_microcredits || 0) - Number(b.receivable_microcredits || 0);
  const routeTotal = Number(t.routes || 0);
  const healthyRoutes = Number(t.healthy_routes || 0);
  const routePercent = routeTotal > 0 ? Math.min(100, (healthyRoutes / routeTotal) * 100) : null;
  const status = data.systemStatus || { services: [], relays: [], recent_errors: [] };
  const healthyServices = status.services.filter((item) => item.status === 'HEALTHY').length;
  const serviceIssues = status.services.length - healthyServices;
  const relayIssues = status.relays.filter((item) => item.status !== 'HEALTHY' || Number(item.dead_lettered_events) > 0).length;
  const alerts = [
    canOrganizations && { label: copy.overview.invitations, value: a.pending_invitations, section: 'organizations', tone: 'warning' },
    canBilling && { label: copy.overview.paymentReview, value: b.review_payment_orders, section: 'payments', tone: 'danger' },
    canBilling && { label: copy.overview.pendingAdjustments, value: b.pending_adjustments, section: 'approvals', tone: 'warning' },
    canUsage && { label: copy.overview.usageReview, value: t.review_usage_requests, section: 'tokens', tone: 'warning' },
    canUsage && { label: copy.overview.reconciliationStopped, value: t.reconciliation_stopped, section: 'tokens', tone: 'danger' },
  ].filter((item) => item && Number(item.value) > 0);
  const sourceError = (source) => availability[source] === 'error';
  return (
    <section className="adm-overview">
      {availability.system !== 'hidden' && (
        <article className={`adm-overview-health ${sourceError('system') ? 'is-error' : ''}`}>
          <div>
            <span className="adm-overview-kicker">{copy.overview.systemHealth}</span>
            {sourceError('system') ? <strong>{copy.overview.unavailable}</strong> : <strong>{copy.overview.servicesHealthy(healthyServices, status.services.length)}</strong>}
          </div>
          {!sourceError('system') && (
            <dl>
              <div><dt>{copy.overview.serviceIssues}</dt><dd className={serviceIssues ? 'has-issue' : ''}>{formatNumber(serviceIssues)}</dd></div>
              <div><dt>{copy.overview.relayIssues}</dt><dd className={relayIssues ? 'has-issue' : ''}>{formatNumber(relayIssues)}</dd></div>
              <div><dt>{copy.overview.recentErrors}</dt><dd className={status.recent_errors.length ? 'has-issue' : ''}>{formatNumber(status.recent_errors.length)}</dd></div>
            </dl>
          )}
          <button type="button" onClick={() => onNavigate('system')}>{copy.overview.viewDetails}</button>
        </article>
      )}
      <div className="adm-overview-domains">
        {(canUsers || canOrganizations) && (
          <OverviewDomain title={copy.overview.identity} action={copy.overview.viewDetails} onAction={() => onNavigate(canUsers ? 'users' : 'organizations')} error={sourceError('identity')} copy={copy}>
            {canUsers && <OverviewPrimary label={copy.overview.activeUsers} value={a.active_users} suffix={copy.overview.ofTotal(a.users)} />}
            {!canUsers && canOrganizations && <OverviewPrimary label={copy.overview.organizations} value={a.organizations} />}
            <OverviewFacts rows={[
              canOrganizations && [copy.overview.organizations, a.organizations],
              canOrganizations && [copy.overview.activeMemberships, a.active_memberships],
              canUsers && [copy.overview.disabledUsers, a.disabled_users],
              canOrganizations && [copy.overview.invitations, a.pending_invitations],
            ].filter(Boolean)} />
          </OverviewDomain>
        )}
        {canBilling && (
          <OverviewDomain title={copy.overview.finance} action={copy.overview.viewDetails} onAction={() => onNavigate('billing')} error={sourceError('finance')} copy={copy} accent>
            <OverviewPrimary label={copy.overview.available} value={available} formatter={(value) => formatUsagePoints(value, language)} />
            <OverviewFacts rows={[
              [copy.overview.posted, b.posted_microcredits, (value) => formatUsagePoints(value, language)],
              [copy.overview.reserved, b.reserved_microcredits, (value) => formatUsagePoints(value, language)],
              [copy.overview.receivable, b.receivable_microcredits, (value) => formatUsagePoints(value, language)],
              [copy.overview.wallets, b.wallets],
            ]} />
          </OverviewDomain>
        )}
        {(canTokens || canUsage) && (
          <OverviewDomain title={copy.overview.modelToken} action={copy.overview.viewDetails} onAction={() => onNavigate('tokens')} error={sourceError('modelToken')} copy={copy}>
            {canTokens ? (
              <OverviewPrimary label={copy.overview.routesHealthy} value={routeTotal ? `${formatNumber(healthyRoutes)} / ${formatNumber(routeTotal)}` : copy.overview.noRoutes} />
            ) : (
              <OverviewPrimary label={copy.overview.cumulativeUsage} value={t.usage_requests} />
            )}
            {canTokens && <div className="adm-overview-progress" aria-label={copy.overview.routesHealthy}><i style={{ width: `${routePercent || 0}%` }} /></div>}
            <OverviewFacts rows={[
              canTokens && [copy.overview.activeAccounts, t.active_accounts, (value) => `${formatNumber(value)} / ${formatNumber(t.token_accounts)}`],
              canTokens && [copy.overview.activeCredentials, t.active_credentials],
              canUsage && [copy.overview.cumulativeUsage, t.usage_requests],
              canUsage && [copy.overview.reconciliationPending, t.reconciliation_pending],
            ].filter(Boolean)} />
          </OverviewDomain>
        )}
      </div>
      <article className="adm-panel adm-overview-attention">
        <SectionHeader title={copy.overview.attention} count={alerts.length} copy={copy} />
        <p className="adm-help">{copy.overview.attentionLead}</p>
        {alerts.length ? (
          <div className="adm-attention-list">
            {alerts.map((item) => (
              <button type="button" key={`${item.section}-${item.label}`} onClick={() => onNavigate(item.section)}>
                <span className={`adm-attention-dot ${item.tone}`} aria-hidden="true" />
                <span>{item.label}</span>
                <strong>{formatNumber(item.value)}</strong>
                <small>{copy.overview.reviewNow} →</small>
              </button>
            ))}
          </div>
        ) : <div className="adm-overview-empty">{copy.overview.noAttention}</div>}
      </article>
    </section>
  );
}

function OverviewDomain({ title, action, onAction, error, copy, accent = false, children }) {
  return (
    <article className={`adm-overview-domain ${accent ? 'accent' : ''} ${error ? 'is-error' : ''}`}>
      <header><h2>{title}</h2><button type="button" onClick={onAction}>{action} →</button></header>
      {error ? <div className="adm-overview-source-error"><strong>{copy.overview.unavailable}</strong><span>{copy.overview.unavailableHelp}</span></div> : children}
    </article>
  );
}

function OverviewPrimary({ label, value, suffix = '', formatter = formatNumber }) {
  return <div className="adm-overview-primary"><span>{label}</span><strong>{typeof value === 'string' ? value : formatter(value)}</strong>{suffix && <small>{suffix}</small>}</div>;
}

function OverviewFacts({ rows }) {
  return <dl className="adm-overview-facts">{rows.map(([label, value, formatter = formatNumber]) => <div key={label}><dt>{label}</dt><dd>{formatter(value)}</dd></div>)}</dl>;
}

function UsersWorkspace({ copy, data, client, setData, permissions, currentUserID, onNavigate }) {
  const permissionSet = new Set(permissions || []);
  const repairCount = (data.registrationRepairs || []).length + (data.profileChangeRepairs || []).length + (data.accountClosures || []).length;
  const repairUnavailable = Object.values(data.userRepairAvailability || {}).includes('error');
  return (
    <div className="adm-user-workspace">
      {permissionSet.has('platform.users.read') && (
        <section className="adm-user-summary" aria-label={copy.userManagement.summary}>
          <Metric label={copy.userManagement.totalUsers} value={data.accountOverview?.users} />
          <Metric label={copy.userManagement.activeUsers} value={data.accountOverview?.active_users} tone="positive" />
          <Metric label={copy.userManagement.disabledUsers} value={data.accountOverview?.disabled_users} />
          <Metric label={copy.userManagement.pendingRepairs} value={repairUnavailable ? '—' : repairCount} formatter={(value) => value} tone={!repairUnavailable && repairCount ? 'warning' : ''} />
        </section>
      )}
      {permissionSet.has('platform.users.read') && <AccountDirectory key={data.usersLoadVersion || 'users'} section="users" copy={copy} data={data} client={client} setData={setData} permissions={permissions} currentUserID={currentUserID} onNavigate={onNavigate} />}
      <UserRepairWorkspace copy={copy} data={data} client={client} setData={setData} permissions={permissions} />
    </div>
  );
}

function OrganizationDeletionDialog({ copy, organization, client, onClose, onDeleted }) {
  const [assessment, setAssessment] = useState(null);
  const [loading, setLoading] = useState(true);
  const [deleting, setDeleting] = useState(false);
  const [typedName, setTypedName] = useState('');
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');
  const [requestID, setRequestID] = useState('');
  const loadReadiness = useCallback(async (signal) => {
    setLoading(true);
    setError('');
    setRequestID('');
    try {
      setAssessment(await client.organizationDeletionReadiness(organization.organization_id, signal));
    } catch (cause) {
      if (cause?.name === 'AbortError') return;
      setError(cause?.code === 'ORGANIZATION_LIFECYCLE_UNAVAILABLE' ? copy.accounts.deletionUnavailable : copy.accounts.deletionLoadFailed);
      setRequestID(cause?.requestID || '');
    } finally {
      setLoading(false);
    }
  }, [client, copy, organization.organization_id]);
  useEffect(() => {
    const controller = new globalThis.AbortController();
    loadReadiness(controller.signal);
    return () => controller.abort();
  }, [loadReadiness]);
  const submit = async (event) => {
    event.preventDefault();
    if (!assessment?.ready || typedName !== organization.name || !reason.trim() || deleting) return;
    setDeleting(true);
    setError('');
    setRequestID('');
    try {
      await client.deleteOrganization(organization.organization_id, reason.trim());
      await onDeleted();
    } catch (cause) {
      const blockers = cause?.details?.blockers;
      if (cause?.code === 'ORGANIZATION_LIFECYCLE_BLOCKED' && Array.isArray(blockers)) {
        setAssessment((current) => ({
          ...(current || {}),
          ready: false,
          blockers,
        }));
        setError(copy.accounts.deletionChanged);
      } else {
        setError(cause?.code === 'ORGANIZATION_LIFECYCLE_UNAVAILABLE' ? copy.accounts.deletionUnavailable : copy.accounts.actionFailed);
      }
      setRequestID(cause?.requestID || '');
    } finally {
      setDeleting(false);
    }
  };
  const blockerMessage = (blocker) => copy.accounts.deletionBlockers[blocker.code] || copy.accounts.unknownDeletionBlocker(blocker.code, copy.accounts.deletionDomains[blocker.domain] || blocker.domain || copy.accounts.unknownDomain);
  return (
    <DetailDrawer label={`${copy.accounts.deletionReview} · ${organization.name}`} onClose={deleting ? () => {} : onClose}>
      <section className="adm-panel adm-deletion-review">
        <SectionHeader title={copy.accounts.deletionReview} copy={copy}>
          <button type="button" onClick={onClose} disabled={deleting}>{copy.accounts.cancel}</button>
        </SectionHeader>
        <p className="adm-help">{copy.accounts.deletionReviewHelp}</p>
        <dl className="adm-detail-list">
          <div><dt>{copy.accounts.organizationName}</dt><dd>{organization.name}</dd></div>
          <div><dt>ID</dt><dd><TechnicalID value={organization.organization_id} copy={copy} /></dd></div>
          <div><dt>{copy.accounts.currentStatus}</dt><dd><Status value={assessment?.current_status || organization.status} /></dd></div>
          <div><dt>{copy.accounts.deletionCheck}</dt><dd>{loading ? copy.loading : assessment?.ready ? copy.accounts.deletionReady : copy.accounts.deletionBlocked}</dd></div>
        </dl>
        {error && <div className="adm-inline-message error" role="alert">{error}{requestID && <small>{copy.accounts.requestID}: {requestID}</small>}</div>}
        {!loading && assessment && !assessment.ready && (
          <div className="adm-deletion-blockers" role="status">
            <strong>{copy.accounts.deletionBlockerTitle}</strong>
            <ul>{(assessment.blockers || []).map((blocker, index) => <li key={`${blocker.domain}:${blocker.code}:${blocker.resource_id}:${index}`}><span>{blockerMessage(blocker)}</span><code>{blocker.domain || 'ACCOUNT'} · {blocker.code}</code></li>)}</ul>
          </div>
        )}
        {!loading && !assessment && <button type="button" onClick={() => loadReadiness()}>{copy.accounts.retryDeletionCheck}</button>}
        <form className="adm-deletion-form" onSubmit={submit}>
          <label><span>{copy.accounts.deletionNameLabel(organization.name)}</span><input value={typedName} onChange={(event) => setTypedName(event.target.value)} placeholder={organization.name} autoComplete="off" /></label>
          <label><span>{copy.accounts.deletionReason}</span><textarea value={reason} onChange={(event) => setReason(event.target.value)} placeholder={copy.accounts.deletionReasonPlaceholder} maxLength={500} rows={4} /></label>
          <div className="adm-row-actions">
            <button type="button" onClick={() => loadReadiness()} disabled={loading || deleting}>{copy.accounts.retryDeletionCheck}</button>
            <button className="danger" disabled={!assessment?.ready || typedName !== organization.name || !reason.trim() || deleting}>{deleting ? copy.accounts.deleting : copy.accounts.confirmDelete}</button>
          </div>
        </form>
      </section>
    </DetailDrawer>
  );
}

function AccountDirectory({ section, copy, data, client, setData, permissions, currentUserID = '', onNavigate, organizationRoute = { organizationID: '', tab: 'overview' }, organizationDirectory = { q: '', status: '', cursor: '' }, routeVersion = 0 }) {
  const [search, setSearch] = useState('');
  const [userCode, setUserCode] = useState('');
  const [userMode, setUserMode] = useState('directory');
  const [currentCursor, setCurrentCursor] = useState('');
  const [cursorHistory, setCursorHistory] = useState([]);
  const [pageNumber, setPageNumber] = useState(1);
  const organizationCursorBack = useRef(new Map());
  const [actionKey, setActionKey] = useState('');
  const [notice, setNotice] = useState('');
  const [actionError, setActionError] = useState('');
  const [routeOrganization, setRouteOrganization] = useState(null);
  const [routeError, setRouteError] = useState('');
  const [deleteTarget, setDeleteTarget] = useState(null);
  const [selectedUser, setSelectedUser] = useState(null);
  const [organizationName, setOrganizationName] = useState('');
  const [organizationStatus, setOrganizationStatus] = useState(organizationDirectory.status || '');
  const [createOpen, setCreateOpen] = useState(false);
  const [ownerSearch, setOwnerSearch] = useState('');
  const [ownerCandidates, setOwnerCandidates] = useState([]);
  const [ownerLoading, setOwnerLoading] = useState(false);
  const [ownerUserID, setOwnerUserID] = useState('');
  const isUserSection = section === 'users';
  const permissionSet = new Set(permissions || []);
  const canManageUserStatus = permissionSet.has('platform.users.status.manage');
  const canCreateOrganization = permissionSet.has('platform.organizations.create') && permissionSet.has('platform.users.read');
  const canManageOrganizationStatus = permissionSet.has('platform.organizations.status.manage');
  const canDeleteOrganization = permissionSet.has('platform.organizations.delete');
  const items = isUserSection ? data.users || [] : data.organizations || [];
  const selectedOrganization = !isUserSection && organizationRoute.organizationID ? items.find((item) => item.organization_id === organizationRoute.organizationID) || routeOrganization : null;
  useEffect(() => {
    if (isUserSection || !organizationRoute.organizationID) {
      setRouteOrganization(null);
      setRouteError('');
      return undefined;
    }
    const existing = (data.organizations || []).find((item) => item.organization_id === organizationRoute.organizationID);
    if (existing) {
      setRouteOrganization(existing);
      setRouteError('');
      return undefined;
    }
    const controller = new globalThis.AbortController();
    setRouteError('');
    client.organizations(organizationRoute.organizationID, controller.signal).then((result) => {
      const match = (result.items || []).find((item) => item.organization_id === organizationRoute.organizationID);
      if (match) {
        setRouteOrganization(match);
        setData((current) => ({
          ...current,
          organizations: [...(current.organizations || []).filter((item) => item.organization_id !== match.organization_id), match],
        }));
      }
      else setRouteError(copy.accounts.organizationNotFound);
    }).catch((reason) => {
      if (reason?.name !== 'AbortError') setRouteError(copy.accounts.organizationLoadFailed);
    });
    return () => controller.abort();
  }, [client, copy.accounts.organizationLoadFailed, copy.accounts.organizationNotFound, data.organizations, isUserSection, organizationRoute.organizationID]);
  const applyUserPage = (result, cursor = '') => {
    setData((current) => ({
      ...current,
      users: result.items || [],
      userNextCursor: result.next_cursor || '',
    }));
  };
  const applyOrganizationPage = (result) => {
    setData((current) => ({
      ...current,
      organizations: result.items || [],
      organizationNextCursor: result.next_cursor || '',
      accountOverview: undefined,
    }));
  };
  useEffect(() => {
    if (isUserSection || organizationRoute.organizationID) return undefined;
    setSearch(organizationDirectory.q || '');
    setOrganizationStatus(organizationDirectory.status || '');
    setCurrentCursor(organizationDirectory.cursor || '');
    let routedPage = 1;
    let cursor = organizationDirectory.cursor || '';
    const seen = new Set();
    while (cursor && organizationCursorBack.current.has(cursor) && !seen.has(cursor)) {
      seen.add(cursor);
      cursor = organizationCursorBack.current.get(cursor) || '';
      routedPage += 1;
    }
    setPageNumber(routedPage);
    const controller = new globalThis.AbortController();
    client.organizations(organizationDirectory, controller.signal).then(applyOrganizationPage).catch((reason) => {
      if (reason?.name !== 'AbortError') setActionError(copy.accounts.lookupUnavailable || copy.accounts.actionFailed);
    });
    return () => controller.abort();
  }, [client, copy.accounts.actionFailed, copy.accounts.lookupUnavailable, isUserSection, organizationDirectory.cursor, organizationDirectory.q, organizationDirectory.status, organizationRoute.organizationID, routeVersion]);
  useEffect(() => {
    if (isUserSection || !createOpen) return undefined;
    const controller = new globalThis.AbortController();
    const timer = globalThis.setTimeout?.(() => {
      setOwnerLoading(true);
      client.users(ownerSearch.trim(), controller.signal).then((result) => {
        setOwnerCandidates((result.items || []).filter((user) => user.status === 'ACTIVE'));
      }).catch((reason) => {
        if (reason?.name !== 'AbortError') setActionError(copy.accounts.ownerSearchFailed);
      }).finally(() => setOwnerLoading(false));
    }, 250);
    return () => {
      if (timer) globalThis.clearTimeout?.(timer);
      controller.abort();
    };
  }, [client, copy.accounts.ownerSearchFailed, createOpen, isUserSection, ownerSearch]);
  const reload = async (cursor = currentCursor) => {
    if (isUserSection && userMode === 'code') {
      const item = await client.userByCode(userCode.trim());
      applyUserPage({ items: [item] });
      return;
    }
    const result = isUserSection ? await client.users(search.trim(), undefined, cursor) : await client.organizations({ q: search.trim(), status: organizationStatus, cursor });
    if (isUserSection) applyUserPage(result, cursor);
    else applyOrganizationPage(result);
  };
  const perform = async (item, action) => {
    const isUser = isUserSection;
    const id = isUser ? item.user_id : item.organization_id;
    const label = isUser ? userLabel(item) : item.name;
    if (isUser && action === 'disable' && !window.confirm(copy.userManagement.confirmDisable(label, item.user_code || shortID(id)))) return;
    setActionKey(`${section}:${id}:${action}`);
    setNotice('');
    setActionError('');
    try {
      if (isUser) {
        await client.setUserStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
      } else await client.setOrganizationStatus(id, action === 'enable' ? 'ACTIVE' : 'DISABLED');
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
    const buttons = [];
    const canManageStatus = isUser ? canManageUserStatus : canManageOrganizationStatus;
    if (isUser && id === currentUserID) return <span className="adm-row-note" title={copy.userManagement.selfActionHelp}>{copy.userManagement.currentAccount}</span>;
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
    if (!isUser && canDeleteOrganization && item.status === 'DISABLED')
      buttons.push(
        <button key="delete" className="danger" disabled={Boolean(actionKey)} onClick={() => setDeleteTarget(item)}>
          {copy.accounts.delete}
        </button>,
      );
    return <div className="adm-row-actions">{buttons}</div>;
  };
  const columns = isUserSection
    ? [
        {
          key: 'username',
          label: copy.userManagement.account,
          render: (item) => (
            <div className="adm-user-identity">
              <button className="adm-name-link" onClick={() => setSelectedUser(item)}>{userLabel(item)}</button>
              {item.display_name && item.display_name !== item.username && <small>{item.display_name}</small>}
            </div>
          ),
        },
        {
          key: 'user_code',
          label: copy.accounts.userCode,
          render: (item) => <TechnicalID value={item.user_code} copy={copy} />,
        },
        {
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        { key: 'organization_count', label: copy.userManagement.organizationCount },
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
            <button className="adm-name-link" onClick={() => onNavigate('organizations', { organization_id: item.organization_id, organization_tab: 'overview' })}>
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
      if (isUserSection) {
        setUserMode('directory');
        setCurrentCursor('');
        setCursorHistory([]);
        organizationCursorBack.current.clear();
        setPageNumber(1);
        const result = await client.users(search.trim());
        applyUserPage(result);
      } else {
        setCurrentCursor('');
        setCursorHistory([]);
        setPageNumber(1);
        onNavigate('organizations', {
          organization_q: search.trim() || null,
          organization_status: organizationStatus || null,
          organization_cursor: null,
        });
      }
    } catch {
      setActionError(copy.accounts.lookupUnavailable || copy.accounts.actionFailed);
    }
  };
  const submitUserCode = async (event) => {
    event.preventDefault();
    const code = userCode.trim();
    if (!code) return;
    setActionError('');
    try {
      const item = await client.userByCode(code);
      setUserMode('code');
      setCurrentCursor('');
      setCursorHistory([]);
      setPageNumber(1);
      applyUserPage({ items: [item] });
    } catch {
      setActionError(copy.userManagement.codeLookupFailed);
    }
  };
  const changeUserPage = async (direction) => {
    const nextCursor = direction === 'next' ? data.userNextCursor : cursorHistory.at(-1) || '';
    const nextHistory = direction === 'next' ? [...cursorHistory, currentCursor] : cursorHistory.slice(0, -1);
    setActionError('');
    try {
      const result = await client.users(search.trim(), undefined, nextCursor);
      setCurrentCursor(nextCursor);
      setCursorHistory(nextHistory);
      setPageNumber((value) => Math.max(1, value + (direction === 'next' ? 1 : -1)));
      applyUserPage(result, nextCursor);
    } catch {
      setActionError(copy.accounts.actionFailed);
    }
  };
  const changeOrganizationPage = (direction) => {
    const nextCursor = direction === 'next' ? data.organizationNextCursor : organizationCursorBack.current.get(currentCursor) || '';
    if (direction === 'next' && nextCursor) organizationCursorBack.current.set(nextCursor, currentCursor);
    onNavigate('organizations', {
      organization_q: search.trim() || null,
      organization_status: organizationStatus || null,
      organization_cursor: nextCursor || null,
    });
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
      setOwnerSearch('');
      setOwnerUserID('');
      setCreateOpen(false);
      await reload();
      setNotice(copy.accounts.organizationCreated);
    } catch {
      setActionError(copy.accounts.actionFailed);
    } finally {
      setActionKey('');
    }
  };
  if (selectedUser) return <UserMembershipDetail copy={copy} user={selectedUser} client={client} onBack={() => setSelectedUser(null)} onNavigate={onNavigate} />;
  if (!isUserSection && organizationRoute.organizationID && !selectedOrganization)
    return (
      <section className="adm-panel adm-org-detail">
        <button className="adm-back" onClick={() => onNavigate('organizations', { organization_id: null, organization_tab: null })}>← {copy.accounts.backToOrganizations}</button>
        {routeError ? <div className="adm-inline-message error" role="alert">{routeError}</div> : <div className="adm-empty" role="status">{copy.loading}</div>}
      </section>
    );
  if (selectedOrganization)
    return (
      <OrganizationDetail
        copy={copy}
        organization={selectedOrganization}
        roles={data.roles || []}
        client={client}
        permissions={permissions}
        onNavigate={onNavigate}
        activeTab={organizationRoute.tab}
        refreshVersion={routeVersion}
        onTabChange={(tab) => onNavigate('organizations', { organization_id: selectedOrganization.organization_id, organization_tab: tab })}
        onBack={() => onNavigate('organizations', { organization_id: null, organization_tab: null })}
        onStatusChanged={(status) => {
          setRouteOrganization((current) => (current ? { ...current, status } : current));
          setData((current) => ({
            ...current,
            organizations: (current.organizations || []).map((item) => (item.organization_id === selectedOrganization.organization_id ? { ...item, status } : item)),
          }));
        }}
        onDeleted={() => {
          onNavigate('organizations', { organization_id: null, organization_tab: null });
          setNotice(copy.accounts.organizationDeleted);
          reload().catch(() => setActionError(copy.accounts.lookupUnavailable || copy.accounts.actionFailed));
        }}
        onRenamed={(name) => {
          setRouteOrganization((current) => (current ? { ...current, name } : current));
          setData((current) => ({
            ...current,
            organizations: (current.organizations || []).map((item) => (item.organization_id === selectedOrganization.organization_id ? { ...item, name } : item)),
          }));
        }}
      />
    );
  return (
    <>
    <section className="adm-panel">
      <SectionHeader title={copy.accounts[section]} count={items.length} copy={copy}>
        {isUserSection ? (
          <div className="adm-user-searches">
            <form className="adm-search" onSubmit={submit}>
              <label><span>{copy.userManagement.fuzzySearch}</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={copy.search} /></label>
              <button>{copy.searchAction}</button>
            </form>
            <form className="adm-search" onSubmit={submitUserCode}>
              <label><span>{copy.userManagement.exactCode}</span><input value={userCode} onChange={(event) => setUserCode(event.target.value)} placeholder={copy.userManagement.codePlaceholder} /></label>
              <button disabled={!userCode.trim()}>{copy.userManagement.lookup}</button>
            </form>
          </div>
        ) : (
          <div className="adm-section-tools">
            <form className="adm-search" onSubmit={submit}>
              <input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={copy.accounts.organizationSearch} />
              <select value={organizationStatus} onChange={(event) => setOrganizationStatus(event.target.value)} aria-label={copy.accounts.organizationStatusFilter}>
                <option value="">{copy.accounts.allStatuses}</option>
                <option value="ACTIVE">ACTIVE</option>
                <option value="DISABLED">DISABLED</option>
                <option value="DELETED">DELETED</option>
              </select>
              <button>{copy.searchAction}</button>
            </form>
            {canCreateOrganization && <button type="button" className="adm-primary-action" onClick={() => setCreateOpen((value) => !value)}>{createOpen ? copy.accounts.cancel : copy.accounts.createOrganization}</button>}
          </div>
        )}
      </SectionHeader>
      {!isUserSection && canCreateOrganization && createOpen && (
        <form className="adm-create-row adm-organization-create" onSubmit={createOrganization}>
          <input value={organizationName} onChange={(event) => setOrganizationName(event.target.value)} placeholder={copy.accounts.organizationNamePlaceholder} maxLength={120} />
          <label className="adm-owner-search"><span>{copy.accounts.searchOwner}</span><input value={ownerSearch} onChange={(event) => { setOwnerSearch(event.target.value); setOwnerUserID(''); }} placeholder={copy.accounts.ownerSearchPlaceholder} /></label>
          <select value={ownerUserID} onChange={(event) => setOwnerUserID(event.target.value)} aria-label={copy.accounts.initialOwner}>
            <option value="">{ownerLoading ? copy.loading : copy.accounts.chooseOwner}</option>
            {ownerCandidates.map((user) => (
              <option key={user.user_id} value={user.user_id}>
                {userLabel(user)}{user.user_code ? ` · ${user.user_code}` : ''}
              </option>
            ))}
          </select>
          <button disabled={!organizationName.trim() || !ownerUserID || Boolean(actionKey)}>{copy.accounts.createOrganization}</button>
          {!ownerLoading && ownerCandidates.length === 0 && <small>{copy.accounts.noActiveOwner}</small>}
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
      {isUserSection && userMode === 'directory' && (
        <footer className="adm-pagination">
          <span>{copy.userManagement.pageSummary(pageNumber, items.length)}</span>
          <div>
            <button type="button" disabled={!cursorHistory.length || Boolean(actionKey)} onClick={() => changeUserPage('previous')}>{copy.userManagement.previous}</button>
            <button type="button" disabled={!data.userNextCursor || Boolean(actionKey)} onClick={() => changeUserPage('next')}>{copy.userManagement.next}</button>
          </div>
        </footer>
      )}
      {!isUserSection && (
        <footer className="adm-pagination">
          <span>{copy.accounts.organizationPageSummary(pageNumber, items.length)}</span>
          <div>
            <button type="button" disabled={!organizationCursorBack.current.has(currentCursor) || Boolean(actionKey)} onClick={() => changeOrganizationPage('previous')}>{copy.userManagement.previous}</button>
            <button type="button" disabled={!data.organizationNextCursor || Boolean(actionKey)} onClick={() => changeOrganizationPage('next')}>{copy.userManagement.next}</button>
          </div>
        </footer>
      )}
    </section>
    {deleteTarget && <OrganizationDeletionDialog copy={copy} organization={deleteTarget} client={client} onClose={() => setDeleteTarget(null)} onDeleted={() => { setDeleteTarget(null); setNotice(copy.accounts.organizationDeleted); reload().catch(() => setActionError(copy.accounts.lookupUnavailable || copy.accounts.actionFailed)); }} />}
    </>
  );
}

function SettingsWorkspace({ copy, language, onLanguageChange }) {
  return (
    <section className="adm-panel adm-settings-panel">
      <SectionHeader title={copy.settings.title} copy={copy} />
      <div className="adm-detail-grid">
        <label>
          <span>{copy.settings.language}</span>
          <select value={language} onChange={(event) => onLanguageChange(event.target.value)}>
            {supportedLocales.map((locale) => (
              <option key={locale} value={locale}>
                {localeNames[locale]}
              </option>
            ))}
          </select>
        </label>
      </div>
      <p className="adm-muted">{copy.settings.boundary}</p>
    </section>
  );
}

function UserMembershipDetail({ copy, user, client, onBack, onNavigate }) {
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
    <DetailDrawer label={`${copy.roles.userMemberships} · ${userLabel(user)}`} onClose={onBack}>
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
      <dl className="adm-user-detail-facts">
        <div><dt>{copy.username}</dt><dd>{user.username || '—'}</dd></div>
        <div><dt>{copy.accounts.displayName}</dt><dd>{user.display_name || '—'}</dd></div>
        <div><dt>{copy.accounts.userCode}</dt><dd><TechnicalID value={user.user_code} copy={copy} compact={false} /></dd></div>
        <div><dt>ID</dt><dd><TechnicalID value={user.user_id} copy={copy} /></dd></div>
        <div><dt>{copy.userManagement.organizationCount}</dt><dd>{formatNumber(user.organization_count)}</dd></div>
        <div><dt>{copy.accounts.lastSeen}</dt><dd>{formatDate(user.last_seen_at)}</dd></div>
        <div><dt>{copy.accounts.created}</dt><dd>{formatDate(user.created_at)}</dd></div>
        <div><dt>{copy.billing.updated}</dt><dd>{formatDate(user.updated_at)}</dd></div>
      </dl>
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
                  <button type="button" key={role} onClick={() => onNavigate('roles', { role })}>{role}</button>
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
      <div className="adm-role-reference">
        <p>{copy.roles.userRoleReference}</p>
        <button type="button" onClick={() => onNavigate('roles')}>{copy.roles.viewRoleCatalog}</button>
      </div>
    </section>
    </DetailDrawer>
  );
}

function OrganizationDetail({ copy, organization, roles, client, permissions, onBack, onRenamed, onStatusChanged, onDeleted, onNavigate, activeTab = 'overview', refreshVersion = 0, onTabChange }) {
  const [units, setUnits] = useState([]);
  const [members, setMembers] = useState([]);
  const [owners, setOwners] = useState([]);
  const [memberNextCursor, setMemberNextCursor] = useState('');
  const [memberCursor, setMemberCursor] = useState('');
  const [memberPage, setMemberPage] = useState(1);
  const memberCursorBack = useRef(new Map());
  const [memberSearch, setMemberSearch] = useState('');
  const [memberQuery, setMemberQuery] = useState('');
  const [memberRole, setMemberRole] = useState('');
  const [memberQueryRole, setMemberQueryRole] = useState('');
  const [selectedMember, setSelectedMember] = useState(null);
  const [addMemberOpen, setAddMemberOpen] = useState(false);
  const [memberCandidateSearch, setMemberCandidateSearch] = useState('');
  const [memberCandidates, setMemberCandidates] = useState([]);
  const [memberCandidatesLoading, setMemberCandidatesLoading] = useState(false);
  const [ownerCandidateSearch, setOwnerCandidateSearch] = useState('');
  const [ownerCandidates, setOwnerCandidates] = useState([]);
  const [name, setName] = useState('');
  const [organizationName, setOrganizationName] = useState(organization.name);
  const [currentStatus, setCurrentStatus] = useState(organization.status);
  const [editingOrganizationName, setEditingOrganizationName] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [parentID, setParentID] = useState('');
  const [newOwnerUserID, setNewOwnerUserID] = useState('');
  const [newMemberUserID, setNewMemberUserID] = useState('');
  const [busy, setBusy] = useState(true);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const permissionSet = new Set(permissions || []);
  const canManageOrganizationProfile = permissionSet.has('platform.organizations.profile.manage');
  const canManageOrganizationStatus = permissionSet.has('platform.organizations.status.manage');
  const canManageOrganizationMembers = permissionSet.has('platform.organizations.members.manage');
  const canManageOrganizationUnits = permissionSet.has('platform.organizations.units.manage');
  const canTransferOwnership = permissionSet.has('platform.organizations.owner.transfer');
  const canDeleteOrganization = permissionSet.has('platform.organizations.delete');
  const load = useCallback(
    async (signal) => {
      setBusy(true);
      setError('');
      try {
        const [unitResult, memberResult, ownerResult] = await Promise.all([
          client.organizationUnits(organization.organization_id, signal),
          client.organizationMembers(organization.organization_id, {}, signal),
          client.organizationMembers(organization.organization_id, { role:'OWNER' }, signal),
        ]);
        setUnits(unitResult.items || []);
        setMembers(memberResult.items || []);
        setMemberNextCursor(memberResult.next_cursor || '');
        setOwners(ownerResult.items || []);
        setMemberSearch('');
        setMemberQuery('');
        setMemberRole('');
        setMemberQueryRole('');
        setMemberCursor('');
        setMemberPage(1);
        memberCursorBack.current.clear();
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
  }, [load, refreshVersion]);
  useEffect(() => setOrganizationName(organization.name), [organization.name]);
  useEffect(() => setCurrentStatus(organization.status), [organization.status]);
  useEffect(() => {
    if (!addMemberOpen) return undefined;
    const controller = new globalThis.AbortController();
    const timer = globalThis.setTimeout?.(() => {
      setMemberCandidatesLoading(true);
      client.users({ q:memberCandidateSearch.trim(), exclude_organization_id:organization.organization_id }, controller.signal).then((result) => {
        setMemberCandidates((result.items || []).filter((user) => user.status === 'ACTIVE'));
      }).catch((reason) => {
        if (reason?.name !== 'AbortError') setError(copy.roles.usersLoadFailed);
      }).finally(() => setMemberCandidatesLoading(false));
    }, 250);
    return () => {
      if (timer) globalThis.clearTimeout?.(timer);
      controller.abort();
    };
  }, [addMemberOpen, client, copy.roles.usersLoadFailed, memberCandidateSearch, organization.organization_id]);
  useEffect(() => {
    if (!canTransferOwnership) return undefined;
    const controller = new globalThis.AbortController();
    const timer = globalThis.setTimeout?.(() => {
      client.organizationMembers(organization.organization_id, { q:ownerCandidateSearch.trim() }, controller.signal).then((result) => {
        setOwnerCandidates((result.items || []).filter((member) => member.status === 'ACTIVE' && !(member.roles || []).includes('OWNER')));
      }).catch((reason) => {
        if (reason?.name !== 'AbortError') setError(copy.accounts.actionFailed);
      });
    }, 250);
    return () => {
      if (timer) globalThis.clearTimeout?.(timer);
      controller.abort();
    };
  }, [canTransferOwnership, client, copy.accounts.actionFailed, organization.organization_id, ownerCandidateSearch]);
  const roots = useMemo(() => buildUnitTree(units), [units]);
  const memberLabel = (member) => member.display_name || member.user_code || shortID(member.user_id);
  const refreshUnits = async () => {
    const result = await client.organizationUnits(organization.organization_id);
    setUnits(result.items || []);
  };
  const refreshMembers = async (filters = { q:memberQuery, role:memberQueryRole, cursor:memberCursor }) => {
    const result = await client.organizationMembers(organization.organization_id, filters);
    setMembers(result.items || []);
    setMemberNextCursor(result.next_cursor || '');
    const ownerResult = await client.organizationMembers(organization.organization_id, { role:'OWNER' });
    setOwners(ownerResult.items || []);
  };
  const searchMembers = async (event) => {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError('');
    const query = memberSearch.trim();
    try {
      setMemberQuery(query);
      setMemberQueryRole(memberRole);
      setMemberCursor('');
      setMemberPage(1);
      memberCursorBack.current.clear();
      await refreshMembers({ q:query, role:memberRole, cursor:'' });
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const changeMemberPage = async (direction) => {
    const nextCursor = direction === 'next' ? memberNextCursor : memberCursorBack.current.get(memberCursor) || '';
    if (direction === 'next' && nextCursor) memberCursorBack.current.set(nextCursor, memberCursor);
    setBusy(true);
    setError('');
    try {
      await refreshMembers({ q:memberQuery, role:memberQueryRole, cursor:nextCursor });
      setMemberCursor(nextCursor);
      setMemberPage((value) => Math.max(1, value + (direction === 'next' ? 1 : -1)));
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const changeStatus = async (status) => {
    if (busy || status === currentStatus) return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.setOrganizationStatus(organization.organization_id, status);
      setCurrentStatus(status);
      onStatusChanged(status);
      setNotice(copy.accounts.updated);
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
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
    if (!name.trim() || busy || currentStatus !== 'ACTIVE') return;
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
    const target = ownerCandidates.find((member) => member.user_id === newOwnerUserID);
    if (!target || busy || currentStatus !== 'ACTIVE' || !window.confirm(copy.accounts.confirmOwnership(memberLabel(target)))) return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.transferOrganizationOwnership(organization.organization_id, target.user_id);
      await refreshMembers();
      setNewOwnerUserID('');
      setOwnerCandidateSearch('');
      setNotice(copy.accounts.ownershipTransferred);
    } catch {
      setError(copy.accounts.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const addMember = async (event) => {
    event.preventDefault();
    if (!newMemberUserID || busy || currentStatus !== 'ACTIVE') return;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await client.addOrganizationMember(organization.organization_id, newMemberUserID, ['MEMBER'], []);
      setMemberSearch('');
      setMemberQuery('');
      setMemberRole('');
      setMemberQueryRole('');
      setMemberCursor('');
      setMemberPage(1);
      memberCursorBack.current.clear();
      await refreshMembers({ q:'', role:'', cursor:'' });
      setNewMemberUserID('');
      setMemberCandidateSearch('');
      setAddMemberOpen(false);
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
      setSelectedMember(null);
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
      setSelectedMember(null);
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
    <>
    <section className="adm-panel adm-org-detail">
      <button className="adm-back" onClick={onBack}>
        ← {copy.accounts.backToOrganizations}
      </button>
      <div className="adm-org-title">
        <div>
          <span className="adm-eyebrow">{copy.accounts.organizationManagement}</span>
          {canManageOrganizationProfile && editingOrganizationName ? (
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
              <p>{copy.accounts.organizationDetailLead}</p>
              {canManageOrganizationProfile && (
                <button type="button" disabled={busy} onClick={() => setEditingOrganizationName(true)}>
                  {copy.accounts.rename}
                </button>
              )}
            </div>
          )}
        </div>
        <div className="adm-org-heading-actions">
          <Status value={currentStatus} />
          <button type="button" disabled={busy} onClick={() => load()}>{copy.refresh}</button>
        </div>
      </div>
      <Tabs
        items={[
          { key: 'overview', label: copy.accounts.organizationTabs.overview },
          { key: 'members', label: copy.accounts.organizationTabs.members },
          { key: 'units', label: copy.accounts.organizationTabs.units },
          { key: 'lifecycle', label: copy.accounts.organizationTabs.lifecycle },
        ]}
        active={activeTab}
        onChange={onTabChange}
      />
      {notice && <div className="adm-inline-message ok" role="status">{notice}</div>}
      {error && <div className="adm-inline-message error" role="alert">{error}</div>}
      {activeTab === 'overview' && (
        <div className="adm-org-section">
      <dl className="adm-detail-list">
        <div><dt>ID</dt><dd><TechnicalID value={organization.organization_id} copy={copy} compact={false} /></dd></div>
        <div><dt>{copy.accounts.status}</dt><dd><Status value={currentStatus} /></dd></div>
        <div><dt>{copy.accounts.members}</dt><dd>{organization.member_count ?? members.length}</dd></div>
        <div><dt>{copy.accounts.unitHierarchy}</dt><dd>{units.length}</dd></div>
        <div><dt>{copy.accounts.created}</dt><dd>{formatDate(organization.created_at)}</dd></div>
        <div><dt>{copy.accounts.updatedAt}</dt><dd>{formatDate(organization.updated_at)}</dd></div>
      </dl>
      <div className="adm-owner-card">
        <div>
          <span>{copy.accounts.organizationOwner}</span>
          <strong>{owners.length ? owners.map(memberLabel).join(' · ') : '—'}</strong>
        </div>
        {canTransferOwnership && (
          <form onSubmit={transferOwnership}>
            <input value={ownerCandidateSearch} onChange={(event) => { setOwnerCandidateSearch(event.target.value); setNewOwnerUserID(''); }} placeholder={copy.roles.memberSearchPlaceholder} />
            <select value={newOwnerUserID} onChange={(event) => setNewOwnerUserID(event.target.value)} aria-label={copy.accounts.chooseNewOwner}>
              <option value="">{copy.accounts.chooseNewOwner}</option>
              {ownerCandidates.map((member) => (
                <option key={member.user_id} value={member.user_id}>
                  {memberLabel(member)}
                </option>
              ))}
            </select>
            <button disabled={busy || currentStatus !== 'ACTIVE' || !newOwnerUserID}>{copy.accounts.transferOwner}</button>
          </form>
        )}
        {!busy && ownerCandidates.length === 0 && <small>{copy.accounts.noTransferCandidate}</small>}
      </div>
        </div>
      )}
      {activeTab === 'members' && (
        <div className="adm-org-section">
      <div className="adm-unit-head">
        <h3>{copy.roles.organizationMembers}</h3>
        <button type="button" disabled={!canManageOrganizationMembers || !permissionSet.has('platform.users.read') || busy || currentStatus !== 'ACTIVE'} onClick={() => setAddMemberOpen((value) => !value)}>{addMemberOpen ? copy.accounts.cancel : copy.roles.addMember}</button>
      </div>
      <form className="adm-member-search" onSubmit={searchMembers}>
        <input value={memberSearch} onChange={(event) => setMemberSearch(event.target.value)} placeholder={copy.roles.memberSearchPlaceholder} />
        <select value={memberRole} onChange={(event) => setMemberRole(event.target.value)} aria-label={copy.roles.roleFilter}>
          <option value="">{copy.roles.allMemberRoles}</option>
          {roles.filter((role) => role.scope === 'ORGANIZATION' || role.scope === 'UNIT').map((role) => <option key={role.code} value={role.code}>{copy.roles.names?.[role.code] || role.code}</option>)}
        </select>
        <button disabled={busy}>{copy.searchAction}</button>
      </form>
      {canManageOrganizationMembers && permissionSet.has('platform.users.read') && addMemberOpen && (
        <form className="adm-create-row adm-member-add" onSubmit={addMember}>
          <input value={memberCandidateSearch} onChange={(event) => { setMemberCandidateSearch(event.target.value); setNewMemberUserID(''); }} placeholder={copy.roles.userSearchPlaceholder} />
          <select value={newMemberUserID} onChange={(event) => setNewMemberUserID(event.target.value)} aria-label={copy.roles.chooseMember}>
            <option value="">{memberCandidatesLoading ? copy.loading : copy.roles.chooseMember}</option>
            {memberCandidates.map((user) => (
              <option key={user.user_id} value={user.user_id}>
                {userLabel(user)}{user.user_code ? ` · ${user.user_code}` : ''}
              </option>
            ))}
          </select>
          <button disabled={busy || !newMemberUserID}>{copy.roles.addMember}</button>
        </form>
      )}
      <Table
        copy={copy}
        items={members}
        columns={[
          { key:'member', label:copy.roles.user, render:(member) => <span className="adm-member-identity"><strong>{memberLabel(member)}</strong><small>{member.user_code || shortID(member.user_id)}</small></span> },
          { key:'status', label:copy.accounts.status, render:(member) => <Status value={member.status} /> },
          { key:'roles', label:copy.roles.role, render:(member) => <div className="adm-tag-list">{(member.roles || []).map((role) => <span key={role}>{copy.roles.names?.[role] || role}</span>)}</div> },
          { key:'unit_scope', label:copy.roles.unitAdminNodes, render:(member) => copy.roles.unitScopeCount((member.unit_admin_unit_ids || []).length) },
          { key:'joined_at', label:copy.roles.joinedAt, render:(member) => formatDate(member.joined_at) },
          { key:'actions', label:copy.accounts.actions, render:(member) => <button type="button" disabled={!canManageOrganizationMembers || busy || currentStatus !== 'ACTIVE'} onClick={() => setSelectedMember(member)}>{copy.roles.editRoles}</button> },
        ]}
      />
      <footer className="adm-pagination">
        <span>{copy.roles.memberPageSummary(memberPage, members.length)}</span>
        <div>
          <button type="button" disabled={!memberCursorBack.current.has(memberCursor) || busy} onClick={() => changeMemberPage('previous')}>{copy.userManagement.previous}</button>
          <button type="button" disabled={!memberNextCursor || busy} onClick={() => changeMemberPage('next')}>{copy.userManagement.next}</button>
        </div>
      </footer>
      <div className="adm-role-reference">
        <p>{copy.roles.organizationRoleReference}</p>
        <button type="button" onClick={() => onNavigate('roles', { role_scope: 'ORGANIZATION' })}>{copy.roles.viewOrganizationRoles}</button>
      </div>
        </div>
      )}
      {activeTab === 'units' && (
        <div className="adm-org-section">
      {canManageOrganizationUnits && (
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
          <button disabled={busy || currentStatus !== 'ACTIVE' || !name.trim()}>{copy.accounts.createUnit}</button>
        </form>
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
              disabled={busy || !canManageOrganizationUnits}
              addDisabled={!canManageOrganizationUnits || currentStatus !== 'ACTIVE'}
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
        </div>
      )}
      {activeTab === 'lifecycle' && (
        <div className="adm-org-section adm-org-lifecycle">
          <div className="adm-lifecycle-summary">
            <div><span>{copy.accounts.currentStatus}</span><Status value={currentStatus} /></div>
            <p>{currentStatus === 'ACTIVE' ? copy.accounts.activeOrganizationHelp : copy.accounts.disabledOrganizationHelp}</p>
            {canManageOrganizationStatus && (
              <div className="adm-row-actions">
                {currentStatus === 'ACTIVE' ? <button type="button" onClick={() => changeStatus('DISABLED')} disabled={busy}>{copy.accounts.disable}</button> : currentStatus === 'DISABLED' ? <button type="button" onClick={() => changeStatus('ACTIVE')} disabled={busy}>{copy.accounts.enable}</button> : null}
              </div>
            )}
          </div>
          {currentStatus === 'DISABLED' && canDeleteOrganization && (
            <div className="adm-danger-zone">
              <div><strong>{copy.accounts.deletionReview}</strong><p>{copy.accounts.deletionReviewHelp}</p></div>
              <button type="button" className="danger" onClick={() => setDeleteOpen(true)}>{copy.accounts.delete}</button>
            </div>
          )}
        </div>
      )}
    </section>
    {selectedMember && (
      <DetailDrawer label={`${copy.roles.editRoles} · ${memberLabel(selectedMember)}`} onClose={() => setSelectedMember(null)}>
        <MemberAccessEditor copy={copy} member={selectedMember} units={units} roles={roles} disabled={!canManageOrganizationMembers || busy || currentStatus !== 'ACTIVE'} onSave={saveMemberRoles} onRemove={removeMember} />
      </DetailDrawer>
    )}
    {deleteOpen && <OrganizationDeletionDialog copy={copy} organization={{ ...organization, status: currentStatus, name: organizationName }} client={client} onClose={() => setDeleteOpen(false)} onDeleted={onDeleted} />}
    </>
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
          <strong>{member.display_name || member.user_code || shortID(member.user_id)}</strong>
          <small>{member.user_code || shortID(member.user_id)}</small>
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

function Billing({ copy, language, data, client, setData, permissions, surface = 'billing' }) {
  const [tab, setTab] = useState(surface === 'payments' ? 'orders' : 'wallets');
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
  const visibleTabs =
    surface === 'payments'
      ? canReadWallets
        ? ['orders', 'refunds', 'lateReviews', 'anomalies']
        : []
      : [...(canReadWallets ? ['wallets', 'receivables', 'adjustments'] : []), ...(canReadPricing ? ['prices'] : []), ...(canReadUsage ? ['holds', 'reconciliations'] : [])];
  useEffect(() => {
    if (!visibleTabs.includes(tab)) setTab(visibleTabs[0] || (surface === 'payments' ? 'orders' : 'wallets'));
  }, [tab, visibleTabs.join('|')]);
  const paymentCapabilities = data.billingOverview?.payment_capabilities || {};
  const normalRefundCreationEnabled =
    paymentCapabilities.normal_refund_creation_enabled === true &&
    paymentCapabilities.refund_supported === true;
  const reconciliationSupported = paymentCapabilities.bill_supported === true;
  const reconciliationImportSupported = paymentCapabilities.bill_file_import_supported === true;
  const repairSupported = paymentCapabilities.manual_repair_supported === true;
  const hasLimitedPaymentCapabilities = paymentCapabilities.provider_id && (!paymentCapabilities.refund_supported || !paymentCapabilities.close_supported || !paymentCapabilities.notification_supported || paymentCapabilities.reconciliation_mode !== 'API');
  const reload = async () => {
    if (surface === 'payments') {
      const [orders, refunds, lateReviews, anomalies] = await Promise.all([canReadWallets ? client.paymentOrders(orderFilter) : Promise.resolve({ items: [] }), canReadWallets ? client.refunds() : Promise.resolve({ items: [] }), canReadWallets ? client.latePaymentReviews(lateFilter) : Promise.resolve({ items: [] }), canReadWallets ? client.paymentAnomalies(anomalyFilter) : Promise.resolve({ items: [] })]);
      setData((current) => ({ ...current, orders: orders.items || [], refunds: refunds.items || [], lateReviews: lateReviews.items || [], anomalies: anomalies.items || [] }));
      return;
    }
    const [wallets, receivables, prices, adjustments, holds, reconciliations] = await Promise.all([canReadWallets ? client.wallets() : Promise.resolve({ items: [] }), canReadWallets ? client.receivables() : Promise.resolve({ items: [] }), canReadPricing ? client.priceVersions() : Promise.resolve({ items: [] }), canReadWallets ? client.adjustments() : Promise.resolve({ items: [] }), canReadUsage ? client.holds() : Promise.resolve({ items: [] }), canReadUsage ? client.reconciliationRuns() : Promise.resolve({ items: [] })]);
    setData((current) => ({ ...current, wallets: wallets.items || [], receivables: receivables.items || [], prices: prices.items || [], adjustments: adjustments.items || [], holds: holds.items || [], reconciliations: reconciliations.items || [] }));
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
    [busy, client, copy, language, normalRefundCreationEnabled, repairSupported],
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
      <DetailDrawer label={`${copy.billing.holdEvidence} · ${shortID(holdDetail.hold.hold_id)}`} onClose={() => setHoldDetail(null)}>
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
      </DetailDrawer>
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
      <DetailDrawer label={`${copy.billing.anomalyDetail} · ${item.anomaly_type}`} onClose={() => setAnomalyDetail(null)}>
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
      </DetailDrawer>
    );
  }
  if (lateReviewDetail) {
    const item = lateReviewDetail.case;
    const active = ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND'].includes(item.review_status);
    return (
      <DetailDrawer label={`${copy.billing.reviewDetail} · ${item.merchant_order_no}`} onClose={() => setLateReviewDetail(null)}>
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
      </DetailDrawer>
    );
  }
  if (orderDetail)
    return (
      <DetailDrawer label={`${copy.billing.orderDetail} · ${orderDetail.merchant_order_no}`} onClose={() => setOrderDetail(null)}>
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
                canRequestRepair && normalRefundCreationEnabled && item.status === 'ACTIVE' ? (
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
      </DetailDrawer>
    );
  return (
    <section className="adm-panel">
      <SectionHeader title={surface === 'payments' ? copy.billing.paymentsTitle : copy.billing.title} count={items.length} copy={copy} />
      {surface === 'payments' && hasLimitedPaymentCapabilities && (
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
            {['CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW'].map((status) => (
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
    <DetailDrawer label={title} onClose={onBack}>
    <section className="adm-panel">
      <button className="adm-back" onClick={onBack}>
        ← {copy.billing.back}
      </button>
      <SectionHeader title={title} count={items.length} copy={copy} />
      <Table columns={columns} items={items} copy={copy} />
    </section>
    </DetailDrawer>
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
              <span className="adm-id-with-type">
                <span>{item.owner_type}</span>
                <TechnicalID value={item.owner_id} copy={copy} />
              </span>
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
              render: (item) => <TechnicalID value={item.upstream_request_id} copy={copy} />,
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
                render: (item) => <TechnicalID value={item.model_id} copy={copy} />,
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
                render: (item) => <TechnicalID value={item.provisioning_id} copy={copy} />,
              },
              {
                key: 'scope_type',
                label: copy.tokens.scope,
                render: (item) => `${item.scope_type} · ${shortID(item.scope_id)}`,
              },
              {
                key: 'model_id',
                label: copy.tokens.modelID,
                render: (item) => <TechnicalID value={item.model_id} copy={copy} />,
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
    <DetailDrawer label={`${copy.tokens.accountDetail} · ${account.name}`} onClose={onBack}>
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
    </DetailDrawer>
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
    [copy.tokens.upstreamReference, <TechnicalID value={item.upstream_request_id} copy={copy} compact={false} />],
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
    <DetailDrawer label={`${copy.tokens.usageDetail} · ${shortID(item.usage_request_id)}`} onClose={onBack}>
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
    </DetailDrawer>
  );
}

function SystemStatus({ copy, data, permissions }) {
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
      render: (item) => <TechnicalID value={item.resource} copy={copy} />,
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
      <div className="adm-system-sections">
        <article className="adm-panel">
          <SectionHeader title={copy.system.services} count={status.services.length} copy={copy} />
          <Table columns={serviceColumns} items={status.services} copy={copy} />
        </article>
        <article className="adm-panel">
          <SectionHeader title={copy.system.relays} count={status.relays.length} copy={copy} />
          <Table columns={relayColumns} items={status.relays} copy={copy} />
        </article>
        <article className="adm-panel">
          <SectionHeader title={copy.system.recentErrors} count={status.recent_errors.length} copy={copy} />
          <Table columns={errorColumns} items={status.recent_errors} copy={copy} />
        </article>
      </div>
      <RunbookBoundary copy={copy} permissions={permissions} mode="system" />
    </section>
  );
}

function RunbookBoundary({ copy, permissions, mode }) {
  const permissionSet = new Set(permissions || []);
  const entries = mode === 'system'
    ? [
        permissionSet.has('platform.system.status.read') && ['bootstrap', copy.runbooks.bootstrap, copy.runbooks.bootstrapHelp],
        permissionSet.has('platform.system.status.read') && ['idp', copy.runbooks.idp, copy.runbooks.idpHelp],
      ]
    : [
        permissionSet.has('platform.audit.read') && ['breakGlass', copy.runbooks.breakGlass, copy.runbooks.breakGlassHelp],
        permissionSet.has('platform.audit.read') && ['serviceIdentities', copy.runbooks.serviceIdentities, copy.runbooks.serviceIdentitiesHelp],
      ];
  const visibleEntries = entries.filter(Boolean);
  if (!visibleEntries.length) return null;
  return (
    <section className="adm-panel adm-runbook-panel">
      <SectionHeader title={copy.runbooks.title} copy={copy} />
      <p className="adm-help">{copy.runbooks.lead}</p>
      <div className="adm-runbook-list">
        {visibleEntries.map(([key, title, help]) => (
          <details key={key}>
            <summary>
              <strong>{title}</strong>
              <Status value="OUT_OF_BAND" />
            </summary>
            <p>{help}</p>
            <p className="adm-help">{copy.runbooks.noExecution}</p>
          </details>
        ))}
      </div>
    </section>
  );
}

function RepairQueue({ title, lead, count, unavailable, forceOpen = false, copy, children }) {
  return (
    <details className="adm-repair-queue" open={count > 0 || unavailable || forceOpen}>
      <summary><span><strong>{title}</strong><small>{lead}</small></span><b>{count}</b></summary>
      {unavailable ? <div className="adm-inline-message error">{copy.userManagement.queueUnavailable}</div> : count || forceOpen ? children : <div className="adm-repair-empty">{copy.userManagement.noRepairs}</div>}
    </details>
  );
}

function UserRepairWorkspace({ copy, data, client, setData, permissions }) {
  const permissionSet = new Set(permissions || []);
  const canRegistration = permissionSet.has('platform.registrations.repair');
  const canProfile = permissionSet.has('platform.profile-changes.repair');
  const canReadClosures = permissionSet.has('platform.account-closures.read');
  const canRepairClosures = permissionSet.has('platform.account-closures.repair');
  if (!canRegistration && !canProfile && !canReadClosures) return null;
  const registrations = data.registrationRepairs || [];
  const profiles = data.profileChangeRepairs || [];
  const closures = data.accountClosures || [];
  const total = registrations.length + profiles.length + closures.length;
  return (
    <section className="adm-panel adm-repair-workspace">
      <SectionHeader title={copy.userManagement.exceptionWorkbench} count={total} copy={copy} />
      <p className="adm-help">{copy.userManagement.exceptionLead}</p>
      {canRegistration && <RegistrationRepairQueue copy={copy} items={registrations} unavailable={data.userRepairAvailability?.registrations === 'error'} client={client} setData={setData} />}
      {canProfile && <ProfileRepairQueue copy={copy} items={profiles} unavailable={data.userRepairAvailability?.profileChanges === 'error'} client={client} setData={setData} />}
      {canReadClosures && <AccountClosureOperations copy={copy} closures={closures} unavailable={data.userRepairAvailability?.closures === 'error'} client={client} setData={setData} canRepair={canRepairClosures} />}
    </section>
  );
}

function RegistrationRepairQueue({ copy, items, unavailable, client, setData }) {
  const [filters, setFilters] = useState({ status: '', stage: '', responsibility: '', sla: '', older_than_minutes: '' });
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const hasActiveFilters = Object.values(filters).some(Boolean);
  const reload = async (nextFilters = filters) => {
    const result = await client.registrationRepairs(nextFilters);
    setData((current) => ({ ...current, registrationRepairs: result.items || [] }));
  };
  const run = async (item, action) => {
    if (action === 'compensate' && !window.confirm(copy.userManagement.confirmCompensation)) return;
    setBusy(`${item.registration_operation_id}:${action}`);
    setError('');
    try {
      if (action === 'compensate') await client.compensateRegistration(item.registration_operation_id);
      else await client.retryRegistration(item.registration_operation_id);
      await reload();
    } catch {
      setError(copy.userManagement.repairFailed);
    } finally {
      setBusy('');
    }
  };
  const columns = [
    { key: 'registration_operation_id', label: copy.userManagement.operation, render: (item) => <TechnicalID value={item.registration_operation_id} copy={copy} /> },
    { key: 'stage', label: copy.closure.stage, render: (item) => <Status value={item.stage} /> },
    { key: 'status', label: copy.accounts.status, render: (item) => <Status value={item.status} /> },
    { key: 'responsibility', label: copy.userManagement.responsibility },
    { key: 'attempts', label: copy.userManagement.attempts },
    { key: 'reason', label: copy.userManagement.reason, render: (item) => item.reason || '—' },
    { key: 'updated_at', label: copy.billing.updated, render: (item) => formatDate(item.updated_at) },
    { key: 'actions', label: copy.accounts.actions, render: (item) => <div className="adm-row-actions"><button disabled={Boolean(busy)} onClick={() => run(item, 'retry')}>{busy === `${item.registration_operation_id}:retry` ? '…' : copy.userManagement.retry}</button>{item.compensation_allowed && <button className="danger" disabled={Boolean(busy)} onClick={() => run(item, 'compensate')}>{busy === `${item.registration_operation_id}:compensate` ? '…' : copy.userManagement.compensate}</button>}</div> },
  ];
  const submitFilters = async (event) => {
    event.preventDefault();
    setError('');
    try { await reload(); } catch { setError(copy.userManagement.queueUnavailable); }
  };
  return (
    <RepairQueue title={copy.userManagement.registrationRepair} lead={copy.userManagement.registrationRepairLead} count={items.length} unavailable={unavailable} forceOpen={hasActiveFilters || Boolean(error)} copy={copy}>
      {error && <div className="adm-inline-message error">{error}</div>}
      <form className="adm-repair-filters" onSubmit={submitFilters}>
        {[
          ['status', copy.accounts.status, ['', 'REPAIR_REQUIRED', 'COMPENSATION_PENDING']],
          ['stage', copy.closure.stage, ['', 'IDENTITY_CREATING', 'ACCOUNT_COMMITTING', 'WALLET_PROVISIONING', 'COMPENSATION']],
          ['responsibility', copy.userManagement.responsibility, ['', 'IDENTITY', 'ACCOUNT', 'BILLING']],
          ['sla', copy.userManagement.sla, ['', 'OVERDUE', 'WITHIN']],
        ].map(([key, label, options]) => <label key={key}><span>{label}</span><select value={filters[key]} onChange={(event) => setFilters((current) => ({ ...current, [key]: event.target.value }))}>{options.map((option) => <option key={option || 'all'} value={option}>{option || copy.userManagement.all}</option>)}</select></label>)}
        <label><span>{copy.userManagement.olderThan}</span><input type="number" min="0" max="43200" value={filters.older_than_minutes} onChange={(event) => setFilters((current) => ({ ...current, older_than_minutes: event.target.value }))} placeholder="—" /></label>
        <button>{copy.refresh}</button>
      </form>
      <Table columns={columns} items={items} copy={copy} />
    </RepairQueue>
  );
}

function ProfileRepairQueue({ copy, items, unavailable, client, setData }) {
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const retry = async (item) => {
    if (!window.confirm(copy.userManagement.confirmRetry)) return;
    setBusy(item.profile_change_request_id);
    setError('');
    try {
      await client.retryProfileChange(item.profile_change_request_id);
      const result = await client.profileChangeRepairs();
      setData((current) => ({ ...current, profileChangeRepairs: result.items || [] }));
    } catch { setError(copy.userManagement.repairFailed); } finally { setBusy(''); }
  };
  const columns = [
    { key: 'profile_change_request_id', label: copy.userManagement.request, render: (item) => <TechnicalID value={item.profile_change_request_id} copy={copy} /> },
    { key: 'user_id', label: copy.closure.user, render: (item) => <TechnicalID value={item.user_id} copy={copy} /> },
    { key: 'field', label: copy.userManagement.field },
    { key: 'status', label: copy.accounts.status, render: (item) => <Status value={item.status} /> },
    { key: 'attempts', label: copy.userManagement.attempts },
    { key: 'reason', label: copy.userManagement.reason, render: (item) => item.reason || '—' },
    { key: 'actions', label: copy.accounts.actions, render: (item) => <button disabled={Boolean(busy)} onClick={() => retry(item)}>{busy === item.profile_change_request_id ? '…' : copy.userManagement.retry}</button> },
  ];
  return <RepairQueue title={copy.userManagement.profileRepair} lead={copy.userManagement.profileRepairLead} count={items.length} unavailable={unavailable} copy={copy}>{error && <div className="adm-inline-message error">{error}</div>}<Table columns={columns} items={items} copy={copy} /></RepairQueue>;
}

function AccountClosureOperations({ copy, closures, unavailable, client, setData, canRepair }) {
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
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
        canRepair ? (
          <button disabled={Boolean(busy)} onClick={() => retryClosure(item)}>
            {busy === item.closure_request_id ? '…' : copy.closure.retry}
          </button>
        ) : (
          '—'
        ),
    },
  ];
  return (
    <RepairQueue title={copy.closure.title} lead={copy.closure.lead} count={closures.length} unavailable={unavailable} copy={copy}>
      {error && <div className="adm-inline-message error">{error}</div>}
      <Table columns={closureColumns} items={closures} copy={copy} />
    </RepairQueue>
  );
}

function KeycloakSecurityAccess({ copy }) {
  const [copied, setCopied] = useState('');
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
  const [filters, setFilters] = useState(() => readAuditQueryFilters());
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
          <TechnicalID value={item.resource_id} copy={copy} />
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
          <TechnicalID value={item.actor_user_id} copy={copy} />
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
      render: (item) => <TechnicalID value={item.request_id} copy={copy} />,
    },
    {
      key: 'occurred_at',
      label: copy.audit.occurred,
      render: (item) => formatDate(item.occurred_at),
    },
  ];
  return (
    <>
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
    <RunbookBoundary copy={copy} permissions={permissions} mode="audit" />
    </>
  );
}

function AuditDetail({ copy, item, onBack }) {
  const rows = [
    [copy.audit.source, 'Account Service'],
    [copy.audit.actor, item.actor_display_name || item.actor_email || <TechnicalID value={item.actor_user_id} copy={copy} compact={false} />],
    [copy.audit.action, item.action],
    [copy.audit.resource, <span>{item.resource_type} · <TechnicalID value={item.resource_id} copy={copy} compact={false} /></span>],
    [copy.audit.result, <Status value={item.result} />],
    [copy.audit.request, <TechnicalID value={item.request_id} copy={copy} compact={false} />],
    [copy.audit.organization, <TechnicalID value={item.active_organization_id} copy={copy} compact={false} />],
    [copy.audit.occurred, formatDate(item.occurred_at)],
  ];
  return (
    <DetailDrawer label={copy.audit.detail} onClose={onBack}>
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
    </DetailDrawer>
  );
}
