import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy, accountClosedGrantCopy } from '../../shared/i18n.js';
import { OrganizationClient } from './organization-client.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';
import { hasPermission } from './authorization-ui.js';
import { integerMoney } from './finance-contract.js';
import { formatBalance } from './balance-view.js';
export function OrganizationClosedGrantsWorkspace({ client, me, language }) {
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const copy = { ...organizationCopy[language], ...accountClosedGrantCopy[language] };
 const org = me.active_context.organization_id;
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}:${client.financeSessionEpoch || 0}`;
 const permitted = hasPermission(me, 'org.grants.reclaim_closed') && hasPermission(me, 'org.quota.read.self');
 const [grants, setGrants] = useState([]), [busy, setBusy] = useState(false), [error, setError] = useState(false);
 const generation = useRef(0);
 const identityGeneration = useRef(0);
 const unresolved = useRef(new Map());
 const load = async (stamp, signal) => {
  const items = await api.grants(org, signal);
  if (stamp === generation.current) setGrants(items);
 };
 const refresh = async () => {
  const stamp = ++generation.current;
  setBusy(true); setError(false);
  try { await load(stamp); }
  catch { if (stamp === generation.current) { setGrants([]); setError(true); } }
  finally { if (stamp === generation.current) setBusy(false); }
 };
 useEffect(() => {
  const stamp = ++generation.current, controller = new AbortController();
  identityGeneration.current++;
  unresolved.current.clear();
  setGrants([]); setError(false); setBusy(permitted);
  if (permitted) void load(stamp, controller.signal).catch(reason => {
   if (stamp === generation.current && reason.name !== 'AbortError') setError(true);
  }).finally(() => { if (stamp === generation.current) setBusy(false); });
  return () => { generation.current++; controller.abort(); };
 }, [api, owner, permitted]); // eslint-disable-line react-hooks/exhaustive-deps
 if (!permitted) return null;
 const eligible = grants.filter(grant => grant.status === 'SUSPENDED' && grant.suspension_reason === 'ACCOUNT_CLOSED' && integerMoney(grant.reserved_microcredits) === 0n && integerMoney(grant.available_microcredits) > 0n);
 // A list read cannot confirm this invocation. Retain its original card until
 // the same-key write returns a known receipt, even after the list changes.
 const candidates = [...new Map([...eligible.map(grant => [grant.grant_id, grant]), ...unresolved.current]).values()];
 const reclaim = async (grant, facts, key) => {
  const stamp = identityGeneration.current;
  unresolved.current.set(grant.grant_id, grant);
  try {
   const result = await api.reclaimClosedGrant(org, facts.grant, facts.reason, key);
   if (stamp === identityGeneration.current) unresolved.current.delete(grant.grant_id);
   return result;
  } catch (reason) {
   // Match the existing operation's UNKNOWN boundary; it remains the owner
   // of frozen request facts and the original idempotency key.
   const unknown = !Number.isInteger(reason.status) || reason.status >= 500 || reason.code === 'COMMAND_RESULT_UNKNOWN';
   if (!unknown && stamp === identityGeneration.current) {
    unresolved.current.delete(grant.grant_id);
    setGrants(items => [...items]);
   }
   throw reason;
  }
 };
 return <section><h3>{copy.title}</h3><p>{copy.lead}</p>
  <button disabled={busy} onClick={() => void refresh()}>{copy.refresh}</button>
  {busy && <p role="status">{copy.loading}</p>}{error && <p role="alert">{copy.unavailable}</p>}
  {candidates.map(grant => <article key={`${owner}:${grant.grant_id}`}>
   <p>{grant.member_user_id} · {grant.grant_id} · {formatBalance(grant.available_microcredits, language)}</p>
   <OrganizationActionForm client={client} owner={owner} copy={copy} scope={`closed-grant-reclaim:${grant.grant_id}`} title={copy.single} fields={[]} prepare={() => ({ grant: grant.grant_id, reason: 'ACCOUNT_CLOSED_FULL_RECLAIM' })} action={(facts, key) => reclaim(grant, facts, key)} onConfirmed={() => void refresh()} disabled={busy || error || !eligible.some(item => item.grant_id === grant.grant_id)} confirm />
  </article>)}
 </section>;
}
