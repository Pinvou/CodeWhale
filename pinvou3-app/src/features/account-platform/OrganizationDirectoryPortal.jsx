import React, { useEffect, useState } from 'react';
import { accountLayoutCopy } from '../../shared/i18n.js';
import { PortalDialog } from './PortalDialog.jsx';

export function OrganizationDirectoryPortal({ copy, language, me, organizations, invitations, busy, error, token, onToken, canSwitch, canReadInvitations, canAccept, onSwitch, onAccept, onAcceptLink, onRefresh }) {
  const words = accountLayoutCopy[language];
  const current = me.active_context.context_type === 'ORGANIZATION' ? me.active_context.organization_id : '';
  const [dialog, setDialog] = useState(null), [selected, setSelected] = useState(current);
  useEffect(() => { setDialog(null); setSelected(current); }, [current]);
  const failure = error && <p className="ap-alert error" role="alert">{error}<button disabled={busy} onClick={onRefresh}>{copy.refresh}</button></p>;
  const label = current ? organizations.find(item => item.organization_id === current)?.name || current : copy.personal;
  return <section className="ap-context-settings">
    <div className="ap-profile-row"><span>{copy.context}</span><strong>{label}</strong>{canSwitch && <button type="button" disabled={busy} onClick={() => { setSelected(current); setDialog('context'); }}>{words.edit}</button>}</div>
    {error && <p className="ap-alert error" role="alert">{error}<button disabled={busy} onClick={onRefresh}>{copy.refresh}</button></p>}
    {(canReadInvitations || canAccept) && <section className="ap-invitations"><div className="ap-section-heading"><h3>{copy.invitations}</h3>{canAccept && <button type="button" disabled={busy} onClick={() => setDialog('invitation')}>{copy.accept}</button>}</div>{canReadInvitations && <>{!invitations.length && <p>{copy.empty}</p>}{invitations.map(invite => <article key={invite.invitation_id} className="ap-profile-row"><strong>{invite.organization_name}</strong><span>{invite.target_hint}</span>{canAccept && <button disabled={busy} onClick={() => onAccept(invite.invitation_id)}>{copy.accept}</button>}</article>)}</>}</section>}
    {dialog === 'context' && canSwitch && <PortalDialog title={copy.context} closeLabel={words.closeDialog} busy={busy} onClose={() => setDialog(null)}>{failure}<form onSubmit={event => { event.preventDefault(); onSwitch(selected || null); }}><label>{copy.context}<select disabled={busy} value={selected} onChange={event => setSelected(event.target.value)}><option value="">{copy.personal}</option>{organizations.map(org => <option key={org.organization_id} value={org.organization_id} disabled={org.status !== 'ACTIVE'}>{org.name}</option>)}</select></label><button disabled={busy || selected === current}>{words.confirmSwitch}</button></form></PortalDialog>}
    {dialog === 'invitation' && canAccept && <PortalDialog title={copy.invitations} closeLabel={words.closeDialog} busy={busy} onClose={() => setDialog(null)}>{failure}<form onSubmit={event => { event.preventDefault(); onAcceptLink(); }}><label>{copy.link}<input type="password" autoComplete="off" value={token} onChange={event => onToken(event.target.value)} /></label><button disabled={busy || !token.trim()}>{copy.accept}</button></form></PortalDialog>}
  </section>;
}
