import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { OrganizationClient } from './organization-client.js';
import { requireOrganizationModelPolicyCatalog } from './organization-model-policy-catalog.js';
import { useFinanceOperation } from './useFinanceOperation.js';

export function OrganizationModelPolicyWorkspace({ client, me, language }) {
 const copy = organizationCopy[language], org = me.active_context.organization_id;
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}`;
 const operation = useFinanceOperation(owner, copy, client, 'organization-model-policy');
 const [policy, setPolicy] = useState(null), [mode, setMode] = useState('ALLOW_ALL'), [ids, setIDs] = useState([]), [reason, setReason] = useState('');
 const [models, setModels] = useState([]), [unavailable, setUnavailable] = useState(false), [loading, setLoading] = useState(false);
 const generation = useRef(0);
 const canRead = hasPermission(me, 'org.models.read'), canManage = hasPermission(me, 'org.models.manage');
 const load = async (stamp, signal) => {
  const [value, catalog] = await Promise.all([api.modelPolicy(org, signal), client.models(signal)]);
  const modelItems = requireOrganizationModelPolicyCatalog(catalog);
  if (generation.current !== stamp) return;
  setPolicy(value); setMode(value.mode); setIDs(value.model_ids); setModels(modelItems); setUnavailable(false);
 };
 const refresh = async () => {
  const stamp = ++generation.current; setLoading(true);
  try { await load(stamp); } catch { if (generation.current === stamp) { setPolicy(null); setModels([]); setUnavailable(true); } }
  finally { if (generation.current === stamp) setLoading(false); }
 };
 useEffect(() => {
  const stamp = ++generation.current, controller = new AbortController();
  setPolicy(null); setModels([]); setReason(''); setUnavailable(false); setLoading(canRead);
   if (canRead) void load(stamp, controller.signal).catch(error => { if (generation.current === stamp && error.name !== 'AbortError') { setPolicy(null); setModels([]); setUnavailable(true); } }).finally(() => { if (generation.current === stamp) setLoading(false); });
  return () => { generation.current++; controller.abort(); };
  // Context changes invalidate both the policy read and catalog selection.
  // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [api, owner, canRead]);
 const save = async event => {
  event.preventDefault();
   if (!policy || !canManage || unavailable || loading) return;
  const facts = operation.pendingFacts || { expected_version: policy.version, mode, model_ids: mode === 'ALLOW_ALL' ? [] : [...ids].sort(), reason: reason.trim() };
  const result = await operation.mutate(facts, (body, key) => api.saveModelPolicy(org, body, key));
  if (result !== undefined) { setPolicy(result); setMode(result.mode); setIDs(result.model_ids); setReason(''); }
 };
 if (!canRead) return null;
 return <section className="ap-org-model-policy"><h3>{copy.modelPolicy}</h3><button disabled={loading || operation.busy || operation.unknown} onClick={() => void refresh()}>{copy.refresh}</button>
  {loading && <p role="status">{copy.loading}</p>}{unavailable && <p role="alert">{copy.unavailable}</p>}
  {policy && <form onSubmit={event => void save(event)}><p>{copy.version}: {policy.version}</p><fieldset disabled={!canManage || loading || operation.busy || operation.unknown}>
   <label>{copy.modelPolicyMode}<select value={mode} onChange={event => setMode(event.target.value)}>{['ALLOW_ALL', 'ALLOW_LIST', 'DENY_LIST'].map(value => <option key={value} value={value}>{copy[value]}</option>)}</select></label>
   {mode !== 'ALLOW_ALL' && <fieldset><legend>{copy.models}</legend>{models.map(model => <label key={model.model_id}><input type="checkbox" checked={ids.includes(model.model_id)} onChange={event => setIDs(current => event.target.checked ? [...current, model.model_id] : current.filter(id => id !== model.model_id))} />{model.display_name || model.upstream_model_ref || model.model_id}</label>)}
    {ids.filter(id => !models.some(model => model.model_id === id)).map(id => <label key={id}><input type="checkbox" checked onChange={() => setIDs(current => current.filter(value => value !== id))} />{id}</label>)}</fieldset>}
   <label>{copy.reason}<input value={reason} onChange={event => setReason(event.target.value)} maxLength={500} required autoComplete="off" /></label>
  </fieldset>{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
   {canManage && <button disabled={operation.busy || loading} type="submit">{operation.unknown ? copy.retry : copy.save}</button>}
  </form>}
 </section>;
}
