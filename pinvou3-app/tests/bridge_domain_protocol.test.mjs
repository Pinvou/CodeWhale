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
  auxChat: ['bridge/aux-chat.js'],
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
  // #566 共享模块移植后的重算：invoke/listen 函数体按字节等价合并进
  // shared/bridge-shared-helpers.js（其共享面哈希单独立钉于 expectedSharedBaseHash），
  // 各 lane feature 文件的原始文本摘要随之变化；命令面（命令名/事件名集合）
  // 经合并前后逐命令比对确认无增删（knowledge 的 kb_model_cancel、
  // remoteControl 的 web_access_reset_relay / web_access:event_unsubscribe
  // 为上游 #566 判定的死代码，随 PR 一并移除）。
  // Aux-chat domain debut (community #433 backflow): three commands —
  // get_or_create_aux_session / discard_aux_session / reset_aux_session —
  // plus the desktop send dispatch chat(restrictTools:true); buffer loading
  // reuses the ensureSessionBufferLoaded injected by the sessions domain (the
  // load_session hash is accounted in the sessions domain).
  // Upstream's hash covered the M7 thin wrapper over the shared
  // PinvouBridgeShared cluster; Official has no shared-cluster mechanism, so
  // the domain bodies live in this module and the hash was recomputed for the
  // self-contained Official-lane file.
  auxChat: '822fb84758b20d045f2fc34673e88c0f9e7aa2acb48e420c0a8b0cdafd93c37a',
  multiAgent: '6896409bb406f1ba54ca496dd78c04549cc78ff5490ae771d9cf60586368a8a8',
  // Recomputed for the rebind wave (community #463): bridge.js's wholesale
  // artifact save now rewrites SavedSession.artifacts[].storage_path along
  // the workspace_rebound mark's segment chain (rebaseArtifactPathsForRebind,
  // consumed from the artifact-tracker feature); same command surface, no new
  // invoke or listen entries (the extractor scans raw source, so body/comment
  // text is part of the digest).
  // Recomputed from the merged main shared-helper surface plus diagnostic wrappers.
  orchestration: "ea8b35e2f9997a51a527387e5cddc94d1ece231dd546529e79eb108fdae18045",
  // Recomputed for artifact export telemetry after a successful external open;
  // attribution is now isolated to the explicitly targeted session.
  // Recomputed again for the Linux clipboard-image paste fallback (community
  // #578 backflow): artifacts.js gains the addPasteImageFromClipboard invoke
  // wrapping the paste_clipboard_image command (the native layer reads the
  // WebKitGTK-invisible clipboard image and saves it server-side; web lane
  // intentionally has no such backend).
  // Recomputed for the rebind wave (community #463): artifact-tracker.js
  // gains the rebind mark helpers (sessionRecentlyRebound /
  // rebaseArtifactPathsForRebind / pathIsRebindStale) and the reconcile's
  // stale-absolute heal arm + rebasing save — same command surface, no new
  // invoke or listen entries (the extractor scans raw source, so
  // body/comment text is part of the digest).
  artifacts: 'd227317f84f5ef929e020d051d96287e9b61c7514d41169c20cffd57aabaa3fe',
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
  // Recomputed for the permission onboarding row: the new
  // computer_use_permission_status invoke (settings permissionStatus) joins
  // the domain's call set.
  computerUse: 'a681a541d4352b99d8e964e5a19f9cbaf45fd9fa9d56d5395d6c9e6c88459843',
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
  // Recomputed for the per-frame stream notification coalescing backflow
  // (Community #727): chat:delta/chat:tool_start/chat:done listener bodies moved
  // to the shared single-publication flush (notify() without domains, one
  // refreshActiveStreamRender hook) — event names and invoke shapes unchanged.
  // Recomputed for community #748 backflow: the chat:tool_delta listener body
  // now opts into deferBackgroundNotify so background output chunks ride the
  // existing terminal 50ms publication timer; invoke and event wire shapes
  // stay unchanged (publication timing is the deliberate change).
  chat: 'd494f576c5484c6af93a4b37c949a1844d5055b46043eb7f5ccdad6850896a42',
  dependencies: 'd39f5a46b8e34c1c30d8f8e08ef62baccfe822ffb7fad8dbf3301c69c566e31c',
  // Recomputed for #445: the YOLO gate threads an adjudicated session id
  // through exitPlanToYolo and planStuckGo.
  interaction: '0665060ac9dcb1cb487c1a8521ad7b539c3638c65bb5f2c336423741f40f685e',
  knowledge: '5d84a1ea660205ae2d13c85643b07b1feeebb0c3e99fb23214fc7d1cc3c16cdc',
  // memory recomputed for the backflowed memory-maintenance feature
  // (Community #419): organize_memory + get_memory_organize_history invokes
  // added to bridge/memory.js.
  memory: 'b2556cc01a987850633365edcaa152dfe750cdbec37053170c7e5b29b2a66316',
  monitor: '01bf9a7c9b9b3f313cf49e975e6503627ff373caed0f4b3be07a6a98492a7c43',
  personas: '08b63778c3da39e2e551d555c860919887d2652450894d2d5359d7e23fb71ca7',
  remoteControl: '8a47f7ef3fbf569a6ae380153f84640d245025e0beb2215eb365287fe06aa00c',
  scheduled: '3c95cd9aa37173dd5a3490da45191aad1ee608107d8a513e458561ba62ae95cb',
  // Recomputed for the combined session archive and #445 workspace binding
  // surfaces while retaining Official's materialization behavior.
  // Recomputed for the rebind wave (community #463): the session:list_changed
  // listener stamps the workspace_rebound mark (applyWorkspaceReboundMark)
  // and burst-coalesces the history refresh (round-20 R3); same two commands,
  // no new invoke or listen entries (the extractor scans raw source, so
  // body/comment text is part of the digest).
  // Recomputed for the #484 backflow merge: create_session now carries the
  // keychain snapshot (workspaceRoots) and project ownership (projectId)
  // captured by the new captureDraftWorkspaceBinding staging helper, and the
  // sessions feature gains rememberDraftWorkspaceRecent /
  // maybePruneFailedWorkspaceRecent wiring (single-entry workspace picker);
  // same command set, no new invoke or listen entries — the extractor scans
  // raw source, so the merged body/comment text is part of the digest.
  sessions: '70eebf1ac7f64b49db7db4f1b987dc075672116261d49000515fcfe2d5a2fdbe',
  // Recomputed for the builtin feature toggle hook: settings.js gains the
  // list_builtin_features / set_builtin_feature_enabled invoke wrappers (the
  // session-mention feature gate reads the list; the setter is the contract
  // hook for the builtin plugin page).
  // Recomputed for the global bring-to-front shortcut: settings.js gains
  // the set_global_shortcut_config invoke (the dedicated command owns both
  // the plugin registration and the prefs persistence; update_settings
  // stays untouched).
  settings: '3c1b77d8cc6494021baae197d9e08a640c8422156b26bd937431aa80abb651c8',
  updater: '30a3762ee45f378d476efec934dc4be50522a02a5b9a97111191496510d4d7d4',
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
  // Recomputed again after the single-entry workspace picker merge: on top of
  // the rebind carryover feed-back the domain also gains
  // ensure_folder_projects / update_project(roots, lastPrimaryRoot) /
  // projects_set_never_materialize / align_session_to_project
  // (bridge/projects.js).
  projects: 'b7b41e3f15a938ab4a5cad8e28e70efe031f382cdced92a4022badf9119027d1',};

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

// The shared payload base carries the invoke/listen bodies that the batch dedup
// relocated out of the per-lane files, so the domain hashes above no longer
// cover that text. Hash the shared file's surface with the same extractor to
// pin payload edits inside the shared base the same way the lane files are.
const expectedSharedBaseHash = '4e63e15a4ec6b41f2f38a667d9e94d9761c230ab927e91d0c5ae49f3c4abedb4';
const sharedBaseSource = fs.readFileSync(path.join(root, 'src', 'shared', 'bridge-shared-helpers.js'), 'utf8');
const sharedBaseSignatures = [
  ...extractCalls(sharedBaseSource, 'invoke').map(call => `shared/bridge-shared-helpers.js:invoke:${call}`),
  ...extractCalls(sharedBaseSource, 'listen').map(call => `shared/bridge-shared-helpers.js:listen:${call}`),
];
const sharedBaseHash = crypto.createHash('sha256').update(sharedBaseSignatures.join('\n')).digest('hex');
if (!expectedSharedBaseHash) console.log(`sharedBase: ${sharedBaseHash} (${sharedBaseSignatures.length} signatures)`);
else assert.equal(sharedBaseHash, expectedSharedBaseHash, 'shared bridge payload protocol changed');

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
// bridge.js delegates shared helpers to window.PinvouBridgeShared; index.html loads
// the shared payload before the bridges, so the harness loads it first too.
vm.runInContext(fs.readFileSync(path.join(root, 'src', 'shared', 'bridge-shared-helpers.js'), 'utf8'),
  context, { filename: 'shared/bridge-shared-helpers.js' });
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
