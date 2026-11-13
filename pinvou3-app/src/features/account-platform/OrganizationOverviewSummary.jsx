import React, { useSyncExternalStore } from 'react';

const unitStatuses = new Set(['ACTIVE', 'DISABLED', 'DELETED']);

function activeUnits(organization, detail) {
  if (organization.status === 'DELETED' || detail.unitsError !== null || !Array.isArray(detail.units?.items)) return null;
  const ids = new Set();
  let count = 0;
  for (const unit of detail.units.items) {
    if (unit.organization_id !== organization.organization_id || !unit.unit_id || ids.has(unit.unit_id) || !unitStatuses.has(unit.status)) return null;
    ids.add(unit.unit_id);
    if (unit.status === 'ACTIVE') count++;
  }
  return count;
}

export function OrganizationOverviewSummary({ organization, detail, directoryPage, listBusy, detailBusy, error, overviewFeedback, labels, navigate }) {
  const overviewBlocked = useSyncExternalStore(overviewFeedback.subscribe, overviewFeedback.getBlocked, overviewFeedback.getBlocked);
  const words = labels.overviewFacts;
  const row = !listBusy && !error ? directoryPage?.items.find(item => item.organization_id === organization.organization_id) : null;
  const memberCount = organization.status !== 'DELETED' && row?.status === organization.status && row != null && Number.isSafeInteger(row.member_count) && row.member_count >= 0 ? row.member_count : null;
  const units = detailBusy ? null : activeUnits(organization, detail);
  const deleted = organization.status === 'DELETED';
  const blockedFact = overviewBlocked === 'session' ? [words.sessionUnavailable, words.sessionSource] : overviewBlocked === 'checking-session' ? [words.sessionChecking, words.sessionCheckingSource] : overviewBlocked === 'session-unconfirmed' ? [words.sessionUnconfirmed, words.sessionUnconfirmedSource] : overviewBlocked === 'permission' ? [words.readDenied, words.deniedSource] : overviewBlocked === 'unconfirmed' ? [words.readUnconfirmed, words.unconfirmedSource] : null;
  const memberState = deleted ? [words.unavailable, words.deletedSource] : blockedFact || (listBusy ? [words.loading, words.directoryLoading] : error ? [words.readFailed, words.directoryFailed] : [memberCount ?? words.unavailable, memberCount === null ? words.membersUnavailable : words.directorySnapshot]);
  const unitState = deleted ? [words.unavailable, words.deletedSource] : blockedFact || (detailBusy ? [words.loading, words.unitsLoading] : detail.unitsError ? [words.readFailed, words.unitsFailed] : [units ?? words.unavailable, units === null ? words.unitsUnavailable : words.unitsSource]);
  return <section className="adm-organization-overview" aria-label={labels.overview}>
    <dl className="adm-organization-overview-facts">
      <div><dt>{words.activeMembers}</dt><dd><strong>{memberState[0]}</strong><small>{memberState[1]}</small>{!deleted && !blockedFact && <button type="button" className="adm-organization-overview-members" onClick={() => navigate({ organization_tab: 'members' })}>{labels.members}</button>}</dd></div>
      <div><dt>{words.activeUnits}</dt><dd><strong>{unitState[0]}</strong><small>{unitState[1]}</small></dd></div>
    </dl>
    <details className="adm-organization-overview-reference"><summary>{words.technical}</summary><dl className="adm-user-details"><dt>{labels.version}</dt><dd>{organization.version}</dd></dl></details>
  </section>;
}
