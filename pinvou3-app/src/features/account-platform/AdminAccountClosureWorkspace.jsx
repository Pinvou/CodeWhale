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
  const copy = financeCopy[language], closureCopy = accountClosureCopy[language];
  const [state, setState] = useState({ client: null, userID: null, epoch: null, items: null, nextCursor: '', loading: true, error: null });
  const generation = useRef(0);
  const permissions = me.platform_permissions || [];
  const canRead = permissions.includes('platform.account-closures.read');
  const canRepair = permissions.includes('platform.account-closures.repair');
  const epoch = client.financeSessionEpoch || 0;
  const load = async (signal, cursor = '') => {
    const stamp = ++generation.current;
    const owned = { client, userID: me.user_id, epoch };
    setState({ ...owned, items: null, nextCursor: '', loading: true, error: null });
    try {
      let pending = [];
      if (!cursor) {
        try { pending = closureRepairIntents(me.user_id).map(value => ({ closure_request_id: value.facts.id, recovery_only: true })); }
        catch {
          setState({ ...owned, items: null, nextCursor: '', loading: false, error: { code: 'CLOSURE_RECOVERY_UNAVAILABLE' } });
          return;
        }
      }
      setState({ ...owned, items: pending, nextCursor: '', loading: true, error: null });
      const result = await client.accountClosures(signal, cursor);
      if (stamp !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_CLOSURE_COLLECTION');
      result.items.forEach(requireAccountClosure);
      setState({ ...owned, items: [...result.items, ...pending.filter(item => !result.items.some(current => current.closure_request_id === item.closure_request_id))], nextCursor: typeof result.next_cursor === 'string' ? result.next_cursor : '', loading: false, error: null });
    } catch (error) { if (stamp === generation.current) setState(current => ({ ...current, loading: false, error })); }
  };
  useEffect(() => { const controller = new AbortController(); if (canRead) void load(controller.signal); return () => { generation.current++; controller.abort(); }; }, [client, me.user_id, epoch, canRead]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!canRead) return <p role="alert">{closureCopy.listPermissionDenied}</p>;
  const visible = state.client === client && state.userID === me.user_id && state.epoch === epoch ? state : { items: null, nextCursor: '', loading: true, error: null };
  const denied = visible.error?.status === 403 || visible.error?.code === 'PERMISSION_DENIED';
  const problem = denied ? closureCopy.listPermissionDenied : visible.error?.code === 'CLOSURE_RECOVERY_UNAVAILABLE' ? closureCopy.recoveryUnavailable : closureCopy.listUnavailable;
  return <section className="adm-closure-directory" aria-busy={visible.loading}><h2>{closureCopy.listTitle}</h2><p>{closureCopy.listLead}</p><button type="button" disabled={visible.loading} onClick={() => void load()}>{copy.refresh}</button>
    {visible.loading && <p role="status">{closureCopy.listLoading}</p>}
    {visible.error && <div className="adm-panel" role="alert"><p>{problem}</p><button type="button" onClick={() => void load()}>{copy.retry}</button></div>}
    {!visible.loading && !visible.error && visible.items?.length === 0 && <div className="adm-panel adm-empty" role="status">{closureCopy.listEmpty}</div>}
    {visible.items?.map(item => <ClosureRepair key={`${me.user_id}:${epoch}:${item.closure_request_id}`} client={client} owner={me.user_id} item={item} copy={copy} closureCopy={closureCopy} canRepair={canRepair} onChanged={() => void load()} />)}
    {visible.nextCursor && <button type="button" onClick={() => void load(undefined, visible.nextCursor)}>{copy.next}</button>}
  </section>;
}
