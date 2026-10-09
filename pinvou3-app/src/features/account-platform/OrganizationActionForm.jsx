import React, { useState } from 'react';
import { useFinanceOperation } from './useFinanceOperation.js';

// Each displayed action owns one immutable invocation. Fields remain frozen
// while its outcome is unknown; changing identity unmounts this scope.
export function OrganizationActionForm({ client, owner, copy, title, scope, fields, prepare, action, onConfirmed, confirm = false, disabled = false }) {
 const [values, setValues] = useState(() => Object.fromEntries(fields.map(field => [field.name, field.initial ?? (field.multiple ? [] : '')])));
 const [invalid, setInvalid] = useState(false);
 const operation = useFinanceOperation(owner, copy, client, scope);
 const submit = async event => {
  event.preventDefault(); setInvalid(false);
  if (confirm && !operation.unknown && !globalThis.confirm(copy.confirm)) return;
  let facts;
  try { facts = operation.pendingFacts || JSON.parse(JSON.stringify(prepare(values))); }
  catch { setInvalid(true); return; }
  const result = await operation.mutate(facts, action);
  if (result !== undefined) onConfirmed?.(result);
 };
 return <form className="ap-org-action" onSubmit={event => void submit(event)}><h4>{title}</h4>
  <fieldset disabled={operation.busy || operation.unknown || disabled}>{fields.map(field => <label key={field.name}>{copy[field.label || field.name]}{field.options ? <select multiple={field.multiple || false} value={values[field.name]} onChange={event => { const value = field.multiple ? [...event.target.selectedOptions].map(option => option.value) : event.target.value; setValues(old => ({ ...old, [field.name]: value })); }}>{!field.multiple && <option value="">{field.optional ? copy.root : '—'}</option>}{(typeof field.options === 'function' ? field.options(values) : field.options).map(option => <option key={option.value} value={option.value}>{option.label}</option>)}</select> : <input type={field.type || 'text'} autoComplete="off" value={values[field.name]} onChange={event => setValues(old => ({ ...old, [field.name]: event.target.value }))} required={!field.optional} />}{field.multiple && <small>{copy.unscopedUnits}</small>}</label>)}</fieldset>
  {invalid && <p role="alert">{copy.invalid}</p>}{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
  <button disabled={operation.busy || (disabled && !operation.unknown)} type="submit">{operation.unknown ? copy.retry : title}</button>
 </form>;
}
