import React, { useId } from 'react';
import './admin-repair-relationship.css';

export function AdminRepairRelationship({ words, projection, reading, pending, normalizedKeys, snapshotKeys }) {
  const title = useId();
  if (!words) return null;
  const values = projection && !reading && !pending
    ? [...normalizedKeys.map(key => projection.normalized_command[key]), ...snapshotKeys.map(key => projection.financial_snapshot[key])]
    : null;
  return <section className="adm-repair-relationship" aria-labelledby={title}>
    <h4 id={title}>{words.title}</h4>
    <p role="status">{reading ? words.reading : pending ? words.pending : values ? words.matched : words.waiting}</p>
    {values && <><dl>{values.map((value, index) => <div key={index}>
      <dt>{words.fields[index]}</dt><dd><bdi dir="ltr"><code>{String(value)}</code></bdi></dd>
    </div>)}</dl><p>{words.limit}</p></>}
  </section>;
}
