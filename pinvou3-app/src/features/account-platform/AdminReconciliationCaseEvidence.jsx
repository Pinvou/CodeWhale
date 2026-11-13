import React from 'react';
import { integerMoney } from './finance-contract.js';

const record = value => value !== null && typeof value === 'object' && !Array.isArray(value) ? value : null;
const rawText = value => typeof value === 'string' ? value.trim() : '';
const numberFact = value => {
  if (value === undefined || value === null) return { state: 'missing' };
  if (!['string', 'number'].includes(typeof value)) return { state: 'unknown' };
  try { return { state: 'known', amount: integerMoney(value) }; } catch { return { state: 'unknown' }; }
};
const money = value => ({
  ...numberFact(value?.amount_fen),
  currency: rawText(value?.currency).toUpperCase(),
});
const providerKind = source => {
  if (!source) return '';
  const explicit = rawText(source.entry_type).toUpperCase(), status = rawText(source.status).toUpperCase();
  if (explicit) return explicit;
  if (status.includes('CHARGEBACK')) return 'CHARGEBACK';
  if (status.includes('REFUND')) return 'REFUND';
  return ['SUCCESS', 'PAID', 'COMPLETED'].includes(status) ? 'PAYMENT' : '';
};
const evidenceState = value => value == null || (Array.isArray(value) ? value.length === 0 : record(value) && Object.keys(value).length === 0)
  ? 'missing' : Array.isArray(value) || record(value) ? 'available' : 'unknown';

export function caseEvidence(item) {
  const provider = record(item.provider_snapshot), internal = record(item.internal_snapshot);
  const reversal = record(item.refund_snapshot);
  const transaction = record(internal?.payment_transaction), order = record(internal?.payment_order);
  const providerAmount = money(provider), transactionAmount = money(transaction), refundAmount = money(reversal);
  const providerReference = rawText(provider?.provider_reference_id) || rawText(provider?.channel_transaction_id);
  const transactionReference = typeof transaction?.provider_transaction_id === 'string' ? transaction.provider_transaction_id : '';
  const merchant = typeof item.merchant_order_no === 'string' ? item.merchant_order_no : '';
  const matchingPayment = item.case_type === 'AMOUNT_MISMATCH' && providerKind(provider) === 'PAYMENT'
    && ['SUCCESS', 'PAID', 'COMPLETED'].includes(rawText(provider?.status).toUpperCase())
    && merchant && provider?.merchant_order_no === merchant && order?.merchant_order_no === merchant
    && providerReference && providerReference === transactionReference
    && order?.id && order.id === transaction?.payment_order_id
    && ['PAID', 'LATE_PAID'].includes(transaction?.status);
  let comparison = { state: 'notComparable' };
  if (matchingPayment && providerAmount.state === 'known' && transactionAmount.state === 'known') {
    const currency = providerAmount.currency;
    if (currency && currency === transactionAmount.currency) {
      comparison = { state: 'known', currency, amount: providerAmount.amount - transactionAmount.amount };
    }
  }
  return {
    providerAmount, transactionAmount, refundAmount, comparison,
    reversalKind: rawText(reversal?.provider_chargeback_id) ? 'chargeback' : 'refund',
    reversedBase: numberFact(reversal?.revoked_base_microcredits),
    reversedBonus: numberFact(reversal?.revoked_bonus_microcredits),
    sources: [
      ['provider_snapshot', 'caseProviderEvidence', evidenceState(item.provider_snapshot)],
      ['internal_snapshot', 'caseInternalEvidence', evidenceState(item.internal_snapshot)],
      ['refund_snapshot', 'caseReversalEvidence', evidenceState(item.refund_snapshot)],
      ['ledger_snapshot', 'caseLedgerEvidence', evidenceState(item.ledger_snapshot)],
    ],
  };
}

const units = (fact, copy) => fact.state === 'known'
  ? <><code dir="ltr">{fact.amount.toString()}</code>{' '}{copy.billFen}{' · '}{fact.currency || copy.caseCurrencyUnknown}</>
  : copy[fact.state === 'unknown' ? 'caseEvidenceUnknown' : 'caseBatchNotProvided'];

export function ReconciliationCaseAmounts({ item, copy }) {
  const facts = caseEvidence(item);
  const rows = [
    [copy.caseProviderAmount, units(facts.providerAmount, copy)],
    [copy.caseTransactionAmount, units(facts.transactionAmount, copy)],
    [copy.casePaymentDelta, facts.comparison.state === 'known' ? units(facts.comparison, copy) : copy.caseAmountNotComparable],
  ];
  if (facts.refundAmount.state !== 'missing') rows.push([copy.caseRefundAmount, units(facts.refundAmount, copy)]);
  return <dl className="adm-case-facts">{rows.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>;
}

export function ReconciliationCaseOwner({ id, copy }) {
  return id ? <code dir="ltr">{id}</code> : copy.caseOwnerUnassigned;
}

export function ReconciliationCaseEvidence({ item, copy }) {
  const { sources, reversalKind, reversedBase, reversedBonus } = caseEvidence(item);
  const stateCopy = { available: copy.caseEvidenceAvailable, missing: copy.caseBatchNotProvided, unknown: copy.caseEvidenceUnknown };
  return <details className="adm-case-evidence"><summary>{copy.snapshots}</summary>
    {sources.map(([key, label, state]) => <section key={key}>
      <h4>{copy[label]}</h4><p>{stateCopy[state]}</p>
      {key === 'refund_snapshot' && reversalKind === 'chargeback' && <dl className="adm-case-facts">
        {[[copy.caseRevokedBase, reversedBase], [copy.caseRevokedBonus, reversedBonus]].map(([term, fact]) => <div key={term}>
          <dt>{term}</dt><dd>{fact.state === 'known' ? <><code dir="ltr">{fact.amount.toString()}</code>{' '}{copy.caseMicrocredits}</> : stateCopy[fact.state]}</dd>
        </div>)}
      </dl>}
      <pre>{JSON.stringify(item[key] ?? {}, null, 2)}</pre>
    </section>)}
  </details>;
}
