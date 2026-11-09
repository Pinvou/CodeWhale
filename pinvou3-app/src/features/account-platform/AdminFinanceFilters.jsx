import React, { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { adminLayoutCopy, accountLayoutCopy } from '../../shared/i18n.js';
import { financeReadDefinition, financeListRouteUpdates, readFinanceListRoute } from './admin-finance-read-route.js';

export function localFinanceDate(value) {
  if (!value) return '';
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return '';
  const pad = number => String(number).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}.${String(date.getMilliseconds()).padStart(3, '0')}`;
}

export function useFinanceReadRoute(kind, route) {
  const current = readFinanceListRoute(kind, route?.search || '');
  const [revision, setRevision] = useState(0);
  const signature = `${JSON.stringify(current.query)}:${revision}`;
  const filterSignature = JSON.stringify(Object.fromEntries(Object.entries(current.query).filter(([key]) => key !== 'cursor')));
  const [draft, setDraft] = useState(current.query);
  useEffect(() => {
    setDraft(Object.fromEntries(Object.entries(current.query).map(([key, value]) => [key, /_(from|to|before)$/.test(key) ? localFinanceDate(value) : value])));
  }, [signature]);
  const move = (query, selectedID = current.selectedID || null, mode = 'push') => { setRevision(value => value + 1); route.navigate(route.section, financeListRouteUpdates(kind, query, selectedID, current.query), mode); };
  return { ...current, signature, filterSignature, draft, setDraft, move };
}

// URL changes may happen through browser history while the command owner is
// busy. Coalesce only list reads; accepted writes keep their original identity.
export function useFinanceRouteLoader({ route, signature, client, owner, operation, load, reset }) {
  const sessionEpoch = client.financeSessionEpoch || 0;
  const identity = useMemo(() => ({ client, owner, signature, sessionEpoch }), [client, owner, signature, sessionEpoch]);
  const live = useRef(identity), attempted = useRef(null);
  useLayoutEffect(() => { live.current = identity; if (route) reset(); }, [identity]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (route && !operation.busy && !operation.unknown && attempted.current !== identity) void load();
  }, [identity, Boolean(route), operation.busy, operation.unknown]); // eslint-disable-line react-hooks/exhaustive-deps
  return {
    read: async (action, classifyError) => {
      const stamp = identity;
      // A completed write may resume a callback from an older render. It must
      // not relabel that callback's captured query as the current URL.
      if (route && stamp !== live.current) return undefined;
      const value = await operation.read(
        () => { attempted.current = stamp; return action(); },
        classifyError ? reason => classifyError(reason, stamp === live.current) : undefined,
      );
      return !route || stamp === live.current ? value : undefined;
    },
  };
}

export function AdminFinanceFilters({ kind, language, value, onChange, onSubmit, disabled = false }) {
  const words = adminLayoutCopy[language].financeLists;
  const config = financeReadDefinition(kind);
  const options = key => {
    if (['status', 'lifecycle_status'].includes(key)) return config.statuses;
    if (key === 'owner_type') return ['PERSONAL', 'ORGANIZATION', 'ORG_UNIT'];
    if (key === 'overdue') return ['true', 'false'];
    if (key === 'source_type') return ['USAGE_OVERAGE', 'CHARGEBACK', 'FORCED_REFUND'];
    if (key === 'severity') return ['P0', 'P1', 'P2'];
    if (key === 'anomaly_type') return ['CHARGEBACK', 'FORCED_REFUND', 'DUPLICATE_PAYMENT', 'LATE_PAYMENT_CONFLICT', 'REFUND_FAILURE', 'PAYMENT_FACT_CONFLICT', 'ILLEGAL_TRANSITION'];
    if (key === 'entry_type') return ['RECHARGE', 'USAGE', 'REFUND', 'REVERSAL', 'ADJUSTMENT', 'EXPIRY', 'RECEIVABLE', 'CHARGEBACK', 'REPAIR_CREDIT', 'FORCED_REFUND'];
    return null;
  };
  return <form className="adm-filter-row" onSubmit={event => { event.preventDefault(); onSubmit(); }}>
    {config.fields.map(key => key === 'include_deleted' ? (
      <label key={key} className="adm-checkbox-field"><input type="checkbox" disabled={disabled} checked={value[key] === 'true'} onChange={event => onChange({ ...value, [key]: event.target.checked ? 'true' : '' })} />{words[key]}</label>
    ) : <label key={key}>{words[key]}{options(key) ? <select disabled={disabled} value={value[key] || ''} onChange={event => onChange({ ...value, [key]: event.target.value })}><option value="">{words.allValues}</option>{options(key).map(option => <option key={option} value={option}>{words[option] || accountLayoutCopy[language].statuses[option] || option}</option>)}</select> : <input disabled={disabled} maxLength={key === 'q' || key === 'business_reference' ? 160 : 120} step={/_(from|to|before)$/.test(key) ? '0.001' : undefined} type={/_(from|to|before)$/.test(key) ? 'datetime-local' : 'text'} value={value[key] || ''} onChange={event => onChange({ ...value, [key]: event.target.value })} />}</label>)}
    <label>{words.limit}<select disabled={disabled} value={value.limit} onChange={event => onChange({ ...value, limit: event.target.value })}>{['25', '50', '100', '200'].map(limit => <option key={limit}>{limit}</option>)}</select></label><button disabled={disabled}>{words.search}</button>
  </form>;
}
