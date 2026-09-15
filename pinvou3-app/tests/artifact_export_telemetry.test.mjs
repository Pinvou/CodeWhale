import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(
  new URL('../src/platform/tauri/bridge/artifacts.js', import.meta.url),
  'utf8',
);

function createArtifactsFeature({ failOpen = false } = {}) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = vm.createContext({ window: root, Uint8Array });
  vm.runInContext(source, sandbox, { filename: 'artifacts.js' });
  const calls = [];
  const state = {
    activeSessionId: 'session-a',
    artifacts: [{
      path: 'C:\\workspace\\report.pdf',
      telemetryId: 'artifact-anonymous',
    }],
    attachments: [],
    sessions: [],
  };
  const api = root.__PINVOU_TAURI_BRIDGE_FEATURES__.artifacts({
    state,
    sessionStates: {},
    notify() {},
    async invoke(command, args) {
      calls.push({ command, args });
      if (failOpen && command === 'open_in_system') throw new Error('open failed');
      return true;
    },
    bt: key => key,
    addSystemItem() {},
    dialogOpen: null,
    basename: value => String(value).split(/[\\/]/).pop(),
    isDeliverable: () => true,
    isAbsPath: () => true,
    discardManagedAttachment: async () => {},
  });
  return { api, calls };
}

test('desktop artifact export is emitted only after the external open succeeds', async () => {
  const success = createArtifactsFeature();
  await success.api.openArtifactExternal('C:\\workspace\\report.pdf', 'session-a');
  assert.deepEqual(success.calls.map(call => call.command), [
    'open_in_system',
    'track_behavior_event',
  ]);
  assert.deepEqual({ ...success.calls[1].args.request }, {
    eventName: 'artifact_exported',
    artifactId: 'artifact-anonymous',
    exportFormat: 'pdf',
  });

  const failed = createArtifactsFeature({ failOpen: true });
  assert.equal(
    await failed.api.openArtifactExternal('C:\\workspace\\report.pdf', 'session-a'),
    false,
  );
  assert.deepEqual(failed.calls.map(call => call.command), ['open_in_system']);
});

test('desktop download alias does not double emit artifact export', async () => {
  const fixture = createArtifactsFeature();
  await fixture.api.downloadArtifact('C:\\workspace\\report.pdf', 'session-a');
  assert.equal(
    fixture.calls.filter(call => call.command === 'track_behavior_event').length,
    1,
  );
});
