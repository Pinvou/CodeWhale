import React, { useEffect, useRef, useState } from 'react';
import { platformRoleCopy } from '../../shared/i18n.js';
import { grantablePlatformRole, platformRoleGrantIntent, platformRolePending, platformRoleFreshAuthority, platformRoleFailureState } from './platform-role-management.js';

export function AdminRolesWorkspace({ client, me, language, onAuthorizationChange }) {
  const copy = platformRoleCopy[language];
  const owner = me?.user_id || '';
  const canRead = me?.platform_permissions?.includes('platform.roles.read');
  const canAssign = me?.platform_permissions?.includes('platform.roles.assign');
  const [roles, setRoles] = useState(null);
  const [bindings, setBindings] = useState(null);
  const [target, setTarget] = useState('');
  const [roleCode, setRoleCode] = useState('');
  const [starts, setStarts] = useState('');
  const [expires, setExpires] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [reauthRequired, setReauthRequired] = useState(false);
  const [pending, setPending] = useState(() => platformRolePending(client, owner));
  const generation = useRef(0);
  const currentOwner = useRef(owner);
  currentOwner.current = owner;
  const reads = useRef(null);
  const actionInFlight = useRef(false);
  const snapshot = () => ({ generation: generation.current, owner, epoch: client.financeSessionEpoch || 0 });
  const current = token => generation.current === token.generation && currentOwner.current === token.owner && (client.financeSessionEpoch || 0) === token.epoch;
  const message = failure => failure?.code === 'RECENT_AUTHENTICATION_REQUIRED' ? copy.reauth : failure?.status === 403 || failure?.message === 'ROLE_AUTHORITY_CHANGED' ? copy.denied : copy.failed;
  const load = async (selected = target) => {
    generation.current++; reads.current?.abort(); const controller = new AbortController(); reads.current = controller;
    const token = snapshot(); setBusy(true); setError('');
    try {
      const [catalog, items] = await Promise.all([client.platformRoles(controller.signal), client.platformRoleBindings(selected, controller.signal)]);
      if (!current(token)) return;
      setRoles(catalog); setBindings(items);
    } catch (failure) { if (current(token) && failure.name !== 'AbortError') { setRoles(null); setBindings(null); setError(message(failure)); } }
    finally { if (current(token)) setBusy(false); }
  };
  useEffect(() => {
    generation.current++; setRoles(null); setBindings(null); setPending(platformRolePending(client, owner));
    setTarget(''); setRoleCode(''); setStarts(''); setExpires(''); setReason('');
    if (canRead) void load('');
    return () => { generation.current++; reads.current?.abort(); };
  }, [client, owner, canRead]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => client.onAuthorizationVersion?.(() => {
    // Retire synchronously: the old response must not win before React renders
    // the parent's current-authority refresh. An uncertain command keeps its key.
    generation.current++; reads.current?.abort(); setRoles(null); setBindings(null);
    setBusy(false); onAuthorizationChange?.();
  }), [client, onAuthorizationChange]);
  const execute = async command => {
    if (actionInFlight.current || busy || !canAssign || command.owner !== owner) return;
    actionInFlight.current = true;
    const token = snapshot(); const wasUnknown = command.unknown === true;
    setBusy(true); setError(''); setReauthRequired(false);
    let dispatched = false;
    try {
      await platformRoleFreshAuthority(client, owner, token.epoch);
      if (!current(token)) return;
      const inFlight = Object.freeze({ ...command, unknown: true });
      platformRolePending(client, owner, inFlight); setPending(inFlight);
      dispatched = true;
      if (command.kind === 'revoke') await client.revokePlatformRole(owner, command.bindingID);
      else await client.grantPlatformRole(owner, command.body);
      if (!current(token)) return;
      platformRolePending(client, owner, null); setPending(null);
      // A successful authoritative ACK is kept even if the subsequent read fails.
      await load(target);
    } catch (failure) {
      if (!current(token)) return;
      const failureState = platformRoleFailureState(failure, wasUnknown, dispatched);
      if (failureState === 'UNKNOWN') {
        const unknown = Object.freeze({ ...command, unknown: true });
        platformRolePending(client, owner, unknown); setPending(unknown);
      } else { platformRolePending(client, owner, null); setPending(null); }
      setReauthRequired(failureState === 'REAUTH_NOT_DISPATCHED');
      setError(failureState === 'UNKNOWN' ? copy.unknown : message(failure));
    } finally { actionInFlight.current = false; if (current(token)) setBusy(false); }
  };
  const grant = event => {
    event.preventDefault(); if (pending || busy || !canAssign) return;
    try {
      const intent = platformRoleGrantIntent(owner, { user_id: target.trim(), role_code: roleCode, starts_at: starts, expires_at: expires, reason }, roles?.find(role => role.code === roleCode));
      if (globalThis.confirm(copy.confirm)) void execute(Object.freeze({ ...intent, kind: 'grant', unknown: false }));
    } catch { setError(copy.failed); }
  };
  const revoke = item => {
    if (pending || busy || !canAssign || !grantablePlatformRole(roles?.find(role => role.code === item.role_code)) || !['ACTIVE', 'SCHEDULED'].includes(item.status)) return;
    if (globalThis.confirm(copy.confirm)) void execute(Object.freeze({ owner, kind: 'revoke', bindingID: item.binding_id, unknown: false }));
  };
  const blocked = busy || Boolean(pending);
  return <section className="adm-panel"><h2>{copy.title}</h2><p>{copy.lead}</p>
    {!canRead ? <p role="alert">{copy.denied}</p> : <>
      <button disabled={busy} onClick={() => void load()}>{copy.refresh}</button>
      {!canAssign && <p>{copy.readOnly}</p>}{busy && <p role="status">{copy.loading}</p>}{error && <p role="alert">{error}</p>}{reauthRequired && !pending && <button disabled={busy} onClick={() => client.reauthenticate('/admin.html?section=roles')}>{copy.reauthAction}</button>}
      {pending && <aside role="status"><p>{copy.unknown}</p><p>{copy.reloadBoundary}</p><pre>{JSON.stringify(pending.kind === 'grant' ? pending.body : { binding_id: pending.bindingID })}</pre><button disabled={busy || !canAssign} onClick={() => void execute(pending)}>{copy.retry}</button></aside>}
      <label>{copy.target}<input value={target} disabled={blocked} onChange={event => { generation.current++; reads.current?.abort(); setBindings(null); setTarget(event.target.value); }} /></label>
      <button disabled={blocked} onClick={() => void load()}>{copy.refresh}</button>
      {roles && <><ul>{roles.map(role => <li key={role.code}><strong>{role.code}</strong>{!grantablePlatformRole(role) && <p>{copy.protected}</p>}<details><summary>{copy.permissions}</summary><ul>{role.permissions.map(permission => <li key={permission}>{permission}</li>)}</ul></details></li>)}</ul>
        {canAssign && <form onSubmit={grant}><label>{copy.role}<select required value={roleCode} disabled={blocked} onChange={event => setRoleCode(event.target.value)}><option value="">—</option>{roles.filter(grantablePlatformRole).map(role => <option key={role.code} value={role.code}>{role.code}</option>)}</select></label>
          <label>{copy.starts}<input type="datetime-local" value={starts} disabled={blocked} onChange={event => setStarts(event.target.value)} /></label><label>{copy.expires}<input type="datetime-local" value={expires} disabled={blocked} onChange={event => setExpires(event.target.value)} /></label><label>{copy.reason}<textarea value={reason} maxLength={500} disabled={blocked} onChange={event => setReason(event.target.value)} /></label>{roleCode === 'PLATFORM_AUDIT_PII_EXPORT' && <p>{copy.pii}</p>}<button disabled={blocked || !target.trim() || !roleCode}>{copy.grant}</button>
        </form>}
      </>}
      {bindings && <table><thead><tr>{[copy.binding, copy.user, copy.role, copy.status, copy.source, copy.starts, copy.expires].map((label, index) => <th key={index}>{label}</th>)}<th>{copy.revoke}</th></tr></thead><tbody>{bindings.map(item => <tr key={item.binding_id}><td>{item.binding_id}</td><td>{item.user_id}</td><td>{item.role_code}</td><td>{{ ACTIVE: copy.active, SCHEDULED: copy.scheduled, EXPIRED: copy.expired, REVOKED: copy.revoked }[item.status]}</td><td>{item.grant_source}</td><td>{item.starts_at}</td><td>{item.expires_at || '—'}</td><td>{canAssign && grantablePlatformRole(roles?.find(role => role.code === item.role_code)) && ['ACTIVE', 'SCHEDULED'].includes(item.status) && <button disabled={blocked} onClick={() => revoke(item)}>{copy.revoke}</button>}</td></tr>)}</tbody></table>}{bindings?.length === 0 && <p>{copy.empty}</p>}
    </>}
  </section>;
}
