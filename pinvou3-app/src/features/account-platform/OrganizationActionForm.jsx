import { OrganizationUserPicker } from './OrganizationUserPicker.jsx';
import { PortalDialog } from './PortalDialog.jsx';
import React, { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { organizationNameInputError } from './organization-name-feedback.js';
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
export function OrganizationActionForm({ client, owner, copy, title, scope, fields, prepare, action, onConfirmed, confirm = false, disabled = false, confirmationTarget = '', confirmationContext = [], submitTargetHint = '', submitManualTargetHint = '', submitWaitingHint = '', description = '', editBeforeSubmit = false, editDrafts, draftContext, useOperation = useFinanceOperation, dialog = false, appearance = 'default', dangerous = false, actionStage, primaryTrigger = false, submitLabel = '' }) {
 const [dialogOpen, setDialogOpen] = useState(false);
 const [stageAttempt, setStageAttempt] = useState(null);
 const opener = useRef(null), wasOpen = useRef(false);
 const draftIdentity = `${owner}:${scope}`;
 const [values, setValues] = useState(() => editDrafts?.get(draftIdentity) || initialValues(fields));
 const [openedDraft, setOpenedDraft] = useState(null);
 const [previewIdentity, setPreviewIdentity] = useState(null);
 const draftIssue = draftContext && dialogOpen ? openedDraft === draftContext.revision ? draftContext.reason(values) : draftContext.changed : '';
 const [invalid, setInvalid] = useState(false);
 const [fieldErrors, setFieldErrors] = useState({});
 const fieldPrefix = useId();
 const [editing, setEditing] = useState(() => Boolean(editDrafts?.has(draftIdentity)));
 const [confirmation, setConfirmation] = useState(null);
 const [renderedReady, setRenderedReady] = useState(null);
 const [selectedPickerRecord, setSelectedPickerRecord] = useState(null);
 const ownerPickerAllowed = fields.some(field => field.pickerPresentation?.words && field.lookup);
 const ownerFocus = useRef({ node: null, priorDocument: null, lookup: ownerPickerAllowed });
 const attachOwnerField = useCallback(node => {
  const focus = ownerFocus.current;
  if (!node && focus.node) {
   const doc = focus.node.ownerDocument;
   focus.priorDocument = focus.node.contains(doc.activeElement) ? doc : null;
  }
  focus.node = node;
 }, []);
 useLayoutEffect(() => {
  const focus = ownerFocus.current, doc = focus.priorDocument;
  const changed = focus.lookup !== ownerPickerAllowed;
  focus.lookup = ownerPickerAllowed; focus.priorDocument = null;
  const active = doc?.activeElement;
  if (!changed || !doc || active?.isConnected && active !== doc.body) return;
  focus.node?.querySelector('input:not(:disabled)')?.focus();
 });
 useLayoutEffect(() => {
  if (!ownerPickerAllowed && selectedPickerRecord) setSelectedPickerRecord(null);
 }, [ownerPickerAllowed, selectedPickerRecord]);
 const operationCopy = fields.some(field => field.nameRule) ? { ...copy, sourceChanged: copy.nameSourceChanged } : copy;
 const operation = useOperation(owner, operationCopy, client, scope);
 const live = useRef(null), ready = useRef(null), generation = useRef(0), pending = useRef(null), dispatching = useRef(null);
 const epoch = client.financeSessionEpoch || 0;
 live.current = { client, owner, scope, epoch, values, fields, prepare, action, onConfirmed, disabled, operation, confirmationTarget, renderedReady, editBeforeSubmit, editing, editDrafts, draftIdentity, draftIssue, onDraftRejection: draftContext?.onKnownRejection, draftClosed: Boolean(draftContext && !dialogOpen) };
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
  ready.current = active; setRenderedReady(active); dispatching.current = null; cancel(); setFieldErrors({}); setStageAttempt(null);
  if (editBeforeSubmit) { const draft = editDrafts?.get(draftIdentity); setEditing(Boolean(draft)); if (draft) setValues(draft); }
  return () => { generation.current++; ready.current = null; pending.current = null; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
 }, [client, owner, scope, epoch, editDrafts]);
 const freshCurrent = snapshot => {
  const current = live.current;
  if (!snapshot || !sameIdentity(snapshot) || dispatching.current || current.disabled || current.draftIssue || current.draftClosed || current.operation.blocked || current.operation.busy || current.operation.unknown || current.operation.pendingFacts || snapshot.confirmationTarget !== current.confirmationTarget) return false;
  try {
   return JSON.stringify(current.values) === snapshot.valuesJSON && currentSelections(current.fields, current.values) && JSON.stringify(clone(current.prepare(current.values))) === snapshot.factsJSON;
  } catch { return false; }
 };
 useEffect(() => { if (pending.current && !freshCurrent(pending.current)) cancel(pending.current); });
 const execute = async (facts, snapshot) => {
  if (!sameIdentity(snapshot) || dispatching.current || live.current.operation.blocked || live.current.operation.busy) return;
  const marker = {}; dispatching.current = marker;
  if (actionStage) setStageAttempt(snapshot.generation);
  try {
   const onDraftRejection = live.current.onDraftRejection;
   const result = await live.current.operation.mutate(facts, live.current.action, onDraftRejection && (reason => { if (sameIdentity(snapshot)) onDraftRejection(reason); }));
   if (result !== undefined && sameIdentity(snapshot)) { if (live.current.editBeforeSubmit) { live.current.editDrafts?.delete(live.current.draftIdentity); setEditing(false); } live.current.onConfirmed?.(result); if (actionStage) setStageAttempt(null); setDialogOpen(false); }
  } finally { if (dispatching.current === marker) dispatching.current = null; }
 };
 const accept = snapshot => {
  if (snapshot !== pending.current) return;
  if (!freshCurrent(snapshot)) { cancel(snapshot); return; }
  cancel(snapshot); // Consume before dispatch so repeated/late callbacks cannot reuse it.
  return execute(snapshot.facts, snapshot);
 };
 const submit = event => {
  event.preventDefault(); setInvalid(false); setFieldErrors({});
  const current = live.current, active = ready.current;
  if (client !== current.client || owner !== current.owner || scope !== current.scope || epoch !== current.epoch) return;
  if (!active || !sameIdentity(active) || dispatching.current || current.operation.blocked || current.operation.busy) return;
  if (current.operation.pendingFacts) { void execute(current.operation.pendingFacts, active); return; }
  if (current.operation.unknown || current.disabled || current.draftIssue || current.draftClosed || current.editBeforeSubmit && !current.editing) return;
  const errors = Object.fromEntries(current.fields.filter(field => field.nameRule).map(field => [field.name, organizationNameInputError(current.values[field.name])]).filter(([, error]) => error));
  if (Object.keys(errors).length) { setFieldErrors(errors); return; }
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
  setValues(fresh); setInvalid(false); setFieldErrors({}); setEditing(next);
 };
 const rememberPickerRecord = candidate => {
  if (!renderedReady || !sameIdentity(renderedReady) || !live.current.fields.some(field => field.pickerPresentation?.words && field.lookup)) return;
  setSelectedPickerRecord(candidate ? { identity: renderedReady, record: { user_id: candidate.user_id, display_name: candidate.display_name, username: candidate.username, user_code: candidate.user_code, status: candidate.status } } : null);
 };
 const changeValue = (name, value) => {
  setFieldErrors(previous => ({ ...previous, [name]: '' }));
  const current = live.current;
  if (!current.editBeforeSubmit) { setValues(previous => ({ ...previous, [name]: value })); return; }
  const next = { ...current.values, [name]: value }; current.values = next;
  if (current.editing) current.editDrafts?.set(current.draftIdentity, next);
  setValues(next);
 };
 const open = dialogOpen || operation.unknown || Boolean(operation.pendingFacts);
 const closeDialog = () => {
  const current = live.current, active = ready.current;
  if (client !== current.client || owner !== current.owner || scope !== current.scope || epoch !== current.epoch || !active || !sameIdentity(active) || dispatching.current || current.operation.busy || current.operation.unknown || current.operation.pendingFacts) return;
  if (current.editBeforeSubmit) changeEditing(false);
  if (actionStage) setStageAttempt(null);
  setDialogOpen(false);
 };
 useLayoutEffect(() => { if (!open && wasOpen.current && !opener.current?.closest('[hidden]')) opener.current?.focus(); wasOpen.current = open; }, [open]);
 const adminAppearance = appearance === 'admin';
 const triggerClass = adminAppearance ? dangerous ? 'ap-admin-org-danger-trigger' : primaryTrigger ? 'ap-admin-org-primary-trigger' : undefined : undefined;
 const trigger = dialog && <button ref={opener} className={triggerClass} type="button" aria-expanded={open} disabled={open || disabled || operation.blocked || !renderedReady || operation.busy} onClick={() => { if (actionStage) setStageAttempt(null); if (editBeforeSubmit) changeEditing(true); if (draftContext) { setOpenedDraft(draftContext.revision); if (draftContext.preview) setPreviewIdentity(renderedReady); setValues(initialValues(fields)); setInvalid(false); setFieldErrors({}); } setDialogOpen(true); }}>{title}</button>;
 if (dialog && !open) return <>{trigger}</>;
 if (editBeforeSubmit && !editing && !operation.busy && !operation.unknown && !operation.pendingFacts) return <div className="ap-org-action"><h4>{title}</h4><p>{fields[0]?.initial}</p>
  {operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
  <button type="button" disabled={disabled || operation.blocked || !renderedReady} onClick={() => changeEditing(true)}>{title}</button>
 </div>;
 const displayed = confirmation && freshCurrent(confirmation);
 const pendingOwner = scope === 'transfer' ? operation.pendingFacts?.new_owner_user_id : undefined;
 const knownTarget = ownerPickerAllowed && selectedPickerRecord?.identity === renderedReady && selectedPickerRecord?.record.user_id === values.newOwner ? selectedPickerRecord.record : null;
 let targetMissing = false;
 if (submitTargetHint && !operation.unknown && !operation.pendingFacts) {
  try { prepare(values); } catch { targetMissing = true; }
 }
 const targetHintID = fieldPrefix + '-target-hint';
 const targetHint = ownerPickerAllowed ? submitTargetHint : submitManualTargetHint || submitTargetHint;
 const targetFeedback = operation.busy ? submitWaitingHint : targetMissing ? targetHint : '';
 const contextual = typeof confirmationContext === 'function';
 const ownerTransferSummary = adminAppearance && scope === 'transfer' && contextual;
 const confirmationRows = contextual ? confirmationContext(values, knownTarget).filter(row => !pendingOwner || row.name !== 'newOwner') : confirmationContext;
 const details = displayed ? [...confirmationRows, ...fields.filter(field => !contextual || !confirmationRows.some(row => row.name === field.name)).map(field => {
  const selected = field.multiple ? confirmation.values[field.name] : [confirmation.values[field.name]];
  return { name: field.name, label: copy[field.labelKey || field.label || field.name], value: selected.map(value => value === '' && field.optional ? copy.root : fieldOptions(field, confirmation.values)?.find(option => option.value === value)?.label || value).join(', ') };
 })] : [];
 const pendingSummary = operation.pendingFacts ? draftContext?.pendingSummary?.(operation.pendingFacts) : null;
 const renderOwnerField = field => {
  const { words } = field.pickerPresentation, value = values[field.name];
  if (pendingOwner) return <div key={field.name} className="adm-owner-picker-selection adm-owner-picker-pending"><strong>{words.pendingTarget}</strong><code dir="ltr">{pendingOwner}</code></div>;
  if (field.lookup) return <div key={field.name + ':lookup'} ref={attachOwnerField}>
   <OrganizationUserPicker client={client} copy={copy} value={value} presentation={field.pickerPresentation} remembered={selectedPickerRecord?.identity === renderedReady ? selectedPickerRecord?.record : null} disabled={operation.busy || operation.unknown || operation.blocked || disabled} onRemember={candidate => {
    if (!candidate || candidate.user_id === live.current.values[field.name]) rememberPickerRecord(candidate);
   }} onChange={(next, candidate) => { rememberPickerRecord(candidate); changeValue(field.name, next); }} />
  </div>;
  const helpID = fieldPrefix + '-owner-direct-help';
  return <label key={field.name + ':direct'} ref={attachOwnerField} className="adm-owner-picker-direct">
   {copy[field.labelKey || field.label || field.name]}
   <input autoComplete="off" value={value} onChange={event => changeValue(field.name, event.target.value)} required aria-describedby={helpID} />
   <small id={helpID}>{words.directHelp}</small>
  </label>;
 };
 const hasStageAttempt = stageAttempt !== null && stageAttempt === renderedReady?.generation;
 const stageError = actionStage && !hasStageAttempt && !operation.unknown ? null : operation.error;
 const stageText = !actionStage ? null : operation.busy ? actionStage.processing : operation.unknown ? actionStage.uncertain : stageError ? actionStage.rejected : actionStage.ready;
 const form = <form className={`ap-org-action${adminAppearance ? ' ap-admin-org-action' : ''}${adminAppearance && dangerous ? ' ap-admin-org-action-danger' : ''}${draftContext ? ' ap-admin-unit-move' : ''}`} onSubmit={submit}><h4>{title}</h4>
  {displayed && <OrganizationActionConfirmation copy={copy} title={title} target={confirmation.confirmationTarget} description={description} details={details} className={ownerTransferSummary ? 'ap-admin-owner-transfer-confirm' : ''} appearance={appearance} dangerous={dangerous} onCancel={() => cancel(confirmation)} onConfirm={() => void accept(confirmation)} canRestoreFocus={() => freshCurrent(confirmation)} />}
  {dialog && (confirmationTarget || confirmationRows.length > 0) && <dl className={ownerTransferSummary ? "ap-org-action-target ap-admin-owner-transfer-summary" : "ap-org-action-target"}>
   {confirmationTarget && <><dt>{copy.confirmationTarget}</dt><dd>{draftContext?.targetSummary?.() ?? confirmationTarget}</dd></>}
   {confirmationRows.map(item => <React.Fragment key={item.name}><dt>{item.label}</dt><dd>{item.value}</dd></React.Fragment>)}
  </dl>}
  {description && <p className="ap-org-action-description">{description}</p>}
  {pendingSummary || (!operation.unknown && !operation.pendingFacts && draftContext?.summary?.())}
  {fields.length > 0 && !pendingSummary && <fieldset disabled={operation.busy || operation.unknown || operation.blocked || disabled} aria-describedby={targetMissing ? targetHintID : undefined}>{fields.map(field => field.pickerPresentation?.words ? renderOwnerField(field) : <label key={field.name}>{copy[field.labelKey || field.label || field.name]}{field.lookup ? <OrganizationUserPicker client={client} copy={copy} value={values[field.name]} disabled={operation.busy || operation.unknown || operation.blocked || disabled} onChange={value => changeValue(field.name, value)} /> : field.options ? <select multiple={field.multiple || false} value={editBeforeSubmit ? operation.pendingFacts?.[field.name] ?? values[field.name] : values[field.name]} onChange={event => { const value = field.multiple ? [...event.target.selectedOptions].map(option => option.value) : event.target.value; changeValue(field.name, value); }}>{!field.multiple && <option value="">{field.emptyLabel ?? (field.optional ? copy.root : '—')}</option>}{fieldOptions(field, values).map(option => <option key={option.value} value={option.value}>{option.label}</option>)}</select> : <input type={field.type || 'text'} autoComplete="off" value={editBeforeSubmit ? operation.pendingFacts?.[field.name] ?? values[field.name] : values[field.name]} onChange={event => changeValue(field.name, event.target.value)} required={!field.optional && !field.nameRule} aria-required={field.nameRule ? true : undefined} aria-invalid={fieldErrors[field.name] ? true : undefined} aria-describedby={field.nameRule ? [`${fieldPrefix}-${field.name}-hint`, fieldErrors[field.name] ? `${fieldPrefix}-${field.name}-error` : '', operation.error ? `${fieldPrefix}-operation-error` : ''].filter(Boolean).join(' ') : undefined} />}{field.nameRule && <small className="ap-org-name-hint" id={`${fieldPrefix}-${field.name}-hint`}>{field.nameRule === 'unit' ? copy.unitNameRules : copy.organizationNameRules}</small>}{fieldErrors[field.name] && <small className="ap-org-name-error" id={`${fieldPrefix}-${field.name}-error`} role="alert">{copy[fieldErrors[field.name]]}</small>}{field.multiple && <small>{copy.unscopedUnits}</small>}</label>)}</fieldset>}
  {dialogOpen && !operation.unknown && !operation.pendingFacts && draftContext?.preview?.(values, openedDraft === draftContext.revision && previewIdentity === renderedReady && previewIdentity?.client === client && previewIdentity.owner === owner && previewIdentity.scope === scope && previewIdentity.epoch === epoch)}
  {draftIssue && !operation.unknown && !operation.pendingFacts && <p className="ap-org-move-draft-help" role="status">{draftIssue}</p>}
  {stageText && <p className="ap-admin-org-action-stage" id={fieldPrefix + "-action-stage"} role="status" aria-atomic="true">{stageText}</p>}
  {invalid && <p role="alert">{copy.invalid}</p>}{stageError && <p id={`${fieldPrefix}-operation-error`} role="alert">{stageError}</p>}{operation.message && <p role="status">{operation.message}</p>}
  {dialog ? <footer className={ownerTransferSummary && submitTargetHint ? "ap-org-action-footer ap-admin-owner-transfer-footer" : "ap-org-action-footer"}>{submitTargetHint && <p className="ap-org-action-footer-feedback" role="status" id={targetHintID}>{targetFeedback}</p>}<button type="button" disabled={operation.busy || operation.unknown || Boolean(operation.pendingFacts)} onClick={closeDialog}>{copy.cancelConfirmation}</button><button disabled={operation.busy || operation.blocked || (disabled && !operation.unknown) || (Boolean(draftIssue) && !operation.pendingFacts && !operation.unknown) || targetMissing} aria-describedby={targetFeedback ? targetHintID : undefined} type="submit">{operation.busy && submitWaitingHint ? copy.loading : operation.unknown ? copy.retry : submitLabel || (editBeforeSubmit ? copy.save : title)}</button></footer> : <><button disabled={operation.busy || operation.blocked || (disabled && !operation.unknown)} type="submit">{operation.unknown ? copy.retry : editBeforeSubmit ? copy.save : title}</button>
  {editBeforeSubmit && <button type="button" disabled={operation.busy || operation.unknown || operation.blocked || Boolean(operation.pendingFacts) || disabled} onClick={() => changeEditing(false)}>{copy.cancelConfirmation}</button>}</>}
 </form>;
 if (!dialog) return form;
 if (displayed) return <>{trigger}<div hidden>{form}</div></>;
 return <>{trigger}<PortalDialog title={title} descriptionID={stageText ? fieldPrefix + "-action-stage" : undefined} closeLabel={copy.cancelConfirmation} busy={operation.busy || operation.unknown || Boolean(operation.pendingFacts)} onClose={closeDialog}>{form}</PortalDialog></>;
}
