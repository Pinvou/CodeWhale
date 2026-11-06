import React from 'react';

// Write acknowledgement and authoritative read are intentionally distinct.
export function OrganizationNameFeedback({ copy, unit, refreshing, targetRef }) {
  return <section className="ap-org-name-feedback"><p role="status">{copy.unitRenameConfirmed}{refreshing ? copy.nameRefreshing : unit ? <>{copy.nameReadConfirmed}<strong>{unit.name}</strong></> : copy.nameReadUnconfirmed}</p>
    {unit && !refreshing && <button type="button" onClick={() => { const node = targetRef.current; if (node) { node.focus(); node.scrollIntoView?.({ block: 'nearest' }); } }}>{copy.viewRenamedUnit}</button>}
  </section>;
}
