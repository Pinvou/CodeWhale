import React, { useEffect, useMemo, useRef, useState } from 'react';
import {
  canGrantRole,
  canRevokeBinding,
  filterBindings,
  filterRoles,
  permissionGroups,
  roleGrantMode,
  roleDescription,
  roleName,
  roleScopeLabel,
  roleScopes,
  summarizeRoleScopes,
} from './admin-role-management.js';
import './admin-role-management.css';

function shortID(value) {
  const text = String(value || '');
  return text.length > 16 ? `${text.slice(0, 8)}…${text.slice(-5)}` : text || '—';
}

function formatDate(value, copy) {
  return value ? new Date(value).toLocaleString() : copy.roles.forever;
}

function userLabel(user, fallback = '') {
  return user?.display_name || user?.username || user?.user_code || shortID(fallback);
}

function Drawer({ label, onClose, children }) {
  const drawerRef = useRef(null);
  useEffect(() => {
    const previousFocus = document.activeElement;
    drawerRef.current?.focus();
    const closeOnEscape = (event) => event.key === 'Escape' && onClose();
    globalThis.addEventListener?.('keydown', closeOnEscape);
    return () => {
      globalThis.removeEventListener?.('keydown', closeOnEscape);
      previousFocus?.focus?.();
    };
  }, [onClose]);
  const trapFocus = (event) => {
    if (event.key !== 'Tab') return;
    const focusable = [...(drawerRef.current?.querySelectorAll('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])') || [])];
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
      <aside ref={drawerRef} className="adm-detail-drawer adm-role-drawer" role="dialog" aria-modal="true" aria-label={label} tabIndex="-1" onKeyDown={trapFocus}>
        {children}
      </aside>
    </div>
  );
}

function ScopeBadge({ copy, scope }) {
  return <span className={`adm-role-scope ${String(scope || '').toLowerCase()}`}>{roleScopeLabel(copy, scope)}</span>;
}

function RoleDetail({ copy, role, onGrant }) {
  if (!role) return <div className="adm-role-detail-empty">{copy.roles.chooseRole}</div>;
  const groups = permissionGroups(copy, role);
  return (
    <aside className="adm-role-detail" aria-live="polite">
      <header>
        <div>
          <h3>{roleName(copy, role.code)}</h3>
          <code>{role.code}</code>
        </div>
        <ScopeBadge copy={copy} scope={role.scope} />
      </header>
      <p>{roleDescription(copy, role)}</p>
      <dl className="adm-role-facts">
        <div><dt>{copy.roles.type}</dt><dd>{copy.roles.builtIn}</dd></div>
        <div><dt>{copy.roles.grantMethod}</dt><dd>{roleGrantMode(copy, role)}</dd></div>
        <div><dt>{copy.roles.permissionCount}</dt><dd>{copy.roles.permissionsCount(role.permissions?.length || 0)}</dd></div>
        <div><dt>{copy.accounts.status}</dt><dd>{copy.roles.active}</dd></div>
      </dl>
      <div className="adm-role-permission-title"><strong>{copy.roles.includedPermissions}</strong><span>{copy.roles.groupedByDomain}</span></div>
      <div className="adm-role-permission-groups">
        {groups.map((group) => (
          <details key={group.domain} open>
            <summary>{group.label}<span>{group.items.length}</span></summary>
            {group.items.map((permission) => <div className="adm-role-permission" key={permission.code}><strong>{permission.label}</strong><code>{permission.code}</code></div>)}
          </details>
        ))}
      </div>
      <div className="adm-role-lifecycle-note">{roleGrantMode(copy, role)}：{copy.roles.lifecycleHints?.[role.code] || copy.roles.defaultLifecycleHint}</div>
      {onGrant && canGrantRole(role) && <button className="adm-role-primary" type="button" onClick={() => onGrant(role)}>{copy.roles.grantThisRole}</button>}
    </aside>
  );
}

function GrantDrawer({ copy, client, roles, users, organizations, onClose, onGranted, initialRole }) {
  const [step, setStep] = useState(1);
  const [form, setForm] = useState(() => ({
    userID: '',
    scope: initialRole?.scope && initialRole.scope !== 'PERSONAL' ? initialRole.scope : 'PLATFORM',
    organizationID: '',
    unitID: '',
    roleCode: initialRole?.code || '',
    startsAt: '',
    expiresAt: '',
    reason: '',
  }));
  const [userSearch, setUserSearch] = useState('');
  const [userOptions, setUserOptions] = useState(users || []);
  const [units, setUnits] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const selectedRole = roles.find((role) => role.code === form.roleCode);
  const availableRoles = roles.filter((role) => role.scope === form.scope && canGrantRole(role));
  const selectedUser = userOptions.find((user) => user.user_id === form.userID) || users.find((user) => user.user_id === form.userID);
  const selectedOrganization = organizations.find((organization) => organization.organization_id === form.organizationID);
  const selectedUnit = units.find((unit) => unit.unit_id === form.unitID);
  const availableScopes = roleScopes.filter((scope) => scope !== 'PERSONAL' && (scope === 'PLATFORM' || organizations.length > 0));

  useEffect(() => {
    if (!form.roleCode && availableRoles[0]) setForm((current) => ({ ...current, roleCode: availableRoles[0].code }));
    if (form.roleCode && !availableRoles.some((role) => role.code === form.roleCode)) setForm((current) => ({ ...current, roleCode: availableRoles[0]?.code || '' }));
  }, [availableRoles, form.roleCode]);

  const updateScope = (scope) => {
    const nextRole = roles.find((role) => role.scope === scope && canGrantRole(role));
    setUnits([]);
    setForm((current) => ({ ...current, scope, organizationID: '', unitID: '', roleCode: nextRole?.code || '' }));
  };
  const updateOrganization = async (organizationID) => {
    setForm((current) => ({ ...current, organizationID, unitID: '' }));
    setUnits([]);
    if (!organizationID || form.scope !== 'UNIT') return;
    setBusy(true);
    setError('');
    try {
      const result = await client.organizationUnits(organizationID);
      setUnits((result.items || []).filter((unit) => unit.status === 'ACTIVE'));
    } catch {
      setError(copy.roles.unitsLoadFailed);
    } finally {
      setBusy(false);
    }
  };
  const searchUsers = async (event) => {
    event.preventDefault();
    setBusy(true);
    setError('');
    try {
      const result = await client.users(userSearch.trim());
      setUserOptions(result.items || []);
    } catch {
      setError(copy.roles.usersLoadFailed);
    } finally {
      setBusy(false);
    }
  };
  const validateStep = () => {
    if (step === 1 && !form.userID) return copy.roles.userRequired;
    if (step === 2 && form.scope !== 'PLATFORM' && !form.organizationID) return copy.roles.organizationRequired;
    if (step === 2 && form.scope === 'UNIT' && !form.unitID) return copy.roles.unitRequired;
    if (step === 3 && !form.roleCode) return copy.roles.roleRequired;
    if (step === 3 && form.startsAt && form.expiresAt && new Date(form.expiresAt) <= new Date(form.startsAt)) return copy.roles.invalidValidity;
    if (step === 3 && form.roleCode === 'PLATFORM_AUDIT_PII_EXPORT' && (!form.expiresAt || form.reason.trim().length < 10)) return copy.roles.piiRequirements;
    return '';
  };
  const next = () => {
    const validation = validateStep();
    setError(validation);
    if (!validation) setStep((value) => Math.min(4, value + 1));
  };
  const submit = async () => {
    setBusy(true);
    setError('');
    try {
      await client.grantRoleBinding({
        user_id: form.userID,
        role_code: form.roleCode,
        organization_id: form.scope === 'PLATFORM' ? null : form.organizationID,
        unit_id: form.scope === 'UNIT' ? form.unitID : null,
        starts_at: form.startsAt ? new Date(form.startsAt).toISOString() : null,
        expires_at: form.expiresAt ? new Date(form.expiresAt).toISOString() : null,
        idempotency_key: `admin-role-${crypto.randomUUID()}`,
        reason: form.reason.trim(),
      });
      await onGranted();
      onClose();
    } catch (reason) {
      setError(reason?.code === 'RECENT_AUTHENTICATION_REQUIRED' ? copy.roles.recentAuthenticationRequired : copy.roles.grantFailed);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Drawer label={copy.roles.newGrant} onClose={onClose}>
      <header className="adm-role-drawer-head"><div><span className="adm-eyebrow">{copy.roles.guidedGrant}</span><h2>{copy.roles.newGrant}</h2></div><button type="button" onClick={onClose} aria-label={copy.accounts.cancel}>×</button></header>
      <ol className="adm-role-steps">
        {[copy.roles.stepUser, copy.roles.stepScope, copy.roles.stepRole, copy.roles.stepConfirm].map((label, index) => <li key={label} className={step === index + 1 ? 'active' : step > index + 1 ? 'done' : ''}><span>{index + 1}</span>{label}</li>)}
      </ol>
      <div className="adm-role-drawer-body">
        {error && <div className="adm-inline-message error" role="alert">{error}</div>}
        {step === 1 && <>
          <h3>{copy.roles.chooseUser}</h3><p>{copy.roles.chooseUserLead}</p>
          <form className="adm-role-search" onSubmit={searchUsers}><input value={userSearch} onChange={(event) => setUserSearch(event.target.value)} placeholder={copy.roles.userSearchPlaceholder} /><button disabled={busy}>{copy.searchAction}</button></form>
          <label className="adm-role-field"><span>{copy.roles.user}</span><select value={form.userID} onChange={(event) => setForm((current) => ({ ...current, userID: event.target.value }))}><option value="">{copy.roles.chooseUser}</option>{userOptions.map((user) => <option key={user.user_id} value={user.user_id}>{userLabel(user, user.user_id)} · {user.user_code || shortID(user.user_id)}</option>)}</select></label>
          <small className="adm-role-help">{copy.roles.noEmailSearch}</small>
        </>}
        {step === 2 && <>
          <h3>{copy.roles.chooseScope}</h3><p>{copy.roles.chooseScopeLead}</p>
          <div className="adm-role-scope-options">{availableScopes.map((scope) => <button key={scope} className={form.scope === scope ? 'active' : ''} type="button" onClick={() => updateScope(scope)}><ScopeBadge copy={copy} scope={scope} /><small>{copy.roles.scopeHints?.[scope]}</small></button>)}</div>
          {form.scope !== 'PLATFORM' && <label className="adm-role-field"><span>{copy.roles.organization}</span><select value={form.organizationID} onChange={(event) => updateOrganization(event.target.value)}><option value="">{copy.roles.chooseOrganization}</option>{organizations.filter((organization) => organization.status === 'ACTIVE').map((organization) => <option key={organization.organization_id} value={organization.organization_id}>{organization.name}</option>)}</select></label>}
          {form.scope === 'UNIT' && <label className="adm-role-field"><span>{copy.roles.unit}</span><select value={form.unitID} disabled={busy || !form.organizationID} onChange={(event) => setForm((current) => ({ ...current, unitID: event.target.value }))}><option value="">{copy.roles.chooseUnit}</option>{units.map((unit) => <option key={unit.unit_id} value={unit.unit_id}>{unit.name}</option>)}</select></label>}
        </>}
        {step === 3 && <>
          <h3>{copy.roles.chooseRoleAndValidity}</h3><p>{copy.roles.chooseRoleLead}</p>
          <label className="adm-role-field"><span>{copy.roles.role}</span><select value={form.roleCode} onChange={(event) => setForm((current) => ({ ...current, roleCode: event.target.value }))}>{availableRoles.map((role) => <option key={role.code} value={role.code}>{roleName(copy, role.code)} · {role.code}</option>)}</select></label>
          {selectedRole && <div className="adm-role-selection-summary"><strong>{copy.roles.permissionsCount(selectedRole.permissions?.length || 0)}</strong><span>{roleDescription(copy, selectedRole)}</span></div>}
          <div className="adm-role-field-row"><label className="adm-role-field"><span>{copy.roles.startsAt}</span><input type="datetime-local" value={form.startsAt} onChange={(event) => setForm((current) => ({ ...current, startsAt: event.target.value }))} /></label><label className="adm-role-field"><span>{copy.roles.expiresAt}</span><input type="datetime-local" value={form.expiresAt} onChange={(event) => setForm((current) => ({ ...current, expiresAt: event.target.value }))} /></label></div>
          <label className="adm-role-field"><span>{copy.roles.reason}{form.roleCode === 'PLATFORM_AUDIT_PII_EXPORT' ? ` · ${copy.roles.required}` : ''}</span><textarea value={form.reason} maxLength="500" onChange={(event) => setForm((current) => ({ ...current, reason: event.target.value }))} placeholder={copy.roles.reasonPlaceholder} /></label>
          {form.roleCode === 'PLATFORM_AUDIT_PII_EXPORT' && <div className="adm-role-risk-note">{copy.roles.piiRequirements}</div>}
        </>}
        {step === 4 && <>
          <h3>{copy.roles.confirmGrant}</h3><p>{copy.roles.confirmGrantLead}</p>
          <dl className="adm-role-preview">
            <div><dt>{copy.roles.user}</dt><dd>{userLabel(selectedUser, form.userID)}</dd></div>
            <div><dt>{copy.roles.role}</dt><dd>{roleName(copy, form.roleCode)}<small>{form.roleCode}</small></dd></div>
            <div><dt>{copy.roles.scope}</dt><dd><ScopeBadge copy={copy} scope={form.scope} />{selectedOrganization && <small>{selectedOrganization.name}{selectedUnit ? ` / ${selectedUnit.name}` : ''}</small>}</dd></div>
            <div><dt>{copy.roles.validity}</dt><dd>{form.startsAt ? formatDate(form.startsAt, copy) : copy.roles.immediately} → {form.expiresAt ? formatDate(form.expiresAt, copy) : copy.roles.forever}</dd></div>
            <div><dt>{copy.roles.permissionCount}</dt><dd>{copy.roles.permissionsCount(selectedRole?.permissions?.length || 0)}</dd></div>
            <div><dt>{copy.roles.reason}</dt><dd>{form.reason || copy.roles.notProvided}</dd></div>
          </dl>
          <div className="adm-role-risk-note">{copy.roles.authorizationImpact}</div>
        </>}
      </div>
      <footer className="adm-role-drawer-foot"><button type="button" onClick={step === 1 ? onClose : () => { setError(''); setStep((value) => value - 1); }}>{step === 1 ? copy.accounts.cancel : copy.roles.previous}</button><button className="adm-role-primary" type="button" disabled={busy} onClick={step === 4 ? submit : next}>{busy ? copy.loading : step === 4 ? copy.roles.confirmAndGrant : copy.roles.next}</button></footer>
    </Drawer>
  );
}

function BindingDetail({ copy, binding, role, user, organization, onClose, onRevoke }) {
  return <Drawer label={copy.roles.bindingDetail} onClose={onClose}>
    <section className="adm-role-binding-detail">
      <header className="adm-role-drawer-head"><div><span className="adm-eyebrow">{copy.roles.bindingDetail}</span><h2>{roleName(copy, binding.role_code)}</h2></div><button type="button" onClick={onClose} aria-label={copy.accounts.cancel}>×</button></header>
      <dl className="adm-role-preview">
        <div><dt>{copy.roles.user}</dt><dd>{userLabel(user, binding.user_id)}<small>{shortID(binding.user_id)}</small></dd></div>
        <div><dt>{copy.roles.role}</dt><dd>{binding.role_code}</dd></div>
        <div><dt>{copy.roles.scope}</dt><dd><ScopeBadge copy={copy} scope={binding.scope} />{organization && <small>{organization.name}</small>}</dd></div>
        <div><dt>{copy.roles.validity}</dt><dd>{formatDate(binding.starts_at, copy)} → {formatDate(binding.expires_at, copy)}</dd></div>
        <div><dt>{copy.roles.source}</dt><dd>{copy.roles.sources?.[binding.grant_source] || binding.grant_source}</dd></div>
        <div><dt>{copy.accounts.status}</dt><dd>{copy.roles.statuses?.[binding.status] || binding.status}</dd></div>
        <div><dt>{copy.roles.bindingID}</dt><dd><code>{binding.binding_id}</code></dd></div>
        <div><dt>{copy.accounts.created}</dt><dd>{formatDate(binding.created_at, copy)}</dd></div>
      </dl>
      {onRevoke && <button className="adm-role-danger" type="button" onClick={() => onRevoke(binding)}>{copy.roles.revoke}</button>}
    </section>
  </Drawer>;
}

export function AdminRolesWorkspace({ copy, roles, bindings, users, organizations, client, setData, permissions, onNavigate }) {
  const params = new URLSearchParams(String(globalThis.location?.search || ''));
  const deepLinkedRole = params.get('role');
  const [tab, setTab] = useState(params.get('role_view') === 'bindings' ? 'bindings' : 'catalog');
  const [roleFilters, setRoleFilters] = useState({ query: '', scope: params.get('role_scope') || '', grantable: '' });
  const [bindingFilters, setBindingFilters] = useState({ query: '', scope: '', status: '' });
  const [selectedRoleCode, setSelectedRoleCode] = useState(deepLinkedRole || roles[0]?.code || '');
  const [selectedBinding, setSelectedBinding] = useState(null);
  const [grantRole, setGrantRole] = useState(null);
  const [bindingPage, setBindingPage] = useState(1);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState({ tone: '', text: '' });
  const permissionSet = new Set(permissions || []);
  const canAssign = permissionSet.has('platform.roles.assign') && permissionSet.has('platform.users.read');
  const roleByCode = useMemo(() => new Map(roles.map((role) => [role.code, role])), [roles]);
  const userByID = useMemo(() => new Map(users.map((user) => [user.user_id, user])), [users]);
  const organizationByID = useMemo(() => new Map(organizations.map((organization) => [organization.organization_id, organization])), [organizations]);
  const visibleRoles = useMemo(() => filterRoles(roles, roleFilters, copy), [copy, roleFilters, roles]);
  const visibleBindings = useMemo(() => filterBindings(bindings, bindingFilters, roleByCode, userByID, organizationByID, copy), [bindingFilters, bindings, copy, organizationByID, roleByCode, userByID]);
  const selectedRole = visibleRoles.find((role) => role.code === selectedRoleCode) || visibleRoles[0] || null;
  const scopeCounts = useMemo(() => summarizeRoleScopes(roles), [roles]);
  const pageSize = 20;
  const pageCount = Math.max(1, Math.ceil(visibleBindings.length / pageSize));
  const pageItems = visibleBindings.slice((bindingPage - 1) * pageSize, bindingPage * pageSize);

  useEffect(() => { if (bindingPage > pageCount) setBindingPage(pageCount); }, [bindingPage, pageCount]);
  const refreshBindings = async () => {
    const result = await client.roleBindings();
    setData((current) => ({ ...current, roleBindings: result.items || [] }));
  };
  const revoke = async (binding) => {
    const role = roleByCode.get(binding.role_code);
    if (!window.confirm(copy.roles.confirmRevoke(roleName(copy, binding.role_code), userLabel(userByID.get(binding.user_id), binding.user_id)))) return;
    setBusy(true);
    setMessage({ tone: '', text: '' });
    try {
      await client.revokeRoleBinding(binding.binding_id);
      await refreshBindings();
      setSelectedBinding(null);
      setMessage({ tone: 'ok', text: copy.roles.revokedNotice });
    } catch {
      setMessage({ tone: 'error', text: copy.roles.revokeFailed });
    } finally {
      setBusy(false);
    }
  };
  return <div className="adm-role-workspace">
    <section className="adm-role-summary" aria-label={copy.roles.scopeSummary}>{roleScopes.map((scope) => <article key={scope}><span>{roleScopeLabel(copy, scope)}</span><strong>{scopeCounts[scope]}</strong><small>{copy.roles.scopeHints?.[scope]}</small></article>)}</section>
    <section className="adm-panel adm-role-panel">
      <header className="adm-role-workspace-head"><div className="adm-tabs" role="tablist"><button type="button" role="tab" aria-selected={tab === 'catalog'} className={tab === 'catalog' ? 'active' : ''} onClick={() => setTab('catalog')}>{copy.roles.catalog}</button><button type="button" role="tab" aria-selected={tab === 'bindings'} className={tab === 'bindings' ? 'active' : ''} onClick={() => setTab('bindings')}>{copy.roles.bindingManagement}</button></div><div>{permissionSet.has('platform.audit.read') && <button type="button" onClick={() => onNavigate('audit', { audit_action: 'admin.role.binding', audit_resource: 'role_binding' })}>{copy.roles.viewChanges}</button>}{canAssign && <button className="adm-role-primary" type="button" onClick={() => setGrantRole({})}>{copy.roles.newGrant}</button>}</div></header>
      {message.text && <div className={`adm-inline-message ${message.tone}`} role={message.tone === 'error' ? 'alert' : 'status'}>{message.text}</div>}
      {tab === 'catalog' ? <>
        <div className="adm-role-toolbar"><input value={roleFilters.query} onChange={(event) => setRoleFilters((current) => ({ ...current, query: event.target.value }))} placeholder={copy.roles.roleSearchPlaceholder} /><select value={roleFilters.scope} onChange={(event) => setRoleFilters((current) => ({ ...current, scope: event.target.value }))}><option value="">{copy.roles.allScopes}</option>{roleScopes.map((scope) => <option key={scope} value={scope}>{roleScopeLabel(copy, scope)}</option>)}</select><select value={roleFilters.grantable} onChange={(event) => setRoleFilters((current) => ({ ...current, grantable: event.target.value }))}><option value="">{copy.roles.allGrantMethods}</option><option value="direct">{copy.roles.directGrant}</option><option value="managed">{copy.roles.managedGrant}</option></select><span>{copy.items(visibleRoles.length)}</span></div>
        <div className="adm-role-catalog-layout"><div className="adm-table-wrap" tabIndex="0"><table className="adm-table adm-role-table"><thead><tr><th>{copy.roles.role}</th><th>{copy.roles.scope}</th><th>{copy.roles.permissionCount}</th><th>{copy.roles.type}</th><th>{copy.roles.grantMethod}</th><th>{copy.accounts.actions}</th></tr></thead><tbody>{visibleRoles.map((role) => <tr key={role.code} className={selectedRole?.code === role.code ? 'selected' : ''} onClick={() => setSelectedRoleCode(role.code)}><td><strong>{roleName(copy, role.code)}</strong><code>{role.code}</code></td><td><ScopeBadge copy={copy} scope={role.scope} /></td><td>{copy.roles.permissionsCount(role.permissions?.length || 0)}</td><td>{copy.roles.builtIn}</td><td>{roleGrantMode(copy, role)}</td><td><button type="button" className="adm-name-link" onClick={(event) => { event.stopPropagation(); setSelectedRoleCode(role.code); }}>{copy.roles.view}</button></td></tr>)}</tbody></table>{!visibleRoles.length && <div className="adm-empty">{copy.roles.noRoles}</div>}</div><RoleDetail copy={copy} role={selectedRole} onGrant={canAssign ? setGrantRole : null} /></div>
      </> : <>
        <div className="adm-role-toolbar"><input value={bindingFilters.query} onChange={(event) => { setBindingPage(1); setBindingFilters((current) => ({ ...current, query: event.target.value })); }} placeholder={copy.roles.bindingSearchPlaceholder} /><select value={bindingFilters.scope} onChange={(event) => { setBindingPage(1); setBindingFilters((current) => ({ ...current, scope: event.target.value })); }}><option value="">{copy.roles.allScopes}</option>{roleScopes.map((scope) => <option key={scope} value={scope}>{roleScopeLabel(copy, scope)}</option>)}</select><select value={bindingFilters.status} onChange={(event) => { setBindingPage(1); setBindingFilters((current) => ({ ...current, status: event.target.value })); }}><option value="">{copy.roles.allStatuses}</option>{['SCHEDULED', 'ACTIVE', 'EXPIRED', 'REVOKED'].map((status) => <option key={status} value={status}>{copy.roles.statuses?.[status] || status}</option>)}</select><span>{copy.items(visibleBindings.length)}</span></div>
        <div className="adm-table-wrap" tabIndex="0"><table className="adm-table adm-role-binding-table"><thead><tr><th>{copy.roles.user}</th><th>{copy.roles.roleAndScope}</th><th>{copy.roles.validity}</th><th>{copy.roles.source}</th><th>{copy.accounts.status}</th><th>{copy.accounts.actions}</th></tr></thead><tbody>{pageItems.map((binding) => { const user = userByID.get(binding.user_id); const organization = organizationByID.get(binding.organization_id); const role = roleByCode.get(binding.role_code); const revocable = canAssign && canRevokeBinding(binding, roleByCode); return <tr key={binding.binding_id}><td><strong>{userLabel(user, binding.user_id)}</strong><small>{user?.user_code || shortID(binding.user_id)}</small></td><td><strong>{roleName(copy, binding.role_code)}</strong><div><ScopeBadge copy={copy} scope={binding.scope} />{organization && <small>{organization.name}</small>}</div></td><td>{formatDate(binding.starts_at, copy)}<small>{formatDate(binding.expires_at, copy)}</small></td><td>{copy.roles.sources?.[binding.grant_source] || binding.grant_source}</td><td><span className={`adm-status ${binding.status === 'ACTIVE' ? 'ok' : binding.status === 'SCHEDULED' ? 'warn' : 'neutral'}`}>{copy.roles.statuses?.[binding.status] || binding.status}</span></td><td><div className="adm-row-actions"><button type="button" onClick={() => setSelectedBinding(binding)}>{copy.roles.view}</button>{revocable && <button type="button" className="danger" disabled={busy} onClick={() => revoke(binding)}>{copy.roles.revoke}</button>}</div></td></tr>; })}</tbody></table>{!pageItems.length && <div className="adm-empty">{copy.roles.noBindings}</div>}</div>
        <footer className="adm-pagination"><span>{copy.roles.pageSummary(bindingPage, pageCount, visibleBindings.length)}</span><div><button type="button" disabled={bindingPage <= 1} onClick={() => setBindingPage((value) => value - 1)}>{copy.roles.previous}</button><button type="button" disabled={bindingPage >= pageCount} onClick={() => setBindingPage((value) => value + 1)}>{copy.roles.nextPage}</button></div></footer>
      </>}
    </section>
    {grantRole && <GrantDrawer copy={copy} client={client} roles={roles} users={users} organizations={organizations} initialRole={grantRole.code ? grantRole : null} onClose={() => setGrantRole(null)} onGranted={async () => { await refreshBindings(); setMessage({ tone: 'ok', text: copy.roles.grantedNotice }); setTab('bindings'); }} />}
    {selectedBinding && <BindingDetail copy={copy} binding={selectedBinding} role={roleByCode.get(selectedBinding.role_code)} user={userByID.get(selectedBinding.user_id)} organization={organizationByID.get(selectedBinding.organization_id)} onClose={() => setSelectedBinding(null)} onRevoke={canAssign && canRevokeBinding(selectedBinding, roleByCode) ? revoke : null} />}
  </div>;
}
