import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createContextRequestGuard, eligibleForContext, rechargePollDelay } from './context-guard.js';
import { formatPoints } from './credit-units.js';
import './account-platform.css';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'PARTIALLY_REFUNDED', 'REFUNDED', 'MANUAL_REVIEW']);
const idempotencyKey = prefix => `${prefix}-${globalThis.crypto.randomUUID()}`;

function Field({ label, children }) {
  return <label className="ap-field"><span>{label}</span>{children}</label>;
}

function Empty({ children }) { return <div className="ap-empty">{children}</div>; }

function NavIcon({ name }) {
  const paths = {
    overview: <><rect x="3" y="3" width="7" height="7" rx="2" /><rect x="14" y="3" width="7" height="7" rx="2" /><rect x="3" y="14" width="7" height="7" rx="2" /><rect x="14" y="14" width="7" height="7" rx="2" /></>,
    organization: <><circle cx="12" cy="7" r="3" /><circle cx="6" cy="17" r="2.5" /><circle cx="18" cy="17" r="2.5" /><path d="M12 10v3M8 15l4-2 4 2" /></>,
    wallet: <><rect x="3" y="5" width="18" height="14" rx="3" /><path d="M3 9h18M16 14h2" /></>,
    tokens: <><circle cx="8" cy="12" r="4" /><path d="M12 12h9M17 12v3M20 12v2" /></>,
  };
  return <svg className="ap-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[name]}</svg>;
}

function localizedError(copy, reason) {
  const raw = String(reason?.code || reason?.message || reason || '');
  const code = raw.split(':', 1)[0].trim();
  return copy.errors?.[code] || (reason?.message && typeof reason.message === 'string' ? reason.message : copy.requestFailed);
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
  const [section, setSection] = useState('overview');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [order, setOrder] = useState(null);
  const [qrData, setQrData] = useState('');
  const guardRef = useRef(createContextRequestGuard());
  const activeOrganizationId = me?.active_context?.organization_id || '';
  const activeOrganization = organizations.find(item => item.organization_id === activeOrganizationId);

  const loadContext = async (nextMe = me) => {
    const request = guardRef.current.begin();
    setError('');
    const organizationId = nextMe?.active_context?.organization_id;
    try {
      const results = await Promise.allSettled([
        client.organizations(request.signal), client.products(request.signal), client.balance(request.signal), client.managedTokens(request.signal),
        client.recentRechargeOrder(request.signal),
        organizationId ? client.units(organizationId, request.signal) : Promise.resolve({ items: [] }),
        organizationId ? client.members(organizationId, request.signal) : Promise.resolve({ items: [] }),
      ]);
      if (!guardRef.current.isCurrent(request.generation)) return;
      const value = index => results[index].status === 'fulfilled' ? results[index].value : null;
      setOrganizations(value(0)?.items || []);
      setProducts(value(1)?.items || []);
      setBalance(value(2));
      setTokens(value(3)?.items || []);
      setOrder(value(4)?.order || null);
      setUnits(value(5)?.items || []);
      setMembers(value(6)?.items || []);
      const billingFailed = [1, 2, 4].some(index => results[index].status === 'rejected');
      setBillingUnavailable(billingFailed);
      if ([0, 3, 5, 6].some(index => results[index].status === 'rejected')) setError(copy.requestFailed);
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
        setMe(account);
        setAuth('ready');
        await loadContext(account);
      } catch {
        if (active) { setAuth('error'); setError(copy.requestFailed); }
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
    try { await action(); } catch (reason) { setError(localizedError(copy, reason)); }
    finally { setBusy(false); }
  };

  const login = () => run(async () => {
    await client.login();
    const account = await client.bootstrap();
    setMe(account);
    setAuth('ready');
    await loadContext(account);
  });

  const logout = () => run(async () => {
    await client.logout();
    guardRef.current.cancel();
    setMe(null); setOrganizations([]); setUnits([]); setMembers([]); setProducts([]);
    setBalance(null); setTokens([]); setOrder(null); setQrData(''); setAuth('guest');
    if (portal) window.location.reload();
  });

  const switchContext = value => run(async () => {
    guardRef.current.cancel();
    setBalance(null); setTokens([]); setUnits([]); setMembers([]); setOrder(null); setBillingUnavailable(false);
    const organizationId = value || null;
    const context = await client.switchContext(organizationId ? 'ORGANIZATION' : 'PERSONAL', organizationId);
    const updated = { ...me, active_context: context };
    setMe(updated);
    await loadContext(updated);
  });

  if (auth === 'loading') return <div className={`ap-shell ap-centered ${portal ? 'ap-portal' : 'ap-desktop'}`}><div className="ap-spinner" />{copy.loading}</div>;
  if (auth === 'guest' || auth === 'error') return <div className={`ap-shell ap-login ${portal ? 'ap-portal' : 'ap-desktop'}`}><div><p className="ap-eyebrow">PINVOU ACCOUNT PLATFORM</p><h1>{copy.title}</h1><p>{portal ? copy.loginIntro : copy.desktopLoginIntro}</p>{error && <div role="alert" className="ap-alert error">{error}</div>}{portal ? <a className="ap-primary" href="/auth/login">{copy.login}</a> : <button className="ap-primary" disabled={busy} onClick={login}>{busy ? copy.signingIn : copy.login}</button>}{onClose && <button className="ap-close" onClick={onClose}>{copy.close}</button>}</div></div>;

  const userLabel = me?.display_name || copy.user;
  const identityLabel = activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal;

  return <div className={`ap-shell ap-app ${portal ? 'ap-portal' : 'ap-desktop'}`}>
    <aside className="ap-sidebar">
      <div className="ap-sidebar-brand"><span className="ap-brand-mark">P</span><div><p className="ap-eyebrow">PINVOU</p><h1>{copy.title}</h1></div></div>
      <div className="ap-user-summary"><span className="ap-user-avatar">{userLabel.trim().slice(0, 1).toUpperCase()}</span><div><strong>{userLabel}</strong><span>{identityLabel}</span></div></div>
      <nav>{['overview', 'organization', 'wallet', 'tokens'].map(key => <button key={key} className={section === key ? 'active' : ''} onClick={() => setSection(key)}><NavIcon name={key} /><span>{copy.sections[key]}</span></button>)}</nav>
      <div className="ap-sidebar-foot">
        <select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}><option value="zh">简体中文</option><option value="en">English</option><option value="ja">日本語</option></select>
        <button onClick={logout}>{copy.logout}</button>
        {onClose && <button onClick={onClose}>{copy.close}</button>}
      </div>
    </aside>
    <main className="ap-main">
      {error && <div role="alert" className="ap-alert error">{error}</div>}{notice && <div className="ap-alert success">{notice}</div>}
      {section === 'overview' && <Overview copy={copy} me={me} organizations={organizations} tokens={tokens} activeOrganization={activeOrganization} activeOrganizationId={activeOrganizationId} busy={busy} onSwitchContext={switchContext} />}
      {section === 'organization' && <Organization copy={copy} organization={activeOrganization} units={units} members={members} busy={busy} onCreateUnit={(name, parentId) => run(async () => { await client.createUnit(activeOrganizationId, name, parentId); await loadContext(me); })} onInvite={(email, roles) => run(async () => { const invite = await client.invite(activeOrganizationId, email, roles); setNotice(`${copy.invitationCreated} ${invite.token}`); })} />}
      {section === 'wallet' && <Wallet copy={copy} language={language} products={products} balance={balance} billingUnavailable={billingUnavailable} order={order} qrData={qrData} busy={busy} onRecharge={(productId, channel) => run(async () => setOrder(await client.createRecharge(productId, channel, idempotencyKey('recharge'))))} />}
      {section === 'tokens' && <Tokens copy={copy} language={language} client={client} me={me} organization={activeOrganization} tokens={tokens} balance={balance} billingUnavailable={billingUnavailable} busy={busy} run={run} reload={() => loadContext(me)} />}
    </main>
  </div>;
}

function Overview({ copy, me, organizations, tokens, activeOrganization, activeOrganizationId, busy, onSwitchContext }) {
  return <section><p className="ap-eyebrow">{copy.sections.overview}</p><h2>{copy.welcome(me?.display_name || copy.user)}</h2><p className="ap-lead">{copy.overviewLead}</p><div className="ap-context-card"><div><span>{copy.activeContext}</span><strong>{activeOrganization ? copy.organizationIdentity(activeOrganization.name) : copy.personal}</strong><small>{copy.identityHelp}</small></div><label><span>{copy.switchContext}</span><select value={activeOrganizationId} onChange={event => onSwitchContext(event.target.value)} disabled={busy}><option value="">{copy.personal}</option>{organizations.map(item => <option key={item.organization_id} value={item.organization_id}>{copy.organizationIdentity(item.name)}</option>)}</select></label></div><div className="ap-metrics"><article><span>{copy.organizations}</span><strong>{organizations.length}</strong></article><article><span>{copy.managedTokens}</span><strong>{tokens.length}</strong></article><article><span>{copy.accountStatus}</span><strong>{me?.status || '—'}</strong></article></div></section>;
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

function Tokens({ copy, language, client, me, organization, tokens, balance, billingUnavailable, busy, run, reload }) {
  const [name, setName] = useState(''); const [selected, setSelected] = useState(''); const [baseURL, setBaseURL] = useState(''); const [secret, setSecret] = useState(''); const [models, setModels] = useState(''); const [credentials, setCredentials] = useState([]); const [model, setModel] = useState(''); const [eligible, setEligible] = useState(null); const [chosen, setChosen] = useState(''); const [usageID, setUsageID] = useState(''); const [usageStatus, setUsageStatus] = useState(null);
  const active = tokens.find(item => item.token_account_id === selected);
  useEffect(() => { if (!selected && tokens[0]) setSelected(tokens[0].token_account_id); }, [tokens, selected]);
  useEffect(() => { setEligible(null); setChosen(''); setModel(''); setUsageID(''); setUsageStatus(null); }, [organization?.organization_id, me?.active_context?.context_type]);
  useEffect(() => { if (!selected) { setCredentials([]); return; } const controller = new window.AbortController(); client.credentials(selected, controller.signal).then(value => setCredentials(value.items || [])).catch(() => setCredentials([])); return () => controller.abort(); }, [client, selected]);
  const owner = organization ? { owner_type: 'ORG', owner_id: organization.organization_id, owner_organization_id: organization.organization_id, owner_unit_id: null, share_scope: 'ORG_ALL' } : { owner_type: 'USER', owner_id: me.user_id, owner_organization_id: null, owner_unit_id: null, share_scope: 'OWNER_UNIT_ONLY' };
  const usable = eligibleForContext(eligible?.items, organization?.organization_id);
  const ready = chosen && usable.some(item => item.token_account_id === chosen) && !billingUnavailable && balance?.available_microcredits > 0;
  return <section><p className="ap-eyebrow">{copy.sections.tokens}</p><h2>{copy.tokensTitle}</h2><article className="ap-card ap-consumption"><h3>{copy.consumptionReadiness}</h3><div className="ap-inline"><Field label={copy.modelToUse}><input value={model} onChange={event => { setModel(event.target.value); setEligible(null); setChosen(''); }} placeholder="model-name" /></Field><button disabled={busy || !model.trim()} onClick={() => run(async () => { const result = await client.eligibleTokens(model.trim()); const items = eligibleForContext(result.items, organization?.organization_id); setEligible(result); if (items.length === 1) setChosen(items[0].token_account_id); })}>{copy.checkAvailability}</button></div><dl className="ap-facts"><div><dt>{copy.paymentWallet}</dt><dd>{balance?.wallet_id || '—'}</dd></div><div><dt>{copy.availableCredits}</dt><dd>{formatPoints(balance?.available_microcredits, language)}</dd></div></dl>{billingUnavailable && <div role="alert" className="ap-alert error">{copy.billingUnavailable}</div>}{balance?.available_microcredits === 0 && <div role="alert" className="ap-alert error">{copy.insufficientBalance}</div>}{eligible && usable.length === 0 && <div role="alert" className="ap-alert error">{copy.noEligibleToken}</div>}{usable.length > 1 && <div className="ap-alert">{copy.chooseExplicitToken}</div>}{usable.length > 0 && <div className="ap-token-list">{usable.map(item => <button className={chosen === item.token_account_id ? 'selected' : ''} key={item.token_account_id} onClick={() => setChosen(item.token_account_id)}><span>{item.name}</span><small>{item.owner_type} · {item.health_status} · {item.allowed_models.join(', ')}</small></button>)}</div>}{ready && <div className="ap-alert success">{copy.tokenReady}</div>}<h3 className="ap-subheading">{copy.usageStatusTitle}</h3><div className="ap-inline"><Field label={copy.usageRequestID}><input value={usageID} onChange={event => { setUsageID(event.target.value); setUsageStatus(null); }} placeholder="00000000-0000-0000-0000-000000000000" /></Field><button disabled={busy || !usageID.trim()} onClick={() => run(async () => setUsageStatus(await client.usageRequest(usageID.trim())))}>{copy.checkUsageStatus}</button></div>{usageStatus && <div className={`ap-alert ${usageStatus.display_status === 'SETTLED' ? 'success' : ''}`}><strong>{copy.usageStatuses[usageStatus.display_status] || usageStatus.display_status}</strong><br />{usageStatus.model} · {formatPoints(usageStatus.actual_microcredits, language)} / {formatPoints(usageStatus.maximum_microcredits, language)} {copy.points}{usageStatus.display_status === 'SETTLEMENT_PENDING' && <><br />{copy.settlementPendingHelp}</>}</div>}</article><div className="ap-grid"><article className="ap-card"><h3>{copy.managedTokens}</h3>{tokens.length ? <div className="ap-token-list">{tokens.map(token => <button className={selected === token.token_account_id ? 'selected' : ''} key={token.token_account_id} onClick={() => setSelected(token.token_account_id)}><span>{token.name}</span><small>{token.status} · {token.share_scope}</small></button>)}</div> : <Empty>{copy.noTokens}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); run(async () => { const created = await client.createTokenAccount({ ...owner, name: name.trim(), default_priority: 100 }); setName(''); await reload(); setSelected(created.token_account_id); }); }}><Field label={copy.tokenName}><input value={name} onChange={event => setName(event.target.value)} maxLength={120} /></Field><button disabled={busy || !name.trim()}>{copy.createToken}</button></form></article><article className="ap-card"><h3>{copy.credentials}</h3>{active && <><ul className="ap-list">{credentials.map(item => <li key={item.credential_id}><span>{item.provider}</span><small>{item.status} · v{item.version} · {item.fingerprint.slice(0, 12)}</small></li>)}</ul><form className="ap-stack" onSubmit={event => { event.preventDefault(); run(async () => { await client.rotateCredential(active.token_account_id, { provider: 'new-api', base_url: baseURL.trim(), secret, allowed_models: models.split(',').map(value => value.trim()).filter(Boolean) }); setSecret(''); setCredentials((await client.credentials(active.token_account_id)).items || []); }); }}><Field label={copy.providerURL}><input type="url" value={baseURL} onChange={event => setBaseURL(event.target.value)} /></Field><Field label={copy.providerSecret}><input type="password" autoComplete="new-password" value={secret} onChange={event => setSecret(event.target.value)} /></Field><Field label={copy.allowedModels}><input value={models} onChange={event => setModels(event.target.value)} placeholder="model-a, model-b" /></Field><button disabled={busy || !baseURL.trim() || secret.length < 8 || !models.trim()}>{copy.rotateCredential}</button></form></>}</article></div></section>;
}
