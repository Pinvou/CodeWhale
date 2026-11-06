import '../../src/features/account-platform/account-platform.css';
import React, { useEffect, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { PersonalFinanceWorkspace } from '../../src/features/account-platform/PersonalFinanceWorkspace.jsx';
import { OrganizationFinanceWorkspace } from '../../src/features/account-platform/OrganizationFinanceWorkspace.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
const users = { A: '11111111-1111-4111-8111-111111111111', B: '22222222-2222-4222-8222-222222222222' };
const organizations = { A: '44444444-4444-4444-8444-444444444444', B: '55555555-5555-4555-8555-555555555555' };
const control = async action => { const r = await fetch('/__fixture__/control', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(action) }); if (!r.ok) throw new Error('FIXTURE_CONTROL_FAILED'); return r.json(); };
// These child-only updates never rerender the parent or finance sibling.
function Metrics({ client, paid, actor, epoch, allowed, mounted, surface, org, wallet }) {
  const [metrics, setMetrics] = useState(null);
  useEffect(() => {
    let stopped = false;
    const read = async () => { const r = await fetch('/__fixture__/state'); const value = await r.json(); if (!stopped) setMetrics(value); };
    void read(); const timer = setInterval(() => { void read(); }, 250);
    return () => { stopped = true; clearInterval(timer); };
  }, []);
  return <pre data-fixture-metrics>{JSON.stringify({ ...metrics, paidCallbacks: paid, renderedActor: actor, loginEpoch: epoch,
    observedClientEpoch: client.financeSessionEpoch, readPermission: allowed, componentMounted: mounted, surface, organization: organizations[org], walletPermission: wallet }, null, 2)}</pre>;
}
function Fixture() {
  const client = useRef(null); if (!client.current) client.current = new AccountPlatformBffClient();
  const [actor, setActor] = useState('A'), [allowed, setAllowed] = useState(true), [epoch, setEpoch] = useState(0);
  const [mounted, setMounted] = useState(true), [mountKey, setMountKey] = useState(0), [paid, setPaid] = useState(0);
  const [language, setLanguage] = useState('en');
  const [surface, setSurface] = useState('personal'), [org, setOrg] = useState('A'), [wallet, setWallet] = useState(true);
  const me = { user_id: users[actor], personal_permissions: allowed ? ['personal.billing.read', 'personal.recharge.create', 'personal.recharge.manage'] : [],
    active_context: surface === 'personal' ? { context_type: 'PERSONAL', context_version: 1, personal_authorization_version: allowed ? 1 : 2 }
      : { context_type: 'ORGANIZATION', organization_id: organizations[org], context_version: org === 'A' ? 1 : 2,
        organization_authorization_version: (allowed ? 1 : 2) + (wallet ? 0 : 2), permissions: [...(allowed ? ['org.entitlements.purchase'] : []), ...(wallet ? ['org.wallet.read'] : [])] } };
  const reset = async () => { await control({ reset: true }); setActor('A'); setOrg('A'); setAllowed(true); setWallet(true); setPaid(0); setMounted(true); setEpoch(client.current.financeSessionEpoch); setMountKey(value => value + 1); };
  const swapActor = async () => { await control({ actor: 'B' }); setActor('B'); };
  return <main className="ap-shell"><h1>Public loopback fixture: actual React finance components</h1>
    <p data-fixture-actor>Actor {actor}; epoch {epoch}; permission {String(allowed)}; surface {surface}</p><p data-fixture-paid>Paid callbacks: {paid}</p>
    <p>Organization order reference: <code>33333333-3333-4333-8333-333333333333</code></p>
    <Metrics client={client.current} paid={paid} actor={actor} epoch={epoch} allowed={allowed} mounted={mounted} surface={surface} org={org} wallet={wallet} />
    <label>Fixture language<select aria-label="Fixture language" value={language} onChange={event => setLanguage(event.target.value)}>{['zh', 'en', 'ja'].map(value => <option key={value}>{value}</option>)}</select></label>
    <button onClick={() => void reset()}>Reset fixture</button>
    <button onClick={() => setSurface('personal')}>Personal surface</button><button onClick={() => setSurface('organization')}>Organization surface</button>
    <button onClick={() => void control({ status: 'PAID' })}>Provider paid proof</button>
    <button onClick={() => void control({ hold: true })}>Hold order GET</button>
    <button onClick={() => void control({ release: true })}>Release old paid ACK</button>
    <button onClick={() => void swapActor()}>Switch to actor B</button>
    <button onClick={() => setOrg('B')}>Switch to organization B</button>
    <button onClick={() => { client.current.financeSessionEpoch++; setEpoch(value => value + 1); }}>Retire login epoch</button>
    <button key={`epoch-gap:${mountKey}`} onClick={event => {
      client.current.financeSessionEpoch++;
      event.currentTarget.textContent = 'Client epoch retired without parent render'; event.currentTarget.disabled = true;
    }}>Retire client epoch without rendering</button>
    <button onClick={() => setAllowed(false)}>Revoke read permission</button>
    <button onClick={() => setWallet(false)}>Remove organization wallet read permission</button>
    <button onClick={() => setMounted(false)}>Unmount component</button>
    {mounted && (surface === 'personal'
      ? <PersonalFinanceWorkspace key={mountKey} client={client.current} me={me} language={language} onPaid={() => setPaid(value => value + 1)} />
      : <OrganizationFinanceWorkspace key={mountKey} client={client.current} me={me} language={language} />)}
  </main>;
}
createRoot(document.getElementById('root')).render(<Fixture />);
