import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createContextRequestGuard, rechargePollDelay } from './context-guard.js';
import './account-platform.css';

const terminalOrderStates = new Set(['PAID', 'PAID_LATE', 'CLOSED', 'FAILED', 'PARTIALLY_REFUNDED', 'REFUNDED', 'MANUAL_REVIEW']);
const idempotencyKey = prefix => `${prefix}-${globalThis.crypto.randomUUID()}`;

function Field({ label, children }) {
  return <label className="ap-field"><span>{label}</span>{children}</label>;
}

function Empty({ children }) { return <div className="ap-empty">{children}</div>; }

export function AccountPlatformView({ client, copy, language, onLanguageChange, portal = false }) {
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [organizations, setOrganizations] = useState([]);
  const [units, setUnits] = useState([]);
  const [members, setMembers] = useState([]);
  const [products, setProducts] = useState([]);
  const [balance, setBalance] = useState(null);
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
      const [orgResult, productResult, balanceResult, tokenResult, recentOrderResult, unitResult, memberResult] = await Promise.all([
        client.organizations(request.signal), client.products(request.signal), client.balance(request.signal), client.managedTokens(request.signal),
        client.recentRechargeOrder(request.signal),
        organizationId ? client.units(organizationId, request.signal) : Promise.resolve({ items: [] }),
        organizationId ? client.members(organizationId, request.signal) : Promise.resolve({ items: [] }),
      ]);
      if (!guardRef.current.isCurrent(request.generation)) return;
      setOrganizations(orgResult.items || []);
      setProducts(productResult.items || []);
      setBalance(balanceResult);
      setTokens(tokenResult.items || []);
      setOrder(recentOrderResult.order || null);
      setUnits(unitResult.items || []);
      setMembers(memberResult.items || []);
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
        if (!session.authenticated) { setAuth('guest'); return; }
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
    try { await action(); } catch (reason) { setError(reason?.message || copy.requestFailed); }
    finally { setBusy(false); }
  };

  const switchContext = value => run(async () => {
    guardRef.current.cancel();
    setBalance(null); setTokens([]); setUnits([]); setMembers([]); setOrder(null);
    const organizationId = value || null;
    const context = await client.switchContext(organizationId ? 'ORGANIZATION' : 'PERSONAL', organizationId);
    const updated = { ...me, active_context: context };
    setMe(updated);
    await loadContext(updated);
  });

  if (auth === 'loading') return <div className="ap-centered"><div className="ap-spinner" />{copy.loading}</div>;
  if (auth === 'guest') return <div className="ap-login"><div><p className="ap-eyebrow">PINVOU ACCOUNT PLATFORM</p><h1>{copy.title}</h1><p>{copy.loginIntro}</p><a className="ap-primary" href="/auth/login">{copy.login}</a></div></div>;

  return <div className="ap-app">
    <aside className="ap-sidebar">
      <div><p className="ap-eyebrow">PINVOU</p><h1>{copy.title}</h1></div>
      <nav>{['overview', 'organization', 'wallet', 'tokens'].map(key => <button key={key} className={section === key ? 'active' : ''} onClick={() => setSection(key)}>{copy.sections[key]}</button>)}</nav>
      <div className="ap-sidebar-foot">
        <select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}><option value="zh">简体中文</option><option value="en">English</option><option value="ja">日本語</option></select>
        {portal && <button onClick={() => run(async () => { await client.logout(); window.location.reload(); })}>{copy.logout}</button>}
      </div>
    </aside>
    <main className="ap-main">
      <header className="ap-header"><div><span>{copy.activeContext}</span><select value={activeOrganizationId} onChange={event => switchContext(event.target.value)} disabled={busy}><option value="">{copy.personal}</option>{organizations.map(item => <option key={item.organization_id} value={item.organization_id}>{item.name}</option>)}</select></div><div className="ap-balance"><span>{copy.balance}</span><strong>{balance ? balance.available_microcredits.toLocaleString() : '—'}</strong></div></header>
      {error && <div role="alert" className="ap-alert error">{error}</div>}{notice && <div className="ap-alert success">{notice}</div>}
      {section === 'overview' && <Overview copy={copy} me={me} organizations={organizations} tokens={tokens} activeOrganization={activeOrganization} onCreate={name => run(async () => { await client.createOrganization(name); await loadContext(me); setNotice(copy.organizationCreated); })} busy={busy} />}
      {section === 'organization' && <Organization copy={copy} organization={activeOrganization} units={units} members={members} busy={busy} onCreateUnit={(name, parentId) => run(async () => { await client.createUnit(activeOrganizationId, name, parentId); await loadContext(me); })} onInvite={(email, roles) => run(async () => { const invite = await client.invite(activeOrganizationId, email, roles); setNotice(`${copy.invitationCreated} ${invite.token}`); })} />}
      {section === 'wallet' && <Wallet copy={copy} products={products} balance={balance} order={order} qrData={qrData} busy={busy} onRecharge={(productId, channel) => run(async () => setOrder(await client.createRecharge(productId, channel, idempotencyKey('recharge'))))} />}
      {section === 'tokens' && <Tokens copy={copy} client={client} me={me} organization={activeOrganization} tokens={tokens} busy={busy} run={run} reload={() => loadContext(me)} />}
    </main>
  </div>;
}

function Overview({ copy, me, organizations, tokens, activeOrganization, onCreate, busy }) {
  const [name, setName] = useState('');
  return <section><p className="ap-eyebrow">{copy.sections.overview}</p><h2>{copy.welcome(me?.display_name || copy.user)}</h2><p className="ap-lead">{activeOrganization ? copy.organizationContext(activeOrganization.name) : copy.personalContext}</p><div className="ap-metrics"><article><span>{copy.organizations}</span><strong>{organizations.length}</strong></article><article><span>{copy.managedTokens}</span><strong>{tokens.length}</strong></article><article><span>{copy.accountStatus}</span><strong>{me?.status || '—'}</strong></article></div><form className="ap-inline" onSubmit={event => { event.preventDefault(); if (name.trim()) onCreate(name.trim()).then?.(() => setName('')); }}><input value={name} onChange={event => setName(event.target.value)} placeholder={copy.organizationName} maxLength={120} /><button className="ap-primary" disabled={busy || !name.trim()}>{copy.createOrganization}</button></form></section>;
}

function Organization({ copy, organization, units, members, busy, onCreateUnit, onInvite }) {
  const [unitName, setUnitName] = useState(''); const [parentId, setParentId] = useState(''); const [email, setEmail] = useState('');
  if (!organization) return <Empty>{copy.chooseOrganization}</Empty>;
  return <section><p className="ap-eyebrow">{copy.sections.organization}</p><h2>{organization.name}</h2><div className="ap-grid"><article className="ap-card"><h3>{copy.organizationTree}</h3>{units.length ? <ul className="ap-list">{units.map(unit => <li key={unit.unit_id}><span>{unit.name}</span><small>{unit.parent_id ? copy.childUnit : copy.rootUnit}</small></li>)}</ul> : <Empty>{copy.noUnits}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); if (unitName.trim()) onCreateUnit(unitName.trim(), parentId || null).then?.(() => setUnitName('')); }}><Field label={copy.unitName}><input value={unitName} onChange={event => setUnitName(event.target.value)} /></Field><Field label={copy.parentUnit}><select value={parentId} onChange={event => setParentId(event.target.value)}><option value="">{copy.rootUnit}</option>{units.map(unit => <option key={unit.unit_id} value={unit.unit_id}>{unit.name}</option>)}</select></Field><button disabled={busy || !unitName.trim()}>{copy.createUnit}</button></form></article><article className="ap-card"><h3>{copy.members}</h3>{members.length ? <ul className="ap-list">{members.map(member => <li key={member.user_id}><span>{member.display_name || member.email || member.user_id}</span><small>{member.roles.join(' · ')}</small></li>)}</ul> : <Empty>{copy.noMembers}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); if (email.trim()) onInvite(email.trim(), ['MEMBER']).then?.(() => setEmail('')); }}><Field label={copy.memberEmail}><input type="email" value={email} onChange={event => setEmail(event.target.value)} /></Field><button disabled={busy || !email.trim()}>{copy.createInvitation}</button></form></article></div></section>;
}

function Wallet({ copy, products, balance, order, qrData, busy, onRecharge }) {
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
  return <section><p className="ap-eyebrow">{copy.sections.wallet}</p><h2>{copy.walletTitle}</h2><div className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{balance ? balance.available_microcredits.toLocaleString() : '—'}</strong><small>{copy.reservedCredits}: {balance ? balance.reserved_microcredits.toLocaleString() : '—'}</small></div><div className="ap-card"><div className="ap-product-grid">{products.map(product => <button key={product.product_id} className={productId === product.product_id ? 'selected' : ''} onClick={() => setProductId(product.product_id)}><strong>¥{(product.amount_fen / 100).toFixed(2)}</strong><span>{product.display_points.toLocaleString()} {copy.points}</span></button>)}</div><div className="ap-inline"><select value={channel} onChange={event => setChannel(event.target.value)}>{['UNIONPAY', 'ALIPAY', 'WECHAT'].map(item => <option key={item} value={item}>{copy.channels[item]}</option>)}</select><button className="ap-primary" disabled={busy || !selected} onClick={() => onRecharge(productId, channel)}>{copy.createRecharge}</button></div>{order && <div className="ap-payment"><div>{qrData && <img src={qrData} alt={copy.paymentQR} />}</div><div><span className="ap-status">{copy.orderStatuses[displayStatus] || displayStatus}</span><h3>¥{(order.amount_fen / 100).toFixed(2)} · {copy.scanToPay}</h3><p>{remainingSeconds > 0 ? copy.countdown(countdown) : copy.orderExpires(new Date(order.expires_at).toLocaleTimeString())}</p>{remainingSeconds > 0 && (order.payment_url || order.qr_content) && <a href={order.payment_url || order.qr_content} target="_blank" rel="noreferrer">{copy.openPayment}</a>}</div></div>}</div></section>;
}

function Tokens({ copy, client, me, organization, tokens, busy, run, reload }) {
  const [name, setName] = useState(''); const [selected, setSelected] = useState(''); const [baseURL, setBaseURL] = useState(''); const [secret, setSecret] = useState(''); const [models, setModels] = useState(''); const [credentials, setCredentials] = useState([]);
  const active = tokens.find(item => item.token_account_id === selected);
  useEffect(() => { if (!selected && tokens[0]) setSelected(tokens[0].token_account_id); }, [tokens, selected]);
  useEffect(() => { if (!selected) { setCredentials([]); return; } const controller = new window.AbortController(); client.credentials(selected, controller.signal).then(value => setCredentials(value.items || [])).catch(() => setCredentials([])); return () => controller.abort(); }, [client, selected]);
  const owner = organization ? { owner_type: 'ORG', owner_id: organization.organization_id, owner_organization_id: organization.organization_id, owner_unit_id: null, share_scope: 'ORG_ALL' } : { owner_type: 'USER', owner_id: me.user_id, owner_organization_id: null, owner_unit_id: null, share_scope: 'OWNER_UNIT_ONLY' };
  return <section><p className="ap-eyebrow">{copy.sections.tokens}</p><h2>{copy.tokensTitle}</h2><div className="ap-grid"><article className="ap-card"><h3>{copy.managedTokens}</h3>{tokens.length ? <div className="ap-token-list">{tokens.map(token => <button className={selected === token.token_account_id ? 'selected' : ''} key={token.token_account_id} onClick={() => setSelected(token.token_account_id)}><span>{token.name}</span><small>{token.status} · {token.share_scope}</small></button>)}</div> : <Empty>{copy.noTokens}</Empty>}<form className="ap-stack" onSubmit={event => { event.preventDefault(); run(async () => { const created = await client.createTokenAccount({ ...owner, name: name.trim(), default_priority: 100 }); setName(''); await reload(); setSelected(created.token_account_id); }); }}><Field label={copy.tokenName}><input value={name} onChange={event => setName(event.target.value)} maxLength={120} /></Field><button disabled={busy || !name.trim()}>{copy.createToken}</button></form></article><article className="ap-card"><h3>{copy.credentials}</h3>{active && <><ul className="ap-list">{credentials.map(item => <li key={item.credential_id}><span>{item.provider}</span><small>{item.status} · v{item.version} · {item.fingerprint.slice(0, 12)}</small></li>)}</ul><form className="ap-stack" onSubmit={event => { event.preventDefault(); run(async () => { await client.rotateCredential(active.token_account_id, { provider: 'new-api', base_url: baseURL.trim(), secret, allowed_models: models.split(',').map(value => value.trim()).filter(Boolean) }); setSecret(''); setCredentials((await client.credentials(active.token_account_id)).items || []); }); }}><Field label={copy.providerURL}><input type="url" value={baseURL} onChange={event => setBaseURL(event.target.value)} /></Field><Field label={copy.providerSecret}><input type="password" autoComplete="new-password" value={secret} onChange={event => setSecret(event.target.value)} /></Field><Field label={copy.allowedModels}><input value={models} onChange={event => setModels(event.target.value)} placeholder="model-a, model-b" /></Field><button disabled={busy || !baseURL.trim() || secret.length < 8 || !models.trim()}>{copy.rotateCredential}</button></form></>}</article></div></section>;
}
