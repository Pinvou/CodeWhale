import React, { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from './context-guard.js';
import { accountErrorMessage, needsAccountLogin } from './errors.js';
import { safePNGImage } from './bff-client.js';
import { formatMoney, formatPoints } from './credit-units.js';
import './account-platform.css';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'PARTIALLY_REFUNDED', 'REFUNDED', 'MANUAL_REVIEW', 'EXPIRED']);
const idempotencyKey = prefix => {
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
  return `${prefix}-${Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('')}`;
};

function Field({ label, children }) {
  return <label className="ap-field"><span>{label}</span>{children}</label>;
}

function Empty({ children }) { return <div className="ap-empty">{children}</div>; }

function NavIcon({ name }) {
  const paths = {
    overview: <><rect x="3" y="3" width="7" height="7" rx="2" /><rect x="14" y="3" width="7" height="7" rx="2" /><rect x="3" y="14" width="7" height="7" rx="2" /><rect x="14" y="14" width="7" height="7" rx="2" /></>,
    organization: <><circle cx="12" cy="7" r="3" /><circle cx="6" cy="17" r="2.5" /><circle cx="18" cy="17" r="2.5" /><path d="M12 10v3M8 15l4-2 4 2" /></>,
    wallet: <><rect x="3" y="5" width="18" height="14" rx="3" /><path d="M3 9h18M16 14h2" /></>,
    orders: <><path d="M6 3h12v18H6z" /><path d="M9 8h6M9 12h6M9 16h4" /></>,
    invoice: <><path d="M5 3h14v18l-3-2-4 2-4-2-3 2z" /><path d="M8 8h8M8 12h8" /></>,
    tokens: <><rect x="3" y="6" width="18" height="12" rx="2" /><path d="M7 10h4M7 14h10" /></>,
    usage: <><path d="M4 19V9M10 19V5M16 19v-7M22 19H2" /></>,
  };
  return <svg className="ap-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[name]}</svg>;
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
  const [orders, setOrders] = useState([]);
  const [packages, setPackages] = useState({ summary: null, items: [] });
  const [tokens, setTokens] = useState([]);
  const [usageRecords, setUsageRecords] = useState([]);
  const [usageCursor, setUsageCursor] = useState(null);
  const [usageUnavailable, setUsageUnavailable] = useState(false);
  const [section, setSection] = useState('overview');
  const [busy, setBusy] = useState(false);
  const [loginAccount, setLoginAccount] = useState('');
  const [loginPassword, setLoginPassword] = useState('');
  const [errorReason, setErrorReason] = useState(null);
  const error = errorReason?.copyKey ? copy[errorReason.copyKey] || copy.requestFailed : errorReason ? accountErrorMessage(errorReason, copy) : '';
  const [notice, setNotice] = useState('');
  const [order, setOrder] = useState(null);
  const [qrData, setQrData] = useState('');
  const guardRef = useRef(createContextRequestGuard());
  const lifecycleRef = useRef({ generation: 0, active: false });
  const mutationRef = useRef(false);
  useLayoutEffect(() => {
    const life = lifecycleRef.current;
    life.generation += 1; life.active = true;
    setAuth('loading'); setMe(null); setOrganizations([]); setUnits([]); setMembers([]);
    setProducts([]); setBalance(null); setTokens([]); setUsageRecords([]); setUsageCursor(null);
    setOrder(null); setQrData(''); setOrders([]); setPackages({ summary: null, items: [] });
    setErrorReason(null); setNotice(''); setLoginPassword('');
    setBillingUnavailable(false); setBillingDetailsUnavailable(false); setUsageUnavailable(false);
    return () => { life.active = false; life.generation += 1; guardRef.current.cancel(); };
  }, [client]);
  const activeOrganizationId = me?.active_context?.organization_id || '';
  const activeOrganization = organizations.find(item => item.organization_id === activeOrganizationId);
  const personalContext = me?.active_context?.context_type === 'PERSONAL';
  const navigationSections = ['overview', ...(organizations.length ? ['organization'] : []),
    'wallet', ...(personalContext ? ['orders', 'invoice'] : []), 'usage', 'tokens'];
  useEffect(() => {
    if (!navigationSections.includes(section)) setSection('overview');
  }, [personalContext, organizations.length, section]);

  const fail = useCallback(reason => {
    if (needsAccountLogin(reason)) {
      lifecycleRef.current.generation += 1;
      guardRef.current.cancel();
      setMe(null); setOrganizations([]); setBalance(null); setTokens([]); setUsageRecords([]); setUsageCursor(null); setUsageUnavailable(false);
      setUnits([]); setMembers([]); setProducts([]); setOrder(null);
      setOrders([]); setPackages({ summary: null, items: [] }); setBillingDetailsUnavailable(false); setQrData('');
      setBillingUnavailable(false); setNotice(''); setErrorReason(null);
      setAuth('guest');
      return;
    }
    setErrorReason(reason);
  }, []);

  const loadContext = async (nextMe = me) => {
    const request = guardRef.current.begin();
    setErrorReason(null);
    const organizationId = nextMe?.active_context?.organization_id;
    try {
      const guardedRead = promise => promise.catch(reason => {
        if (guardRef.current.isCurrent(request.generation) && needsAccountLogin(reason)) fail(reason);
        throw reason;
      });
      const results = await Promise.allSettled([
        client.organizations(request.signal), client.products(request.signal), client.balance(request.signal),
        client.recentRechargeOrder(request.signal),
        organizationId ? client.units(organizationId, request.signal) : Promise.resolve({ items: [] }),
        organizationId ? client.members(organizationId, request.signal) : Promise.resolve({ items: [] }),
        client.usageHistory('', request.signal), client.managedTokens(request.signal),
        nextMe?.active_context?.context_type === 'PERSONAL' ? client.entitlementPackages(request.signal) : Promise.resolve({ summary: null, items: [] }),
        nextMe?.active_context?.context_type === 'PERSONAL' ? client.rechargeOrders(request.signal) : Promise.resolve({ items: [] }),
      ].map(guardedRead));
      if (!guardRef.current.isCurrent(request.generation)) return;
      const unauthorized = results.find(result => result.status === 'rejected' && needsAccountLogin(result.reason));
      if (unauthorized) { fail(unauthorized.reason); return; }
      const value = index => results[index].status === 'fulfilled' ? results[index].value : null;
      setOrganizations(value(0)?.items || []);
      setProducts(value(1)?.items || []);
      setBalance(value(2));
      setOrder(value(3)?.order || null);
      setUnits(value(4)?.items || []);
      setMembers(value(5)?.items || []);
      setUsageRecords(value(6)?.items || []);
      setUsageCursor(value(6)?.next_cursor || null);
      setTokens(value(7)?.items || []);
      setPackages(value(8) || { summary: null, items: [] }); setOrders(value(9)?.items || []);
      setBillingDetailsUnavailable([8, 9].some(index => results[index].status === 'rejected'));
      setUsageUnavailable(results[6].status === 'rejected');
      const billingFailed = [1, 2, 3].some(index => results[index].status === 'rejected');
      setBillingUnavailable(billingFailed);
      const failed = [0, 4, 5, 7].find(index => results[index].status === 'rejected');
      if (failed !== undefined) fail(results[failed].reason);
    } catch (reason) {
      if (reason?.name !== 'AbortError' && guardRef.current.isCurrent(request.generation)) fail(reason);
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
          if (session.errorCode && session.errorCode !== 'account_platform_disabled') fail({ code: session.errorCode });
          return;
        }
        const account = await client.bootstrap();
        if (!active) return;
        setMe(account);
        setAuth('ready');
        await loadContext(account);
      } catch (reason) {
        if (active) {
          if (needsAccountLogin(reason)) fail(reason);
          else { setAuth('error'); fail(reason); }
        }
      }
    })();
    return () => { active = false; guardRef.current.cancel(); };
  }, [client]);

  useEffect(() => {
    setQrData('');
    if (!order || terminalOrderStates.has(order.status) || new Date(order.expires_at).getTime() <= Date.now()) return undefined;
    if (order.qr_data) { setQrData(safePNGImage(order.qr_data)); return undefined; }
    const content = order.payment_url || order.qr_content || '';
    if (typeof content !== 'string' || !content || content.length > 8192) return undefined;
    let active = true;
    import('qrcode').then(({ default: QRCode }) => QRCode.toDataURL(content, { width: 240, margin: 2, errorCorrectionLevel: 'M' }))
      .then(value => { if (active) setQrData(value); }).catch(() => { if (active) setQrData(''); });
    return () => { active = false; };
  }, [client, order?.order_id, order?.status, order?.expires_at, order?.qr_data, order?.payment_url, order?.qr_content]);

  const acceptOrder = async (latest, isCurrent, signal) => {
    if (!isCurrent()) return;
    if (latest.status === 'PAID' || latest.status === 'PAID_LATE') {
      const results = await Promise.allSettled([client.balance(signal),
        personalContext ? client.entitlementPackages(signal) : Promise.resolve({ summary: null, items: [] }),
        personalContext ? client.rechargeOrders(signal) : Promise.resolve({ items: [] })].map(promise => promise.catch(reason => {
          if (isCurrent() && needsAccountLogin(reason)) fail(reason);
          throw reason;
        })));
      if (!isCurrent()) return;
      const denied = results.find(result => result.status === 'rejected' && needsAccountLogin(result.reason));
      if (denied) { fail(denied.reason); return; }
      if (results[0].status === 'fulfilled') { setBalance(results[0].value); setNotice('paymentReceived'); }
      else { setNotice(''); setErrorReason({ copyKey: 'balanceRefreshFailed' }); }
      if (results[1].status === 'fulfilled') setPackages(results[1].value);
      if (results[2].status === 'fulfilled') setOrders(results[2].value.items || []);
      setBillingDetailsUnavailable([1, 2].some(index => results[index].status === 'rejected'));
    }
    if (isCurrent()) setOrder(latest);
  };
  useEffect(() => {
    if (busy || auth !== 'ready' || !order?.order_id || terminalOrderStates.has(order.status) || new Date(order.expires_at).getTime() <= Date.now()) return undefined;
    const controller = new window.AbortController();
    const life = lifecycleRef.current.generation;
    let stopped = false; let timer; let attempt = 0;
    const isCurrent = () => !stopped && !controller.signal.aborted && lifecycleRef.current.active && lifecycleRef.current.generation === life;
    const poll = async () => {
      try {
        const latest = await client.rechargeOrder(order.order_id, controller.signal);
        if (!isCurrent()) return;
        await acceptOrder(latest, isCurrent, controller.signal);
        if (isCurrent() && !terminalOrderStates.has(latest.status) && new Date(latest.expires_at).getTime() > Date.now()) timer = setTimeout(poll, rechargePollDelay(attempt++));
      } catch (reason) {
        if (!isCurrent()) return;
        if (needsAccountLogin(reason)) { fail(reason); return; }
        timer = setTimeout(poll, rechargePollDelay(attempt++));
      }
    };
    timer = setTimeout(poll, rechargePollDelay(attempt++));
    return () => { stopped = true; controller.abort(); clearTimeout(timer); };
  }, [client, busy, auth, order?.order_id, order?.status, order?.expires_at, me?.active_context?.context_type, activeOrganizationId, fail]);

  const run = async action => {
    if (mutationRef.current) return;
    mutationRef.current = true;
    const generation = lifecycleRef.current.generation;
    const isCurrent = () => lifecycleRef.current.active && lifecycleRef.current.generation === generation;
    guardRef.current.cancel();
    setBusy(true); setErrorReason(null); setNotice('');
    try { await action(isCurrent); }
    catch (reason) { if (isCurrent() && reason?.name !== 'AbortError') fail(reason); }
    finally { mutationRef.current = false; if (lifecycleRef.current.active) setBusy(false); }
  };

  const login = () => run(async isCurrent => {
    const accountName = loginAccount.trim(); const password = loginPassword;
    setLoginPassword('');
    await client.login(accountName, password);
    if (!isCurrent()) return;
    const account = await client.bootstrap();
    if (!isCurrent()) return;
    setMe(account); setAuth('ready');
    await loadContext(account);
  });
  const logout = () => run(async isCurrent => {
    await client.logout();
    if (!isCurrent()) return;
    guardRef.current.cancel();
    setMe(null); setOrganizations([]); setUnits([]); setMembers([]); setProducts([]);
    setOrders([]); setPackages({ summary: null, items: [] }); setBillingDetailsUnavailable(false); setQrData('');
    setBalance(null); setTokens([]); setUsageRecords([]); setUsageCursor(null); setUsageUnavailable(false); setOrder(null); setQrData(''); setBillingUnavailable(false); setOrders([]); setPackages({ summary: null, items: [] }); setBillingDetailsUnavailable(false); setAuth('guest');
    if (portal) window.location.reload();
  });

  const switchContext = value => run(async isCurrent => {
    guardRef.current.cancel();
    setOrders([]); setPackages({ summary: null, items: [] }); setBillingDetailsUnavailable(false); setQrData('');
    setBalance(null); setTokens([]); setUsageRecords([]); setUsageCursor(null); setUsageUnavailable(false); setUnits([]); setMembers([]); setOrder(null); setBillingUnavailable(false);
    const organizationId = value || null;
    const context = await client.switchContext(organizationId ? 'ORGANIZATION' : 'PERSONAL', organizationId);
    if (!isCurrent()) return;
    const updated = { ...me, active_context: context };
    setMe(updated);
    await loadContext(updated);
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

  if (auth === 'loading') return <div className={"ap-shell ap-centered " + (portal ? "ap-portal" : "ap-desktop")}><div className="ap-spinner" />{copy.loading}</div>;
  if (auth === 'guest') return <div className={"ap-shell ap-login " + (portal ? "ap-portal" : "ap-desktop")}><div><p className="ap-eyebrow">PINVOU ACCOUNT PLATFORM</p><h1>{copy.title}</h1><p>{portal ? copy.loginIntro : copy.desktopLoginIntro}</p>{portal ? <a className="ap-primary" href="/auth/login">{copy.login}</a> : <form className="ap-stack" onSubmit={event => { event.preventDefault(); void login(); }}><Field label={copy.account}><input value={loginAccount} onChange={event => setLoginAccount(event.target.value)} autoComplete="username" disabled={busy} /></Field><Field label={copy.password}><input type="password" value={loginPassword} onChange={event => setLoginPassword(event.target.value)} autoComplete="current-password" disabled={busy} /></Field><button className="ap-primary" disabled={busy || !loginAccount.trim() || !loginPassword}>{busy ? copy.signingIn : copy.login}</button></form>}{error && <div role="alert" className="ap-alert error">{error}</div>}{onClose && <button className="ap-close" disabled={busy} onClick={onClose}>{copy.close}</button>}</div></div>;
  if (auth === 'error') return <div className={"ap-shell ap-centered " + (portal ? "ap-portal" : "ap-desktop")} role="alert">{error || copy.requestFailed}<button onClick={() => window.location.reload()}>{copy.retry}</button>{onClose && <button onClick={onClose}>{copy.close}</button>}</div>;
  const userLabel = me?.display_name || copy.user;
  const identityLabel = activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal;

  return <div className={"ap-shell ap-app " + (portal ? "ap-portal" : "ap-desktop")}>
    <aside className="ap-sidebar">
      <div className="ap-sidebar-brand"><span className="ap-brand-mark">P</span><div><p className="ap-eyebrow">PINVOU</p><h1>{copy.title}</h1></div></div>
      <div className="ap-user-summary"><span className="ap-user-avatar">{userLabel.trim().slice(0, 1).toUpperCase()}</span><div><strong>{userLabel}</strong><span>{identityLabel}</span></div></div>
      <nav>{navigationSections.map(key => <button key={key} className={section === key ? 'active' : ''} onClick={() => setSection(key)}><NavIcon name={key} /><span>{copy.sections[key]}</span></button>)}</nav>
      <div className="ap-sidebar-foot">
        <select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}><option value="zh">简体中文</option><option value="en">English</option><option value="ja">日本語</option></select>
        <button disabled={busy} onClick={logout}>{copy.logout}</button>
        {onClose && <button disabled={busy} onClick={onClose}>{copy.close}</button>}
      </div>
    </aside>
    <main className="ap-main">
      {error && <div role="alert" className="ap-alert error">{error}</div>}{notice && <div className="ap-alert success">{typeof notice === 'string' ? copy[notice] : copy[notice.copyKey] + ' ' + notice.value}</div>}
      {section === 'overview' && <Overview copy={copy} language={language} me={me} organizations={organizations} balance={balance} activeOrganization={activeOrganization} activeOrganizationId={activeOrganizationId} busy={busy} onSwitchContext={switchContext} />}
      {section === 'organization' && <Organization copy={copy} organization={activeOrganization} units={units} members={members} busy={busy} onCreateUnit={(name, parentId) => run(async isCurrent => { await client.createUnit(activeOrganizationId, name, parentId); if (isCurrent()) await loadContext(me); })} onInvite={(email, roles) => run(async isCurrent => { const invite = await client.invite(activeOrganizationId, email, roles); if (isCurrent()) setNotice({ copyKey: 'invitationCreated', value: invite.token }); })} />}
      {section === 'wallet' && <Wallet copy={copy} language={language} products={products} balance={balance} packages={packages} personalContext={personalContext} detailsUnavailable={billingDetailsUnavailable} billingUnavailable={billingUnavailable} order={order} qrData={qrData} busy={busy} onRecharge={(productId, channel) => run(async isCurrent => { const nextOrder = await client.createRecharge(productId, channel, idempotencyKey('recharge')); await acceptOrder(nextOrder, isCurrent); })} />}
      {section === 'orders' && personalContext && <Orders copy={copy} language={language} orders={orders} unavailable={billingDetailsUnavailable} />}
      {section === 'invoice' && personalContext && <Invoice copy={copy} />}
      {section === 'tokens' && <Tokens key={`${me?.active_context?.context_type}:${activeOrganizationId}`} copy={copy} language={language} client={client} onError={fail} me={me} organization={activeOrganization} tokens={tokens} balance={balance} billingUnavailable={billingUnavailable} busy={busy} run={run} reload={() => loadContext(me)} />}
      {section === 'usage' && <Usage copy={copy} language={language} records={usageRecords} unavailable={usageUnavailable} hasMore={Boolean(usageCursor)} busy={busy} onLoadMore={loadMoreUsage} />}
    </main>
  </div>;
}

function Overview({ copy, language, me, organizations, balance, activeOrganization, activeOrganizationId, busy, onSwitchContext }) {
  return <section><p className="ap-eyebrow">{copy.sections.overview}</p><h2>{copy.welcome(me?.display_name || copy.user)}</h2><p className="ap-lead">{copy.overviewLead}</p><div className="ap-context-card"><div><span>{copy.activeContext}</span><strong>{activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal}</strong><small>{copy.identityHelp}</small></div><label><span>{copy.switchContext}</span><select value={activeOrganizationId} onChange={event => onSwitchContext(event.target.value)} disabled={busy}><option value="">{copy.personal}</option>{organizations.map(item => <option key={item.organization_id} value={item.organization_id}>{copy.organizationIdentity(item.name)}</option>)}</select></label></div><div className="ap-metrics"><article><span>{copy.organizations}</span><strong>{organizations.length}</strong></article><article><span>{copy.availableCredits}</span><strong>{formatPoints(balance?.available_microcredits, language)}</strong></article><article><span>{copy.accountStatus}</span><strong>{me?.status || '—'}</strong></article></div></section>;
}

function Organization({ copy, organization, units, members, busy, onCreateUnit, onInvite }) {
  const [unitName, setUnitName] = useState(''); const [parentId, setParentId] = useState(''); const [email, setEmail] = useState('');
  if (!organization) return <Empty>{copy.chooseOrganization}</Empty>;
  return <section><p className="ap-eyebrow">{copy.sections.organization}</p><h2>{organization.name}</h2><div className="ap-grid"><article className="ap-card"><h3>{copy.organizationTree}</h3>{units.length ? <ul className="ap-list">{units.map(unit => <li key={unit.unit_id}><span>{unit.name}</span><small>{unit.parent_id ? copy.childUnit : copy.rootUnit}</small></li>)}</ul> : <Empty>{copy.noUnits}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); if (unitName.trim()) onCreateUnit(unitName.trim(), parentId || null).then?.(() => setUnitName('')); }}><Field label={copy.unitName}><input value={unitName} onChange={event => setUnitName(event.target.value)} /></Field><Field label={copy.parentUnit}><select value={parentId} onChange={event => setParentId(event.target.value)}><option value="">{copy.rootUnit}</option>{units.map(unit => <option key={unit.unit_id} value={unit.unit_id}>{unit.name}</option>)}</select></Field><button disabled={busy || !unitName.trim()}>{copy.createUnit}</button></form></article><article className="ap-card"><h3>{copy.members}</h3>{members.length ? <ul className="ap-list">{members.map(member => <li key={member.user_id}><span>{member.display_name || member.email || member.user_id}</span><small>{member.roles.join(' · ')}</small></li>)}</ul> : <Empty>{copy.noMembers}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); if (email.trim()) onInvite(email.trim(), ['MEMBER']).then?.(() => setEmail('')); }}><Field label={copy.memberEmail}><input type="email" value={email} onChange={event => setEmail(event.target.value)} /></Field><button disabled={busy || !email.trim()}>{copy.createInvitation}</button></form></article></div></section>;
}

function Wallet({ copy, language, products, balance, packages, personalContext, billingUnavailable, detailsUnavailable, order, qrData, busy, onRecharge }) {
  const [productId, setProductId] = useState(''); const [channel, setChannel] = useState('ALIPAY');
  const [now, setNow] = useState(Date.now());
  useEffect(() => { if (!products.some(item => item.product_id === productId) && products[0]) setProductId(products[0].product_id); }, [products, productId]);
  useEffect(() => {
    if (!order || terminalOrderStates.has(order.status)) return undefined;
    setNow(Date.now());
    const timer = window.setInterval(() => { const current = Date.now(); setNow(current); if (new Date(order.expires_at).getTime() <= current) window.clearInterval(timer); }, 1000);
    return () => window.clearInterval(timer);
  }, [order?.order_id, order?.status]);
  const selected = products.find(item => item.product_id === productId);
  const hasPlans = personalContext && products.some(item => item.plan_name && item.validity_days > 0);
  const remainingSeconds = order ? Math.max(0, Math.ceil((new Date(order.expires_at).getTime() - now) / 1000)) : 0;
  const displayStatus = order && remainingSeconds === 0 && !terminalOrderStates.has(order.status) ? 'EXPIRED' : order?.status;
  const canScan = Boolean(order && !terminalOrderStates.has(order.status) && remainingSeconds > 0);
  const channels = selected?.channels?.length ? selected.channels : ['UNIONPAY', 'ALIPAY', 'WECHAT'];
  useEffect(() => { if (!channels.includes(channel)) setChannel(channels[0]); }, [channel, channels]);
  const countdown = `${String(Math.floor(remainingSeconds / 60)).padStart(2, '0')}:${String(remainingSeconds % 60).padStart(2, '0')}`;
  return <section><p className="ap-eyebrow">{copy.sections.wallet}</p><h2>{hasPlans ? copy.walletTitle : copy.rechargeTitle}</h2>{hasPlans && <p className="ap-lead">{copy.walletLead}</p>}{billingUnavailable && <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div>}<div className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{formatPoints(balance?.available_microcredits, language)}</strong><small>{copy.reservedCredits}: {formatPoints(balance?.reserved_microcredits, language)}</small></div><div className="ap-card"><h3>{hasPlans ? copy.availablePlans : copy.selectAmount}</h3><div className="ap-product-grid">{products.map(product => <button key={product.product_id} className={productId === product.product_id ? 'selected' : ''} disabled={busy} aria-pressed={productId === product.product_id} onClick={() => setProductId(product.product_id)}><strong>{product.plan_name || formatMoney(product.amount_fen, language)}</strong><span>{product.plan_name && <>{formatMoney(product.amount_fen, language)} · </>}{formatPoints(String(product.display_points) + '0000', language)} {copy.points}</span><small>{product.validity_days > 0 ? copy.validityDays(product.validity_days) : ''}{product.price_status === 'UAT_TENTATIVE' ? ` · ${copy.tentativePrice}` : ''}</small></button>)}</div><div className="ap-inline"><select disabled={busy} value={channel} onChange={event => setChannel(event.target.value)}>{channels.map(item => <option key={item} value={item}>{copy.channels[item] || item}</option>)}</select><button className="ap-primary" disabled={busy || billingUnavailable || !selected} onClick={() => onRecharge(productId, channel)}>{copy.createRecharge}</button></div>{order && <div className="ap-payment"><div>{canScan && qrData && <img src={qrData} alt={copy.paymentQR} />}</div><div><span className="ap-status">{copy.orderStatuses[displayStatus] || displayStatus}</span><h3>{formatMoney(order.amount_fen, language)}{canScan && <> · {copy.scanToPay}</>}</h3><p>{canScan ? copy.countdown(countdown) : copy.orderNoLongerPayable}</p>{canScan && /^https?:\/\//u.test(order.payment_url || '') && <a href={order.payment_url} target="_blank" rel="noreferrer">{copy.openPayment}</a>}</div></div>}</div>{personalContext && <div className="ap-card ap-entitlements"><h3>{copy.myEntitlements}</h3>{detailsUnavailable ? <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div> : packages.items?.length ? <div className="ap-order-list">{packages.items.map(item => <article className="ap-trace-card" key={item.package_id}><header><div><strong>{item.plan_name}</strong><small>{item.sku_code}</small></div><div><strong>{formatPoints(item.points_remaining_microcredits, language)} {copy.points}</strong><span className="ap-status">{copy.packageStatuses[item.status] || item.status}</span></div></header><TraceDetails items={[
    [copy.packageId, item.package_id],
    [copy.pinvouOrder, item.pinvou_order_id],
    [copy.mallOrder, item.mall_order_id],
    [copy.amount, formatMoney(item.amount_fen, language)],
    [copy.creditedAt, formatUsageTime(item.credited_at, language)],
    [copy.validPeriod, `${formatUsageTime(item.valid_from, language)} — ${formatUsageTime(item.valid_until, language)}`],
    [copy.totalPoints, `${formatPoints(item.points_total_microcredits, language)} ${copy.points}`],
    [copy.usedPoints, `${formatPoints(item.points_used_microcredits, language)} ${copy.points}`],
  ]} /></article>)}</div> : <Empty>{copy.noEntitlements}</Empty>}</div>}</section>;
}

function Orders({ copy, language, orders, unavailable }) {
  return <section><p className="ap-eyebrow">{copy.sections.orders}</p><h2>{copy.ordersTitle}</h2><p className="ap-lead">{copy.ordersLead}</p>{unavailable ? <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div> : orders.length ? <div className="ap-order-list">{orders.map(item => <article className="ap-trace-card" key={item.order_id}><header><div><strong>{item.plan_name}</strong><small>{item.sku_code} · {copy.channels[item.channel] || item.channel}</small></div><div><strong>{formatMoney(item.amount_fen, language)}</strong><span className="ap-status">{copy.orderStatuses[item.status] || item.status}</span></div></header><TraceDetails items={[
    [copy.pinvouOrder, item.pinvou_order_id || item.order_id],
    [copy.mallOrder, item.mall_order_id],
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

function Invoice({ copy }) {
  return <section><p className="ap-eyebrow">{copy.sections.invoice}</p><h2>{copy.invoiceTitle}</h2><p className="ap-lead">{copy.invoiceLead}</p><div className="ap-card"><h3>{copy.invoiceUnavailableTitle}</h3><p className="ap-muted">{copy.invoiceUnavailable}</p></div></section>;
}

function Tokens({ copy, language, client, onError, me, organization, tokens, balance, billingUnavailable, busy, run: runAction, reload }) {
  const [name, setName] = useState(''); const [selected, setSelected] = useState(''); const [baseURL, setBaseURL] = useState(''); const [secret, setSecret] = useState(''); const [models, setModels] = useState(''); const [credentials, setCredentials] = useState([]); const [model, setModel] = useState(''); const [eligible, setEligible] = useState(null); const [chosen, setChosen] = useState(''); const [usageID, setUsageID] = useState(''); const [usageStatus, setUsageStatus] = useState(null);
  const lifeRef = useRef({ generation: 0, active: false });
  useEffect(() => {
    const life = lifeRef.current; life.generation += 1; life.active = true;
    return () => { life.active = false; life.generation += 1; };
  }, [client, selected, organization?.organization_id, me?.active_context?.context_type]);
  const run = action => {
    const generation = lifeRef.current.generation;
    return runAction(async parentCurrent => {
      const isCurrent = () => parentCurrent() && lifeRef.current.active && lifeRef.current.generation === generation;
      try { await action(isCurrent); } catch (reason) { if (isCurrent()) throw reason; }
    });
  };
  const active = tokens.find(item => item.token_account_id === selected);
  useEffect(() => { if (!tokens.some(item => item.token_account_id === selected)) setSelected(tokens[0]?.token_account_id || ''); }, [tokens, selected]);
  useEffect(() => { setEligible(null); setChosen(''); setModel(''); setUsageID(''); setUsageStatus(null); setSecret(''); setBaseURL(''); setModels(''); }, [selected, organization?.organization_id, me?.active_context?.context_type]);
  useEffect(() => {
    setCredentials([]);
    if (!selected || !tokens.some(item => item.token_account_id === selected)) return undefined;
    let current = true;
    const controller = new window.AbortController();
    client.credentials(selected, controller.signal)
      .then(value => { if (current) setCredentials(value.items || []); })
      .catch(reason => { if (current) { setCredentials([]); onError(reason); } });
    return () => { current = false; controller.abort(); };
  }, [client, selected, tokens, onError]);
  const owner = organization ? { owner_type: 'ORG', owner_id: organization.organization_id, owner_organization_id: organization.organization_id, owner_unit_id: null, share_scope: 'ORG_ALL' } : { owner_type: 'USER', owner_id: me.user_id, owner_organization_id: null, owner_unit_id: null, share_scope: 'OWNER_UNIT_ONLY' };
  const usable = eligibleForContext(eligible?.items, organization?.organization_id);
  const ready = chosen && usable.some(item => item.token_account_id === chosen) && !billingUnavailable && balance?.available_microcredits > 0;
  return <section><p className="ap-eyebrow">{copy.sections.tokens}</p><h2>{copy.tokensTitle}</h2><article className="ap-card ap-consumption"><h3>{copy.consumptionReadiness}</h3><div className="ap-inline"><Field label={copy.modelToUse}><input value={model} disabled={busy} onChange={event => { setModel(event.target.value); setEligible(null); setChosen(''); }} placeholder="model-name" /></Field><button disabled={busy || !model.trim()} onClick={() => run(async isCurrent => { const result = await client.eligibleTokens(model.trim()); if (!isCurrent()) return; const items = eligibleForContext(result.items, organization?.organization_id); setEligible(result); if (items.length === 1) setChosen(items[0].token_account_id); })}>{copy.checkAvailability}</button></div><dl className="ap-facts"><div><dt>{copy.paymentWallet}</dt><dd>{balance?.wallet_id || '—'}</dd></div><div><dt>{copy.availableCredits}</dt><dd>{formatPoints(balance?.available_microcredits, language)}</dd></div></dl>{billingUnavailable && <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div>}{balance?.available_microcredits === 0 && <div role="alert" className="ap-alert error">{copy.insufficientBalance}</div>}{eligible && usable.length === 0 && <div role="alert" className="ap-alert error">{copy.noEligibleToken}</div>}{usable.length > 1 && <div className="ap-alert">{copy.chooseExplicitToken}</div>}{usable.length > 0 && <div className="ap-token-list">{usable.map(item => <button className={chosen === item.token_account_id ? 'selected' : ''} aria-pressed={chosen === item.token_account_id} key={item.token_account_id} onClick={() => setChosen(item.token_account_id)}><span>{item.name}</span><small>{item.owner_type} · {item.health_status} · {item.allowed_models.join(', ')}</small></button>)}</div>}{ready && <div className="ap-alert success">{copy.tokenReady}</div>}<h3 className="ap-subheading">{copy.usageStatusTitle}</h3><div className="ap-inline"><Field label={copy.usageRequestID}><input value={usageID} disabled={busy} onChange={event => { setUsageID(event.target.value); setUsageStatus(null); }} placeholder="00000000-0000-0000-0000-000000000000" /></Field><button disabled={busy || !usageID.trim()} onClick={() => run(async isCurrent => { const status = await client.usageRequest(usageID.trim()); if (isCurrent()) setUsageStatus(status); })}>{copy.checkUsageStatus}</button></div>{usageStatus && <div className={`ap-alert ${usageStatus.display_status === 'SETTLED' ? 'success' : ''}`}><strong>{copy.usageStatuses[usageStatus.display_status] || usageStatus.display_status}</strong><br />{usageStatus.model} · {usageStatus.actual_microcredits ?? '—'} / {usageStatus.maximum_microcredits}{usageStatus.display_status === 'SETTLEMENT_PENDING' && <><br />{copy.settlementPendingHelp}</>}</div>}</article><div className="ap-grid"><article className="ap-card"><h3>{copy.managedTokens}</h3>{tokens.length ? <div className="ap-token-list">{tokens.map(token => <button disabled={busy} className={selected === token.token_account_id ? 'selected' : ''} aria-pressed={selected === token.token_account_id} key={token.token_account_id} onClick={() => setSelected(token.token_account_id)}><span>{token.name}</span><small>{token.status} · {token.share_scope}</small></button>)}</div> : <Empty>{copy.noTokens}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); run(async isCurrent => { const created = await client.createTokenAccount({ ...owner, name: name.trim(), default_priority: 100 }); if (!isCurrent()) return; setName(''); await reload(); if (isCurrent()) setSelected(created.token_account_id); }); }}><Field label={copy.tokenName}><input value={name} onChange={event => setName(event.target.value)} maxLength={120} /></Field><button disabled={busy || !name.trim()}>{copy.createToken}</button></form></article><article className="ap-card"><h3>{copy.credentials}</h3>{active && <><ul className="ap-list">{credentials.map(item => <li key={item.credential_id}><span>{item.provider}</span><small>{item.status} · v{item.version} · {item.fingerprint.slice(0, 12)}</small></li>)}</ul><form className="ap-stack" onSubmit={event => { event.preventDefault(); run(async isCurrent => { const submittedSecret = secret; setSecret(''); await client.rotateCredential(active.token_account_id, { provider: 'new-api', base_url: baseURL.trim(), secret: submittedSecret, allowed_models: models.split(',').map(value => value.trim()).filter(Boolean) }); if (!isCurrent()) return; const result = await client.credentials(active.token_account_id); if (isCurrent()) setCredentials(result.items || []); }); }}><Field label={copy.providerURL}><input type="url" value={baseURL} onChange={event => setBaseURL(event.target.value)} /></Field><Field label={copy.providerSecret}><input type="password" autoComplete="new-password" value={secret} onChange={event => setSecret(event.target.value)} /></Field><Field label={copy.allowedModels}><input value={models} onChange={event => setModels(event.target.value)} placeholder="model-a, model-b" /></Field><button disabled={busy || !baseURL.trim() || secret.length < 8 || !models.trim()}>{copy.rotateCredential}</button></form></>}</article></div></section>;
}

function formatUsageTime(value, language) {
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language] || language;
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
}

function Usage({ copy, language, records, unavailable, hasMore, busy, onLoadMore }) {
  if (unavailable) return <section><p className="ap-eyebrow">{copy.sections.usage}</p><h2>{copy.usageTitle}</h2><p className="ap-lead">{copy.usageLead}</p><div role="alert" className="ap-alert error">{copy.usageUnavailable}</div></section>;
  return <section><p className="ap-eyebrow">{copy.sections.usage}</p><h2>{copy.usageTitle}</h2><p className="ap-lead">{copy.usageLead}</p>{records.length ? <div className="ap-usage-list">{records.map((record, index) => {
    const consumed = record.actual_microcredits == null && record.display_status !== 'RELEASED' ? '—' : formatPoints(record.actual_microcredits || 0, language);
    const statusClass = String(record.display_status || '').toLowerCase().replaceAll('_', '-');
    const tokenKnown = record.input_tokens != null && record.output_tokens != null;
    return <article className="ap-usage-row" key={record.usage_request_id || `${record.started_at}-${record.model}-${index}`}><div><strong>{record.model || copy.unknownModel}</strong><small>{formatUsageTime(record.started_at, language)}</small>{tokenKnown && <small className="ap-usage-tokens">{copy.inputTokens}: {record.input_tokens.toLocaleString()} · {copy.outputTokens}: {record.output_tokens.toLocaleString()} · {copy.cachedTokens}: {(record.cached_input_tokens || 0).toLocaleString()}</small>}<small title={record.usage_request_id}>{copy.usageRecord}: {String(record.usage_request_id || '—').slice(0, 8)}</small></div><div><strong className="ap-usage-amount">{consumed} {copy.points}</strong><span className={`ap-status ap-usage-status-${statusClass}`}>{copy.usageStatuses[record.display_status] || record.display_status}</span>{record.completed_at && <small>{copy.completedAt}: {formatUsageTime(record.completed_at, language)}</small>}</div></article>;
  })}{hasMore && <button className="ap-usage-more" disabled={busy} onClick={onLoadMore}>{busy ? copy.loadingMore : copy.loadMore}</button>}</div> : <Empty>{copy.noUsage}</Empty>}</section>;
}
