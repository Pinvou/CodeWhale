import React, { useEffect, useState } from 'react';
import { productAdmissionCopy } from '../../shared/i18n.js';
import { useFinanceOperation } from './useFinanceOperation.js';
import { requireAdmissionMaterial, validateAdmissionUpload } from './product-admission-client.js';

export function ProductAdmissionWorkspace({ client, me, item, verifications, copy, language, onChanged, onLocked, disabled = false }) {
  const labels = productAdmissionCopy[language];
  const owner = `${me.user_id}:${me.platform_authorization_version || me.active_context?.platform_authorization_version || 0}`;
  const operation = useFinanceOperation(owner, copy, client, `manual-admission:${item.catalog_item_id}`);
  const [file, setFile] = useState(null);
  const [expiry, setExpiry] = useState('');
  const [materialID, setMaterialID] = useState('');
  const [material, setMaterial] = useState(null);
  const [review, setReview] = useState(null);
  const [reason, setReason] = useState('');
  const [inputError, setInputError] = useState(false);
  const ownLocked = operation.busy || operation.unknown;
  const locked = ownLocked || disabled;
  useEffect(() => { onLocked(ownLocked); return () => onLocked(false); }, [ownLocked, onLocked]);
  const execute = async (facts, key) => {
    if (facts.action === 'upload') {
      if (operation.unknown) {
        try {
          const value = requireAdmissionMaterial(await client.productAdmissionMaterialReceipt(item.catalog_item_id, key), item.catalog_item_id);
          if (value.catalog_version !== facts.version || value.size !== facts.file.size || value.media_type !== facts.file.type || Date.parse(value.intended_expires_at) !== Date.parse(facts.expiresAt)) throw new Error('INVALID_PRODUCT_ADMISSION_ACK');
          return value;
        } catch (error) { if (error.status !== 404) throw error; }
      }
      // Missing receipt does not prove non-commit. Replay the same bytes and key.
      return client.uploadProductAdmissionMaterial(item.catalog_item_id, facts.file, facts.version, facts.expiresAt, key);
    }
    if (facts.action === 'submit') return client.submitManualProductAdmission(item.catalog_item_id, facts.body, key);
    return client.decideManualProductAdmission(item.catalog_item_id, facts.verificationID, facts.body, key);
  };
  const submit = async action => {
    let facts = operation.pendingFacts;
    if (!facts) {
      setInputError(false);
      try {
        if (action === 'upload') {
          const expiresAt = new Date(expiry).toISOString(); validateAdmissionUpload(file, expiresAt);
          facts = { action, file, version: item.version, expiresAt };
        } else if (action === 'submit') {
          requireAdmissionMaterial(material, item.catalog_item_id);
          if (!reason.trim()) throw new Error('REASON_REQUIRED');
          facts = { action, body: { expected_version: item.version, material_id: material.material_id, reason: reason.trim() } };
        } else {
          if (!review || !reason.trim()) throw new Error('REVIEW_REQUIRED');
          facts = { action, verificationID: review.verification_id, body: { expected_version: review.snapshot.publication.catalog_version, submission_sha: review.submission_sha, decision: action, reason: reason.trim() } };
        }
      } catch { setInputError(true); return; }
      if (!globalThis.confirm(copy.confirmAction)) return;
    }
    const value = await operation.mutate(facts, execute);
    if (!value) return;
    if (facts.action === 'upload') { setMaterial(value); setMaterialID(value.material_id); setFile(null); }
    else { setReview(value); setReason(''); await onChanged(); }
  };
  const loadReview = async verificationID => {
    const value = await operation.read(async () => {
      const review = await client.manualProductAdmission(item.catalog_item_id, verificationID);
      const material = requireAdmissionMaterial(await client.productAdmissionMaterial(item.catalog_item_id, review.material_id), item.catalog_item_id);
      return { review, material };
    });
    if (value) { setReview(value.review); setMaterial(value.material); setMaterialID(value.material.material_id); setReason(''); }
  };
  const loadMaterial = async () => {
    const value = await operation.read(async () => requireAdmissionMaterial(await client.productAdmissionMaterial(item.catalog_item_id, materialID.trim()), item.catalog_item_id));
    if (value) { setMaterial(value); setReview(null); }
  };
  const download = async () => {
    const blob = await operation.read(() => client.downloadProductAdmissionMaterial(item.catalog_item_id, material));
    if (!blob) return;
    const href = URL.createObjectURL(blob);
    try {
      const anchor = document.createElement('a'); anchor.href = href;
      anchor.download = `${material.material_id}.${material.media_type === 'application/pdf' ? 'pdf' : material.media_type === 'image/png' ? 'png' : 'jpg'}`;
      anchor.click();
    } finally { window.setTimeout(() => URL.revokeObjectURL(href), 60_000); }
  };
  const alreadyVoted = review?.decisions.some(value => value.reviewer_id === me.user_id);
  return <section aria-label={labels.title}>
    <h5>{labels.title}</h5><p>{labels.instructions}</p>
    <button disabled={operation.busy} onClick={() => client.reauthenticate('/admin.html')}>{labels.reauthenticate}</button>
    <p>{labels.retention}</p>
    {inputError && <p role="alert">{copy.inputInvalid}</p>}{operation.error && <p role="alert">{operation.error}</p>}{operation.message && <p role="status">{operation.message}</p>}
    {operation.unknown && <button disabled={operation.busy} onClick={() => void submit('')}>{copy.retry}</button>}
    <label>{labels.original}<input type="file" accept="image/png,image/jpeg,application/pdf" disabled={locked} onChange={event => setFile(event.target.files?.[0] || null)} /></label>
    <label>{labels.expiry}<input type="datetime-local" value={expiry} disabled={locked} onChange={event => setExpiry(event.target.value)} /></label>
    <button disabled={locked || !file || !expiry} onClick={() => void submit('upload')}>{labels.upload}</button>
    <label>{labels.materialID}<input value={materialID} disabled={locked} onChange={event => { setMaterialID(event.target.value); setMaterial(null); setReview(null); }} /></label>
    <button disabled={locked || !materialID.trim()} onClick={() => void loadMaterial()}>{copy.details}</button>
    {material && <><dl><dt>{labels.digest}</dt><dd>{material.sha256}</dd><dt>{labels.original}</dt><dd>{material.media_type} / {material.size}</dd><dt>{labels.expiry}</dt><dd>{material.intended_expires_at}</dd></dl>
      <button disabled={locked || Boolean(material.purged_at)} onClick={() => void download()}>{labels.download}</button>
    </>}
    <label>{copy.reason}<textarea disabled={locked} value={reason} onChange={event => setReason(event.target.value)} /></label>
    <button disabled={locked || !material || Boolean(material.verification_id) || Boolean(material.purged_at) || !reason.trim()} onClick={() => void submit('submit')}>{labels.submit}</button>
    <ul>{verifications.filter(value => value.manual_submission).map(value => <li key={value.verification_id}>{value.status} <button disabled={locked} onClick={() => void loadReview(value.verification_id)}>{labels.review}</button></li>)}</ul>
    {review && <article><h5>{labels.review}</h5><p>{review.verification_id} / {review.status}</p><p>{labels.digest}: {review.submission_sha}</p>
      <pre>{JSON.stringify(review.snapshot, null, 2)}</pre>
      <ul>{review.decisions.map(value => <li key={value.reviewer_id}>{value.reviewer_id} / {value.decision} / {value.reason} / {value.decided_at}</li>)}</ul>
      {alreadyVoted && <p>{labels.alreadyVoted}</p>}
      <button disabled={locked || review.status !== 'PENDING' || alreadyVoted || !reason.trim()} onClick={() => void submit('APPROVE')}>{labels.approve}</button>
      <button disabled={locked || review.status !== 'PENDING' || alreadyVoted || !reason.trim()} onClick={() => void submit('REJECT')}>{labels.reject}</button>
    </article>}
  </section>;
}
