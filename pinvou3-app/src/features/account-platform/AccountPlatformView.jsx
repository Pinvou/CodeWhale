import React, { useEffect, useRef, useState } from 'react';
import { createContextRequestGuard, rechargePollDelay } from './context-guard.js';
import { formatPoints, MICROCREDITS_PER_POINT } from './credit-units.js';
import { localeNames, supportedLocales } from '../../shared/supported-locales.js';
import './account-platform.css';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'MANUAL_REVIEW']);
const idempotencyKey = prefix => `${prefix}-${globalThis.crypto.randomUUID()}`;

function Field({ label, children }) {
  return <label className="ap-field"><span>{label}</span>{children}</label>;
}

function Empty({ children }) { return <div className="ap-empty">{children}</div>; }

function ResourceSection({ name, personalContext, copy, children }) {
  const title = name === 'credits' ? copy.creditsTitle : copy.entitlementsTitle;
  const lead = name === 'credits' ? copy.creditsLead : copy.entitlementsLead;
  return <div className={`ap-resource-section ap-${name}-section ${personalContext ? 'ap-context-personal' : 'ap-context-organization'}`}><p className="ap-eyebrow">{copy.sections[name]}</p><h2>{title}</h2><p className="ap-lead">{lead}</p>{children}</div>;
}

function ContextCredits({ copy, language, balance }) {
  return <div className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{formatPoints(balance?.available_microcredits, language)}</strong><small>{copy.reservedCredits}: {formatPoints(balance?.reserved_microcredits, language)}</small></div>;
}

function NavIcon({ name }) {
  const paths = {
    overview: <><rect x="3" y="3" width="7" height="7" rx="2" /><rect x="14" y="3" width="7" height="7" rx="2" /><rect x="3" y="14" width="7" height="7" rx="2" /><rect x="14" y="14" width="7" height="7" rx="2" /></>,
    organization: <><circle cx="12" cy="7" r="3" /><circle cx="6" cy="17" r="2.5" /><circle cx="18" cy="17" r="2.5" /><path d="M12 10v3M8 15l4-2 4 2" /></>,
    credits: <><rect x="3" y="5" width="18" height="14" rx="3" /><path d="M3 9h18M16 14h2" /></>,
    entitlements: <><path d="M4 7h16v13H4zM2 7h20M12 7v13M7 3c2.5 0 5 4 5 4S9.5 7 7 7a2 2 0 1 1 0-4ZM17 3c-2.5 0-5 4-5 4s2.5 0 5 0a2 2 0 1 0 0-4Z" /></>,
    orders: <><path d="M6 3h12v18H6z" /><path d="M9 8h6M9 12h6M9 16h4" /></>,
    invoice: <><path d="M5 3h14v18l-3-2-4 2-4-2-3 2z" /><path d="M8 8h8M8 12h8" /></>,
    usage: <><path d="M4 19V9M10 19V5M16 19v-7M22 19H2" /></>,
    closure: <><path d="M5 4h14v16H5zM8 8h8M8 12h8M8 16h5" /></>,
  };
  return <svg className="ap-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[name]}</svg>;
}

function localizedError(copy, reason) {
  const raw = String(reason?.code || reason?.message || reason || '');
  const code = raw.split(':', 1)[0].trim();
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
  const [section, setSection] = useState('overview');
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
  const activeOrganization = organizations.find(item => item.organization_id === activeOrganizationId);
  const personalContext = !activeOrganizationId;
  const navigationSections = [
    'overview',
    'credits',
    'entitlements',
    'usage',
    ...(organizations.length > 0 ? ['organization'] : []),
    ...(personalContext ? ['orders', 'invoice'] : []),
    'closure',
  ];

  const refreshClosure = async signal => {
    try { setClosure(await client.accountClosure(signal)); }
    catch (reason) { if (reason?.status !== 404 && reason?.name !== 'AbortError') throw reason; }
  };

  const clearContextResources = () => {
    guardRef.current.cancel();
    setBalance(null); setUsageRecords([]); setUsageCursor(null); setUsageUnavailable(false);
    setUnits([]); setMembers([]); setOrder(null); setOrders([]); setPackages({ summary: null, items: [] }); setQuotaGrants([]); setQuotaMembers([]); setCatalogModels([]); setModelPolicy(null);
    setBillingUnavailable(false); setBillingDetailsUnavailable(false);
  };

  const loadContext = async (nextMe = me) => {
    const request = guardRef.current.begin();
    setError('');
    const organizationId = nextMe?.active_context?.organization_id;
    try {
      const isPersonal = !organizationId;
      const permissions = new Set(nextMe?.active_context?.permissions || []);
      const canReadOrganizationEntitlements = permissions.has('org.wallet.read') || permissions.has('org.quota.allocate') || permissions.has('org.entitlements.purchase');
      const canReadQuota = permissions.has('org.quota.read.self') || permissions.has('org.quota.read.all');
      const canReadMembers = permissions.has('org.members.read');
      const canAllocateQuota = permissions.has('org.quota.allocate');
      const canReadModelPolicy = permissions.has('org.models.read') || permissions.has('org.models.manage');
      const results = await Promise.allSettled([
        client.organizations(request.signal), client.products(request.signal), client.balance(request.signal),
        client.recentRechargeOrder(request.signal),
        organizationId ? client.units(organizationId, request.signal) : Promise.resolve({ items: [] }),
        organizationId && canReadMembers ? client.members(organizationId, request.signal) : Promise.resolve({ items: [] }),
        client.usageHistory('', request.signal),
        isPersonal || canReadOrganizationEntitlements ? client.entitlementPackages(request.signal) : Promise.resolve({ summary: null, items: [] }),
        isPersonal ? client.rechargeOrders(request.signal) : Promise.resolve({ items: [] }),
        organizationId && canReadQuota ? client.organizationQuotaGrants(organizationId, request.signal) : Promise.resolve({ items: [] }),
        organizationId && canAllocateQuota ? client.quotaMembers(organizationId, request.signal) : Promise.resolve({ items: [] }),
        client.models(request.signal),
        organizationId && canReadModelPolicy ? client.organizationModelPolicy(organizationId, request.signal) : Promise.resolve(null),
      ]);
      if (!guardRef.current.isCurrent(request.generation)) return;
      const value = index => results[index].status === 'fulfilled' ? results[index].value : null;
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
      const billingFailed = [1, 2, 3].some(index => results[index].status === 'rejected');
      setBillingUnavailable(billingFailed);
      setBillingDetailsUnavailable([7, 8, 9].some(index => results[index].status === 'rejected'));
      if ([0, 4, 5].some(index => results[index].status === 'rejected')) setError(copy.requestFailed);
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
        if (active) { setAuth('error'); setError(copy.requestFailed); }
      }
    })();
    return () => { active = false; guardRef.current.cancel(); };
  }, [client]);

  useEffect(() => {
    let disposed = false;
    const synchronize = async (minimumVersion = 0) => {
      if (!meRef.current) return;
      const currentVersion = Number(meRef.current?.active_context?.context_version || 0);
      if (contextMutationRef.current || contextRefreshRef.current || (minimumVersion > 0 && minimumVersion <= currentVersion)) return;
      contextRefreshRef.current = true;
      try {
        const account = await client.bootstrap();
        if (disposed) return;
        const nextVersion = Number(account?.active_context?.context_version || 0);
        const currentOrganization = meRef.current?.active_context?.organization_id || '';
        const nextOrganization = account?.active_context?.organization_id || '';
        if (nextVersion > currentVersion || nextOrganization !== currentOrganization) {
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
    const unsubscribe = client.onContextVersion?.(version => { void synchronize(version); });
    const onFocus = () => { if (meRef.current) void synchronize(); };
    globalThis.addEventListener?.('focus', onFocus);
    return () => {
      disposed = true;
      unsubscribe?.();
      globalThis.removeEventListener?.('focus', onFocus);
    };
  }, [client]);

  useEffect(() => {
    setQrData(order?.qr_data || '');
  }, [order?.qr_data]);

  useEffect(() => {
    if (!closure || closure.status === 'COMPLETED') return undefined;
    const timer = globalThis.setInterval(() => { void refreshClosure().catch(reason => setError(localizedError(copy, reason))); }, 5000);
    return () => globalThis.clearInterval(timer);
  }, [closure?.closure_request_id, closure?.status]);

  useEffect(() => {
    if (!navigationSections.includes(section)) setSection('overview');
  }, [activeOrganizationId, organizations.length, section]);

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
    return () => { stopped = true; clearTimeout(timer); };
  }, [order?.order_id, order?.status]);

  const run = async action => {
    setBusy(true); setError(''); setNotice('');
    try { await action(); } catch (reason) { if (reason?.name !== 'AbortError') setError(localizedError(copy, reason)); }
    finally { setBusy(false); }
  };

  const login = () => run(async () => {
    await client.login();
    const account = await client.bootstrap();
    meRef.current = account;
    setMe(account);
    setAuth('ready');
    await loadContext(account);
  });

  const logout = () => run(async () => {
    await client.logout();
    guardRef.current.cancel();
    meRef.current = null;
    setMe(null); setOrganizations([]); setUnits([]); setMembers([]); setProducts([]); setOrders([]); setPackages({ summary: null, items: [] }); setQuotaGrants([]); setQuotaMembers([]);
    setBalance(null); setUsageRecords([]); setUsageCursor(null); setUsageUnavailable(false); setBillingDetailsUnavailable(false); setOrder(null); setQrData(''); setAuth('guest');
    if (portal) window.location.reload();
  });

  const switchContext = value => run(async () => {
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

  const loadMoreUsage = () => run(async () => {
    if (!usageCursor) return;
    const request = guardRef.current.begin();
    const page = await client.usageHistory(usageCursor, request.signal);
    if (!guardRef.current.isCurrent(request.generation)) return;
    setUsageRecords(current => {
      const known = new Set(current.map(item => item.usage_request_id));
      return [...current, ...(page.items || []).filter(item => !known.has(item.usage_request_id))];
    });
    setUsageCursor(page.next_cursor || null);
  });

  const requestClosure = () => run(async () => {
    const confirmation = globalThis.prompt(copy.closure.confirmPrompt);
    if (confirmation !== 'CLOSE_MY_ACCOUNT') {
      if (confirmation !== null) setError(copy.closure.confirmMismatch);
      return;
    }
    const created = await client.createAccountClosure(confirmation, idempotencyKey('account-closure'));
    setClosure(created); setNotice(copy.closure.requested);
  });

  if (auth === 'loading') return <div className={`ap-shell ap-centered ${portal ? 'ap-portal' : 'ap-desktop'}`}><div className="ap-spinner" />{copy.loading}</div>;
  if (auth === 'guest' || auth === 'error') return <div className={`ap-shell ap-login ${portal ? 'ap-portal' : 'ap-desktop'}`}><div><p className="ap-eyebrow">PINVOU ACCOUNT PLATFORM</p><h1>{copy.title}</h1><p>{portal ? copy.loginIntro : copy.desktopLoginIntro}</p>{error && <div role="alert" className="ap-alert error">{error}</div>}{portal ? <a className="ap-primary" href="/auth/login">{copy.login}</a> : <button className="ap-primary" disabled={busy} onClick={login}>{busy ? copy.signingIn : copy.login}</button>}{onClose && <button className="ap-close" onClick={onClose}>{copy.close}</button>}</div></div>;

  const userLabel = me?.display_name || copy.user;
  const identityLabel = activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal;

  return <div className={`ap-shell ap-app ${portal ? 'ap-portal' : 'ap-desktop'}`}>
    <aside className="ap-sidebar">
      <div className="ap-sidebar-brand"><span className="ap-brand-mark">P</span><div><p className="ap-eyebrow">PINVOU</p><h1>{copy.title}</h1></div></div>
      <div className="ap-user-summary"><span className="ap-user-avatar">{userLabel.trim().slice(0, 1).toUpperCase()}</span><div><strong>{userLabel}</strong><span>{identityLabel}</span></div></div>
      <nav>{navigationSections.map(key => <button key={key} className={section === key ? 'active' : ''} onClick={() => setSection(key)}><NavIcon name={key} /><span>{copy.sections[key]}</span></button>)}</nav>
      <div className="ap-sidebar-foot">
        <select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}>{supportedLocales.map(locale => <option key={locale} value={locale}>{localeNames[locale]}</option>)}</select>
        <button onClick={logout}>{copy.logout}</button>
        {onClose && <button onClick={onClose}>{copy.close}</button>}
      </div>
    </aside>
    <main className="ap-main">
      {error && <div role="alert" className="ap-alert error">{error}</div>}{notice && <div className="ap-alert success">{notice}</div>}
      {section === 'overview' && <Overview copy={copy} language={language} me={me} organizations={organizations} balance={balance} activeOrganization={activeOrganization} activeOrganizationId={activeOrganizationId} busy={busy} onSwitchContext={switchContext} />}
      {section === 'credits' && <ResourceSection name="credits" personalContext={personalContext} copy={copy}>{personalContext ? <Wallet copy={copy} language={language} products={products} balance={balance} packages={packages} billingUnavailable={billingUnavailable} detailsUnavailable={billingDetailsUnavailable} order={order} qrData={qrData} busy={busy} onRecharge={(productId, channel) => run(async () => setOrder(await client.createRecharge(productId, channel, idempotencyKey('recharge'))))} /> : <ContextCredits copy={copy} language={language} balance={balance} />}</ResourceSection>}
      {section === 'entitlements' && <ResourceSection name="entitlements" personalContext={personalContext} copy={copy}>{personalContext ? <Wallet copy={copy} language={language} products={products} balance={balance} packages={packages} billingUnavailable={billingUnavailable} detailsUnavailable={billingDetailsUnavailable} order={order} qrData={qrData} busy={busy} onRecharge={(productId, channel) => run(async () => setOrder(await client.createRecharge(productId, channel, idempotencyKey('recharge'))))} /> : <OrganizationEntitlements copy={copy} language={language} organizationId={activeOrganizationId} permissions={me?.active_context?.permissions || []} products={products} packages={packages} members={quotaMembers} grants={quotaGrants} order={order} busy={busy} onPurchase={(productId, channel) => run(async () => setOrder(await client.purchaseOrganizationEntitlements(activeOrganizationId, productId, channel, idempotencyKey('org-recharge'))))} onAllocate={items => run(async () => { await client.allocateOrganizationQuota(activeOrganizationId, items, idempotencyKey('quota-batch')); await loadContext(me); })} onRevoke={(grantId, reason) => run(async () => { await client.revokeOrganizationQuota(activeOrganizationId, grantId, reason, idempotencyKey('quota-revoke')); await loadContext(me); })} onReclaim={(grantId, reason) => run(async () => { await client.reclaimAccountClosedGrant(activeOrganizationId, grantId, reason, idempotencyKey('closed-grant-reclaim')); await loadContext(me); })} />}</ResourceSection>}
      {section === 'organization' && <Organization copy={copy} organization={activeOrganization} units={units} members={members} models={catalogModels} modelPolicy={modelPolicy} permissions={me?.active_context?.permissions || []} busy={busy} onCreateUnit={(name, parentId) => run(async () => { await client.createUnit(activeOrganizationId, name, parentId); await loadContext(me); })} onInvite={(email, roles) => run(async () => { const invite = await client.invite(activeOrganizationId, email, roles); setNotice(`${copy.invitationCreated} ${invite.token}`); })} onUpdateModelPolicy={(mode, modelIds, reason) => run(async () => { await client.updateOrganizationModelPolicy(activeOrganizationId, { expected_version:modelPolicy?.version || 0, mode, model_ids:modelIds, reason }, idempotencyKey('model-policy')); await loadContext(me); })} />}
      {section === 'orders' && <Orders copy={copy} language={language} orders={orders} unavailable={billingDetailsUnavailable} />}
      {section === 'invoice' && <Invoice copy={copy} />}
      {section === 'usage' && <Usage copy={copy} language={language} records={usageRecords} unavailable={usageUnavailable} hasMore={Boolean(usageCursor)} busy={busy} onLoadMore={loadMoreUsage} onCancel={usageRequestID => run(async () => { await client.cancelUsage(usageRequestID, idempotencyKey('usage-cancel')); await loadContext(me); setNotice(copy.stopRequested); })} />}
      {section === 'closure' && <AccountClosure copy={copy} item={closure} busy={busy} onRequest={requestClosure} />}
    </main>
  </div>;
}

function AccountClosure({ copy, item, busy, onRequest }) {
  return <section><p className="ap-eyebrow">{copy.sections.closure}</p><h2>{copy.closure.title}</h2><p className="ap-lead">{copy.closure.lead}</p><article className="ap-card">{item ? <><div className="ap-inline"><span className="ap-status">{item.status}</span><strong>{item.stage}</strong></div><p>{copy.closure.sla}: {new Date(item.sla_expires_at).toLocaleString()}</p><p className="ap-muted">{copy.closure.updated}: {new Date(item.updated_at).toLocaleString()}</p>{item.blockers?.length ? <ul className="ap-list">{item.blockers.map(blocker => <li key={`${blocker.domain}:${blocker.code}`}><span>{copy.closure.blockerCodes[blocker.code] || blocker.code}</span><small>{copy.closure.actionCodes[blocker.user_action] || blocker.user_action || blocker.domain}</small></li>)}</ul> : <p className="ap-muted">{copy.closure.noBlockers}</p>}</> : <><h3>{copy.closure.beforeTitle}</h3><p className="ap-muted">{copy.closure.before}</p><button className="ap-primary" disabled={busy} onClick={onRequest}>{copy.closure.start}</button></>}</article></section>;
}

function Overview({ copy, language, me, organizations, balance, activeOrganization, activeOrganizationId, busy, onSwitchContext }) {
  return <section><p className="ap-eyebrow">{copy.sections.overview}</p><h2>{copy.welcome(me?.display_name || copy.user)}</h2><p className="ap-lead">{copy.overviewLead}</p><div className="ap-context-card"><div><span>{copy.activeContext}</span><strong>{activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal}</strong><small>{copy.identityHelp}</small></div><label><span>{copy.switchContext}</span><select value={activeOrganizationId} onChange={event => onSwitchContext(event.target.value)} disabled={busy}><option value="">{copy.personal}</option>{organizations.map(item => <option key={item.organization_id} value={item.organization_id}>{copy.organizationIdentity(item.name)}</option>)}</select></label></div><div className="ap-metrics"><article><span>{copy.userCode}</span><strong>{me?.user_code || '—'}</strong></article><article><span>{copy.organizations}</span><strong>{organizations.length}</strong></article><article><span>{copy.availableCredits}</span><strong>{formatPoints(balance?.available_microcredits, language)}</strong></article><article><span>{copy.accountStatus}</span><strong>{me?.status || '—'}</strong></article></div></section>;
}

function Organization({ copy, organization, units, members, models, modelPolicy, permissions, busy, onCreateUnit, onInvite, onUpdateModelPolicy }) {
  const [unitName, setUnitName] = useState(''); const [parentId, setParentId] = useState(''); const [email, setEmail] = useState('');
  if (!organization) return <Empty>{copy.chooseOrganization}</Empty>;
  return <section><p className="ap-eyebrow">{copy.sections.organization}</p><h2>{organization.name}</h2><div className="ap-grid"><article className="ap-card"><h3>{copy.organizationTree}</h3>{units.length ? <ul className="ap-list">{units.map(unit => <li key={unit.unit_id}><span>{unit.name}</span><small>{unit.parent_id ? copy.childUnit : copy.rootUnit}</small></li>)}</ul> : <Empty>{copy.noUnits}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); if (unitName.trim()) onCreateUnit(unitName.trim(), parentId || null).then?.(() => setUnitName('')); }}><Field label={copy.unitName}><input value={unitName} onChange={event => setUnitName(event.target.value)} /></Field><Field label={copy.parentUnit}><select value={parentId} onChange={event => setParentId(event.target.value)}><option value="">{copy.rootUnit}</option>{units.map(unit => <option key={unit.unit_id} value={unit.unit_id}>{unit.name}</option>)}</select></Field><button disabled={busy || !unitName.trim()}>{copy.createUnit}</button></form></article><article className="ap-card"><h3>{copy.members}</h3>{members.length ? <ul className="ap-list">{members.map(member => <li key={member.user_id}><span>{member.display_name || member.email || member.user_id}</span><small>{member.roles.join(' · ')}</small></li>)}</ul> : <Empty>{copy.noMembers}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); if (email.trim()) onInvite(email.trim(), ['MEMBER']).then?.(() => setEmail('')); }}><Field label={copy.memberEmail}><input type="email" value={email} onChange={event => setEmail(event.target.value)} /></Field><button disabled={busy || !email.trim()}>{copy.createInvitation}</button></form></article></div>{modelPolicy && <ModelPolicy copy={copy} policy={modelPolicy} models={models} canManage={(permissions || []).includes('org.models.manage')} busy={busy} onSave={onUpdateModelPolicy} />}</section>;
}

function ModelPolicy({ copy, policy, models, canManage, busy, onSave }) {
  const [mode, setMode] = useState(policy.mode);
  const [selected, setSelected] = useState(policy.model_ids || []);
  useEffect(() => { setMode(policy.mode); setSelected(policy.model_ids || []); }, [policy.version, policy.mode]);
  const toggle = id => setSelected(current => current.includes(id) ? current.filter(value => value !== id) : [...current, id]);
  const save = () => { const reason = window.prompt(copy.modelPolicyReason); if (reason?.trim()) onSave(mode, mode === 'ALLOW_ALL' ? [] : selected, reason.trim()); };
  return <article className="ap-card"><h3>{copy.modelPolicy}</h3><p className="ap-muted">{copy.modelPolicyLead}</p><Field label={copy.modelPolicyMode}><select value={mode} disabled={!canManage || busy} onChange={event => setMode(event.target.value)}><option value="ALLOW_ALL">{copy.modelPolicyModes.ALLOW_ALL}</option><option value="ALLOW_LIST">{copy.modelPolicyModes.ALLOW_LIST}</option><option value="DENY_LIST">{copy.modelPolicyModes.DENY_LIST}</option></select></Field>{mode !== 'ALLOW_ALL' && <div className="ap-stack">{models.map(model => <label key={model.model_id} className="ap-inline"><input type="checkbox" checked={selected.includes(model.model_id)} disabled={!canManage || busy} onChange={() => toggle(model.model_id)} /><span>{model.display_name}</span><small>{model.eligibility_reason}</small></label>)}</div>}{canManage && <button disabled={busy} onClick={save}>{copy.saveModelPolicy}</button>}</article>;
}

function OrganizationEntitlements({ copy, language, permissions, products, packages, members, grants, order, busy, onPurchase, onAllocate, onRevoke, onReclaim }) {
  const permissionSet = new Set(permissions || []);
  const canPurchase = permissionSet.has('org.entitlements.purchase');
  const canAllocate = permissionSet.has('org.quota.allocate');
  const canRevoke = permissionSet.has('org.quota.revoke');
  const canReclaimClosed = permissionSet.has('organization.grant.reclaim_closed');
  const [productId, setProductId] = useState('');
  const [channel, setChannel] = useState('');
  const [memberId, setMemberId] = useState('');
  const [packageId, setPackageId] = useState('');
  const [componentId, setComponentId] = useState('');
  const [amount, setAmount] = useState('');
  useEffect(() => { if (!productId && products[0]) setProductId(products[0].product_id); }, [productId, products]);
  const selectedProduct = products.find(item => item.product_id === productId);
  const availableChannels = selectedProduct?.channels || [];
  useEffect(() => {
    if (!availableChannels.includes(channel)) setChannel(availableChannels[0] || '');
  }, [channel, productId, availableChannels.join('|')]);
  useEffect(() => { if (!memberId && members[0]) setMemberId(members[0].user_id); }, [memberId, members]);
  useEffect(() => { if (!packageId && packages.items?.[0]) setPackageId(packages.items[0].package_id); }, [packageId, packages.items]);
  const selectedPackage = packages.items?.find(item => item.package_id === packageId);
  const allocatableComponents = (selectedPackage?.components || []).filter(item => item.status === 'ACTIVE' && Number(item.available_microcredits) > 0);
  const effectiveComponentId = allocatableComponents.some(item => item.component_id === componentId) ? componentId : (allocatableComponents[0]?.component_id || '');
  const selectedComponent = allocatableComponents.find(item => item.component_id === effectiveComponentId);
  const selectedMember = members.find(item => item.user_id === memberId);
  const amountMicrocredits = Math.round(Number(amount) * MICROCREDITS_PER_POINT);
  return <section className="ap-stack">
    {canPurchase && <article className="ap-card"><h3>{copy.organizationPurchase}</h3><p className="ap-muted">{copy.organizationPurchaseLead}</p><div className="ap-inline"><select value={productId} onChange={event => setProductId(event.target.value)}>{products.map(product => <option key={product.product_id} value={product.product_id}>{product.plan_name} · {formatMoney(product.amount_fen)}</option>)}</select><select value={channel} onChange={event => setChannel(event.target.value)}>{availableChannels.map(item => <option key={item} value={item}>{copy.channels[item] || item}</option>)}</select><button className="ap-primary" disabled={busy || !productId || !channel} onClick={() => onPurchase(productId, channel)}>{copy.purchaseForOrganization}</button></div>{order?.beneficiary_organization_id && <p className="ap-muted">{copy.organizationOrderCreated}: {order.merchant_order_no}</p>}</article>}
    {canAllocate && <article className="ap-card"><h3>{copy.allocateMemberQuota}</h3><p className="ap-muted">{copy.allocateMemberQuotaLead}</p><form className="ap-stack" onSubmit={event => { event.preventDefault(); if (selectedMember && selectedComponent && amountMicrocredits > 0) onAllocate([{ member_user_id:selectedMember.user_id, membership_version:selectedMember.version, package_id:packageId, component_id:effectiveComponentId, component_type:selectedComponent.component_type, amount_microcredits:amountMicrocredits }]).then?.(() => setAmount('')); }}><Field label={copy.member}><select value={memberId} onChange={event => setMemberId(event.target.value)}>{members.map(member => <option key={member.user_id} value={member.user_id}>{member.display_name || member.email || member.user_id}</option>)}</select></Field><Field label={copy.entitlementPackage}><select value={packageId} onChange={event => setPackageId(event.target.value)}>{(packages.items || []).map(item => <option key={item.package_id} value={item.package_id}>{item.plan_name} · {String(item.package_id).slice(0, 8)}</option>)}</select></Field><Field label={copy.entitlementComponent}><select value={effectiveComponentId} onChange={event => setComponentId(event.target.value)}>{allocatableComponents.map(component => <option key={component.component_id} value={component.component_id}>{copy.componentTypes[component.component_type] || component.component_type} · {formatPoints(component.available_microcredits, language)}</option>)}</select></Field><Field label={copy.quotaAmount}><input type="number" min="0.0001" step="0.0001" value={amount} onChange={event => setAmount(event.target.value)} /></Field><button disabled={busy || !selectedMember || !selectedComponent || amountMicrocredits <= 0 || amountMicrocredits > Number(selectedComponent?.available_microcredits || 0)}>{copy.allocateQuota}</button></form></article>}
    <article className="ap-card"><h3>{copy.memberQuotaGrants}</h3>{grants.length ? <div className="ap-order-list">{grants.map(grant => <article className="ap-trace-card" key={grant.grant_id}><header><div><strong>{members.find(member => member.user_id === grant.member_user_id)?.display_name || grant.member_user_id}</strong><small>{copy.componentTypes[grant.component_type] || grant.component_type} · {String(grant.package_id).slice(0, 8)}</small></div><div><strong>{formatPoints(grant.available_microcredits, language)} {copy.points}</strong><span className="ap-status">{copy.grantStatuses?.[grant.status] || grant.status}</span></div></header>{canRevoke && (grant.status === 'ACTIVE' || (grant.status === 'SUSPENDED' && grant.suspension_reason !== 'ACCOUNT_CLOSED')) && Number(grant.reserved_microcredits) === 0 && Number(grant.available_microcredits) > 0 && <button disabled={busy} onClick={() => onRevoke(grant.grant_id, 'ADMIN_REVOKE')}>{copy.revokeQuota}</button>}{canReclaimClosed && grant.status === 'SUSPENDED' && grant.suspension_reason === 'ACCOUNT_CLOSED' && Number(grant.reserved_microcredits) === 0 && Number(grant.available_microcredits) > 0 && <button disabled={busy} onClick={() => onReclaim(grant.grant_id, 'ACCOUNT_CLOSED_FULL_RECLAIM')}>{copy.reclaimClosedQuota}</button>}</article>)}</div> : <Empty>{copy.noMemberQuotaGrants}</Empty>}</article>
  </section>;
}

function Wallet({ copy, language, products, balance, packages, billingUnavailable, detailsUnavailable, order, qrData, busy, onRecharge }) {
  const [productId, setProductId] = useState(''); const [channel, setChannel] = useState('');
  const [now, setNow] = useState(Date.now());
  useEffect(() => { if (!productId && products[0]) setProductId(products[0].product_id); }, [products, productId]);
  const selected = products.find(item => item.product_id === productId);
  const availableChannels = selected?.channels || [];
  useEffect(() => {
    if (!availableChannels.includes(channel)) setChannel(availableChannels[0] || '');
  }, [channel, productId, availableChannels.join('|')]);
  useEffect(() => {
    if (!order || terminalOrderStates.has(order.status)) return undefined;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [order?.order_id, order?.status]);
  const remainingSeconds = order ? Math.max(0, Math.ceil((new Date(order.expires_at).getTime() - now) / 1000)) : 0;
  const displayStatus = order?.status;
  const countdown = `${String(Math.floor(remainingSeconds / 60)).padStart(2, '0')}:${String(remainingSeconds % 60).padStart(2, '0')}`;
  return <section><p className="ap-eyebrow">{copy.sections.wallet}</p><h2>{copy.walletTitle}</h2><p className="ap-lead">{copy.walletLead}</p>{billingUnavailable && <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div>}<div className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{formatPoints(balance?.available_microcredits, language)}</strong><small>{copy.basePoints}: {formatPoints(packages.summary?.base_available_microcredits, language)} · {copy.bonusPoints}: {formatPoints(packages.summary?.bonus_available_microcredits, language)} · {copy.reservedCredits}: {formatPoints(balance?.reserved_microcredits, language)}</small></div><div className="ap-card"><h3>{copy.availablePlans}</h3><div className="ap-product-grid">{products.map(product => <button key={product.product_id} className={productId === product.product_id ? 'selected' : ''} onClick={() => setProductId(product.product_id)}><strong>{product.plan_name}</strong><span>¥{(product.amount_fen / 100).toFixed(2)} · {product.display_points.toLocaleString()} {copy.points}</span><small>{copy.validityDays(product.validity_days)}{product.price_status !== 'FROZEN' ? ` · ${copy.tentativePrice}` : ''}</small></button>)}</div><div className="ap-inline"><select value={channel} onChange={event => setChannel(event.target.value)}>{availableChannels.map(item => <option key={item} value={item}>{copy.channels[item] || item}</option>)}</select><button className="ap-primary" disabled={busy || billingUnavailable || !selected || !channel} onClick={() => onRecharge(productId, channel)}>{copy.createRecharge}</button></div>{order && <div className="ap-payment"><div>{qrData && <img src={qrData} alt={copy.paymentQR} />}</div><div><span className="ap-status">{copy.orderStatuses[displayStatus] || displayStatus}</span><h3>¥{(order.amount_fen / 100).toFixed(2)} · {copy.scanToPay}</h3><p>{remainingSeconds > 0 ? copy.countdown(countdown) : copy.orderStatuses.PENDING}</p></div></div>}</div><div className="ap-card ap-entitlements"><h3>{copy.myEntitlements}</h3><p className="ap-muted">{copy.consumptionOrder}</p><p className="ap-muted">{copy.refundCompositionNotice}</p>{detailsUnavailable ? <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div> : packages.items?.length ? <div className="ap-order-list">{packages.items.map(item => <article className="ap-trace-card" key={item.package_id}><header><div><strong>{item.plan_name}</strong><small>{item.sku_code}</small></div><div><strong>{formatPoints(item.points_remaining_microcredits, language)} {copy.points}</strong><span className="ap-status">{copy.packageStatuses[item.status] || item.status}</span></div></header><TraceDetails items={[
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
  ]} />{item.components?.map(component => <div className="ap-card" key={component.component_id}><strong>{copy.componentTypes[component.component_type] || component.component_type}</strong><TraceDetails items={[
    [copy.totalPoints, `${formatPoints(component.original_microcredits, language)} ${copy.points}`],
    [copy.availableCredits, `${formatPoints(component.available_microcredits, language)} ${copy.points}`],
    [copy.reservedCredits, `${formatPoints(component.reserved_microcredits, language)} ${copy.points}`],
    [copy.usedPoints, `${formatPoints(component.used_microcredits, language)} ${copy.points}`],
    [copy.expiredPoints, `${formatPoints(component.expired_microcredits, language)} ${copy.points}`],
    [copy.revokedPoints, `${formatPoints(component.revoked_microcredits, language)} ${copy.points}`],
    [copy.validUntil, formatUsageTime(component.expires_at, language)],
    [copy.expiryReminder, copy.expiryReminderAt(formatUsageTime(component.expires_at, language))],
  ]} /></div>)}</article>)}</div> : <Empty>{copy.noEntitlements}</Empty>}</div></section>;
}

function Orders({ copy, language, orders, unavailable }) {
  return <section><p className="ap-eyebrow">{copy.sections.orders}</p><h2>{copy.ordersTitle}</h2><p className="ap-lead">{copy.ordersLead}</p>{unavailable ? <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div> : orders.length ? <div className="ap-order-list">{orders.map(item => <article className="ap-trace-card" key={item.order_id}><header><div><strong>{item.plan_name}</strong><small>{item.sku_code} · {copy.channels[item.channel] || item.channel}</small></div><div><strong>{formatMoney(item.amount_fen)}</strong><span className="ap-status">{copy.orderStatuses[item.status] || item.status}</span></div></header><TraceDetails items={[
    [copy.pinvouOrder, item.pinvou_order_id || item.order_id],
    [copy.providerOrderReference, item.provider_order_reference],
    [copy.createdAt, formatUsageTime(item.created_at, language)],
    [copy.paidAt, formatUsageTime(item.paid_at, language)],
    [copy.creditedAt, formatUsageTime(item.credited_at, language)],
    [copy.validUntil, formatUsageTime(item.valid_until, language)],
    [copy.fulfillmentStatus, copy.fulfillmentStatuses?.[item.fulfillment_status] || item.fulfillment_status],
    [copy.invoiceStatus, copy.invoiceStatuses?.[item.invoice_status] || item.invoice_status],
  ]} /></article>)}</div> : <Empty>{copy.noOrders}</Empty>}</section>;
}

function TraceDetails({ items }) {
  return <dl className="ap-trace-details">{items.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value || '—'}</dd></div>)}</dl>;
}

function formatMoney(amountFen) {
  return Number.isFinite(Number(amountFen)) ? `¥${(Number(amountFen) / 100).toFixed(2)}` : '—';
}

function Invoice({ copy }) {
  return <section><p className="ap-eyebrow">{copy.sections.invoice}</p><h2>{copy.invoiceTitle}</h2><p className="ap-lead">{copy.invoiceLead}</p><div className="ap-card"><h3>{copy.invoiceUnavailableTitle}</h3><p className="ap-muted">{copy.invoiceUnavailable}</p></div></section>;
}

function formatUsageTime(value, language) {
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language] || language;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function Usage({ copy, language, records, unavailable, hasMore, busy, onLoadMore, onCancel }) {
  if (unavailable) return <section><p className="ap-eyebrow">{copy.sections.usage}</p><h2>{copy.usageTitle}</h2><p className="ap-lead">{copy.usageLead}</p><div role="alert" className="ap-alert error">{copy.usageUnavailable}</div></section>;
  return <section><p className="ap-eyebrow">{copy.sections.usage}</p><h2>{copy.usageTitle}</h2><p className="ap-lead">{copy.usageLead}</p>{records.length ? <div className="ap-usage-list">{records.map((record, index) => {
    const consumed = record.actual_microcredits == null && record.display_status !== 'RELEASED' ? '—' : formatPoints(record.actual_microcredits || 0, language);
    const statusClass = String(record.display_status || '').toLowerCase().replaceAll('_', '-');
    const tokenKnown = record.input_tokens != null && record.output_tokens != null;
    const canStop = record.delivery_status !== 'USER_STOPPED' && !['SUCCEEDED', 'FAILED', 'CANCELLED_BEFORE_DISPATCH'].includes(record.execution_status);
    return <article className="ap-usage-row" key={record.usage_request_id || `${record.started_at}-${record.model}-${index}`}><div><strong>{record.model || copy.unknownModel}</strong><small>{formatUsageTime(record.started_at, language)}</small>{tokenKnown && <small className="ap-usage-tokens">{copy.inputTokens}: {record.input_tokens.toLocaleString()} · {copy.outputTokens}: {record.output_tokens.toLocaleString()} · {copy.cachedTokens}: {(record.cached_input_tokens || 0).toLocaleString()}</small>}<small title={record.usage_request_id}>{copy.usageRecord}: {String(record.usage_request_id || '—').slice(0, 8)}</small></div><div><strong className="ap-usage-amount">{consumed} {copy.points}</strong><span className={`ap-status ap-usage-status-${statusClass}`}>{copy.usageStatuses[record.display_status] || record.display_status}</span><small>{copy.executionState}: {record.execution_status} · {copy.settlementState}: {record.settlement_status}</small>{canStop && <button disabled={busy} onClick={() => onCancel(record.usage_request_id)}>{copy.stopDelivery}</button>}{record.completed_at && <small>{copy.completedAt}: {formatUsageTime(record.completed_at, language)}</small>}</div></article>;
  })}{hasMore && <button className="ap-usage-more" disabled={busy} onClick={onLoadMore}>{busy ? copy.loadingMore : copy.loadMore}</button>}</div> : <Empty>{copy.noUsage}</Empty>}</section>;
}
