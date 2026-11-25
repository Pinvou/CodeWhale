import React from 'react';
import { Cpu, Package } from '../../components/icons.jsx';
import { StatusDot } from '../../components/StatusDot.jsx';

/** Runtime identifiers and paths remain inspectable without becoming the main label. */
export function CmsEngineRow({ engine, copy }) {
  const native = engine.engineType !== 'container';
  const name = native ? /(?:^|[/\\])tachyon-serve(?:[/\\]|$)/i.test(engine.path) ? copy.tachyonEngine : copy.nativeRuntime : engine.id;
  const Icon = native ? Cpu : Package;
  return <article className="cms-task cms-engine" data-testid="cms-engine-row">
    <div className="cms-heading"><span className="cms-icon-tile cms-icon-tile-small" aria-hidden="true"><Icon size={16} /></span>
      <h4>{name}</h4><span className="cms-badge">{native ? copy.nativeProcess : copy.containerRuntime}</span></div>
    {engine.instances.length === 0 && <p className="cms-muted mt-2 text-xs">{copy.engineIdle}</p>}
    {engine.instances.map((instance, index) => <div key={index} className="mt-3 text-xs">
      {instance.modelId && <p className="mb-2 font-medium">{instance.modelId}</p>}
      <p className="flex flex-wrap items-center gap-2"><StatusDot tone={instance.alive === false ? 'idle' : instance.healthy === true ? 'ok' : instance.healthy === false ? 'fail' : 'idle'} />
        <span>{instance.source === 'external' ? copy.sourceExternal : copy.sourceManaged}</span>
        <span className="cms-muted">{instance.alive === false ? copy.status.stopped : instance.healthy === true ? copy.healthy : instance.healthy === false ? copy.unhealthy : copy.healthUnknown}</span>
      </p>
    </div>)}
    <details className="cms-details mt-2"><summary>{copy.technicalDetails}</summary>
      <dl className="cms-engine-details">
        <dt>{copy.engineIdentifier}</dt><dd><code>{engine.id}</code></dd>
        <dt>{copy.engineLocation}</dt><dd><code>{engine.path || '—'}</code></dd>
        {engine.instances.map((instance, index) => <React.Fragment key={index}><dt>PID / {copy.port}</dt><dd>{instance.pid ?? '—'} / {instance.port ?? '—'}</dd></React.Fragment>)}
      </dl>
    </details>
  </article>;
}
