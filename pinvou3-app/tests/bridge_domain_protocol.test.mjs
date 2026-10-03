import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';
import { desktopBridgeApi } from './bridge_domain_contract.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const bridgeRoot = path.join(root, 'src', 'platform', 'tauri');
const appMainSource = fs.readFileSync(path.join(root, 'src', 'app', 'main.jsx'), 'utf8');

function read(relativePath) {
  return fs.readFileSync(path.join(bridgeRoot, relativePath), 'utf8');
}

function extractCalls(source, callee, normalize = true) {
  const calls = [];
  const needle = `${callee}(`;
  let cursor = 0;
  while ((cursor = source.indexOf(needle, cursor)) !== -1) {
    const previous = source[cursor - 1] || '';
    if (/[A-Za-z0-9_$]/.test(previous)) {
      cursor += needle.length;
      continue;
    }
    let index = cursor + needle.length;
    let depth = 1;
    let quote = null;
    let escaped = false;
    let lineComment = false;
    let blockComment = false;
    for (; index < source.length && depth > 0; index += 1) {
      const char = source[index];
      const next = source[index + 1];
      if (lineComment) {
        if (char === '\n') lineComment = false;
        continue;
      }
      if (blockComment) {
        if (char === '*' && next === '/') { blockComment = false; index += 1; }
        continue;
      }
      if (quote) {
        if (escaped) escaped = false;
        else if (char === '\\') escaped = true;
        else if (char === quote) quote = null;
        continue;
      }
      if (char === '/' && next === '/') { lineComment = true; index += 1; continue; }
      if (char === '/' && next === '*') { blockComment = true; index += 1; continue; }
      if (char === '"' || char === "'" || char === '`') { quote = char; continue; }
      if (char === '(') depth += 1;
      else if (char === ')') depth -= 1;
    }
    assert.equal(depth, 0, `unclosed ${callee} call near offset ${cursor}`);
    const call = source.slice(cursor, index);
    calls.push(normalize ? call.replace(/\s+/g, ' ').trim() : call);
    cursor = index;
  }
  return calls;
}

const protocolSources = {
  orchestration: ['bridge.js'],
  artifacts: ['bridge/artifact-tracker.js', 'bridge/artifacts.js'],
  chat: ['bridge/chat.js', 'bridge/chat-events.js', 'bridge/terminal.js'],
  dependencies: ['bridge/dependencies.js'],
  interaction: ['bridge/interaction.js'],
  knowledge: ['bridge/knowledge-model.js'],
  memory: ['bridge/memory.js'],
  monitor: ['bridge/monitor.js'],
  personas: ['bridge/personas.js'],
  remoteControl: ['bridge/remote-control.js'],
  scheduled: ['bridge/scheduled.js'],
  sessions: ['bridge/sessions.js'],
  settings: ['bridge/settings.js'],
  updater: ['bridge/updater.js'],
  voice: ['bridge/voice.js'],
  multiAgent: ['bridge/multiagent.js'],
  teamRelay: ['bridge/team-relay.js'],
  projects: ['bridge/projects.js'],
  computerUse: ['bridge/computer_use.js'],
};

const expectedProtocolHashes = {
  multiAgent: '6896409bb406f1ba54ca496dd78c04549cc78ff5490ae771d9cf60586368a8a8',
  orchestration: '3dfae00b063d45f76e93c4fb8770c746c2b49e8a25a1e37f80767f08c7051724',
  // Recomputed for artifact export telemetry after a successful external open;
  // attribution is now isolated to the explicitly targeted session.
  // Recomputed again for the Linux clipboard-image paste fallback (community
  // #578 backflow): artifacts.js gains the addPasteImageFromClipboard invoke
  // wrapping the paste_clipboard_image command (the native layer reads the
  // WebKitGTK-invisible clipboard image and saves it server-side; web lane
  // intentionally has no such backend).
  artifacts: 'bb1974a6cff526075dc98813ee75dd7253ec970203ef59cb133d2c443ec5ed71',
// New domain: computer-use consent commands/events (desktop-only; the web
  // RPC allowlist excludes them, same policy as browser:*).
  // Recomputed when the 30s deny-suppression cooldown was removed: a deny now
  // only closes its own dialog and fresh requests re-prompt immediately (no
  // invoke/listen call-set change; comment wording is part of the digest).
  // Recomputed when the deny()/grant_required handler comments were updated
  // for the mainstream narrowing (no deny-cooldown memory, no idle-expired
  // grant re-arm; comment wording inside the listen callback span is part of
  // the digest). Recomputed again when the confirm_required handler's
  // type-preview comment was corrected to the shipped contract (full text
  // rides for every non-password Type action, short texts included).
  // Recomputed for the fresh-review fixes: the grant/confirm_required inert branches
  // now re-read authoritative status (other-window enable gap) and sameRequest compares
  // the typed-text preview (comment and expression wording inside the callback spans
  // is part of the digest; no invoke/listen call-set change).
  // Recomputed for the structured confirm payload: confirm_required builds the
  // request via buildConfirmRequest (action/button/click_count/point/text_preview/
  // text_preview_truncated passthrough + summary fallback) and dismissConfirm was
  // removed (no invoke/listen call-set change; listener body wording is part of
  // the digest).
  // Recomputed for the round-15 fix wave: secure-target chord masking (the
  // chord_masked_chars passthrough in buildConfirmRequest), the
  // background-session refresh guard, and the inert-branch optional chaining
  // inside the listener callback spans (no invoke/listen call-set change).
  computerUse: '9d0bad5bab784eb784a92b8df52ff7e83adef94cc0f6cb7bdbb99f913217e5b8',
  // Recomputed for #308 follow-ups: prefillComposer(text, append) recovery
  // entry + comment translations touching `invoke(` mentions (the extractor
  // scans raw source, so comment wording is part of the digest). Recomputed
  // again for the eighth-review fixes: zap resend gating on the in-flight
  // steer settlement, the per-sid zap entry guard, and the accompanying
  // comment wording. And again for the streaming-freeze follow-ups: the
  // settlement side table (a Promise on the queued chip poisoned the
  // subscription snapshot), steeredMidTurn bubble markers, and the hydration
  // envelope strip. And again for the r13 follow-ups: the transcript
  // fallback's legacy-chip pre-check (chat-events.js) and the zap
  // skip-resend settlement helper (chat.js). And again for the steer
  // persistence alignment: the steered-messages sidecar save/get invokes
  // (bridge.js) and the position-capture load_session (chat.js). Recomputed
  // again for the background-task indicator: the multiagent:agent_progress
  // listener schedules shell snapshot polling for sub-agent spawned tasks
  // (chat-events.js). Recomputed for explicit artifact presentation: a
  // successful present_artifact tool_end now emits a session-scoped request
  // that opens the preview even when the existing card is updated in place.
  // #148 preserves Official invocation shapes; the callback body now redacts errors.
  // Recomputed for the preinstalled-model startup removal on top of #148.
  // Recomputed for the CodeWhale v0.9.12 backflow: canonical `bash` name in the
  // shell background-task projection, compaction cancel phase, and the new
  // chat:tool_gate_decision audit listener in chat-events.js. Recomputed
  // again for the backflowed memory-footprint reduction (Community #442):
  // streaming-render trailing-edge timers, terminal toolMeta sweep, monitor
  // display-equality gate — listener body-internal edits only, no new invoke
  // or listen entries (capture-text refresh). Recomputed for per-frame stream
  // notification coalescing; event names and invoke shapes remain unchanged.
  chat: 'cad6c1cb471c97240673b9706fc6d799c81e64224f4fbaf56f5f44b3698cdc1e',
  dependencies: '257468e4f9e2e9270de6ef75f685d5eafcd000226d44cfb81a1b04c0e7615707',
  // Recomputed for #445: the YOLO gate threads an adjudicated session id
  // through exitPlanToYolo and planStuckGo.
  interaction: 'b62238d0404da126673ed75d74887bae4088861f46f893083c29412fa03d5636',
  knowledge: '96d4f6a1534f0aedf10714e7b945e0a29579b224ec98cbf27659237b5aac59b6',
  // memory recomputed for the backflowed memory-maintenance feature
  // (Community #419): organize_memory + get_memory_organize_history invokes
  // added to bridge/memory.js.
  memory: 'fc10a0538ac12c7b43b9d14d9846726f5847d1f21d578d1e918b9c68cdca35fd',
  monitor: '01bf9a7c9b9b3f313cf49e975e6503627ff373caed0f4b3be07a6a98492a7c43',
  personas: '75845688a405c5dc3590e176238969d846d61810eaeb3d632a0a741d30370239',
  remoteControl: '07b52395ae8c4af6547c9bbbe84d76399884fec0e64f3bcad31cd757b839b798',
  scheduled: '6492813eabb2472f892cb4b199a27b09d78355f56fc3aff24b8409af236799b6',
  // Recomputed for the combined session archive and #445 workspace binding
  // surfaces while retaining Official's materialization behavior.
  sessions: '24c9d103bfa90094b02200441222d387d86131836d0c7029d96c5ec28d071ddc',
  // Recomputed for the builtin feature toggle hook: settings.js gains the
  // list_builtin_features / set_builtin_feature_enabled invoke wrappers (the
  // session-mention feature gate reads the list; the setter is the contract
  // hook for the builtin plugin page).
  settings: 'b8a388fdf028c539a3e8cc86030dcf7ca0c575a223d142821875db5969c3e3d0',
  updater: 'd603bbac5c51f3b53bf19272fbafd9c430786fd5e35a9b8da9aa67f4c2334e5f',
  // Official's bridge/voice.js is untouched by this backflow, so the hash
  // stays at the Official base value (the community value differs only via
  // comment differences inside its own voice.js; no invoke/listen surface).
  voice: '8581b43940d101353fdd1468232dc778c2d1320854af5fdbbbc0326e5cdedbbb',
  // Official-only team-relay surface; unchanged by this backflow.
  teamRelay: '246f7cfd5a5ff4b730e2f3972fe9e74860b51a4781bd323bf10b3d795d24ed1d',
  // Recomputed for the rebind carryover feed-back (review #463 F-Major):
  // rebind_workspace_root gains the optional previousPostBusySessionIds
  // payload — the dialog's previous report fed back on retry, honored by the
  // backend only as a reporting reclassification inside its own to-lane
  // retry population. Same command surface, no new invoke or listen entries.
  projects: '90c12ab7494aba3975ac4ece4b23594f4a4fcf309f6252b721ce15575b245979',
};

for (const [domain, files] of Object.entries(protocolSources)) {
  const signatures = files.flatMap(file => {
    const source = read(file);
    return [
      ...extractCalls(source, 'invoke').map(call => `${file}:invoke:${call}`),
      ...extractCalls(source, 'listen').map(call => `${file}:listen:${call}`),
    ];
  });
  const hash = crypto.createHash('sha256').update(signatures.join('\n')).digest('hex');
  if (!expectedProtocolHashes[domain]) console.log(`${domain}: ${hash}`);
  else assert.equal(hash, expectedProtocolHashes[domain], `${domain} bridge protocol changed`);
}

const featureRegistry = new Proxy({}, {
  get() {
    return () => new Proxy({}, { get: () => function () {} });
  },
});
const windowObject = {
  __TAURI__: {
    core: { invoke: async () => null },
    event: { listen: async () => function () {} },
    dialog: { open: async () => null },
  },
  __PINVOU_TAURI_BRIDGE_FEATURES__: featureRegistry,
  location: { search: '' },
  performance: { now: () => 0 },
  setTimeout,
  clearTimeout,
};
const context = vm.createContext({
  window: windowObject,
  document: { readyState: 'loading', addEventListener() {} },
  console,
  setTimeout,
  clearTimeout,
  structuredClone,
  URL,
  Blob,
});
vm.runInContext(read('bridge.js'), context, { filename: 'bridge.js' });

const api = windowObject.TauriBridge;
assert.deepEqual(Object.keys(api).sort(), ['available', ...Object.keys(desktopBridgeApi)].sort());
for (const [domain, methods] of Object.entries(desktopBridgeApi)) {
  assert.deepEqual(Object.keys(api[domain]).sort(), methods.sort(), `${domain} API surface changed`);
}
assert.equal(api.sendMessage, undefined, 'flat compatibility facade must not return');
assert.equal(api.getState, undefined, 'flat state facade must not return');

function sourceFiles(directory) {
  return fs.readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
    const absolute = path.join(directory, entry.name);
    if (entry.isDirectory()) return sourceFiles(absolute);
    return /\.(?:js|jsx)$/.test(entry.name) ? [absolute] : [];
  });
}
for (const file of sourceFiles(path.join(root, 'src'))) {
  if (file.startsWith(bridgeRoot)) continue;
  const source = fs.readFileSync(file, 'utf8');
  assert.doesNotMatch(
    source,
    /\bbridge\.[A-Za-z_$][\w$]*\s*\(/,
    `${path.relative(root, file)} must not call the removed flat bridge facade`,
  );
  if (file.startsWith(path.join(root, 'src', 'features'))) {
    assert.doesNotMatch(
      source,
      /\b(?:window|globalThis)\s*\.\s*__TAURI__\b/,
      `${path.relative(root, file)} must use the platform Tauri client`,
    );
  }
  for (const match of source.matchAll(/\bbridge\.([A-Za-z_$][\w$]*)\.([A-Za-z_$][\w$]*)/g)) {
    const [, domain, method] = match;
    assert.equal(typeof api[domain]?.[method], 'function', `${path.relative(root, file)} uses unknown bridge API ${domain}.${method}`);
  }
}

const clientSource = read('client.js');
const client = await import(`data:text/javascript;base64,${Buffer.from(clientSource).toString('base64')}`);
const previousTauri = globalThis.__TAURI__;
const nativeCalls = [];
class PhysicalPosition {
  constructor(x, y) { this.x = x; this.y = y; }
}
const currentWindow = { label: 'main' };
globalThis.__TAURI__ = {
  core: { invoke: async (command, payload) => { nativeCalls.push(['invoke', command, payload]); return 'ok'; } },
  event: {
    listen: async (name, handler) => { nativeCalls.push(['listen', name, handler]); return () => {}; },
    emit: async (name, payload) => { nativeCalls.push(['emit', name, payload]); },
  },
  window: {
    getCurrentWindow: () => currentWindow,
    currentMonitor: async () => ({ name: 'primary' }),
    availableMonitors: async () => [{ name: 'primary' }],
    PhysicalPosition,
  },
};
try {
  assert.equal(client.isTauriAvailable(), true);
  assert.equal(await client.invokeTauri('protocol_probe', { value: 1 }), 'ok');
  await client.listenTauri('protocol:event', () => {});
  await client.emitTauri('protocol:emit', { value: 2 });
  assert.equal(client.getCurrentTauriWindow(), currentWindow);
  assert.deepEqual(await client.currentTauriMonitor(), { name: 'primary' });
  assert.deepEqual(await client.availableTauriMonitors(), [{ name: 'primary' }]);
  const position = client.createPhysicalPosition(10.6, -2.4);
  assert.equal(position.x, 11);
  assert.equal(position.y, -2);
  assert.deepEqual(nativeCalls.slice(0, 3).map(call => call.slice(0, 2)), [
    ['invoke', 'protocol_probe'],
    ['listen', 'protocol:event'],
    ['emit', 'protocol:emit'],
  ]);
} finally {
  if (previousTauri === undefined) delete globalThis.__TAURI__;
  else globalThis.__TAURI__ = previousTauri;
}
assert.doesNotMatch(
  read('bridge.js'),
  /repairPythonDependenciesAfterFirstFrame|repair_python_dependencies_after_first_frame/u,
  'marketplace startup repair must not be exposed through the knowledge bridge domain',
);
const firstFrameCall = extractCalls(appMainSource, 'window.requestAnimationFrame', false)
  .find(call => call.includes("mark('react:first_animation_frame')"));
assert.ok(firstFrameCall, 'first animation frame startup callback must exist');
const firstFrameBody = firstFrameCall.slice('window.requestAnimationFrame('.length, -1);
const secondFrameCall = extractCalls(firstFrameBody, 'window.requestAnimationFrame', false)
  .find(call => call.includes("mark('react:first_frame_presented')"));
assert.ok(secondFrameCall, 'second animation frame startup callback must exist');
const marketplaceCommands = fs.readFileSync(
  path.join(root, 'src-tauri', 'src', 'app', 'commands', 'marketplace.rs'),
  'utf8',
);
const uninstallStart = marketplaceCommands.indexOf('pub async fn uninstall_marketplace_tool(');
const uninstallEnd = marketplaceCommands.indexOf(
  'pub(super) fn uninstall_marketplace_tool_sync(',
  uninstallStart,
);
assert.ok(uninstallStart >= 0 && uninstallEnd > uninstallStart, 'marketplace uninstall command boundary must exist');
const uninstallCommand = marketplaceCommands.slice(uninstallStart, uninstallEnd);
assert.match(
  uninstallCommand,
  /tauri::async_runtime::spawn_blocking\s*\([\s\S]*uninstall_marketplace_tool_sync/u,
  'desktop marketplace uninstall must not wait for the synchronous tool lock on the async runtime',
);
console.log('bridge domain API and protocol contracts passed');
