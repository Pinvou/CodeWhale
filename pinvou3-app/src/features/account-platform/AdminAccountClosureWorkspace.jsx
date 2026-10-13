import React, { useEffect, useRef, useState } from 'react';
import { accountClosureCopy, financeCopy } from '../../shared/i18n.js';
import { requireAccountClosure } from './account-closure-contract.js';
import { useClosureOperation } from './useClosureOperation.js';
import { closureRepairIntents } from './closure-intent.js';

function ClosureRepair({ client, owner, item, copy, closureCopy, canRepair, onChanged }) {
  const [reason, setReason] = useState('');
  const [needsAuthentication, setNeedsAuthentication] = useState(false);
  const operation = useClosureOperation(owner, { ...copy, recoveryUnavailable: closureCopy.recoveryUnavailable }, client, `account-closure-retry:${item.closure_request_id}`);
  const retry = async () => {
    if (!canRepair || operation.busy || operation.blocked || (!operation.unknown && !reason.trim())) return;
    const result = await operation.mutate({ id: item.closure_request_id, reason: reason.trim() }, async (facts, key) => {
      setNeedsAuthentication(false);
      let raw;
      try { raw = await client.retryAccountClosure(facts.id, facts.reason, key); }
      catch (error) { if (error.code === 'RECENT_AUTHENTICATION_REQUIRED') setNeedsAuthentication(true); throw error; }
      const receipt = requireAccountClosure(raw);
      if (receipt.closure_request_id !== facts.id) { const error = new Error('COMMAND_RESULT_UNKNOWN'); error.code = 'COMMAND_RESULT_UNKNOWN'; throw error; }
      return receipt;
    });
    if (result) onChanged();
  };
  return <article><p>{item.closure_request_id} · {item.recovery_only ? closureCopy.recoveryPending : `${closureCopy.statuses[item.status]} · ${closureCopy.stages[item.stage]}`}</p>
    {operation.error && <p role="alert">{needsAuthentication && !operation.unknown ? closureCopy.recentAuthenticationRequired : operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
    {needsAuthentication && canRepair && <><p>{closureCopy.recentAuthenticationRequired}</p><button onClick={() => client.reauthenticate('/admin.html?section=closures')}>{closureCopy.reauthenticate}</button></>}
    {canRepair && <><label>{copy.reason}<textarea value={operation.pendingFacts?.reason || reason} disabled={operation.busy || operation.unknown || operation.blocked} onChange={event => setReason(event.target.value)} /></label><button disabled={operation.busy || operation.blocked || (!operation.unknown && !reason.trim())} onClick={() => void retry()}>{copy.retry}</button></>}
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
      const pending = closureRepairIntents(me.user_id).map(value => ({ closure_request_id: value.facts.id, recovery_only: true }));
      setItems(pending);
      const result = await client.accountClosures(signal);
      if (stamp !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_CLOSURE_COLLECTION');
      result.items.forEach(requireAccountClosure); setItems([...result.items, ...pending.filter(item => !result.items.some(current => current.closure_request_id === item.closure_request_id))]);
    } catch { if (stamp === generation.current) setError(true); }
  };
  useEffect(() => { const controller = new AbortController(); if (canRead) void load(controller.signal); return () => { generation.current++; controller.abort(); }; }, [client, me.user_id, epoch, canRead]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!canRead) return null;
  return <section><h2>{accountClosureCopy[language].title}</h2>{error && <p role="alert">{copy.unavailable}</p>}<button onClick={() => void load()}>{copy.refresh}</button>
    {items?.map(item => <ClosureRepair key={`${me.user_id}:${epoch}:${item.closure_request_id}`} client={client} owner={me.user_id} item={item} copy={copy} closureCopy={accountClosureCopy[language]} canRepair={canRepair} onChanged={() => void load()} />)}
  </section>;
}
