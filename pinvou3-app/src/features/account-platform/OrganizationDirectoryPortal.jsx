import React, { useEffect, useRef, useState } from 'react';
import { accountLayoutCopy, accountCenterCopy } from '../../shared/i18n.js';
import { PortalDialog } from './PortalDialog.jsx';
import { PortalSectionAction } from './PortalSectionAction.jsx';

export function OrganizationDirectoryPortal({ copy, language, me, organizations, invitations, busy, error, token, onToken, canSwitch, canReadInvitations, canAccept, onSwitch, onAccept, onAcceptLink, onRefresh, switchRequest = 0 }) {
  const words = accountLayoutCopy[language];
  const center = accountCenterCopy[language];
  const current = me.active_context.context_type === 'ORGANIZATION' ? me.active_context.organization_id : '';
  const [dialog, setDialog] = useState(null), [selected, setSelected] = useState(current);
  const handledSwitch = useRef(0);
  useEffect(() => { setDialog(null); setSelected(current); }, [current]);
  useEffect(() => { if (canSwitch && switchRequest > handledSwitch.current) { handledSwitch.current = switchRequest; setSelected(current); setDialog('context'); } }, [canSwitch, current, switchRequest]);
  const failure = error && <p className="ap-alert error" role="alert">{error}<button disabled={busy} onClick={onRefresh}>{copy.refresh}</button></p>;
  const label = current ? organizations.find(item => item.organization_id === current)?.name || center.organization : copy.personal;
  return <section className="ap-context-settings">
    <div className="ap-profile-row ap-current-context-row"><span>{words.currentContext}</span><strong>{label}</strong>{canSwitch && <PortalSectionAction icon="switch" disabled={busy} onClick={() => { setSelected(current); setDialog('context'); }}>{center.switchIdentity}</PortalSectionAction>}</div>
    <p className="ap-muted">{center.identityLead}</p>
    {error && <p className="ap-alert error" role="alert">{error}<button disabled={busy} onClick={onRefresh}>{copy.refresh}</button></p>}
    {(canReadInvitations || canAccept) && <section className="ap-invitations"><div className="ap-section-heading"><h3>{center.receivedInvitations}</h3>{canAccept && <PortalSectionAction icon="invite" disabled={busy} onClick={() => setDialog('invitation')}>{center.enterInvitation}</PortalSectionAction>}</div>{canReadInvitations && <>{!invitations.length && <p className="ap-empty">{center.noInvitations}</p>}{invitations.map(invite => <article key={invite.invitation_id} className="ap-profile-row"><strong>{invite.organization_name}</strong><span>{invite.target_hint}</span>{canAccept && <button disabled={busy} onClick={() => onAccept(invite.invitation_id)}>{copy.accept}</button>}</article>)}</>}</section>}
    {dialog === 'context' && canSwitch && <PortalDialog title={center.switchIdentity} closeLabel={words.closeDialog} busy={busy} onClose={() => setDialog(null)}>{failure}<p>{center.identityLead}</p><form onSubmit={event => { event.preventDefault(); onSwitch(selected || null); }}><label>{copy.context}<select disabled={busy} value={selected} onChange={event => setSelected(event.target.value)}><option value="">{copy.personal}</option>{organizations.map(org => <option key={org.organization_id} value={org.organization_id} disabled={org.status !== 'ACTIVE'}>{org.name}</option>)}</select></label><button className="ap-primary" disabled={busy || selected === current}>{words.confirmSwitch}</button></form></PortalDialog>}
    {dialog === 'invitation' && canAccept && <PortalDialog title={center.enterInvitation} closeLabel={words.closeDialog} busy={busy} onClose={() => setDialog(null)}>{failure}<form onSubmit={event => { event.preventDefault(); onAcceptLink(); }}><label>{center.invitationCode}<input type="password" autoComplete="off" value={token} onChange={event => onToken(event.target.value)} /></label><button className="ap-primary" disabled={busy || !token.trim()}>{copy.accept}</button></form></PortalDialog>}
  </section>;
}
