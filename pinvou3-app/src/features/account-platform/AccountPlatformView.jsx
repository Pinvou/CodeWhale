import React, { useEffect, useRef, useState } from 'react';
import { createContextRequestGuard, rechargePollDelay } from './context-guard.js';
import { formatPoints, formatUsagePoints, formatWholePoints, MICROCREDITS_PER_POINT } from './credit-units.js';
import { activeEntitlementPackages, allEntitlementPackages, entitlementProgress, remainingEntitlementMicrocredits } from './entitlement-view.js';
import { compactOrderID, isPayablePaymentOrder, orderDurationDays, orderSummaryStatus, payablePaymentOrders, paymentOrderRemainingSeconds, summarizedPaymentOrders } from './order-view.js';
import { availablePortalSection, initialPortalSection, portalNavigationSections, portalSectionLocation } from './portal-sections.js';
import { authorizationVersionChanged, authorizationVersions, hasPermission, personalSurfaceAccess } from './authorization-ui.js';
import { summarizeUsageRecords } from './usage-summary.js';
import { localeNames, supportedLocales } from '../../shared/supported-locales.js';
import './account-platform.css';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'EXPIRED', 'MANUAL_REVIEW']);
const idempotencyKey = (prefix) => `${prefix}-${globalThis.crypto.randomUUID()}`;

function Field({ label, children }) {
  return (
    <label className="ap-field">
      <span>{label}</span>
      {children}
    </label>
  );
}

function Empty({ children }) {
  return <div className="ap-empty">{children}</div>;
}

function ResourceSection({ name, personalContext, copy, children }) {
  const title = name === 'credits' ? copy.creditsTitle : copy.entitlementsTitle;
  const lead = name === 'credits' ? copy.creditsLead : copy.entitlementsLead;
  return (
    <div className={`ap-resource-section ap-${name}-section ${personalContext ? 'ap-context-personal' : 'ap-context-organization'}`}>
      <p className="ap-eyebrow">{copy.sections[name]}</p>
      <h2>{title}</h2>
      <p className="ap-lead">{lead}</p>
      {children}
    </div>
  );
}

function ContextCredits({ copy, language, balance }) {
  return (
    <div className="ap-balance-hero">
      <span>{copy.availableCredits}</span>
      <strong>{formatWholePoints(balance?.available_microcredits, language)}</strong>
      <small>
        {copy.reservedCredits}: {formatPoints(balance?.reserved_microcredits, language)}
      </small>
    </div>
  );
}

function NavIcon({ name }) {
  const paths = {
    overview: (
      <>
        <rect x="3" y="3" width="7" height="7" rx="2" />
        <rect x="14" y="3" width="7" height="7" rx="2" />
        <rect x="3" y="14" width="7" height="7" rx="2" />
        <rect x="14" y="14" width="7" height="7" rx="2" />
      </>
    ),
    organization: (
      <>
        <circle cx="12" cy="7" r="3" />
        <circle cx="6" cy="17" r="2.5" />
        <circle cx="18" cy="17" r="2.5" />
        <path d="M12 10v3M8 15l4-2 4 2" />
      </>
    ),
    credits: (
      <>
        <rect x="3" y="5" width="18" height="14" rx="3" />
        <path d="M3 9h18M16 14h2" />
      </>
    ),
    entitlements: (
      <>
        <path d="M4 7h16v13H4zM2 7h20M12 7v13M7 3c2.5 0 5 4 5 4S9.5 7 7 7a2 2 0 1 1 0-4ZM17 3c-2.5 0-5 4-5 4s2.5 0 5 0a2 2 0 1 0 0-4Z" />
      </>
    ),
    orders: (
      <>
        <path d="M6 3h12v18H6z" />
        <path d="M9 8h6M9 12h6M9 16h4" />
      </>
    ),
    invoice: (
      <>
        <path d="M5 3h14v18l-3-2-4 2-4-2-3 2z" />
        <path d="M8 8h8M8 12h8" />
      </>
    ),
    usage: (
      <>
        <path d="M4 19V9M10 19V5M16 19v-7M22 19H2" />
      </>
    ),
    profile: (
      <>
        <circle cx="12" cy="8" r="4" />
        <path d="M4 21a8 8 0 0 1 16 0M18 4l2 2M20 4l-2 2" />
      </>
    ),
    closure: (
      <>
        <path d="M5 4h14v16H5zM8 8h8M8 12h8M8 16h5" />
      </>
    ),
    creditsUsage: (
      <>
        <rect x="3" y="5" width="18" height="14" rx="3" />
        <path d="M3 9h18M7 16v-3M12 16v-5M17 16v-2" />
      </>
    ),
    ordersInvoices: (
      <>
        <path d="M5 3h14v18l-3-2-4 2-4-2-3 2z" />
        <path d="M8 8h8M8 12h8M8 16h5" />
      </>
    ),
    accountSettings: (
      <>
        <circle cx="12" cy="8" r="4" />
        <path d="M4 21a8 8 0 0 1 16 0M18 4l2 2M20 4l-2 2" />
      </>
    ),
  };
  return (
    <svg className="ap-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {paths[name]}
    </svg>
  );
}

function localizedError(copy, reason) {
  const raw = String(reason?.code || reason?.message || reason || '');
  const code = raw.split(':', 1)[0].trim();
  if (code === 'RECENT_AUTHENTICATION_REQUIRED') return copy.recentAuthenticationRequired;
  if (code === 'USERNAME_INVALID') return copy.usernameInvalid;
  if (code === 'USERNAME_ALREADY_REGISTERED') return copy.usernameAlreadyRegistered;
  if (code === 'MOBILE_INVALID') return copy.mobileInvalid;
  if (code === 'MOBILE_ALREADY_REGISTERED') return copy.mobileAlreadyRegistered;
  if (code === 'CAPTCHA_INVALID') return copy.captchaInvalid;
  if (code === 'SMS_RATE_LIMITED') return copy.smsRateLimited;
  if (code === 'SMS_CODE_INVALID') return copy.mobileAuthFailed;
  if (code === 'PROFILE_CHANGE_CONFLICT') return copy.profileChangeConflict;
  if (code === 'MOBILE_VERIFICATION_REQUIRED') return copy.mobileVerificationRequired;
  if (code === 'PROFILE_CHANGE_NOT_FOUND') return copy.profileChangeNotFound;
  return copy.errors?.[code] || copy.requestFailed;
}

export function AccountPlatformView({ client, copy, language, onLanguageChange, portal = false, onClose }) {
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [organizations, setOrganizations] = useState([]);
  const [units, setUnits] = useState([]);
  const [members, setMembers] = useState([]);
  const [products, setProducts] = useState([]);
  const [balance, setBalance] = useState(null);
  const [billingUnavailable, setBillingUnavailable] = useState(false);
  const [billingDetailsUnavailable, setBillingDetailsUnavailable] = useState(false);
  const [usageRecords, setUsageRecords] = useState([]);
  const [usageCursor, setUsageCursor] = useState(null);
  const [usageUnavailable, setUsageUnavailable] = useState(false);
  const [section, setSection] = useState(() => (portal ? initialPortalSection(globalThis.location?.search || '') : 'overview'));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [order, setOrder] = useState(null);
  const [orders, setOrders] = useState([]);
  const [packages, setPackages] = useState({ summary: null, items: [] });
  const [quotaGrants, setQuotaGrants] = useState([]);
  const [quotaMembers, setQuotaMembers] = useState([]);
  const [catalogModels, setCatalogModels] = useState([]);
  const [modelPolicy, setModelPolicy] = useState(null);
  const [closure, setClosure] = useState(null);
  const [qrData, setQrData] = useState('');
  const guardRef = useRef(createContextRequestGuard());
  const meRef = useRef(null);
  const contextMutationRef = useRef(false);
  const contextRefreshRef = useRef(false);
  const activeOrganizationId = me?.active_context?.organization_id || '';
  const activeOrganization = organizations.find((item) => item.organization_id === activeOrganizationId);
  const personalContext = !activeOrganizationId;
  const access = personalSurfaceAccess(me);
  const activePermissions = new Set(me?.active_context?.permissions || []);
  const canReadOrganization = activePermissions.has('org.profile.read') || activePermissions.has('org.units.read') || activePermissions.has('org.members.read') || activePermissions.has('org.models.read');
  const navigationSections = portal ? portalNavigationSections.filter((key) => key === 'accountSettings' || (key === 'creditsUsage' && (access.billingRead || access.usageRead)) || (key === 'ordersInvoices' && personalContext && access.billingRead)) : [...(access.profileRead || access.billingRead ? ['overview'] : []), ...(access.billingRead ? ['credits', 'entitlements'] : []), ...(access.usageRead ? ['usage'] : []), ...(organizations.length > 0 && canReadOrganization ? ['organization'] : []), ...(personalContext && access.billingRead ? ['orders', 'invoice'] : []), 'closure'];
  const pendingOrderRefreshKey = orders
    .filter((item) => ['CREATED', 'PENDING'].includes(item.status) && item.close_status === 'OPEN')
    .map((item) => `${item.order_id}:${item.status}:${item.close_status}:${item.expires_at}`)
    .join('|');

  const refreshClosure = async (signal) => {
    try {
      setClosure(await client.accountClosure(signal));
    } catch (reason) {
      if (reason?.status !== 404 && reason?.name !== 'AbortError') throw reason;
    }
  };

  const clearContextResources = () => {
    guardRef.current.cancel();
    setBalance(null);
    setUsageRecords([]);
    setUsageCursor(null);
    setUsageUnavailable(false);
    setUnits([]);
    setMembers([]);
    setOrder(null);
    setOrders([]);
    setPackages({ summary: null, items: [] });
    setQuotaGrants([]);
    setQuotaMembers([]);
    setCatalogModels([]);
    setModelPolicy(null);
    setBillingUnavailable(false);
    setBillingDetailsUnavailable(false);
  };

  const loadContext = async (nextMe = me) => {
    const request = guardRef.current.begin();
    setError('');
    const organizationId = nextMe?.active_context?.organization_id;
    try {
      const isPersonal = !organizationId;
      const surfaceAccess = personalSurfaceAccess(nextMe);
      const permissions = new Set(nextMe?.active_context?.permissions || []);
      const canReadOrganizationEntitlements = permissions.has('org.wallet.read') || permissions.has('org.quota.allocate') || permissions.has('org.entitlements.purchase');
      const canReadQuota = permissions.has('org.quota.read.self') || permissions.has('org.quota.read.all');
      const canReadMembers = permissions.has('org.members.read');
      const canAllocateQuota = permissions.has('org.quota.allocate');
      const canReadModelPolicy = permissions.has('org.models.read') || permissions.has('org.models.manage');
      const canReadModels = isPersonal ? hasPermission(nextMe, 'personal.models.read') : canReadModelPolicy;
      const results = await Promise.allSettled([hasPermission(nextMe, 'personal.profile.read') ? client.organizations(request.signal) : Promise.resolve({ items: [] }), surfaceAccess.rechargeCreate ? client.products(request.signal) : Promise.resolve({ items: [] }), surfaceAccess.billingRead ? client.balance(request.signal) : Promise.resolve(null), Promise.resolve({ order: null }), organizationId ? client.units(organizationId, request.signal) : Promise.resolve({ items: [] }), organizationId && canReadMembers ? client.members(organizationId, request.signal) : Promise.resolve({ items: [] }), surfaceAccess.usageRead ? client.usageHistory('', request.signal) : Promise.resolve({ items: [] }), surfaceAccess.billingRead && (isPersonal || canReadOrganizationEntitlements) ? client.entitlementPackages(request.signal) : Promise.resolve({ summary: null, items: [] }), isPersonal && surfaceAccess.billingRead ? client.rechargeOrders(request.signal) : Promise.resolve({ items: [] }), organizationId && canReadQuota ? client.organizationQuotaGrants(organizationId, request.signal) : Promise.resolve({ items: [] }), organizationId && canAllocateQuota ? client.quotaMembers(organizationId, request.signal) : Promise.resolve({ items: [] }), canReadModels ? client.models(request.signal) : Promise.resolve({ items: [] }), organizationId && canReadModelPolicy ? client.organizationModelPolicy(organizationId, request.signal) : Promise.resolve(null)]);
      if (!guardRef.current.isCurrent(request.generation)) return;
      const value = (index) => (results[index].status === 'fulfilled' ? results[index].value : null);
      setOrganizations(value(0)?.items || []);
      setProducts(value(1)?.items || []);
      setBalance(value(2));
      setOrder(value(3)?.order || null);
      setUnits(value(4)?.items || []);
      setMembers(value(5)?.items || []);
      setUsageRecords(value(6)?.items || []);
      setUsageCursor(value(6)?.next_cursor || null);
      setUsageUnavailable(results[6].status === 'rejected');
      setPackages(value(7) || { summary: null, items: [] });
      setOrders(value(8)?.items || []);
      setQuotaGrants(value(9)?.items || []);
      setQuotaMembers(value(10)?.items || []);
      setCatalogModels(value(11)?.items || []);
      setModelPolicy(value(12));
      const billingFailed = [1, 2, 3].some((index) => results[index].status === 'rejected');
      setBillingUnavailable(billingFailed);
      setBillingDetailsUnavailable([7, 8, 9].some((index) => results[index].status === 'rejected'));
      if ([0, 4, 5].some((index) => results[index].status === 'rejected')) setError(copy.requestFailed);
    } catch (reason) {
      if (reason?.name !== 'AbortError' && guardRef.current.isCurrent(request.generation)) setError(copy.requestFailed);
    }
  };

  useEffect(() => {
    let active = true;
    (async () => {
      try {
        const session = await client.session();
        if (!active) return;
        if (!session.authenticated) {
          setAuth('guest');
          if (session.errorCode && session.errorCode !== 'account_platform_disabled') setError(localizedError(copy, session.errorCode));
          return;
        }
        const account = await client.bootstrap();
        if (!active) return;
        meRef.current = account;
        setMe(account);
        setAuth('ready');
        await Promise.all([loadContext(account), refreshClosure()]);
      } catch {
        if (active) {
          setAuth('error');
          setError(copy.requestFailed);
        }
      }
    })();
    return () => {
      active = false;
      guardRef.current.cancel();
    };
  }, [client]);

  useEffect(() => {
    let disposed = false;
    const synchronize = async () => {
      if (!meRef.current) return;
      if (contextMutationRef.current || contextRefreshRef.current) return;
      contextRefreshRef.current = true;
      try {
        const account = await client.bootstrap();
        if (disposed) return;
        if (authorizationVersionChanged(authorizationVersions(meRef.current), authorizationVersions(account))) {
          clearContextResources();
          meRef.current = account;
          setMe(account);
          await loadContext(account);
        }
      } catch (reason) {
        if (!disposed && reason?.name !== 'AbortError') setError(localizedError(copy, reason));
      } finally {
        contextRefreshRef.current = false;
      }
    };
    const unsubscribeContext = client.onContextVersion?.(() => {
      void synchronize();
    });
    const unsubscribeAuthorization = client.onAuthorizationVersion?.(() => {
      void synchronize();
    });
    const onFocus = () => {
      if (meRef.current) void synchronize();
    };
    globalThis.addEventListener?.('focus', onFocus);
    return () => {
      disposed = true;
      unsubscribeContext?.();
      unsubscribeAuthorization?.();
      globalThis.removeEventListener?.('focus', onFocus);
    };
  }, [client]);

  useEffect(() => {
    setQrData(isPayablePaymentOrder(order) ? order.qr_data || '' : '');
    if (!isPayablePaymentOrder(order)) return undefined;
    const expiresIn = new Date(order.expires_at).getTime() - Date.now();
    const timer = globalThis.setTimeout(() => setQrData(''), Math.max(0, expiresIn));
    return () => globalThis.clearTimeout(timer);
  }, [order?.order_id, order?.qr_data, order?.status, order?.close_status, order?.expires_at]);

  useEffect(() => {
    if (auth !== 'ready' || !personalContext || !pendingOrderRefreshKey) return undefined;
    let stopped = false;
    const refresh = async () => {
      try {
        const history = await client.rechargeOrders();
        if (!stopped) setOrders(history.items || []);
      } catch {
        // Preserve the last authoritative list while the background refresh retries.
      }
    };
    const timer = globalThis.setInterval(() => void refresh(), 5000);
    return () => {
      stopped = true;
      globalThis.clearInterval(timer);
    };
  }, [auth, client, personalContext, pendingOrderRefreshKey]);

  useEffect(() => {
    if (!closure || closure.status === 'COMPLETED') return undefined;
    const timer = globalThis.setInterval(() => {
      void refreshClosure().catch((reason) => setError(localizedError(copy, reason)));
    }, 5000);
    return () => globalThis.clearInterval(timer);
  }, [closure?.closure_request_id, closure?.status]);

  useEffect(() => {
    if (!portal) {
      if (!navigationSections.includes(section)) setSection('overview');
      return;
    }
    if (auth !== 'ready') return;
    const nextSection = navigationSections.includes(section) ? section : availablePortalSection(globalThis.location?.search || '', navigationSections);
    if (nextSection !== section) setSection(nextSection);
    const nextLocation = portalSectionLocation(nextSection, globalThis.location?.search || '', globalThis.location?.pathname || '/', true);
    if (`${globalThis.location?.pathname || '/'}${globalThis.location?.search || ''}` !== nextLocation) {
      globalThis.history?.replaceState?.({}, '', nextLocation);
    }
  }, [auth, portal, section, navigationSections.join('|')]);

  useEffect(() => {
    if (!portal) return undefined;
    const onPopState = () => setSection(initialPortalSection(globalThis.location?.search || ''));
    globalThis.addEventListener?.('popstate', onPopState);
    return () => globalThis.removeEventListener?.('popstate', onPopState);
  }, [portal]);

  useEffect(() => {
    if (portal && (auth === 'guest' || auth === 'error')) globalThis.location.replace('/auth/login');
  }, [auth, portal]);

  useEffect(() => {
    if (!order?.order_id || terminalOrderStates.has(order.status)) return undefined;
    let stopped = false;
    let timer;
    let attempt = 0;
    const poll = async () => {
      try {
        const latest = await client.rechargeOrder(order.order_id);
        if (stopped) return;
        setOrder(latest);
        setOrders((current) => current.map((item) => (item.order_id === latest.order_id ? { ...item, status: latest.status, fulfillment_status: latest.fulfillment_status, close_status: latest.close_status, expires_at: latest.expires_at, paid_at: latest.paid_at } : item)));
        if (latest.status === 'PAID' || latest.status === 'PAID_LATE') {
          setBalance(await client.balance());
          setNotice(copy.paymentReceived);
          return;
        }
        if (!terminalOrderStates.has(latest.status) && new Date(latest.expires_at).getTime() > Date.now()) {
          timer = setTimeout(poll, rechargePollDelay(attempt++));
        }
      } catch {
        if (!stopped) timer = setTimeout(poll, rechargePollDelay(attempt++));
      }
    };
    timer = setTimeout(poll, rechargePollDelay(attempt++));
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [order?.order_id, order?.status]);

  const run = async (action) => {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      await action();
      return true;
    } catch (reason) {
      if (reason?.name !== 'AbortError') setError(localizedError(copy, reason));
      return false;
    } finally {
      setBusy(false);
    }
  };

  const createPersonalRecharge = (productId, channel) =>
    run(async () => {
      const created = await client.createRecharge(productId, channel, idempotencyKey('recharge'));
      const product = products.find((item) => item.product_id === productId);
      setOrder(created);
      setOrders((current) => [
        {
          ...created,
          plan_name: product?.plan_name || productId,
          display_points: product?.display_points || 0,
          created_at: created.created_at || new Date().toISOString(),
        },
        ...current.filter((item) => item.order_id !== created.order_id),
      ]);
    });

  const activatePersonalRecharge = async (orderId) => {
    let payable = false;
    const loaded = await run(async () => {
      const latest = await client.rechargeOrder(orderId);
      payable = isPayablePaymentOrder(latest);
      setOrder(latest);
      setOrders((current) => current.map((item) => (item.order_id === latest.order_id ? { ...item, status: latest.status, fulfillment_status: latest.fulfillment_status, close_status: latest.close_status, expires_at: latest.expires_at, paid_at: latest.paid_at } : item)));
    });
    return loaded && payable;
  };

  const login = () =>
    run(async () => {
      await client.login();
      const account = await client.bootstrap();
      meRef.current = account;
      setMe(account);
      setAuth('ready');
      await loadContext(account);
    });

  const logout = () =>
    run(async () => {
      await client.logout();
      guardRef.current.cancel();
      meRef.current = null;
      setMe(null);
      setOrganizations([]);
      setUnits([]);
      setMembers([]);
      setProducts([]);
      setOrders([]);
      setPackages({ summary: null, items: [] });
      setQuotaGrants([]);
      setQuotaMembers([]);
      setBalance(null);
      setUsageRecords([]);
      setUsageCursor(null);
      setUsageUnavailable(false);
      setBillingDetailsUnavailable(false);
      setOrder(null);
      setQrData('');
      setAuth('guest');
      if (portal) window.location.reload();
    });

  const switchContext = (value) =>
    run(async () => {
      clearContextResources();
      const organizationId = value || null;
      contextMutationRef.current = true;
      try {
        const expectedVersion = Number(meRef.current?.active_context?.context_version || 0);
        const context = await client.switchContext(organizationId ? 'ORGANIZATION' : 'PERSONAL', organizationId, expectedVersion);
        const updated = { ...meRef.current, active_context: context };
        meRef.current = updated;
        setMe(updated);
        await loadContext(updated);
      } catch (reason) {
        if (reason?.code === 'ACCOUNT_CONFLICT') {
          const account = await client.bootstrap();
          meRef.current = account;
          setMe(account);
          await loadContext(account);
        }
        throw reason;
      } finally {
        contextMutationRef.current = false;
      }
    });

  const loadMoreUsage = () =>
    run(async () => {
      if (!usageCursor) return;
      const request = guardRef.current.begin();
      const page = await client.usageHistory(usageCursor, request.signal);
      if (!guardRef.current.isCurrent(request.generation)) return;
      setUsageRecords((current) => {
        const known = new Set(current.map((item) => item.usage_request_id));
        return [...current, ...(page.items || []).filter((item) => !known.has(item.usage_request_id))];
      });
      setUsageCursor(page.next_cursor || null);
    });

  const requestClosure = () =>
    run(async () => {
      const confirmation = globalThis.prompt(copy.closure.confirmPrompt);
      if (confirmation !== 'CLOSE_MY_ACCOUNT') {
        if (confirmation !== null) setError(copy.closure.confirmMismatch);
        return;
      }
      const created = await client.createAccountClosure(confirmation, idempotencyKey('account-closure'));
      setClosure(created);
      setNotice(copy.closure.requested);
    });

  const finishProfileChange = async (result) => {
    if (result?.status !== 'COMPLETED') {
      setNotice(copy.profileChangeRecovering);
      return;
    }
    await client.logout();
    globalThis.location.assign('/auth/login?return_to=%2Faccount');
  };

  if (auth === 'loading')
    return (
      <div className={`ap-shell ap-centered ${portal ? 'ap-portal' : 'ap-desktop'}`}>
        <div className="ap-spinner" />
        {copy.loading}
      </div>
    );
  if (portal && (auth === 'guest' || auth === 'error'))
    return (
      <div className="ap-shell ap-centered ap-portal">
        <div className="ap-spinner" />
        {copy.loading}
      </div>
    );
  if (auth === 'guest' || auth === 'error')
    return (
      <div className={`ap-shell ap-login ${portal ? 'ap-portal' : 'ap-desktop'}`}>
        <div>
          <p className="ap-eyebrow">PINVOU ACCOUNT PLATFORM</p>
          <h1>{copy.title}</h1>
          <p>{portal ? copy.loginIntro : copy.desktopLoginIntro}</p>
          {error && (
            <div role="alert" className="ap-alert error">
              {error}
            </div>
          )}
          {portal ? (
            <a className="ap-primary" href="/auth/login">
              {copy.login}
            </a>
          ) : (
            <button className="ap-primary" disabled={busy} onClick={login}>
              {busy ? copy.signingIn : copy.login}
            </button>
          )}
          {onClose && (
            <button className="ap-close" onClick={onClose}>
              {copy.close}
            </button>
          )}
        </div>
      </div>
    );

  const userLabel = me?.display_name || copy.user;
  const identityLabel = activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal;
  const navigateSection = (key) => {
    setSection(key);
    if (!portal) return;
    const nextLocation = portalSectionLocation(key, globalThis.location?.search || '', globalThis.location?.pathname || '/');
    if (`${globalThis.location?.pathname || '/'}${globalThis.location?.search || ''}` !== nextLocation) {
      globalThis.history?.pushState?.({}, '', nextLocation);
    }
  };

  return (
    <div className={`ap-shell ap-app ${portal ? 'ap-portal' : 'ap-desktop'}`}>
      <aside className="ap-sidebar">
        <div className="ap-sidebar-brand">
          <span className="ap-brand-mark">P</span>
          <div>
            <p className="ap-eyebrow">PINVOU</p>
            <h1>{copy.title}</h1>
          </div>
        </div>
        <div className="ap-user-summary">
          <span className="ap-user-avatar">{userLabel.trim().slice(0, 1).toUpperCase()}</span>
          <div>
            <strong>{userLabel}</strong>
            <span>{identityLabel}</span>
          </div>
        </div>
        <nav>
          {navigationSections.map((key) => (
            <button key={key} className={section === key ? 'active' : ''} onClick={() => navigateSection(key)}>
              <NavIcon name={key} />
              <span>{copy.sections[key]}</span>
            </button>
          ))}
        </nav>
        <div className="ap-sidebar-foot">
          <select aria-label={copy.language} value={language} onChange={(event) => onLanguageChange(event.target.value)}>
            {supportedLocales.map((locale) => (
              <option key={locale} value={locale}>
                {localeNames[locale]}
              </option>
            ))}
          </select>
          <button onClick={logout}>{copy.logout}</button>
          {onClose && <button onClick={onClose}>{copy.close}</button>}
        </div>
      </aside>
      <main className="ap-main">
        {error && (
          <div role="alert" className="ap-alert error">
            {error}
          </div>
        )}
        {notice && <div className="ap-alert success">{notice}</div>}
        {portal && section === 'creditsUsage' && (
          <PortalCreditsUsage
            copy={copy}
            language={language}
            personalContext={personalContext}
            organizationId={activeOrganizationId}
            products={products}
            balance={balance}
            packages={packages}
            orders={orders}
            billingUnavailable={billingUnavailable}
            detailsUnavailable={billingDetailsUnavailable}
            order={order}
            qrData={qrData}
            busy={busy}
            permissions={me?.active_context?.permissions || []}
            members={quotaMembers}
            grants={quotaGrants}
            usageRecords={usageRecords}
            usageUnavailable={usageUnavailable}
            usageCursor={usageCursor}
            canRecharge={access.rechargeCreate}
            canCancelUsage={access.usageCancel}
            onRecharge={createPersonalRecharge}
            onActivateRecharge={activatePersonalRecharge}
            onPurchase={(productId, channel) => run(async () => setOrder(await client.purchaseOrganizationEntitlements(activeOrganizationId, productId, channel, idempotencyKey('org-recharge'))))}
            onAllocate={(items) =>
              run(async () => {
                await client.allocateOrganizationQuota(activeOrganizationId, items, idempotencyKey('quota-batch'));
                await loadContext(me);
              })
            }
            onRevoke={(grantId, reason) =>
              run(async () => {
                await client.revokeOrganizationQuota(activeOrganizationId, grantId, reason, idempotencyKey('quota-revoke'));
                await loadContext(me);
              })
            }
            onReclaim={(grantId, reason) =>
              run(async () => {
                await client.reclaimAccountClosedGrant(activeOrganizationId, grantId, reason, idempotencyKey('closed-grant-reclaim'));
                await loadContext(me);
              })
            }
            onLoadMoreUsage={loadMoreUsage}
            onCancelUsage={(usageRequestID) =>
              run(async () => {
                await client.cancelUsage(usageRequestID, idempotencyKey('usage-cancel'));
                await loadContext(me);
                setNotice(copy.stopRequested);
              })
            }
          />
        )}
        {portal && section === 'ordersInvoices' && <PortalOrdersInvoices copy={copy} language={language} orders={orders} unavailable={billingDetailsUnavailable} />}
        {portal && section === 'accountSettings' && (
          <PortalAccountSettings
            copy={copy}
            me={me}
            access={access}
            organizations={organizations}
            activeOrganization={activeOrganization}
            activeOrganizationId={activeOrganizationId}
            busy={busy}
            onSwitchContext={switchContext}
            client={client}
            onProfileError={(reason) => localizedError(copy, reason)}
            onProfileChanged={finishProfileChange}
            onProfileRecovering={() => setNotice(copy.profileChangeRecovering)}
            closure={closure}
            onRequestClosure={requestClosure}
            units={units}
            members={members}
            models={catalogModels}
            modelPolicy={modelPolicy}
            onCreateUnit={(name, parentId) =>
              run(async () => {
                await client.createUnit(activeOrganizationId, name, parentId);
                await loadContext(me);
              })
            }
            onInvite={(email, roles) =>
              run(async () => {
                const invite = await client.invite(activeOrganizationId, email, roles);
                setNotice(`${copy.invitationCreated} ${invite.token}`);
              })
            }
            onUpdateModelPolicy={(mode, modelIds, reason) =>
              run(async () => {
                await client.updateOrganizationModelPolicy(
                  activeOrganizationId,
                  {
                    expected_version: modelPolicy?.version || 0,
                    mode,
                    model_ids: modelIds,
                    reason,
                  },
                  idempotencyKey('model-policy'),
                );
                await loadContext(me);
              })
            }
          />
        )}
        {!portal && (
          <>
            {section === 'overview' && <Overview copy={copy} language={language} me={me} organizations={organizations} balance={balance} activeOrganization={activeOrganization} activeOrganizationId={activeOrganizationId} busy={busy} canSwitch={access.contextSwitch} onSwitchContext={switchContext} />}
            {section === 'credits' && (
              <ResourceSection name="credits" personalContext={personalContext} copy={copy}>
                {personalContext ? <Wallet copy={copy} language={language} products={products} balance={balance} packages={packages} orders={orders} billingUnavailable={billingUnavailable} detailsUnavailable={billingDetailsUnavailable} order={order} qrData={qrData} busy={busy} canRecharge={access.rechargeCreate} onRecharge={createPersonalRecharge} onActivateRecharge={activatePersonalRecharge} /> : <ContextCredits copy={copy} language={language} balance={balance} />}
              </ResourceSection>
            )}
            {section === 'entitlements' && (
              <ResourceSection name="entitlements" personalContext={personalContext} copy={copy}>
                {personalContext ? (
                  <Wallet copy={copy} language={language} products={products} balance={balance} packages={packages} orders={orders} billingUnavailable={billingUnavailable} detailsUnavailable={billingDetailsUnavailable} order={order} qrData={qrData} busy={busy} canRecharge={access.rechargeCreate} onRecharge={createPersonalRecharge} onActivateRecharge={activatePersonalRecharge} />
                ) : (
                  <OrganizationEntitlements
                    copy={copy}
                    language={language}
                    organizationId={activeOrganizationId}
                    permissions={me?.active_context?.permissions || []}
                    products={products}
                    packages={packages}
                    members={quotaMembers}
                    grants={quotaGrants}
                    order={order}
                    busy={busy}
                    onPurchase={(productId, channel) => run(async () => setOrder(await client.purchaseOrganizationEntitlements(activeOrganizationId, productId, channel, idempotencyKey('org-recharge'))))}
                    onAllocate={(items) =>
                      run(async () => {
                        await client.allocateOrganizationQuota(activeOrganizationId, items, idempotencyKey('quota-batch'));
                        await loadContext(me);
                      })
                    }
                    onRevoke={(grantId, reason) =>
                      run(async () => {
                        await client.revokeOrganizationQuota(activeOrganizationId, grantId, reason, idempotencyKey('quota-revoke'));
                        await loadContext(me);
                      })
                    }
                    onReclaim={(grantId, reason) =>
                      run(async () => {
                        await client.reclaimAccountClosedGrant(activeOrganizationId, grantId, reason, idempotencyKey('closed-grant-reclaim'));
                        await loadContext(me);
                      })
                    }
                  />
                )}
              </ResourceSection>
            )}
            {section === 'organization' && (
              <Organization
                copy={copy}
                organization={activeOrganization}
                units={units}
                members={members}
                models={catalogModels}
                modelPolicy={modelPolicy}
                permissions={me?.active_context?.permissions || []}
                busy={busy}
                onCreateUnit={(name, parentId) =>
                  run(async () => {
                    await client.createUnit(activeOrganizationId, name, parentId);
                    await loadContext(me);
                  })
                }
                onInvite={(email, roles) =>
                  run(async () => {
                    const invite = await client.invite(activeOrganizationId, email, roles);
                    setNotice(`${copy.invitationCreated} ${invite.token}`);
                  })
                }
                onUpdateModelPolicy={(mode, modelIds, reason) =>
                  run(async () => {
                    await client.updateOrganizationModelPolicy(
                      activeOrganizationId,
                      {
                        expected_version: modelPolicy?.version || 0,
                        mode,
                        model_ids: modelIds,
                        reason,
                      },
                      idempotencyKey('model-policy'),
                    );
                    await loadContext(me);
                  })
                }
              />
            )}
            {section === 'orders' && <Orders copy={copy} language={language} orders={orders} unavailable={billingDetailsUnavailable} />}
            {section === 'invoice' && <Invoice copy={copy} />}
            {section === 'usage' && (
              <Usage
                copy={copy}
                language={language}
                records={usageRecords}
                unavailable={usageUnavailable}
                hasMore={Boolean(usageCursor)}
                busy={busy}
                canCancel={access.usageCancel}
                onLoadMore={loadMoreUsage}
                onCancel={(usageRequestID) =>
                  run(async () => {
                    await client.cancelUsage(usageRequestID, idempotencyKey('usage-cancel'));
                    await loadContext(me);
                    setNotice(copy.stopRequested);
                  })
                }
              />
            )}
            {section === 'profile' && <ProfileSettings copy={copy} client={client} me={me} onError={(reason) => setError(localizedError(copy, reason))} onChanged={finishProfileChange} onRecovering={() => setNotice(copy.profileChangeRecovering)} />}
            {section === 'closure' && <AccountClosure copy={copy} item={closure} busy={busy} canRequest={access.closureRequest} onRequest={requestClosure} />}
          </>
        )}
      </main>
    </div>
  );
}

function PortalCreditsUsage({ copy, language, personalContext, organizationId, products, balance, packages, orders, billingUnavailable, detailsUnavailable, order, qrData, busy, permissions, members, grants, usageRecords, usageUnavailable, usageCursor, canRecharge, canCancelUsage, onRecharge, onActivateRecharge, onPurchase, onAllocate, onRevoke, onReclaim, onLoadMoreUsage, onCancelUsage }) {
  return (
    <section className="ap-combined-tab">
      <p className="ap-eyebrow">{copy.sections.creditsUsage}</p>
      <h2>{copy.creditsUsageTitle}</h2>
      {personalContext ? (
        <Wallet copy={copy} language={language} products={products} balance={balance} packages={packages} orders={orders} billingUnavailable={billingUnavailable} detailsUnavailable={detailsUnavailable} order={order} qrData={qrData} busy={busy} canRecharge={canRecharge} onRecharge={onRecharge} onActivateRecharge={onActivateRecharge} embedded />
      ) : (
        <>
          <ContextCredits copy={copy} language={language} balance={balance} />
          <OrganizationEntitlements copy={copy} language={language} organizationId={organizationId} permissions={permissions} products={products} packages={packages} members={members} grants={grants} order={order} busy={busy} onPurchase={onPurchase} onAllocate={onAllocate} onRevoke={onRevoke} onReclaim={onReclaim} />
        </>
      )}
      <Usage copy={copy} language={language} records={usageRecords} unavailable={usageUnavailable} hasMore={Boolean(usageCursor)} busy={busy} canCancel={canCancelUsage} onLoadMore={onLoadMoreUsage} onCancel={onCancelUsage} embedded />
    </section>
  );
}

function PortalOrdersInvoices({ copy, language, orders, unavailable }) {
  return (
    <section className="ap-combined-tab">
      <p className="ap-eyebrow">{copy.sections.ordersInvoices}</p>
      <h2>{copy.ordersInvoicesTitle}</h2>
      <p className="ap-lead">{copy.ordersInvoicesLead}</p>
      <Orders copy={copy} language={language} orders={orders} unavailable={unavailable} successfulOnly embedded />
      <Invoice copy={copy} embedded />
    </section>
  );
}

function PortalAccountSettings({ copy, me, access, organizations, activeOrganization, activeOrganizationId, busy, onSwitchContext, client, onProfileError, onProfileChanged, onProfileRecovering, closure, onRequestClosure, units, members, models, modelPolicy, onCreateUnit, onInvite, onUpdateModelPolicy }) {
  const requestedProfile = new URLSearchParams(globalThis.location?.search || '').get('profile');
  const initialProfile = requestedProfile === 'username' || requestedProfile === 'mobile' ? requestedProfile : '';
  const [profileDialog, setProfileDialog] = useState(initialProfile);
  const [identityDialog, setIdentityDialog] = useState(false);
  const closeProfileDialog = () => {
    setProfileDialog('');
    if (requestedProfile) globalThis.history?.replaceState?.({}, '', portalSectionLocation('accountSettings', globalThis.location?.search || '', globalThis.location?.pathname || '/'));
  };
  return (
    <section className="ap-combined-tab">
      <p className="ap-eyebrow">{copy.sections.accountSettings}</p>
      <h2>{copy.accountSettingsTitle}</h2>
      <p className="ap-lead">{copy.accountSettingsLead}</p>
      <AccountProfileCard copy={copy} me={me} organizations={organizations} activeOrganization={activeOrganization} canSwitchIdentity={access.contextSwitch} canEditProfile={access.profileUpdate} onSwitchIdentity={() => setIdentityDialog(true)} onEditUsername={() => setProfileDialog('username')} onEditMobile={() => setProfileDialog('mobile')} />
      {activeOrganization && <Organization copy={copy} organization={activeOrganization} units={units} members={members} models={models} modelPolicy={modelPolicy} permissions={me?.active_context?.permissions || []} busy={busy} onCreateUnit={onCreateUnit} onInvite={onInvite} onUpdateModelPolicy={onUpdateModelPolicy} embedded />}
      <AccountClosure copy={copy} item={closure} busy={busy} canRequest={access.closureRequest} onRequest={onRequestClosure} embedded />
      {identityDialog && (
        <IdentityDialog
          copy={copy}
          organizations={organizations}
          activeOrganizationId={activeOrganizationId}
          busy={busy}
          onClose={() => setIdentityDialog(false)}
          onConfirm={async (value) => {
            if (await onSwitchContext(value)) setIdentityDialog(false);
          }}
        />
      )}
      {profileDialog && <ProfileChangeDialog copy={copy} field={profileDialog} me={me} client={client} reauthenticated={requestedProfile === profileDialog} onClose={closeProfileDialog} onError={onProfileError} onChanged={onProfileChanged} onRecovering={onProfileRecovering} />}
    </section>
  );
}

function Dialog({ title, closeLabel, onClose, children, className = '' }) {
  useEffect(() => {
    const closeOnEscape = (event) => {
      if (event.key === 'Escape') onClose();
    };
    globalThis.addEventListener?.('keydown', closeOnEscape);
    return () => globalThis.removeEventListener?.('keydown', closeOnEscape);
  }, [onClose]);
  return (
    <div
      className="ap-dialog-backdrop"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <section className={`ap-dialog ${className}`} role="dialog" aria-modal="true" aria-labelledby="ap-dialog-title">
        <header>
          <h3 id="ap-dialog-title">{title}</h3>
          <button type="button" className="ap-icon-button" onClick={onClose} aria-label={closeLabel}>
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M6 6l12 12M18 6 6 18" />
            </svg>
          </button>
        </header>
        {children}
      </section>
    </div>
  );
}

function maskMobile(value) {
  const digits = String(value || '')
    .replace(/\D/g, '')
    .replace(/^86(?=1\d{10}$)/, '');
  return /^1\d{10}$/.test(digits) ? `${digits.slice(0, 3)}****${digits.slice(-4)}` : '—';
}

function AccountProfileCard({ copy, me, organizations, activeOrganization, canSwitchIdentity, canEditProfile, onSwitchIdentity, onEditUsername, onEditMobile }) {
  const identity = activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal;
  const accountStatus = copy.accountStatuses?.[me?.status] || me?.status || '—';
  return (
    <article className="ap-profile-card">
      <dl className="ap-profile-details">
        <div className="ap-profile-detail-row ap-profile-identity-row">
          <dt>{copy.activeContext}</dt>
          <dd>
            <span className="ap-profile-identity">
              <span className="ap-profile-value">{identity}</span>
              <span className={`ap-account-state ap-account-state-${String(me?.status || '').toLowerCase()}`} aria-label={`${copy.accountStatus}: ${accountStatus}`}>
                {accountStatus}
              </span>
            </span>
            <button type="button" disabled={!canSwitchIdentity} title={!canSwitchIdentity ? copy.permissionDenied : undefined} onClick={onSwitchIdentity}>
              {copy.switchContext}
            </button>
          </dd>
        </div>
        <div className="ap-profile-detail-row">
          <dt>{copy.currentUsername}</dt>
          <dd>
            <span className="ap-profile-value">{me?.username || '—'}</span>
            <button type="button" disabled={!canEditProfile} title={!canEditProfile ? copy.permissionDenied : undefined} onClick={onEditUsername}>
              {copy.modify}
            </button>
          </dd>
        </div>
        <div className="ap-profile-detail-row">
          <dt>{copy.currentMobile}</dt>
          <dd>
            <span className="ap-profile-value">{maskMobile(me?.mobile)}</span>
            <button type="button" disabled={!canEditProfile} title={!canEditProfile ? copy.permissionDenied : undefined} onClick={onEditMobile}>
              {copy.modify}
            </button>
          </dd>
        </div>
        <div className="ap-profile-detail-row">
          <dt>{copy.userCode}</dt>
          <dd>{me?.user_code || '—'}</dd>
        </div>
        <div className="ap-profile-detail-row">
          <dt>{copy.organizations}</dt>
          <dd>{organizations.length}</dd>
        </div>
      </dl>
    </article>
  );
}

function IdentityDialog({ copy, organizations, activeOrganizationId, busy, onClose, onConfirm }) {
  const [selected, setSelected] = useState(activeOrganizationId || '');
  return (
    <Dialog title={copy.identitySwitcherTitle} closeLabel={copy.closeDialog} onClose={onClose} className="ap-identity-dialog">
      <p className="ap-dialog-lead">{copy.identitySwitcherLead}</p>
      <div className="ap-identity-options">
        <button type="button" className={selected === '' ? 'selected' : ''} onClick={() => setSelected('')}>
          <span className="ap-radio-dot" />
          <span>
            <strong>{copy.personal}</strong>
            <small>{copy.personalIdentityLead}</small>
          </span>
        </button>
        {organizations.length ? (
          organizations.map((item) => (
            <button type="button" key={item.organization_id} className={selected === item.organization_id ? 'selected' : ''} onClick={() => setSelected(item.organization_id)}>
              <span className="ap-radio-dot" />
              <span>
                <strong>{copy.organizationIdentity(item.name)}</strong>
                <small>{copy.organizationIdentityLead}</small>
              </span>
            </button>
          ))
        ) : (
          <button type="button" disabled>
            <span className="ap-radio-dot" />
            <span>
              <strong>{copy.organizationIdentityUnavailable}</strong>
              <small>{copy.noOrganizationIdentity}</small>
            </span>
          </button>
        )}
      </div>
      <footer className="ap-dialog-actions">
        <button type="button" onClick={onClose}>
          {copy.cancel}
        </button>
        <button type="button" className="ap-dialog-primary" disabled={busy} onClick={() => onConfirm(selected)}>
          {copy.confirmIdentity}
        </button>
      </footer>
    </Dialog>
  );
}

function ProfileChangeDialog({ copy, field, me, client, reauthenticated, onClose, onError, onChanged, onRecovering }) {
  const [username, setUsername] = useState(me?.username || '');
  const [mobile, setMobile] = useState('');
  const [requestId, setRequestId] = useState('');
  const [smsCode, setSmsCode] = useState('');
  const [captcha, setCaptcha] = useState(null);
  const [captchaOffset, setCaptchaOffset] = useState(0);
  const [challengeSent, setChallengeSent] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const usernameValid = /^[a-z][a-z0-9._-]{2,31}$/i.test(username.trim());
  const mobileValid = /^(?:\+?86)?1[3-9]\d{9}$/.test(mobile.trim());
  const title = field === 'username' ? copy.changeUsername : copy.changeMobile;
  const perform = async (action) => {
    setBusy(true);
    setError('');
    try {
      await action();
    } catch (reason) {
      setError(onError(reason) || copy.requestFailed);
    } finally {
      setBusy(false);
    }
  };
  const finish = (result) => (result?.status === 'COMPLETED' ? onChanged(result) : onRecovering());
  const reauthenticate = () => client.reauthenticate(`/account?section=accountSettings&profile=${field}`);
  const submitUsername = (event) => {
    event.preventDefault();
    void perform(async () => {
      const created = await client.createProfileChange('USERNAME', username.trim().toLowerCase());
      await finish(await client.completeProfileChange(created.profile_change_request_id));
    });
  };
  const openMobileCaptcha = () =>
    void perform(async () => {
      let id = requestId;
      if (!id) {
        const created = await client.createProfileChange('MOBILE', mobile.trim());
        id = created.profile_change_request_id;
        setRequestId(id);
      }
      setCaptcha(await client.createProfileSliderCaptcha(mobile.trim()));
      setCaptchaOffset(0);
    });
  const sendMobileCode = () =>
    void perform(async () => {
      await client.sendProfileMobileChallenge(requestId, captcha.captcha_id, captchaOffset);
      setCaptcha(null);
      setCaptchaOffset(0);
      setChallengeSent(true);
    });
  const completeMobile = (event) => {
    event.preventDefault();
    void perform(async () => {
      await finish(await client.completeProfileChange(requestId, smsCode.trim()));
    });
  };
  return (
    <Dialog
      title={title}
      closeLabel={copy.closeDialog}
      onClose={() => {
        if (!busy) onClose();
      }}
      className="ap-profile-dialog"
    >
      {error && (
        <div role="alert" className="ap-alert error">
          {error}
        </div>
      )}
      {!reauthenticated ? (
        <>
          <p className="ap-dialog-lead">{copy.reauthenticateLead}</p>
          <div className="ap-security-note">
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d="M12 3 5 6v5c0 4.6 2.8 8.7 7 10 4.2-1.3 7-5.4 7-10V6l-7-3Z" />
              <path d="m9 12 2 2 4-4" />
            </svg>
            <span>{copy.portalNeverReceivesPassword}</span>
          </div>
          <footer className="ap-dialog-actions">
            <button type="button" onClick={onClose}>
              {copy.cancel}
            </button>
            <button type="button" className="ap-dialog-primary" onClick={reauthenticate}>
              {copy.verifyAndContinue}
            </button>
          </footer>
        </>
      ) : field === 'username' ? (
        <form onSubmit={submitUsername}>
          <Field label={copy.newUsername}>
            <input value={username} onChange={(event) => setUsername(event.target.value)} disabled={busy} autoComplete="username" placeholder={copy.newUsernamePlaceholder} />
          </Field>
          <p className={`ap-field-hint ${username && !usernameValid ? 'error' : ''}`}>{copy.usernameRules}</p>
          <footer className="ap-dialog-actions">
            <button type="button" onClick={onClose}>
              {copy.cancel}
            </button>
            <button className="ap-dialog-primary" disabled={busy || !usernameValid || username.trim().toLowerCase() === String(me?.username || '').toLowerCase()}>
              {copy.confirmChange}
            </button>
          </footer>
        </form>
      ) : (
        <form onSubmit={completeMobile}>
          <Field label={copy.newMobile}>
            <input
              value={mobile}
              onChange={(event) => {
                setMobile(event.target.value);
                setRequestId('');
                setChallengeSent(false);
                setSmsCode('');
                setCaptcha(null);
              }}
              disabled={busy || challengeSent}
              inputMode="tel"
              autoComplete="tel-national"
              placeholder={copy.newMobilePlaceholder}
            />
          </Field>
          <p className={`ap-field-hint ${mobile && !mobileValid ? 'error' : ''}`}>{copy.mobileRules}</p>
          {challengeSent && (
            <>
              <Field label={copy.smsCode}>
                <div className="ap-code-field">
                  <input value={smsCode} onChange={(event) => setSmsCode(event.target.value.replace(/\D/g, '').slice(0, 6))} disabled={busy} inputMode="numeric" autoComplete="one-time-code" />
                  <button type="button" disabled>
                    {copy.codeSent}
                  </button>
                </div>
              </Field>
              <p className="ap-field-hint">{copy.smsValidity}</p>
            </>
          )}
          <footer className="ap-dialog-actions">
            <button type="button" onClick={onClose}>
              {copy.cancel}
            </button>
            {challengeSent ? (
              <button className="ap-dialog-primary" disabled={busy || smsCode.length !== 6}>
                {copy.confirmChange}
              </button>
            ) : (
              <button type="button" className="ap-dialog-primary" disabled={busy || !mobileValid || maskMobile(mobile) === maskMobile(me?.mobile)} onClick={openMobileCaptcha}>
                {copy.sendCode}
              </button>
            )}
          </footer>
        </form>
      )}
      {captcha && (
        <div className="ap-captcha-backdrop" role="dialog" aria-modal="true" aria-label={copy.captchaTitle}>
          <ProfileCaptcha
            challenge={captcha}
            offset={captchaOffset}
            busy={busy}
            copy={copy}
            onOffset={setCaptchaOffset}
            onConfirm={sendMobileCode}
            onRefresh={openMobileCaptcha}
            onClose={() => {
              if (!busy) setCaptcha(null);
            }}
          />
        </div>
      )}
    </Dialog>
  );
}

function ProfileCaptcha({ challenge, offset, busy, copy, onOffset, onConfirm, onRefresh, onClose }) {
  const width = Number(challenge?.width || 280);
  const height = Number(challenge?.height || 120);
  const pieceWidth = Number(challenge?.piece_width || 44);
  const pieceHeight = Number(challenge?.piece_height || 44);
  const maximum = Math.max(1, width - pieceWidth);
  return (
    <div className="ap-captcha">
      <header>
        <strong>{copy.captchaTitle}</strong>
        <div>
          <button type="button" disabled={busy} onClick={onRefresh}>
            {copy.captchaRefresh}
          </button>
          <button type="button" disabled={busy} onClick={onClose} aria-label={copy.captchaClose}>
            ×
          </button>
        </div>
      </header>
      <div className="ap-captcha-image" style={{ aspectRatio: `${width}/${height}` }}>
        <img src={challenge.background_image} alt="" draggable="false" />
        <img
          src={challenge.piece_image}
          alt=""
          draggable="false"
          className="ap-captcha-piece"
          style={{
            left: `${(Number(offset || 0) / width) * 100}%`,
            top: `${(Number(challenge.piece_y || 0) / height) * 100}%`,
            width: `${(pieceWidth / width) * 100}%`,
            height: `${(pieceHeight / height) * 100}%`,
          }}
        />
      </div>
      <input type="range" min="0" max={maximum} step="1" value={offset} disabled={busy} onChange={(event) => onOffset(Number(event.target.value))} aria-label={copy.captchaSlider} />
      <button type="button" className="ap-primary" disabled={busy} onClick={onConfirm}>
        {busy ? copy.sendingCode : copy.captchaConfirm}
      </button>
    </div>
  );
}

function ProfileSettings({ copy, client, me, onError, onChanged, onRecovering, embedded = false }) {
  const [username, setUsername] = useState(me?.username || '');
  const [mobile, setMobile] = useState(me?.mobile || '');
  const [requestId, setRequestId] = useState('');
  const [smsCode, setSmsCode] = useState('');
  const [captcha, setCaptcha] = useState(null);
  const [captchaOffset, setCaptchaOffset] = useState(0);
  const [challengeSent, setChallengeSent] = useState(false);
  const [busy, setBusy] = useState(false);

  const perform = async (action) => {
    setBusy(true);
    try {
      await action();
    } catch (reason) {
      onError(reason);
    } finally {
      setBusy(false);
    }
  };
  const finish = (result) => (result?.status === 'COMPLETED' ? onChanged(result) : onRecovering());
  const changeUsername = (event) => {
    event.preventDefault();
    void perform(async () => {
      const created = await client.createProfileChange('USERNAME', username.trim());
      await finish(await client.completeProfileChange(created.profile_change_request_id));
    });
  };
  const openMobileCaptcha = () =>
    void perform(async () => {
      let id = requestId;
      if (!id) {
        const created = await client.createProfileChange('MOBILE', mobile.trim());
        id = created.profile_change_request_id;
        setRequestId(id);
      }
      setCaptcha(await client.createProfileSliderCaptcha(mobile.trim()));
      setCaptchaOffset(0);
    });
  const sendMobileCode = () =>
    void perform(async () => {
      await client.sendProfileMobileChallenge(requestId, captcha.captcha_id, captchaOffset);
      setCaptcha(null);
      setCaptchaOffset(0);
      setChallengeSent(true);
    });
  const completeMobile = (event) => {
    event.preventDefault();
    void perform(async () => {
      await finish(await client.completeProfileChange(requestId, smsCode.trim()));
    });
  };
  const reauthenticate = () => client.reauthenticate('/account?section=accountSettings');

  const Heading = embedded ? 'h3' : 'h2';
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.profile}</p>}
      <Heading>{copy.profileSettings}</Heading>
      <p className="ap-lead">{copy.profileSettingsLead}</p>
      <article className="ap-card ap-profile-reauth">
        <div>
          <h3>{copy.reauthenticateTitle}</h3>
          <p className="ap-muted">{copy.reauthenticateLead}</p>
        </div>
        <button type="button" className="ap-primary" onClick={reauthenticate}>
          {copy.reauthenticate}
        </button>
      </article>
      <div className="ap-grid">
        <article className="ap-card">
          <h3>{copy.changeUsername}</h3>
          <p className="ap-muted">
            {copy.currentUsername}: {me?.username || '—'}
          </p>
          <form className="ap-stack" onSubmit={changeUsername}>
            <Field label={copy.newUsername}>
              <input value={username} onChange={(event) => setUsername(event.target.value)} disabled={busy} autoComplete="username" />
            </Field>
            <button disabled={busy || !username.trim() || username.trim() === (me?.username || '')}>{copy.confirmChange}</button>
          </form>
        </article>
        <article className="ap-card">
          <h3>{copy.changeMobile}</h3>
          <p className="ap-muted">
            {copy.currentMobile}: {me?.mobile || '—'}
          </p>
          <form className="ap-stack" onSubmit={completeMobile}>
            <Field label={copy.newMobile}>
              <input
                value={mobile}
                onChange={(event) => {
                  setMobile(event.target.value);
                  setRequestId('');
                  setChallengeSent(false);
                  setSmsCode('');
                  setCaptcha(null);
                }}
                disabled={busy || challengeSent}
                inputMode="tel"
                autoComplete="tel-national"
              />
            </Field>
            {challengeSent && (
              <Field label={copy.smsCode}>
                <input value={smsCode} onChange={(event) => setSmsCode(event.target.value)} disabled={busy} inputMode="numeric" autoComplete="one-time-code" />
              </Field>
            )}
            {challengeSent ? (
              <button disabled={busy || !smsCode.trim()}>{copy.confirmChange}</button>
            ) : (
              <button type="button" disabled={busy || !mobile.trim() || mobile.trim() === (me?.mobile || '')} onClick={openMobileCaptcha}>
                {copy.sendCode}
              </button>
            )}
          </form>
        </article>
      </div>
      {captcha && (
        <div className="ap-captcha-backdrop" role="dialog" aria-modal="true" aria-label={copy.captchaTitle}>
          <ProfileCaptcha
            challenge={captcha}
            offset={captchaOffset}
            busy={busy}
            copy={copy}
            onOffset={setCaptchaOffset}
            onConfirm={sendMobileCode}
            onRefresh={openMobileCaptcha}
            onClose={() => {
              if (!busy) setCaptcha(null);
            }}
          />
        </div>
      )}
    </section>
  );
}

function AccountClosure({ copy, item, busy, canRequest = true, onRequest, embedded = false }) {
  const Heading = embedded ? 'h3' : 'h2';
  return (
    <section className={embedded ? 'ap-combined-section ap-danger-section' : 'ap-danger-section'}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.closure}</p>}
      <Heading>{copy.closure.title}</Heading>
      <p className="ap-lead">{copy.closure.lead}</p>
      <article className="ap-card ap-danger-card">
        {item ? (
          <>
            <div className="ap-inline">
              <span className="ap-status">{item.status}</span>
              <strong>{item.stage}</strong>
            </div>
            <p>
              {copy.closure.sla}: {new Date(item.sla_expires_at).toLocaleString()}
            </p>
            <p className="ap-muted">
              {copy.closure.updated}: {new Date(item.updated_at).toLocaleString()}
            </p>
            {item.blockers?.length ? (
              <ul className="ap-list">
                {item.blockers.map((blocker) => (
                  <li key={`${blocker.domain}:${blocker.code}`}>
                    <span>{copy.closure.blockerCodes[blocker.code] || blocker.code}</span>
                    <small>{copy.closure.actionCodes[blocker.user_action] || blocker.user_action || blocker.domain}</small>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="ap-muted">{copy.closure.noBlockers}</p>
            )}
          </>
        ) : (
          <>
            <h3>{copy.closure.beforeTitle}</h3>
            <p className="ap-muted">{copy.closure.before}</p>
            <button className="ap-primary" disabled={busy || !canRequest} title={!canRequest ? copy.permissionDenied : undefined} onClick={onRequest}>
              {copy.closure.start}
            </button>
          </>
        )}
      </article>
    </section>
  );
}

function Overview({ copy, language, me, organizations, balance, activeOrganization, activeOrganizationId, busy, canSwitch = true, onSwitchContext, embedded = false }) {
  const Heading = embedded ? 'h3' : 'h2';
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.overview}</p>}
      <Heading>{copy.welcome(me?.display_name || copy.user)}</Heading>
      <p className="ap-lead">{copy.overviewLead}</p>
      <div className="ap-context-card">
        <div>
          <span>{copy.activeContext}</span>
          <strong>{activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal}</strong>
          <small>{copy.identityHelp}</small>
        </div>
        <label>
          <span>{copy.switchContext}</span>
          <select value={activeOrganizationId} onChange={(event) => onSwitchContext(event.target.value)} disabled={busy || !canSwitch} title={!canSwitch ? copy.permissionDenied : undefined}>
            <option value="">{copy.personal}</option>
            {organizations.map((item) => (
              <option key={item.organization_id} value={item.organization_id}>
                {copy.organizationIdentity(item.name)}
              </option>
            ))}
          </select>
        </label>
      </div>
      <div className="ap-metrics">
        <article>
          <span>{copy.userCode}</span>
          <strong>{me?.user_code || '—'}</strong>
        </article>
        <article>
          <span>{copy.organizations}</span>
          <strong>{organizations.length}</strong>
        </article>
        <article>
          <span>{copy.availableCredits}</span>
          <strong>{formatPoints(balance?.available_microcredits, language)}</strong>
        </article>
        <article>
          <span>{copy.accountStatus}</span>
          <strong>{me?.status || '—'}</strong>
        </article>
      </div>
    </section>
  );
}

function Organization({ copy, organization, units, members, models, modelPolicy, permissions, busy, onCreateUnit, onInvite, onUpdateModelPolicy, embedded = false }) {
  const [unitName, setUnitName] = useState('');
  const [parentId, setParentId] = useState('');
  const [email, setEmail] = useState('');
  const permissionSet = new Set(permissions || []);
  const canManageUnits = permissionSet.has('org.units.manage');
  const canManageMembers = permissionSet.has('org.members.manage');
  if (!organization) return <Empty>{copy.chooseOrganization}</Empty>;
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.organization}</p>}
      {embedded ? (
        <h3>
          {copy.organizationSettings}: {organization.name}
        </h3>
      ) : (
        <h2>{organization.name}</h2>
      )}
      <div className="ap-grid">
        <article className="ap-card">
          <h3>{copy.organizationTree}</h3>
          {units.length ? (
            <ul className="ap-list">
              {units.map((unit) => (
                <li key={unit.unit_id}>
                  <span>{unit.name}</span>
                  <small>{unit.parent_id ? copy.childUnit : copy.rootUnit}</small>
                </li>
              ))}
            </ul>
          ) : (
            <Empty>{copy.noUnits}</Empty>
          )}
          {canManageUnits && (
            <form
              className="ap-stack"
              onSubmit={(event) => {
                event.preventDefault();
                if (unitName.trim()) onCreateUnit(unitName.trim(), parentId || null).then?.(() => setUnitName(''));
              }}
            >
              <Field label={copy.unitName}>
                <input value={unitName} onChange={(event) => setUnitName(event.target.value)} />
              </Field>
              <Field label={copy.parentUnit}>
                <select value={parentId} onChange={(event) => setParentId(event.target.value)}>
                  <option value="">{copy.rootUnit}</option>
                  {units.map((unit) => (
                    <option key={unit.unit_id} value={unit.unit_id}>
                      {unit.name}
                    </option>
                  ))}
                </select>
              </Field>
              <button disabled={busy || !unitName.trim()}>{copy.createUnit}</button>
            </form>
          )}
        </article>
        <article className="ap-card">
          <h3>{copy.members}</h3>
          {members.length ? (
            <ul className="ap-list">
              {members.map((member) => (
                <li key={member.user_id}>
                  <span>{member.display_name || member.email || member.user_id}</span>
                  <small>{member.roles.join(' · ')}</small>
                </li>
              ))}
            </ul>
          ) : (
            <Empty>{copy.noMembers}</Empty>
          )}
          {canManageMembers && (
            <form
              className="ap-stack"
              onSubmit={(event) => {
                event.preventDefault();
                if (email.trim()) onInvite(email.trim(), ['MEMBER']).then?.(() => setEmail(''));
              }}
            >
              <Field label={copy.memberEmail}>
                <input type="email" value={email} onChange={(event) => setEmail(event.target.value)} />
              </Field>
              <button disabled={busy || !email.trim()}>{copy.createInvitation}</button>
            </form>
          )}
        </article>
      </div>
      {modelPolicy && <ModelPolicy copy={copy} policy={modelPolicy} models={models} canManage={(permissions || []).includes('org.models.manage')} busy={busy} onSave={onUpdateModelPolicy} />}
    </section>
  );
}

function ModelPolicy({ copy, policy, models, canManage, busy, onSave }) {
  const [mode, setMode] = useState(policy.mode);
  const [selected, setSelected] = useState(policy.model_ids || []);
  useEffect(() => {
    setMode(policy.mode);
    setSelected(policy.model_ids || []);
  }, [policy.version, policy.mode]);
  const toggle = (id) => setSelected((current) => (current.includes(id) ? current.filter((value) => value !== id) : [...current, id]));
  const save = () => {
    const reason = window.prompt(copy.modelPolicyReason);
    if (reason?.trim()) onSave(mode, mode === 'ALLOW_ALL' ? [] : selected, reason.trim());
  };
  return (
    <article className="ap-card">
      <h3>{copy.modelPolicy}</h3>
      <p className="ap-muted">{copy.modelPolicyLead}</p>
      <Field label={copy.modelPolicyMode}>
        <select value={mode} disabled={!canManage || busy} onChange={(event) => setMode(event.target.value)}>
          <option value="ALLOW_ALL">{copy.modelPolicyModes.ALLOW_ALL}</option>
          <option value="ALLOW_LIST">{copy.modelPolicyModes.ALLOW_LIST}</option>
          <option value="DENY_LIST">{copy.modelPolicyModes.DENY_LIST}</option>
        </select>
      </Field>
      {mode !== 'ALLOW_ALL' && (
        <div className="ap-stack">
          {models.map((model) => (
            <label key={model.model_id} className="ap-inline">
              <input type="checkbox" checked={selected.includes(model.model_id)} disabled={!canManage || busy} onChange={() => toggle(model.model_id)} />
              <span>{model.display_name}</span>
              <small>{model.eligibility_reason}</small>
            </label>
          ))}
        </div>
      )}
      {canManage && (
        <button disabled={busy} onClick={save}>
          {copy.saveModelPolicy}
        </button>
      )}
    </article>
  );
}

function OrganizationEntitlements({ copy, language, permissions, products, packages, members, grants, order, busy, onPurchase, onAllocate, onRevoke, onReclaim }) {
  const permissionSet = new Set(permissions || []);
  const canPurchase = permissionSet.has('org.entitlements.purchase');
  const canAllocate = permissionSet.has('org.quota.allocate');
  const canRevoke = permissionSet.has('org.quota.revoke');
  const canReclaimClosed = permissionSet.has('org.grants.reclaim_closed');
  const [productId, setProductId] = useState('');
  const [channel, setChannel] = useState('');
  const [memberId, setMemberId] = useState('');
  const [packageId, setPackageId] = useState('');
  const [componentId, setComponentId] = useState('');
  const [amount, setAmount] = useState('');
  useEffect(() => {
    if (!products.some((item) => item.product_id === productId && item.availability_status === 'AVAILABLE')) {
      setProductId(products.find((item) => item.availability_status === 'AVAILABLE')?.product_id || '');
    }
  }, [productId, products]);
  const selectedProduct = products.find((item) => item.product_id === productId && item.availability_status === 'AVAILABLE');
  const availableChannels = selectedProduct?.channels || [];
  useEffect(() => {
    if (!availableChannels.includes(channel)) setChannel(availableChannels[0] || '');
  }, [channel, productId, availableChannels.join('|')]);
  useEffect(() => {
    if (!memberId && members[0]) setMemberId(members[0].user_id);
  }, [memberId, members]);
  useEffect(() => {
    if (!packageId && packages.items?.[0]) setPackageId(packages.items[0].package_id);
  }, [packageId, packages.items]);
  const selectedPackage = packages.items?.find((item) => item.package_id === packageId);
  const allocatableComponents = (selectedPackage?.components || []).filter((item) => item.status === 'ACTIVE' && Number(item.available_microcredits) > 0);
  const effectiveComponentId = allocatableComponents.some((item) => item.component_id === componentId) ? componentId : allocatableComponents[0]?.component_id || '';
  const selectedComponent = allocatableComponents.find((item) => item.component_id === effectiveComponentId);
  const selectedMember = members.find((item) => item.user_id === memberId);
  const amountMicrocredits = Math.round(Number(amount) * MICROCREDITS_PER_POINT);
  return (
    <section className="ap-stack">
      {canPurchase && (
        <article className="ap-card">
          <h3>{copy.organizationPurchase}</h3>
          <p className="ap-muted">{copy.organizationPurchaseLead}</p>
          <div className="ap-inline">
            <select value={productId} onChange={(event) => setProductId(event.target.value)}>
              {products.map((product) => (
                <option key={product.product_id} value={product.product_id} disabled={product.availability_status !== 'AVAILABLE'}>
                  {product.plan_name} · {formatMoney(product.amount_fen)}
                  {product.availability_status !== 'AVAILABLE' ? ` · ${copy.paymentChannelMaintenance}` : ''}
                </option>
              ))}
            </select>
            <select value={channel} onChange={(event) => setChannel(event.target.value)}>
              {availableChannels.map((item) => (
                <option key={item} value={item}>
                  {copy.channels[item] || item}
                </option>
              ))}
            </select>
            <button className="ap-primary" disabled={busy || !selectedProduct || !channel} onClick={() => onPurchase(productId, channel)}>
              {copy.purchaseForOrganization}
            </button>
          </div>
          {order?.beneficiary_organization_id && (
            <p className="ap-muted">
              {copy.organizationOrderCreated}: {order.merchant_order_no}
            </p>
          )}
        </article>
      )}
      {canAllocate && (
        <article className="ap-card">
          <h3>{copy.allocateMemberQuota}</h3>
          <p className="ap-muted">{copy.allocateMemberQuotaLead}</p>
          <form
            className="ap-stack"
            onSubmit={(event) => {
              event.preventDefault();
              if (selectedMember && selectedComponent && amountMicrocredits > 0)
                onAllocate([
                  {
                    member_user_id: selectedMember.user_id,
                    membership_version: selectedMember.version,
                    package_id: packageId,
                    component_id: effectiveComponentId,
                    component_type: selectedComponent.component_type,
                    amount_microcredits: amountMicrocredits,
                  },
                ]).then?.(() => setAmount(''));
            }}
          >
            <Field label={copy.member}>
              <select value={memberId} onChange={(event) => setMemberId(event.target.value)}>
                {members.map((member) => (
                  <option key={member.user_id} value={member.user_id}>
                    {member.display_name || member.email || member.user_id}
                  </option>
                ))}
              </select>
            </Field>
            <Field label={copy.entitlementPackage}>
              <select value={packageId} onChange={(event) => setPackageId(event.target.value)}>
                {(packages.items || []).map((item) => (
                  <option key={item.package_id} value={item.package_id}>
                    {item.plan_name} · {String(item.package_id).slice(0, 8)}
                  </option>
                ))}
              </select>
            </Field>
            <Field label={copy.entitlementComponent}>
              <select value={effectiveComponentId} onChange={(event) => setComponentId(event.target.value)}>
                {allocatableComponents.map((component) => (
                  <option key={component.component_id} value={component.component_id}>
                    {copy.componentTypes[component.component_type] || component.component_type} · {formatPoints(component.available_microcredits, language)}
                  </option>
                ))}
              </select>
            </Field>
            <Field label={copy.quotaAmount}>
              <input type="number" min="0.0001" step="0.0001" value={amount} onChange={(event) => setAmount(event.target.value)} />
            </Field>
            <button disabled={busy || !selectedMember || !selectedComponent || amountMicrocredits <= 0 || amountMicrocredits > Number(selectedComponent?.available_microcredits || 0)}>{copy.allocateQuota}</button>
          </form>
        </article>
      )}
      <article className="ap-card">
        <h3>{copy.memberQuotaGrants}</h3>
        {grants.length ? (
          <div className="ap-order-list">
            {grants.map((grant) => (
              <article className="ap-trace-card" key={grant.grant_id}>
                <header>
                  <div>
                    <strong>{members.find((member) => member.user_id === grant.member_user_id)?.display_name || grant.member_user_id}</strong>
                    <small>
                      {copy.componentTypes[grant.component_type] || grant.component_type} · {String(grant.package_id).slice(0, 8)}
                    </small>
                  </div>
                  <div>
                    <strong>
                      {formatPoints(grant.available_microcredits, language)} {copy.points}
                    </strong>
                    <span className="ap-status">{copy.grantStatuses?.[grant.status] || grant.status}</span>
                  </div>
                </header>
                {canRevoke && (grant.status === 'ACTIVE' || (grant.status === 'SUSPENDED' && grant.suspension_reason !== 'ACCOUNT_CLOSED')) && Number(grant.reserved_microcredits) === 0 && Number(grant.available_microcredits) > 0 && (
                  <button disabled={busy} onClick={() => onRevoke(grant.grant_id, 'ADMIN_REVOKE')}>
                    {copy.revokeQuota}
                  </button>
                )}
                {canReclaimClosed && grant.status === 'SUSPENDED' && grant.suspension_reason === 'ACCOUNT_CLOSED' && Number(grant.reserved_microcredits) === 0 && Number(grant.available_microcredits) > 0 && (
                  <button disabled={busy} onClick={() => onReclaim(grant.grant_id, 'ACCOUNT_CLOSED_FULL_RECLAIM')}>
                    {copy.reclaimClosedQuota}
                  </button>
                )}
              </article>
            ))}
          </div>
        ) : (
          <Empty>{copy.noMemberQuotaGrants}</Empty>
        )}
      </article>
    </section>
  );
}

function Wallet({ copy, language, products, balance, packages, orders = [], billingUnavailable, detailsUnavailable, order, qrData, busy, canRecharge = true, onRecharge, onActivateRecharge, embedded = false }) {
  const [productId, setProductIdState] = useState('');
  const [channel, setChannel] = useState('');
  const [entitlementDialog, setEntitlementDialog] = useState(null);
  const [paymentDialogOpen, setPaymentDialogOpen] = useState(false);
  const setProductId = (nextProductId) => {
    if (!nextProductId || products.some((item) => item.product_id === nextProductId && item.availability_status === 'AVAILABLE')) setProductIdState(nextProductId);
  };
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!products.some((item) => item.product_id === productId && item.availability_status === 'AVAILABLE')) setProductId(products.find((item) => item.availability_status === 'AVAILABLE')?.product_id || '');
  }, [products, productId]);
  const selected = products.find((item) => item.product_id === productId);
  const selectedAvailable = selected?.availability_status === 'AVAILABLE';
  const allPaused = products.length > 0 && !products.some((item) => item.availability_status === 'AVAILABLE');
  const availableChannels = selectedAvailable ? selected?.channels || [] : [];
  if (allPaused) {
    billingUnavailable = true;
    copy = { ...copy, billingUnavailable: copy.paymentChannelMaintenance };
  }
  products = products.map((product) =>
    product.availability_status === 'AVAILABLE'
      ? product
      : {
          ...product,
          plan_name: `${product.plan_name} · ${copy.paymentChannelMaintenance}`,
        },
  );
  useEffect(() => {
    if (!availableChannels.includes(channel)) setChannel(availableChannels[0] || '');
  }, [channel, productId, availableChannels.join('|')]);
  useEffect(() => {
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  const pendingOrders = payablePaymentOrders(orders, now);
  const activePayment = isPayablePaymentOrder(order, now);
  const remainingSeconds = paymentOrderRemainingSeconds(order, now);
  useEffect(() => {
    if (paymentDialogOpen && !activePayment) setPaymentDialogOpen(false);
  }, [paymentDialogOpen, activePayment]);
  const createAndOpenPayment = async () => {
    if (await onRecharge(productId, channel)) setPaymentDialogOpen(true);
  };
  const activateAndOpenPayment = async (orderId) => {
    if (await onActivateRecharge(orderId)) setPaymentDialogOpen(true);
  };
  const activePackages = activeEntitlementPackages(packages.items || [], now);
  const summarizedPackages = activePackages.slice(0, 5);
  const allPackages = allEntitlementPackages(packages.items || []);
  const remainingMicrocredits = remainingEntitlementMicrocredits(activePackages);
  const Heading = embedded ? 'h3' : 'h2';
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.wallet}</p>}
      <Heading>{copy.walletTitle}</Heading>
      <p className="ap-lead">{copy.walletLead}</p>
      {billingUnavailable && (
        <div role="alert" className="ap-alert error">
          {copy.billingUnavailable}
        </div>
      )}
      <div className="ap-balance-hero">
        <span>{copy.availableCredits}</span>
        <strong>{formatWholePoints(balance?.available_microcredits, language)}</strong>
        <small>
          {copy.basePoints}: {formatPoints(packages.summary?.base_available_microcredits, language)} · {copy.bonusPoints}: {formatPoints(packages.summary?.bonus_available_microcredits, language)} · {copy.reservedCredits}: {formatPoints(balance?.reserved_microcredits, language)}
        </small>
      </div>
      <div className="ap-card">
        <h3>{copy.availablePlans}</h3>
        <div className="ap-product-grid">
          {products.map((product) => (
            <button key={product.product_id} className={productId === product.product_id ? 'selected' : ''} onClick={() => setProductId(product.product_id)}>
              <strong>{product.plan_name}</strong>
              <span>
                ¥{(product.amount_fen / 100).toFixed(2)} · {product.display_points.toLocaleString()} {copy.points}
              </span>
              <small>
                {copy.validityDays(product.validity_days)}
                {product.price_status !== 'FROZEN' ? ` · ${copy.tentativePrice}` : ''}
              </small>
            </button>
          ))}
        </div>
        <div className="ap-inline">
          <select value={channel} disabled={!canRecharge} title={!canRecharge ? copy.permissionDenied : undefined} onChange={(event) => setChannel(event.target.value)}>
            {availableChannels.map((item) => (
              <option key={item} value={item}>
                {copy.channels[item] || item}
              </option>
            ))}
          </select>
          <button className="ap-primary" disabled={busy || !canRecharge || billingUnavailable || !selected || !channel} title={!canRecharge ? copy.permissionDenied : undefined} onClick={createAndOpenPayment}>
            {copy.createRecharge}
          </button>
        </div>
        {pendingOrders.length > 0 && <PendingPaymentOrders copy={copy} orders={pendingOrders} now={now} busy={busy} onActivate={activateAndOpenPayment} />}
      </div>
      {paymentDialogOpen && activePayment && <PaymentQRCodeDialog copy={copy} order={order} qrData={qrData} remainingSeconds={remainingSeconds} onClose={() => setPaymentDialogOpen(false)} />}
      <section className="ap-entitlements">
        <h3>{copy.myEntitlements}</h3>
        {detailsUnavailable ? (
          <div role="alert" className="ap-alert error">
            {copy.billingUnavailable}
          </div>
        ) : allPackages.length ? (
          <div className="ap-entitlement-summary">
            {summarizedPackages.length ? (
              <div className="ap-entitlement-summary-list">
                {summarizedPackages.map((item) => (
                  <EntitlementPackageRow key={item.package_id} copy={copy} language={language} item={item} onOpen={() => setEntitlementDialog({ mode: 'detail', item, returnToAll: false })} />
                ))}
              </div>
            ) : <Empty>{copy.noEntitlements}</Empty>}
            <footer>
              <strong>{copy.entitlementSummary(activePackages.length, formatWholePoints(remainingMicrocredits, language))}</strong>
              <button type="button" onClick={() => setEntitlementDialog({ mode: 'all' })}>{copy.viewAll}</button>
            </footer>
          </div>
        ) : (
          <Empty>{copy.noEntitlements}</Empty>
        )}
      </section>
      {entitlementDialog && (
        <Dialog title={entitlementDialog.mode === 'all' ? copy.allEntitlements : copy.entitlementDetails} closeLabel={copy.closeDialog} onClose={() => setEntitlementDialog(null)} className="ap-entitlement-dialog">
          {entitlementDialog.mode === 'all' ? (
            <div className="ap-entitlement-summary-list ap-entitlement-all-list">
              {allPackages.map((item) => (
                <EntitlementPackageRow key={item.package_id} copy={copy} language={language} item={item} onOpen={() => setEntitlementDialog({ mode: 'detail', item, returnToAll: true })} />
              ))}
            </div>
          ) : (
            <>
              {entitlementDialog.returnToAll && <button type="button" className="ap-dialog-back" onClick={() => setEntitlementDialog({ mode: 'all' })}>{copy.backToEntitlements}</button>}
              <EntitlementPackageDetail copy={copy} language={language} item={entitlementDialog.item} />
            </>
          )}
        </Dialog>
      )}
    </section>
  );
}

function formatPaymentCountdown(seconds) {
  return `${String(Math.floor(seconds / 60)).padStart(2, '0')}:${String(seconds % 60).padStart(2, '0')}`;
}

function PendingPaymentOrders({ copy, orders, now, busy, onActivate }) {
  return (
    <section className="ap-pending-payments" aria-label={copy.pendingPayments}>
      <h4>{copy.pendingPayments}</h4>
      <div className="ap-pending-payment-list">
        {orders.map((item) => {
          const remaining = paymentOrderRemainingSeconds(item, now);
          return (
            <button type="button" key={item.order_id} disabled={busy} onClick={() => onActivate(item.order_id)} aria-label={copy.continuePaymentFor(item.plan_name)}>
              <span>
                <strong>{item.plan_name}</strong>
                <small>¥{(item.amount_fen / 100).toFixed(2)}</small>
              </span>
              <span>
                <span className="ap-status">{copy.orderStatuses[item.status] || item.status}</span>
                <small>{copy.countdown(formatPaymentCountdown(remaining))}</small>
              </span>
            </button>
          );
        })}
      </div>
    </section>
  );
}

function PaymentQRCodeDialog({ copy, order, qrData, remainingSeconds, onClose }) {
  return (
    <Dialog title={copy.createRecharge} closeLabel={copy.closeDialog} onClose={onClose} className="ap-payment-dialog">
      <div className="ap-payment-dialog-body">
        <div className="ap-payment-qr">
          {qrData ? (
            <img src={qrData} alt={copy.paymentQR} />
          ) : (
            <span className="ap-payment-qr-loading">
              <span className="ap-spinner" aria-hidden="true" />
              <span>{copy.qrGenerating}</span>
            </span>
          )}
        </div>
        <span className="ap-status">{copy.orderStatuses[order.status] || order.status}</span>
        <strong className="ap-payment-amount">¥{(order.amount_fen / 100).toFixed(2)}</strong>
        <p>{copy.scanToPay}</p>
        <small>{copy.countdown(formatPaymentCountdown(remainingSeconds))}</small>
      </div>
    </Dialog>
  );
}

function EntitlementPackageRow({ copy, language, item, onOpen }) {
  const progress = entitlementProgress(item);
  return (
    <button type="button" className="ap-entitlement-row" onClick={onOpen}>
      <span className="ap-entitlement-row-main">
        <strong>{item.plan_name}</strong>
        <small>{copy.usedOfTotal(formatPoints(item.points_used_microcredits, language), formatPoints(item.points_total_microcredits, language))}</small>
      </span>
      <span className="ap-entitlement-row-meta">
        <span className="ap-status">{copy.packageStatuses[item.status] || item.status}</span>
        <small>{copy.validUntil}: {formatUsageTime(item.valid_until, language)}</small>
      </span>
      <span className="ap-entitlement-progress" role="progressbar" aria-label={copy.entitlementProgress} aria-valuemin="0" aria-valuemax="100" aria-valuenow={Math.round(progress)}>
        <span style={{ width: `${progress}%` }} />
      </span>
    </button>
  );
}

function EntitlementPackageDetail({ copy, language, item }) {
  return (
    <div className="ap-entitlement-detail">
      <header>
        <div>
          <strong>{item.plan_name}</strong>
          <small>{item.sku_code || item.plan_id}</small>
        </div>
        <span className="ap-status">{copy.packageStatuses[item.status] || item.status}</span>
      </header>
      <TraceDetails
        items={[
          [copy.packageId, item.package_id],
          [copy.entitlementSource, item.source_reference],
          [copy.pinvouOrder, item.pinvou_order_id],
          [copy.providerOrderReference, item.provider_order_reference],
          [copy.amount, formatMoney(item.amount_fen)],
          [copy.creditedAt, formatUsageTime(item.credited_at, language)],
          [copy.validPeriod, `${formatUsageTime(item.valid_from, language)} — ${formatUsageTime(item.valid_until, language)}`],
          [copy.totalPoints, `${formatPoints(item.points_total_microcredits, language)} ${copy.points}`],
          [copy.usedPoints, `${formatPoints(item.points_used_microcredits, language)} ${copy.points}`],
          [copy.expiredPoints, `${formatPoints(item.points_expired_microcredits, language)} ${copy.points}`],
          [copy.revokedPoints, `${formatPoints(item.points_revoked_microcredits, language)} ${copy.points}`],
        ]}
      />
      <p className="ap-muted">{copy.consumptionOrder}</p>
      <p className="ap-muted">{copy.refundCompositionNotice}</p>
      <div className="ap-entitlement-components">
        {item.components?.map((component) => (
          <article className="ap-card" key={component.component_id}>
            <strong>{copy.componentTypes[component.component_type] || component.component_type}</strong>
            <TraceDetails
              items={[
                [copy.totalPoints, `${formatPoints(component.original_microcredits, language)} ${copy.points}`],
                [copy.availableCredits, `${formatPoints(component.available_microcredits, language)} ${copy.points}`],
                [copy.reservedCredits, `${formatPoints(component.reserved_microcredits, language)} ${copy.points}`],
                [copy.usedPoints, `${formatPoints(component.used_microcredits, language)} ${copy.points}`],
                [copy.expiredPoints, `${formatPoints(component.expired_microcredits, language)} ${copy.points}`],
                [copy.revokedPoints, `${formatPoints(component.revoked_microcredits, language)} ${copy.points}`],
                [copy.validUntil, formatUsageTime(component.expires_at, language)],
                [copy.expiryReminder, copy.expiryReminderAt(formatUsageTime(component.expires_at, language))],
              ]}
            />
          </article>
        ))}
      </div>
    </div>
  );
}

function Orders({ copy, language, orders, unavailable, successfulOnly = false, embedded = false }) {
  const [selectedOrder, setSelectedOrder] = useState(null);
  const visibleOrders = successfulOnly ? summarizedPaymentOrders(orders) : orders;
  const Heading = embedded ? 'h3' : 'h2';
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.orders}</p>}
      <Heading>{copy.ordersTitle}</Heading>
      <p className="ap-lead">{successfulOnly ? copy.paidOrdersLead : copy.ordersLead}</p>
      {unavailable ? (
        <div role="alert" className="ap-alert error">
          {copy.billingUnavailable}
        </div>
      ) : visibleOrders.length && successfulOnly ? (
        <OrderSummaryList copy={copy} language={language} orders={visibleOrders} onOpen={setSelectedOrder} />
      ) : visibleOrders.length ? (
        <div className="ap-order-list">
          {visibleOrders.map((item) => (
            <article className="ap-trace-card" key={item.order_id}>
              <header>
                <div>
                  <strong>{item.plan_name}</strong>
                  <small>
                    {item.sku_code} · {copy.channels[item.channel] || item.channel}
                  </small>
                </div>
                <div>
                  <strong>{formatMoney(item.amount_fen)}</strong>
                  <span className="ap-status">{copy.orderStatuses[item.status] || item.status}</span>
                </div>
              </header>
              <TraceDetails
                items={[
                  [copy.pinvouOrder, item.pinvou_order_id || item.order_id],
                  [copy.providerOrderReference, item.provider_order_reference],
                  [copy.createdAt, formatUsageTime(item.created_at, language)],
                  [copy.paidAt, formatUsageTime(item.paid_at, language)],
                  [copy.creditedAt, formatUsageTime(item.credited_at, language)],
                  [copy.validUntil, formatUsageTime(item.valid_until, language)],
                  [copy.fulfillmentStatus, copy.fulfillmentStatuses?.[item.fulfillment_status] || item.fulfillment_status],
                  [copy.invoiceStatus, copy.invoiceStatuses?.[item.invoice_status] || item.invoice_status],
                ]}
              />
            </article>
          ))}
        </div>
      ) : (
        <Empty>{successfulOnly ? copy.noPaidOrders : copy.noOrders}</Empty>
      )}
      {selectedOrder && (
        <Dialog title={copy.orderDetails} closeLabel={copy.closeDialog} onClose={() => setSelectedOrder(null)} className="ap-order-dialog">
          <OrderDetails copy={copy} language={language} item={selectedOrder} />
        </Dialog>
      )}
    </section>
  );
}

function OrderSummaryList({ copy, language, orders, onOpen }) {
  const openWithKeyboard = (event, item) => {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onOpen(item);
    }
  };
  return (
    <>
      <div className="ap-order-summary-table-wrap">
        <table className="ap-order-summary-table">
          <thead>
            <tr>
              <th>{copy.orderColumns.description}</th>
              <th>{copy.orderColumns.type}</th>
              <th>{copy.orderColumns.amount}</th>
              <th>{copy.orderColumns.date}</th>
              <th>{copy.orderColumns.status}</th>
              <th>{copy.orderColumns.orderNumber}</th>
              <th className="ap-order-optional">{copy.orderColumns.duration}</th>
              <th className="ap-order-optional">{copy.orderColumns.quantity}</th>
            </tr>
          </thead>
          <tbody>
            {orders.map((item) => {
              const orderID = item.pinvou_order_id || item.order_id;
              const duration = orderDurationDays(item);
              return (
                <tr key={item.order_id} tabIndex="0" onClick={() => onOpen(item)} onKeyDown={(event) => openWithKeyboard(event, item)} aria-label={copy.openOrderDetails(item.plan_name)}>
                  <td><strong>{item.plan_name}</strong></td>
                  <td>{copy.orderTypeRecharge}</td>
                  <td>{formatMoney(item.amount_fen)}</td>
                  <td>{formatUsageTime(item.paid_at, language)}</td>
                  <td><span className="ap-status">{copy.orderSummaryStatuses[orderSummaryStatus(item)]}</span></td>
                  <td><span className="ap-order-id" title={orderID}>{compactOrderID(orderID)}</span></td>
                  <td className="ap-order-optional">{duration == null ? '—' : copy.durationDays(duration)}</td>
                  <td className="ap-order-optional">1</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <div className="ap-order-summary-cards">
        {orders.map((item) => (
          <button type="button" key={item.order_id} onClick={() => onOpen(item)} aria-label={copy.openOrderDetails(item.plan_name)}>
            <span><strong>{item.plan_name}</strong><small>{formatUsageTime(item.paid_at, language)}</small></span>
            <span><strong>{formatMoney(item.amount_fen)}</strong><span className="ap-status">{copy.orderSummaryStatuses[orderSummaryStatus(item)]}</span></span>
          </button>
        ))}
      </div>
    </>
  );
}

function OrderDetails({ copy, language, item }) {
  return (
    <div className="ap-order-detail">
      <header>
        <div>
          <strong>{item.plan_name}</strong>
          <small>{item.sku_code}</small>
        </div>
        <strong>{formatMoney(item.amount_fen)}</strong>
      </header>
      <TraceDetails
        items={[
          [copy.paymentMethod, copy.channels[item.channel] || item.channel],
          [copy.pinvouOrder, item.pinvou_order_id || item.order_id],
          [copy.providerOrderReference, item.provider_order_reference],
          [copy.createdAt, formatUsageTime(item.created_at, language)],
          [copy.paidAt, formatUsageTime(item.paid_at, language)],
          [copy.creditedAt, formatUsageTime(item.credited_at, language)],
          [copy.validUntil, formatUsageTime(item.valid_until, language)],
          [copy.paymentStatus, copy.orderStatuses[item.status] || item.status],
          [copy.fulfillmentStatus, copy.fulfillmentStatuses?.[item.fulfillment_status] || item.fulfillment_status],
          [copy.invoiceStatus, copy.invoiceStatuses?.[item.invoice_status] || item.invoice_status],
        ]}
      />
    </div>
  );
}

function TraceDetails({ items }) {
  return (
    <dl className="ap-trace-details">
      {items.map(([label, value]) => (
        <div key={label}>
          <dt>{label}</dt>
          <dd>{value || '—'}</dd>
        </div>
      ))}
    </dl>
  );
}

function formatMoney(amountFen) {
  return Number.isFinite(Number(amountFen)) ? `¥${(Number(amountFen) / 100).toFixed(2)}` : '—';
}

function Invoice({ copy, embedded = false }) {
  const Heading = embedded ? 'h3' : 'h2';
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {!embedded && <p className="ap-eyebrow">{copy.sections.invoice}</p>}
      <Heading>{copy.invoiceTitle}</Heading>
      <p className="ap-lead">{copy.invoiceLead}</p>
      <div className="ap-card">
        <h3>{copy.invoiceUnavailableTitle}</h3>
        <p className="ap-muted">{copy.invoiceUnavailable}</p>
      </div>
    </section>
  );
}

function formatUsageTime(value, language) {
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language] || language;
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? '—'
    : new Intl.DateTimeFormat(locale, {
        dateStyle: 'medium',
        timeStyle: 'short',
      }).format(date);
}

function Usage({ copy, language, records, unavailable, hasMore, busy, canCancel = true, onLoadMore, onCancel, embedded = false }) {
  const Heading = embedded ? 'h3' : 'h2';
  const summaries = summarizeUsageRecords(records);
  const header = (
    <>
      {!embedded && <p className="ap-eyebrow">{copy.sections.usage}</p>}
      <Heading>{copy.usageTitle}</Heading>
      <p className="ap-lead">{copy.usageLead}</p>
    </>
  );
  if (unavailable)
    return (
      <section className={embedded ? 'ap-combined-section' : undefined}>
        {header}
        <div role="alert" className="ap-alert error">
          {copy.usageUnavailable}
        </div>
      </section>
    );
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {header}
      {summaries.length ? (
        <div className="ap-usage-list">
          {summaries.map((record, index) => {
            const consumed = record.actual_microcredits == null && record.display_status !== 'RELEASED' ? '—' : formatUsagePoints(record.actual_microcredits || 0, language);
            const statusClass = String(record.display_status || '')
              .toLowerCase()
              .replaceAll('_', '-');
            const tokenKnown = record.input_tokens != null && record.output_tokens != null;
            const canStop = Boolean(record.cancel_usage_request_id);
            return (
              <article className="ap-usage-row" key={record.logical_turn_id || record.usage_request_id || `${record.started_at}-${record.model}-${index}`}>
                <div>
                  <strong>{record.model || copy.unknownModel}</strong>
                  <small>{formatUsageTime(record.started_at, language)}</small>
                  {tokenKnown && (
                    <small className="ap-usage-tokens">
                      {copy.inputTokens}: {record.input_tokens.toLocaleString()} · {copy.outputTokens}: {record.output_tokens.toLocaleString()} · {copy.cachedTokens}: {(record.cached_input_tokens || 0).toLocaleString()}
                    </small>
                  )}
                  <small title={record.logical_turn_id || record.usage_request_id}>
                    {copy.usageRecord}: {String(record.logical_turn_id || record.usage_request_id || '—').slice(0, 8)}
                  </small>
                </div>
                <div>
                  <strong className="ap-usage-amount">
                    {consumed} {copy.points}
                  </strong>
                  <span className={`ap-status ap-usage-status-${statusClass}`}>{copy.usageStatuses[record.display_status] || record.display_status}</span>
                  <small>
                    {copy.executionState}: {record.execution_status} · {copy.settlementState}: {record.settlement_status}
                  </small>
                  {canStop && (
                    <button disabled={busy || !canCancel} title={!canCancel ? copy.permissionDenied : undefined} onClick={() => onCancel(record.cancel_usage_request_id)}>
                      {copy.stopDelivery}
                    </button>
                  )}
                  {record.completed_at && (
                    <small>
                      {copy.completedAt}: {formatUsageTime(record.completed_at, language)}
                    </small>
                  )}
                </div>
              </article>
            );
          })}
          {hasMore && (
            <button className="ap-usage-more" disabled={busy} onClick={onLoadMore}>
              {busy ? copy.loadingMore : copy.loadMore}
            </button>
          )}
        </div>
      ) : (
        <Empty>{copy.noUsage}</Empty>
      )}
    </section>
  );
}
