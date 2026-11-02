import React, { useEffect, useMemo, useRef, useState } from 'react';
import { accountClosureCopy, financeCopy, organizationCopy, tokenManagementCopy } from '../../shared/i18n.js';
import { AccountPlatformBffClient } from './bff-client.js';
import { requirePersonalAccount } from './personal-contract.js';
import { OrganizationWorkspace } from './OrganizationWorkspace.jsx';
import { createOrganizationScopeFailureGuard } from './organization-scope.js';
import { OrganizationDirectory } from './OrganizationDirectory.jsx';
import { RetainedWorkspace } from './RetainedWorkspace.jsx';
import { AdminFinanceOverview } from './AdminFinanceOverview.jsx';
import { RechargeProductWorkspace } from './RechargeProductWorkspace.jsx';
import { PricingLifecycleWorkspace } from './PricingLifecycleWorkspace.jsx';
import { AdminOperationsWorkspace } from './AdminOperationsWorkspace.jsx';
import { AdminAccountClosureWorkspace } from './AdminAccountClosureWorkspace.jsx';
import { AdminRepairQueue } from './AdminRepairQueue.jsx';
import { personalPortalSections } from './portal-sections.js';

export function AdminProductsPage({ client, me, copy, language, permissions, route }) {
  const granted = new Set(permissions);
  const tabs = [granted.has('platform.products.read') && ['catalog', financeCopy[language].productAdmin], granted.has('platform.pricing.read') && ['prices', tokenManagementCopy[language].pricingLifecycle.priceVersions]].filter(Boolean);
  const requested = (new URLSearchParams(route.search).get('finance_tab') || new URLSearchParams(route.search).get('products_tab'));
  const active = tabs.some(([key]) => key === requested) ? requested : tabs[0]?.[0];
  const words = { ...copy, billing: { ...copy.billing, pricingLifecycle: tokenManagementCopy[language].pricingLifecycle } };
  return <section><AdminFinanceOverview {...{ client, language, route, permissions }} surface={active === 'catalog' ? 'products' : 'pricing'} /><nav className="adm-tabs">{tabs.map(([key, label]) => <button key={key} aria-pressed={active === key} onClick={() => route.navigate('products', { finance_tab: key, products_tab: null })}>{label}</button>)}</nav>
    <RetainedWorkspace active={active} available={tabs.map(([key]) => key)}>{view => <>
      {view === 'catalog' && <RechargeProductWorkspace {...{ client, me, language, route }} copy={financeCopy[language]} />}
      {view === 'prices' && <PricingLifecycleWorkspace client={client} owner={me.user_id} copy={words} permissions={permissions} />}
    </>}</RetainedWorkspace>
  </section>;
}

export function AdminOperationsPage({ client, me, copy, language, permissions, route, authorizationRefreshing, authorizationRefreshRef }) {
  const tabs = [
    ['policy', 'platform.registration-bonus.read', copy.operations.nav],
    ['closures', 'platform.account-closures.read', accountClosureCopy[language].title],
    ['registration', 'platform.registrations.repair', copy.userManagement.registrationRepair],
    ['profile', 'platform.profile-changes.repair', copy.userManagement.profileRepair],
  ].filter(([, permission]) => permissions.includes(permission));
  const active = tabs.some(([key]) => key === route.operationsTab) ? route.operationsTab : tabs[0]?.[0];
  return <section><nav className="adm-tabs">{tabs.map(([key, , label]) => <button key={key} aria-pressed={active === key} onClick={() => route.navigate('operations', { operations_page: key })}>{label}</button>)}</nav>
    <RetainedWorkspace active={active} available={tabs.map(([key]) => key)}>{view => <>
      {view === 'policy' && <AdminOperationsWorkspace {...{ client, copy, language, permissions, authorizationRefreshing }} currentUserID={me.user_id} authorizationRefreshGuard={authorizationRefreshRef} />}
      {view === 'closures' && <AdminAccountClosureWorkspace {...{ client, me, language }} />}
      {view === 'registration' && <AdminRepairQueue {...{ client, copy, language }} />}
      {view === 'profile' && <AdminRepairQueue {...{ client, copy, language }} profile />}
    </>}</RetainedWorkspace>
  </section>;
}

export function AdminMyOrganizationPage({ client, me, language, onContextChanged }) {
  const bff = useMemo(() => new AccountPlatformBffClient(client.baseURL), [client]);
  // Profile refresh unmounts the workspace below; retain denied authority here.
  const scopeFailureGuard = useMemo(() => createOrganizationScopeFailureGuard(), [bff]);
  const contextIdentity = account => { const context = account.active_context; return JSON.stringify([account.user_id, context.context_type, context.organization_id || '', context.context_version || 0, context.membership_version || account.membership_version || 0, context.organization_authorization_version || 0]); };
  const contextKey = contextIdentity(me);
  const [loadedContext, setLoadedContext] = useState('');
  const [profile, setProfile] = useState(null), [error, setError] = useState(false), [revision, setRevision] = useState(0);
  const generation = useRef(0), copy = organizationCopy[language];
  useEffect(() => {
    const stamp = ++generation.current, controller = new AbortController();
    setLoadedContext(contextKey); setProfile(null); setError(false);
    void (async () => {
      const session = await bff.session(controller.signal);
      if (!session.authenticated) throw new Error('SESSION_EXPIRED');
      const result = requirePersonalAccount(await bff.bootstrap(controller.signal));
      if (contextIdentity(result) !== contextKey) throw new Error('SESSION_CHANGED');
      if (stamp === generation.current) setProfile(result);
    })().catch(reason => { if (stamp === generation.current && reason.name !== 'AbortError') setError(true); });
    return () => { generation.current++; controller.abort(); };
  }, [bff, contextKey, revision]);
  const changed = async () => { if (onContextChanged) await onContextChanged(); setRevision(value => value + 1); };
  const visibleProfile = loadedContext === contextKey ? profile : null;
  const visibleError = loadedContext === contextKey && error;
  return <section className="adm-panel">{visibleError && <p role="alert">{copy.unavailable}<button onClick={changed}>{copy.refresh}</button></p>}
    {!visibleProfile && !visibleError && <p role="status">{copy.loading}</p>}
    {visibleProfile && <><OrganizationDirectory client={bff} me={visibleProfile} language={language} onChanged={changed} />{personalPortalSections(visibleProfile).includes('myOrganization') ? <OrganizationWorkspace key={`${visibleProfile.user_id}:${visibleProfile.active_context.organization_id}:${visibleProfile.active_context.context_version}`} client={bff} me={visibleProfile} language={language} onChanged={changed} scopeFailureGuard={scopeFailureGuard} /> : <p role="alert">{copy.permissions}</p>}</>}
  </section>;
}
