import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(
  new URL('../src/platform/tauri/bridge/artifacts.js', import.meta.url),
  'utf8',
);
const webSource = await readFile(
  new URL('../src/platform/web/bridge.js', import.meta.url),
  'utf8',
);

function extractNamedFunction(input, name) {
  const start = input.indexOf(`function ${name}(`);
  assert.notEqual(start, -1, `${name} must exist`);
  const bodyStart = input.indexOf('{', start);
  let depth = 0;
  let quote = null;
  let escaped = false;
  for (let index = bodyStart; index < input.length; index += 1) {
    const char = input[index];
    if (quote) {
      if (escaped) escaped = false;
      else if (char === '\\') escaped = true;
      else if (char === quote) quote = null;
      continue;
    }
    if (char === '"' || char === "'" || char === '`') {
      quote = char;
      continue;
    }
    if (char === '{') depth += 1;
    if (char === '}' && --depth === 0) return input.slice(start, index + 1);
  }
  throw new Error(`${name} body is not closed`);
}

function createArtifactsFeature({ failOpen = false, state: stateOverride, sessionStates = {} } = {}) {
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  const sandbox = vm.createContext({ window: root, Uint8Array });
  vm.runInContext(source, sandbox, { filename: 'artifacts.js' });
  const calls = [];
  const state = stateOverride || {
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
    sessionStates,
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
  return { api, calls, state, sessionStates };
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

test('desktop artifact export attribution stays inside the target session', async () => {
  const activeArtifact = {
    path: 'C:\\workspace-a\\report.pdf',
    telemetryId: 'artifact-session-a',
  };
  const fixture = createArtifactsFeature({
    state: {
      activeSessionId: 'session-a',
      artifacts: [activeArtifact],
      attachments: [],
      sessions: [],
    },
  });

  await fixture.api.openArtifactExternal('C:\\workspace-b\\report.pdf', 'session-b');
  assert.deepEqual(
    fixture.calls.map(call => call.command),
    ['open_in_system'],
    'an unloaded target session must not fall back to the active session artifact',
  );

  fixture.sessionStates['session-b'] = {
    artifacts: [{
      path: 'C:\\workspace-b\\report.pdf',
      telemetryId: 'artifact-session-b',
    }],
  };
  await fixture.api.openArtifactExternal('C:\\workspace-b\\report.pdf', 'session-b');
  const tracked = fixture.calls.filter(call => call.command === 'track_behavior_event');
  assert.equal(tracked.length, 1);
  assert.equal(tracked[0].args.request.artifactId, 'artifact-session-b');
});

test('desktop artifact basename fallback requires one match in the target session', async () => {
  const fixture = createArtifactsFeature({
    state: {
      activeSessionId: 'session-a',
      artifacts: [],
      attachments: [],
      sessions: [],
    },
    sessionStates: {
      'session-b': {
        artifacts: [
          { path: 'relative-a\\report.pdf', telemetryId: 'artifact-b-1' },
          { path: 'relative-b\\report.pdf', telemetryId: 'artifact-b-2' },
        ],
      },
    },
  });

  await fixture.api.openArtifactExternal('C:\\workspace-b\\report.pdf', 'session-b');
  assert.equal(
    fixture.calls.filter(call => call.command === 'track_behavior_event').length,
    0,
  );
});

test('web artifact export attribution stays inside the downloaded session', () => {
  const calls = [];
  const state = {
    activeSessionId: 'session-a',
    artifacts: [{ path: '/workspace-a/report.pdf', telemetryId: 'artifact-a' }],
  };
  const sessionStates = {};
  const sandbox = vm.createContext({
    state,
    sessionStates,
    IS_WEB: true,
    basename: value => String(value).split(/[\\/]/).pop(),
    invoke(command, args) {
      calls.push({ command, args });
      return Promise.resolve(true);
    },
  });
  vm.runInContext([
    extractNamedFunction(webSource, 'artifactExportFormat'),
    extractNamedFunction(webSource, 'artifactsForSession'),
    extractNamedFunction(webSource, 'trackArtifactExported'),
    'globalThis.trackArtifactExportedForTest = trackArtifactExported;',
  ].join('\n'), sandbox, { filename: 'web-artifact-export-functions.js' });

  sandbox.trackArtifactExportedForTest('/workspace-b/report.pdf', 'session-b');
  assert.equal(calls.length, 0, 'an unloaded target session must not reuse the active artifact id');

  sessionStates['session-b'] = {
    artifacts: [{ path: '/workspace-b/report.pdf', telemetryId: 'artifact-b' }],
  };
  sandbox.trackArtifactExportedForTest('/workspace-b/report.pdf', 'session-b');
  assert.equal(calls.length, 1);
  assert.equal(calls[0].command, 'web_access_track_behavior_event');
  assert.equal(calls[0].args.request.artifactId, 'artifact-b');

  sessionStates['session-b'].artifacts = [
    { path: 'one/report.pdf', telemetryId: 'artifact-b-1' },
    { path: 'two/report.pdf', telemetryId: 'artifact-b-2' },
  ];
  sandbox.trackArtifactExportedForTest('/workspace-b/report.pdf', 'session-b');
  assert.equal(calls.length, 1, 'an ambiguous basename must not emit an export event');
});
