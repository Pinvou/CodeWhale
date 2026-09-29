import { secureRequestID } from './request-id.js';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { buildUnitTree } from './admin-organization-tree.js';
import { adjustmentApprovalAction, buildPaymentControlApprovalDraft, createFinanceTabRequestTracker, editWalletAdjustmentApproval, financeTabShouldLoad, latestApprovalByTarget, paymentControlApprovalAction, paymentControlSupersedesRequest, readFinanceTabQuery, resetFinanceSurfaceLoading, submitPaymentControlApproval, updateFinanceTabState, updatePaymentCapabilityState, walletAdjustmentApprovalDraft, walletAdjustmentReplacementReference, walletReplacementEligible } from './admin-finance.js';
import { createAndSubmitGovernedApproval, updateAndSubmitGovernedApproval } from './admin-governed-approval-workflow.js';
import { acquireAuditExportTicket, newAuditExportAttempt, submitAuditExportAttempt } from './admin-audit-export.js';
import { formatPoints, formatUsagePoints, MICROCREDITS_PER_POINT, parsePointsInputToMicrocredits } from './credit-units.js';
import { adminSectionReadOnly, adminVisibleSections, authorizationVersionChanged, authorizationVersions, hasPermission } from './authorization-ui.js';
import { adminFinanceDetailUpdates, adminFinanceQueryUpdates, adminModelDetailsGateEnabled, adminModelOperationsDetailsGateEnabled, adminModelOperationsOverviewGateEnabled, adminModelPricingEditGateEnabled, adminModelTokenActionsGateEnabled, adminModelTokenIAGateEnabled, adminModelTokenOverviewRoute, adminModelTokenPageOverviewsGateEnabled, adminNavigationGroupsForGate, adminTokenDetailsGateEnabled, adminTokenResourceFormsGateEnabled, clearAdminFinanceDetailUpdates, clearAdminFinanceQueryUpdates, clearAdminModelTokenUpdates, isAdminFinancePaginatedTab, normalizeAdminFinanceDetailQuery, normalizeAdminFinanceQuery, normalizeAdminModelTokenEntry, normalizeAdminModelTokenNavigation, readAdminFinanceDetail, readAdminFinanceQuery, readAdminFinanceTab, readAdminOrganizationDirectory, readAdminOrganizationRoute, readAdminSection, readAdminUserRoute, resolveAdminFinanceTab, writeAdminSection } from './admin-navigation.js';
import { createFinanceDetailRecovery, financeDetailRouteIsCurrent, isFinanceSnapshotExpired, normalizeFinanceCollection, previousFinanceCursor, rememberFinanceCursor, resetExpiredFinanceQuery, runFinanceDetailRequest } from './admin-finance-pagination.js';
import { AdminRolesWorkspace } from './AdminRolesWorkspace.jsx';
import { AdminApprovalWorkspace } from './AdminApprovalWorkspace.jsx';
import { AdminModelTokenWorkspace } from './AdminModelTokenWorkspace.jsx';
import { PricingLifecycleWorkspace } from './PricingLifecycleWorkspace.jsx';
import { AdminOperationsWorkspace } from './AdminOperationsWorkspace.jsx';
import { createFinanceIdempotencyKey, executeStructuredFinanceAction, financeActionRequiresRefresh, isUnknownMutationOutcome, paymentAnomalyResolutionOptions } from './finance-actions.js';
import { financeOverviewDrilldown, financeOverviewMetricTimeScope, formatFinanceOverviewValue } from './admin-finance-overview.js';
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
  models: '□',
  tokenResources: '◇',
  modelOperations: '↗',
  approvals: '✓',
  system: '◉',
  audit: '≡',
  settings: '⚙',
  operations: '⌘',
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
function formatUTCDate(value) {
  return value ? new Date(value).toLocaleString(undefined, { timeZone: 'UTC' }) : '—';
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

function PageBreadcrumbs({ copy, groupLabel, groupTarget, section, sectionLabel, detailLabel, onNavigate }) {
  return (
    <nav className="adm-breadcrumbs" aria-label={copy.pageNavigation}>
      <ol>
        <li>
          {groupTarget && groupTarget !== section
            ? <button type="button" onClick={() => onNavigate(groupTarget)}>{groupLabel}</button>
            : <span>{groupLabel}</span>}
        </li>
        <li aria-hidden="true" className="adm-breadcrumb-separator">/</li>
        <li>
          {detailLabel
            ? <button type="button" onClick={() => onNavigate(section)}>{sectionLabel}</button>
            : <span aria-current="page">{sectionLabel}</span>}
        </li>
        {detailLabel && <>
          <li aria-hidden="true" className="adm-breadcrumb-separator">/</li>
          <li><span aria-current="page">{detailLabel}</span></li>
        </>}
      </ol>
    </nav>
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

function StructuredActionPanel({ operation, copy, busy, onChange, onClose, onRefresh, onSubmit }) {
  if (!operation) return null;
  const labels = copy.billing.actionPanel;
  const refreshRequired = financeActionRequiresRefresh(operation);
  return (
    <DetailDrawer label={operation.title} onClose={busy || refreshRequired ? () => {} : onClose}>
      <section className="adm-panel adm-action-panel">
        <SectionHeader title={operation.title} copy={copy}>
          <button type="button" disabled={busy || refreshRequired} onClick={onClose}>{labels.cancel}</button>
        </SectionHeader>
        <p className="adm-help">{operation.help || labels.help}</p>
        <dl className="adm-detail-list adm-action-summary">
          {(operation.summary || []).map((item) => <div key={item.label}><dt>{item.label}</dt><dd>{item.value || '—'}</dd></div>)}
          {operation.idempotencyKey && <div><dt>{labels.idempotencyKey}</dt><dd><code>{operation.idempotencyKey}</code></dd></div>}
        </dl>
        <form className="adm-action-form" onSubmit={onSubmit}>
          {(operation.fields || []).map((field) => {
            const fieldID = `finance-action-${field.name}`;
            const helpID = `${fieldID}-help`;
            const errorID = `${fieldID}-error`;
            const describedBy = [field.help ? helpID : '', operation.fieldErrors?.[field.name] ? errorID : ''].filter(Boolean).join(' ') || undefined;
            return (
            <label key={field.name} htmlFor={fieldID}>
              <span>{field.label}</span>
              {field.type === 'directory-search' ? (
                <div className="adm-action-search">
                  <input id={fieldID} value={operation.values[field.name] || ''} onChange={(event) => onChange(field.name, event.target.value)} placeholder={field.placeholder} aria-describedby={describedBy} aria-invalid={Boolean(operation.fieldErrors?.[field.name])} />
                  <button type="button" disabled={busy || operation.lookupBusy} onClick={() => field.onSearch(operation.values[field.name] || '')}>
                    {operation.lookupBusy ? labels.searching : labels.searchUsers}
                  </button>
                </div>
              ) : field.type === 'select' ? (
                <select id={fieldID} value={operation.values[field.name] || ''} onChange={(event) => onChange(field.name, event.target.value)} required={field.required} aria-describedby={describedBy} aria-invalid={Boolean(operation.fieldErrors?.[field.name])}>
                  <option value="">{labels.choose}</option>
                  {(field.options || []).map((option) => {
                    const value = typeof option === 'string' ? option : option.value;
                    const label = typeof option === 'string' ? option : option.label;
                    return <option key={value} value={value}>{label}</option>;
                  })}
                </select>
              ) : field.type === 'textarea' ? (
                <textarea id={fieldID} rows={field.rows || 4} value={operation.values[field.name] || ''} onChange={(event) => onChange(field.name, event.target.value)} minLength={field.minLength} maxLength={field.maxLength} required={field.required} aria-describedby={describedBy} aria-invalid={Boolean(operation.fieldErrors?.[field.name])} />
              ) : (
                <input id={fieldID} type={field.type || 'text'} value={operation.values[field.name] || ''} onChange={(event) => onChange(field.name, event.target.value)} minLength={field.minLength} maxLength={field.maxLength} required={field.required} aria-describedby={describedBy} aria-invalid={Boolean(operation.fieldErrors?.[field.name])} />
              )}
              {field.help && <small id={helpID}>{field.help}</small>}
              {operation.fieldErrors?.[field.name] && <small id={errorID} className="adm-field-error">{operation.fieldErrors[field.name]}</small>}
            </label>
            );
          })}
          {operation.error && <div className="adm-inline-message error" role="alert" aria-live="assertive">{operation.error}</div>}
          <div className="adm-action-confirmation">
            <strong>{labels.confirmation}</strong>
            <span>{operation.confirmation}</span>
          </div>
          <div className="adm-row-actions">
            <button type="button" disabled={busy || refreshRequired} onClick={onClose}>{labels.cancel}</button>
            {refreshRequired ? (
              <button type="button" disabled={busy} onClick={onRefresh}>{busy ? labels.refreshing : operation.restartPagination ? copy.billing.restartPagination : labels.refreshStatus}</button>
            ) : (
              <button className={operation.danger ? 'danger' : ''} disabled={busy}>{busy ? labels.submitting : labels.confirm}</button>
            )}
          </div>
        </form>
      </section>
    </DetailDrawer>
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

function FinanceLimitSelect({ copy, value, onChange }) {
  return (
    <label>
      {copy.billing.pageSize}
      <select value={value} onChange={onChange}>
        {[25, 50, 100, 200].map((limit) => <option key={limit} value={String(limit)}>{limit}</option>)}
      </select>
    </label>
  );
}

function FinanceDateTimeRange({ copy, value, onChange }) {
  return (
    <>
      <label>{copy.billing.createdFrom}<input type="datetime-local" value={value.created_from || ''} onChange={(event) => onChange('created_from', event.target.value)} /></label>
      <label>{copy.billing.createdTo}<input type="datetime-local" value={value.created_to || ''} onChange={(event) => onChange('created_to', event.target.value)} /></label>
    </>
  );
}

function FinancePagination({ copy, count, limit, page, canPrevious, loading, onPrevious, onNext }) {
  if (!page) return null;
  if (page.legacy) return <div className="adm-inline-message info">{copy.billing.legacyPagination}</div>;
  return (
    <div className="adm-pagination adm-finance-pagination">
      <span>{copy.billing.currentPageRange(count, limit)}</span>
      {page.snapshotAsOf && <small>{copy.billing.snapshotAsOf}: {formatDate(page.snapshotAsOf)}</small>}
      <div className="adm-row-actions">
        <button type="button" disabled={loading || page.expired || !canPrevious} onClick={onPrevious}>{copy.billing.previousPage}</button>
        <button type="button" disabled={loading || page.expired || !page.nextCursor} onClick={onNext}>{copy.billing.nextPage}</button>
      </div>
    </div>
  );
}

function AdminExtensionSlot({ section, financeTab, authorization }) {
  const hostRef = useRef(null);
  const platformPermissions = new Set(authorization?.platform_permissions || []);
  const authorizationVersion = JSON.stringify(authorizationVersions(authorization));
  const visible = {
    'approval-requests-root': section === 'approvals',
    'product-admin-root': section === 'products' && financeTab === 'catalog' && platformPermissions.has('platform.products.read'),
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
      if (node) {
        node.hidden = !isVisible;
        node.dispatchEvent(new CustomEvent('pinvou-admin-extension-visibility', { detail: { visible: isVisible, authorizationVersion } }));
      }
    }
  }, [section, financeTab, authorization]);

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
  const [modelTokenIAEnabled, setModelTokenIAEnabled] = useState(false);
  const [tokenResourceFormsEnabled, setTokenResourceFormsEnabled] = useState(false);
  const [modelOperationsDetailsEnabled, setModelOperationsDetailsEnabled] = useState(false);
  const [modelOperationsOverviewEnabled, setModelOperationsOverviewEnabled] = useState(false);
  const [modelTokenPageOverviewsEnabled, setModelTokenPageOverviewsEnabled] = useState(false);
  const [modelDetailsEnabled, setModelDetailsEnabled] = useState(false);
	const [modelPricingEditEnabled, setModelPricingEditEnabled] = useState(false);
	const [tokenDetailsEnabled, setTokenDetailsEnabled] = useState(false);
	const [credentialOrigin, setCredentialOrigin] = useState('');
  const [modelTokenActionsEnabled, setModelTokenActionsEnabled] = useState(false);
  const [modelTokenRouteNotice, setModelTokenRouteNotice] = useState('');
  const [breadcrumbDetail, setBreadcrumbDetail] = useState({ routeKey: '', label: '' });
  const authorizationRef = useRef(null);
  const authorizationRefreshRef = useRef(false);
  const financeRequestTrackerRef = useRef(null);
  if (!financeRequestTrackerRef.current) financeRequestTrackerRef.current = createFinanceTabRequestTracker();
  const updateBreadcrumbDetail = useCallback((routeKey, label) => {
    setBreadcrumbDetail((current) => (current.routeKey === routeKey && current.label === label ? current : { routeKey, label }));
  }, []);

  const load = useCallback(
    async (target, signal, granted = authorizationRef.current) => {
      setBusy(true);
      setError('');
      try {
        const allowed = (permission) => hasPermission(granted, permission);
        const read = (permission, action, fallback = { items: [] }) => (allowed(permission) ? action() : Promise.resolve(fallback));
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
        } else if (target === 'billing' || target === 'payments' || target === 'products' || target === 'models' || target === 'tokenResources' || target === 'modelOperations' || target === 'forbidden') {
          next = {};
        } else if (target === 'approvals' || target === 'settings' || target === 'operations') {
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
        const iaEnabled = adminModelTokenIAGateEnabled(session);
        setModelTokenIAEnabled(iaEnabled);
        const resourceFormsEnabled = adminTokenResourceFormsGateEnabled(session);
        setTokenResourceFormsEnabled(resourceFormsEnabled);
        const operationsDetailsEnabled = adminModelOperationsDetailsGateEnabled(session);
        setModelOperationsDetailsEnabled(operationsDetailsEnabled);
        const operationsOverviewEnabled = adminModelOperationsOverviewGateEnabled(session);
        setModelOperationsOverviewEnabled(operationsOverviewEnabled);
        const pageOverviewsEnabled = adminModelTokenPageOverviewsGateEnabled(session);
        setModelTokenPageOverviewsEnabled(pageOverviewsEnabled);
        const detailsEnabled = adminModelDetailsGateEnabled(session);
        setModelDetailsEnabled(detailsEnabled);
				const pricingEditEnabled = adminModelPricingEditGateEnabled(session);
				setModelPricingEditEnabled(pricingEditEnabled);
				const tokenDetailGate = adminTokenDetailsGateEnabled(session);
				setTokenDetailsEnabled(tokenDetailGate);
				setCredentialOrigin(tokenDetailGate ? session.credential_origin : '');
        const actionsEnabled = adminModelTokenActionsGateEnabled(session);
        setModelTokenActionsEnabled(actionsEnabled);
        const visible = adminVisibleSections(granted, { modelTokenIAEnabled: iaEnabled });
        const requested = readAdminSection();
				const normalized = normalizeAdminModelTokenEntry(requested, visible, iaEnabled, undefined, granted.platform_permissions || [], { tokenResourceFormsEnabled: resourceFormsEnabled, modelOperationsDetailsEnabled: operationsDetailsEnabled, modelOperationsOverviewEnabled: operationsOverviewEnabled, modelTokenPageOverviewsEnabled: pageOverviewsEnabled, modelDetailsEnabled: detailsEnabled, modelPricingEditEnabled: pricingEditEnabled, tokenDetailsEnabled: tokenDetailGate, modelTokenActionsEnabled: actionsEnabled });
        if (visible.length === 0) {
          if (normalized.replace) writeAdminSection(normalized.section, 'replace', undefined, undefined, normalized.updates);
          setAuth('denied');
          return;
        }
        authorizationRef.current = granted;
        setAuthorization(granted);
        const target = normalized.section || visible[0];
        setSection(target);
        setModelTokenRouteNotice(normalized.notice || '');
        writeAdminSection(target, 'replace', undefined, undefined, normalized.updates);
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
        const [next, nextSession] = await Promise.all([client.authorization(), client.session()]);
        if (disposed) return;
        if (!nextSession.authenticated) { setAuth('guest'); return; }
        if (retryTimer !== null) {
          globalThis.clearTimeout?.(retryTimer);
          retryTimer = null;
        }
        const current = authorizationRef.current;
        const permissionsChanged = JSON.stringify(current?.platform_permissions || []) !== JSON.stringify(next?.platform_permissions || []);
        const nextIAEnabled = adminModelTokenIAGateEnabled(nextSession);
        const nextResourceFormsEnabled = adminTokenResourceFormsGateEnabled(nextSession);
        const nextOperationsDetailsEnabled = adminModelOperationsDetailsGateEnabled(nextSession);
        const nextOperationsOverviewEnabled = adminModelOperationsOverviewGateEnabled(nextSession);
        const nextPageOverviewsEnabled = adminModelTokenPageOverviewsGateEnabled(nextSession);
        const nextDetailsEnabled = adminModelDetailsGateEnabled(nextSession);
				const nextPricingEditEnabled = adminModelPricingEditGateEnabled(nextSession);
				const nextTokenDetailsEnabled = adminTokenDetailsGateEnabled(nextSession);
        const nextActionsEnabled = adminModelTokenActionsGateEnabled(nextSession);
				const gatesChanged = nextIAEnabled !== modelTokenIAEnabled || nextResourceFormsEnabled !== tokenResourceFormsEnabled || nextOperationsDetailsEnabled !== modelOperationsDetailsEnabled || nextOperationsOverviewEnabled !== modelOperationsOverviewEnabled || nextPageOverviewsEnabled !== modelTokenPageOverviewsEnabled || nextDetailsEnabled !== modelDetailsEnabled || nextPricingEditEnabled !== modelPricingEditEnabled || nextTokenDetailsEnabled !== tokenDetailsEnabled || nextActionsEnabled !== modelTokenActionsEnabled;
        if (permissionsChanged || authorizationVersionChanged(authorizationVersions(current), authorizationVersions(next)) || gatesChanged) {
          const visible = adminVisibleSections(next, { modelTokenIAEnabled: nextIAEnabled });
          authorizationRef.current = next;
          setAuthorization(next);
          setModelTokenIAEnabled(nextIAEnabled);
          setTokenResourceFormsEnabled(nextResourceFormsEnabled);
          setModelOperationsDetailsEnabled(nextOperationsDetailsEnabled);
          setModelOperationsOverviewEnabled(nextOperationsOverviewEnabled);
          setModelTokenPageOverviewsEnabled(nextPageOverviewsEnabled);
          setModelDetailsEnabled(nextDetailsEnabled);
					setModelPricingEditEnabled(nextPricingEditEnabled);
					setTokenDetailsEnabled(nextTokenDetailsEnabled);
					setCredentialOrigin(nextTokenDetailsEnabled ? nextSession.credential_origin : '');
          setModelTokenActionsEnabled(nextActionsEnabled);
          financeRequestTrackerRef.current.invalidateAll();
          setData({});
					const normalized = normalizeAdminModelTokenEntry(section, visible, nextIAEnabled, undefined, next.platform_permissions || [], { tokenResourceFormsEnabled: nextResourceFormsEnabled, modelOperationsDetailsEnabled: nextOperationsDetailsEnabled, modelOperationsOverviewEnabled: nextOperationsOverviewEnabled, modelTokenPageOverviewsEnabled: nextPageOverviewsEnabled, modelDetailsEnabled: nextDetailsEnabled, modelPricingEditEnabled: nextPricingEditEnabled, tokenDetailsEnabled: nextTokenDetailsEnabled, modelTokenActionsEnabled: nextActionsEnabled });
          if (visible.length === 0) {
            if (normalized.replace) writeAdminSection(normalized.section, 'replace', undefined, undefined, normalized.updates);
            setAuth('denied');
            return;
          }
          const target = normalized.section || visible[0];
          setSection(target);
          setModelTokenRouteNotice(normalized.notice || '');
          writeAdminSection(target, 'replace', undefined, undefined, normalized.updates);
          setRouteVersion((value) => value + 1);
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
	}, [auth, client, load, modelDetailsEnabled, modelPricingEditEnabled, tokenDetailsEnabled, modelOperationsDetailsEnabled, modelOperationsOverviewEnabled, modelTokenPageOverviewsEnabled, modelTokenActionsEnabled, modelTokenIAEnabled, section, tokenResourceFormsEnabled]);

  useEffect(() => {
    if (auth !== 'ready' || !authorization) return undefined;
    const onPopState = () => {
      const requested = readAdminSection();
      const visible = adminVisibleSections(authorization, { modelTokenIAEnabled });
			const normalized = normalizeAdminModelTokenEntry(requested, visible, modelTokenIAEnabled, undefined, authorization.platform_permissions || [], { tokenResourceFormsEnabled, modelOperationsDetailsEnabled, modelOperationsOverviewEnabled, modelTokenPageOverviewsEnabled, modelDetailsEnabled, modelPricingEditEnabled, tokenDetailsEnabled, modelTokenActionsEnabled });
      if (!normalized.section) {
        writeAdminSection(section, 'replace');
        return;
      }
      if (normalized.replace) writeAdminSection(normalized.section, 'replace', undefined, undefined, normalized.updates);
      setModelTokenRouteNotice(normalized.notice || '');
      if (normalized.section === section) {
        setRouteVersion((value) => value + 1);
        return;
      }
      setSection(normalized.section);
      setRouteVersion((value) => value + 1);
      if (!sectionHasData(normalized.section, data)) void load(normalized.section, undefined, authorization);
    };
    globalThis.addEventListener?.('popstate', onPopState);
    return () => globalThis.removeEventListener?.('popstate', onPopState);
	}, [auth, authorization, data, load, modelDetailsEnabled, modelPricingEditEnabled, tokenDetailsEnabled, modelOperationsDetailsEnabled, modelOperationsOverviewEnabled, modelTokenPageOverviewsEnabled, modelTokenActionsEnabled, modelTokenIAEnabled, section, tokenResourceFormsEnabled]);

  const navigate = (key, query = {}) => {
    if (!adminVisibleSections(authorization, { modelTokenIAEnabled }).includes(key)) return;
    const modelTokenQuery = key === 'modelOperations' && !query.operations_tab
      ? { operations_tab: platformPermissions.includes('platform.tokens.read') ? 'provisionings' : 'usage', ...query }
      : query;
    setSection(key);
    writeAdminSection(key, 'push', undefined, undefined, {
      role: null,
      role_scope: null,
      role_view: null,
      audit_action: null,
      audit_resource: null,
      organization_id: null,
      organization_tab: null,
      ...(key === 'organizations' ? {} : { organization_q: null, organization_status: null, organization_cursor: null }),
      user_id: null,
      finance_tab: null,
      ...clearAdminFinanceQueryUpdates(),
      ...clearAdminFinanceDetailUpdates(),
      product_cursor: null,
      product_limit: null,
      product_lifecycle_status: null,
      product_include_deleted: null,
      approval_command_type: null,
      approval_target: null,
      approval_view: null,
      approval_status: null,
      approval_cursor: null,
      approval_request_id: null,
      ...clearAdminModelTokenUpdates({ includeLegacy: key !== 'tokens' }),
      ...modelTokenQuery,
    });
    globalThis.dispatchEvent?.(new CustomEvent('pinvou-admin-route-change'));
    setRouteVersion((value) => value + 1);
    if (!sectionHasData(key, data)) load(key, undefined, authorization);
  };
  const logout = async () => {
    setError('');
    try {
      await client.logout();
      setAuth('guest');
      window.location.assign('/admin.html');
    } catch {
      setError(copy.failed);
    }
  };

  if (auth === 'loading') return <StatePage copy={copy} kind="loading" />;
  if (auth === 'guest') return <StatePage copy={copy} kind="guest" />;
  if (auth === 'denied') return <StatePage copy={copy} kind="denied" />;
  if (auth === 'error') return <StatePage copy={copy} kind="error" onRetry={() => window.location.reload()} />;

  const visibleSections = adminVisibleSections(authorization, { modelTokenIAEnabled });
  const visibleGroups = adminNavigationGroupsForGate(modelTokenIAEnabled)
    .map((group) => ({ ...group, sections: group.sections.filter((key) => visibleSections.includes(key)) }))
    .filter((group) => group.sections.length > 0);
  const platformPermissions = authorization?.platform_permissions || [];
  const sectionIsReadOnly = section === 'settings' || section === 'overview' || section === 'operations' || section === 'forbidden' ? false : adminSectionReadOnly(authorization, section);
  const currentGroup = visibleGroups.find((group) => group.sections.includes(section));
  const organizationRoute = section === 'organizations' ? readAdminOrganizationRoute() : { organizationID: '', tab: 'overview' };
  const userRoute = section === 'users' ? readAdminUserRoute() : { userID: '' };
  const organizationDirectory = section === 'organizations' ? readAdminOrganizationDirectory() : { q: '', status: '', cursor: '' };
  const financeTab = ['billing', 'payments', 'products'].includes(section) ? readAdminFinanceTab(section) : '';
  const financeQuery = ['billing', 'payments'].includes(section) ? readAdminFinanceQuery(section, financeTab) : {};
  const financeDetail = ['billing', 'payments'].includes(section) ? readAdminFinanceDetail(section, financeTab) : { type: '', id: '', query: {} };
  const changeFinanceTab = (nextTab, mode = 'push') => {
    writeAdminSection(section, mode, undefined, undefined, { finance_tab: nextTab, ...clearAdminFinanceDetailUpdates() });
    setRouteVersion((value) => value + 1);
  };
  const changeFinanceQuery = (nextTab, nextQuery, mode = 'push') => {
    writeAdminSection(section, mode, undefined, undefined, { finance_tab: nextTab, ...adminFinanceQueryUpdates(nextTab, nextQuery) });
    setRouteVersion((value) => value + 1);
  };
  const changeFinanceDetail = (type, id, query = {}, mode = 'push') => {
    writeAdminSection(section, mode, undefined, undefined, adminFinanceDetailUpdates(type, id, query));
    setRouteVersion((value) => value + 1);
  };
  const changeModelTokenRoute = (updates, mode = 'push') => {
		const normalized = normalizeAdminModelTokenNavigation(section, visibleSections, modelTokenIAEnabled, platformPermissions, updates, undefined, { tokenResourceFormsEnabled, modelOperationsDetailsEnabled, modelOperationsOverviewEnabled, modelTokenPageOverviewsEnabled, modelDetailsEnabled, modelPricingEditEnabled, tokenDetailsEnabled, modelTokenActionsEnabled });
    writeAdminSection(normalized.section, mode, undefined, undefined, normalized.updates);
    setModelTokenRouteNotice('');
    setRouteVersion((value) => value + 1);
  };
  const currentPageTitle = copy.nav[section] || copy.modelToken?.forbiddenTitle || copy.title;
  const currentPageLead = section === 'overview' ? copy.overview.lead : copy.pageLeads?.[section] || copy.subtitle;
  const currentGroupLabel = copy.navGroups[currentGroup?.key || (section === 'forbidden' ? 'models' : '')] || copy.product;
  const currentGroupTarget = currentGroup?.sections[0] || section;
  const organizationBreadcrumb = organizationRoute.organizationID
    ? (data.organizations || []).find((item) => item.organization_id === organizationRoute.organizationID)?.name || copy.accounts.organizationManagement
    : '';
  const breadcrumbUser = userRoute.userID ? (data.users || []).find((item) => item.user_id === userRoute.userID) : null;
  const userBreadcrumb = userRoute.userID
    ? (breadcrumbUser ? userLabel(breadcrumbUser) : copy.detailPage)
    : '';
  const detailRouteKey = organizationRoute.organizationID
    ? `organizations:${organizationRoute.organizationID}`
    : userRoute.userID ? `users:${userRoute.userID}` : '';
  const loadedDetailBreadcrumb = breadcrumbDetail.routeKey === detailRouteKey ? breadcrumbDetail.label : '';
  const detailBreadcrumb = loadedDetailBreadcrumb || organizationBreadcrumb || userBreadcrumb;

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
            <h1>{copy.title}</h1>
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
          <PageBreadcrumbs
            copy={copy}
            groupLabel={currentGroupLabel}
            groupTarget={currentGroupTarget}
            section={section}
            sectionLabel={currentPageTitle}
            detailLabel={detailBreadcrumb}
            onNavigate={navigate}
          />
          {!organizationRoute.organizationID && !userRoute.userID && <PageHeader
            eyebrow={currentGroupLabel}
            title={currentPageTitle}
            subtitle={currentPageLead}
            updatedAt={['billing', 'payments', 'products', 'approvals', 'operations', 'settings', 'models', 'tokenResources', 'modelOperations', 'forbidden'].includes(section) ? null : updatedAt}
            canRefresh={!['billing', 'payments', 'products', 'approvals', 'operations', 'settings', 'models', 'tokenResources', 'modelOperations', 'forbidden'].includes(section)}
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
          {section === 'overview' && <Overview copy={copy} language={language} data={data} permissions={platformPermissions} modelTokenIAEnabled={modelTokenIAEnabled} onNavigate={navigate} />}
          {section === 'organizations' && <AccountDirectory section="organizations" copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} onNavigate={navigate} onBreadcrumbDetailChange={updateBreadcrumbDetail} organizationRoute={organizationRoute} organizationDirectory={organizationDirectory} routeVersion={routeVersion} />}
          {section === 'users' && (
            <UsersWorkspace copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} currentUserID={authorization?.user_id || ''} onNavigate={navigate} onBreadcrumbDetailChange={updateBreadcrumbDetail} userRoute={userRoute} />
          )}
          {section === 'roles' && <AdminRolesWorkspace copy={copy} roles={data.roles || []} bindings={data.roleBindings || []} users={data.roleUsers || []} organizations={data.roleOrganizations || []} client={client} setData={setData} permissions={platformPermissions} onNavigate={navigate} />}
          {section === 'billing' && <Billing key="billing" surface="billing" routeTab={financeTab} routeQuery={financeQuery} routeDetail={financeDetail} routeVersion={routeVersion} onRouteTabChange={changeFinanceTab} onRouteQueryChange={changeFinanceQuery} onRouteDetailChange={changeFinanceDetail} onNavigate={navigate} copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} currentUserID={authorization?.user_id || ''} requestTracker={financeRequestTrackerRef.current} />}
          {section === 'payments' && <Billing key="payments" surface="payments" routeTab={financeTab} routeQuery={financeQuery} routeDetail={financeDetail} routeVersion={routeVersion} onRouteTabChange={changeFinanceTab} onRouteQueryChange={changeFinanceQuery} onRouteDetailChange={changeFinanceDetail} onNavigate={navigate} copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} requestTracker={financeRequestTrackerRef.current} />}
          {section === 'products' && <ProductsWorkspace routeTab={financeTab} onRouteTabChange={changeFinanceTab} onNavigate={navigate} copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} requestTracker={financeRequestTrackerRef.current} modelDirectoryVisible={modelTokenIAEnabled && adminVisibleSections(authorization, { modelTokenIAEnabled }).includes('models')} modelDetailsEnabled={modelDetailsEnabled} />}
          {section === 'tokens' && <Tokens copy={copy} language={language} data={data} client={client} setData={setData} permissions={platformPermissions} />}
					{['models', 'tokenResources', 'modelOperations'].includes(section) && <AdminModelTokenWorkspace section={section} copy={copy} client={client} permissions={platformPermissions} routeVersion={routeVersion} onRouteChange={changeModelTokenRoute} onNavigate={navigate} routeNotice={modelTokenRouteNotice} tokenResourceFormsEnabled={tokenResourceFormsEnabled} modelOperationsDetailsEnabled={modelOperationsDetailsEnabled} modelOperationsOverviewEnabled={modelOperationsOverviewEnabled} modelTokenPageOverviewsEnabled={modelTokenPageOverviewsEnabled} overviewUserID={authorization?.user_id || ''} overviewAuthorizationVersion={authorizationVersions(authorization).platform_authorization_version} modelDetailsEnabled={modelDetailsEnabled} modelPricingEditEnabled={modelPricingEditEnabled} tokenDetailsEnabled={tokenDetailsEnabled} credentialOrigin={credentialOrigin} modelTokenActionsEnabled={modelTokenActionsEnabled} />}
          {section === 'forbidden' && <section className="adm-card adm-source-card"><div className="adm-source-state"><strong>{copy.modelToken.forbiddenTitle}</strong><span>{copy.modelToken.forbiddenBody}</span></div></section>}
          {section === 'system' && (
            <>
              {hasPermission(authorization, 'platform.system.status.read') && <SystemStatus copy={copy} data={data} permissions={platformPermissions} />}
              {hasPermission(authorization, 'platform.users.identity.repair') && <KeycloakSecurityAccess copy={copy} />}
            </>
          )}
          {section === 'audit' && <Audit copy={copy} data={data} client={client} setData={setData} permissions={platformPermissions} />}
          {section === 'approvals' && <AdminApprovalWorkspace key={JSON.stringify(authorizationVersions(authorization))} client={client} copy={copy} language={language} currentUserID={authorization?.user_id || ''} routeVersion={routeVersion} onNavigate={navigate} visibleSections={visibleSections} modelDetailsEnabled={modelDetailsEnabled} tokenDetailsEnabled={tokenDetailsEnabled} />}
          {section === 'operations' && <AdminOperationsWorkspace client={client} copy={copy} permissions={platformPermissions} />}
          {section === 'settings' && <SettingsWorkspace copy={copy} language={language} onLanguageChange={onLanguageChange} client={client} permissions={platformPermissions} />}
          <AdminExtensionSlot section={section} financeTab={financeTab} authorization={authorization} />
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

function Overview({ copy, language, data, permissions, modelTokenIAEnabled, onNavigate }) {
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
  const modelTokenRoute = adminModelTokenOverviewRoute(permissions, modelTokenIAEnabled);
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
    canUsage && modelTokenRoute && { label: copy.overview.usageReview, value: t.review_usage_requests, section: modelTokenIAEnabled ? 'modelOperations' : modelTokenRoute.section, query: modelTokenIAEnabled ? { operations_tab: 'usage' } : {}, tone: 'warning' },
    canUsage && modelTokenRoute && { label: copy.overview.reconciliationStopped, value: t.reconciliation_stopped, section: modelTokenIAEnabled ? 'modelOperations' : modelTokenRoute.section, query: modelTokenIAEnabled ? { operations_tab: 'usage' } : {}, tone: 'danger' },
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
          <OverviewDomain title={copy.overview.modelToken} action={copy.overview.viewDetails} onAction={() => onNavigate(modelTokenRoute.section, modelTokenRoute.query)} error={sourceError('modelToken')} copy={copy}>
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
              <button type="button" key={`${item.section}-${item.label}`} onClick={() => onNavigate(item.section, item.query || {})}>
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

function UsersWorkspace({ copy, data, client, setData, permissions, currentUserID, onNavigate, onBreadcrumbDetailChange, userRoute }) {
  const permissionSet = new Set(permissions || []);
  const repairCount = (data.registrationRepairs || []).length + (data.profileChangeRepairs || []).length + (data.accountClosures || []).length;
  const repairUnavailable = Object.values(data.userRepairAvailability || {}).includes('error');
  return (
    <div className="adm-user-workspace">
      {permissionSet.has('platform.users.read') && !userRoute.userID && (
        <section className="adm-user-summary" aria-label={copy.userManagement.summary}>
          <Metric label={copy.userManagement.totalUsers} value={data.accountOverview?.users} />
          <Metric label={copy.userManagement.activeUsers} value={data.accountOverview?.active_users} tone="positive" />
          <Metric label={copy.userManagement.disabledUsers} value={data.accountOverview?.disabled_users} />
          <Metric label={copy.userManagement.pendingRepairs} value={repairUnavailable ? '—' : repairCount} formatter={(value) => value} tone={!repairUnavailable && repairCount ? 'warning' : ''} />
        </section>
      )}
      {permissionSet.has('platform.users.read') && <AccountDirectory key={data.usersLoadVersion || 'users'} section="users" copy={copy} data={data} client={client} setData={setData} permissions={permissions} currentUserID={currentUserID} onNavigate={onNavigate} onBreadcrumbDetailChange={onBreadcrumbDetailChange} userRoute={userRoute} />}
      {!userRoute.userID && <UserRepairWorkspace copy={copy} data={data} client={client} setData={setData} permissions={permissions} />}
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

const DEFAULT_DIRECTORY_ROUTE = Object.freeze({ organizationID: '', tab: 'overview' });
const DEFAULT_DIRECTORY_QUERY = Object.freeze({ q: '', status: '', cursor: '' });
const DEFAULT_USER_ROUTE = Object.freeze({ userID: '' });

function AccountDirectory({ section, copy, data, client, setData, permissions, currentUserID = '', onNavigate, onBreadcrumbDetailChange, organizationRoute = DEFAULT_DIRECTORY_ROUTE, organizationDirectory = DEFAULT_DIRECTORY_QUERY, userRoute = DEFAULT_USER_ROUTE, routeVersion = 0 }) {
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
  const [routeUser, setRouteUser] = useState(null);
  const [routeUserError, setRouteUserError] = useState('');
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
  const selectedUser = isUserSection && userRoute.userID
    ? items.find((item) => item.user_id === userRoute.userID) || (routeUser?.user_id === userRoute.userID ? routeUser : null)
    : null;
  const selectedOrganization = !isUserSection && organizationRoute.organizationID ? items.find((item) => item.organization_id === organizationRoute.organizationID) || routeOrganization : null;
  const breadcrumbRouteKey = isUserSection && userRoute.userID
    ? `users:${userRoute.userID}`
    : !isUserSection && organizationRoute.organizationID ? `organizations:${organizationRoute.organizationID}` : '';
  const breadcrumbRouteLabel = selectedUser ? userLabel(selectedUser) : selectedOrganization?.name || '';
  useEffect(() => {
    if (breadcrumbRouteKey) onBreadcrumbDetailChange?.(breadcrumbRouteKey, breadcrumbRouteLabel);
  }, [breadcrumbRouteKey, breadcrumbRouteLabel, onBreadcrumbDetailChange]);
  useEffect(() => {
    if (!isUserSection || !userRoute.userID) {
      setRouteUser(null);
      setRouteUserError('');
      return undefined;
    }
    const existing = (data.users || []).find((item) => item.user_id === userRoute.userID);
    if (existing) {
      setRouteUser(existing);
      setRouteUserError('');
      return undefined;
    }
    const controller = new globalThis.AbortController();
    setRouteUser(null);
    setRouteUserError('');
    client.users(userRoute.userID, controller.signal).then((result) => {
      const match = (result.items || []).find((item) => item.user_id === userRoute.userID);
      if (match) setRouteUser(match);
      else setRouteUserError(copy.accounts.lookupUnavailable || copy.accounts.actionFailed);
    }).catch((reason) => {
      if (reason?.name !== 'AbortError') setRouteUserError(copy.accounts.lookupUnavailable || copy.accounts.actionFailed);
    });
    return () => controller.abort();
  }, [client, copy.accounts.actionFailed, copy.accounts.lookupUnavailable, data.users, isUserSection, userRoute.userID]);
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
              <button className="adm-name-link" onClick={() => onNavigate('users', { user_id: item.user_id })}>{userLabel(item)}</button>
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
  if (isUserSection && userRoute.userID && !selectedUser)
    return (
      <section className="adm-panel adm-org-detail">
        <button className="adm-back" onClick={() => onNavigate('users', { user_id: null })}>{copy.roles.backToUsers}</button>
        {routeUserError ? <div className="adm-inline-message error" role="alert">{routeUserError}</div> : <div className="adm-empty" role="status">{copy.loading}</div>}
      </section>
    );
  if (selectedUser) return <UserMembershipDetail copy={copy} user={selectedUser} client={client} onBack={() => onNavigate('users', { user_id: null })} onNavigate={onNavigate} />;
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

function emptySMSTemplates() {
  return { REGISTER: '', LOGIN: '', PROFILE_MOBILE: '', PASSWORD_RESET: '' };
}

function smsConfigurationForm(configuration = {}) {
  return {
    enabled: Boolean(configuration.enabled),
    provider_id: configuration.provider_id || 'aliyun-managed',
    credential_profile: configuration.credential_profile || 'aliyun-default',
    sign_name: configuration.sign_name || '',
    templates: { ...emptySMSTemplates(), ...(configuration.templates || {}) },
    reason: '',
  };
}

function SettingsWorkspace({ copy, language, onLanguageChange, client, permissions }) {
  const permissionSet = useMemo(() => new Set(permissions || []), [permissions]);
  const canReadSMS = permissionSet.has('platform.sms-config.read');
  const canManageSMS = permissionSet.has('platform.sms-config.manage');
  const [sms, setSMS] = useState(null);
  const [smsForm, setSMSForm] = useState(() => smsConfigurationForm());
  const [smsState, setSMSState] = useState({ loading: canReadSMS, saving: false, error: '', notice: '' });

  const loadSMSConfiguration = useCallback(async (signal) => {
    if (!canReadSMS) return;
    setSMSState((value) => ({ ...value, loading: true, error: '', notice: '' }));
    try {
      const result = await client.smsConfiguration(signal);
      setSMS(result);
      setSMSForm(smsConfigurationForm(result));
      setSMSState({ loading: false, saving: false, error: '', notice: '' });
    } catch (error) {
      if (error?.name !== 'AbortError') setSMSState({ loading: false, saving: false, error: copy.settings.sms.loadFailed, notice: '' });
    }
  }, [canReadSMS, client, copy.settings.sms.loadFailed]);

  useEffect(() => {
    const controller = new globalThis.AbortController();
    void loadSMSConfiguration(controller.signal);
    return () => controller.abort();
  }, [loadSMSConfiguration]);

  const chooseSMSProvider = (providerID) => {
    setSMSForm((value) => ({
      ...value,
      provider_id: providerID,
      credential_profile: providerID === 'aliyun-managed' ? 'aliyun-default' : 'http-default',
      sign_name: providerID === 'aliyun-managed' ? value.sign_name : '',
      templates: providerID === 'aliyun-managed' ? value.templates : emptySMSTemplates(),
    }));
  };

  const saveSMSConfiguration = async (event) => {
    event.preventDefault();
    if (!canManageSMS || !sms) return;
    setSMSState((value) => ({ ...value, saving: true, error: '', notice: '' }));
    try {
      const result = await client.updateSMSConfiguration({
        expected_revision: Number(sms.revision || 0),
        enabled: smsForm.enabled,
        provider_id: smsForm.provider_id,
        credential_profile: smsForm.credential_profile,
        sign_name: smsForm.provider_id === 'aliyun-managed' ? smsForm.sign_name : '',
        templates: smsForm.provider_id === 'aliyun-managed' ? smsForm.templates : emptySMSTemplates(),
        reason: smsForm.reason.trim(),
      });
      setSMS(result);
      setSMSForm(smsConfigurationForm(result));
      setSMSState({ loading: false, saving: false, error: '', notice: copy.settings.sms.saved });
    } catch (error) {
      const message = error?.code === 'SMS_CONFIGURATION_CONFLICT'
        ? copy.settings.sms.conflict
        : error?.code === 'SMS_PROVIDER_PROFILE_UNAVAILABLE'
          ? copy.settings.sms.profileUnavailable
          : error?.code === 'RECENT_AUTHENTICATION_REQUIRED'
            ? copy.settings.sms.recentAuthenticationRequired
            : copy.settings.sms.saveFailed;
      setSMSState({ loading: false, saving: false, error: message, notice: '' });
    }
  };

  const selectedProfile = sms?.available_profiles?.find((profile) => profile.profile_id === smsForm.credential_profile);
  const formValid = smsForm.reason.trim().length >= 10 && (!smsForm.enabled || selectedProfile?.configured) && (smsForm.provider_id !== 'aliyun-managed' || (
    smsForm.sign_name.trim() && Object.values(smsForm.templates).every((value) => value.trim())
  ));
  return (
    <div className="adm-settings-stack">
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
      {canReadSMS && (
        <section className="adm-panel adm-settings-panel adm-sms-configuration" data-readiness={sms?.readiness || 'LOADING'}>
          <SectionHeader title={copy.settings.sms.title} copy={copy} />
          <p className="adm-muted">{copy.settings.sms.boundary}</p>
          {smsState.loading && <div className="adm-empty" role="status">{copy.loading}</div>}
          {smsState.error && <div className="adm-inline-message error adm-sms-error" role="alert"><span>{smsState.error}</span><button type="button" disabled={smsState.loading || smsState.saving} onClick={() => void loadSMSConfiguration()}>{copy.retry}</button></div>}
          {smsState.notice && <div className="adm-inline-message ok" role="status">{smsState.notice}</div>}
          {sms && !smsState.loading && (
            <form className="adm-sms-form" onSubmit={saveSMSConfiguration}>
              <div className="adm-detail-grid adm-sms-summary">
                <div><small>{copy.settings.sms.revision}</small><strong>{sms.revision}</strong></div>
                <div><small>{copy.settings.sms.source}</small><strong>{copy.settings.sms.sources[sms.source] || sms.source}</strong></div>
                <div><small>{copy.settings.sms.readiness}</small><strong>{copy.settings.sms.readinessValues[sms.readiness] || sms.readiness}</strong></div>
              </div>
              <label className="adm-sms-switch"><input type="checkbox" checked={smsForm.enabled} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, enabled: event.target.checked }))} /><span>{copy.settings.sms.enabled}</span></label>
              <div className="adm-sms-fields">
                <label><span>{copy.settings.sms.provider}</span><select value={smsForm.provider_id} disabled={!canManageSMS || smsState.saving} onChange={(event) => chooseSMSProvider(event.target.value)}>{(sms.available_profiles || []).map((profile) => <option key={profile.profile_id} value={profile.provider_id}>{copy.settings.sms.providers[profile.provider_id] || profile.provider_id}</option>)}</select></label>
                <label><span>{copy.settings.sms.profile}</span><select value={smsForm.credential_profile} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, credential_profile: event.target.value }))}>{(sms.available_profiles || []).filter((profile) => profile.provider_id === smsForm.provider_id).map((profile) => <option key={profile.profile_id} value={profile.profile_id}>{profile.profile_id} · {profile.configured ? copy.settings.sms.configured : copy.settings.sms.notConfigured}</option>)}</select></label>
                {smsForm.provider_id === 'aliyun-managed' && <>
                  <label><span>{copy.settings.sms.signName}</span><input required maxLength="100" value={smsForm.sign_name} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, sign_name: event.target.value }))} /></label>
                  {Object.entries(copy.settings.sms.templateLabels).map(([purpose, label]) => <label key={purpose}><span>{label}</span><input required maxLength="120" value={smsForm.templates[purpose]} disabled={!canManageSMS || smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, templates: { ...value.templates, [purpose]: event.target.value } }))} /></label>)}
                </>}
                {canManageSMS && <label className="adm-sms-reason"><span>{copy.settings.sms.reason}</span><textarea required minLength="10" maxLength="500" rows="3" value={smsForm.reason} disabled={smsState.saving} onChange={(event) => setSMSForm((value) => ({ ...value, reason: event.target.value }))} placeholder={copy.settings.sms.reasonPlaceholder} /></label>}
              </div>
              {!selectedProfile?.configured && smsForm.enabled && <div className="adm-inline-message error" role="alert">{copy.settings.sms.profileUnavailable}</div>}
              {!canManageSMS && <div className="adm-inline-message info" role="status">{copy.settings.sms.readOnly}</div>}
              {canManageSMS && <button type="submit" className="adm-primary-button" disabled={smsState.saving || !formValid}>{smsState.saving ? copy.settings.sms.saving : copy.settings.sms.save}</button>}
            </form>
          )}
        </section>
      )}
    </div>
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
  );
}

function OrganizationDetail({ copy, organization, roles, client, permissions, onBack, onRenamed, onStatusChanged, onDeleted, onNavigate, activeTab = 'overview', refreshVersion = 0, onTabChange }) {
  const [units, setUnits] = useState([]);
  const [structureVersion, setStructureVersion] = useState(0);
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
        setStructureVersion(Number(unitResult.structure_version || 0));
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
    setStructureVersion(Number(result.structure_version || 0));
    return result;
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
  const moveUnit = async (unit, nextParentID) => {
    const normalizedParentID = nextParentID || null;
    const currentParentID = unit.parent_id || null;
    if (normalizedParentID === currentParentID) return true;
    if (busy || currentStatus !== 'ACTIVE' || structureVersion < 1) return false;
    setBusy(true);
    setNotice('');
    setError('');
    try {
      const result = await client.moveOrganizationUnit(organization.organization_id, unit.unit_id, normalizedParentID, structureVersion);
      setStructureVersion(Number(result.structure_version || structureVersion));
      await refreshUnits();
      setNotice(copy.accounts.unitMoved);
      return true;
    } catch (reason) {
      if (reason?.code === 'ACCOUNT_CONFLICT') {
        try { await refreshUnits(); } catch { /* retain the original conflict feedback */ }
        setError(copy.accounts.unitStructureConflict);
      } else if (reason?.code === 'ORG_UNIT_HIERARCHY_INVALID') {
        setError(copy.accounts.unitMoveInvalid);
      } else {
        setError(copy.accounts.actionFailed);
      }
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
              allUnits={units}
              onAddChild={(id) => {
                setParentID(id);
                setName('');
              }}
              onRename={renameUnit}
              onMove={moveUnit}
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

function UnitBranch({ unit, copy, disabled, addDisabled, allUnits, onAddChild, onRename, onMove, onDelete }) {
  const [editing, setEditing] = useState(false);
  const [moving, setMoving] = useState(false);
  const [name, setName] = useState(unit.name);
  const [nextParentID, setNextParentID] = useState(unit.parent_id || '');
  useEffect(() => setName(unit.name), [unit.name]);
  useEffect(() => setNextParentID(unit.parent_id || ''), [unit.parent_id]);
  const excludedMoveTargets = useMemo(() => {
    const result = new Set([unit.unit_id]);
    const visit = (node) => {
      for (const child of node.children || []) {
        result.add(child.unit_id);
        visit(child);
      }
    };
    visit(unit);
    return result;
  }, [unit]);
  const submit = async (event) => {
    event.preventDefault();
    if (await onRename(unit, name)) setEditing(false);
  };
  const submitMove = async (event) => {
    event.preventDefault();
    if (await onMove(unit, nextParentID)) setMoving(false);
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
          {moving ? (
            <form className="adm-unit-move" onSubmit={submitMove}>
              <select value={nextParentID} onChange={(event) => setNextParentID(event.target.value)} aria-label={copy.accounts.moveToParent}>
                <option value="">{copy.accounts.organizationRoot}</option>
                {(allUnits || []).filter((candidate) => candidate.status === 'ACTIVE' && !excludedMoveTargets.has(candidate.unit_id)).map((candidate) => (
                  <option key={candidate.unit_id} value={candidate.unit_id}>{candidate.name}</option>
                ))}
              </select>
              <button disabled={disabled || (unit.parent_id || '') === nextParentID}>{copy.accounts.save}</button>
              <button type="button" onClick={() => { setNextParentID(unit.parent_id || ''); setMoving(false); }}>{copy.accounts.cancel}</button>
            </form>
          ) : (
            <button type="button" disabled={disabled || unit.status !== 'ACTIVE'} onClick={() => setMoving(true)}>{copy.accounts.moveUnit}</button>
          )}
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
            <UnitBranch key={child.unit_id} unit={child} copy={copy} disabled={disabled} addDisabled={addDisabled} allUnits={allUnits} onAddChild={onAddChild} onRename={onRename} onMove={onMove} onDelete={onDelete} />
          ))}
        </div>
      )}
    </div>
  );
}

const defaultCanDrilldown = () => true;

function FinanceOverviewBlock({ copy, language, surface, load, onDrilldown, canDrilldown = defaultCanDrilldown }) {
  const [state, setState] = useState({ loading: true, data: null, error: null, forbidden: false });
  const generationRef = useRef(0);
  const controllerRef = useRef(null);
  const reload = useCallback(async () => {
    const generation = ++generationRef.current;
    controllerRef.current?.abort();
    const controller = new globalThis.AbortController();
    controllerRef.current = controller;
    setState((current) => ({ ...current, loading: true, error: null, forbidden: false }));
    try {
      const result = await load(controller.signal);
      if (generation === generationRef.current) setState({ loading: false, data: result, error: null, forbidden: false });
    } catch (error) {
      if (generation !== generationRef.current || error?.name === 'AbortError') return;
      setState({ loading: false, data: null, error: { code: error?.code || '', requestID: error?.requestID || '' }, forbidden: error?.status === 403 });
    }
  }, [load]);
  useEffect(() => {
    void reload();
    return () => {
      generationRef.current += 1;
      controllerRef.current?.abort();
    };
  }, [reload]);
  if (state.forbidden) return null;
  const labels = copy.billing.overview;
  const overview = state.data;
  return (
    <section className="adm-finance-overview" aria-labelledby={`finance-overview-${surface}`}>
      <header>
        <div>
          <span className="adm-eyebrow">{labels.eyebrow}</span>
          <h2 id={`finance-overview-${surface}`}>{labels.titles[surface]}</h2>
        </div>
        <button type="button" aria-busy={state.loading} onClick={() => void reload()}>{copy.refresh}</button>
      </header>
      {state.loading && !overview && <div className="adm-finance-overview-state" role="status">{copy.loading}</div>}
      {state.error && <div className="adm-finance-overview-state error" role="alert"><span>{labels.unavailable}{state.error.code ? ` · ${state.error.code}` : ''}{state.error.requestID ? ` · request_id=${state.error.requestID}` : ''}</span><button type="button" onClick={() => void reload()}>{copy.retry}</button></div>}
      {overview && !state.error && (
        <div className="adm-finance-overview-grid">
          {(overview.metrics || []).map((metric) => {
            const candidate = financeOverviewDrilldown(surface, metric.key, overview.window);
            const target = candidate && canDrilldown(candidate) ? candidate : null;
            const usesWindow = financeOverviewMetricTimeScope(surface, metric.key) === 'window' && overview.window;
            const content = (
              <>
                <span>{labels.metrics[metric.key] || metric.key}</span>
                <strong>{formatFinanceOverviewValue(metric, {
                  formatCount: formatNumber,
                  formatPoints: (value) => formatPoints(value, language),
                  formatMoney,
                })}</strong>
                <small>{usesWindow
                  ? labels.window(formatUTCDate(overview.window.from), formatUTCDate(overview.window.to), overview.window.timezone)
                  : labels.pointInTime(formatDate(overview.as_of))}</small>
                <small>{labels.units[metric.unit] || metric.unit} · {labels.statuses}: {(metric.included_statuses || []).join(', ') || labels.notApplicable}</small>
                <small>{labels.descriptions[metric.key] || labels.defaultDescription}</small>
                {target && <em>{target.exact ? labels.viewFiltered : labels.viewUnfiltered}</em>}
              </>
            );
            return target
              ? <button type="button" className="adm-finance-overview-card" key={metric.key} onClick={() => onDrilldown(target)}>{content}</button>
              : <article className="adm-finance-overview-card" key={metric.key}>{content}</article>;
          })}
        </div>
      )}
      {overview && !state.error && <footer>{labels.noDenominator} · {labels.updated}: {formatDate(overview.as_of)}</footer>}
    </section>
  );
}

function ProductsWorkspace({ copy, language, client, permissions, routeTab, onRouteTabChange, onNavigate, modelDirectoryVisible = false, modelDetailsEnabled = false }) {
  const permissionSet = new Set(permissions || []);
  const visibleTabs = [...(permissionSet.has('platform.products.read') ? ['catalog'] : []), ...(permissionSet.has('platform.pricing.read') ? ['prices'] : [])];
  const activeTab = visibleTabs.includes(routeTab) ? routeTab : visibleTabs[0] || '';
  useEffect(() => {
    if (activeTab && activeTab !== routeTab) onRouteTabChange(activeTab, 'replace');
  }, [activeTab, onRouteTabChange, routeTab]);
  const pricingLoad = useCallback((signal) => client.financePricingOverview(signal), [client]);
  const productsLoad = useCallback((signal) => client.financeProductsOverview(signal), [client]);
  const drilldown = (target) => {
    if (target.tab === 'catalog') {
      onNavigate?.('products', { finance_tab: 'catalog', product_cursor: null, ...target.query });
      return;
    }
    onRouteTabChange?.(target.tab);
  };
  return (
    <>
      <div className="adm-finance-overviews">
        {permissionSet.has('platform.pricing.read') && <FinanceOverviewBlock copy={copy} language={language} surface="pricing" load={pricingLoad} onDrilldown={drilldown} canDrilldown={(target) => visibleTabs.includes(target.tab)} />}
        {permissionSet.has('platform.products.read') && <FinanceOverviewBlock copy={copy} language={language} surface="products" load={productsLoad} onDrilldown={drilldown} canDrilldown={(target) => visibleTabs.includes(target.tab)} />}
      </div>
      <section className="adm-panel">
        <SectionHeader title={copy.nav.products} copy={copy} />
        <Tabs active={activeTab} onChange={onRouteTabChange} items={visibleTabs.map((key) => ({ key, label: key === 'catalog' ? copy.billing.productCatalog : copy.billing.prices }))} />
        {permissionSet.has('platform.pricing.read') && <div hidden={activeTab !== 'prices'}><PricingLifecycleWorkspace copy={copy} client={client} permissions={permissions} onOpenModel={modelDirectoryVisible && modelDetailsEnabled && permissionSet.has('platform.models.read') ? (modelID) => onNavigate?.('models', { model_tab: 'directory', model_id: modelID, model_detail_tab: 'pricing', model_price_return: 'products' }) : null} /></div>}
      </section>
    </>
  );
}

function PaymentControlsPanel({ copy, data, client, permissions, onNavigate, onReload }) {
  const control = data.paymentControls;
  const permissionSet = new Set(permissions || []);
  const canRequest = permissionSet.has('platform.billing.payment-controls.resume.request');
  const canApprove = permissionSet.has('platform.billing.payment-controls.resume.approve');
  const [reasons, setReasons] = useState({});
  const [busyTarget, setBusyTarget] = useState('');
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const requestKeysRef = useRef(new Map());
  const approvals = data.paymentControlApprovals || [];
  const approvalFor = (commandType, targetID) => {
    return approvals.find((item) => item.command_type === commandType &&
      (item.target?.resource_id || item.target_resource_id) === targetID);
  };
  const reload = async () => {
    const result = await onReload?.();
    if (!result) throw new Error('PAYMENT_CONTROLS_RELOAD_FAILED');
    return result.paymentControlApprovals || [];
  };
  const requestResume = async (product = null, existingApproval = null, updateDraft = false) => {
    const targetKey = product?.product_id || 'provider';
    const reason = String(reasons[targetKey] || '').trim();
    if (!canRequest || ((!existingApproval || updateDraft) && reason.length < 10)) return;
    setBusyTarget(targetKey);
    setNotice('');
    setError('');
    try {
      let approvalInput = null;
      if (!existingApproval || updateDraft) {
        const freshControl = await client.paymentProviderControls();
        const freshProduct = product ? freshControl?.products?.find((item) => item.product_id === product.product_id) : null;
        if (!freshControl?.provider_id || (!product && freshControl.new_orders_enabled) ||
          (product && (!freshProduct || freshProduct.new_orders_enabled))) {
          throw new Error('payment control changed');
        }
        const commandType = product ? 'PAYMENT_PRODUCT_RESUME' : 'PAYMENT_PROVIDER_RESUME';
        const targetID = product?.product_id || freshControl?.provider_id;
        const supersedesID = existingApproval ? '' : paymentControlSupersedesRequest(
          await client.governedApprovalRequests({ command_type: commandType, target: targetID, limit: 200 }), commandType, targetID);
        if (!requestKeysRef.current.has(targetKey)) requestKeysRef.current.set(targetKey, `payment-control-resume-${secureRequestID()}`);
        approvalInput = buildPaymentControlApprovalDraft(freshControl, freshProduct, reason,
          requestKeysRef.current.get(targetKey), supersedesID);
        if (!approvalInput) throw new Error('payment control resume draft unavailable');
      }
      if (updateDraft) await updateAndSubmitGovernedApproval(client, existingApproval, approvalInput);
      else await submitPaymentControlApproval(client, approvalInput, existingApproval);
      setReasons((current) => ({ ...current, [targetKey]: '' }));
      requestKeysRef.current.delete(targetKey);
      await reload();
      setNotice(copy.billing.controlResumeSubmitted);
    } catch {
      try {
        await reload();
      } catch {
        // Preserve the original failure and idempotency key when convergence cannot be checked.
      }
      setError(copy.billing.controlResumeFailed);
    } finally {
      setBusyTarget('');
    }
  };
  const openApproval = (commandType, targetID, approval) => onNavigate?.('approvals', {
    approval_command_type: commandType, approval_target: targetID,
    approval_request_id: approval?.approval_request_id || null,
  });
  if (!control) return <div className="adm-inline-message error">{copy.billing.controlsUnavailable}</div>;
  const providerApproval = approvalFor('PAYMENT_PROVIDER_RESUME', control.provider_id);
  const providerAction = paymentControlApprovalAction(providerApproval, { canRequest });
  const evidence = control.stop_loss_evidence || {};
  return (
    <section className="adm-finance-subpanel">
      <SectionHeader title={copy.billing.controls} copy={copy} />
      {notice && <div className="adm-inline-message ok">{notice}</div>}
      {error && <div className="adm-inline-message error">{error}</div>}
      <div className="adm-detail-grid">
        <div><small>{copy.billing.provider}</small><strong>{control.provider_id} · {control.provider_version}</strong></div>
        <div><small>{copy.billing.newOrders}</small><Status value={control.new_orders_enabled ? 'ENABLED' : 'PAUSED'} /></div>
        <div><small>{copy.billing.controlVersion}</small><strong>v{control.provider_control_version}</strong></div>
        <div><small>{copy.billing.controlReason}</small><strong>{control.reason_code || '—'} · {control.reason || '—'}</strong></div>
        <div><small>{copy.billing.observationGaps}</small><strong>{evidence.observation_gap_count ?? 0}</strong></div>
        <div><small>{copy.billing.updated}</small><strong>{formatDate(control.updated_at)}</strong></div>
      </div>
      {!control.new_orders_enabled && (
        <div className="adm-ops-form">
          {providerAction === 'create' && <label>{copy.billing.resumeReason}<input minLength="10" maxLength="2000" value={reasons.provider || ''} onChange={(event) => setReasons((current) => ({ ...current, provider: event.target.value }))} /></label>}
          {providerAction === 'create' && <button type="button" disabled={!control.resume_draft || busyTarget === 'provider' || String(reasons.provider || '').trim().length < 10} title={!control.resume_draft ? copy.billing.resumeDraftUnavailable : undefined} onClick={() => requestResume()}>{copy.billing.requestProviderResume}</button>}
          {providerAction === 'submit' && <button type="button" disabled={busyTarget === 'provider'} onClick={() => requestResume(null, providerApproval)}>{copy.billing.continueControlResume}</button>}
          {providerAction === 'submit' && <label>{copy.billing.resumeReason}<input minLength="10" maxLength="2000" value={reasons.provider || ''} onChange={(event) => setReasons((current) => ({ ...current, provider: event.target.value }))} /></label>}
          {providerAction === 'submit' && <button type="button" disabled={busyTarget === 'provider' || String(reasons.provider || '').trim().length < 10} onClick={() => requestResume(null, providerApproval, true)}>{copy.billing.updateAndSubmitControlResume}</button>}
          {providerAction === 'create' && !control.resume_draft && <small>{copy.billing.resumeDraftUnavailable}</small>}
          {providerApproval && (canRequest || canApprove) && <button type="button" onClick={() => openApproval('PAYMENT_PROVIDER_RESUME', control.provider_id, providerApproval)}>{copy.billing.viewApproval} · {providerApproval.status}</button>}
        </div>
      )}
      <h3>{copy.billing.productControls}</h3>
      <div className="adm-table-wrap" tabIndex="0">
        <table className="adm-table"><thead><tr><th>{copy.billing.product}</th><th>{copy.billing.newOrders}</th><th>{copy.billing.controlVersion}</th><th>{copy.billing.controlReason}</th><th>{copy.accounts.actions}</th></tr></thead>
          <tbody>{(control.products || []).map((product) => {
            const approval = approvalFor('PAYMENT_PRODUCT_RESUME', product.product_id);
            const action = paymentControlApprovalAction(approval, { canRequest });
            return (
              <tr key={product.product_id}>
                <td><code>{product.product_id}</code></td>
                <td><Status value={product.new_orders_enabled ? 'ENABLED' : 'PAUSED'} /></td>
                <td>v{product.version}</td>
                <td>{product.reason_code || '—'} · {product.reason || '—'}</td>
                <td>{!product.new_orders_enabled ? (
                  <div className="adm-row-actions">
                    {action === 'create' && <input aria-label={`${copy.billing.resumeReason} · ${product.product_id}`} minLength="10" maxLength="2000" value={reasons[product.product_id] || ''} onChange={(event) => setReasons((current) => ({ ...current, [product.product_id]: event.target.value }))} />}
                    {action === 'create' && <button disabled={!product.resume_draft || busyTarget === product.product_id || String(reasons[product.product_id] || '').trim().length < 10} title={!product.resume_draft ? copy.billing.resumeDraftUnavailable : undefined} onClick={() => requestResume(product)}>{copy.billing.requestProductResume}</button>}
                    {action === 'submit' && <button disabled={busyTarget === product.product_id} onClick={() => requestResume(product, approval)}>{copy.billing.continueControlResume}</button>}
                    {action === 'submit' && <input aria-label={`${copy.billing.resumeReason} · ${product.product_id}`} minLength="10" maxLength="2000" value={reasons[product.product_id] || ''} onChange={(event) => setReasons((current) => ({ ...current, [product.product_id]: event.target.value }))} />}
                    {action === 'submit' && <button disabled={busyTarget === product.product_id || String(reasons[product.product_id] || '').trim().length < 10} onClick={() => requestResume(product, approval, true)}>{copy.billing.updateAndSubmitControlResume}</button>}
                    {action === 'create' && !product.resume_draft && <small>{copy.billing.resumeDraftUnavailable}</small>}
                    {approval && (canRequest || canApprove) && <button onClick={() => openApproval('PAYMENT_PRODUCT_RESUME', product.product_id, approval)}>{copy.billing.viewApproval} · {approval.status}</button>}
                  </div>
                ) : '—'}</td>
              </tr>
            );
          })}</tbody>
        </table>
      </div>
      <h3>{copy.billing.stopLossEvidence}</h3>
      <Table copy={copy} items={evidence.evaluations || []} columns={[
        { key: 'scope_type', label: copy.billing.scope },
        { key: 'product_id', label: copy.billing.product },
        { key: 'trigger_type', label: copy.billing.trigger },
        { key: 'source_case_id', label: copy.billing.sourceCase, render: (item) => item.source_case_id ? <code>{item.source_case_id}</code> : '—' },
        { key: 'severity', label: copy.billing.severity },
        { key: 'action_applied', label: copy.billing.actionApplied, render: (item) => item.action_applied === true ? copy.billing.applied : item.action_applied === false ? copy.billing.notApplied : '—' },
        { key: 'policy_version', label: copy.billing.policyVersion },
        { key: 'metrics', label: copy.billing.metrics, render: (item) => <code>{formatSummary(item.metrics)}</code> },
        { key: 'evidence_details', label: copy.billing.evidenceDetails, render: (item) => <details><summary>{copy.billing.viewDetails}</summary><small>{copy.billing.window}: {formatDate(item.window_start)} — {formatDate(item.window_end)}<br />{copy.billing.highWatermark}: <code>{item.observation_high_watermark || '—'}</code></small></details> },
        { key: 'evaluated_at', label: copy.billing.updated, render: (item) => formatDate(item.evaluated_at) },
      ]} />
    </section>
  );
}

const EMPTY_BILLING_QUERY = Object.freeze({});
const EMPTY_BILLING_DETAIL = Object.freeze({ type: '', id: '', query: EMPTY_BILLING_QUERY });

function Billing({ copy, language, data, client, setData, permissions, currentUserID = '', surface = 'billing', routeTab = '', routeQuery = EMPTY_BILLING_QUERY, routeDetail = EMPTY_BILLING_DETAIL, routeVersion = 0, onRouteTabChange, onRouteQueryChange, onRouteDetailChange, onNavigate, embedded = false, requestTracker }) {
  const defaultTab = surface === 'payments' ? 'orders' : surface === 'products' ? 'prices' : 'wallets';
  const initialFilter = (targetTab, fallback) => routeTab === targetTab
    ? normalizeAdminFinanceQuery(targetTab, routeQuery)
    : readFinanceTabQuery(data, surface, targetTab, fallback);
  const [tab, setTab] = useState(routeTab || defaultTab);
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
  const [actionPanel, setActionPanel] = useState(null);
  const ownerSearchSequence = useRef(0);
  const financeCursorBackRef = useRef(new Map());
  const financeDetailCursorBackRef = useRef(new Map());
  const routeDetailRef = useRef(routeDetail);
  const [reconciliation, setReconciliation] = useState({
    channel: 'ALIPAY',
    businessDate: new Date().toISOString().slice(0, 10),
  });
  const [adjustment, setAdjustment] = useState({ walletID: '', amount: '', reason: '' });
  const [adjustmentEditReasons, setAdjustmentEditReasons] = useState({});
  const [adjustmentRequestKey, setAdjustmentRequestKey] = useState(() => secureRequestID());
  const [adjustmentReplacement, setAdjustmentReplacement] = useState(null);
  const [walletFilter, setWalletFilter] = useState(() => initialFilter('wallets', {}));
  const [receivableFilter, setReceivableFilter] = useState(() => initialFilter('receivables', {}));
  const [adjustmentFilter, setAdjustmentFilter] = useState(() => initialFilter('adjustments', {}));
  const [holdFilter, setHoldFilter] = useState(() => initialFilter('holds', {}));
  const [orderFilter, setOrderFilter] = useState(() => initialFilter('orders', {}));
  const [refundFilter, setRefundFilter] = useState(() => initialFilter('refunds', {}));
  const [lateFilter, setLateFilter] = useState(() => initialFilter('lateReviews', {
    status: '',
    provider_id: '',
    owner_id: '',
    entered_from: '',
    entered_to: '',
    next_review_before: '',
    overdue: false,
    limit: '50',
  }));
  const [anomalyFilter, setAnomalyFilter] = useState(() => initialFilter('anomalies', {
    anomaly_type: '',
    status: 'OPEN',
    severity: '',
    owner_id: '',
    limit: '50',
  }));
  const [reconciliationFilter, setReconciliationFilter] = useState(() => initialFilter('reconciliations', {}));
  const [ledgerFilter, setLedgerFilter] = useState(() => normalizeAdminFinanceDetailQuery('ledger', routeDetail.type === 'ledger' ? routeDetail.query : {}));
  const [caseFilter, setCaseFilter] = useState(() => normalizeAdminFinanceDetailQuery('reconciliationCases', routeDetail.type === 'reconciliationCases' ? routeDetail.query : {}));
  const permissionSet = new Set(permissions || []);
  const canReadWallets = permissionSet.has('platform.billing.wallets.read');
  const canReadLedger = permissionSet.has('platform.billing.ledger.read');
  const canReadPricing = permissionSet.has('platform.pricing.read');
  const canReadUsage = permissionSet.has('platform.usage.read');
  const canRequestAdjustment = permissionSet.has('platform.billing.repairs.request');
  const canSyncOrders = permissionSet.has('platform.billing.orders.sync');
  const canRequestRepair = permissionSet.has('platform.billing.repairs.request');
  const canRequestControlResume = permissionSet.has('platform.billing.payment-controls.resume.request');
  const canApproveControlResume = permissionSet.has('platform.billing.payment-controls.resume.approve');
  const canApproveAdjustment = permissionSet.has('platform.billing.repairs.approve');
  const canReleaseHold = permissionSet.has('platform.billing.holds.release');
  const canReadUserDirectory = permissionSet.has('platform.users.read');
  const canReconcile = permissionSet.has('platform.usage.reconcile.retry');
  const overviewSurface = surface === 'payments' ? 'payments' : 'accounts';
  const overviewLoad = useCallback((signal) => surface === 'payments'
    ? client.financePaymentsOverview({}, signal)
    : client.financeAccountsOverview(signal), [client, surface]);
  routeDetailRef.current = routeDetail;
  const canReadUsageRef = useRef(canReadUsage);
  const canReadUserDirectoryRef = useRef(canReadUserDirectory);
  const canReconcileRef = useRef(canReconcile);
  canReadUsageRef.current = canReadUsage;
  canReadUserDirectoryRef.current = canReadUserDirectory;
  canReconcileRef.current = canReconcile;
  const visibleTabs =
    surface === 'payments'
      ? [...(canReadWallets ? ['orders', 'refunds', 'lateReviews', 'anomalies'] : []), ...(canReadUsage ? ['reconciliations'] : []), ...(canReadWallets ? ['controls'] : [])]
      : surface === 'products'
        ? canReadPricing
          ? ['prices']
          : []
        : [...(canReadWallets ? ['wallets', 'receivables', 'adjustments'] : []), ...(canReadUsage ? ['holds'] : [])];
  useEffect(() => {
    const resolved = resolveAdminFinanceTab(routeTab, tab, visibleTabs);
    if (resolved.tab !== tab) setTab(resolved.tab);
    if (resolved.replace) onRouteTabChange?.(resolved.tab, 'replace');
  }, [onRouteTabChange, routeTab, tab, visibleTabs.join('|')]);
  const changeTab = (nextTab) => {
    setTab(nextTab);
    onRouteTabChange?.(nextTab);
  };
  const drilldownOverview = (target) => {
    setTab(target.tab);
    if (isAdminFinancePaginatedTab(target.tab)) onRouteQueryChange?.(target.tab, target.query || {});
    else onRouteTabChange?.(target.tab);
  };
  const activeTabState = data.financeTabStates?.[surface]?.[tab] || {};
  const paymentCapabilityReady = data.paymentCapabilityState?.loaded === true && !data.paymentCapabilityState?.loading && !data.paymentCapabilityState?.error;
  const paymentCapabilities = paymentCapabilityReady ? data.paymentCapabilities || {} : {};
  const normalRefundCreationEnabled =
    paymentCapabilities.normal_refund_creation_enabled === true &&
    paymentCapabilities.refund_supported === true;
  const reconciliationSupported = paymentCapabilities.bill_supported === true;
  const reconciliationImportSupported = paymentCapabilities.bill_file_import_supported === true;
  const repairSupported = paymentCapabilities.manual_repair_supported === true;
  const hasLimitedPaymentCapabilities = paymentCapabilities.provider_id && (!paymentCapabilities.refund_supported || !paymentCapabilities.close_supported || !paymentCapabilities.notification_supported || paymentCapabilities.reconciliation_mode !== 'API');
  const loadTab = useCallback(async (targetTab, options = {}) => {
    const requestKey = `${surface}:${targetTab}`;
    const generation = requestTracker.begin(requestKey);
    const routeAware = isAdminFinancePaginatedTab(targetTab);
    const querySnapshot = routeAware ? normalizeAdminFinanceQuery(targetTab, options.query || routeQuery) : null;
    setData((current) => updateFinanceTabState(current, surface, targetTab, {
      loading: true,
      attempted: true,
      error: '',
      ...(querySnapshot ? { requestedQuery: querySnapshot } : {}),
    }));
    try {
      let next = {};
      let page = null;
      if (targetTab === 'wallets' && canReadWallets) {
        const collection = normalizeFinanceCollection(await client.wallets(querySnapshot), querySnapshot);
        next.wallets = collection.items;
        page = collection.page;
      } else if (targetTab === 'receivables' && canReadWallets) {
        const collection = normalizeFinanceCollection(await client.receivables(querySnapshot), querySnapshot);
        next.receivables = collection.items;
        page = collection.page;
      } else if (targetTab === 'adjustments' && canReadWallets) {
        const [adjustments, approvals] = await Promise.all([
          client.adjustments(querySnapshot),
          canRequestAdjustment || canApproveAdjustment ? client.governedApprovalRequests({ command_type: 'WALLET_ADJUSTMENT', limit: 200 }) : Promise.resolve({ items: [] }),
        ]);
        const collection = normalizeFinanceCollection(adjustments, querySnapshot);
        next = { adjustments: collection.items, adjustmentApprovals: approvals.items || [] };
        page = collection.page;
      } else if (targetTab === 'holds' && canReadUsage) {
        const collection = normalizeFinanceCollection(await client.holds(querySnapshot), querySnapshot);
        next.holds = collection.items;
        page = collection.page;
      } else if (targetTab === 'orders' && canReadWallets) {
        const collection = normalizeFinanceCollection(await client.paymentOrders(querySnapshot), querySnapshot);
        next.orders = collection.items;
        page = collection.page;
      } else if (targetTab === 'refunds' && canReadWallets) {
        const collection = normalizeFinanceCollection(await client.refunds(querySnapshot), querySnapshot);
        next.refunds = collection.items;
        page = collection.page;
      } else if (targetTab === 'lateReviews' && canReadWallets) {
        const collection = normalizeFinanceCollection(await client.latePaymentReviews(querySnapshot), querySnapshot);
        next.lateReviews = collection.items;
        page = collection.page;
      } else if (targetTab === 'anomalies' && canReadWallets) {
        const collection = normalizeFinanceCollection(await client.paymentAnomalies(querySnapshot), querySnapshot);
        next.anomalies = collection.items;
        page = collection.page;
      } else if (targetTab === 'reconciliations' && canReadUsage) {
        const collection = normalizeFinanceCollection(await client.reconciliationRuns(querySnapshot), querySnapshot);
        next.reconciliations = collection.items;
        page = collection.page;
      } else if (targetTab === 'controls' && canReadWallets) {
        const canReadControlApprovals = canRequestControlResume || canApproveControlResume;
        const [paymentControls, providerApprovals, productApprovals] = await Promise.all([
          client.paymentProviderControls(),
          canReadControlApprovals ? client.governedApprovalRequests({ command_type: 'PAYMENT_PROVIDER_RESUME', limit: 200 }) : Promise.resolve({ items: [] }),
          canReadControlApprovals ? client.governedApprovalRequests({ command_type: 'PAYMENT_PRODUCT_RESUME', limit: 200 }) : Promise.resolve({ items: [] }),
        ]);
        next = { paymentControls, paymentControlApprovals: [...(providerApprovals.items || []), ...(productApprovals.items || [])] };
      } else if (targetTab === 'prices' && canReadPricing) {
        const result = await client.priceVersions();
        next.prices = result.items || [];
      } else {
        throw new Error('FINANCE_TAB_NOT_AUTHORIZED');
      }
      if (!requestTracker.isCurrent(requestKey, generation)) return null;
      setData((current) => updateFinanceTabState({ ...current, ...next }, surface, targetTab, {
        loading: false,
        attempted: true,
        loaded: true,
        error: '',
        updatedAt: Date.now(),
        ...(querySnapshot ? { query: querySnapshot, page } : {}),
      }));
      return next;
    } catch (reason) {
      if (!requestTracker.isCurrent(requestKey, generation)) return null;
      if (reason?.name === 'AbortError') {
        setData((current) => updateFinanceTabState(current, surface, targetTab, { loading: false }));
        return null;
      }
      setData((current) => updateFinanceTabState(current, surface, targetTab, {
        loading: false,
        attempted: true,
        error: isFinanceSnapshotExpired(reason) ? copy.billing.snapshotExpired : copy.failed,
        ...(isFinanceSnapshotExpired(reason) ? { page: { ...(current?.financeTabStates?.[surface]?.[targetTab]?.page || {}), expired: true } } : {}),
      }));
      return null;
    }
  }, [canApproveAdjustment, canApproveControlResume, canReadPricing, canReadUsage, canReadWallets, canRequestAdjustment, canRequestControlResume, client, copy.failed, requestTracker, routeQuery, setData, surface]);
  const loadPaymentCapabilities = useCallback(async () => {
    if (surface !== 'payments' || !canReadWallets) return null;
    const requestKey = 'payments:capabilities';
    const generation = requestTracker.begin(requestKey);
    setData((current) => updatePaymentCapabilityState(current, { loading: true, attempted: true, error: '' }));
    try {
      const billingOverview = await client.billingOverview();
      if (!requestTracker.isCurrent(requestKey, generation)) return null;
      setData((current) => updatePaymentCapabilityState(current, { loading: false, attempted: true, loaded: true, error: '', updatedAt: Date.now() }, billingOverview.payment_capabilities || {}));
      return billingOverview;
    } catch (reason) {
      if (!requestTracker.isCurrent(requestKey, generation)) return null;
      if (reason?.name === 'AbortError') {
        setData((current) => updatePaymentCapabilityState(current, { loading: false }));
        return null;
      }
      setData((current) => updatePaymentCapabilityState(current, { loading: false, attempted: true, loaded: false, error: copy.failed }, null));
      return null;
    }
  }, [canReadWallets, client, copy.failed, requestTracker, setData, surface]);
  const routeQueryKey = JSON.stringify(routeQuery);
  useEffect(() => {
    if (tab === 'wallets') setWalletFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'receivables') setReceivableFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'adjustments') setAdjustmentFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'holds') setHoldFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'orders') setOrderFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'refunds') setRefundFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'lateReviews') setLateFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'anomalies') setAnomalyFilter(normalizeAdminFinanceQuery(tab, routeQuery));
    else if (tab === 'reconciliations') setReconciliationFilter(normalizeAdminFinanceQuery(tab, routeQuery));
  }, [routeQueryKey, routeVersion, tab]);
  useEffect(() => {
    if (!tab) return;
    const routeAware = isAdminFinancePaginatedTab(tab);
    const desiredQuery = routeAware ? normalizeAdminFinanceQuery(tab, routeQuery) : null;
    if (financeTabShouldLoad(activeTabState, desiredQuery)) {
      void loadTab(tab, desiredQuery ? { query: desiredQuery } : undefined);
    }
  }, [activeTabState.attempted, activeTabState.loading, activeTabState.query, activeTabState.requestedQuery, loadTab, routeQueryKey, routeVersion, tab]);
  useEffect(() => {
    if (surface === 'payments' && canReadWallets && !data.paymentCapabilityState?.attempted && !data.paymentCapabilityState?.loading) void loadPaymentCapabilities();
  }, [canReadWallets, data.paymentCapabilityState?.attempted, data.paymentCapabilityState?.loading, loadPaymentCapabilities, surface]);
  useEffect(() => () => {
    requestTracker.invalidatePrefix(`${surface}:`);
    setData((current) => resetFinanceSurfaceLoading(current, surface));
  }, [requestTracker, setData, surface]);
  const reload = async () => {
    const routeAware = isAdminFinancePaginatedTab(tab);
    const [result] = await Promise.all([
      loadTab(tab, routeAware ? { query: normalizeAdminFinanceQuery(tab, routeQuery) } : undefined),
      surface === 'payments' && canReadWallets ? loadPaymentCapabilities() : Promise.resolve(null),
    ]);
    return result;
  };
  const perform = async (action) => {
    setBusy(true);
    setNotice('');
    setError('');
    try {
      await action();
      await reload();
      setNotice(copy.billing.completed);
    } catch (cause) {
      setError(copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const openActionPanel = (configuration) => {
    ownerSearchSequence.current += 1;
    const values = Object.fromEntries((configuration.fields || []).map((field) => [field.name, field.value || '']));
    const idempotencyKey = configuration.idempotencyPrefix ? createFinanceIdempotencyKey(configuration.idempotencyPrefix) : '';
    setActionPanel({ ...configuration, values, fieldErrors: {}, error: '', idempotencyKey, attempted: false, writeSucceeded: false, outcomeUnknown: false });
  };
  const changeActionField = (name, value) => setActionPanel((current) => ({
    ...current,
    values: { ...current.values, [name]: value },
    fieldErrors: { ...current.fieldErrors, [name]: '' },
    error: '',
    idempotencyKey: current.attempted && current.idempotencyPrefix ? createFinanceIdempotencyKey(current.idempotencyPrefix) : current.idempotencyKey,
    attempted: current.attempted && current.idempotencyPrefix ? false : current.attempted,
  }));
  const refreshActionPanel = async () => {
    if (!actionPanel || busy) return;
    setBusy(true);
    setActionPanel((current) => ({ ...current, error: '' }));
    try {
      const result = actionPanel.after ? await actionPanel.after() : await reload();
      if (!actionPanel.after && result === null) throw new Error('REFRESH_FAILED');
      setActionPanel(null);
      setNotice(copy.billing.actionPanel.statusRefreshed);
    } catch (cause) {
      const requestID = cause?.requestID ? copy.billing.actionPanel.requestID(cause.requestID) : '';
      setActionPanel((current) => ({ ...current, restartPagination: isFinanceSnapshotExpired(cause), error: `${copy.billing.actionPanel.refreshFailed}${requestID}` }));
    } finally {
      setBusy(false);
    }
  };
  const submitActionPanel = async (event) => {
    event.preventDefault();
    if (!actionPanel || busy) return;
    const labels = copy.billing.actionPanel;
    const values = {};
    const fieldErrors = {};
    for (const field of actionPanel.fields || []) {
      if (field.submit === false) continue;
      const value = String(actionPanel.values[field.name] || '').trim();
      values[field.name] = value;
      if (field.required && !value) fieldErrors[field.name] = labels.required;
      else if (field.minLength && [...value].length < field.minLength) fieldErrors[field.name] = labels.minimumLength(field.minLength);
      else if (field.future && (!Number.isFinite(new Date(value).getTime()) || new Date(value).getTime() <= Date.now())) fieldErrors[field.name] = labels.futureTime;
    }
    if (Object.keys(fieldErrors).length) {
      setActionPanel((current) => ({ ...current, values: { ...current.values, ...values }, fieldErrors }));
      return;
    }
    setBusy(true);
    setNotice('');
    setError('');
    setActionPanel((current) => ({ ...current, values: { ...current.values, ...values }, fieldErrors: {}, error: '', attempted: true }));
    try {
      const outcome = await executeStructuredFinanceAction({
        execute: () => actionPanel.execute(values, { idempotencyKey: actionPanel.idempotencyKey }),
        refresh: async () => {
          const result = actionPanel.after ? await actionPanel.after() : await reload();
          if (!actionPanel.after && result === null) throw new Error('REFRESH_FAILED');
        },
      });
      setNotice(copy.billing.completed);
      if (outcome.refreshed) setActionPanel(null);
      else {
        const requestID = outcome.refreshError?.requestID ? copy.billing.actionPanel.requestID(outcome.refreshError.requestID) : '';
        setActionPanel((current) => ({ ...current, writeSucceeded: true, restartPagination: isFinanceSnapshotExpired(outcome.refreshError), error: `${copy.billing.actionPanel.refreshFailed}${requestID}` }));
      }
    } catch (cause) {
      const unknown = !actionPanel.idempotencyKey && isUnknownMutationOutcome(cause);
      const mapped = labels.errors?.[cause?.code]
        || (cause?.status === 401 ? labels.authenticationRequired : cause?.status === 403 ? copy.permissionDenied : cause?.status >= 500 ? labels.serviceUnavailable : copy.billing.actionFailed);
      const requestID = cause?.requestID ? labels.requestID(cause.requestID) : '';
      setActionPanel((current) => ({ ...current, outcomeUnknown: unknown, error: `${unknown ? labels.resultUnknown : mapped}${requestID}` }));
    } finally {
      setBusy(false);
    }
  };
  const loadLedgerPage = useCallback(async (wallet, rawQuery = {}) => {
    const query = normalizeAdminFinanceDetailQuery('ledger', rawQuery);
    const requestKey = `${surface}:ledger-detail`;
    const generation = requestTracker.begin(requestKey);
    setLedger((current) => ({
      wallet,
      query,
      items: current?.wallet?.wallet_id === wallet.wallet_id ? current.items || [] : [],
      page: current?.wallet?.wallet_id === wallet.wallet_id ? current.page || null : null,
      loading: true,
      error: '',
    }));
    try {
      const collection = normalizeFinanceCollection(await client.walletLedger(wallet.wallet_id, query), query);
      if (!requestTracker.isCurrent(requestKey, generation)) return;
      setLedger({ wallet, query, ...collection, loading: false, error: '' });
    } catch (cause) {
      if (!requestTracker.isCurrent(requestKey, generation)) return;
      setLedger((current) => current ? {
        ...current,
        loading: false,
        error: isFinanceSnapshotExpired(cause) ? copy.billing.snapshotExpired : copy.billing.actionFailed,
        ...(isFinanceSnapshotExpired(cause) ? { page: { ...current.page, expired: true } } : {}),
      } : current);
    }
  }, [client, copy.billing.actionFailed, copy.billing.snapshotExpired, requestTracker, surface]);
  const showLedger = (wallet) => {
    if (!canReadLedger) return;
    onRouteDetailChange?.('ledger', wallet.wallet_id, normalizeAdminFinanceDetailQuery('ledger', {}));
  };
  const changeLedgerPage = (direction) => {
    if (!ledger || ledger.loading || ledger.page?.legacy || ledger.page?.expired) return;
    let cursor = '';
    if (direction === 'next') {
      cursor = ledger.page?.nextCursor || '';
      if (!cursor) return;
      rememberFinanceCursor(financeDetailCursorBackRef.current, 'ledger', ledger.query, cursor, ledger.wallet.wallet_id);
    } else {
      cursor = previousFinanceCursor(financeDetailCursorBackRef.current, 'ledger', ledger.query, ledger.wallet.wallet_id);
      if (cursor == null) return;
    }
    onRouteDetailChange?.('ledger', ledger.wallet.wallet_id, { ...ledger.query, cursor });
  };
  const searchLedger = (event) => {
    event.preventDefault();
    if (!ledger) return;
    onRouteDetailChange?.('ledger', ledger.wallet.wallet_id, normalizeAdminFinanceDetailQuery('ledger', { ...ledgerFilter, cursor: '' }));
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
  const ensureAdjustmentApproval = async (item, reason, requestKey) =>
    createAndSubmitGovernedApproval(client, walletAdjustmentApprovalDraft(item, reason, requestKey));
  const submitAdjustment = async (event) => {
    event.preventDefault();
    const amountMicrocredits = parsePointsInputToMicrocredits(adjustment.amount);
    const walletID = adjustment.walletID.trim();
    const reason = adjustment.reason.trim();
    if (!walletID || amountMicrocredits == null || amountMicrocredits === 0 || reason.length < 10) return;
    setBusy(true);
    setNotice('');
    setError('');
    let created = null;
    try {
      const supersedesID = walletAdjustmentReplacementReference(adjustmentRequestKey);
      if (adjustmentReplacement) {
        if (!supersedesID || supersedesID !== adjustmentReplacement.requestID || walletID !== adjustmentReplacement.walletID ||
          !walletReplacementEligible(await client.governedApprovalRequest(supersedesID),
            adjustmentReplacement.adjustmentID, walletID, currentUserID)) {
          throw new Error('old adjustment approval is not eligible for replacement');
        }
      }
      created = await client.createAdjustment({ wallet_id: walletID, amount_microcredits: amountMicrocredits, reason, idempotency_key: adjustmentRequestKey });
      await ensureAdjustmentApproval(created, reason, adjustmentRequestKey);
      await reload();
      setAdjustment({ walletID: '', amount: '', reason: '' });
      setAdjustmentRequestKey(secureRequestID());
      setAdjustmentReplacement(null);
      setNotice(copy.billing.adjustmentSubmitted);
    } catch {
      try {
        if (created) await reload();
      } catch {
        // Keep the original submission failure as the actionable result.
      }
      setError(created ? copy.billing.adjustmentApprovalPending : copy.billing.actionFailed);
    } finally {
      setBusy(false);
    }
  };
  const recoverAdjustmentApproval = (item) => {
    const requestKey = item.request_idempotency_key;
    if (!requestKey) return;
    void perform(() => ensureAdjustmentApproval(item, item.reason, requestKey));
  };
  const editAdjustmentApproval = (item, approval) => {
    const reason = String(adjustmentEditReasons[item.adjustment_id] || '').trim();
    if (!approval || approval.status !== 'DRAFT' || reason.length < 10 || reason.length > 2000) return;
    void perform(async () => {
      await editWalletAdjustmentApproval(client, item, approval, reason);
      setAdjustmentEditReasons((current) => ({ ...current, [item.adjustment_id]: '' }));
    });
  };
  const openAdjustmentApproval = (item) => {
    onNavigate?.('approvals', { approval_command_type: 'WALLET_ADJUSTMENT', approval_target: item.adjustment_id,
      approval_request_id: adjustmentApprovalByTarget.get(item.adjustment_id)?.approval_request_id || item.approval_request_id || null });
  };
  const replaceAdjustmentApproval = (item, approval) => {
    const requester = approval?.requester?.user_id || approval?.requester_user_id;
    if (!canRequestAdjustment || !currentUserID || requester !== currentUserID ||
      !['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED'].includes(approval?.status) ||
      approval?.command_type !== 'WALLET_ADJUSTMENT' ||
      (approval?.target?.resource_id || approval?.target_resource_id) !== item.adjustment_id) return;
    setAdjustment({ walletID: item.wallet_id, amount: '', reason: '' });
    setAdjustmentRequestKey(`replacement:${approval.approval_request_id}:${secureRequestID()}`);
    setAdjustmentReplacement({ requestID: approval.approval_request_id, adjustmentID: item.adjustment_id, walletID: item.wallet_id });
  };
  const refund = (item) => {
    if (!orderDetail) return;
    const orderID = orderDetail.order_id;
    openActionPanel({
      title: copy.billing.actionPanel.refundTitle,
      danger: true,
      idempotencyPrefix: 'target-refund',
      summary: [
        { label: copy.billing.order, value: orderDetail.merchant_order_no || orderID },
        { label: copy.billing.packageID, value: item.package_id },
      ],
      fields: [{ name: 'reason', label: copy.billing.actionPanel.reason, type: 'textarea', required: true, minLength: 10, maxLength: 2000 }],
      confirmation: copy.billing.actionPanel.refundConfirmation,
      execute: ({ reason }, { idempotencyKey }) => client.refundPaymentOrder(orderID, item.source_payment_transaction_id, item.package_id, reason, idempotencyKey),
      after: async () => {
        await reload();
        setOrderDetail(await client.paymentOrder(orderID));
      },
    });
  };
  const repair = (item) => {
    openActionPanel({
      title: copy.billing.actionPanel.repairTitle,
      summary: [{ label: copy.billing.order, value: item.merchant_order_no || item.order_id }],
      fields: [{ name: 'reason', label: copy.billing.actionPanel.reason, type: 'textarea', required: true, minLength: 10, maxLength: 2000 }],
      confirmation: copy.billing.actionPanel.repairConfirmation,
      execute: ({ reason }) => client.repairPaymentOrder(item.order_id, reason),
    });
  };
  const searchFinanceCollection = async (event, targetTab, filter, setFilter) => {
    event.preventDefault();
    const query = normalizeAdminFinanceQuery(targetTab, { ...filter, cursor: '' });
    setFilter(query);
    onRouteQueryChange?.(targetTab, query);
    await loadTab(targetTab, { query });
  };
  const changeFinancePage = async (direction) => {
    if (!isAdminFinancePaginatedTab(tab) || activeTabState.loading || activeTabState.page?.legacy || activeTabState.page?.expired) return;
    const query = normalizeAdminFinanceQuery(tab, activeTabState.query || routeQuery);
    const scope = surface;
    let cursor = '';
    if (direction === 'next') {
      cursor = activeTabState.page?.nextCursor || '';
      if (!cursor) return;
      rememberFinanceCursor(financeCursorBackRef.current, tab, query, cursor, scope);
    } else {
      cursor = previousFinanceCursor(financeCursorBackRef.current, tab, query, scope);
      if (cursor == null) return;
    }
    const nextQuery = { ...query, cursor };
    onRouteQueryChange?.(tab, nextQuery);
    await loadTab(tab, { query: nextQuery });
  };
  const restartExpiredFinancePage = () => {
    const query = resetExpiredFinanceQuery(activeTabState.query || routeQuery);
    onRouteQueryChange?.(tab, query, 'replace');
    void loadTab(tab, { query });
  };
  const release = (item) => {
    openActionPanel({
      title: copy.billing.actionPanel.releaseTitle,
      danger: true,
      idempotencyPrefix: 'hold-release',
      summary: [
        { label: copy.billing.holdID, value: item.hold_id },
        { label: copy.billing.maximum, value: formatUsagePoints(item.max_microcredits, language) },
      ],
      fields: [{ name: 'reason', label: copy.billing.actionPanel.reason, type: 'textarea', required: true, minLength: 10, maxLength: 2000 }],
      confirmation: copy.billing.actionPanel.releaseConfirmation,
      execute: ({ reason }, { idempotencyKey }) => client.releaseHold(item.hold_id, reason, idempotencyKey),
    });
  };
  const isCurrentReconciliationCaseRoute = useCallback((runID, query) => {
    return financeDetailRouteIsCurrent({
      canRead: canReadUsageRef.current,
      current: routeDetailRef.current,
      type: 'reconciliationCases',
      id: runID,
      query,
      normalizeQuery: (value) => normalizeAdminFinanceDetailQuery('reconciliationCases', value),
    });
  }, []);
  const loadReconciliationCasePage = useCallback(async (run, rawQuery = {}, options = {}) => {
    const query = normalizeAdminFinanceDetailQuery('reconciliationCases', rawQuery);
    const requestKey = `${surface}:reconciliation-cases-detail`;
    const routeMatches = () => isCurrentReconciliationCaseRoute(run.reconciliation_run_id, query);
    if (!routeMatches()) return null;
    setCases((current) => ({
      run,
      query,
      items: current?.run?.reconciliation_run_id === run.reconciliation_run_id ? current.items || [] : [],
      page: current?.run?.reconciliation_run_id === run.reconciliation_run_id ? current.page || null : null,
      loading: true,
      error: '',
    }));
    const result = await runFinanceDetailRequest({
      requestTracker,
      requestKey,
      load: () => client.reconciliationCases(run.reconciliation_run_id, query),
      isRouteCurrent: routeMatches,
    });
    if (result.status === 'stale') return null;
    if (result.status === 'ok') {
      const collection = normalizeFinanceCollection(result.value, query);
      setCases({ run, query, ...collection, loading: false, error: '' });
      return collection;
    }
    setCases((current) => current ? {
      ...current,
      loading: false,
      error: result.status === 'expired' ? copy.billing.snapshotExpired : copy.billing.actionFailed,
      ...(result.status === 'expired' ? { page: { ...current.page, expired: true } } : {}),
    } : current);
    if (options.throwOnError) throw result.error;
    return null;
  }, [client, copy.billing.actionFailed, copy.billing.snapshotExpired, isCurrentReconciliationCaseRoute, requestTracker, surface]);
  const showCases = (run) => onRouteDetailChange?.('reconciliationCases', run.reconciliation_run_id, normalizeAdminFinanceDetailQuery('reconciliationCases', {}));
  const changeReconciliationCasePage = (direction) => {
    if (!cases || cases.loading || cases.page?.legacy || cases.page?.expired) return;
    let cursor = '';
    if (direction === 'next') {
      cursor = cases.page?.nextCursor || '';
      if (!cursor) return;
      rememberFinanceCursor(financeDetailCursorBackRef.current, 'reconciliationCases', cases.query, cursor, cases.run.reconciliation_run_id);
    } else {
      cursor = previousFinanceCursor(financeDetailCursorBackRef.current, 'reconciliationCases', cases.query, cases.run.reconciliation_run_id);
      if (cursor == null) return;
    }
    onRouteDetailChange?.('reconciliationCases', cases.run.reconciliation_run_id, { ...cases.query, cursor });
  };
  const searchReconciliationCases = (event) => {
    event.preventDefault();
    if (!cases) return;
    onRouteDetailChange?.('reconciliationCases', cases.run.reconciliation_run_id, normalizeAdminFinanceDetailQuery('reconciliationCases', { ...caseFilter, cursor: '' }));
  };
  const routeDetailKey = JSON.stringify(routeDetail);
  useEffect(() => {
    if (routeDetail.type === 'ledger' && canReadLedger) {
      const query = normalizeAdminFinanceDetailQuery('ledger', routeDetail.query);
      setLedgerFilter(query);
      const wallet = (data.wallets || []).find((item) => item.wallet_id === routeDetail.id) || { wallet_id: routeDetail.id };
      if (!ledger || ledger.wallet?.wallet_id !== routeDetail.id || JSON.stringify(ledger.query) !== JSON.stringify(query)) void loadLedgerPage(wallet, query);
    } else {
      requestTracker.invalidate(`${surface}:ledger-detail`);
      setLedger(null);
    }
    if (routeDetail.type === 'reconciliationCases' && canReadUsage) {
      const query = normalizeAdminFinanceDetailQuery('reconciliationCases', routeDetail.query);
      setCaseFilter(query);
      const run = (data.reconciliations || []).find((item) => item.reconciliation_run_id === routeDetail.id) || { reconciliation_run_id: routeDetail.id };
      if (!cases || cases.run?.reconciliation_run_id !== routeDetail.id || JSON.stringify(cases.query) !== JSON.stringify(query)) void loadReconciliationCasePage(run, query);
    } else {
      requestTracker.invalidate(`${surface}:reconciliation-cases-detail`);
      setCases(null);
    }
  }, [canReadLedger, canReadUsage, loadLedgerPage, loadReconciliationCasePage, routeDetailKey, routeVersion]);
  const updateReconciliationCase = async (item, action) => {
    if (!canReconcileRef.current || !cases || cases.loading || cases.page?.expired) return;
    if (action === 'ASSIGN' && !canReadUserDirectoryRef.current) return;
    const run = cases.run;
    const query = { ...cases.query };
    const runID = run.reconciliation_run_id;
    const refreshCurrentCases = createFinanceDetailRecovery({
      query,
      load: (refreshQuery) => loadReconciliationCasePage(run, refreshQuery, { requireCurrentRoute: true, throwOnError: true }),
      onRestart: (refreshQuery) => {
        routeDetailRef.current = { type: 'reconciliationCases', id: runID, query: refreshQuery };
        onRouteDetailChange?.('reconciliationCases', runID, refreshQuery, 'replace');
      },
    });
    if (action === 'CLAIM') {
      setBusy(true);
      setNotice('');
      setCases((current) => current ? { ...current, error: '' } : current);
      try {
        await client.updateReconciliationCase(item.case_id, { action, reason: copy.billing.actionPanel.claimReason });
        await loadReconciliationCasePage(run, query, { requireCurrentRoute: true });
        if (isCurrentReconciliationCaseRoute(runID, query)) setNotice(copy.billing.completed);
      } catch {
        if (isCurrentReconciliationCaseRoute(runID, query)) setCases((current) => current ? { ...current, loading: false, error: copy.billing.actionFailed } : current);
      } finally {
        setBusy(false);
      }
      return;
    }
    const fields = [];
    if (action === 'ASSIGN') {
      if (!canReadUserDirectoryRef.current) return;
      const searchOwners = async (query) => {
        const sequence = ++ownerSearchSequence.current;
        setActionPanel((current) => current ? {
          ...current,
          lookupBusy: true,
          error: '',
          values: { ...current.values, owner_id: '' },
          fieldErrors: { ...current.fieldErrors, owner_id: '' },
        } : current);
        try {
          const result = await client.users(String(query || '').trim());
          if (sequence !== ownerSearchSequence.current) return;
          const options = (result.items || []).filter((user) => user.status === 'ACTIVE').map((user) => ({
            value: user.user_id,
            label: `${user.display_name || user.username || user.user_code} · ${user.user_code || shortID(user.user_id)}`,
          }));
          setActionPanel((current) => current ? {
            ...current,
            lookupBusy: false,
            fields: current.fields.map((field) => field.name === 'owner_id' ? { ...field, options } : field),
            fieldErrors: { ...current.fieldErrors, owner_id: options.length ? '' : copy.billing.actionPanel.noUsers },
          } : current);
        } catch {
          if (sequence !== ownerSearchSequence.current) return;
          setActionPanel((current) => current ? { ...current, lookupBusy: false, error: copy.billing.actionPanel.userDirectoryFailed } : current);
        }
      };
      fields.push({
        name: 'owner_query', label: copy.billing.actionPanel.userQuery, type: 'directory-search', submit: false,
        placeholder: copy.billing.actionPanel.userQueryPlaceholder, help: copy.billing.actionPanel.userQueryHelp, onSearch: searchOwners,
      });
      fields.push({ name: 'owner_id', label: copy.billing.actionPanel.owner, type: 'select', required: true, value: item.owner_id || '', options: [] });
    }
    if (action === 'SET_REPAIR_ACTION') fields.push({ name: 'repair_action', label: copy.billing.actionPanel.repairAction, type: 'textarea', required: true, maxLength: 500, value: item.repair_action || '' });
    if (action === 'RESOLVE' || action === 'IGNORE') fields.push({ name: 'review_result', label: copy.billing.actionPanel.reviewResult, type: 'textarea', required: true, maxLength: 2000, value: item.review_result || '' });
    fields.push({ name: 'reason', label: copy.billing.actionPanel.reason, type: 'textarea', required: true, minLength: 10, maxLength: 2000 });
    openActionPanel({
      title: copy.billing.actionPanel.caseTitles[action],
      danger: action === 'IGNORE',
      idempotencyPrefix: 'reconciliation-case',
      summary: [
        { label: copy.billing.caseID, value: item.case_id },
        { label: copy.billing.caseType, value: item.case_type },
        { label: copy.billing.status, value: item.status },
      ],
      fields,
      confirmation: copy.billing.actionPanel.caseConfirmations[action],
      execute: (values, { idempotencyKey }) => {
        if (!canReconcileRef.current || (action === 'ASSIGN' && !canReadUserDirectoryRef.current)) {
          const permissionError = new Error('PERMISSION_DENIED');
          permissionError.status = 403;
          throw permissionError;
        }
        return client.updateReconciliationCase(item.case_id, { action, ...values }, idempotencyKey);
      },
      after: refreshCurrentCases,
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
    const resolutionOptions = paymentAnomalyResolutionOptions(item);
    if (!resolutionOptions.length) return;
    openActionPanel({
      title: copy.billing.actionPanel.anomalyTitle,
      idempotencyPrefix: 'payment-anomaly-resolve',
      summary: [
        { label: copy.billing.caseID, value: item.case_id },
        { label: copy.billing.anomalyType, value: item.anomaly_type },
      ],
      fields: [
        { name: 'resolution', label: copy.billing.actionPanel.resolution, type: 'select', required: true, value: resolutionOptions[0], options: resolutionOptions },
        { name: 'reason', label: copy.billing.actionPanel.reason, type: 'textarea', required: true, minLength: 10, maxLength: 2000 },
      ],
      confirmation: copy.billing.actionPanel.anomalyConfirmation,
      execute: ({ resolution, reason }, { idempotencyKey }) => client.resolvePaymentAnomaly(item.case_id, resolution, reason, idempotencyKey),
      after: async () => {
        await reload();
        setAnomalyDetail(await client.paymentAnomaly(item.case_id));
      },
    });
  };
  const performLate = (action) =>
    perform(async () => {
      await action();
      if (lateReviewDetail) setLateReviewDetail(await client.latePaymentReview(lateReviewDetail.case.case_id));
    });
  const scheduleLate = (item) => {
    openActionPanel({
      title: copy.billing.actionPanel.scheduleTitle,
      summary: [
        { label: copy.billing.caseID, value: item.case_id },
        { label: copy.billing.order, value: item.merchant_order_no },
      ],
      fields: [
        { name: 'next_review_at', label: copy.billing.nextReview, type: 'datetime-local', required: true, future: true, value: new Date(Date.now() + 86400000).toISOString().slice(0, 16) },
        { name: 'reason', label: copy.billing.actionPanel.reason, type: 'textarea', required: true, minLength: 10, maxLength: 500 },
      ],
      confirmation: copy.billing.actionPanel.scheduleConfirmation,
      execute: ({ next_review_at, reason }) => client.scheduleLatePaymentReview(item.case_id, next_review_at, reason),
      after: async () => {
        await reload();
        setLateReviewDetail(await client.latePaymentReview(item.case_id));
      },
    });
  };
  const markProviderBlind = (item) => {
    openActionPanel({
      title: copy.billing.actionPanel.providerBlindTitle,
      danger: true,
      summary: [
        { label: copy.billing.caseID, value: item.case_id },
        { label: copy.billing.provider, value: item.provider_id },
      ],
      fields: [{ name: 'reason', label: copy.billing.actionPanel.reason, type: 'textarea', required: true, minLength: 10, maxLength: 500 }],
      confirmation: copy.billing.actionPanel.providerBlindConfirmation,
      execute: ({ reason }) => client.escalateProviderBlind(item.case_id, reason),
      after: async () => {
        await reload();
        setLateReviewDetail(await client.latePaymentReview(item.case_id));
      },
    });
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
  const adjustmentApprovalByTarget = latestApprovalByTarget(data.adjustmentApprovals || []);
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
        ...(canReadLedger ? [{
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => <button onClick={() => showLedger(item)}>{copy.billing.viewLedger}</button>,
        }] : []),
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
          key: 'status',
          label: copy.accounts.status,
          render: (item) => <Status value={item.status} />,
        },
        {
          key: 'effective_from',
          label: copy.billing.effectiveFrom,
          render: (item) => formatDate(item.effective_from),
        },
        {
          key: 'effective_to',
          label: copy.billing.effectiveTo,
          render: (item) => (item.effective_to ? formatDate(item.effective_to) : '—'),
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
          label: copy.billing.executionStatus,
          render: (item) => <Status value={item.status} />,
        },
        {
          key: 'approval_request_id',
          label: copy.billing.approvalStatus,
          render: (item) => {
            const approval = adjustmentApprovalByTarget.get(item.adjustment_id);
            const requestID = approval?.approval_request_id || item.approval_request_id;
            return requestID ? (
              <span>
                <Status value={approval?.status || '—'} />
                <br />
                <code title={requestID}>{shortID(requestID)}</code>
              </span>
            ) : (
              <Status value="NOT_SUBMITTED" />
            );
          },
        },
        {
          key: 'updated_at',
          label: copy.billing.updated,
          render: (item) => formatDate(adjustmentApprovalByTarget.get(item.adjustment_id)?.updated_at || item.updated_at),
        },
        {
          key: 'actions',
          label: copy.accounts.actions,
          render: (item) => {
            const approval = adjustmentApprovalByTarget.get(item.adjustment_id);
            const action = adjustmentApprovalAction(item, approval, { canRequest: canRequestAdjustment, canApprove: canApproveAdjustment });
            if (action === 'resume') return <div className="adm-row-actions">
              <button disabled={busy} onClick={() => recoverAdjustmentApproval(item)}>{copy.billing.resumeApproval}</button>
              {approval?.status === 'DRAFT' && <>
                <input aria-label={`${copy.billing.adjustmentReason} · ${item.adjustment_id}`} minLength="10" maxLength="2000" value={adjustmentEditReasons[item.adjustment_id] || ''} onChange={(event) => setAdjustmentEditReasons((current) => ({ ...current, [item.adjustment_id]: event.target.value }))} />
                <button disabled={busy || String(adjustmentEditReasons[item.adjustment_id] || '').trim().length < 10} onClick={() => editAdjustmentApproval(item, approval)}>{copy.billing.updateAndSubmitAdjustment}</button>
                <button onClick={() => openAdjustmentApproval(item)}>{copy.billing.viewApproval}</button>
              </>}
            </div>;
            if (action === 'view') return <div className="adm-row-actions">
              <button onClick={() => openAdjustmentApproval(item)}>{copy.billing.viewApproval}</button>
              {canRequestAdjustment && (approval?.requester?.user_id || approval?.requester_user_id) === currentUserID &&
                ['REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED'].includes(approval?.status) &&
                <button onClick={() => replaceAdjustmentApproval(item, approval)}>{copy.billing.replaceAdjustment}</button>}
            </div>;
            return '—';
          },
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
    [busy, canApproveAdjustment, canReadLedger, canRequestAdjustment, client, copy, data.adjustmentApprovals, language, normalRefundCreationEnabled, repairSupported],
  );
  const renderWithActionPanel = (content) => (
    <>
      {content}
      <StructuredActionPanel
        operation={actionPanel}
        copy={copy}
        busy={busy}
        onChange={changeActionField}
        onClose={() => setActionPanel(null)}
        onRefresh={refreshActionPanel}
        onSubmit={submitActionPanel}
      />
    </>
  );
  if (ledger)
    return renderWithActionPanel(
      <BillingDetail
        copy={copy}
        title={`${copy.billing.ledger} · ${shortID(ledger.wallet.wallet_id)}`}
        items={ledger.items}
        filters={(
          <form className="adm-ops-form" onSubmit={searchLedger}>
            <select value={ledgerFilter.entry_type} onChange={(event) => setLedgerFilter((current) => ({ ...current, entry_type: event.target.value }))}>
              <option value="">{copy.billing.allEntryTypes}</option>
              {['RECHARGE', 'USAGE', 'REFUND', 'REVERSAL', 'ADJUSTMENT', 'EXPIRY', 'RECEIVABLE', 'CHARGEBACK'].map((value) => <option key={value}>{value}</option>)}
            </select>
            <input value={ledgerFilter.business_reference} onChange={(event) => setLedgerFilter((current) => ({ ...current, business_reference: event.target.value }))} placeholder={copy.billing.businessReference} />
            <FinanceDateTimeRange copy={copy} value={ledgerFilter} onChange={(key, value) => setLedgerFilter((current) => ({ ...current, [key]: value }))} />
            <FinanceLimitSelect copy={copy} value={ledgerFilter.limit} onChange={(event) => setLedgerFilter((current) => ({ ...current, limit: event.target.value }))} />
            <button disabled={ledger.loading}>{copy.searchAction}</button>
          </form>
        )}
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
        page={ledger.page}
        limit={ledger.query?.limit || '50'}
        busy={ledger.loading}
        canPrevious={previousFinanceCursor(financeDetailCursorBackRef.current, 'ledger', ledger.query, ledger.wallet.wallet_id) !== null}
        onPrevious={() => changeLedgerPage('previous')}
        onNext={() => changeLedgerPage('next')}
        error={ledger.error}
        onRestart={() => ledger.page?.expired ? onRouteDetailChange?.('ledger', ledger.wallet.wallet_id, resetExpiredFinanceQuery(ledger.query), 'replace') : loadLedgerPage(ledger.wallet, ledger.query)}
        onBack={() => onRouteDetailChange?.('', '', {})}
      />
    );
  if (receivableDetail)
    return renderWithActionPanel(
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
    return renderWithActionPanel(
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
    return renderWithActionPanel(
      <BillingDetail
        copy={copy}
        title={`${copy.billing.cases} · ${cases.run.business_date || shortID(cases.run.reconciliation_run_id)}`}
        items={cases.items}
        filters={(
          <form className="adm-ops-form" onSubmit={searchReconciliationCases}>
            <select value={caseFilter.status} onChange={(event) => setCaseFilter((current) => ({ ...current, status: event.target.value }))}>
              <option value="">{copy.billing.allStatuses}</option>
              {['OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED'].map((value) => <option key={value}>{value}</option>)}
            </select>
            <select value={caseFilter.severity} onChange={(event) => setCaseFilter((current) => ({ ...current, severity: event.target.value }))}>
              <option value="">{copy.billing.allSeverities}</option>
              {['P0', 'P1', 'P2'].map((value) => <option key={value}>{value}</option>)}
            </select>
            <input value={caseFilter.owner_id} onChange={(event) => setCaseFilter((current) => ({ ...current, owner_id: event.target.value }))} placeholder={copy.billing.ownerID} />
            <select value={caseFilter.case_type} onChange={(event) => setCaseFilter((current) => ({ ...current, case_type: event.target.value }))}>
              <option value="">{copy.billing.allCaseTypes}</option>
              {['PROVIDER_PAID_INTERNAL_MISSING', 'INTERNAL_PAID_PROVIDER_MISSING', 'AMOUNT_MISMATCH', 'DUPLICATE_TRANSACTION', 'PROVIDER_REFUND_INTERNAL_MISSING', 'INTERNAL_REFUND_PROVIDER_MISSING', 'REFUND_AMOUNT_MISMATCH', 'PROVIDER_CHARGEBACK_INTERNAL_MISSING', 'INTERNAL_CHARGEBACK_PROVIDER_MISSING', 'PAYMENT_LEDGER_MISSING', 'REFUND_LEDGER_MISSING', 'CHARGEBACK_LEDGER_MISSING', 'UNSUPPORTED_PROVIDER_ENTRY'].map((value) => <option key={value}>{value}</option>)}
            </select>
            <FinanceDateTimeRange copy={copy} value={caseFilter} onChange={(key, value) => setCaseFilter((current) => ({ ...current, [key]: value }))} />
            <FinanceLimitSelect copy={copy} value={caseFilter.limit} onChange={(event) => setCaseFilter((current) => ({ ...current, limit: event.target.value }))} />
            <button disabled={cases.loading}>{copy.searchAction}</button>
          </form>
        )}
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
          ...(canReconcile ? [{
            key: 'actions',
            label: copy.accounts.actions,
            render: (item) => (
              <div className="adm-row-actions">
                {!item.owner_id && ['OPEN', 'IN_REVIEW'].includes(item.status) && (
                  <button disabled={busy || cases.loading || cases.page?.expired} onClick={() => updateReconciliationCase(item, 'CLAIM')}>
                    {copy.billing.claimReview}
                  </button>
                )}
                {canReadUserDirectory && (
                  <button disabled={busy || cases.loading || cases.page?.expired || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'ASSIGN')}>
                    {copy.billing.assignOwner}
                  </button>
                )}
                <button disabled={busy || cases.loading || cases.page?.expired || !item.owner_id || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'SET_REPAIR_ACTION')}>
                  {copy.billing.setRepairAction}
                </button>
                <button disabled={busy || cases.loading || cases.page?.expired || !item.owner_id || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'RESOLVE')}>
                  {copy.billing.resolveCase}
                </button>
                <button disabled={busy || cases.loading || cases.page?.expired || !item.owner_id || !['OPEN', 'IN_REVIEW'].includes(item.status)} onClick={() => updateReconciliationCase(item, 'IGNORE')}>
                  {copy.billing.ignoreCase}
                </button>
              </div>
            ),
          }] : []),
        ]}
        page={cases.page}
        limit={cases.query?.limit || '50'}
        busy={cases.loading}
        canPrevious={previousFinanceCursor(financeDetailCursorBackRef.current, 'reconciliationCases', cases.query, cases.run.reconciliation_run_id) !== null}
        onPrevious={() => changeReconciliationCasePage('previous')}
        onNext={() => changeReconciliationCasePage('next')}
        error={cases.error}
        onRestart={() => cases.page?.expired ? onRouteDetailChange?.('reconciliationCases', cases.run.reconciliation_run_id, resetExpiredFinanceQuery(cases.query), 'replace') : loadReconciliationCasePage(cases.run, cases.query)}
        onBack={() => onRouteDetailChange?.('', '', {})}
      />
    );
  if (anomalyDetail) {
    const item = anomalyDetail.case;
    return renderWithActionPanel(
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
            <button disabled={busy || !canRequestRepair || item.status !== 'OPEN' || !item.owner_id || !paymentAnomalyResolutionOptions(item).length} title={!canRequestRepair ? copy.permissionDenied : undefined} onClick={() => resolveAnomaly(item)}>
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
    return renderWithActionPanel(
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
    return renderWithActionPanel(
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
    <>
      {!embedded && canReadWallets && <div className="adm-finance-overviews"><FinanceOverviewBlock copy={copy} language={language} surface={overviewSurface} load={overviewLoad} onDrilldown={drilldownOverview} canDrilldown={(target) => visibleTabs.includes(target.tab)} /></div>}
      <section className={embedded ? 'adm-finance-subpanel' : 'adm-panel'}>
      <StructuredActionPanel
        operation={actionPanel}
        copy={copy}
        busy={busy}
        onChange={changeActionField}
        onClose={() => setActionPanel(null)}
        onRefresh={refreshActionPanel}
        onSubmit={submitActionPanel}
      />
      {!embedded && <SectionHeader title={surface === 'payments' ? copy.billing.paymentsTitle : surface === 'products' ? copy.nav.products : copy.billing.title} copy={copy}>
        <div className="adm-row-actions">
          {activeTabState.updatedAt && <small>{copy.updated} {new Date(activeTabState.updatedAt).toLocaleTimeString()}</small>}
          <button type="button" disabled={activeTabState.loading || busy} onClick={() => void reload()}>{copy.refresh}</button>
        </div>
      </SectionHeader>}
      {embedded && <div className="adm-section-tools"><span>{activeTabState.updatedAt ? `${copy.updated} ${new Date(activeTabState.updatedAt).toLocaleTimeString()}` : ''}</span><button type="button" disabled={activeTabState.loading || busy} onClick={() => void reload()}>{copy.refresh}</button></div>}
      {surface === 'payments' && hasLimitedPaymentCapabilities && (
        <div className="adm-inline-message">
          {copy.billing.capabilityLimitations} {copy.billing.reconciliationMode}: <strong>{paymentCapabilities.reconciliation_mode || '—'}</strong>
        </div>
      )}
      {surface === 'payments' && data.paymentCapabilityState?.error && <div className="adm-inline-message error">{copy.billing.capabilitiesUnavailable}</div>}
      {!embedded && <Tabs active={tab} onChange={changeTab} items={visibleTabs.map((key) => ({ key, label: copy.billing[key] }))} />}
      {tab === 'wallets' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'wallets', walletFilter, setWalletFilter)}>
          <input value={walletFilter.q} onChange={(event) => setWalletFilter((current) => ({ ...current, q: event.target.value }))} placeholder={copy.billing.walletSearch} />
          <select value={walletFilter.owner_type} onChange={(event) => setWalletFilter((current) => ({ ...current, owner_type: event.target.value }))}>
            <option value="">{copy.billing.allOwnerTypes}</option>
            {['PERSONAL', 'ORGANIZATION', 'ORG_UNIT'].map((value) => <option key={value}>{value}</option>)}
          </select>
          <select value={walletFilter.status} onChange={(event) => setWalletFilter((current) => ({ ...current, status: event.target.value }))}>
            <option value="">{copy.billing.allStatuses}</option>
            {['ACTIVE', 'FROZEN', 'CLOSED'].map((value) => <option key={value}>{value}</option>)}
          </select>
          <FinanceLimitSelect copy={copy} value={walletFilter.limit} onChange={(event) => setWalletFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'receivables' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'receivables', receivableFilter, setReceivableFilter)}>
          <input
            value={receivableFilter.wallet_id}
            onChange={(event) => setReceivableFilter((current) => ({ ...current, wallet_id: event.target.value }))}
            placeholder={copy.billing.walletID}
          />
          <select value={receivableFilter.status} onChange={(event) => setReceivableFilter((current) => ({ ...current, status: event.target.value }))}>
            <option value="">{copy.billing.allStatuses}</option>
            {['OPEN', 'PARTIALLY_SETTLED', 'SETTLED', 'WRITTEN_OFF'].map((status) => <option key={status}>{status}</option>)}
          </select>
          <input value={receivableFilter.source_type} onChange={(event) => setReceivableFilter((current) => ({ ...current, source_type: event.target.value }))} placeholder={copy.billing.sourceType} />
          <FinanceDateTimeRange copy={copy} value={receivableFilter} onChange={(key, value) => setReceivableFilter((current) => ({ ...current, [key]: value }))} />
          <FinanceLimitSelect copy={copy} value={receivableFilter.limit} onChange={(event) => setReceivableFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'adjustments' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'adjustments', adjustmentFilter, setAdjustmentFilter)}>
          <input value={adjustmentFilter.wallet_id} onChange={(event) => setAdjustmentFilter((current) => ({ ...current, wallet_id: event.target.value }))} placeholder={copy.billing.walletID} />
          <select value={adjustmentFilter.status} onChange={(event) => setAdjustmentFilter((current) => ({ ...current, status: event.target.value }))}>
            <option value="">{copy.billing.allStatuses}</option>
            {['PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'POSTED'].map((value) => <option key={value}>{value}</option>)}
          </select>
          <input value={adjustmentFilter.requested_by} onChange={(event) => setAdjustmentFilter((current) => ({ ...current, requested_by: event.target.value }))} placeholder={copy.billing.requestedBy} />
          <FinanceDateTimeRange copy={copy} value={adjustmentFilter} onChange={(key, value) => setAdjustmentFilter((current) => ({ ...current, [key]: value }))} />
          <FinanceLimitSelect copy={copy} value={adjustmentFilter.limit} onChange={(event) => setAdjustmentFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'adjustments' && canRequestAdjustment && (
        <form className="adm-ops-form" onSubmit={submitAdjustment}>
          {adjustmentReplacement && <div className="adm-inline-message" role="status">
            {copy.billing.replacingAdjustment} <code>{shortID(adjustmentReplacement.requestID)}</code>
            <button type="button" onClick={() => { setAdjustmentReplacement(null); setAdjustmentRequestKey(secureRequestID()); setAdjustment({ walletID: '', amount: '', reason: '' }); }}>{copy.billing.cancelReplacement}</button>
          </div>}
          <label>
            {copy.billing.adjustmentWallet}
            <input list="adjustment-wallet-options" value={adjustment.walletID} onChange={(event) => setAdjustment((current) => ({ ...current, walletID: event.target.value }))} placeholder={copy.billing.selectWallet} readOnly={Boolean(adjustmentReplacement)} required />
            <datalist id="adjustment-wallet-options">
              {(data.wallets || []).map((wallet) => (
                <option key={wallet.wallet_id} value={wallet.wallet_id}>{wallet.owner_type} · {shortID(wallet.owner_id)}</option>
              ))}
            </datalist>
          </label>
          <label>
            {copy.billing.adjustmentAmount}
            <input type="number" step="0.0001" value={adjustment.amount} onChange={(event) => setAdjustment((current) => ({ ...current, amount: event.target.value }))} required />
          </label>
          <label>
            {copy.billing.adjustmentReason}
            <input minLength="10" maxLength="2000" value={adjustment.reason} onChange={(event) => setAdjustment((current) => ({ ...current, reason: event.target.value }))} required />
          </label>
          <div className="adm-action-confirmation">
            <strong>{copy.billing.actionPanel.confirmation}</strong>
            <span>{copy.billing.actionPanel.adjustmentConfirmation(adjustment.walletID ? shortID(adjustment.walletID) : '—', adjustment.amount || '—')}</span>
          </div>
          <button disabled={busy}>{copy.billing.submitAdjustment}</button>
          <small>{copy.billing.adjustmentApprovalBoundary}</small>
        </form>
      )}
      {tab === 'holds' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'holds', holdFilter, setHoldFilter)}>
          <input value={holdFilter.wallet_id} onChange={(event) => setHoldFilter((current) => ({ ...current, wallet_id: event.target.value }))} placeholder={copy.billing.walletID} />
          <select value={holdFilter.status} onChange={(event) => setHoldFilter((current) => ({ ...current, status: event.target.value }))}>
            <option value="">{copy.billing.allStatuses}</option>
            {['ACTIVE', 'COMMITTED', 'RELEASED', 'EXPIRED', 'REVIEW'].map((value) => <option key={value}>{value}</option>)}
          </select>
          <input value={holdFilter.model_id} onChange={(event) => setHoldFilter((current) => ({ ...current, model_id: event.target.value }))} placeholder={copy.billing.modelID} />
          <FinanceDateTimeRange copy={copy} value={holdFilter} onChange={(key, value) => setHoldFilter((current) => ({ ...current, [key]: value }))} />
          <FinanceLimitSelect copy={copy} value={holdFilter.limit} onChange={(event) => setHoldFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'orders' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'orders', orderFilter, setOrderFilter)}>
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
          <input value={orderFilter.provider_id} onChange={(event) => setOrderFilter((current) => ({ ...current, provider_id: event.target.value }))} placeholder={copy.billing.provider} />
          <input value={orderFilter.channel} onChange={(event) => setOrderFilter((current) => ({ ...current, channel: event.target.value }))} placeholder={copy.billing.channel} />
          <FinanceDateTimeRange copy={copy} value={orderFilter} onChange={(key, value) => setOrderFilter((current) => ({ ...current, [key]: value }))} />
          <FinanceLimitSelect copy={copy} value={orderFilter.limit} onChange={(event) => setOrderFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'refunds' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'refunds', refundFilter, setRefundFilter)}>
          <input value={refundFilter.q} onChange={(event) => setRefundFilter((current) => ({ ...current, q: event.target.value }))} placeholder={copy.billing.refundSearch} />
          <select value={refundFilter.status} onChange={(event) => setRefundFilter((current) => ({ ...current, status: event.target.value }))}>
            <option value="">{copy.billing.allStatuses}</option>
            {['REQUESTED', 'SUCCEEDED', 'FAILED', 'MANUAL_REVIEW', 'REVIEW_REQUIRED', 'RESOLVED'].map((value) => <option key={value}>{value}</option>)}
          </select>
          <input value={refundFilter.provider_id} onChange={(event) => setRefundFilter((current) => ({ ...current, provider_id: event.target.value }))} placeholder={copy.billing.provider} />
          <FinanceDateTimeRange copy={copy} value={refundFilter} onChange={(key, value) => setRefundFilter((current) => ({ ...current, [key]: value }))} />
          <FinanceLimitSelect copy={copy} value={refundFilter.limit} onChange={(event) => setRefundFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'lateReviews' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'lateReviews', lateFilter, setLateFilter)}>
          <input value={lateFilter.q} onChange={(event) => setLateFilter((current) => ({ ...current, q: event.target.value }))} placeholder={copy.billing.caseSearch} />
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
          <select value={lateFilter.severity} onChange={(event) => setLateFilter((current) => ({ ...current, severity: event.target.value }))}>
            <option value="">{copy.billing.allSeverities}</option>
            {['P0', 'P1', 'P2'].map((value) => <option key={value}>{value}</option>)}
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
          <FinanceDateTimeRange copy={copy} value={lateFilter} onChange={(key, value) => setLateFilter((current) => ({ ...current, [key]: value }))} />
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
          <FinanceLimitSelect copy={copy} value={lateFilter.limit} onChange={(event) => setLateFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'anomalies' && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'anomalies', anomalyFilter, setAnomalyFilter)}>
          <input value={anomalyFilter.q} onChange={(event) => setAnomalyFilter((current) => ({ ...current, q: event.target.value }))} placeholder={copy.billing.caseSearch} />
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
          <input
            value={anomalyFilter.owner_id}
            onChange={(event) => setAnomalyFilter((current) => ({ ...current, owner_id: event.target.value }))}
            placeholder={copy.billing.ownerID}
          />
          <input value={anomalyFilter.provider_id} onChange={(event) => setAnomalyFilter((current) => ({ ...current, provider_id: event.target.value }))} placeholder={copy.billing.provider} />
          <FinanceDateTimeRange copy={copy} value={anomalyFilter} onChange={(key, value) => setAnomalyFilter((current) => ({ ...current, [key]: value }))} />
          <FinanceLimitSelect copy={copy} value={anomalyFilter.limit} onChange={(event) => setAnomalyFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
        </form>
      )}
      {tab === 'prices' && (
        <div className="adm-inline-message info adm-pricing-read-only" role="status">
          <strong>{copy.billing.pricingReadOnlyTitle}</strong>
          <span>{copy.billing.pricingReadOnlyDescription}</span>
        </div>
      )}
      {tab === 'reconciliations' && canReadUsage && (
        <form className="adm-ops-form" onSubmit={(event) => searchFinanceCollection(event, 'reconciliations', reconciliationFilter, setReconciliationFilter)}>
          <input value={reconciliationFilter.provider_id} onChange={(event) => setReconciliationFilter((current) => ({ ...current, provider_id: event.target.value }))} placeholder={copy.billing.provider} />
          <input value={reconciliationFilter.channel} onChange={(event) => setReconciliationFilter((current) => ({ ...current, channel: event.target.value }))} placeholder={copy.billing.channel} />
          <select value={reconciliationFilter.status} onChange={(event) => setReconciliationFilter((current) => ({ ...current, status: event.target.value }))}>
            <option value="">{copy.billing.allStatuses}</option>
            {['RUNNING', 'COMPLETED', 'FAILED'].map((value) => <option key={value}>{value}</option>)}
          </select>
          <label>{copy.billing.businessDateFrom}<input type="date" value={reconciliationFilter.business_date_from} onChange={(event) => setReconciliationFilter((current) => ({ ...current, business_date_from: event.target.value }))} /></label>
          <label>{copy.billing.businessDateTo}<input type="date" value={reconciliationFilter.business_date_to} onChange={(event) => setReconciliationFilter((current) => ({ ...current, business_date_to: event.target.value }))} /></label>
          <FinanceLimitSelect copy={copy} value={reconciliationFilter.limit} onChange={(event) => setReconciliationFilter((current) => ({ ...current, limit: event.target.value }))} />
          <button disabled={busy}>{copy.searchAction}</button>
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
      {activeTabState.error && <div className="adm-inline-message error" role="alert">{activeTabState.error} <button type="button" disabled={activeTabState.loading} onClick={activeTabState.page?.expired ? restartExpiredFinancePage : () => void reload()}>{activeTabState.page?.expired ? copy.billing.restartPagination : copy.retry}</button></div>}
      {activeTabState.loading && !activeTabState.loaded && <div className="adm-empty" role="status">{copy.loading}</div>}
      {activeTabState.loaded && (tab === 'controls' ? <PaymentControlsPanel copy={copy} data={data} client={client} permissions={permissions} onNavigate={onNavigate} onReload={() => loadTab('controls', { force: true })} /> : <Table columns={columns[tab]} items={items} copy={copy} />)}
      {activeTabState.loaded && isAdminFinancePaginatedTab(tab) && (
        <FinancePagination
          copy={copy}
          count={items.length}
          limit={activeTabState.query?.limit || '50'}
          page={activeTabState.page}
          canPrevious={previousFinanceCursor(financeCursorBackRef.current, tab, activeTabState.query || routeQuery, surface) !== null}
          loading={activeTabState.loading}
          onPrevious={() => void changeFinancePage('previous')}
          onNext={() => void changeFinancePage('next')}
        />
      )}
      </section>
    </>
  );
}

function BillingDetail({ copy, title, items, columns, filters = null, page = null, limit = '50', busy = false, canPrevious = false, onPrevious, onNext, error = '', onRestart, onBack }) {
  return (
    <DetailDrawer label={title} onClose={onBack}>
    <section className="adm-panel">
      <button className="adm-back" onClick={onBack}>
        ← {copy.billing.back}
      </button>
      <SectionHeader title={title} count={page ? undefined : items.length} copy={copy} />
      {filters}
      {error && <div className="adm-inline-message error" role="alert">{error} <button type="button" disabled={busy} onClick={onRestart}>{page?.expired ? copy.billing.restartPagination : copy.retry}</button></div>}
      {busy && items.length === 0 && <div className="adm-empty" role="status">{copy.loading}</div>}
      {(!busy || items.length > 0) && <Table columns={columns} items={items} copy={copy} />}
      {page && <FinancePagination copy={copy} count={items.length} limit={limit} page={page} canPrevious={canPrevious} loading={busy} onPrevious={onPrevious} onNext={onNext} />}
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
  // The legacy Token Gateway remains read-only. Structured writes live only in
  // the gated model/Token workspaces, so the old page can never bypass impact review.
  const canManageTokens = false;
  const canValidateCredentials = false;
  const canReadModels = permissionSet.has('platform.models.read');
  const canManageModels = false;
  const canSyncModels = false;
  const canReadUsage = permissionSet.has('platform.usage.read');
  const canRetryUsage = false;
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
  const refreshProvisionings = async () => {
    const latest = await client.provisionings();
    setData((current) => ({ ...current, provisionings: latest.items || [] }));
  };
  const unavailableWrite = () => {};
  const preventUnavailableWrite = (event) => event.preventDefault();
  const updateStatus = unavailableWrite;
  const saveRoute = preventUnavailableWrite;
  const deleteRoute = unavailableWrite;
  const validateCredential = unavailableWrite;
  const createTokenAccount = preventUnavailableWrite;
  const retryReconciliation = unavailableWrite;
  const syncModels = unavailableWrite;
  const editModel = unavailableWrite;
  const publishModel = unavailableWrite;
  const transitionModel = unavailableWrite;
  const rollbackCatalog = unavailableWrite;
  const repairProvisioning = unavailableWrite;
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
    [copy.accounts.status, <Status key="accounts.status" value={item.status} />],
    [copy.tokens.executionStatus, <Status key="tokens.executionStatus" value={item.execution_status} />],
    [copy.tokens.deliveryStatus, <Status key="tokens.deliveryStatus" value={item.delivery_status} />],
    [copy.tokens.cancelStatus, <Status key="tokens.cancelStatus" value={item.cancel_status} />],
    [copy.tokens.settlementStatus, <Status key="tokens.settlementStatus" value={item.settlement_status} />],
    [copy.tokens.maximum, formatPoints(item.maximum_microcredits, language)],
    [copy.tokens.actual, formatPoints(item.actual_microcredits, language)],
    [copy.tokens.provider, item.provider_id || '—'],
    [copy.tokens.adapterVersion, item.adapter_version || '—'],
    [copy.tokens.upstreamReference, <TechnicalID key="tokens.upstreamReference" value={item.upstream_request_id} copy={copy} compact={false} />],
    [copy.tokens.attemptStatus, item.attempt_status || '—'],
    [copy.tokens.tokens, `${formatNumber(item.input_tokens)} / ${formatNumber(item.output_tokens)} / ${formatNumber(item.cached_input_tokens)}`],
    [copy.tokens.persistedSequence, formatNumber(item.last_persisted_sequence)],
    [copy.tokens.confirmedSequence, formatNumber(item.last_client_confirmed_sequence)],
    [copy.tokens.reconciliationAttempts, formatNumber(item.reconciliation_attempts)],
    [copy.tokens.lastError, item.reconciliation_last_error || item.error_class || '—'],
    [copy.tokens.cancellationEvidence, <pre key="tokens.cancellationEvidence" className="adm-json-detail">{JSON.stringify(item.cancellation_evidence || [], null, 2)}</pre>],
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
  const [exportReason, setExportReason] = useState('');
  const [exportAttempted, setExportAttempted] = useState(false);
  const [ticketIssueUnknown, setTicketIssueUnknown] = useState(false);
  const exportAttemptRef = useRef(null);
  const exportTicketRef = useRef(null);
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
    if (!exportAttemptRef.current) {
      let filter;
      try { filter = queryFilters(); } catch { setError(copy.audit.exportRequestInvalid); return; }
      if (!filter.from || !filter.to) { setError(copy.audit.exportRangeRequired); return; }
      try {
        exportAttemptRef.current = newAuditExportAttempt(filter, exportReason, includePII,
          `audit-export-${secureRequestID()}`);
      } catch { setError(copy.audit.exportRequestInvalid); return; }
      setExportAttempted(true);
    }
    setBusy(true);
    setError('');
    try {
      const job = await submitAuditExportAttempt(client, exportAttemptRef.current);
      setExportJob(job);
      setExportKey(job.decryption_key || '');
    } catch (cause) {
      // A definite rejection proves the server answered and created no export
      // job, so the operator may correct the reason or PII choice and start a
      // new attempt. Every other outcome - 5xx, idempotency conflict, request
      // timeout, network loss, abort - is an unknown result and keeps the frozen
      // attempt, leaving retrying the identical request as the only action.
      const status = typeof cause?.status === 'number' ? cause.status : 0;
      if (status >= 400 && status < 500 && status !== 408 && status !== 409) {
        exportAttemptRef.current = null;
        setExportAttempted(false);
      }
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
      const job = await client.auditExport(exportJob.export_id);
      setExportJob(job);
      if (!job.download_ticket_issued_at) setTicketIssueUnknown(false);
    } catch {
      setError(copy.audit.exportFailed);
    } finally {
      setBusy(false);
    }
  };
  const downloadExport = async () => {
    if (!exportJob || (exportJob.download_ticket_issued_at && !exportTicketRef.current) || ticketIssueUnknown) return;
    setBusy(true);
    setError('');
    try {
      if (!exportTicketRef.current) {
        try {
          exportTicketRef.current = await acquireAuditExportTicket(client, exportJob.export_id, null);
        } catch (error) {
          setTicketIssueUnknown(true);
          throw error;
        }
      }
      const blob = await client.downloadAuditExport(exportJob.export_id, exportTicketRef.current.download_token);
      const href = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = href;
      link.download = exportJob.filename || `pinvou-audit-${exportJob.export_id}.pae`;
      document.body.append(link);
      try {
        link.click();
      } finally {
        link.remove();
        window.setTimeout(() => URL.revokeObjectURL(href), 60_000);
      }
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
      <SectionHeader title={copy.audit.title} count={items.length} copy={copy} />
      <form className="adm-ops-form" onSubmit={search}>
        <input aria-label={copy.audit.actorFilter} placeholder={copy.audit.actorFilter} value={filters.actor} onChange={(event) => setFilters((current) => ({ ...current, actor: event.target.value }))} />
        <input
          aria-label={copy.audit.actionFilter}
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
          aria-label={copy.audit.resourceFilter}
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
          aria-label={copy.audit.result}
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
        <button disabled={busy}>{copy.searchAction}</button>
      </form>
      <p>{copy.audit.exportLimits}</p>
      {canExport && <form className="adm-ops-form" onSubmit={(event) => { event.preventDefault(); void createExport(); }}>
        <label>{copy.audit.exportReason}<input value={exportReason} minLength="10" maxLength="500" disabled={exportAttempted || busy || Boolean(exportJob)} onChange={(event) => setExportReason(event.target.value)} required /></label>
        <label><input type="checkbox" checked={includePII} disabled={!canExportPII || exportAttempted || busy || Boolean(exportJob)} title={!canExportPII ? copy.permissionDenied : undefined} onChange={(event) => setIncludePII(event.target.checked)} />{copy.audit.includePII}</label>
        <button type="submit" disabled={busy || Boolean(exportJob)}>{exportAttempted ? copy.audit.retryExport : copy.audit.export}</button>
        {exportAttempted && !exportJob && <small role="status">{copy.audit.retrySameExport}</small>}
      </form>}
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
            <button disabled={busy || ticketIssueUnknown || (Boolean(exportJob.download_ticket_issued_at) && !exportTicketRef.current)} onClick={downloadExport}>
              {copy.audit.download}
            </button>
          )}
          {ticketIssueUnknown && <small role="status">{copy.audit.ticketIssueUnknown}</small>}
          {exportJob.download_ticket_issued_at && !exportTicketRef.current && <small role="status">{copy.audit.ticketUnavailable}</small>}
          <button type="button" disabled={busy} onClick={() => { exportAttemptRef.current = null; exportTicketRef.current = null; setTicketIssueUnknown(false); setExportAttempted(false); setExportJob(null); setExportKey(''); setExportReason(''); setIncludePII(false); }}>{copy.audit.newExport}</button>
        </div>
      )}
      {error && <div className="adm-inline-message error" role="alert">{error}</div>}
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
    [copy.audit.resource, <span key="audit.resource">{item.resource_type} · <TechnicalID value={item.resource_id} copy={copy} compact={false} /></span>],
    [copy.audit.result, <Status key="audit.result" value={item.result} />],
    [copy.audit.request, <TechnicalID key="audit.request" value={item.request_id} copy={copy} compact={false} />],
    [copy.audit.organization, <TechnicalID key="audit.organization" value={item.active_organization_id} copy={copy} compact={false} />],
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
