import React from 'react';
import { createRoot } from 'react-dom/client';
import { dictZh } from '../../src/shared/i18n/zh.js';

// The real component is dynamically imported after its host bridge is installed.
// This fixture verifies renderer behavior, not Store, IPC or replay protection.
const state = window.archiveFixture = {
  active: [{ localSessionId: 'remote-proxy-hidden', title: 'Hidden offline', hidden: true }],
  archived: [], calls: [], failure: null, cleanup: true, refreshFails: false,
};
window.TauriBridge = { desktopRemoteControl: {
  async listRemoteProxyArchiveCandidates() {
    if (state.refreshFails) throw new Error('list_failed');
    return [...state.active];
  },
  async listRemoteProxyArchives(before) {
    state.calls.push(['list', before]);
    if (state.refreshFails) throw new Error('list_failed');
    const start = before ? state.archived.findIndex(record => record.archiveCursor === before) + 1 : 0;
    return state.archived.slice(start, start + 256);
  },
  async archiveRemoteProxyHistory(id) {
    state.calls.push(['archive', id]);
    if (state.failure) throw Object.assign(new Error(state.failure), { code: state.failure });
    const item = state.active.find(record => record.localSessionId === id);
    state.active = state.active.filter(record => record !== item);
    state.archived = [{ ...item, archived: true, archiveCursor: '9007199254740993' }];
    return state.cleanup;
  },
  async readRemoteProxyHistory(id) {
    state.calls.push(['read', id]);
    return { localSessionId: id, title: 'Archived conversation', archived: true,
      entries: [{ journalEpoch: 'epoch-1', sequence: 1, body: 'Preserved history',
        attachments: [{ id: 'artifact-1', displayName: 'result.txt' }] }] };
  },
} };
const { default: Panel } = await import('../../src/features/remote-control/RemoteProxyArchivePanel.jsx');
createRoot(document.getElementById('root')).render(<Panel t={dictZh} />);
