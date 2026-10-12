import { useEffect, useRef, useState } from 'react';
import { secureRequestID } from './request-id.js';
const pendingByClient = new WeakMap();

// One owner and one immutable invocation retain the same key until its receipt
// is known. A retired completion cannot update the next identity or resource.
export function useFinanceOperation(owner, copy, client, scope) {
  let pending = pendingByClient.get(client);
  if (!pending) { pending = new Map(); pendingByClient.set(client, pending); }
  const identity = `${owner}:${client.financeSessionEpoch || 0}:${scope}`;
  const generation = useRef(0);
  const intent = useRef(null);
  const running = useRef(false);
  const [busy, setBusy] = useState(false);
  const [unknown, setUnknown] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  useEffect(() => {
    generation.current++; intent.current = pending.get(identity) || null; running.current = false;
    setBusy(false); setUnknown(Boolean(intent.current)); setMessage(''); setError(intent.current ? 'unknown' : '');
    return () => { generation.current++; };
  }, [identity, client]); // eslint-disable-line react-hooks/exhaustive-deps
  const read = async (action, classifyError) => {
    if (running.current) return;
    const stamp = generation.current; running.current = true; setBusy(true); setError('');
    try { const value = await action(); if (stamp === generation.current) return value; }
    catch (reason) { if (stamp === generation.current) setError(classifyError ? classifyError(reason) : reason.status === 403 ? 'permissions' : 'unavailable'); }
    finally { if (stamp === generation.current) { running.current = false; setBusy(false); } }
  };
  const mutate = async (facts, action) => {
    if (running.current) return;
    // A denial of this replay cannot establish whether its earlier dispatch
    // committed. The retained invocation, rather than asynchronously rendered
    // state, is the authority for its original facts and idempotency key.
    const hadUnresolved = Boolean(intent.current);
    const current = intent.current || { facts, key: `finance-${secureRequestID()}` };
    intent.current = current;
    pending.set(identity, current);
    const stamp = generation.current; running.current = true; setBusy(true); setMessage(''); setError('');
    try {
      const value = await action(current.facts, current.key);
      if (stamp !== generation.current) return;
      intent.current = null; pending.delete(identity); setUnknown(false); setMessage('saved'); return value;
    } catch (reason) {
      if (stamp !== generation.current) return;
      const uncertain = hadUnresolved || reason.financeRequestState === 'WRITE_OBSERVED' ||
        (reason.financeRequestState !== 'NOT_DISPATCHED' &&
          (!Number.isInteger(reason.status) || reason.status >= 500 || reason.code === 'COMMAND_RESULT_UNKNOWN'));
      if (!uncertain) { intent.current = null; pending.delete(identity); }
      setUnknown(uncertain); setError(uncertain ? 'unknown' : reason.financeInputInvalid ? 'inputInvalid' : reason.status === 403 ? 'permissions' : 'sourceChanged');
    } finally { if (stamp === generation.current) { running.current = false; setBusy(false); } }
  };
  return { busy, unknown, pendingFacts: intent.current?.facts || null, message: copy[message] || '', error: copy[error] || '', read, mutate };
}
