import React from 'react';

const element = React.createElement;
const empty = (copy) => element('div', { className: 'ap-empty' }, copy);

export function PendingInvitations({ copy, language, items, unavailable, busy, canAccept, onAccept }) {
  const content = unavailable
    ? empty(copy.pendingInvitationsUnavailable)
    : items.length
      ? element('ul', { className: 'ap-list' }, items.map((invitation) => {
        const organizationName = String(invitation.organization_name || '').trim();
        const recoveredOperation = !invitation.organization_id;
        const details = [
          element('span', { key: 'name' }, recoveredOperation ? copy.invitationAcceptanceRecovery : organizationName || copy.invitationOrganization(invitation.organization_id)),
        ];
        if (!recoveredOperation && organizationName) details.push(element('small', { key: 'organization' }, copy.invitationOrganizationReference(invitation.organization_id)));
        if (!recoveredOperation) details.push(element('small', { key: 'roles' }, copy.invitationRoles((invitation.roles || []).join(', '))));
        if (!recoveredOperation) details.push(element('small', { key: 'expiry' }, copy.invitationExpiresAt(new Date(invitation.expires_at).toLocaleString(language))));
        if (canAccept) details.push(element('button', { key: 'accept', type: 'button', disabled: busy, onClick: () => onAccept(invitation) }, copy.acceptInvitation));
        return element('li', { key: invitation.invitation_id }, details);
      }))
      : empty(copy.noPendingInvitations);
  return element('article', { className: 'ap-card ap-combined-section', 'data-testid': 'personal-pending-invitations' },
    element('h3', null, copy.pendingInvitationsTitle),
    element('p', null, copy.pendingInvitationsLead),
    content,
  );
}
