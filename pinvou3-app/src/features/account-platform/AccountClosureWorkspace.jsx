import React, { useEffect, useRef, useState } from 'react';
import { accountClosureCopy, financeCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { accountClosureBlockerLabel, accountClosureResumeAction, accountClosurePresentation, queryAccountClosure, submitAccountClosure } from './account-closure-flow.js';
import { requireAccountClosure } from './account-closure-contract.js';
import { useClosureOperation } from './useClosureOperation.js';
import { readClosureIntent } from './closure-intent.js';
import { AccountClosureConfirmation } from './AccountClosureConfirmation.jsx';

export function AccountClosureWorkspace({ client, me, language }) {
  const copy = accountClosureCopy[language];
  const operation = useClosureOperation(me.user_id, { ...financeCopy[language], recoveryUnavailable: copy.recoveryUnavailable }, client, 'account-closure');
  const [query, setQuery] = useState({ status: 'loading', closure: null });
  const [notice, setNotice] = useState('');
  const [confirmation, setConfirmation] = useState(null);
  const [renderedReady, setRenderedReady] = useState(null);
  const [renderedAction, setRenderedAction] = useState(null);
  const [resumeRequested, setResumeRequested] = useState(null);
  const generation = useRef(0);
  const live = useRef(null), ready = useRef(null), pending = useRef(null), dispatching = useRef(null), settledAction = useRef(null);
  const identityGeneration = useRef(0), displaySequence = useRef(0);
  const epoch = client.financeSessionEpoch || 0;
  const canRead = hasPermission(me, 'personal.account.close.read');
  const canRequest = hasPermission(me, 'personal.account.close.request');
  live.current = { client, language, owner: me.user_id, epoch, canRead, canRequest, query, operation, renderedReady, renderedAction };
  const sameIdentity = snapshot => {
    const current = live.current, active = ready.current;
    return snapshot && active && current.renderedReady === active && current.renderedAction === settledAction.current &&
      current.epoch === (current.client.financeSessionEpoch || 0) && snapshot.client === current.client && snapshot.owner === current.owner && snapshot.epoch === current.epoch &&
      active.client === current.client && active.owner === current.owner && active.epoch === current.epoch && snapshot.identityGeneration === active.identityGeneration;
  };
  const freshCurrent = snapshot => {
    const current = live.current;
    return sameIdentity(snapshot) && snapshot.queryGeneration === generation.current && current.canRead && current.canRequest &&
      !dispatching.current && !current.operation.busy && !current.operation.blocked && !current.operation.unknown && !current.operation.pendingFacts &&
      accountClosurePresentation({ queryStatus: current.query.status, closure: current.query.closure }).canRequest;
  };
  const cancel = expected => {
    if (expected && expected !== pending.current) return;
    pending.current = null; setConfirmation(null);
  };
  useEffect(() => {
    const active = { client, owner: me.user_id, epoch, identityGeneration: ++identityGeneration.current };
    ready.current = active; setRenderedReady(active); dispatching.current = null; cancel(); setResumeRequested(null);
    return () => { identityGeneration.current++; ready.current = null; pending.current = null; };
  }, [client, me.user_id, epoch]);
  const load = async (signal, { silent = false } = {}) => {
    const stamp = ++generation.current;
    if (!silent) setQuery({ status: 'loading', closure: null });
    const result = await queryAccountClosure(client, signal);
    if (stamp !== generation.current) return;
    try {
      if (result.closure) requireAccountClosure(result.closure);
    } catch { setQuery({ status: 'error', closure: null }); return; }
    setQuery(result);
    try {
      const location = new URL(globalThis.location.href);
      const resume = accountClosureResumeAction({ requestedClosure: location.searchParams.get('account_closure'), queryStatus: result.status, closure: result.closure, resumed: false });
      if (resume.clearMarker) { location.searchParams.delete('account_closure'); globalThis.history.replaceState(null, '', location.pathname + location.search + location.hash); }
      if (resume.prompt && canRequest && !readClosureIntent(me.user_id, 'account-closure')) setResumeRequested(stamp);
    }
    catch {
      // Resume navigation is optional and must not erase validated server facts.
    }
  };
  useEffect(() => {
    const controller = new AbortController();
    if (canRead) void load(controller.signal);
    return () => { generation.current++; controller.abort(); };
    // Owner and current session are the authority; translated labels do not restart a request.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, me.user_id, epoch, canRead]);
  useEffect(() => {
    if (!canRead || typeof document === 'undefined') return undefined;
    const controller = new AbortController();
    const intervalMs = 15_000;
    let timer = null;
    let disposed = false;
    let inFlight = null;
    const visible = () => document.visibilityState === 'visible';
    const terminal = () => {
      const closure = live.current.query.closure;
      return Boolean(closure && (closure.status === 'COMPLETED' || closure.stage === 'ANONYMIZED' || closure.stage === 'COMPLETED'));
    };
    const stop = () => { if (timer !== null) { clearInterval(timer); timer = null; } };
    const tick = () => {
      if (disposed || !visible() || terminal()) { if (terminal()) stop(); return; }
      if (inFlight) return;
      inFlight = load(controller.signal, { silent: true }).catch(() => {}).finally(() => { inFlight = null; });
    };
    const restart = () => {
      stop();
      if (visible() && !terminal()) timer = setInterval(tick, intervalMs);
    };
    const onVisibilityChange = () => { if (visible()) { restart(); tick(); } else stop(); };
    document.addEventListener('visibilitychange', onVisibilityChange);
    restart();
    return () => { disposed = true; stop(); controller.abort(); document.removeEventListener('visibilitychange', onVisibilityChange); };
    // The poll is deliberately read-only and only follows the current identity/session.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, me.user_id, epoch, canRead]);
  const presentation = accountClosurePresentation({ queryStatus: query.status, closure: query.closure });
  const openConfirmation = () => {
    const active = ready.current;
    const snapshot = { ...active, queryGeneration: generation.current, sequence: ++displaySequence.current };
    if (!freshCurrent(snapshot) || freshCurrent(pending.current)) return;
    setNotice(old => old === copy.confirmMismatch ? '' : old);
    pending.current = snapshot; setConfirmation(snapshot);
  };
  const performRequest = async (facts, snapshot) => {
    const current = live.current;
    if (!sameIdentity(snapshot) || !current.canRead || !current.canRequest || current.operation.busy || current.operation.blocked || dispatching.current) return;
    const marker = {}; dispatching.current = marker;
    const stamp = generation.current;
    try {
      const result = await current.operation.mutate(facts, async (retainedFacts, key) => {
        const submitted = await submitAccountClosure({ client: current.client, confirmation: retainedFacts.confirmation, key, returnTo: '/?account_closure=confirm', language: current.language });
        if (submitted.status === 'created') submitted.closure = requireAccountClosure(submitted.closure);
        return submitted;
      });
      if (!sameIdentity(snapshot) || stamp !== generation.current || !result) return;
      if (result.status === 'created') { setQuery({ status: 'ready', closure: result.closure }); setNotice(copy.requested); }
      else if (result.status === 'recent-authentication-required') setNotice(copy.recentAuthenticationRequired);
    } finally {
      if (dispatching.current === marker) {
        dispatching.current = null;
        // The hook has settled its retained facts. A preceding render must not
        // turn that old operation snapshot into a fresh invocation.
        const settled = {}; settledAction.current = settled; setRenderedAction(settled);
      }
    }
  };
  const request = () => {
    const current = live.current, active = ready.current;
    if (client !== current.client || me.user_id !== current.owner || epoch !== current.epoch || !sameIdentity(active) || !current.canRead || !current.canRequest || current.operation.busy || current.operation.blocked || dispatching.current) return;
    if (current.operation.pendingFacts) { void performRequest(current.operation.pendingFacts, active); return; }
    if (!current.operation.unknown) openConfirmation();
  };
  const accept = (snapshot, text) => {
    if (snapshot !== pending.current) return;
    if (!freshCurrent(snapshot)) { cancel(snapshot); return; }
    if (text !== 'CLOSE_MY_ACCOUNT') { setNotice(copy.confirmMismatch); return; }
    cancel(snapshot);
    return performRequest({ confirmation: text }, snapshot);
  };
  useEffect(() => { if (pending.current && !freshCurrent(pending.current)) cancel(pending.current); });
  useEffect(() => {
    if (resumeRequested === null || !sameIdentity(ready.current)) return;
    if (resumeRequested === generation.current && query.status === 'ready' && !query.closure) openConfirmation();
    setResumeRequested(null);
  });
  if (!canRead) return null;
  const displayed = confirmation && freshCurrent(confirmation);
  const item = query.closure;
  return <section className="ap-danger-section"><h2>{copy.title}</h2><p>{copy.lead}</p>
    {displayed && <AccountClosureConfirmation key={confirmation.sequence} copy={copy} mismatch={notice === copy.confirmMismatch} onCancel={() => cancel(confirmation)} onConfirm={text => void accept(confirmation, text)} canRestoreFocus={() => freshCurrent(confirmation) && (!pending.current || pending.current === confirmation)} />}
    {operation.error && <p role="alert">{operation.error}</p>}{notice && <p role="status">{notice}</p>}
    {query.status === 'loading' && <p role="status">{copy.queryLoading}</p>}
    {query.status === 'error' && <p role="alert">{copy.queryFailed}</p>}
    <button disabled={operation.busy} onClick={() => void load()}>{copy.queryRetry}</button>
    {item && <><p>{copy.statuses[item.status]} · {copy.stages[item.stage]}</p><p>{copy.sla}: {item.sla_expires_at}</p>
      {item.blockers.length ? <ul>{item.blockers.map((blocker, index) => <li key={`${blocker.domain}:${blocker.code}:${index}`}>{accountClosureBlockerLabel(blocker, copy)}</li>)}</ul> : <p>{copy.noBlockers}</p>}</>}
    {canRequest && (presentation.canRequest || operation.unknown) && <><p>{copy.before}</p><button className="ap-danger" disabled={operation.busy || operation.blocked || !sameIdentity(ready.current)} onClick={() => void request()}>{operation.unknown ? financeCopy[language].retry : copy.start}</button></>}
  </section>;
}
