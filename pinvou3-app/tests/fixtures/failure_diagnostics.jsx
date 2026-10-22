import React from 'react';
import { createRoot } from 'react-dom/client';
window.diagnosticTest = { enabled: true, submitted: [], previews: [], files: [], reports: [] };
// Deterministic manual/IAB scenarios also work when no local CDP browser can
// launch. They affect this fixture only, never the production bridge.
const scenario = new URL(window.location.href).searchParams.get('scenario');
if (scenario === 'authorization') window.diagnosticTest.submitError = 'diagnostic_authorization_changed';
if (scenario === 'queue-full') window.diagnosticTest.submitError = 'diagnostic_queue_full';
if (scenario === 'expired') window.diagnosticTest.previewError = 'diagnostic_preview_expired';
if (scenario === 'four-files') window.diagnosticTest.files = ['a.log', 'b.log', 'c.log', 'd.log'];
if (scenario === 'disabled') window.diagnosticTest.enabled = false;
if (scenario === 'malformed-null') window.diagnosticTest.previewAttachments = [null];
if (scenario === 'malformed-kind') window.diagnosticTest.previewAttachments = [{ kind: 'image/svg+xml', content: '<svg />' }];
window.TauriBridge = {
  available: true,
  state: { getMany: () => ({ platformCapabilities: { failureDiagnosticsSupported: true } }), subscribe: () => () => {} },
  feedback: {
    listFailureDiagnostics: async () => {
      if (scenario === 'refresh-failure' && window.diagnosticTest.submitted.length) throw new Error('fixture refresh unavailable');
      return { enabled: window.diagnosticTest.enabled, incidents: [
      { diagnostic_id: 'incident_fixture_1', session_id: 'session_fixture', turn_id: 'turn_fixture', diagnostic_code: 'rate_limited', diagnostic_steps: [{ stage: 'model', offset_ms: 12 }] },
      { diagnostic_id: 'incident_other_turn', session_id: 'session_fixture', turn_id: 'other_turn', diagnostic_code: 'unknown' },
    ], reports: [...window.diagnosticTest.submitted.map(id => ({ report_id: id, incident_id: 'incident_fixture_1', status: 'queued' })), ...window.diagnosticTest.reports] };
    },
    pickDiagnosticFiles: async () => window.diagnosticTest.files,
    previewFailureDiagnostic: async request => {
      if (window.diagnosticTest.previewError) throw new Error(window.diagnosticTest.previewError);
      window.diagnosticTest.previews.push(request);
      return { preview_token: 'preview_fixture_1', report: { notes: request.notes, attachments: window.diagnosticTest.previewAttachments ?? [] } };
    },
    submitFailureDiagnostic: async token => {
      if (window.diagnosticTest.submitError) throw new Error(window.diagnosticTest.submitError);
      window.diagnosticTest.submitted.push(token); return { status: 'queued' };
    },
  },
};
const { default: DiagnosticsPanel } = await import('../../src/features/failure-diagnostics/DiagnosticsPanel.jsx');
const { failureDiagnosticsEn } = await import('../../src/shared/i18n/failure-diagnostics.js');
createRoot(document.getElementById('root')).render(<DiagnosticsPanel copy={failureDiagnosticsEn} sessionId="session_fixture" turnId="turn_fixture" />);
