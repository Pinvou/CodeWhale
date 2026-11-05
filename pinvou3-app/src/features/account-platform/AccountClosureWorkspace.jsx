import React, { useEffect, useRef, useState } from 'react';
import { accountClosureCopy, financeCopy, accountCenterCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { accountClosureBlockerLabel, accountClosureResumeAction, accountClosurePresentation, submitAccountClosure } from './account-closure-flow.js';
import { createAccountClosureReader } from './account-closure-reader.js';
import { requireAccountClosure } from './account-closure-contract.js';
import { useClosureOperation } from './useClosureOperation.js';
import { readClosureIntent } from './closure-intent.js';
import { AccountClosureConfirmation } from './AccountClosureConfirmation.jsx';
import { usePortalDialogScope } from './portal-dialog-scope.jsx';

export function AccountClosureWorkspace({ client, me, language }) {
  const panelActive = usePortalDialogScope();
  const copy = accountClosureCopy[language];
  const operation = useClosureOperation(me.user_id, { ...financeCopy[language], recoveryUnavailable: copy.recoveryUnavailable }, client, 'account-closure');
  const [query, setQuery] = useState({ status: 'loading', closure: null });
  const [notice, setNotice] = useState('');
  const [confirmation, setConfirmation] = useState(null);
  const [renderedReady, setRenderedReady] = useState(null);
  const [renderedAction, setRenderedAction] = useState(null);
  const [resumeRequested, setResumeRequested] = useState(null);
  const generation = useRef(0);
  const reading = useRef(null), polling = useRef(null);
  const live = useRef(null), ready = useRef(null), pending = useRef(null), dispatching = useRef(null), settledAction = useRef(null);
  const identityGeneration = useRef(0), displaySequence = useRef(0);
  const epoch = client.financeSessionEpoch || 0;
  const canRead = hasPermission(me, 'personal.account.close.read');
  const canRequest = hasPermission(me, 'personal.account.close.request');
  live.current = { client, language, owner: me.user_id, epoch, canRead, canRequest, query, operation, renderedReady, renderedAction, panelActive };
  const retireReading = () => {
    reading.current?.invalidate();
    polling.current?.retireRead();
  };
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
  const load = async ({ silent = false } = {}) => {
    const owned = reading.current, current = live.current;
    if (!owned || current.client !== client || current.owner !== me.user_id || current.epoch !== epoch || !current.canRead || dispatching.current) return;
    if (silent && (pending.current || current.operation.busy)) return;
    // Background reads do not retire a displayed confirmation or resume intent.
    const stamp = silent ? generation.current : ++generation.current;
    if (!silent) setQuery({ status: 'loading', closure: null });
    let result;
    try { result = await owned.read(); }
    catch (reason) { if (reason?.name === 'AbortError') return; throw reason; }
    if (stamp !== generation.current || owned !== reading.current || !owned.current()) return;
    try {
      if (result.closure) requireAccountClosure(result.closure);
    } catch (reason) { result = { status: 'error', closure: null, reason }; }
    polling.current?.settled(result);
    const denied = result.reason?.status === 401 || result.reason?.status === 403;
    if (!silent || result.status !== 'error' || denied) setQuery(result);
    if (result.status !== 'ready') return result;
    try {
      const location = new URL(globalThis.location.href);
      const resume = accountClosureResumeAction({ requestedClosure: location.searchParams.get('account_closure'), queryStatus: result.status, closure: result.closure, resumed: false });
      if (resume.clearMarker) { location.searchParams.delete('account_closure'); globalThis.history.replaceState(null, '', location.pathname + location.search + location.hash); }
      if (resume.prompt && canRequest && !readClosureIntent(me.user_id, 'account-closure')) setResumeRequested(stamp);
    }
    catch {
      // Resume navigation is optional and must not erase validated server facts.
    }
    return result;
  };
  useEffect(() => {
    const owned = createAccountClosureReader(client, () => {
      const current = live.current;
      return current.client === client && current.owner === me.user_id && current.epoch === epoch && current.canRead;
    });
    reading.current = owned;
    if (canRead) void load();
    return () => {
      generation.current++; owned.dispose();
      if (reading.current === owned) reading.current = null;
    };
    // Owner and current session are the authority; translated labels do not restart a request.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, me.user_id, epoch, canRead]);
  useEffect(() => {
    if (!canRead || typeof document === 'undefined') return undefined;
    const intervalMs = 15_000;
    const maxFailures = 3;
    let timer = null;
    let disposed = false, retired = false, failures = 0;
    let inFlight = null;
    const reader = reading.current;
    const visible = () => document.visibilityState === 'visible' && live.current.panelActive;
    const terminal = () => {
      const closure = live.current.query.closure;
      return Boolean(closure && (closure.status === 'COMPLETED' || closure.stage === 'ANONYMIZED' || closure.stage === 'COMPLETED'));
    };
    const stop = () => { if (timer !== null) { clearInterval(timer); timer = null; } };
    const tick = () => {
      if (disposed || retired || !visible() || terminal()) { if (terminal()) stop(); return; }
      if (!reading.current?.current()) { retired = true; stop(); return; }
      if (inFlight || pending.current || dispatching.current || live.current.operation.busy || reading.current?.pending()) return;
      const request = load({ silent: true }).catch(reason => {
        control.settled({ status: 'error', closure: null, reason });
      }).finally(() => { if (inFlight === request) inFlight = null; });
      inFlight = request;
    };
    const restart = () => {
      stop();
      if (!disposed && !retired && visible() && !terminal()) timer = setInterval(tick, intervalMs);
    };
    const control = {
      settled(result) {
        if (disposed) return;
        if (result.reason?.status === 401 || result.reason?.status === 403) { retired = true; stop(); return; }
        if (result.status === 'error') {
          if (++failures >= maxFailures) {
            retired = true; stop();
            setQuery(previous => ({ ...previous, status: 'error', reason: result.reason }));
          }
          return;
        }
        failures = 0;
        if (result.closure && (result.closure.status === 'COMPLETED' || result.closure.stage === 'ANONYMIZED' || result.closure.stage === 'COMPLETED')) { retired = true; stop(); }
      },
      resume() { retired = false; failures = 0; restart(); },
      retireRead() { inFlight = null; },
      syncVisibility() {
        if (disposed) return;
        if (visible()) { restart(); tick(); }
        else {
          stop();
          if (inFlight) { reader?.invalidate(); inFlight = null; }
        }
      },
    };
    polling.current = control;
    const onVisibilityChange = () => control.syncVisibility();
    document.addEventListener('visibilitychange', onVisibilityChange);
    restart();
    return () => {
      disposed = true; stop(); document.removeEventListener('visibilitychange', onVisibilityChange);
      if (polling.current === control) polling.current = null;
    };
    // The poll is deliberately read-only and only follows the current identity/session.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, me.user_id, epoch, canRead]);
  useEffect(() => { polling.current?.syncVisibility(); }, [panelActive]);
  const presentation = accountClosurePresentation({ queryStatus: query.status, closure: query.closure });
  const openConfirmation = () => {
    const active = ready.current;
    const snapshot = { ...active, queryGeneration: generation.current, sequence: ++displaySequence.current };
    if (!freshCurrent(snapshot) || freshCurrent(pending.current)) return;
    retireReading(); snapshot.queryGeneration = ++generation.current;
    setNotice(old => old === copy.confirmMismatch ? '' : old);
    pending.current = snapshot; setConfirmation(snapshot);
  };
  const performRequest = async (facts, snapshot) => {
    const current = live.current;
    if (!sameIdentity(snapshot) || !current.canRead || !current.canRequest || current.operation.busy || current.operation.blocked || dispatching.current) return;
    const marker = {}; dispatching.current = marker;
    retireReading();
    const stamp = ++generation.current;
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
  const center = accountCenterCopy[language];
  return <section className="ap-danger-section"><h2>{copy.title}</h2><p>{center.closureConsequences}</p>
    {displayed && <AccountClosureConfirmation key={confirmation.sequence} copy={copy} mismatch={notice === copy.confirmMismatch} onCancel={() => cancel(confirmation)} onConfirm={text => void accept(confirmation, text)} canRestoreFocus={() => freshCurrent(confirmation) && (!pending.current || pending.current === confirmation)} />}
    {operation.error && <p role="alert">{operation.error}</p>}{notice && <p role="status">{notice}</p>}
    {query.status === 'loading' && <p role="status">{copy.queryLoading}</p>}
    {query.status === 'error' && <p role="alert">{copy.queryFailed}</p>}
    <button disabled={operation.busy} onClick={() => { polling.current?.resume(); void load(); }}>{copy.queryRetry}</button>
    {query.status === 'ready' && !item && <p><strong>{center.closureStatus}: </strong>{center.closureIdle}</p>}
    {item && <><p><strong>{center.closureStatus}: </strong>{copy.statuses[item.status]} · {copy.stages[item.stage]}</p><p>{copy.sla}: {item.sla_expires_at}</p>
      {item.blockers.length ? <div><h3>{center.closureChecklist}</h3><ul>{item.blockers.map((blocker, index) => <li key={`${blocker.domain}:${blocker.code}:${index}`}>{accountClosureBlockerLabel(blocker, copy)}</li>)}</ul></div> : <p>{copy.noBlockers}</p>}</>}
    <details className="ap-closure-rules"><summary>{copy.beforeTitle}</summary><p>{copy.lead}</p><p>{copy.before}</p></details>
    {canRequest && (presentation.canRequest || operation.unknown) && <button className="ap-danger" disabled={operation.busy || operation.blocked || !sameIdentity(ready.current)} onClick={() => void request()}>{operation.pendingFacts || operation.unknown ? financeCopy[language].retry : copy.start}</button>}
  </section>;
}
