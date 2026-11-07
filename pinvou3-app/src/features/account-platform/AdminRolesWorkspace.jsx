import React, { useEffect, useRef, useState } from 'react';
import { adminRolePresentationCopy, platformRoleCopy } from '../../shared/i18n.js';
import { grantablePlatformRole, platformRoleGrantIntent, platformRolePending, platformRoleFreshAuthority, platformRoleFailureState } from './platform-role-management.js';
import { PlatformRoleCatalog } from './PlatformRoleCatalog.jsx';
import { PlatformRoleGrantDialog } from './PlatformRoleGrantDialog.jsx';
import { PlatformRoleBindingDetail } from './PlatformRoleBindingDetail.jsx';
import { PlatformRoleBindings } from './PlatformRoleBindings.jsx';
import { requireFinanceID } from './finance-contract.js';
import { platformRoleName } from './platform-role-presentation.js';
import './admin-role-presentation.css';

export function AdminRolesWorkspace({ client, me, language, onAuthorizationChange, initialTarget = '', route, acceptedSessionEpoch }) {
  const copy = platformRoleCopy[language];
  const words = adminRolePresentationCopy[language];
  const owner = me?.user_id || '';
  const canRead = me?.platform_permissions?.includes('platform.roles.read');
  const canAssign = me?.platform_permissions?.includes('platform.roles.assign');
  const [roles, setRoles] = useState(null);
  const [bindings, setBindings] = useState(null);
  const [target, setTarget] = useState(initialTarget);
  const [grantTarget, setGrantTarget] = useState(initialTarget);
  const [roleCode, setRoleCode] = useState('');
  const [starts, setStarts] = useState('');
  const [expires, setExpires] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [reauthRequired, setReauthRequired] = useState(false);
  const [pending, setPending] = useState(() => platformRolePending(client, owner));
  const [view, setView] = useState(initialTarget ? 'bindings' : 'catalog');
  const [grantOpen, setGrantOpen] = useState(false), [confirmation, setConfirmation] = useState(null);
  const [completedGrant, setCompletedGrant] = useState(null);
  const [selectedBinding, setSelectedBinding] = useState(null);
  const recovery = useRef(null);
  const dialogContext = useRef(null);
  const generation = useRef(0);
  const commandGeneration = useRef(0);
  const currentOwner = useRef(owner);
  currentOwner.current = owner;
  const currentClient = useRef(client), appliedTarget = useRef(target);
  currentClient.current = client; appliedTarget.current = route ? initialTarget : target;
  const reads = useRef(null);
  const actionInFlight = useRef(false);
  const snapshot = () => ({ generation: generation.current, owner, epoch: client.financeSessionEpoch || 0 });
  const current = token => generation.current === token.generation && currentOwner.current === token.owner && currentClient.current === client && (client.financeSessionEpoch || 0) === token.epoch;
  // List navigation retires reads, not a same-session authoritative command
  // receipt. Identity/authority changes still retire both independent fences.
  const commandCurrent = token => commandGeneration.current === token.generation && currentOwner.current === token.owner && currentClient.current === client && (client.financeSessionEpoch || 0) === token.epoch;
  const message = failure => failure?.code === 'RECENT_AUTHENTICATION_REQUIRED' ? copy.reauth : failure?.status === 403 || failure?.message === 'ROLE_AUTHORITY_CHANGED' ? copy.denied : copy.failed;
  const load = async (selected = target) => {
    generation.current++; reads.current?.abort(); const controller = new AbortController(); reads.current = controller;
    const token = snapshot(); setBusy(true); setError('');
    try {
      const [catalog, items] = await Promise.all([client.platformRoles(controller.signal), client.platformRoleBindings(selected, controller.signal)]);
      if (!current(token)) return;
      setRoles(catalog); setBindings(items);
    } catch (failure) { if (current(token) && failure.name !== 'AbortError') { setRoles(null); setBindings(null); setError(failure?.status === 403 ? copy.denied : copy.loadFailed); } }
    finally { if (current(token)) setBusy(false); }
  };
  useEffect(() => {
    generation.current++; commandGeneration.current++; setRoles(null); setBindings(null); setPending(platformRolePending(client, owner));
    setGrantOpen(false); setConfirmation(null); setCompletedGrant(null); setSelectedBinding(null); setNotice(''); setGrantTarget(initialTarget);
    setTarget(initialTarget); setRoleCode(''); setStarts(''); setExpires(''); setReason('');
    if (canRead) void load(initialTarget);
    return () => { generation.current++; commandGeneration.current++; reads.current?.abort(); };
  }, [client, owner, canRead]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => client.onAuthorizationVersion?.(() => {
    // Retire synchronously: the old response must not win before React renders
    // the parent's current-authority refresh. An uncertain command keeps its key.
    generation.current++; commandGeneration.current++; reads.current?.abort(); setRoles(null); setBindings(null);
    setGrantOpen(false); setConfirmation(null); setSelectedBinding(null); setNotice('');
    setBusy(false); onAuthorizationChange?.();
  }), [client, onAuthorizationChange]);
  useEffect(() => {
    if (initialTarget !== target) { setTarget(initialTarget); setBindings(null); if (canRead) void load(initialTarget); }
  }, [initialTarget]); // URL reads retire old read receipts, never replace an accepted command.
  const execute = async command => {
    if (actionInFlight.current || busy || !canAssign || command.owner !== owner) return;
    actionInFlight.current = true;
    const token = { generation: commandGeneration.current, owner, epoch: client.financeSessionEpoch || 0 }; const wasUnknown = command.unknown === true;
    setBusy(true); setError(''); setNotice(''); setReauthRequired(false);
    let dispatched = false;
    try {
      await platformRoleFreshAuthority(client, owner, token.epoch);
      if (!commandCurrent(token)) return;
      const inFlight = Object.freeze({ ...command, unknown: true });
      platformRolePending(client, owner, inFlight); setPending(inFlight);
      dispatched = true;
      const acknowledged = command.kind === 'revoke' ? await client.revokePlatformRole(owner, command.bindingID) : await client.grantPlatformRole(owner, command.body);
      if (!commandCurrent(token)) return;
      platformRolePending(client, owner, null); setPending(null);
      setNotice({ status: acknowledged.status, owner, client });
      setSelectedBinding(null);
      if (command.kind === 'grant') { setGrantOpen(false); setConfirmation(null); }
      // A successful authoritative ACK is kept even if the subsequent read fails.
      if (command.kind === 'grant') {
        await load(appliedTarget.current);
        if (commandCurrent(token)) setCompletedGrant({ owner: command.owner, target: command.body.user_id, epoch: token.epoch, originSearch: route?.search });
      } else await load(appliedTarget.current);
    } catch (failure) {
      if (!commandCurrent(token)) return;
      setSelectedBinding(null);
      if (command.kind === 'grant') { setGrantOpen(false); setConfirmation(null); }
      const failureState = platformRoleFailureState(failure, wasUnknown, dispatched);
      if (failureState === 'UNKNOWN') {
        const unknown = Object.freeze({ ...command, unknown: true });
        platformRolePending(client, owner, unknown); setPending(unknown);
      } else { platformRolePending(client, owner, null); setPending(null); }
      setReauthRequired(failureState === 'REAUTH_NOT_DISPATCHED');
      setError(failureState === 'UNKNOWN' ? copy.unknown : message(failure));
      if (failure?.message === 'ROLE_AUTHORITY_CHANGED') onAuthorizationChange?.();
    } finally { actionInFlight.current = false; if (commandCurrent(token)) setBusy(false); }
  };
  const reviewGrant = () => {
    if (pending || busy || !canAssign) return false;
    try {
      const intent = platformRoleGrantIntent(owner, { user_id: grantTarget.trim(), role_code: roleCode, starts_at: starts, expires_at: expires, reason }, roles?.find(role => role.code === roleCode));
      setConfirmation(Object.freeze({ ...intent, kind: 'grant', unknown: false })); setError(''); return true;
    } catch { setError(copy.failed); return false; }
  };
  const revoke = item => {
    if (pending || busy || !canAssign || !grantablePlatformRole(roles?.find(role => role.code === item.role_code)) || !['ACTIVE', 'SCHEDULED'].includes(item.status)) return;
    if (globalThis.confirm(copy.confirm)) void execute(Object.freeze({ owner, kind: 'revoke', bindingID: item.binding_id, unknown: false }));
  };
  const blocked = busy || Boolean(pending);
  const tab = route ? new URLSearchParams(route.search).get('role_view') === 'bindings' || !new URLSearchParams(route.search).has('role_view') && initialTarget ? 'bindings' : 'catalog' : view;
  const changeView = next => { setView(next); route?.navigate('roles', { role_view: next }); };
  const update = (field, value) => {
    if (blocked) return;
    setConfirmation(null);
    if (field === 'target') setGrantTarget(value);
    else ({ roleCode: setRoleCode, starts: setStarts, expires: setExpires, reason: setReason })[field]?.(value);
  };
  const applyBindingTarget = value => {
    if (blocked) return;
    const selected = value.trim().toLowerCase();
    try { if (selected) requireFinanceID(selected); } catch { setError(copy.failed); return; }
    if (route && (new URLSearchParams(route.search).get('role_user_id') || '') !== selected) {
      route.navigate('roles', { role_user_id: selected || null, role_view: 'bindings' });
    } else { setTarget(selected); setBindings(null); void load(selected); }
  };
  const openGrant = (code = '') => { if (!blocked && canAssign) { dialogContext.current = { owner, client }; setRoleCode(code); setGrantTarget(target); setStarts(''); setExpires(''); setReason(''); setConfirmation(null); setError(''); setNotice(''); setGrantOpen(true); } };
  const openBinding = item => { dialogContext.current = { owner, client }; setSelectedBinding(item); };
  const ownsDialog = dialogContext.current?.owner === owner && dialogContext.current?.client === client;
  const ownsPending = pending && platformRolePending(client, owner) === pending;
  useEffect(() => {
    if (!completedGrant || grantOpen) return;
    if (completedGrant.owner === owner && completedGrant.epoch === (client.financeSessionEpoch || 0) && (!route || route.section === 'roles' && route.search === completedGrant.originSearch)) {
      setView('bindings'); route?.navigate('roles', { role_view: 'bindings', role_user_id: completedGrant.target });
    }
    setCompletedGrant(null);
  }, [completedGrant, grantOpen, owner, client, route]);
  useEffect(() => { if (pending && !busy && !grantOpen) recovery.current?.focus(); }, [pending, busy, grantOpen]);
  return <div className="adm-role-workspace adm-platform-roles">
    <p className="adm-muted">{copy.lead}</p>
    {!canRead ? <p role="alert">{copy.denied}</p> : <>
      {roles && <div className="adm-role-summary"><article><span>{words.catalog}</span><strong>{roles.length}</strong><small>{words.scopes.PLATFORM}</small></article><article><span>{words.permissionCount}</span><strong>{new Set(roles.flatMap(role => role.permissions)).size}</strong><small>{words.includedPermissions}</small></article><article><span>{words.bindingManagement}</span><strong>{bindings ? bindings.length : '—'}</strong><small title={target || undefined}>{target || words.scopes.PLATFORM}</small></article></div>}
      {notice?.owner === owner && notice?.client === client && <p className="adm-inline-message ok" role="status">{notice.status === 'REVOKED' ? words.revokedNotice : notice.status === 'EXPIRED' ? words.expiredNotice : words.grantedNotice}</p>}
      {!canAssign && <p className="adm-inline-message">{copy.readOnly}</p>}{busy && <p role="status">{copy.loading}</p>}{!grantOpen && error && <p className="adm-inline-message error" role="alert">{error}</p>}{reauthRequired && !pending && <button disabled={busy} onClick={() => client.reauthenticate('/admin.html?section=roles')}>{copy.reauthAction}</button>}
      {ownsPending && <aside className="adm-role-risk-note" role="status"><p>{copy.unknown}</p><p>{pending.kind === 'grant' ? `${words.user}: ${pending.body.user_id} · ${words.role}: ${platformRoleName(words, pending.body.role_code)}` : `${words.bindingID}: ${pending.bindingID}`}</p><p>{copy.reloadBoundary}</p><details><summary>{words.bindingDetail}</summary><pre>{JSON.stringify(pending.kind === 'grant' ? pending.body : { binding_id: pending.bindingID }, null, 2)}</pre></details><button ref={recovery} disabled={busy || !canAssign} onClick={() => void execute(pending)}>{copy.retry}</button></aside>}
      <section className="adm-panel adm-role-panel"><div className="adm-role-workspace-head"><div className="adm-tabs" role="group" aria-label={copy.title}><button type="button" aria-pressed={tab === 'catalog'} onClick={() => changeView('catalog')}>{words.catalog}</button><button type="button" aria-pressed={tab === 'bindings'} onClick={() => changeView('bindings')}>{words.bindingManagement}</button></div><div><button type="button" disabled={busy} onClick={() => void load()}>{copy.refresh}</button>{canAssign && <button type="button" className="adm-role-primary" disabled={blocked || !roles} onClick={() => openGrant()}>{words.newGrant}</button>}</div></div>
        <div hidden={tab !== 'catalog'}>{roles && <PlatformRoleCatalog key={owner} {...{ client, me, language, acceptedSessionEpoch }} roles={roles} words={words} authorityCopy={copy} canGrant={grantablePlatformRole} onGrant={canAssign && !blocked ? openGrant : null} />}</div>
        <div hidden={tab !== 'bindings'}><PlatformRoleBindings key={owner} {...{ words, copy, bindings, target, blocked }} onApplyTarget={applyBindingTarget} onView={openBinding} onRevoke={revoke} canRevoke={item => canAssign && grantablePlatformRole(roles?.find(role => role.code === item.role_code)) && ['ACTIVE', 'SCHEDULED'].includes(item.status)} /></div>
      </section>
      {ownsDialog && grantOpen && roles && canAssign && (!pending || busy) && <PlatformRoleGrantDialog key={owner} {...{ words, copy, client, owner, roleCode, starts, expires, reason, update, error, confirmation }} target={grantTarget} canSearchUsers={me?.platform_permissions?.includes('platform.users.read')} roles={roles.filter(grantablePlatformRole)} busy={busy} onReview={reviewGrant} onConfirm={() => { if (confirmation && !pending) void execute(confirmation); }} onClose={() => setGrantOpen(false)} />}
      {ownsDialog && selectedBinding && <PlatformRoleBindingDetail binding={selectedBinding} {...{ words, copy, busy }} onClose={() => setSelectedBinding(null)} onRevoke={!pending && canAssign && grantablePlatformRole(roles?.find(role => role.code === selectedBinding.role_code)) && ['ACTIVE', 'SCHEDULED'].includes(selectedBinding.status) ? () => revoke(selectedBinding) : null} />}
    </>}
  </div>;
}
