import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { ProductAdmissionWorkspace } from '../../src/features/account-platform/ProductAdmissionWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { financeCopy, productAdmissionCopy } from '../../src/shared/i18n.js';
const id = '11111111-1111-4111-8111-111111111111', materialID = '22222222-2222-4222-8222-222222222222', reviewID = '33333333-3333-4333-8333-333333333333';
const people = ['44444444-4444-4444-8444-444444444444', '55555555-5555-4555-8555-555555555555'];
const state = { calls: [], actor: people[0], material: null, review: null, unknownVote: true };
const client = new AdminPlatformClient(); const json = (value, status = 200) => new Response(JSON.stringify(value), { status });
globalThis.confirm = () => true;
globalThis.fetch = async (url, options = {}) => {
  state.calls.push({ url, method: options.method || 'GET', body: typeof options.body === 'string' ? options.body : null, key: options.headers?.['Idempotency-Key'] });
  if (url.includes('/admission-materials?')) {
    const sha256 = [...new Uint8Array(await crypto.subtle.digest('SHA-256', await options.body.arrayBuffer()))].map(byte => byte.toString(16).padStart(2, '0')).join('');
    state.material = { material_id: materialID, catalog_item_id: id, catalog_version: 1, uploaded_by: state.actor, media_type: options.body.type, size: options.body.size, sha256, intended_expires_at: new URL(url, location.origin).searchParams.get('intended_expires_at') };
    return json(state.material);
  }
  if (url.endsWith('/manual-admission-submissions')) {
    state.review = { verification_id: reviewID, catalog_item_id: id, material_id: materialID, submission_sha: 'b'.repeat(64), status: 'PENDING', requested_by: state.actor, snapshot: { environment: 'controlled-ui-test', publication: { catalog_item_id: id, catalog_version: 1, product_id: 'controlled-product', mapping: { amount_fen: '100', currency: 'CNY', sku_bom: 'controlled-sku' } }, original: { document_id: materialID, media_type: state.material.media_type, sha256: state.material.sha256 } }, decisions: [] };
    return json(state.review, 201);
  }
  if (url.endsWith('/decisions')) {
    if (state.unknownVote) { state.unknownVote = false; return json({ code: 'UPSTREAM_UNAVAILABLE' }, 503); }
    const body = JSON.parse(options.body);
    state.review.decisions.push({ reviewer_id: state.actor, decision: body.decision, reason: body.reason, decided_at: '2026-10-31T00:00:00Z' });
    if (state.review.decisions.length === 2) { state.review.status = 'VERIFIED'; state.review.evidence_id = reviewID; }
    return json(state.review);
  }
  if (url.endsWith(`/manual-admission-submissions/${reviewID}`)) return json(state.review);
  if (url.endsWith(`/admission-materials/${materialID}`)) return json(state.material);
  throw new Error('Unexpected controlled UI request');
};
function Fixture() {
  const [language, setLanguage] = useState('en'), [actor, setActor] = useState(people[0]), [, refresh] = useState(0);
  state.actor = actor; window.admissionFixture = { ...state, labels: productAdmissionCopy[language], copy: financeCopy[language], switchActor: () => setActor(people[1]) };
  return <><select aria-label="fixture-language" value={language} onChange={event => setLanguage(event.target.value)}>{['zh', 'en', 'ja'].map(value => <option key={value}>{value}</option>)}</select>
    <ProductAdmissionWorkspace key={actor} client={client} me={{ user_id: actor, platform_authorization_version: 1 }} item={{ catalog_item_id: id, version: 1 }} verifications={state.review ? [{ verification_id: reviewID, manual_submission: true, status: state.review.status }] : []} copy={financeCopy[language]} language={language} onLocked={() => {}} onChanged={async () => refresh(value => value + 1)} />
  </>;
}
createRoot(document.getElementById('root')).render(<Fixture />);
