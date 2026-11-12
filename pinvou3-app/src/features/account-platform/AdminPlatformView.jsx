import { AdminRolesWorkspace } from './AdminRolesWorkspace.jsx';
import { AdminSystemStatusWorkspace } from './AdminSystemStatusWorkspace.jsx';
import { systemRelatedReconciliationRoute } from './admin-system-status.js';
import { AuditWorkspace } from './AuditWorkspace.jsx';
import { OrganizationAdminWorkspace, useOrganizationOverviewReadFeedback } from './OrganizationAdminWorkspace.jsx';
import { AdminManagementWorkspace } from './AdminManagementWorkspace.jsx';
import { adminAuthorizationView, adminAuthorizationSnapshotIsCurrent, readAdminAuthorizationSnapshot } from './admin-authorization-snapshot.js';
import React, { useEffect, useRef, useState } from 'react';
import { SettingsWorkspace } from './SettingsWorkspace.jsx';
import { AdminApprovalWorkspace } from './AdminApprovalWorkspace.jsx';
import { AdminFinanceWorkspace } from './AdminFinanceWorkspace.jsx';
import { auditWorkspaceCopy, adminLayoutCopy } from '../../shared/i18n.js';
import { localizedAccountPlatformError } from './error-localization.js';
import './admin-platform.css';
import './account-platform.css';
import { AdminUsersWorkspace } from './AdminUsersWorkspace.jsx';
import { AdminPortalLayout } from './AdminPortalLayout.jsx';
import { useLastActiveRoute } from './useLastActiveRoute.js';
import { useAdminSections } from './useAdminSections.js';
import { RetainedWorkspace } from './RetainedWorkspace.jsx';
import { AdminOverviewWorkspace } from './AdminOverviewWorkspace.jsx';
import { AdminProductsPage, AdminOperationsPage, AdminMyOrganizationPage } from './AdminRestoredPages.jsx';
import { portalLoginLocation } from './portal-sections.js';

export function AdminPlatformView({ client, copy, language, onLanguageChange }) {
  const [authorizationSnapshot, setAuthorizationSnapshot] = useState(null);
  const overviewSnapshotGenerations = useRef(new WeakMap());
  const { authorization, sessionEpoch: acceptedSessionEpoch } = adminAuthorizationView(authorizationSnapshot, client);
  const [credentialOrigin, setCredentialOrigin] = useState('');
  const [overviewFlags, setOverviewFlags] = useState({});
  const [authorizationOwner, setAuthorizationOwner] = useState(client);
  const [state, setState] = useState('loading');
  const [error, setError] = useState('');
  const [systemHeaderTarget, setSystemHeaderTarget] = useState(null);
  const authorizationReload = useRef(null);
  const authorizationRefreshRef = useRef({ active: false, generation: 0, settled: Promise.resolve() });
  const organizationOverviewFeedback = useOrganizationOverviewReadFeedback(client, authorization, acceptedSessionEpoch, () => authorizationRefreshRef.current, overviewSnapshotGenerations.current.get(authorizationSnapshot) ?? 0);
  const publishAuthorizationSnapshot = (snapshot, overviewCheckGeneration) => {
    overviewSnapshotGenerations.current.set(snapshot, overviewCheckGeneration);
    setAuthorizationSnapshot(snapshot);
  };
  const generation = useRef(0);
  const currentClient = useRef(client); currentClient.current = client;
  const [authorizationRefreshing, setAuthorizationRefreshing] = useState(false);
  const load = async () => {
    const request = ++generation.current;
    const overviewCheckGeneration = authorizationRefreshRef.current.generation;
    setAuthorizationOwner(client); setAuthorizationSnapshot(null); setState('loading'); setError('');
    try {
      const snapshot = await readAdminAuthorizationSnapshot(client, () => request === generation.current && currentClient.current === client);
      if (!snapshot || request !== generation.current || currentClient.current !== client || !adminAuthorizationSnapshotIsCurrent(snapshot, client)) return;
      const { session } = snapshot;
      if (!session.authenticated) { setState('guest'); return; }
      setCredentialOrigin(typeof session.credential_origin === 'string' ? session.credential_origin : '');
      setOverviewFlags({ ...(session.feature_flags && typeof session.feature_flags === 'object' ? session.feature_flags : {}), page: session.feature_flags?.admin_model_token_page_overviews === true, aggregate: session.feature_flags?.admin_model_operations_overview === true });
      publishAuthorizationSnapshot(snapshot, overviewCheckGeneration); setState('ready');
    } catch (reason) { if (request === generation.current) { setError(localizedAccountPlatformError(copy, reason)); setState('error'); } }
  };
  useEffect(() => { void load(); return () => { generation.current++; }; }, [client]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    const refresh = async () => {
      const guard = authorizationRefreshRef.current;
      if (guard.active) return guard.settled;
      const request = ++generation.current;
      guard.generation++; guard.active = true;
      let settle;
      guard.settled = new Promise(resolve => { settle = resolve; });
      setAuthorizationRefreshing(true);
      client.invalidateRead('billing', '/admin/v1/registration-bonus-policy');
      try {
        const snapshot = await readAdminAuthorizationSnapshot(client, () => request === generation.current && currentClient.current === client);
        if (!snapshot || request !== generation.current || currentClient.current !== client || !adminAuthorizationSnapshotIsCurrent(snapshot, client)) return;
        const { session } = snapshot;
        if (!session.authenticated) { setAuthorizationSnapshot(null); setState('guest'); return; }
        setCredentialOrigin(typeof session.credential_origin === 'string' ? session.credential_origin : '');
        setOverviewFlags({ ...(session.feature_flags && typeof session.feature_flags === 'object' ? session.feature_flags : {}), page: session.feature_flags?.admin_model_token_page_overviews === true, aggregate: session.feature_flags?.admin_model_operations_overview === true });
        publishAuthorizationSnapshot(snapshot, guard.generation); setState('ready');
      } catch (reason) {
        if (request === generation.current) { setState('error'); setError(localizedAccountPlatformError(copy, reason)); }
      } finally {
        guard.active = false; settle(); setAuthorizationRefreshing(false);
      }
    };
    authorizationReload.current = refresh;
    globalThis.addEventListener('focus', refresh);
    const unsubscribe = client.onAuthorizationVersion?.(refresh);
    return () => { if (authorizationReload.current === refresh) authorizationReload.current = null; unsubscribe?.(); globalThis.removeEventListener('focus', refresh); };
  }, [client, copy]);
  const permissions = authorization?.platform_permissions || [];
  const navigation = useAdminSections({ me: authorization, flags: overviewFlags, ready: state === 'ready' && authorizationOwner === client });
  const visibleSection = navigation.section;
  useEffect(() => {
    if (state !== 'ready' || visibleSection !== 'operations') return undefined;
    let disposed = false;
    const refresh = async () => {
      if (authorizationRefreshRef.current.active) return;
      let settle = () => {};
      const settled = new Promise(resolve => { settle = resolve; });
      const refreshGeneration = authorizationRefreshRef.current.generation + 1;
      authorizationRefreshRef.current = { active: true, generation: refreshGeneration, settled };
      setAuthorizationRefreshing(true);
      try {
        const snapshot = await readAdminAuthorizationSnapshot(client, () => !disposed && currentClient.current === client);
        if (!snapshot || disposed || currentClient.current !== client || !adminAuthorizationSnapshotIsCurrent(snapshot, client)) return;
        const { session } = snapshot;
        if (!session.authenticated) { setAuthorizationSnapshot(null); setState('guest'); return; }
        setCredentialOrigin(typeof session.credential_origin === 'string' ? session.credential_origin : '');
        setOverviewFlags({ ...(session.feature_flags && typeof session.feature_flags === 'object' ? session.feature_flags : {}), page: session.feature_flags?.admin_model_token_page_overviews === true, aggregate: session.feature_flags?.admin_model_operations_overview === true });
        publishAuthorizationSnapshot(snapshot, refreshGeneration);
      } catch (reason) {
        if (!disposed) { setAuthorizationSnapshot(null); setState('error'); setError(localizedAccountPlatformError(copy, reason)); }
      } finally {
        if (authorizationRefreshRef.current.generation === refreshGeneration) {
          authorizationRefreshRef.current = { active: false, generation: refreshGeneration, settled: Promise.resolve() };
          setAuthorizationRefreshing(false);
        }
        settle();
      }
    };
    const unsubscribe = client.onAuthorizationVersion?.(() => { void refresh(); });
    const onFocus = () => { void refresh(); };
    globalThis.addEventListener?.('focus', onFocus);
    return () => { disposed = true; unsubscribe?.(); globalThis.removeEventListener?.('focus', onFocus); };
  }, [client, copy, state, visibleSection]);
  const logout = async () => {
    const request = ++generation.current;
    setAuthorizationOwner(client); setAuthorizationSnapshot(null); setState('loading'); setError('');
    try { await client.logout(); if (request === generation.current) setState('guest'); }
    catch (reason) { if (request === generation.current) { setState('error'); setError(localizedAccountPlatformError(copy, reason)); } }
  };
  const renderedState = authorizationOwner === client ? state : 'loading';
  const words = adminLayoutCopy[language];
  const version = authorization?.platform_authorization_version || authorization?.active_context?.platform_authorization_version || authorization?.authorization_version || 0;
  const ownerKey = `${authorization?.user_id}:${version}:${client.financeSessionEpoch || 0}`;
  const workspace = key => ['billing', 'payments'].includes(key) ? 'finance' : ['tokens', 'models', 'tokenResources', 'modelOperations'].includes(key) ? 'management' : key;
  const active = workspace(visibleSection);
  const available = [...new Set(navigation.available.map(workspace))];
  const financeRoute = useLastActiveRoute(navigation, active === 'finance');
  const managementRoute = useLastActiveRoute(navigation, active === 'management');
  const productsRoute = useLastActiveRoute(navigation, active === 'products');
  const organizationRoute = useLastActiveRoute(navigation, active === 'organizations');
  const operationsRoute = useLastActiveRoute(navigation, active === 'operations');
  const viewApproval = (id, updates = {}) => navigation.navigate('approvals', { ...updates, approval_view: 'mine', approval_request_id: id || null });
  const reconciliationRoute = systemRelatedReconciliationRoute(navigation.available, permissions);
  const openSystemReconciliation = reconciliationRoute ? () => navigation.navigate(reconciliationRoute.section, reconciliationRoute.query) : undefined;
  const systemHeaderActive = renderedState === 'ready' && visibleSection === 'system' && navigation.available.includes('system');
  return <AdminPortalLayout {...{ copy, language, onLanguageChange, navigation }} ready={renderedState === 'ready'} onLogout={logout} pageActionsRef={systemHeaderActive ? setSystemHeaderTarget : undefined}>
    {renderedState === 'loading' && <p role="status">{copy.loading}</p>}
    {error && <div role="alert">{error}<button onClick={load}>{copy.retry}</button></div>}
    {renderedState === 'guest' && <a href={portalLoginLocation(globalThis.location.pathname, globalThis.location.search)}>{copy.login}</a>}
    {renderedState === 'ready' && <>
      {!navigation.available.includes(visibleSection) && <p role="alert">{words.unauthorized}</p>}
      <RetainedWorkspace key={ownerKey} active={navigation.available.includes(visibleSection) ? active : ''} available={available}>{view => <>
        {view === 'overview' && <AdminOverviewWorkspace {...{ client, copy, language, permissions, navigation }} onOpenReconciliation={openSystemReconciliation} />}
        {view === 'operations' && <AdminOperationsPage {...{ client, copy, language, permissions }} me={authorization} route={operationsRoute} authorizationRefreshing={authorizationRefreshing} authorizationRefreshRef={authorizationRefreshRef} />}
        {view === 'system' && <AdminSystemStatusWorkspace {...{ client, copy, language, permissions }} showTitle={false} headerTarget={systemHeaderActive ? systemHeaderTarget : null} onOpenReconciliation={openSystemReconciliation} />}
        {view === 'audit' && <AuditWorkspace client={client} copy={{ ...copy, ...auditWorkspaceCopy[language] }} permissions={permissions} currentUserID={authorization.user_id} />}
        {view === 'management' && <AdminManagementWorkspace {...{ client, copy, language, permissions, credentialOrigin, overviewFlags }} owner={authorization.user_id} authorizationVersion={version} overviewAuthorizationVersion={version} controlledSection={['models', 'tokenResources', 'modelOperations'].includes(managementRoute.section) ? managementRoute.section : undefined} routeSearch={managementRoute.search} onNavigateSection={overviewFlags.admin_model_token_ia === true ? (target, updates) => navigation.navigate(target, updates) : undefined} onRouteChange={(updates, mode) => navigation.navigate(navigation.section, updates, mode)} onViewApproval={viewApproval} />}
        {view === 'organizations' && <OrganizationAdminWorkspace client={client} me={authorization} language={language} route={organizationRoute} acceptedSessionEpoch={acceptedSessionEpoch} overviewReadFeedback={organizationOverviewFeedback} />}
        {view === 'myorg' && <AdminMyOrganizationPage client={client} me={authorization} language={language} onContextChanged={() => authorizationReload.current?.()} />}
        {view === 'approvals' && <AdminApprovalWorkspace {...{ client, copy, language }} me={authorization} routeSearch={visibleSection === 'approvals' ? navigation.search : undefined} onRouteChange={updates => navigation.navigate('approvals', updates, 'replace')} onNavigate={route => navigation.navigate(['orders', 'late', 'anomalies', 'controls'].includes(route.tab) ? 'payments' : 'billing', { finance_tab: route.tab })} />}
        {view === 'products' && <AdminProductsPage {...{ client, copy, language, permissions }} me={authorization} route={productsRoute} />}
        {view === 'finance' && <AdminFinanceWorkspace {...{ client, language, acceptedSessionEpoch }} acceptedPlatformAuthorizationVersion={version} me={authorization} surface={financeRoute.section === 'payments' ? 'payments' : 'billing'} initialTab={financeRoute.tab || (financeRoute.section === 'billing' ? 'wallets' : 'orders')} route={financeRoute} onTabChange={tab => navigation.navigate(financeRoute.section, { finance_tab: tab })} onViewApproval={viewApproval} />}
        {view === 'roles' && <AdminRolesWorkspace client={client} me={authorization} language={language} acceptedSessionEpoch={acceptedSessionEpoch} route={navigation} initialTarget={new URLSearchParams(navigation.search).get('role_user_id') || ''} onAuthorizationChange={load} />}
        {view === 'users' && <AdminUsersWorkspace {...{ client, copy, language }} currentUserID={authorization.user_id} acceptedSessionEpoch={acceptedSessionEpoch} acceptedPlatformAuthorizationVersion={version} route={navigation} canManage={permissions.includes('platform.users.status.manage')} />}
        {view === 'settings' && <SettingsWorkspace {...{ client, copy, language, onLanguageChange, permissions }} />}
      </>}</RetainedWorkspace>
    </>}
  </AdminPortalLayout>;
}
