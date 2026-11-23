import React from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/base.css';
import '../../src/styles/tailwind.css';
import { DeviceManagementView } from '../../src/features/megacube/DeviceManagementView.jsx';
import { dictZh } from '../../src/shared/i18n/zh.js';
import { dictEn } from '../../src/shared/i18n/en.js';
import { dictJa } from '../../src/shared/i18n/ja.js';

// Only this isolated fixture supplies transport responses. No system writes occur.
const params = new URLSearchParams(location.search);
const copy = { zh: dictZh, en: dictEn, ja: dictJa }[params.get('lang')] || dictZh;
const scenario = params.get('scenario') || 'normal';
window.__cmsCalls = [];
window.__cmsOffline = ['offline', 'service-repair'].includes(scenario);
const models = [
  { card: 'ornith15-35b-a3b-nvfp4-tachyon', model_id: 'Ornith-1.5-35B-A3B-NVFP4', managed: true, status: scenario === 'error' ? 'degraded' : scenario === 'stopped-default' ? 'stopped' : 'running', healthy: scenario !== 'error', boot_enabled: true, can_set_autostart: scenario === 'boot-race', recovery_owner: 'cmsd', port: 18080, measured_footprint_gib: 33, degraded_reason: scenario === 'error' ? 'health_probe_failed' : null },
  { card: 'external-with-a-very-long-internal-identifier-1234567890', model_id: 'External model', managed: false, can_adopt: false, status: 'running', healthy: null },
];
if (scenario === 'stopped-default') models.push({ card: 'secondary-stopped-card', model_id: 'Secondary stopped model', managed: true, status: 'stopped', healthy: false });
if (scenario === 'quiet') models.push({ card: 'legacy-stopped-card', model_id: '', managed: true, status: 'stopped', degraded_reason: 'runtime_missing', healthy: false });
window.__TAURI__ = { core: { invoke: async (command, args) => {
  window.__cmsCalls.push({ command, args });
  const available = data => window.__cmsOffline ? { available: false, reason: 'socket_missing' } : { available: true, data };
  switch (command) {
    case 'cms_status': return available({ version: '0.1.0', active_model: models[0].card, uptime_s: 4200, gateway: '127.0.0.1:8000', degraded: scenario === 'error' ? [{ card: models[0].card, reason: 'health_probe_failed' }] : [] });
    case 'cms_models': {
      const snapshot = structuredClone({ active_model: models[0].card, models });
      if (window.__pauseNextModels) {
        window.__pauseNextModels = false;
        await new Promise(resolve => { window.__releaseOldModels = resolve; });
      }
      return scenario === 'partial' ? { available: false } : available(snapshot);
    }
    case 'cms_engines': return available({ engines: [{ id: 'engine-sha-internal', path: '/engines/tachyon-serve/engine-sha-internal', engine_type: 'native', instances: [{ pid: 123456, model_id: models[0].model_id, port: 18080, alive: true, healthy: scenario !== 'error', source: 'managed' }] }] });
    case 'cms_capacity': return available({ memory: { total_gib: 121.7, available_gib: 56.4 }, safety_margin_gib: 9.7 });
    case 'cms_tasks': return available({ tasks: scenario === 'quiet' ? [] : [
      { id: 'task-running', card: 'Model download', kind: 'download', status: 'running', source: 'cmsd', progress_done_bytes: 1024, progress_total_bytes: 4096, last_event: { message: 'Downloading weights' } },
      { id: 'task-failed', card: 'Interrupted model', kind: 'install', status: 'failed', source: 'cmsd', error: 'network_interrupted' },
      ...Array.from({ length: 25 }, (_, i) => ({ id: `task-complete-${i}`, card: 'Completed model', kind: 'install', status: 'succeeded', source: 'cmsd', result: 'completed detail' })),
    ] });
    case 'cms_service_status': return { supported: true, installed: scenario !== 'service-repair', scope: 'user', activeState: window.__cmsOffline ? 'failed' : 'active', reason: 'external_installation', unitName: 'cmsd.service', version: '0.1.0', socketPath: '/run/megacube/internal-cmsd.sock', canStart: scenario !== 'service-repair' && window.__cmsOffline, canStop: !window.__cmsOffline, canUninstall: false, canUpdate: true, canRestore: window.__cmsOffline, canRollback: scenario === 'service-repair' };
    case 'cms_device_status': return { supported: true, detected: true, hardware: { state: 'compatible' }, running: { state: 'running', observedAt: '2026-10-19T08:00:00Z', models: [{ pid: 123456, kind: 'native' }], gpuProcesses: [] } };
    case 'cms_model_action': {
      if (scenario !== 'boot-race') throw new Error('fixture_action_failure');
      await new Promise(resolve => { window.__commitBootChange = () => {
        models[0].boot_enabled = args.action === 'enable-boot'; resolve();
      }; });
      return { boot_enabled: models[0].boot_enabled };
    }
    default: throw new Error(`Fixture command not implemented: ${command}`);
  }
} } };
if (params.get('dark')) document.documentElement.classList.add('dark');
document.documentElement.lang = { zh: 'zh-CN', en: 'en', ja: 'ja' }[params.get('lang')] || 'zh-CN';
createRoot(document.getElementById('root')).render(<div style={{height:'100vh'}}><DeviceManagementView theme="light" t={copy} bs={{}} device={{ isCube: true, managementAvailable: true }} onOpenModelStore={() => window.__cmsCalls.push({ command: 'open-model-store' })} /></div>);
