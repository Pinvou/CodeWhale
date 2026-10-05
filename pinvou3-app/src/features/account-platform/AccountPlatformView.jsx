import React, { useCallback, useEffect, useRef, useState } from 'react';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from './context-guard.js';
import { accountErrorMessage, needsAccountLogin } from './errors.js';
import { formatPoints } from './credit-units.js';
import './account-platform.css';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'PARTIALLY_REFUNDED', 'REFUNDED', 'MANUAL_REVIEW']);
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
  const [tokens, setTokens] = useState([]);
  const [usageRecords, setUsageRecords] = useState([]);
  const [usageCursor, setUsageCursor] = useState(null);
  const [usageUnavailable, setUsageUnavailable] = useState(false);
  const [section, setSection] = useState('overview');
  const [busy, setBusy] = useState(false);
  const [loginAccount, setLoginAccount] = useState('');
  const [loginPassword, setLoginPassword] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [order, setOrder] = useState(null);
  const [qrData, setQrData] = useState('');
  const guardRef = useRef(createContextRequestGuard());
  const lifecycleRef = useRef({ generation: 0, active: false });
  const mutationRef = useRef(false);
  useEffect(() => {
    const life = lifecycleRef.current;
    life.generation += 1; life.active = true;
    return () => { life.active = false; life.generation += 1; guardRef.current.cancel(); };
  }, [client]);
  const activeOrganizationId = me?.active_context?.organization_id || '';
  const activeOrganization = organizations.find(item => item.organization_id === activeOrganizationId);

  const fail = useCallback(reason => {
    if (needsAccountLogin(reason)) {
      guardRef.current.cancel();
      setMe(null); setOrganizations([]); setBalance(null); setTokens([]); setUsageRecords([]); setUsageCursor(null); setUsageUnavailable(false);
      setUnits([]); setMembers([]); setProducts([]); setOrder(null);
      setBillingUnavailable(false); setNotice(''); setError('');
      setAuth('guest');
      return;
    }
    setError(accountErrorMessage(reason, copy));
  }, [copy]);

  const loadContext = async (nextMe = me) => {
    const request = guardRef.current.begin();
    setError('');
    const organizationId = nextMe?.active_context?.organization_id;
    try {
      const results = await Promise.allSettled([
        client.organizations(request.signal), client.products(request.signal), client.balance(request.signal),
        client.recentRechargeOrder(request.signal),
        organizationId ? client.units(organizationId, request.signal) : Promise.resolve({ items: [] }),
        organizationId ? client.members(organizationId, request.signal) : Promise.resolve({ items: [] }),
        client.usageHistory('', request.signal), client.managedTokens(request.signal),
      ]);
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
    const paymentURL = order?.payment_url || order?.qr_content || '';
    if (!paymentURL) { setQrData(''); return; }
    let active = true;
    import('qrcode').then(({ default: QRCode }) => QRCode.toDataURL(paymentURL, { width: 240, margin: 2, errorCorrectionLevel: 'M' })).then(value => active && setQrData(value)).catch(() => active && setQrData(''));
    return () => { active = false; };
  }, [order?.payment_url, order?.qr_content]);

  useEffect(() => {
    if (!order?.order_id || terminalOrderStates.has(order.status) || new Date(order.expires_at).getTime() <= Date.now()) return undefined;
    let stopped = false;
    let timer;
    let attempt = 0;
    const poll = async () => {
      try {
        const latest = await client.rechargeOrder(order.order_id);
        if (stopped) return;
        if (latest.status === 'PAID' || latest.status === 'PAID_LATE') {
          const latestBalance = await client.balance();
          if (stopped) return;
          setOrder(latest);
          setBalance(latestBalance);
          setNotice(copy.paymentReceived);
          return;
        }
        setOrder(latest);
        if (!terminalOrderStates.has(latest.status) && new Date(latest.expires_at).getTime() > Date.now()) {
          timer = setTimeout(poll, rechargePollDelay(attempt++));
        }
      } catch (reason) {
        if (stopped) return;
        if (needsAccountLogin(reason)) { fail(reason); return; }
        timer = setTimeout(poll, rechargePollDelay(attempt++));
      }
    };
    timer = setTimeout(poll, rechargePollDelay(attempt++));
    return () => { stopped = true; clearTimeout(timer); };
  }, [order?.order_id, order?.status, me?.active_context?.context_type, activeOrganizationId, fail]);

  const run = async action => {
    if (mutationRef.current) return;
    mutationRef.current = true;
    const generation = lifecycleRef.current.generation;
    const isCurrent = () => lifecycleRef.current.active && lifecycleRef.current.generation === generation;
    setBusy(true); setError(''); setNotice('');
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
    setBalance(null); setTokens([]); setUsageRecords([]); setUsageCursor(null); setUsageUnavailable(false); setOrder(null); setQrData(''); setBillingUnavailable(false); setAuth('guest');
    if (portal) window.location.reload();
  });

  const switchContext = value => run(async isCurrent => {
    guardRef.current.cancel();
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
      <nav>{['overview', 'organization', 'wallet', 'usage', 'tokens'].map(key => <button key={key} className={section === key ? 'active' : ''} onClick={() => setSection(key)}><NavIcon name={key} /><span>{copy.sections[key]}</span></button>)}</nav>
      <div className="ap-sidebar-foot">
        <select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}><option value="zh">简体中文</option><option value="en">English</option><option value="ja">日本語</option></select>
        <button disabled={busy} onClick={logout}>{copy.logout}</button>
        {onClose && <button disabled={busy} onClick={onClose}>{copy.close}</button>}
      </div>
    </aside>
    <main className="ap-main">
      {error && <div role="alert" className="ap-alert error">{error}</div>}{notice && <div className="ap-alert success">{notice}</div>}
      {section === 'overview' && <Overview copy={copy} language={language} me={me} organizations={organizations} balance={balance} activeOrganization={activeOrganization} activeOrganizationId={activeOrganizationId} busy={busy} onSwitchContext={switchContext} />}
      {section === 'organization' && <Organization copy={copy} organization={activeOrganization} units={units} members={members} busy={busy} onCreateUnit={(name, parentId) => run(async isCurrent => { await client.createUnit(activeOrganizationId, name, parentId); if (isCurrent()) await loadContext(me); })} onInvite={(email, roles) => run(async isCurrent => { const invite = await client.invite(activeOrganizationId, email, roles); if (isCurrent()) setNotice(`${copy.invitationCreated} ${invite.token}`); })} />}
      {section === 'wallet' && <Wallet copy={copy} language={language} products={products} balance={balance} billingUnavailable={billingUnavailable} order={order} qrData={qrData} busy={busy} onRecharge={(productId, channel) => run(async isCurrent => { const nextOrder = await client.createRecharge(productId, channel, idempotencyKey('recharge')); if (isCurrent()) setOrder(nextOrder); })} />}
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

function Wallet({ copy, language, products, balance, billingUnavailable, order, qrData, busy, onRecharge }) {
  const [productId, setProductId] = useState(''); const [channel, setChannel] = useState('ALIPAY');
  const [now, setNow] = useState(Date.now());
  useEffect(() => { if (!productId && products[0]) setProductId(products[0].product_id); }, [products, productId]);
  useEffect(() => {
    if (!order || terminalOrderStates.has(order.status)) return undefined;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [order?.order_id, order?.status]);
  const selected = products.find(item => item.product_id === productId);
  const remainingSeconds = order ? Math.max(0, Math.ceil((new Date(order.expires_at).getTime() - now) / 1000)) : 0;
  const displayStatus = order && remainingSeconds === 0 && !terminalOrderStates.has(order.status) ? 'EXPIRED' : order?.status;
  const countdown = `${String(Math.floor(remainingSeconds / 60)).padStart(2, '0')}:${String(remainingSeconds % 60).padStart(2, '0')}`;
  return <section><p className="ap-eyebrow">{copy.sections.wallet}</p><h2>{copy.walletTitle}</h2>{billingUnavailable && <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div>}<div className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{formatPoints(balance?.available_microcredits, language)}</strong><small>{copy.reservedCredits}: {formatPoints(balance?.reserved_microcredits, language)}</small></div><div className="ap-card"><div className="ap-product-grid">{products.map(product => <button key={product.product_id} className={productId === product.product_id ? 'selected' : ''} onClick={() => setProductId(product.product_id)}><strong>¥{(product.amount_fen / 100).toFixed(2)}</strong><span>{product.display_points.toLocaleString()} {copy.points}</span></button>)}</div><div className="ap-inline"><select value={channel} onChange={event => setChannel(event.target.value)}>{['UNIONPAY', 'ALIPAY', 'WECHAT'].map(item => <option key={item} value={item}>{copy.channels[item]}</option>)}</select><button className="ap-primary" disabled={busy || billingUnavailable || !selected} onClick={() => onRecharge(productId, channel)}>{copy.createRecharge}</button></div>{order && <div className="ap-payment"><div>{qrData && <img src={qrData} alt={copy.paymentQR} />}</div><div><span className="ap-status">{copy.orderStatuses[displayStatus] || displayStatus}</span><h3>¥{(order.amount_fen / 100).toFixed(2)} · {copy.scanToPay}</h3><p>{remainingSeconds > 0 ? copy.countdown(countdown) : copy.orderExpires(new Date(order.expires_at).toLocaleTimeString())}</p>{remainingSeconds > 0 && (order.payment_url || order.qr_content) && <a href={order.payment_url || order.qr_content} target="_blank" rel="noreferrer">{copy.openPayment}</a>}</div></div>}</div></section>;
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
