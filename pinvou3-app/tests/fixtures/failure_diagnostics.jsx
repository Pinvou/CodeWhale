import React from 'react';
import { createRoot } from 'react-dom/client';
window.diagnosticTest = { enabled: true, submitted: [], previews: [], files: [] };
window.TauriBridge = {
  available: true,
  state: { getMany: () => ({ platformCapabilities: { failureDiagnosticsSupported: true } }), subscribe: () => () => {} },
  feedback: {
    listFailureDiagnostics: async () => ({ enabled: window.diagnosticTest.enabled, incidents: [
      { diagnostic_id: 'incident_fixture_1', session_id: 'session_fixture', turn_id: 'turn_fixture', diagnostic_code: 'rate_limited', diagnostic_steps: [{ stage: 'model', offset_ms: 12 }] },
      { diagnostic_id: 'incident_other_turn', session_id: 'session_fixture', turn_id: 'other_turn', diagnostic_code: 'unknown' },
    ], reports: window.diagnosticTest.submitted.map(id => ({ report_id: id, incident_id: 'incident_fixture_1', status: 'queued' })) }),
    pickDiagnosticFiles: async () => window.diagnosticTest.files,
    previewFailureDiagnostic: async request => {
      window.diagnosticTest.previews.push(request);
      return { preview_token: 'preview_fixture_1', report: { notes: request.notes, attachments: [] } };
    },
    submitFailureDiagnostic: async token => {
      window.diagnosticTest.submitted.push(token); return { status: 'queued' };
    },
  },
};
const { default: DiagnosticsPanel } = await import('../../src/features/failure-diagnostics/DiagnosticsPanel.jsx');
const { failureDiagnosticsEn } = await import('../../src/shared/i18n/failure-diagnostics.js');
createRoot(document.getElementById('root')).render(<DiagnosticsPanel copy={failureDiagnosticsEn} sessionId="session_fixture" turnId="turn_fixture" />);
