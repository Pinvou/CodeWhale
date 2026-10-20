import { useEffect, useRef, useState } from 'react';
import { secureRequestID } from './request-id.js';
import { clearClosureIntent, readClosureIntent, saveClosureIntent } from './closure-intent.js';

// This feature's proof must survive the redirect to recent authentication.
// Current permission remains the caller's gate; storage never grants authority.
export function useClosureOperation(owner, copy, client, scope, storageAdapter) {
  const readIntent = storageAdapter?.read || readClosureIntent;
  const saveIntent = storageAdapter?.save || saveClosureIntent;
  const clearIntent = storageAdapter?.clear || clearClosureIntent;
  const actor = owner.split(':')[0];
  const generation = useRef(0), running = useRef(false), intent = useRef(null);
  const authority = useRef(null);
  const [busy, setBusy] = useState(false), [unknown, setUnknown] = useState(false);
  const [error, setError] = useState(''), [message, setMessage] = useState(''), [blocked, setBlocked] = useState(true);
  useEffect(() => {
    authority.current = { owner, scope, client, epoch: client.financeSessionEpoch };
    generation.current++; running.current = false; setBusy(false); setError(''); setMessage('');
    try { intent.current = readIntent(actor, scope); setUnknown(Boolean(intent.current)); setBlocked(false); }
    catch { intent.current = null; setUnknown(false); setBlocked(true); setError('recoveryUnavailable'); }
    return () => { generation.current++; };
  }, [actor, scope, owner, client, client.financeSessionEpoch, readIntent]);
  const mutate = async (facts, action) => {
    if (authority.current?.owner !== owner || authority.current.scope !== scope || authority.current.client !== client || authority.current.epoch !== client.financeSessionEpoch) return;
    if (running.current || blocked) return;
    const stamp = generation.current;
    const hadUnresolved = Boolean(intent.current);
    let current;
    try {
      current = intent.current || { actor, scope, key: `closure-${secureRequestID()}`, facts: JSON.parse(JSON.stringify(facts)) };
      intent.current = saveIntent(current);
    } catch { setBlocked(true); setError('recoveryUnavailable'); return; }
    running.current = true; setBusy(true); setError(''); setMessage('');
    try {
      const result = await action(current.facts, current.key);
      const noReceipt = ['reauthenticating', 'recent-authentication-required', 'cancelled', 'confirmation-mismatch'].includes(result?.status);
      if (!noReceipt || !hadUnresolved) {
        try { clearIntent(actor, scope, current.key); }
        catch {
          if (stamp === generation.current) { setBlocked(true); setUnknown(true); setError('recoveryUnavailable'); }
          return;
        }
      }
      if (stamp !== generation.current) return;
      if (noReceipt && hadUnresolved) { setUnknown(true); setError('unknown'); return result; }
      intent.current = null; setUnknown(false); if (!noReceipt) setMessage('saved'); return result;
    } catch (reason) {
      if (stamp !== generation.current) return;
      const uncertain = hadUnresolved || !Number.isInteger(reason.status) || reason.status >= 500 || reason.code === 'COMMAND_RESULT_UNKNOWN';
      if (!uncertain) {
        try { clearIntent(actor, scope, current.key); intent.current = null; }
        catch { setBlocked(true); setError('recoveryUnavailable'); return; }
      }
      setUnknown(uncertain); setError(uncertain ? 'unknown' : reason.status === 403 ? 'permissions' : 'sourceChanged');
    } finally { if (stamp === generation.current) { running.current = false; setBusy(false); } }
  };
  return { busy, unknown, blocked, error: copy[error] || '', message: copy[message] || '', pendingFacts: intent.current?.facts || null, mutate };
}

export function useClosedGrantOperation(owner, copy, client, scope) {
  const org = owner.split(':')[1];
  const durableScope = scope === 'closed-grant-reclaim-batch' ? `${scope}:${org}` : scope.replace('closed-grant-reclaim:', `closed-grant-reclaim:${org}:`);
  return useClosureOperation(owner, copy, client, durableScope);
}
