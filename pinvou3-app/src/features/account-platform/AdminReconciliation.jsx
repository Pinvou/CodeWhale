import React, { useState } from 'react';
import { useAdminOperation } from './use-admin-operation.js';
import { requireAdminItems, requireAdminRecord } from './admin-acknowledgements.js';
import { AdminFinancialEvidence } from './AdminFinancialOperations.jsx';

export function AdminReconciliationCases({ initial, client, copy, onBack, onAuthorizationError }) {
  const ops = useAdminOperation(onAuthorizationError, 'billing');
  const [items, setItems] = useState(initial.items);
  const [selected, setSelected] = useState(null);
  const command = (item, action) => {
    const reason = String(window.prompt(copy.billing.caseCommandReason) || '').trim();
    if ([...reason].length < 10) return;
    const input = { action, reason };
    if (action === 'ASSIGN') {
      input.owner_id = String(window.prompt(copy.billing.ownerPrompt) || '').trim();
      if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(input.owner_id)) return;
    }
    if (action === 'SET_REPAIR_ACTION') {
      input.repair_action = String(window.prompt(copy.billing.repairActionPrompt) || '').trim();
      if (!input.repair_action) return;
    }
    if (action === 'RESOLVE' || action === 'IGNORE') {
      input.review_result = String(window.prompt(copy.billing.reviewResultPrompt) || '').trim();
      if (!input.review_result) return;
    }
    void ops.run(async ({ signal, isCurrent }) => {
      requireAdminRecord(await client.updateReconciliationCase(item.case_id, input), 'case_id', item.case_id);
      const current = requireAdminItems(await client.reconciliationCases(initial.run.reconciliation_run_id, signal));
      if (isCurrent()) { setItems(current); setSelected(current.find(value => value.case_id === item.case_id) || null); }
      return 'completed';
    }, true);
  };
  const labels = { CLAIM:copy.billing.claimReview, ASSIGN:copy.billing.assignOwner, SET_REPAIR_ACTION:copy.billing.setRepairAction, RESOLVE:copy.billing.resolveCase, IGNORE:copy.billing.ignoreCase };
  return <section className="adm-panel">
    <button className="adm-back" disabled={ops.busy} onClick={onBack}>← {copy.billing.back}</button><h2>{copy.billing.cases} · {initial.run.business_date}</h2>
    {ops.failed && <p role="alert">{copy.billing.actionFailed}</p>}{ops.noticeKey && <p role="status">{copy.billing.completed}</p>}
    {!items.length && <p>{copy.noData}</p>}
    {items.map(item => <article key={item.case_id}>
      <code>{item.case_id} · {item.merchant_order_no}</code><p>{item.case_type} · {item.status} · {item.severity} · {copy.billing.owner}: {item.owner_id || '—'}</p>
      <p>{copy.billing.repairAction}: {item.repair_action || '—'} · {copy.billing.reviewResult}: {item.review_result || '—'}</p>
      <div className="adm-row-actions"><button disabled={ops.busy} onClick={() => setSelected(item)}>{copy.billing.viewEvidence}</button>
        {!['RESOLVED','IGNORED'].includes(item.status) && Object.entries(labels).map(([action, label]) => <button key={action} disabled={ops.busy} onClick={() => command(item, action)}>{label}</button>)}
      </div>
    </article>)}
    {selected && <><AdminFinancialEvidence title={copy.billing.providerSnapshot} value={selected.provider_snapshot} /><AdminFinancialEvidence title={copy.billing.internalSnapshot} value={selected.internal_snapshot} /><AdminFinancialEvidence title={copy.billing.refundSnapshot} value={selected.refund_snapshot} /><AdminFinancialEvidence title={copy.billing.ledgerSnapshot} value={selected.ledger_snapshot} /></>}
  </section>;
}
