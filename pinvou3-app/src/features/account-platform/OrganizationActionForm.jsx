import React, { useEffect, useRef, useState } from 'react';
import { useFinanceOperation } from './useFinanceOperation.js';
import { OrganizationActionConfirmation } from './OrganizationActionConfirmation.jsx';

const clone = value => JSON.parse(JSON.stringify(value));
const initialValues = fields => Object.fromEntries(fields.map(field => [field.name, field.initial ?? (field.multiple ? [] : '')]));
const fieldOptions = (field, values) => typeof field.options === 'function' ? field.options(values) : field.options;
const currentSelections = (fields, values) => fields.every(field => {
 if (!field.options) return true;
 const selected = field.multiple ? values[field.name] : [values[field.name]];
 return selected.every(value => field.optional && value === '' || fieldOptions(field, values).some(option => option.value === value));
});

// Each displayed action owns one immutable invocation. Unknown outcomes retain
// the original hook facts/key; only fresh invocations require user confirmation.
export function OrganizationActionForm({ client, owner, copy, title, scope, fields, prepare, action, onConfirmed, confirm = false, disabled = false, confirmationTarget = '', editBeforeSubmit = false, editDrafts, useOperation = useFinanceOperation }) {
 const draftIdentity = `${owner}:${scope}`;
 const [values, setValues] = useState(() => editDrafts?.get(draftIdentity) || initialValues(fields));
 const [invalid, setInvalid] = useState(false);
 const [editing, setEditing] = useState(() => Boolean(editDrafts?.has(draftIdentity)));
 const [confirmation, setConfirmation] = useState(null);
 const [renderedReady, setRenderedReady] = useState(null);
 const operation = useOperation(owner, copy, client, scope);
 const live = useRef(null), ready = useRef(null), generation = useRef(0), pending = useRef(null), dispatching = useRef(null);
 const epoch = client.financeSessionEpoch || 0;
 live.current = { client, owner, scope, epoch, values, fields, prepare, action, onConfirmed, disabled, operation, confirmationTarget, renderedReady, editBeforeSubmit, editing, editDrafts, draftIdentity };
 const sameIdentity = snapshot => {
  const current = live.current, active = ready.current;
  return active && current.renderedReady === active && current.epoch === (current.client.financeSessionEpoch || 0) && snapshot.client === current.client && snapshot.owner === current.owner && snapshot.scope === current.scope && snapshot.epoch === current.epoch &&
   active.client === current.client && active.owner === current.owner && active.scope === current.scope && active.epoch === current.epoch && snapshot.generation === active.generation;
 };
 const cancel = expected => {
  if (expected && expected !== pending.current) return;
  pending.current = null; setConfirmation(null);
 };
 // This runs after the hook has restored this identity's retained invocation.
 // Render-time identity checks reject callbacks during the preceding effect gap.
 useEffect(() => {
  const active = { client, owner, scope, epoch, generation: ++generation.current };
  ready.current = active; setRenderedReady(active); dispatching.current = null; cancel();
  if (editBeforeSubmit) { const draft = editDrafts?.get(draftIdentity); setEditing(Boolean(draft)); if (draft) setValues(draft); }
  return () => { generation.current++; ready.current = null; pending.current = null; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [client, owner, scope, epoch, editDrafts]);
 const freshCurrent = snapshot => {
  const current = live.current;
  if (!snapshot || !sameIdentity(snapshot) || dispatching.current || current.disabled || current.operation.blocked || current.operation.busy || current.operation.unknown || current.operation.pendingFacts || snapshot.confirmationTarget !== current.confirmationTarget) return false;
  try {
   return JSON.stringify(current.values) === snapshot.valuesJSON && currentSelections(current.fields, current.values) && JSON.stringify(clone(current.prepare(current.values))) === snapshot.factsJSON;
  } catch { return false; }
 };
 useEffect(() => { if (pending.current && !freshCurrent(pending.current)) cancel(pending.current); });
 const execute = async (facts, snapshot) => {
  if (!sameIdentity(snapshot) || dispatching.current || live.current.operation.blocked || live.current.operation.busy) return;
  const marker = {}; dispatching.current = marker;
  try {
   const result = await live.current.operation.mutate(facts, live.current.action);
   if (result !== undefined && sameIdentity(snapshot)) { if (live.current.editBeforeSubmit) { live.current.editDrafts?.delete(live.current.draftIdentity); setEditing(false); } live.current.onConfirmed?.(result); }
  } finally { if (dispatching.current === marker) dispatching.current = null; }
 };
 const accept = snapshot => {
  if (snapshot !== pending.current) return;
  if (!freshCurrent(snapshot)) { cancel(snapshot); return; }
  cancel(snapshot); // Consume before dispatch so repeated/late callbacks cannot reuse it.
  return execute(snapshot.facts, snapshot);
 };
 const submit = event => {
  event.preventDefault(); setInvalid(false);
  const current = live.current, active = ready.current;
  if (client !== current.client || owner !== current.owner || scope !== current.scope || epoch !== current.epoch) return;
  if (!active || !sameIdentity(active) || dispatching.current || current.operation.blocked || current.operation.busy) return;
  if (current.operation.pendingFacts) { void execute(current.operation.pendingFacts, active); return; }
  if (current.operation.unknown || current.disabled || current.editBeforeSubmit && !current.editing) return;
  let facts;
  try { facts = clone(current.prepare(current.values)); }
  catch { setInvalid(true); return; }
  if (!confirm) { void execute(facts, active); return; }
  if (freshCurrent(pending.current)) return;
  const snapshot = { ...active, facts, factsJSON: JSON.stringify(facts), values: clone(current.values), valuesJSON: JSON.stringify(current.values), confirmationTarget: current.confirmationTarget };
  if (!freshCurrent(snapshot)) { setInvalid(true); return; }
  pending.current = snapshot; setConfirmation(snapshot);
 };
 // Rename opt-in shares the invocation owner: an unresolved write cannot be
 // cancelled into a fresh edit, even before the busy state has rendered.
 const changeEditing = next => {
  const current = live.current, active = ready.current;
  if (client !== current.client || owner !== current.owner || scope !== current.scope || epoch !== current.epoch || !active || !sameIdentity(active) || dispatching.current || current.disabled || current.operation.busy || current.operation.unknown || current.operation.pendingFacts || current.operation.blocked) return;
  const fresh = initialValues(current.fields);
  if (next) current.editDrafts?.set(current.draftIdentity, fresh); else current.editDrafts?.delete(current.draftIdentity);
  current.values = fresh; current.editing = next;
  setValues(fresh); setInvalid(false); setEditing(next);
 };
 const changeValue = (name, value) => {
  const current = live.current;
  if (!current.editBeforeSubmit) { setValues(previous => ({ ...previous, [name]: value })); return; }
  const next = { ...current.values, [name]: value }; current.values = next;
  if (current.editing) current.editDrafts?.set(current.draftIdentity, next);
  setValues(next);
 };
 if (editBeforeSubmit && !editing && !operation.busy && !operation.unknown && !operation.pendingFacts) return <div className="ap-org-action"><h4>{title}</h4><p>{fields[0]?.initial}</p>
  {operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
  <button type="button" disabled={disabled || operation.blocked || !renderedReady} onClick={() => changeEditing(true)}>{title}</button>
 </div>;
 const displayed = confirmation && freshCurrent(confirmation);
 const details = displayed ? fields.map(field => {
  const selected = field.multiple ? confirmation.values[field.name] : [confirmation.values[field.name]];
  return { name: field.name, label: copy[field.label || field.name], value: selected.map(value => value === '' && field.optional ? copy.root : fieldOptions(field, confirmation.values)?.find(option => option.value === value)?.label || value).join(', ') };
 }) : [];
 return <form className="ap-org-action" onSubmit={submit}><h4>{title}</h4>
  {displayed && <OrganizationActionConfirmation copy={copy} title={title} target={confirmation.confirmationTarget} details={details} onCancel={() => cancel(confirmation)} onConfirm={() => void accept(confirmation)} canRestoreFocus={() => freshCurrent(confirmation)} />}
  <fieldset disabled={operation.busy || operation.unknown || operation.blocked || disabled}>{fields.map(field => <label key={field.name}>{copy[field.label || field.name]}{field.options ? <select multiple={field.multiple || false} value={editBeforeSubmit ? operation.pendingFacts?.[field.name] ?? values[field.name] : values[field.name]} onChange={event => { const value = field.multiple ? [...event.target.selectedOptions].map(option => option.value) : event.target.value; changeValue(field.name, value); }}>{!field.multiple && <option value="">{field.optional ? copy.root : '—'}</option>}{fieldOptions(field, values).map(option => <option key={option.value} value={option.value}>{option.label}</option>)}</select> : <input type={field.type || 'text'} autoComplete="off" value={editBeforeSubmit ? operation.pendingFacts?.[field.name] ?? values[field.name] : values[field.name]} onChange={event => changeValue(field.name, event.target.value)} required={!field.optional} />}{field.multiple && <small>{copy.unscopedUnits}</small>}</label>)}</fieldset>
  {invalid && <p role="alert">{copy.invalid}</p>}{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
  <button disabled={operation.busy || operation.blocked || (disabled && !operation.unknown)} type="submit">{operation.unknown ? copy.retry : editBeforeSubmit ? copy.save : title}</button>
  {editBeforeSubmit && <button type="button" disabled={operation.busy || operation.unknown || operation.blocked || Boolean(operation.pendingFacts) || disabled} onClick={() => changeEditing(false)}>{copy.cancelConfirmation}</button>}
 </form>;
}
