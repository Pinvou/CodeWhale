import React from 'react';
import { Cpu, Layers, Server } from '../../components/icons.jsx';
import { StatusDot } from '../../components/StatusDot.jsx';
import { formatGiB } from './cms-view-model.js';

/** A summary of confirmed observations, independent from the chat's selected model. */
export function CmsOverview({ copy, status, models, capacity, loading, unavailableReason, uptime }) {
  const ready = (models?.models || []).filter(model => model.status === 'running' && model.healthy === true);
  const memory = !unavailableReason && capacity && Number.isFinite(capacity.totalGiB) && capacity.totalGiB > 0 && Number.isFinite(capacity.availableGiB);
  return <div className="cms-overview" data-testid="cms-overview">
    <section className="cms-stat">
      <h3><span aria-hidden="true"><Server size={15} /></span>{copy.managementServiceLabel}</h3>
      <p><StatusDot tone={unavailableReason ? 'warn' : status ? 'ok' : 'idle'} />
        {unavailableReason ? copy.unavailable : status ? copy.online : loading ? copy.loading : copy.queryFailed}</p>
      {status && !unavailableReason && <span>{copy.uptime} · {uptime}</span>}
    </section>
    <section className="cms-stat cms-stat-models">
      <h3><span aria-hidden="true"><Layers size={15} /></span>{copy.modelServices}</h3>
      <p>{!unavailableReason && models ? copy.readyServices(ready.length) : '—'}</p>
      <span>{!unavailableReason && models ? ready.map(model => model.modelId).join(' · ') || copy.noReadyServices : copy.queryFailed}</span>
    </section>
    <section className="cms-stat">
      <h3><span aria-hidden="true"><Cpu size={15} /></span>{copy.availableMemoryTitle}</h3>
      <p>{!unavailableReason && capacity ? formatGiB(capacity.availableGiB) : '—'}</p>
      {memory && <div className="cms-memory-meter" aria-hidden="true"><span style={{ width: `${Math.min(100, Math.max(0, capacity.availableGiB / capacity.totalGiB * 100))}%` }} /></div>}
      {capacity && !unavailableReason && <span>{copy.memoryTotal} · {formatGiB(capacity.totalGiB)}</span>}
    </section>
  </div>;
}
