import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy } from '../../shared/i18n.js';
import { OrganizationClient } from './organization-client.js';
import { hasPermission } from './authorization-ui.js';
import { OrganizationDirectoryPortal } from './OrganizationDirectoryPortal.jsx';

export function OrganizationDirectory({ client, me, language, onChanged, portalPresentation = false, onOrganizationsRead, switchRequest = 0, refreshVersion = 0 }) {
 const copy = organizationCopy[language];
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const [organizations, setOrganizations] = useState([]);
 const [invitations, setInvitations] = useState([]);
 const [token, setToken] = useState('');
 const [busy, setBusy] = useState(false);
 const [error, setError] = useState('');
 const generation = useRef(0);
 const owner = `${me.user_id}:${me.active_context.context_version}:${me.personal_authorization_version || 0}:${me.active_context.organization_authorization_version || 0}:${client.financeSessionEpoch || 0}`;
 useEffect(() => { setToken(''); }, [api, owner]);
 useEffect(() => {
  const stamp = ++generation.current, controller = new AbortController(); setBusy(true); setError(''); setOrganizations([]); setInvitations([]);
  const load = async () => {
   const orgs = await api.organizations(controller.signal);
   const invites = hasPermission(me, 'personal.invitations.read') ? await api.invitations(controller.signal) : [];
   if (stamp === generation.current) { setOrganizations(orgs); setInvitations(invites); onOrganizationsRead?.(orgs); }
  };
  void load().catch(reason => { if (stamp === generation.current && reason.name !== 'AbortError') setError(reason.status === 403 ? copy.permissions : copy.unavailable); }).finally(() => { if (stamp === generation.current) setBusy(false); });
  return () => { generation.current++; controller.abort(); };
 }, [api, owner, refreshVersion]); // eslint-disable-line react-hooks/exhaustive-deps
 const perform = async (action, kind) => {
  if (busy) return;
  const stamp = generation.current; let confirmed = false; setBusy(true); setError('');
  try { await action(); confirmed = true; if (stamp === generation.current) await onChanged(); }
  catch (reason) {
   if (stamp !== generation.current) return;
   if (confirmed) setError(kind === 'invitation' ? copy.invitationAcceptedRefreshUnknown : copy.contextUnknown);
   else if (reason.financeRequestState === 'WRITE_OBSERVED') setError(kind === 'invitation' ? copy.invitationUnknown : copy.contextUnknown);
   else if (reason.status === 401 || reason.status === 403) setError(copy.permissions);
   else if (reason.status === 400 || reason.message === 'INVALID_ORGANIZATION_INPUT') setError(copy.invalid);
   else if (reason.status === 404 || reason.status === 410) setError(kind === 'invitation' ? copy.invitationUnavailable : copy.sourceChanged);
   else if (reason.status === 409) setError(copy.sourceChanged);
   else setError(kind === 'invitation' ? copy.invitationUnknown : copy.contextUnknown);
  }
  finally { if (stamp === generation.current) setBusy(false); }
 };
 if (portalPresentation) return <OrganizationDirectoryPortal copy={copy} language={language} me={me} organizations={organizations} invitations={invitations} busy={busy} error={error} token={token} onToken={setToken} switchRequest={switchRequest}
  canSwitch={hasPermission(me, 'personal.context.switch')} canReadInvitations={hasPermission(me, 'personal.invitations.read')} canAccept={hasPermission(me, 'personal.invitations.accept')}
  onSwitch={id => void perform(() => api.switchContext(id, me.active_context.context_version), 'context')}
  onAccept={id => void perform(() => api.acceptInvitation(id), 'invitation')} onAcceptLink={() => void perform(() => api.acceptLink(token), 'invitation')} onRefresh={() => void onChanged()} />;
 return <section><h2>{copy.context}</h2>{error && <p role="alert">{error}<button onClick={() => void onChanged()} disabled={busy}>{copy.refresh}</button></p>}
  {hasPermission(me, 'personal.context.switch') && <><button disabled={busy || me.active_context.context_type === 'PERSONAL'} onClick={() => void perform(() => api.switchContext(null, me.active_context.context_version), 'context')}>{copy.personal}</button>{organizations.map(org => <button key={org.organization_id} disabled={busy || org.status !== 'ACTIVE' || org.organization_id === me.active_context.organization_id} onClick={() => void perform(() => api.switchContext(org.organization_id, me.active_context.context_version), 'context')}>{org.name}</button>)}</>}
  {hasPermission(me, 'personal.invitations.read') && <div><h3>{copy.invitations}</h3>{invitations.map(invite => <p key={invite.invitation_id}>{invite.organization_name} · {invite.target_hint} · {invite.expires_at}{hasPermission(me, 'personal.invitations.accept') && <button disabled={busy} onClick={() => void perform(() => api.acceptInvitation(invite.invitation_id), 'invitation')}>{copy.accept}</button>}</p>)}{!invitations.length && <p>{copy.empty}</p>}</div>}
  {hasPermission(me, 'personal.invitations.accept') && <form onSubmit={event => { event.preventDefault(); void perform(() => api.acceptLink(token), 'invitation'); }}><label>{copy.link}<input type="password" autoComplete="off" value={token} onChange={event => setToken(event.target.value)} /></label><button disabled={busy || !token.trim()}>{copy.accept}</button></form>}
 </section>;
}
