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
  accountPlatform: ['bridge/account-platform.js'],
  orchestration: ['bridge.js'],
  artifacts: ['bridge/artifact-tracker.js', 'bridge/artifacts.js'],
  auxChat: ['bridge/aux-chat.js'],
  chat: ['bridge/chat.js', 'bridge/chat-events.js', 'bridge/terminal.js'],
  dependencies: ['bridge/dependencies.js'],
  interaction: ['bridge/interaction.js'],
  knowledge: ['bridge/knowledge-model.js'],
  memory: ['bridge/memory.js'],
  monitor: ['bridge/monitor.js'],
  device: ['bridge/device.js'],
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

// Combined main954 shared extraction with current Account model refs, typed selection,
// local rollback and compact API. Digests pin actual combined invoke/listen bodies.
const expectedProtocolHashes = {
  // Registration prepares only the password reference; availability stays behind captcha in Send.
  accountPlatform: '5a768685d7d2c8f5eea288b6b00d0ba03e680f76be195d20a131a71b8436fa8f',
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
  auxChat: '559070c284d5738a8fb60664487eb1ce96e766b92fcd786e8baa9dc7a8d461a8',
  multiAgent: 'bebb01cb6a970146dd5b6a52679e35dce2d6ed9542171817fc1feadb12bb5561',
  // Recomputed for the subagent status flicker fix (community #773): the
  // agent_progress listen forwards the structured worker_status token (same
  // vocabulary as the worker ledger) alongside the legacy free-text phrase —
  // no new invoke or listen entries, payload-internal edit only.
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
  // T4b 重算（main）：listDeliverableIndex 委托回共享核 v2 协商（官方 v2 命令已随 TB
  // 入主线），list_deliverable_index / list_workspace_files 两个 invoke 面随之
  // 回到 helpers 共享基座；裁决⑤ 定案删除 open_in_system / open_external_url
  // 包装位（Rust 命令有 features 层消费，仅删 JS 面）。同批改动见
  // artifacts.js / tauri/bridge.js facade / web bridge。
  // Attachment failure telemetry extends the two track_behavior_event payloads
  // with stage, step and inputType and updates the terminal error fallback.
  // Retain that payload extension when combining main's Account bridge surface.
  artifacts: '3e7a4ef7c3924e2c1028500de53a3de747f2f3ecc30780b5043ac1f37fe54403',
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
  // 回流 community PR pinvou-agent#826：domain 新增 computer_use_set_virtual_display_preference
  // invoke（设置页"优先使用隔离的虚拟显示器"开关；仅桌面端，与其余命令一样不在 web
  // 访问策略 allowlist 上）。hash 由合并树实际运行本测试读回（actual 值，与来源 PR
  // 记录一致）后写回。
  computerUse: '6e06e71458a8f8e347e83abda1fc39cd04ecbf80c78168abf43f3d8873cf0d75',
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
  // Recomputed for the F05 model lifecycle receipt: the existing chat listener
  // preserves the typed rejection only for its exact current timing owner.
  // Commands, event names and invoke payloads retain their existing shapes;
  // listener callback text is included in this source fingerprint.
  // Deleted scheduled notice IDs now take precedence over live/prefix admission; callback text only, no event/command surface changes.
  // Combined Account billing/model receipts with main's shared stream publication.
  // Recomputed for the per-frame stream notification coalescing backflow
  // (Community #727): chat:delta/chat:tool_start/chat:done listener bodies moved
  // to the shared single-publication flush (notify() without domains, one
  // refreshActiveStreamRender hook) — event names and invoke shapes unchanged.
  // Recomputed for community #748 backflow: the chat:tool_delta listener body
  // now opts into deferBackgroundNotify so background output chunks ride the
  // existing terminal 50ms publication timer; invoke and event wire shapes
  // stay unchanged (publication timing is the deliberate change).
  // T0-d (#701 桥接面对齐, FFE4-1) 重算：remote_control:tools_changed 监听体
  // 的内联 CustomEvent 派发改经 window.PinvouToolStoreEvents
  // .notifyComposerToolsChanged()（shared/tool-store-events.js），命令面无增删。
  // Recomputed for the #2135 review fix wave: the chat:done listener body in
  // chat-events.js gains cancelPendingStreamNotify(sid) (with its trailing
  // comment) so a background session's terminal cancels its own pending
  // stream notify frame — the wave's only digest-input change. The desktop
  // admission-reject flush pairing and its context parameter (chat.js) and
  // the chat-events cluster export entry all sit outside every invoke/listen
  // call span, so they ride along hash-neutral — no invoke or listen call-set
  // change.
  // r10 重钉（评审 r10 P1）：r9 的取消点修复改写了 chat:done 监听体/notify 抑制臂/
  // refresh 钩子的载荷文本——签名清单不变（52 条），但 join 文本哈希随之漂移；
  // 原钉 ec6881aa 是父提交 1d2aa96d8 的值。此钉=当前 head 实算值。
  // r12 重钉（评审 r12 P2 收口批）：终态节流臂加 terminalForeground 前台门
  // （runSyncOnSession 之前捕获）、isNotifySuppressed 谓词并入
  // startupNotificationBatching——invoke/listen 调用面零增删（52 条不变），
  // 抑制臂载荷文本漂移。此钉=当前 head 实算值。
  // 合并 main fb01c48b6 重钉：main 侧 Account lifecycle receipts + 流式发布
  // 载荷改动与本分支 r12 收口批叠加，chat 面哈希=合并树实算值。
  // Stream bubbles now carry htmlSource for native selection provenance.
  // This additive local state field is inside the hashed delta listener;
  // invoke payloads and event names are unchanged.
  // Recomputed for the usage-meter occupancy fix: the chat:usage listener
  // body gains the per-request context_input_tokens consumption ahead of the
  // cumulative-input dirty guard (chat:usage fires once per turn at
  // TurnComplete and any tool_start latches the guard until the next send —
  // without the per-request field the meter froze at "—/…·0%" on every
  // tool-using conversation). Same event surface — no new invoke or listen
  // entries.
  // 834 回流重钉：chat:compaction 监听体新增 in-flight 标记维护块（含
  // compactionStarts/compactionSeq 与 post_tokens 消费）；官方侧 addSystemItem
  // 行保留 `compactId: compactId` 写法，与社区 shorthand 不同，故哈希为本仓实算值。
  // 回流社区 #836 重算：chat.js/chat-events.js 的 <context_update> 序号基线
  // （runtimeContextUpdatesBefore 随气泡下发、压缩后 vintage 重置）与
  // committed 转写权威重同步监听体落在采集文本内；invoke/listen 调用面
  // 无增删（dependencies 钉不变）。
  // rebase 至 main e828f68fe6 重算：#2735（手动压缩入口）对
  // bridge-shared-helpers.js 的 compaction 状态切片与 in-flight 缓冲豁免
  // 与本分支 <context_update> vintage 重置叠加进哈希面，此钉=并集树实算值。
  chat: '0a1ed5b87dcb42672256370ea5cdbd266da011164f3770b14d37363450860712',
  dependencies: 'd39f5a46b8e34c1c30d8f8e08ef62baccfe822ffb7fad8dbf3301c69c566e31c',
  // Recomputed for #445: the YOLO gate threads an adjudicated session id
  // through exitPlanToYolo and planStuckGo.
  interaction: '0665060ac9dcb1cb487c1a8521ad7b539c3638c65bb5f2c336423741f40f685e',
  knowledge: '5d84a1ea660205ae2d13c85643b07b1feeebb0c3e99fb23214fc7d1cc3c16cdc',
  // memory recomputed for the backflowed memory-maintenance feature
  // (Community #419): organize_memory + get_memory_organize_history invokes
  // added to bridge/memory.js.
  // T0-b (#701 桥接面对齐) 重算：confirm/ignore/never 三支 invoke 移入
  // shared/bridge-shared-helpers.js 的 resolveMemoryCandidate（memory.js 留
  // 参数化薄包装），命令面无增删。
  // Recomputed for the PR #834 review-fix wave: the chat:compaction listener
  // now stores a timestamp (not a boolean) at start so the read side can
  // expire a lost-terminal in-flight flag with the same 15-minute backstop
  // as the Rust MAX_COMPACTION_LATCH_AGE latch, and the comment wording no
  // longer calls post_tokens "accurate" (Rust emits a conservative estimate).
  // Event names, invoke shapes and the payload contract unchanged; the web
  // twin carries the identical body.
  // Recomputed for review 5874108926 follow-up: the chat:compaction
  // marker-maintenance block also records the persistent per-session
  // compactionStarts signal (no invoke/listen surface change — the body
  // text is part of the pinned call argument).
  // Recomputed again for review 5876783763: the marker-maintenance block
  // also advances the monotonic compactionSeq counter (no invoke/listen
  // surface change).
  memory: 'ca88a71d9f0f9b15f1df52cf80218694f76b2295add98e65a0d167fc88b627c8',
  monitor: 'e63e8d5d4edf813640e08c9951a12abdbc3cad4991763d3dd956e883e277fb1c',
  // New domain: MegaCube 设备管理 (device manager). Six commands —
  // get_device_snapshot (2s polling) / get_device_services / get_external_models /
  // device_register_external_model / device_remove_external_model /
  // device_doctor_prompt — no listen() entries (the device feature polls, it
  // does not subscribe to events). Initial hash for bridge/device.js.
  device: 'bf61134e5c434322c08b149ce68529c7588ff2cada3b58f44230a5643c22ea24',
  personas: '33efcab9c42a6830ade35861b8a6c60891da9cb282954e78c3bda42091e2474d',
  // 为 rebase 带入的已合并面重算（PR #2322，community #781）：
  // bridge/remote-control.js 增 web_access_revoke_host_workspace 一个 invoke
  // （允许工作区撤销命令，随 #781 合入 main 后经 rebase 进入本分支）；
  // 逐签名 diff 确认除此之外无任何增删。
  remoteControl: 'e2b52e8c6ed48c9a353a5058f68cb50470844224a8488b5be12fb9b9d8b9f771',
  scheduled: '9081c503ef3600f83a0cfd2506955990f6556b196d9ba3903ad08b308b2966b1',
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
  // T0-b (#701 桥接面对齐) 重算：get_session_persona_events /
  // get_session_pinvou_reviews 两支 invoke 移入 helpers 的
  // loadPersonaEventsForSession / loadPinvouReviewsForSession（sessions.js 留
  // 转发壳 + reportSidecarReadFailure 注入），命令面无增删。
  // round-39 评审 P2 重算：sessions.js 新增 createBackgroundSession 的
  // create_session invoke（后台物化——切到既有会话后晚到的诊断动作改走
  // 不切工作集的创建/登记路径），复用既有 create_session 命令，命令面无增删。
  // review P2 重算：createBackgroundSession 的 create_session invoke 显式
  // 携带 setActive:false（后台诊断会话不得抢占后端 SessionStore.active，
  // 与 Web 宿主 web_access_create_session 的 Some(false) 契约对齐），仍复用
  // 既有 create_session 命令，命令面无增删。
  sessions: 'c8e941ef800477dbd5eaf09482a946f8897429c29ddd3005ba76d658a4643069',
  // Recomputed for the builtin feature toggle hook: settings.js gains the
  // list_builtin_features / set_builtin_feature_enabled invoke wrappers (the
  // session-mention feature gate reads the list; the setter is the contract
  // hook for the builtin plugin page).
  // Recomputed for the global bring-to-front shortcut: settings.js gains
  // the set_global_shortcut_config invoke (the dedicated command owns both
  // the plugin registration and the prefs persistence; update_settings
  // stays untouched).
  // Retain PR #1995 model refresh ordering and Account builtin toggles and typed voice references; updater also
  // inherits main's Windows progress event and installation lifecycle.
  // Guarded store deletion, endpoint-conditional deletion and models:changed are retained; list_models
  // uses the shared loader so cross-cluster reads share one ordering boundary.
  // Recomputed for the keyless-aware model and tier probes: the form's noAuth
  // posture is forwarded so saved credentials cannot ride a keyless request.
  settings: 'fd75c39aadcb8eedba062d1e1cf84a36c7e61cc851701894a9c953df8b394cc6',
  updater: 'cfe92470008c2dade64f891c36a5fa70930478f803d4d2a6d100e68d9c29392b',
  // Voice draft preparation now carries its typed source/model reference;
  // session bindings remain authoritative and the command set is unchanged.
  voice: '79c031f1039f8b8cb46d47c37ec9946706d1ec5378cf06c1f7f4654cdf21bd37',
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
// T0-a (#701 桥接面对齐) 重算：helpers 纯搬运新增 10 个 invoke 命令面
// （create_scheduled_task / save_session_artifacts / list_workspace_files /
// get_monitor_snapshot / set_super_permission / get_memory_overview /
// equip_persona / summon_pinvou / get_or_create_aux_session / reset_aux_session）。
// T0-b 重算：helpers 新增 3 个 invoke 面（get_session_persona_events /
// get_session_pinvou_reviews / resolveMemoryCandidate 的 invoke(command, args)
// 参数化入口），合计 74 个签名。
// kb_model_cancel 回流：共享知识模型 helper 新增取消命令，合计 75 个签名。
// Recomputed for the merged main and keyless shared invoke/listen surface.
const expectedSharedBaseHash = '21d0aec57949ee28e4f14d39762a0398eab246f19e405f071006dfbb9ace2945';
const sharedBaseSource = fs.readFileSync(path.join(root, 'src', 'shared', 'bridge-shared-helpers.js'), 'utf8');
const sharedBaseSignatures = [
  ...extractCalls(sharedBaseSource, 'invoke').map(call => `shared/bridge-shared-helpers.js:invoke:${call}`),
  ...extractCalls(sharedBaseSource, 'listen').map(call => `shared/bridge-shared-helpers.js:listen:${call}`),
];
const sharedBaseHash = crypto.createHash('sha256').update(sharedBaseSignatures.join('\n')).digest('hex');
if (!expectedSharedBaseHash) console.log(`sharedBase: ${sharedBaseHash} (${sharedBaseSignatures.length} signatures)`);
else assert.equal(sharedBaseHash, expectedSharedBaseHash, 'shared bridge payload protocol changed');

const featureRegistry = new Proxy({}, {
  get(target, name) {
    if (Object.prototype.hasOwnProperty.call(target, name)) return target[name];
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
vm.runInContext(read('bridge/account-platform.js'), context, { filename: 'bridge/account-platform.js' });
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
assert.equal(typeof api.desktopRemoteControl.getRemoteProxyLoadStatus, 'function', 'diagnostic must reach the assembled desktop bridge');
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
