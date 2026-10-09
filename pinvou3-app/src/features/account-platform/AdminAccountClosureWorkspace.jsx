import React, { useEffect, useRef, useState } from 'react';
import { accountClosureCopy, financeCopy } from '../../shared/i18n.js';
import { requireAccountClosure } from './account-closure-contract.js';
import { useFinanceOperation } from './useFinanceOperation.js';

function ClosureRepair({ client, owner, item, copy, canRepair, onChanged }) {
  const [reason, setReason] = useState('');
  const operation = useFinanceOperation(owner, copy, client, `account-closure-retry:${item.closure_request_id}`);
  const retry = async () => {
    if (!canRepair || operation.busy || (!operation.unknown && !reason.trim())) return;
    const result = await operation.mutate({ id: item.closure_request_id, reason: reason.trim() }, async (facts, key) => {
      const receipt = requireAccountClosure(await client.retryAccountClosure(facts.id, facts.reason, key));
      if (receipt.closure_request_id !== facts.id) { const error = new Error('COMMAND_RESULT_UNKNOWN'); error.code = 'COMMAND_RESULT_UNKNOWN'; throw error; }
      return receipt;
    });
    if (result) onChanged();
  };
  return <article><p>{item.closure_request_id} · {item.status} · {item.stage}</p>
    {operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
    {canRepair && <><label>{copy.reason}<textarea value={operation.pendingFacts?.reason || reason} disabled={operation.busy || operation.unknown} onChange={event => setReason(event.target.value)} /></label><button disabled={operation.busy || (!operation.unknown && !reason.trim())} onClick={() => void retry()}>{copy.retry}</button></>}
  </article>;
}

export function AdminAccountClosureWorkspace({ client, me, language }) {
  const copy = financeCopy[language];
  const [items, setItems] = useState(null);
  const [error, setError] = useState(false);
  const generation = useRef(0);
  const permissions = me.platform_permissions || [];
  const canRead = permissions.includes('platform.account-closures.read');
  const canRepair = permissions.includes('platform.account-closures.repair');
  const epoch = client.financeSessionEpoch || 0;
  const load = async signal => {
    const stamp = ++generation.current; setItems(null); setError(false);
    try {
      const result = await client.accountClosures(signal);
      if (stamp !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_CLOSURE_COLLECTION');
      result.items.forEach(requireAccountClosure); setItems(result.items);
    } catch { if (stamp === generation.current) setError(true); }
  };
  useEffect(() => { const controller = new AbortController(); if (canRead) void load(controller.signal); return () => { generation.current++; controller.abort(); }; }, [client, me.user_id, epoch, canRead]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!canRead) return null;
  return <section><h2>{accountClosureCopy[language].title}</h2>{error && <p role="alert">{copy.unavailable}</p>}<button onClick={() => void load()}>{copy.refresh}</button>
    {items?.map(item => <ClosureRepair key={`${me.user_id}:${epoch}:${item.closure_request_id}`} client={client} owner={me.user_id} item={item} copy={copy} canRepair={canRepair} onChanged={() => void load()} />)}
  </section>;
}
