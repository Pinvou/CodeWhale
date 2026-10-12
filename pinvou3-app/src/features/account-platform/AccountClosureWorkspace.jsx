import React, { useEffect, useRef, useState } from 'react';
import { accountClosureCopy, financeCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { accountClosureResumeAction, accountClosurePresentation, queryAccountClosure, submitAccountClosure } from './account-closure-flow.js';
import { requireAccountClosure } from './account-closure-contract.js';
import { useClosureOperation } from './useClosureOperation.js';
import { readClosureIntent } from './closure-intent.js';

export function AccountClosureWorkspace({ client, me, language }) {
  const copy = accountClosureCopy[language];
  const operation = useClosureOperation(me.user_id, { ...financeCopy[language], recoveryUnavailable: copy.recoveryUnavailable }, client, 'account-closure');
  const [query, setQuery] = useState({ status: 'loading', closure: null });
  const [notice, setNotice] = useState('');
  const generation = useRef(0);
  const epoch = client.financeSessionEpoch || 0;
  const canRead = hasPermission(me, 'personal.account.close.read');
  const canRequest = hasPermission(me, 'personal.account.close.request');
  const load = async signal => {
    const stamp = ++generation.current;
    setQuery({ status: 'loading', closure: null });
    const result = await queryAccountClosure(client, signal);
    if (stamp !== generation.current) return;
    try {
      if (result.closure) requireAccountClosure(result.closure);
      setQuery(result);
      const location = new URL(globalThis.location.href);
      const resume = accountClosureResumeAction({ requestedClosure: location.searchParams.get('account_closure'), queryStatus: result.status, closure: result.closure, resumed: false });
      if (resume.clearMarker) { location.searchParams.delete('account_closure'); globalThis.history.replaceState(null, '', location.pathname + location.search + location.hash); }
      if (resume.prompt && canRequest && !readClosureIntent(me.user_id, 'account-closure')) await performRequest(globalThis.prompt(copy.confirmPrompt));
    }
    catch { setQuery({ status: 'error', closure: null }); }
  };
  useEffect(() => {
    const controller = new AbortController();
    if (canRead) void load(controller.signal);
    return () => { generation.current++; controller.abort(); };
    // Owner and current session are the authority; translated labels do not restart a request.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, me.user_id, epoch, canRead]);
  if (!canRead) return null;
  const presentation = accountClosurePresentation({ queryStatus: query.status, closure: query.closure });
  const request = async () => {
    if (!canRequest || operation.busy || operation.blocked || (!operation.unknown && !presentation.canRequest)) return;
    await performRequest(operation.pendingFacts?.confirmation || globalThis.prompt(copy.confirmPrompt));
  };
  const performRequest = async confirmation => {
    if (!canRequest || operation.busy || operation.blocked) return;
    if (confirmation !== 'CLOSE_MY_ACCOUNT') { if (confirmation !== null) setNotice(copy.confirmMismatch); return; }
    const stamp = generation.current;
    const result = await operation.mutate({ confirmation }, async (facts, key) => {
      const submitted = await submitAccountClosure({ client, confirmation: facts.confirmation, key, returnTo: '/?account_closure=confirm' });
      if (submitted.status === 'created') submitted.closure = requireAccountClosure(submitted.closure);
      return submitted;
    });
    if (stamp !== generation.current || !result) return;
    if (result.status === 'created') { setQuery({ status: 'ready', closure: result.closure }); setNotice(copy.requested); }
    else if (result.status === 'recent-authentication-required') setNotice(copy.recentAuthenticationRequired);
  };
  const item = query.closure;
  return <section className="ap-danger-section"><h2>{copy.title}</h2><p>{copy.lead}</p>
    {operation.error && <p role="alert">{operation.error}</p>}{notice && <p role="status">{notice}</p>}
    {query.status === 'loading' && <p role="status">{copy.queryLoading}</p>}
    {query.status === 'error' && <p role="alert">{copy.queryFailed}</p>}
    <button disabled={operation.busy} onClick={() => void load()}>{copy.queryRetry}</button>
    {item && <><p>{copy.statuses[item.status]} · {copy.stages[item.stage]}</p><p>{copy.sla}: {item.sla_expires_at}</p>
      {item.blockers.length ? <ul>{item.blockers.map((blocker, index) => <li key={`${blocker.domain}:${blocker.code}:${index}`}>{copy.blockerCodes[blocker.code] || blocker.code} — {copy.actionCodes[blocker.user_action] || blocker.user_action || blocker.domain}</li>)}</ul> : <p>{copy.noBlockers}</p>}</>}
    {canRequest && (presentation.canRequest || operation.unknown) && <><p>{copy.before}</p><button disabled={operation.busy || operation.blocked} onClick={() => void request()}>{operation.unknown ? financeCopy[language].retry : copy.start}</button></>}
  </section>;
}
