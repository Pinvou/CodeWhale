import assert from 'node:assert/strict';
import test from 'node:test';
import { isDiagnosticPreview } from '../src/features/failure-diagnostics/preview.js';

test('diagnostic preview rejects malformed attachments before rendering or consent', () => {
  const preview = attachments => ({ preview_token: 'preview_fixture', report: { notes: 'reviewed text', attachments } });
  for (const bad of [null, undefined, {}, { preview_token: {} }, { preview_token: 'valid', report: [] },
    preview(null), preview([null]), preview([[]]), preview([{}]), preview([{kind:'text'}]),
    preview([{kind:'image/svg+xml',content:'<svg/>'}]), preview([{kind:'image/png',content:12}]),
    preview(Array(4).fill({kind:'text',content:'bounded'})), {...preview([]),preview_token:12}]) {
    assert.equal(isDiagnosticPreview(bad), false, JSON.stringify(bad));
  }
  assert.equal(isDiagnosticPreview(preview([])), true);
  assert.equal(isDiagnosticPreview(preview([{kind:'text',content:'http_status=429'},
    {kind:'image/png',content:'fixture'}, {kind:'image/jpeg',content:'fixture'}])), true);
});
