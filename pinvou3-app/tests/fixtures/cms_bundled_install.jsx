import React from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/base.css';
import { CmsDaemonPanel } from '../../src/features/megacube/CmsDaemonPanel.jsx';
import { CMSD_SERVICE_COPY } from '../../src/shared/i18n/cmsd-service.js';

// Controlled transport only: never executes a service or model operation.
const params = new URLSearchParams(location.search);
const scenario = params.get('scenario');
const copy = CMSD_SERVICE_COPY[params.get('lang')] || CMSD_SERVICE_COPY.zh;
const manifest = { version: '0.1.0', architecture: 'aarch64', sha256: 'a'.repeat(64) };
let installed = scenario === 'update', operation = null;
window.__calls = [];
window.__failStatus = false;
window.__TAURI__ = { core: { invoke: async (command, args) => {
  window.__calls.push({ command, args });
  if (command === 'cms_service_status') {
    if (window.__failStatus) throw new Error('fixture_status_failed');
    return { supported: true, scope: 'user', installed, recoveryScope: 'a'.repeat(64), operation,
      activeState: installed ? 'active' : 'inactive', canInstall: !installed && scenario !== 'repair',
      canUpdate: installed || scenario === 'repair' };
  }
  if (command === 'cms_service_bundled_package') {
    if (scenario === 'slow') await new Promise(resolve => { window.__releaseBundle = resolve; });
    if (scenario === 'corrupt') throw new Error('package_checksum_mismatch');
    if (scenario === 'missing') return { available: false };
    return { available: true, source: scenario === 'wrong-source' ? 'operator-selected-offline-package' : 'bundled-with-pinvou', packagePath: '/fixture/bundled-cmsd', manifest };
  }
  if (command === 'cms_service_inspect_package') return { manifest, source: 'operator-selected-offline-package' };
  if (command === 'cms_service_action') {
    if (scenario === 'rejected') throw new Error('component_busy');
    await new Promise(resolve => { window.__finishAction = () => {
      installed = true;
      operation = { id: 'fixture-operation', action: args.action, stage: 'done', status: 'succeeded' };
      resolve();
    }; });
    return { operationId: 'fixture-operation' };
  }
  throw new Error('Unexpected fixture command ' + command);
} } };
createRoot(document.getElementById('root')).render(<main style={{ padding: 12 }}><CmsDaemonPanel copy={copy} /></main>);
