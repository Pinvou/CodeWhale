/* eslint-disable no-promise-executor-return -- Promise executors adapt event and server-close APIs whose registration handles are intentionally ignored. */
import assert from 'node:assert/strict';
import {
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { createBrowserWrapperHost } from './helpers/browser-wrapper-host-fixture.mjs';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import test from 'node:test';
import { spawnNdjsonChild, stopNdjsonChild } from './helpers/ndjson-child-driver.mjs';
import { cleanupBrowserFixture, confirmedChildStopper, confirmedProcessExited, readPendingHostRequest } from './helpers/browser-wrapper-fixture-cleanup.mjs';
import { heldRequestResultDiagnostic, heldRequestAuthorizationDiagnostic } from './helpers/browser-wrapper-held-result.mjs';

const WRAPPER = fileURLToPath(new URL(
  '../src-tauri/resources/common/bundle/mcp-servers/browser-wrapper.mjs',
  import.meta.url,
));
const SESSION_ID = 'windows-lifecycle-a';
const SESSION_TOKEN = '0123456789abcdef';
const OTHER_SESSION_TOKEN = 'fedcba9876543210';
// A missing workspace may require prepare (25s), a CDP probe (1s), and
// list/select (15s each). Observe that existing path without shortening its
// deadlines; this changes only these read-only recovery test requests.
const WORKSPACE_RECOVERY_OBSERVATION_MS = 60_000;

const FAKE_MCP = String.raw`
import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';

const workspacePath = process.env.FAKE_BROWSER_WORKSPACE;
const modePath = process.env.FAKE_BROWSER_MCP_MODE;
const auditPath = process.env.FAKE_BROWSER_MCP_AUDIT;
let selectedPageId = null;
let input = '';
const pendingToolTimers = new Map();

function audit(value) {
  appendFileSync(auditPath, JSON.stringify({ pid: process.pid, at: Date.now(), ...value }) + '\n');
}

function mode() {
  try { return JSON.parse(readFileSync(modePath, 'utf8')); } catch { return {}; }
}

function currentPages() {
  const workspace = JSON.parse(readFileSync(workspacePath, 'utf8'));
  const currentMode = mode();
  const active = workspace.tabs.find((candidate) => candidate.token === workspace.active_tab);
  const activeId = Number(active?.target_id.match(/-(\d+)$/)?.[1] || 1);
  return workspace.tabs.map((tab) => {
    const suffix = Number(tab.target_id.match(/-(\d+)$/)?.[1] || 1);
    return {
      id: suffix,
      target_id: tab.target_id,
      url: currentMode.bootstrapBlank && !currentMode.navigated
        ? 'about:blank'
        : 'https://' + suffix + '.example/',
      title: 'Page ' + suffix,
      selected: (selectedPageId ?? activeId) === suffix,
    };
  });
}

function write(message) {
  process.stdout.write(JSON.stringify(message) + '\n');
}

function result(id, value) {
  write({ jsonrpc: '2.0', id, result: value });
}

function error(id, message) {
  write({ jsonrpc: '2.0', id, error: { code: -32000, message } });
}

function scheduleToolCompletion(id, complete, delayMs, releasePath) {
  const completeWhenReleased = () => {
    if (releasePath && !existsSync(releasePath)) {
      pendingToolTimers.set(id, setTimeout(completeWhenReleased, 10));
      return;
    }
    complete();
  };
  if (delayMs > 0) pendingToolTimers.set(id, setTimeout(completeWhenReleased, delayMs));
  else completeWhenReleased();
}

audit({ kind: 'start' });
process.stdin.on('data', (chunk) => {
  input += chunk;
  let newline;
  while ((newline = input.indexOf('\n')) >= 0) {
    const line = input.slice(0, newline);
    input = input.slice(newline + 1);
    if (!line.trim()) continue;
    const message = JSON.parse(line);
    if (message.method === 'initialize') {
      const currentMode = mode();
      if (currentMode.handshakeError) {
        error(message.id, 'configured startup handshake rejection');
        continue;
      }
      const finishHandshake = () => result(message.id, {
          protocolVersion: message.params?.protocolVersion ?? '2024-11-05',
          capabilities: { tools: {} },
          serverInfo: { name: 'fake-windows-mcp', version: '0' },
        });
      const handshakeDelayMs = Number(currentMode.handshakeDelayMs) || 0;
      if (handshakeDelayMs > 0) setTimeout(finishHandshake, handshakeDelayMs);
      else finishHandshake();
      continue;
    }
    if (message.method === 'tools/list') {
      if (mode().toolsListError) {
        error(message.id, 'configured runtime catalog rejection');
        continue;
      }
      result(message.id, {
        tools: [
          { name: 'list_pages', inputSchema: { type: 'object', properties: {} } },
          {
            name: 'select_page',
            inputSchema: {
              type: 'object',
              properties: { pageId: { type: 'number' } },
              required: ['pageId'],
            },
          },
          {
            name: 'navigate_page',
            inputSchema: {
              type: 'object',
              properties: {
                pageId: { type: 'number' },
                type: { type: 'string' },
                url: { type: 'string' },
              },
              required: ['pageId'],
            },
          },
          {
            name: 'click',
            annotations: { category: 'input' },
            inputSchema: {
              type: 'object',
              properties: {
                pageId: { type: 'number' },
                uid: { type: 'string' },
              },
              required: ['pageId', 'uid'],
            },
          },
          {
            name: 'type_text',
            annotations: { category: 'input' },
            inputSchema: {
              type: 'object',
              properties: {
                pageId: { type: 'number' },
                text: { type: 'string' },
              },
              required: ['pageId', 'text'],
            },
          },
          {
            name: 'evaluate_script',
            inputSchema: {
              type: 'object',
              properties: {
                pageId: { type: 'number' },
                function: { type: 'string' },
              },
              required: ['pageId', 'function'],
            },
          },
          {
            name: 'take_snapshot',
            inputSchema: {
              type: 'object',
              properties: { pageId: { type: 'number' } },
              required: ['pageId'],
            },
          },
          {
            name: 'close_page',
            inputSchema: {
              type: 'object',
              properties: { pageId: { type: 'number' } },
              required: ['pageId'],
            },
          },
        ],
      });
      continue;
    }
    if (message.method === 'notifications/cancelled') {
      audit({
        kind: 'cancel',
        requestId: message.params?.requestId,
        reason: message.params?.reason,
      });
      // Deliberately ignore cooperative cancellation. The wrapper must keep
      // the pending request alive until this fake upstream really settles,
      // rather than ending the host operation while trusted input may remain.
      continue;
    }
    if (message.method !== 'tools/call') continue;

    const name = message.params?.name;
    const args = message.params?.arguments ?? {};
    audit({ kind: 'tool', name, args });
    if (name === 'list_pages') {
      const currentMode = mode();
      if (currentMode.listPagesErrorAfterNavigate && currentMode.navigated) {
        error(message.id, 'configured post-navigation list rejection');
        continue;
      }
      if (currentMode.listPagesErrorWhenSingle) {
        const workspace = JSON.parse(readFileSync(workspacePath, 'utf8'));
        if (workspace.tabs.length === 1) {
          error(message.id, 'configured post-close list rejection');
          continue;
        }
      }
      const pages = currentPages();
      result(message.id, {
        content: [{
          type: 'text',
          text: '## Pages\n' + pages.map((page) =>
            page.id + ': ' + page.title + ' (' + page.url + ')' +
              (page.selected ? ' [selected]' : '')
          ).join('\n'),
        }],
        structuredContent: { pages },
      });
      continue;
    }
    if (name === 'navigate_page') {
      const currentMode = mode();
      writeFileSync(modePath, JSON.stringify({ ...currentMode, navigated: true }));
      if (currentMode.navigateError) error(message.id, currentMode.navigateError);
      else if (currentMode.navigateToolError) {
        result(message.id, {
          isError: true,
          content: [{ type: 'text', text: currentMode.navigateToolError }],
        });
      } else result(message.id, { content: [{ type: 'text', text: 'navigated' }] });
      continue;
    }
    if (name === 'select_page') {
      selectedPageId = args.pageId;
      result(message.id, { content: [{ type: 'text', text: 'selected' }] });
      continue;
    }
    if (name === 'click') {
      const currentMode = mode();
      const complete = () => {
        pendingToolTimers.delete(message.id);
        audit({ kind: 'tool-complete', name, requestId: message.id });
        if (currentMode.clickError) error(message.id, currentMode.clickError);
        else result(message.id, { content: [{ type: 'text', text: 'clicked' }] });
      };
      const delayMs = Number(currentMode.clickDelayMs) || 0;
      scheduleToolCompletion(message.id, complete, delayMs, currentMode.clickReleasePath);
      continue;
    }
    if (name === 'type_text' || name === 'evaluate_script') {
      const currentMode = mode();
      const modePrefix = name === 'type_text' ? 'typeText' : 'evaluate';
      audit({ kind: 'tool-complete', name, requestId: message.id });
      if (currentMode[modePrefix + 'Error']) {
        error(message.id, currentMode[modePrefix + 'Error']);
      } else if (currentMode[modePrefix + 'ToolError']) {
        result(message.id, {
          isError: true,
          content: [{ type: 'text', text: currentMode[modePrefix + 'ToolError'] }],
        });
      } else {
        result(message.id, {
          content: [{ type: 'text', text: name === 'type_text' ? 'typed' : 'evaluated' }],
        });
      }
      continue;
    }
    if (name === 'take_snapshot') {
      const currentMode = mode();
      const complete = () => {
        pendingToolTimers.delete(message.id);
        audit({ kind: 'tool-complete', name, requestId: message.id });
        if (currentMode.snapshotToolError) {
          result(message.id, {
            isError: true,
            content: [{ type: 'text', text: currentMode.snapshotToolError }],
          });
        } else result(message.id, { content: [{ type: 'text', text: 'snapshot' }] });
      };
      const delayMs = Number(currentMode.snapshotDelayMs) || 0;
      scheduleToolCompletion(message.id, complete, delayMs, currentMode.snapshotReleasePath);
      continue;
    }
    error(message.id, 'unexpected tool: ' + name);
  }
});
`;

const CATALOG = {
  initializeResult: {
    protocolVersion: '2024-11-05',
    capabilities: { tools: {} },
    serverInfo: { name: 'fake-windows-mcp', version: '0' },
  },
  toolsListResult: {
    tools: [
      { name: 'list_pages', inputSchema: { type: 'object', properties: {} } },
      {
        name: 'select_page',
        inputSchema: {
          type: 'object',
          properties: { pageId: { type: 'number' } },
          required: ['pageId'],
        },
      },
      {
        name: 'navigate_page',
        inputSchema: {
          type: 'object',
          properties: { type: { type: 'string' }, url: { type: 'string' } },
          required: ['url'],
        },
      },
      {
        name: 'click',
        annotations: { category: 'input' },
        inputSchema: {
          type: 'object',
          properties: { uid: { type: 'string' } },
          required: ['uid'],
        },
      },
      {
        name: 'type_text',
        annotations: { category: 'input' },
        inputSchema: {
          type: 'object',
          properties: { text: { type: 'string' } },
          required: ['text'],
        },
      },
      {
        name: 'evaluate_script',
        inputSchema: {
          type: 'object',
          properties: { function: { type: 'string' } },
          required: ['function'],
        },
      },
      {
        name: 'take_snapshot',
        inputSchema: { type: 'object', properties: {} },
      },
      {
        name: 'close_page',
        inputSchema: {
          type: 'object',
          properties: { pageId: { type: 'number' } },
          required: ['pageId'],
        },
      },
    ],
  },
};

function atomicWriteJson(path, value) {
  const temporary = `${path}.${process.pid}.tmp`;
  writeFileSync(temporary, JSON.stringify(value));
  renameSync(temporary, path);
}

function readAudit(path) {
  if (!existsSync(path)) return [];
  return readFileSync(path, 'utf8')
    .split(/\r?\n/)
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

// Cross-process observables (host-request files, audit entries, counters
// published by the wrapper subprocess) only need to eventually appear; every
// call site asserts state, not speed. The default budget absorbs loaded
// Windows CI runners where subprocess startup and stdout delivery can stall
// for seconds (11-04/11-05 flakes: a 3s budget expired before the wrapper
// finished publishing an in-flight close request).
async function waitUntil(predicate, message, timeoutMs = 10_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await sleep(10);
  }
  assert.fail(typeof message === 'function' ? await message() : message);
}

async function makeFixture() {
  const root = mkdtempSync(join(tmpdir(), 'pinvou-windows-lifecycle-'));
  const binDirectory = join(root, 'pkg', 'build', 'src', 'bin');
  const workspaceDirectory = join(root, 'workspaces');
  mkdirSync(binDirectory, { recursive: true });
  mkdirSync(workspaceDirectory, { recursive: true });

  const mcpBin = join(binDirectory, 'fake-mcp.mjs');
  const workspacePath = join(workspaceDirectory, `${SESSION_TOKEN}.json`);
  const otherWorkspacePath = join(workspaceDirectory, `${OTHER_SESSION_TOKEN}.json`);
  const modePath = join(root, 'mcp-mode.json');
  const auditPath = join(root, 'mcp-audit.ndjson');
  const portPath = join(root, 'cdp-port.json');
  writeFileSync(mcpBin, FAKE_MCP);
  writeFileSync(join(root, 'pkg', 'catalog-shim.json'), JSON.stringify(CATALOG));
  writeFileSync(modePath, '{}');
  writeFileSync(auditPath, '');
  atomicWriteJson(otherWorkspacePath, {
    version: 2,
    mapping_authority: 'host',
    revision: 1,
    session_token: OTHER_SESSION_TOKEN,
    active_tab: OTHER_SESSION_TOKEN,
    tabs: [{ token: OTHER_SESSION_TOKEN, target_id: 'target-b-99' }],
  });


  return {
    root,
    mcpBin,
    workspacePath,
    otherWorkspacePath,
    modePath,
    auditPath,
    portPath,
    releaseGates: new Set(),
    cdp: null,
  };
}

function ownedReleaseGate(fixture, filename) {
  const path = join(fixture.root, filename);
  fixture.releaseGates.add(path);
  return path;
}

async function createFakeHost(fixture, options) {
  return createBrowserWrapperHost(fixture, { sessionId: SESSION_ID, sessionToken: SESSION_TOKEN }, options);
}

function driveWrapper(fixture) {
  const driver = spawnNdjsonChild({
    args: [WRAPPER, fixture.mcpBin, fixture.portPath],
    env: {
      ...process.env,
      PINVOU3_BROWSER_SESSION_ID: SESSION_ID,
      PINVOU3_BROWSER_SESSION_TOKEN: SESSION_TOKEN,
      FAKE_BROWSER_WORKSPACE: fixture.workspacePath,
      FAKE_BROWSER_MCP_MODE: fixture.modePath,
      FAKE_BROWSER_MCP_AUDIT: fixture.auditPath,
    },
    timeoutMs: 15_000,
  });
  return {
    ...driver,
    stopConfirmed: confirmedChildStopper(driver.child, stopNdjsonChild),
    abandonRequest(id) {
      driver.dropRequest(id, { abandoned: true });
    },
  };
}

// Held tools share one observation policy, above the 30s release barrier.
// The real slow-snapshot regression below rejects a return to the 15s default.
function requestHeldTool(wrapper, name, args = {}) {
  return wrapper.request('tools/call', { name, arguments: args }, 31_000);
}

async function initializeWrapper(wrapper, clientName) {
  const initialized = await wrapper.request('initialize', {
    protocolVersion: '2024-11-05',
    capabilities: {},
    clientInfo: { name: clientName, version: '0' },
  });
  wrapper.notify('notifications/initialized');
  return initialized;
}

async function initializeWorkspace(wrapper, clientName) {
  await initializeWrapper(wrapper, clientName);
  // The initialize shim can answer before native prepare (25s) and MCP startup.
  // Observe that setup path explicitly before a case's 15s mutation deadline.
  const response = await wrapper.request('tools/call', {
    name: 'list_pages', arguments: {},
  }, WORKSPACE_RECOVERY_OBSERVATION_MS);
  assert.equal(response?.error, undefined, JSON.stringify(heldRequestResultDiagnostic(response)));
  assert.notEqual(response?.result?.isError, true, JSON.stringify(heldRequestResultDiagnostic(response)));
  assert.ok(response?.result?.structuredContent?.pages, JSON.stringify(heldRequestResultDiagnostic(response)));
  return response;
}

async function cleanupFixture(fixture, host, ...wrappers) {
  if (fixture.primaryErrors?.length) {
    console.error('[browser-host-fixture-diagnostic]', JSON.stringify(host.diagnostic()));
    try {
      const audit = readAudit(fixture.auditPath).slice(-40).map(({ kind, name, pid, at }) =>
        ({ kind, name, pid, at }));
      console.error('[browser-upstream-fixture-events]', JSON.stringify(audit));
    } catch {
      console.error('[browser-upstream-fixture-events]', JSON.stringify({ unavailable: true }));
    }
    try {
      const observed = await host.snapshot();
      console.error('[browser-host-fixture-events]', JSON.stringify({
        timeline: observed.operationTimeline.slice(-40),
        pendingObservations: observed.pendingObservations,
        prepareCalls: observed.prepareCalls,
      }));
    } catch {
      console.error('[browser-host-fixture-events]', JSON.stringify({ unavailable: true,
        progress: host.diagnostic() }));
    }
  }
  // Release only this fixture's finite barriers, even after an assertion.
  // The intentional never-release shutdown case is not registered here.
  for (const path of fixture.releaseGates) {
    try { writeFileSync(path, 'cleanup release'); }
    catch (error) { (fixture.primaryErrors ||= []).push(error); }
  }
  await cleanupBrowserFixture({
    primaryErrors: fixture.primaryErrors,
    closeHost: () => host.close(),
    stopChildren: [
      ...wrappers.filter(Boolean).map(wrapper => () => wrapper.stopConfirmed()),
      () => waitUntil(
        async () => readAudit(fixture.auditPath).filter(entry => entry.kind === 'start')
          .every(entry => confirmedProcessExited(entry.pid)),
        'owned fake MCP children did not exit; preserving fixture files',
      ),
    ],
    closeServer: () => new Promise((resolveClose, reject) => fixture.cdp.close(error => {
      if (error) reject(error); else resolveClose();
    })),
    removeFiles: () => {
      const target = resolve(fixture.root);
      assert.equal(dirname(target), resolve(tmpdir()), 'cleanup must stay within this fixture temp parent');
      assert.ok(basename(target).startsWith('pinvou-windows-lifecycle-'), 'cleanup must target the owned fixture');
      assert.equal(lstatSync(target).isSymbolicLink(), false, 'cleanup refuses a replaced fixture link');
      rmSync(target, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    },
  });
}

function processIsAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

test('Windows proxy: a deliberately killed failed startup returns to reusable shim state', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWrapper(wrapper, 'windows-startup-retry-test');
    writeFileSync(fixture.modePath, JSON.stringify({ handshakeError: true }));

    const failed = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    });
    assert.match(failed.error.message, /configured startup handshake rejection/);
    await waitUntil(
      async () => readAudit(fixture.auditPath).filter((entry) => entry.kind === 'start').length === 1,
      'the failed startup attempt did not launch',
    );
    await sleep(100);
    assert.equal(wrapper.child.exitCode, null, 'failed-attempt cleanup must not exit the shim');
    assert.equal(wrapper.child.signalCode, null, 'failed-attempt cleanup must not signal the shim');

    writeFileSync(fixture.modePath, '{}');
    const recovered = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    });
    assert.equal(recovered.result.structuredContent.pages.length, 1);
    assert.equal(
      readAudit(fixture.auditPath).filter((entry) => entry.kind === 'start').length,
      2,
      'the next distinct request must launch exactly one fresh MCP child',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: post-handshake setup failure retires child before shim retry', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWrapper(wrapper, 'windows-post-handshake-retry-test');
    writeFileSync(fixture.modePath, JSON.stringify({ toolsListError: true }));

    const failed = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    });
    assert.match(failed.error.message, /configured runtime catalog rejection/);
    const firstPid = readAudit(fixture.auditPath)
      .filter((entry) => entry.kind === 'start')
      .at(-1)?.pid;
    assert.ok(Number.isInteger(firstPid));
    await waitUntil(
      async () => !processIsAlive(firstPid),
      'post-handshake setup child survived after wrapper returned to shim',
    );
    assert.equal(wrapper.child.exitCode, null, 'retiring a setup child must preserve the shim');

    writeFileSync(fixture.modePath, '{}');
    const recovered = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    });
    assert.equal(recovered.result.structuredContent.pages.length, 1);
    assert.equal(
      readAudit(fixture.auditPath).filter((entry) => entry.kind === 'start').length,
      2,
      'retry must launch one fresh child after retiring the failed setup child',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: stdin shutdown cannot release a request buffered during startup', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWrapper(wrapper, 'windows-startup-shutdown-test');
    writeFileSync(fixture.modePath, JSON.stringify({ handshakeDelayMs: 500 }));

    const buffered = wrapper.startRequest('tools/call', {
      name: 'click',
      arguments: { uid: 'must-not-run-after-stdin-close' },
    });
    wrapper.abandonRequest(buffered.id);
    await waitUntil(
      async () => readAudit(fixture.auditPath).some((entry) => entry.kind === 'start'),
      'the startup child was never launched',
    );
    wrapper.child.stdin.end();
    if (wrapper.child.exitCode == null && wrapper.child.signalCode == null) {
      await Promise.race([
        new Promise((resolve) => wrapper.child.once('exit', resolve)),
        sleep(10_000).then(() => { throw new Error('startup shutdown did not exit promptly'); }),
      ]);
    }
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
      0,
      'a buffered page action must not dispatch after stdin has closed',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: missing filesystem notifications still service host requests', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture, { watchRequests: false });
  const wrapper = driveWrapper(fixture);
  try {
    const ready = await initializeWorkspace(wrapper, 'windows-host-poll-fallback-test');
    assert.equal(ready.result.structuredContent.pages.length, 1);
    const before = await host.snapshot();
    const response = await wrapper.request('tools/call', {
      name: 'click', arguments: { uid: 'fallback-target' },
    });
    assert.equal(response.error, undefined, JSON.stringify(heldRequestResultDiagnostic(response)));
    assert.notEqual(response.result?.isError, true);
    const after = await host.snapshot();
    assert.ok((after.operationCalls.get('begin_agent_operation') || 0)
      > (before.operationCalls.get('begin_agent_operation') || 0));
    assert.equal(readAudit(fixture.auditPath)
      .filter(entry => entry.kind === 'tool' && entry.name === 'click').length, 1);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: committed close with unusable host acknowledgement is non-retryable', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-close-ack-test');

    const workspace = JSON.parse(readFileSync(fixture.workspacePath, 'utf8'));
    atomicWriteJson(fixture.workspacePath, {
      ...workspace,
      revision: workspace.revision + 1,
      tabs: [
        ...workspace.tabs,
        { token: 'aaaaaaaaaaaaaaaa', target_id: 'target-a-22' },
      ],
    });
    const pages = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    });
    assert.deepEqual(
      pages.result.structuredContent.pages.map((page) => page.id),
      [1, 22],
    );

    await host.malformResponse('close_tab', true);
    const beforeClose = (await host.snapshot()).operationCalls.get('close_tab') || 0;
    const closed = await wrapper.request('tools/call', {
      name: 'close_page',
      arguments: { pageId: 1 },
    });
    assert.equal(
      closed.result.structuredContent.errorCode,
      'browser/action-commit-unknown-after-host-acknowledgement-loss',
    );
    assert.equal(closed.result.structuredContent.actionCommitState, 'unknown');
    assert.equal(closed.result.structuredContent.retryable, false);
    assert.equal(
      ((await host.snapshot()).operationCalls.get('close_tab') || 0) - beforeClose,
      1,
      'the logical close must be dispatched exactly once',
    );
    const committedWorkspace = JSON.parse(readFileSync(fixture.workspacePath, 'utf8'));
    assert.deepEqual(committedWorkspace.tabs.map((tab) => tab.token), ['aaaaaaaaaaaaaaaa']);

    const recovered = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    });
    assert.deepEqual(
      recovered.result.structuredContent.pages.map((page) => page.id),
      [22],
      'the wrapper remains usable and discovers the already-committed close',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: committed close stays non-retryable when post-commit page sync fails', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-close-post-sync-test');

    const workspace = JSON.parse(readFileSync(fixture.workspacePath, 'utf8'));
    atomicWriteJson(fixture.workspacePath, {
      ...workspace,
      revision: workspace.revision + 1,
      tabs: [
        ...workspace.tabs,
        { token: 'aaaaaaaaaaaaaaaa', target_id: 'target-a-22' },
      ],
    });
    await wrapper.request('tools/call', { name: 'list_pages', arguments: {} });
    writeFileSync(fixture.modePath, JSON.stringify({ listPagesErrorWhenSingle: true }));

    const beforeClose = (await host.snapshot()).operationCalls.get('close_tab') || 0;
    const closed = await wrapper.request('tools/call', {
      name: 'close_page',
      arguments: { pageId: 1 },
    });
    assert.equal(
      closed.result.structuredContent.errorCode,
      'browser/action-committed-but-post-sync-failed',
    );
    assert.equal(closed.result.structuredContent.actionCommitState, 'committed');
    assert.equal(closed.result.structuredContent.retryable, false);
    assert.equal(
      ((await host.snapshot()).operationCalls.get('close_tab') || 0) - beforeClose,
      1,
      'post-commit sync failure must not redispatch the logical close',
    );
    assert.deepEqual(
      JSON.parse(readFileSync(fixture.workspacePath, 'utf8')).tabs.map((tab) => tab.token),
      ['aaaaaaaaaaaaaaaa'],
      'the close was committed before the synthetic list failure',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: native tab-close uncertainty is structured and non-retryable', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-close-native-unknown-test');
    const workspace = JSON.parse(readFileSync(fixture.workspacePath, 'utf8'));
    atomicWriteJson(fixture.workspacePath, {
      ...workspace,
      revision: workspace.revision + 1,
      tabs: [
        ...workspace.tabs,
        { token: 'aaaaaaaaaaaaaaaa', target_id: 'target-a-22' },
      ],
    });
    await wrapper.request('tools/call', { name: 'list_pages', arguments: {} });
    await host.setOperationError(
      'close_tab',
      'browser/action-commit-unknown-after-tab-close: native close acknowledgement lost',
    );

    const beforeClose = (await host.snapshot()).operationCalls.get('close_tab') || 0;
    const closed = await wrapper.request('tools/call', {
      name: 'close_page',
      arguments: { pageId: 1 },
    });
    assert.equal(
      closed.result.structuredContent.errorCode,
      'browser/action-commit-unknown-after-tab-close',
    );
    assert.equal(closed.result.structuredContent.actionCommitState, 'unknown');
    assert.equal(closed.result.structuredContent.retryable, false);
    assert.equal(((await host.snapshot()).operationCalls.get('close_tab') || 0) - beforeClose, 1);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: settled mutation errors are commit-unknown and never redispatched', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-upstream-mutation-error-test');

    const assertMutationUnknown = async ({ name, arguments: args, mode, upstreamError }) => {
      const beforeDispatches = readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === name).length;
      writeFileSync(fixture.modePath, JSON.stringify(mode));
      const response = await wrapper.request('tools/call', { name, arguments: args });
      assert.equal(response.error, undefined);
      assert.equal(response.result.isError, true);
      assert.equal(
        response.result.structuredContent.errorCode,
        'browser/action-commit-unknown-after-upstream-error',
      );
      assert.equal(response.result.structuredContent.outcome, 'unknown');
      assert.equal(response.result.structuredContent.actionCommitState, 'unknown');
      assert.equal(response.result.structuredContent.actionMayHaveCommitted, true);
      assert.equal(response.result.structuredContent.retryable, false);
      assert.equal(response.result.structuredContent.upstreamError, upstreamError);
      assert.equal(
        readAudit(fixture.auditPath)
          .filter((entry) => entry.kind === 'tool' && entry.name === name).length -
          beforeDispatches,
        1,
        `${name} must execute exactly once when its acknowledgement is an error`,
      );
    };

    const beforeLifecycleHostState = await host.snapshot();
    const beforeLifecycle = {
      begin: beforeLifecycleHostState.operationCalls.get('begin_agent_operation') || 0,
      end: beforeLifecycleHostState.operationCalls.get('end_agent_operation') || 0,
    };
    await assertMutationUnknown({
      name: 'click',
      arguments: { uid: 'mutation-json-error' },
      mode: { clickError: 'click committed before JSON-RPC error' },
      upstreamError: 'click committed before JSON-RPC error',
    });
    await assertMutationUnknown({
      name: 'type_text',
      arguments: { text: 'committed text' },
      mode: { typeTextToolError: 'type committed before tool error' },
      upstreamError: 'type committed before tool error',
    });
    await assertMutationUnknown({
      name: 'navigate_page',
      arguments: { type: 'url', url: 'https://mutation-error.example/' },
      mode: { navigateError: 'navigation committed before JSON-RPC error' },
      upstreamError: 'navigation committed before JSON-RPC error',
    });
    await assertMutationUnknown({
      name: 'evaluate_script',
      arguments: { function: '() => { globalThis.__committed = true; }' },
      mode: { evaluateToolError: 'script committed before tool error' },
      upstreamError: 'script committed before tool error',
    });

    // A tool classified as read-only keeps the official MCP result verbatim;
    // only potentially mutating dispatches need commit-unknown conversion.
    const beforeSnapshots = readAudit(fixture.auditPath)
      .filter((entry) => entry.kind === 'tool' && entry.name === 'take_snapshot').length;
    writeFileSync(fixture.modePath, JSON.stringify({
      snapshotToolError: 'snapshot read failed',
    }));
    const snapshot = await wrapper.request('tools/call', {
      name: 'take_snapshot',
      arguments: {},
    });
    assert.equal(snapshot.result.isError, true);
    assert.equal(snapshot.result.content[0].text, 'snapshot read failed');
    assert.equal(snapshot.result.structuredContent, undefined);
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'take_snapshot').length -
        beforeSnapshots,
      1,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - beforeLifecycle.begin,
      5,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - beforeLifecycle.end,
      5,
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: host tab-navigation uncertainty is never compensated into a retryable create', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-create-navigation-unknown-test');
    await host.setOperationError(
      'create_tab',
      'browser/action-commit-unknown-after-tab-navigation: final CAS acknowledgement lost',
    );

    const created = await wrapper.request('tools/call', {
      name: 'new_page',
      arguments: { url: 'https://create.example/' },
    });
    assert.equal(
      created.result.structuredContent.errorCode,
      'browser/action-commit-unknown-after-tab-navigation',
    );
    assert.equal(created.result.structuredContent.actionCommitState, 'unknown');
    assert.equal(created.result.structuredContent.retryable, false);
    assert.equal((await host.snapshot()).operationCalls.get('create_tab'), 1);
    assert.equal(
      (await host.snapshot()).operationCalls.get('rollback_created_tab') || 0,
      0,
      'network/script effects cannot be proven undone by closing a staging tab',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: bootstrap navigation remains committed when its post-sync fails', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    writeFileSync(fixture.modePath, JSON.stringify({
      bootstrapBlank: true,
      listPagesErrorAfterNavigate: true,
    }));
    await initializeWrapper(wrapper, 'windows-bootstrap-navigation-commit-test');

    const created = await wrapper.request('tools/call', {
      name: 'new_page',
      arguments: { url: 'https://reused.example/' },
    });
    assert.equal(
      created.result.structuredContent.errorCode,
      'browser/action-committed-but-post-sync-failed',
    );
    assert.equal(created.result.structuredContent.actionCommitState, 'committed');
    assert.equal(created.result.structuredContent.retryable, false);
    assert.equal((await host.snapshot()).operationCalls.get('create_tab') || 0, 0);
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'navigate_page').length,
      1,
      'the bootstrap navigation must be dispatched exactly once',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: external close cancellation reaches the host tombstone before commit', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-close-cancel-test');

    const workspace = JSON.parse(readFileSync(fixture.workspacePath, 'utf8'));
    atomicWriteJson(fixture.workspacePath, {
      ...workspace,
      revision: workspace.revision + 1,
      tabs: [
        ...workspace.tabs,
        { token: 'aaaaaaaaaaaaaaaa', target_id: 'target-a-22' },
      ],
    });
    await wrapper.request('tools/call', { name: 'list_pages', arguments: {} });

    await host.deferOperation('close_tab', true);
    const endBefore = (await host.snapshot()).operationCalls.get('end_agent_operation') || 0;
    const closing = wrapper.startRequest('tools/call', {
      name: 'close_page',
      arguments: { pageId: 1 },
    });
    const requestDirectory = join(fixture.root, 'host-requests');
    await waitUntil(async () => readdirSync(requestDirectory).some((name) => {
      if (!name.endsWith('.json')) return false;
      try {
        return JSON.parse(readFileSync(join(requestDirectory, name), 'utf8')).operation ===
          'close_tab';
      } catch {
        return false;
      }
    }), 'close_tab host request was not published');

    wrapper.abandonRequest(closing.id);
    wrapper.notify('notifications/cancelled', {
      requestId: closing.id,
      reason: 'cancel close before native commit',
    });
    await waitUntil(
      async () => readdirSync(requestDirectory).some((name) => name.endsWith('.cancelled')),
      'external cancellation did not publish a durable host tombstone',
    );
    await waitUntil(
      async () => ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) === endBefore + 1,
      'cancelled close did not settle end_agent_operation',
    );
    assert.equal((await host.snapshot()).operationCalls.get('close_tab') || 0, 0);
    assert.equal(
      JSON.parse(readFileSync(fixture.workspacePath, 'utf8')).tabs.length,
      2,
      'a host mutation cancelled before claim must not close a tab',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: external cancellation tombstones an in-flight tab activation', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-activation-cancel-test');

    await host.deferOperation('activate_tab', true);
    const operationBeginBefore = (await host.snapshot()).operationCalls.get('begin_agent_operation') || 0;
    const snapshot = wrapper.startRequest('tools/call', {
      name: 'take_snapshot',
      arguments: {},
    });
    const requestDirectory = join(fixture.root, 'host-requests');
    await waitUntil(async () => readdirSync(requestDirectory).some((name) => {
      if (!name.endsWith('.json')) return false;
      try {
        return JSON.parse(readFileSync(join(requestDirectory, name), 'utf8')).operation ===
          'activate_tab';
      } catch {
        return false;
      }
    }), 'activate_tab host request was not published');

    wrapper.abandonRequest(snapshot.id);
    wrapper.notify('notifications/cancelled', {
      requestId: snapshot.id,
      reason: 'cancel activation before host acknowledgement',
    });
    await waitUntil(
      async () => readdirSync(requestDirectory).some((name) => name.endsWith('.cancelled')),
      'external cancellation did not tombstone the in-flight activation',
    );
    assert.equal(
      (await host.snapshot()).operationCalls.get('begin_agent_operation') || 0,
      operationBeginBefore,
      'a cancelled activation must not advance into an executable operation',
    );
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'take_snapshot').length,
      0,
      'a cancelled activation must not dispatch the upstream page tool',
    );
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: stdin shutdown tombstones an in-flight close before exit', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-close-stdin-shutdown-test');
    const workspace = JSON.parse(readFileSync(fixture.workspacePath, 'utf8'));
    atomicWriteJson(fixture.workspacePath, {
      ...workspace,
      revision: workspace.revision + 1,
      tabs: [
        ...workspace.tabs,
        { token: 'aaaaaaaaaaaaaaaa', target_id: 'target-a-22' },
      ],
    });
    await wrapper.request('tools/call', { name: 'list_pages', arguments: {} });

    await host.deferOperation('close_tab', true);
    const endBefore = (await host.snapshot()).operationCalls.get('end_agent_operation') || 0;
    const closing = wrapper.startRequest('tools/call', {
      name: 'close_page',
      arguments: { pageId: 1 },
    });
    const requestDirectory = join(fixture.root, 'host-requests');
    await waitUntil(async () => readdirSync(requestDirectory).some((name) => {
      if (!name.endsWith('.json')) return false;
      try {
        return JSON.parse(readFileSync(join(requestDirectory, name), 'utf8')).operation ===
          'close_tab';
      } catch {
        return false;
      }
    }), 'close_tab host request was not published before stdin shutdown');

    wrapper.abandonRequest(closing.id);
    wrapper.child.stdin.end();
    await waitUntil(
      async () => readdirSync(requestDirectory).some((name) => name.endsWith('.cancelled')),
      'stdin shutdown did not publish a durable close tombstone',
    );
    await waitUntil(
      async () => ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) === endBefore + 1,
      'stdin shutdown did not settle end_agent_operation',
    );
    if (wrapper.child.exitCode == null && wrapper.child.signalCode == null) {
      await Promise.race([
        new Promise((resolve) => wrapper.child.once('exit', resolve)),
        sleep(10_000).then(() => { throw new Error('Windows wrapper did not exit promptly'); }),
      ]);
    }
    assert.equal((await host.snapshot()).operationCalls.get('close_tab') || 0, 0);
    assert.equal(JSON.parse(readFileSync(fixture.workspacePath, 'utf8')).tabs.length, 2);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: first workspace setup may outlive the mutation observer without dispatching a mutation', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);
  let settled = false;
  let pending;
  try {
    await host.deferOperation('prepare', true);
    pending = initializeWorkspace(wrapper, 'windows-first-workspace-setup-test').then(
      response => { settled = true; return { response }; },
      error => { settled = true; return { error }; },
    );
    const requests = join(fixture.root, 'host-requests');
    await waitUntil(() => existsSync(requests) && readdirSync(requests).some(name => {
      if (!name.endsWith('.json')) return false;
      const raw = readPendingHostRequest(join(requests, name));
      return raw !== null && JSON.parse(raw).operation === 'prepare';
    }), 'initial setup did not publish prepare');
    await sleep(16_000);
    assert.equal(settled, false, 'setup must not inherit the shorter mutation observer');
    await host.deferOperation('prepare', false);
    const outcome = await pending;
    assert.ifError(outcome.error);
    assert.equal(outcome.response.result.structuredContent.pages[0].id, 1);
    assert.equal((await host.snapshot()).operationCalls.get('create_tab') || 0, 0);
    const diagnostic = host.diagnostic();
    assert.deepEqual(Object.keys(diagnostic).sort(), ['ageMs', 'exited', 'inputRefreshReplies', 'phase', 'sequence']);
    assert.equal(diagnostic.inputRefreshReplies, 0, 'read-only setup must not invent input renewals');
    assert.ok(Number.isSafeInteger(diagnostic.sequence) && diagnostic.sequence > 0);
    assert.ok(Number.isSafeInteger(diagnostic.ageMs) && diagnostic.ageMs >= 0);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    try { await host.deferOperation('prepare', false); }
    catch (error) { (fixture.primaryErrors ||= []).push(error); }
    await cleanupFixture(fixture, host, wrapper);
    await pending;
  }
});

test('Windows proxy: workspace recovery observes a prepare response beyond the driver default', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);
  let settled = false;
  try {
    await initializeWorkspace(wrapper, 'windows-delayed-prepare-test');
    host.stopSessionA();
    const before = (await host.snapshot()).prepareCalls;
    await host.deferOperation('prepare', true);
    const pending = wrapper.request('tools/call', {
      name: 'list_pages', arguments: {},
    }, WORKSPACE_RECOVERY_OBSERVATION_MS).then(
      response => { settled = true; return { response }; },
      error => { settled = true; return { error }; },
    );
    const requestDirectory = join(fixture.root, 'host-requests');
    await waitUntil(async () => readdirSync(requestDirectory).some(name => {
      if (!name.endsWith('.json')) return false;
      const raw = readPendingHostRequest(join(requestDirectory, name));
      return raw !== null && JSON.parse(raw).operation === 'prepare';
    }), 'workspace recovery did not publish its deferred prepare request');
    // This remains below the real prepare deadline (25s), but above the
    // driver's default (15s). There is no active input-authority heartbeat.
    try {
      await sleep(16_000);
      assert.equal(settled, false, 'the observer must not terminate a valid in-flight prepare');
    } finally {
      await host.deferOperation('prepare', false);
    }
    const outcome = await pending;
    assert.ifError(outcome.error);
    assert.equal(outcome.response?.error, undefined, JSON.stringify(heldRequestResultDiagnostic(outcome.response)));
    assert.notEqual(outcome.response?.result?.isError, true, JSON.stringify(heldRequestResultDiagnostic(outcome.response)));
    assert.ok(outcome.response?.result?.structuredContent?.pages, JSON.stringify(heldRequestResultDiagnostic(outcome.response)));
    assert.equal(outcome.response.result.structuredContent.pages[0].id, 2);
    assert.equal((await host.snapshot()).prepareCalls - before, 1);
    assert.equal(readAudit(fixture.auditPath).filter(entry => entry.kind === 'start').length, 1);
    assert.equal(existsSync(fixture.otherWorkspacePath), true);
    assert.equal(fixture.cdp.listening, true);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    try { await host.deferOperation('prepare', false); }
    catch (error) { (fixture.primaryErrors ||= []).push(error); }
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: held response observation exceeds fifteen seconds without an input lease', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);
  try {
    await initializeWorkspace(wrapper, 'windows-held-observer-test');
    writeFileSync(fixture.modePath, JSON.stringify({ snapshotDelayMs: 15_500 }));
    const beforeHostState = await host.snapshot();
    const before = {
      begin: beforeHostState.operationCalls.get('begin_agent_operation') || 0,
      refresh: beforeHostState.operationCalls.get('refresh_agent_operation') || 0,
      input: beforeHostState.operationCalls.get('refresh_agent_input') || 0,
      end: beforeHostState.operationCalls.get('end_agent_operation') || 0,
    };
    const started = Date.now();
    const response = await requestHeldTool(wrapper, 'take_snapshot');
    assert.equal(response.result.content[0].text, 'snapshot');
    assert.ok(Date.now() - started >= 15_500, 'the real tool must outlive the default observer');
    assert.equal(((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - before.begin, 1);
    assert.ok(((await host.snapshot()).operationCalls.get('refresh_agent_operation') || 0) - before.refresh >= 1);
    assert.equal((await host.snapshot()).operationCalls.get('refresh_agent_input') || 0, before.input);
    assert.equal(((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - before.end, 1);
    const audit = readAudit(fixture.auditPath);
    assert.equal(audit.filter(entry => entry.kind === 'tool' && entry.name === 'take_snapshot').length, 1);
    assert.equal(audit.filter(entry => entry.kind === 'tool-complete' && entry.name === 'take_snapshot').length, 1);
    assert.equal(audit.filter(entry => entry.kind === 'start').length, 1);
    assert.equal(existsSync(fixture.otherWorkspacePath), true);
    assert.equal(fixture.cdp.listening, true);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: a paused observer cannot starve the independent host input refresh', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);
  const releasePath = ownedReleaseGate(fixture, 'paused-observer.release');
  try {
    await initializeWorkspace(wrapper, 'windows-paused-observer-test');
    writeFileSync(fixture.modePath, JSON.stringify({ clickReleasePath: releasePath }));
    const before = await host.snapshot();
    const request = requestHeldTool(wrapper, 'click', { uid: 'pause-observer' });
    // Keep an observer attached even if a barrier assertion fails first.
    request.catch(() => {});
    await waitUntil(async () => (await host.snapshot()).operationCalls.get('refresh_agent_input') >
      (before.operationCalls.get('refresh_agent_input') || 0), 'first real input refresh did not arrive');
    // A renewal interval includes the actual host-file round trip, not just
    // the 250ms timer. Keep the >=1.1s stall, but end the synchronous barrier
    // only after two real accepted replies instead of comparing wall clocks.
    let observerTicked = false;
    const pendingTick = setImmediate(() => { observerTicked = true; });
    try {
      const replies = host.waitForInputRefreshesWhileBlocked(2, { minBlockedMs: 1_100 });
      assert.equal(observerTicked, false, 'the coordinator must stay synchronously blocked');
      assert.ok(replies.blockedMs >= 1_100);
      assert.ok(replies.after - replies.before >= 2, 'host must really answer renewals while the observer is blocked');
      console.log('[browser-blocked-observer-witness]', JSON.stringify(replies));
    } finally { clearImmediate(pendingTick); }
    assert.equal(readAudit(fixture.auditPath).filter(entry => entry.kind === 'tool' && entry.name === 'click').length, 1);
    writeFileSync(releasePath, 'release');
    const response = await request;
    assert.equal(response.error, undefined);
    assert.equal(response.result.content[0].text, 'clicked');
    assert.equal(response.result.isError, undefined);
    assert.equal(readAudit(fixture.auditPath).filter(entry => entry.kind === 'cancel').length, 0,
      'committed success must not conceal a failed authorization refresh during the pause');
    const settled = await host.snapshot();
    assert.equal((settled.operationCalls.get('begin_agent_operation') || 0) - (before.operationCalls.get('begin_agent_operation') || 0), 1);
    assert.equal((settled.operationCalls.get('end_agent_operation') || 0) - (before.operationCalls.get('end_agent_operation') || 0), 1);
    assert.equal(readAudit(fixture.auditPath).filter(entry => entry.kind === 'tool' && entry.name === 'click').length, 1);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: a genuinely withheld host refresh still fails closed without redispatch', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);
  const releasePath = ownedReleaseGate(fixture, 'withheld-refresh.release');
  try {
    await initializeWorkspace(wrapper, 'windows-withheld-host-refresh-test');
    // ACK the host configuration before dispatch: this withholds real IPC
    // responses, unlike freezeObservations, which still answers every request.
    await host.deferOperation('refresh_agent_input', true);
    writeFileSync(fixture.modePath, JSON.stringify({ clickReleasePath: releasePath, clickError: 'synthetic failed acknowledgement after authorization loss' }));
    const before = await host.snapshot();
    const request = requestHeldTool(wrapper, 'click', { uid: 'withhold-host-refresh' });
    request.catch(() => {});
    assert.throws(() => host.waitForInputRefreshesWhileBlocked(1, { timeoutMs: 1_000 }),
      /blocked observer did not receive real input refresh replies/u,
      'idle worker progress must not count as a successful authorization reply');
    await waitUntil(() => readAudit(fixture.auditPath).some(entry => entry.kind === 'cancel'),
      'withheld refresh must cause real upstream cancellation');
    // The claimed upstream fails only after cancellation was observed. A
    // committed upstream success is intentionally authoritative in production.
    writeFileSync(releasePath, 'release');
    const response = await request;
    assert.equal(response.error, undefined);
    assert.equal(response.result.isError, true);
    const outcome = response.result.structuredContent;
    assert.equal(outcome.errorCode, 'browser/action-commit-unknown-after-authorization-loss');
    assert.equal(outcome.retryable, false);
    assert.equal(outcome.actionMayHaveCommitted, true);
    assert.equal(outcome.outcome, 'unknown');
    assert.equal(outcome.authorizationError, 'browser/host-request-timeout: refresh_agent_input exceeded 400ms');
    assert.equal(readAudit(fixture.auditPath).filter(entry => entry.kind === 'tool' && entry.name === 'click').length, 1);
    const settled = await host.snapshot();
    assert.equal(settled.operationCalls.get('refresh_agent_input') || 0, before.operationCalls.get('refresh_agent_input') || 0);
    assert.equal((settled.operationCalls.get('end_agent_operation') || 0) - (before.operationCalls.get('end_agent_operation') || 0), 1);
    await host.deferOperation('refresh_agent_input', false);
    writeFileSync(fixture.modePath, '{}');
    const next = await wrapper.request('tools/call', { name: 'list_pages', arguments: {} });
    assert.ok(next.result.structuredContent.pages.length > 0, 'a subsequent distinct read must remain usable');
    assert.equal(readAudit(fixture.auditPath).filter(entry => entry.kind === 'tool' && entry.name === 'click').length, 1);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: same wrapper recovers A once after stop while B keeps shared CDP alive', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  const releaseAfterRefreshes = async ({ request, before, operation, count, releasePath }) => {
    try {
      await Promise.race([
        waitUntil(
          async () => ((await host.snapshot()).operationCalls.get(operation) || 0) - before.refresh >= count,
          async () => 'heartbeat barrier timed out: ' + JSON.stringify({
            operation,
            expectedRefreshes: count,
            observedRefreshes: ((await host.snapshot()).operationCalls.get(operation) || 0) - before.refresh,
            hostError: (await host.snapshot()).failure?.message,
            operations: (await host.snapshot()).operationTimeline.slice(before.log),
            audit: readAudit(fixture.auditPath).slice(-20),
          }),
          // The wrapper shares the (often loaded) runner with this test and
          // the fake host; a starved wrapper pauses its heartbeat timer for
          // seconds before catching up in a burst. Both barrier users ride
          // this budget: the 250ms input heartbeat (count 4) and the 5s
          // generic operation heartbeat (count 1). 30s absorbs that stall
          // while staying a bounded negative control: a wrapper that stops
          // refreshing entirely still fails here, it just fails later.
          30_000,
        ),
        request.then((response) => assert.fail(
          'tool completed before its release barrier: ' +
          JSON.stringify({ ...heldRequestResultDiagnostic(response),
            authorization: heldRequestAuthorizationDiagnostic(response) }),
        )),
      ]);
      assert.equal(
        ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - before.end,
        0,
        'the operation must remain active while the tool waits for release',
      );
    } finally {
      writeFileSync(releasePath, 'release');
    }
    return await request;
  };

  try {
    const initialized = await initializeWrapper(wrapper, 'windows-lifecycle-test');
    assert.equal(initialized.result.serverInfo.name, 'fake-windows-mcp');

    const first = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    }, WORKSPACE_RECOVERY_OBSERVATION_MS);
    assert.equal(first.result.structuredContent.pages[0].id, 1);
    assert.equal((await host.snapshot()).prepareCalls, 1);
    assert.equal(readAudit(fixture.auditPath).filter((entry) => entry.kind === 'start').length, 1);

    // Closing A does not stop the app-owned CDP endpoint because B is still alive.
    host.stopSessionA();
    assert.equal(existsSync(fixture.otherWorkspacePath), true);
    assert.equal(fixture.cdp.listening, true);
    const recovered = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    }, WORKSPACE_RECOVERY_OBSERVATION_MS);
    assert.equal(recovered.result.structuredContent.pages[0].id, 2);
    assert.equal((await host.snapshot()).prepareCalls, 2, 'A is prepared exactly once after its workspace disappears');
    assert.equal(readAudit(fixture.auditPath).filter((entry) => entry.kind === 'start').length, 1, 'the MCP child remains the same process');

    // A failed recovery performs one prepare and one sync attempt, never an unbounded loop.
    host.stopSessionA();
    await host.setPreparePublishesWorkspace(false);
    const beforeFailedRecovery = (await host.snapshot()).prepareCalls;
    const failedRecovery = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    }, WORKSPACE_RECOVERY_OBSERVATION_MS);
    assert.match(failedRecovery.error.message, /^browser\/workspace-missing/);
    assert.equal((await host.snapshot()).prepareCalls - beforeFailedRecovery, 1);
    await sleep(100);
    assert.equal((await host.snapshot()).prepareCalls - beforeFailedRecovery, 1);

    // Failure keeps proxy routing enabled; the next distinct request may prepare once again.
    await host.setPreparePublishesWorkspace(true);
    const recoveredAgain = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    }, WORKSPACE_RECOVERY_OBSERVATION_MS);
    assert.equal(recoveredAgain.result.structuredContent.pages[0].id, 3);

    // Hold a >1.5s input tool until multiple real refreshes have arrived. Each
    // serial refresh also includes host-file round-trip time, so a fixed 1.6s
    // duration cannot guarantee four refreshes on a loaded Windows runner.
    // The bounded barrier must fail if the wrapper stops refreshing entirely.
    const clickReleasePath = ownedReleaseGate(fixture, 'long-input.release');
    writeFileSync(fixture.modePath, JSON.stringify({ clickDelayMs: 1_600, clickReleasePath }));
    const longInputBeforeHostState = await host.snapshot();
    const longInputBefore = {
      begin: longInputBeforeHostState.operationCalls.get('begin_agent_operation') || 0,
      refresh: longInputBeforeHostState.operationCalls.get('refresh_agent_input') || 0,
      end: longInputBeforeHostState.operationCalls.get('end_agent_operation') || 0,
      log: longInputBeforeHostState.operationLog.length,
      clicks: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
    };
    // 31s per-request ceiling sits just above the 30s heartbeat barrier:
    // the barrier, not the driver's 15s default, bounds a held request.
    const longInputRequest = requestHeldTool(wrapper, 'click', { uid: 'slow-button' });
    const longInput = await releaseAfterRefreshes({
      request: longInputRequest,
      before: longInputBefore,
      operation: 'refresh_agent_input',
      count: 4,
      releasePath: clickReleasePath,
    });
    assert.equal(longInput.result.content[0].text, 'clicked');
    assert.equal(
      ((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - longInputBefore.begin,
      1,
    );
    assert.ok(
      ((await host.snapshot()).operationCalls.get('refresh_agent_input') || 0) - longInputBefore.refresh >= 4,
      'a held input tool must receive multiple real heartbeat refreshes',
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - longInputBefore.end,
      1,
    );
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length -
        longInputBefore.clicks,
      1,
      'heartbeat must not redispatch the logical tool',
    );
    const longInputOperations = (await host.snapshot()).operationLog.slice(longInputBefore.log);
    assert.ok(
      longInputOperations.lastIndexOf('refresh_agent_input') <
        longInputOperations.lastIndexOf('end_agent_operation'),
      'the in-flight heartbeat must settle before end_agent_operation',
    );
    const refreshesAfterLongInput = (await host.snapshot()).operationCalls.get('refresh_agent_input') || 0;
    await sleep(600);
    assert.equal(
      (await host.snapshot()).operationCalls.get('refresh_agent_input') || 0,
      refreshesAfterLongInput,
      'no heartbeat may survive end_agent_operation',
    );

    // Slow non-input/DOM work uses the generic 5s operation heartbeat. It must
    // neither borrow the trusted-input suppression endpoint nor leave a timer
    // running after the host operation is ended.
    const snapshotReleasePath = ownedReleaseGate(fixture, 'slow-snapshot.release');
    writeFileSync(fixture.modePath, JSON.stringify({ snapshotDelayMs: 5_500, snapshotReleasePath }));
    const slowSnapshotBeforeHostState = await host.snapshot();
    const slowSnapshotBefore = {
      begin: slowSnapshotBeforeHostState.operationCalls.get('begin_agent_operation') || 0,
      refresh: slowSnapshotBeforeHostState.operationCalls.get('refresh_agent_operation') || 0,
      inputRefresh: slowSnapshotBeforeHostState.operationCalls.get('refresh_agent_input') || 0,
      end: slowSnapshotBeforeHostState.operationCalls.get('end_agent_operation') || 0,
      log: slowSnapshotBeforeHostState.operationLog.length,
    };
    // 31s per-request ceiling sits just above the 30s heartbeat barrier:
    // the barrier, not the driver's 15s default, bounds a held request.
    const slowSnapshotRequest = requestHeldTool(wrapper, 'take_snapshot');
    const slowSnapshot = await releaseAfterRefreshes({
      request: slowSnapshotRequest,
      before: slowSnapshotBefore,
      operation: 'refresh_agent_operation',
      count: 1,
      releasePath: snapshotReleasePath,
    });
    assert.equal(slowSnapshot.result.content[0].text, 'snapshot');
    assert.equal(
      ((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - slowSnapshotBefore.begin,
      1,
    );
    assert.ok(
      ((await host.snapshot()).operationCalls.get('refresh_agent_operation') || 0) -
        slowSnapshotBefore.refresh >= 1,
      'a slow non-input tool must refresh the generic operation lease',
    );
    assert.equal(
      (await host.snapshot()).operationCalls.get('refresh_agent_input') || 0,
      slowSnapshotBefore.inputRefresh,
      'a non-input tool must not extend trusted-input suppression',
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - slowSnapshotBefore.end,
      1,
    );
    const slowSnapshotOperations = (await host.snapshot()).operationLog.slice(slowSnapshotBefore.log);
    assert.ok(
      slowSnapshotOperations.lastIndexOf('refresh_agent_operation') <
        slowSnapshotOperations.lastIndexOf('end_agent_operation'),
      'the generic heartbeat must settle before end_agent_operation',
    );
    const genericRefreshesAfterSnapshot =
      (await host.snapshot()).operationCalls.get('refresh_agent_operation') || 0;
    await sleep(5_200);
    assert.equal(
      (await host.snapshot()).operationCalls.get('refresh_agent_operation') || 0,
      genericRefreshesAfterSnapshot,
      'no generic heartbeat may survive end_agent_operation',
    );
    writeFileSync(fixture.modePath, JSON.stringify({ clickDelayMs: 1_600 }));

    // Freeze observations until four real refreshes have been acknowledged,
    // then expose the backlog as a burst. Requiring 15.5s of live input
    // authority here couples the observer regression to dozens of unrelated
    // 400ms host round trips. The separate slow-snapshot test above covers
    // the historical 15s ceiling without an active native-input lease.
    const lateRecoveryReleasePath = ownedReleaseGate(fixture, 'late-recovery.release');
    writeFileSync(
      fixture.modePath,
      JSON.stringify({ clickDelayMs: 1_600, clickReleasePath: lateRecoveryReleasePath }),
    );
    const lateRecoveryBeforeHostState = await host.snapshot();
    const lateRecoveryBefore = {
      begin: lateRecoveryBeforeHostState.operationCalls.get('begin_agent_operation') || 0,
      refresh: lateRecoveryBeforeHostState.operationCalls.get('refresh_agent_input') || 0,
      end: lateRecoveryBeforeHostState.operationCalls.get('end_agent_operation') || 0,
      log: lateRecoveryBeforeHostState.operationLog.length,
      clicks: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
    };
    await host.freezeObservations('refresh_agent_input', true);
    const lateRecoveryRequest = requestHeldTool(wrapper, 'click', { uid: 'late-heartbeat-button' });
    await Promise.race([
      waitUntil(
        async () => (await host.snapshot()).pendingObservations
          .filter(operation => operation === 'refresh_agent_input').length >= 4,
        'four acknowledged input refreshes did not reach the frozen observation backlog',
        30_000,
      ),
      lateRecoveryRequest.then(response => assert.fail(
        'tool completed before its frozen observations were released: ' +
        JSON.stringify(heldRequestResultDiagnostic(response)),
      )),
    ]);
    assert.equal(
      ((await host.snapshot()).operationCalls.get('refresh_agent_input') || 0) - lateRecoveryBefore.refresh,
      0,
      'the fixture must really withhold every barrier-visible heartbeat during the stall',
    );
    // The real four-refresh backlog must cross the same release barrier.
    await host.freezeObservations('refresh_agent_input', false);
    const lateRecovery = await releaseAfterRefreshes({
      request: lateRecoveryRequest,
      before: lateRecoveryBefore,
      operation: 'refresh_agent_input',
      count: 4,
      releasePath: lateRecoveryReleasePath,
    });
    assert.equal(lateRecovery.result.content[0].text, 'clicked');
    assert.equal(
      ((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - lateRecoveryBefore.begin,
      1,
    );
    assert.ok(
      ((await host.snapshot()).operationCalls.get('refresh_agent_input') || 0) - lateRecoveryBefore.refresh >= 4,
      'the delayed heartbeat burst must still cross the four-refresh barrier',
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - lateRecoveryBefore.end,
      1,
    );
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length -
        lateRecoveryBefore.clicks,
      1,
      'a late heartbeat burst must not redispatch the logical tool',
    );
    const lateRecoveryOperations = (await host.snapshot()).operationLog.slice(lateRecoveryBefore.log);
    assert.ok(
      lateRecoveryOperations.lastIndexOf('refresh_agent_input') <
        lateRecoveryOperations.lastIndexOf('end_agent_operation'),
      'the delayed in-flight heartbeat must settle before end_agent_operation',
    );
    const refreshesAfterLateRecovery =
      (await host.snapshot()).operationCalls.get('refresh_agent_input') || 0;
    await sleep(600);
    assert.equal(
      (await host.snapshot()).operationCalls.get('refresh_agent_input') || 0,
      refreshesAfterLateRecovery,
      'no delayed heartbeat may survive end_agent_operation',
    );
    writeFileSync(fixture.modePath, JSON.stringify({ clickDelayMs: 1_600 }));

    // A rejected refresh (the host atomically observed takeover/lease loss)
    // cooperatively cancels the already claimed upstream request and waits for
    // its authoritative result. This fake upstream deliberately ignores the
    // cancellation and commits, so the committed success must win; the logical
    // click is never invoked a second time.
    await host.setOperationError(
      'refresh_agent_input',
      'browser/agent-input-refresh-rejected: user takeover',
    );
    const failedHeartbeatBeforeHostState = await host.snapshot();
    const failedHeartbeatBefore = {
      begin: failedHeartbeatBeforeHostState.operationCalls.get('begin_agent_operation') || 0,
      refresh: failedHeartbeatBeforeHostState.operationCalls.get('refresh_agent_input') || 0,
      end: failedHeartbeatBeforeHostState.operationCalls.get('end_agent_operation') || 0,
      clicks: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
      cancellations: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'cancel').length,
      completed: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool-complete' && entry.name === 'click').length,
      timeline: failedHeartbeatBeforeHostState.operationTimeline.length,
    };
    const failedHeartbeatRelease = ownedReleaseGate(fixture, 'rejected-heartbeat.release');
    writeFileSync(fixture.modePath, JSON.stringify({ clickReleasePath: failedHeartbeatRelease }));
    const failedHeartbeatRequest = requestHeldTool(wrapper, 'click', { uid: 'revoked-button' });
    failedHeartbeatRequest.catch(() => {});
    try {
      await waitUntil(
        async () => readAudit(fixture.auditPath).filter(entry => entry.kind === 'cancel').length ===
          failedHeartbeatBefore.cancellations + 1,
        'heartbeat failure did not cancel the claimed upstream request',
      );
      assert.equal(readAudit(fixture.auditPath)
        .filter(entry => entry.kind === 'tool-complete' && entry.name === 'click').length,
      failedHeartbeatBefore.completed, 'the upstream must stay held after cancellation');
      assert.equal((await host.snapshot()).operationCalls.get('end_agent_operation') || 0,
        failedHeartbeatBefore.end, 'host end must not precede the held upstream completion');
    } finally {
      writeFileSync(failedHeartbeatRelease, 'release');
    }
    const failedHeartbeat = await failedHeartbeatRequest;
    assert.equal(failedHeartbeat.result.content[0].text, 'clicked');
    assert.equal(
      ((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - failedHeartbeatBefore.begin,
      1,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('refresh_agent_input') || 0) - failedHeartbeatBefore.refresh,
      1,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - failedHeartbeatBefore.end,
      1,
    );
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length -
        failedHeartbeatBefore.clicks,
      1,
    );
    await waitUntil(
      async () => readAudit(fixture.auditPath).filter((entry) => entry.kind === 'cancel').length ===
        failedHeartbeatBefore.cancellations + 1,
      'heartbeat failure did not cancel the claimed upstream request',
    );
    const failedAudit = readAudit(fixture.auditPath);
    const completion = failedAudit
      .filter((entry) => entry.kind === 'tool-complete' && entry.name === 'click')
      .at(-1);
    const end = (await host.snapshot()).operationTimeline
      .slice(failedHeartbeatBefore.timeline)
      .filter((entry) => entry.operation === 'end_agent_operation')
      .at(-1);
    assert.equal(
      failedAudit.filter((entry) => entry.kind === 'tool-complete' && entry.name === 'click').length -
        failedHeartbeatBefore.completed,
      1,
    );
    assert.ok(completion && end && end.completedToolCounts.click >= failedHeartbeatBefore.completed + 1,
      'end must observe real upstream completion: ' + JSON.stringify({ completion, end }));
    const refreshesAfterFailure = (await host.snapshot()).operationCalls.get('refresh_agent_input') || 0;
    await sleep(600);
    assert.equal((await host.snapshot()).operationCalls.get('refresh_agent_input') || 0, refreshesAfterFailure);
    await host.clearOperationError('refresh_agent_input');
    writeFileSync(fixture.modePath, '{}');

    // An external MCP cancellation is also cooperative after begin. The
    // client abandons its response, but the wrapper must retain the internal
    // request until an upstream that ignores cancellation really completes;
    // only then may it end the Rust operation.
    const externalCancelRelease = ownedReleaseGate(fixture, 'external-cancel.release');
    writeFileSync(fixture.modePath, JSON.stringify({ clickReleasePath: externalCancelRelease }));
    const externalCancelBeforeHostState = await host.snapshot();
    const externalCancelBefore = {
      clicks: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
      completed: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool-complete' && entry.name === 'click').length,
      end: externalCancelBeforeHostState.operationCalls.get('end_agent_operation') || 0,
      timeline: externalCancelBeforeHostState.operationTimeline.length,
      cancellations: readAudit(fixture.auditPath).filter(entry => entry.kind === 'cancel').length,
    };
    const cancelled = wrapper.startRequest('tools/call', {
      name: 'click',
      arguments: { uid: 'externally-cancelled-button' },
    });
    await waitUntil(
      async () => readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length ===
        externalCancelBefore.clicks + 1,
      'external cancellation fixture did not dispatch its one click',
    );
    wrapper.abandonRequest(cancelled.id);
    wrapper.notify('notifications/cancelled', {
      requestId: cancelled.id,
      reason: 'user cancelled the browser tool',
    });
    try {
      await waitUntil(
        async () => readAudit(fixture.auditPath).filter(entry => entry.kind === 'cancel').length ===
          externalCancelBefore.cancellations + 1,
        'external cancellation did not reach the held upstream request',
      );
      assert.equal(readAudit(fixture.auditPath)
        .filter(entry => entry.kind === 'tool-complete' && entry.name === 'click').length,
      externalCancelBefore.completed, 'the cancelled upstream must remain held');
      assert.equal((await host.snapshot()).operationCalls.get('end_agent_operation') || 0,
        externalCancelBefore.end, 'host end must wait for real upstream completion');
    } finally {
      writeFileSync(externalCancelRelease, 'release');
    }
    await waitUntil(
      async () => readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool-complete' && entry.name === 'click').length ===
        externalCancelBefore.completed + 1 &&
        ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) === externalCancelBefore.end + 1,
      async () => JSON.stringify({
        message: 'cancelled upstream completion and host end did not both settle',
        expected: externalCancelBefore,
        audit: readAudit(fixture.auditPath).filter(entry =>
          ['tool', 'tool-complete', 'cancel'].includes(entry.kind)).slice(-12),
        host: (await host.snapshot()).operationTimeline.slice(-12),
        progress: host.diagnostic(),
      }),
      5_000,
    );
    const externalCancelAudit = readAudit(fixture.auditPath);
    const externalCompletion = externalCancelAudit
      .filter((entry) => entry.kind === 'tool-complete' && entry.name === 'click')
      .at(-1);
    const externalEnd = (await host.snapshot()).operationTimeline
      .slice(externalCancelBefore.timeline)
      .filter((entry) => entry.operation === 'end_agent_operation')
      .at(-1);
    assert.ok(
      externalCompletion && externalEnd
        && externalEnd.completedToolCounts.click >= externalCancelBefore.completed + 1,
      'external cancellation must observe upstream completion before host end: '
        + JSON.stringify({ completion: externalCompletion, end: externalEnd }),
    );
    writeFileSync(fixture.modePath, '{}');

    // The upstream click has committed before host cleanup begins. A failed
    // end transport must therefore keep the successful result and only emit a
    // diagnostic; returning a normal tool error could make the Agent replay it.
    await host.setOperationError('end_agent_operation', 'browser/end-cleanup-failed');
    const committedBeforeHostState = await host.snapshot();
    const committedBefore = {
      prepare: committedBeforeHostState.prepareCalls,
      begin: committedBeforeHostState.operationCalls.get('begin_agent_operation') || 0,
      end: committedBeforeHostState.operationCalls.get('end_agent_operation') || 0,
      clicks: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
    };
    const committedDespiteEndFailure = await wrapper.request('tools/call', {
      name: 'click',
      arguments: { uid: 'committed-before-end-failure' },
    });
    assert.equal(committedDespiteEndFailure.result.content[0].text, 'clicked');
    assert.equal((await host.snapshot()).prepareCalls, committedBefore.prepare);
    assert.equal(
      ((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - committedBefore.begin,
      1,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - committedBefore.end,
      1,
    );
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length -
        committedBefore.clicks,
      1,
      'end cleanup failure must not redispatch the committed click',
    );
    assert.match(
      wrapper.stderrText(),
      /end_agent_operation cleanup failed after committed tool result: browser\/end-cleanup-failed/,
    );
    await host.clearOperationError('end_agent_operation');

    const assertNoLifecycleRetry = async ({
      operation,
      error,
      configure,
      expectedClickDelta = 0,
      expectedMutationUnknown = false,
    }) => {
      const beforePrepare = (await host.snapshot()).prepareCalls;
      const beforeOperation = (await host.snapshot()).operationCalls.get(operation) || 0;
      const beforeClicks = readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length;
      await configure();
      const response = await wrapper.request('tools/call', {
        name: 'click',
        arguments: { uid: 'button-1' },
      });
      if (expectedMutationUnknown) {
        assert.equal(response.error, undefined);
        assert.equal(
          response.result.structuredContent.errorCode,
          'browser/action-commit-unknown-after-upstream-error',
        );
        assert.equal(response.result.structuredContent.actionCommitState, 'unknown');
        assert.equal(response.result.structuredContent.retryable, false);
        assert.equal(response.result.structuredContent.upstreamError, error);
      } else {
        assert.equal(response.error.message, error);
      }
      assert.equal((await host.snapshot()).prepareCalls, beforePrepare, `${error} must not trigger prepare`);
      assert.equal(((await host.snapshot()).operationCalls.get(operation) || 0) - beforeOperation, 1);
      const afterClicks = readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length;
      assert.equal(afterClicks - beforeClicks, expectedClickDelta);
    };

    await assertNoLifecycleRetry({
      operation: 'activate_tab',
      error: 'browser/user-takeover',
      configure: () => host.setOperationError('activate_tab', 'browser/user-takeover'),
    });
    await host.clearOperationError('activate_tab');

    await assertNoLifecycleRetry({
      operation: 'begin_agent_operation',
      error: 'browser/native-surface-missing',
      configure: () => host.setOperationError('begin_agent_operation', 'browser/native-surface-missing'),
    });
    await host.clearOperationError('begin_agent_operation');

    await assertNoLifecycleRetry({
      operation: 'begin_agent_operation',
      error: 'browser/control-lease-lost',
      configure: () => writeFileSync(fixture.modePath, JSON.stringify({ clickError: 'browser/control-lease-lost' })),
      expectedClickDelta: 1,
      expectedMutationUnknown: true,
    });
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: upstream crash settles the claimed dispatch before wrapper exit', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);

  try {
    await initializeWorkspace(wrapper, 'windows-upstream-crash-test');

    writeFileSync(fixture.modePath, JSON.stringify({
      clickReleasePath: ownedReleaseGate(fixture, 'upstream-crash.release'),
    }));
    const beforeHostState = await host.snapshot();
    const before = {
      begin: beforeHostState.operationCalls.get('begin_agent_operation') || 0,
      end: beforeHostState.operationCalls.get('end_agent_operation') || 0,
      clicks: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
    };
    const inFlight = wrapper.startRequest('tools/call', {
      name: 'click',
      arguments: { uid: 'crash-during-dispatch' },
    });
    await waitUntil(
      async () => readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length ===
        before.clicks + 1,
      'crash fixture did not dispatch its click',
    );
    const upstreamPid = readAudit(fixture.auditPath)
      .filter((entry) => entry.kind === 'start')
      .at(-1)?.pid;
    assert.ok(Number.isInteger(upstreamPid), 'fake upstream pid is missing');
    process.kill(upstreamPid, 'SIGKILL');

    const outcome = await inFlight.response;
    assert.equal(
      outcome.result.structuredContent.errorCode,
      'browser/action-commit-unknown-after-upstream-interruption',
    );
    assert.equal(outcome.result.structuredContent.retryable, false);
    await waitUntil(
      async () => ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) === before.end + 1,
      'upstream crash did not end the claimed host operation',
      5_000,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('begin_agent_operation') || 0) - before.begin,
      1,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - before.end,
      1,
    );
    if (wrapper.child.exitCode == null && wrapper.child.signalCode == null) {
      await Promise.race([
        new Promise((resolve) => wrapper.child.once('exit', resolve)),
        sleep(10_000).then(() => { throw new Error('wrapper did not exit after upstream crash'); }),
      ]);
    }
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper);
  }
});

test('Windows proxy: cancelled uncooperative dispatch shuts down on the first deadline and rejects queued work', {
  skip: process.platform !== 'win32',
}, async () => {
  const fixture = await makeFixture();
  const host = await createFakeHost(fixture);
  const wrapper = driveWrapper(fixture);
  let replacement = null;
  // In this fixture the first tool call includes lazy startup. Allow for a
  // slow handshake followed by a slow post-bind catalog response. The
  // cancellation phase below has its own observer and timing assertion.
  const startupObservationMs = 45_000;

  try {
    await initializeWrapper(wrapper, 'windows-cancel-shutdown-test');
    const initial = await wrapper.request('tools/call', { name: 'list_pages', arguments: {} }, {
      timeoutMs: startupObservationMs,
      label: 'original wrapper first list_pages',
    });
    assert.equal(initial.result?.structuredContent?.pages?.length, 1);

    // Hold the claimed action until shutdown, even if observing its dispatch
    // is delayed. A wall-clock delay alone may expire before cancellation.
    writeFileSync(fixture.modePath, JSON.stringify({
      clickDelayMs: 30_000,
      clickReleasePath: join(fixture.root, 'cancel-shutdown-never-release'),
    }));
    const beforeHostState = await host.snapshot();
    const before = {
      end: beforeHostState.operationCalls.get('end_agent_operation') || 0,
      clicks: readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length,
    };
    const inFlight = wrapper.startRequest('tools/call', {
      name: 'click',
      arguments: { uid: 'cancel-shutdown-button' },
    });
    await waitUntil(
      async () => readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'click').length ===
        before.clicks + 1,
      'cancel shutdown fixture did not dispatch its click',
    );
    const listsBeforeQueued = readAudit(fixture.auditPath)
      .filter((entry) => entry.kind === 'tool' && entry.name === 'list_pages').length;

    wrapper.abandonRequest(inFlight.id);
    const cancelledAt = Date.now();
    wrapper.notify('notifications/cancelled', {
      requestId: inFlight.id,
      reason: 'cancel uncooperative dispatch',
    });
    const queuedOutcome = await wrapper.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    }, {
      label: 'queued list_pages awaiting shutdown rejection',
    });
    const rejectedAt = Date.now();
    assert.match(queuedOutcome.error?.message || '', /browser\/wrapper-shutting-down/,
      JSON.stringify(heldRequestResultDiagnostic(queuedOutcome)));

    if (wrapper.child.exitCode == null && wrapper.child.signalCode == null) {
      await Promise.race([
        new Promise((resolve) => wrapper.child.once('exit', resolve)),
        sleep(10_000).then(() => { throw new Error('wrapper did not exit after rejecting queued work'); }),
      ]);
    }
    const shutdownElapsedMs = rejectedAt - cancelledAt;
    // Reject delivery that takes a full additional 5s grace window even if
    // the response arrives within the driver's 15s observation budget.
    assert.ok(
      shutdownElapsedMs >= 4_500 && shutdownElapsedMs < 9_500,
      `cooperative cancel shutdown did not reject queued work on its single first deadline: ${shutdownElapsedMs}ms`,
    );
    assert.equal(
      ((await host.snapshot()).operationCalls.get('end_agent_operation') || 0) - before.end,
      1,
      'cancelled claimed dispatch must end its exact host operation once',
    );
    assert.equal(
      readAudit(fixture.auditPath)
        .filter((entry) => entry.kind === 'tool' && entry.name === 'list_pages').length,
      listsBeforeQueued,
      'queued work must not reach the stopped upstream child',
    );
    assert.ok(
      readAudit(fixture.auditPath).some((entry) =>
        entry.kind === 'cancel' && entry.reason === 'cancel uncooperative dispatch'
      ),
      'the upstream cancellation notification was not published',
    );

    writeFileSync(fixture.modePath, '{}');
    replacement = driveWrapper(fixture);
    await initializeWrapper(replacement, 'windows-cancel-replacement-test');
    const recovered = await replacement.request('tools/call', {
      name: 'list_pages',
      arguments: {},
    }, {
      timeoutMs: startupObservationMs,
      label: 'replacement wrapper first list_pages',
    });
    assert.equal(recovered.result.structuredContent.pages.length, 1);
  } catch (error) {
    fixture.primaryErrors = [error];
    throw error;
  } finally {
    await cleanupFixture(fixture, host, wrapper, replacement);
  }
});
