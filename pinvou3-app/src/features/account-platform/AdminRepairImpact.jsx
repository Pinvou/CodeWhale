import React, { useId } from 'react';
import { integerMoney } from './finance-contract.js';
import { ledgerMicrocreditsToPoints } from './ledger-amount-input.js';

function snapshotValue(value, kind, words) {
  if (kind === 'missing' || value == null) return words.missing;
  if (kind === 'id') return typeof value === 'string' && value ? <bdi dir="ltr"><code>{value}</code></bdi> : words.missing;
  try {
    const exact = integerMoney(value);
    if (exact < 0n) return words.missing;
    if (kind === 'amount') return <><bdi dir="ltr">{exact.toString()}</bdi> {words.amount}</>;
    return <><bdi dir="ltr">{exact === 0n ? '0' : ledgerMicrocreditsToPoints(value)}</bdi> {words.unit}</>;
  } catch {
    return words.missing;
  }
}

export function AdminRepairImpact({ words, fields, snapshot }) {
  const title = useId();
  if (!words || !snapshot) return null;
  return <section className="adm-repair-relationship adm-repair-impact" aria-labelledby={title}>
    <h4 id={title}>{words.title}</h4>
    <p>{words.gate}</p>
    <dl>{fields.map(([key, kind], i) => <div key={key}><dt>{words.fields[i]}</dt><dd>{snapshotValue(snapshot[key], kind, words)}</dd></div>)}</dl>
  </section>;
}

export function AdminRepairApprovalReceipt({ words, receipt }) {
  if (!receipt) return null;
  const status = words && Object.prototype.hasOwnProperty.call(words.states, receipt.status) ? words.states[receipt.status] : words?.unknown;
  return <p className="adm-repair-approval-receipt" role="status">{words?.ref}: <bdi dir="ltr"><code>{receipt.approval_request_id}</code></bdi>. {words?.state}: {status}</p>;
}
