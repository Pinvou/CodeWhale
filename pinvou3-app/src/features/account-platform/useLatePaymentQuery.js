import { useEffect, useRef, useState } from 'react';
import { secureRequestID } from './request-id.js';
import { isCurrentLatePaymentQueryAuthority } from './late-payment-query-contract.js';

const retainedByClient = new WeakMap();

// Authorization refresh retires operations, not the same actor's UNKNOWNs.
// Session and actor remain the journal's privacy boundary.
export function useLatePaymentQuery(owner, copy, client, enabled = true, authority) {
  let retained = retainedByClient.get(client);
  if (!retained) { retained = new Map(); retainedByClient.set(client, retained); }
  const sessionEpoch = client.financeSessionEpoch || 0;
  const identity = `${owner}:${sessionEpoch}:${enabled ? 'late-review' : 'anomaly'}`;
  const authorityKey = JSON.stringify(authority);
  const authorization = () => client.financeQueryAuthorizationGeneration || 0;
  let entries = retained.get(identity);
  if (!entries) { entries = []; retained.set(identity, entries); }
  const currentAuthority = () => sessionEpoch === (client.financeSessionEpoch || 0) &&
    authority?.actorID === owner && isCurrentLatePaymentQueryAuthority(authority, client.authorizationVersions);
  const generation = useRef(0);
  const observedAuthorization = useRef(authorization());
  const activeIdentity = useRef(identity);
  const activeAuthority = useRef(authorityKey);
  activeIdentity.current = identity; activeAuthority.current = authorityKey;
  const running = useRef(false);
  const [busy, setBusy] = useState(false);
  const [, redraw] = useState(0);
  const [message, setMessage] = useState(null);
  useEffect(() => {
    generation.current++;
    observedAuthorization.current = client.financeQueryAuthorizationGeneration || 0;
    running.current = false; setBusy(false); setMessage(null);
    const unsubscribe = client.onAuthorizationVersion?.(() => {
      const next = client.financeQueryAuthorizationGeneration || 0;
      if (observedAuthorization.current === next) return;
      observedAuthorization.current = next;
      generation.current++;
      running.current = false; setBusy(false); setMessage(null); redraw(value => value + 1);
    });
    return () => { generation.current++; unsubscribe?.(); };
  }, [client, identity, authorityKey]);

  const run = async (entry, fresh = false) => {
    if (running.current || !currentAuthority() || activeIdentity.current !== identity || activeAuthority.current !== authorityKey) return;
    const stamp = generation.current;
    const lineage = authorization();
    const current = () => stamp === generation.current && lineage === authorization() &&
      activeIdentity.current === identity && activeAuthority.current === authorityKey && currentAuthority();
    running.current = true; setBusy(true); setMessage(null);
    try {
      const result = fresh
        ? await client.queryLatePaymentReview(entry.facts.id, entry.facts, entry.key)
        : await client.queryLatePaymentReviewResult(entry.facts.id, entry.facts, entry.key, undefined, authority);
      if (!current()) return;
      entry.state = 'COMPLETED';
      setMessage({ identity, authorityKey, authorization: authorization() });
      return result;
    } catch {
      if (current()) entry.state = 'UNKNOWN';
    } finally {
      if (current()) { running.current = false; setBusy(false); redraw(value => value + 1); }
    }
  };
  const begin = facts => {
    if (running.current || !currentAuthority() || activeIdentity.current !== identity || activeAuthority.current !== authorityKey ||
      facts?.actorID !== owner || JSON.stringify(facts.authorization) !== authorityKey) return;
    const entry = { facts: Object.freeze({ ...facts }), key: `finance-${secureRequestID()}`, state: 'UNKNOWN' };
    entries.push(entry);
    return run(entry, true);
  };
  const retry = key => {
    const entry = entries.find(value => value.key === key && value.state === 'UNKNOWN');
    return entry ? run(entry) : undefined;
  };
  return { busy, retired: !currentAuthority(), unknown: entries.some(value => value.state === 'UNKNOWN'),
    unknownQueries: entries.filter(value => value.state === 'UNKNOWN').map(value => ({ key: value.key, facts: value.facts })),
    message: message?.identity === identity && message.authorityKey === authorityKey && message.authorization === authorization()
      ? copy.queryRecorded : '', begin, retry };
}
