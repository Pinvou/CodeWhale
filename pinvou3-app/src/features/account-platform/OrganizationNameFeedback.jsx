import React from 'react';

// Write acknowledgement and authoritative read are intentionally distinct.
export function OrganizationNameFeedback({ copy, kind = 'unit', unit, organization, refreshing, targetRef }) {
  const isOrganization = kind === 'organization', current = isOrganization ? organization : unit;
  const confirmed = isOrganization ? copy.organizationRenameConfirmed : copy.unitRenameConfirmed;
  const reading = isOrganization ? copy.organizationNameRefreshing : copy.nameRefreshing;
  const read = isOrganization ? copy.organizationNameReadConfirmed : copy.nameReadConfirmed;
  const unconfirmed = isOrganization ? copy.organizationNameReadUnconfirmed : copy.nameReadUnconfirmed;
  return <section className="ap-org-name-feedback"><p role="status">{confirmed}{refreshing ? reading : current ? <>{read}<strong>{current.name}</strong></> : unconfirmed}</p>
    {!isOrganization && unit && !refreshing && <button type="button" onClick={() => { const node = targetRef.current; if (node) { node.focus(); node.scrollIntoView?.({ block: 'nearest' }); } }}>{copy.viewRenamedUnit}</button>}
  </section>;
}
