#!/usr/bin/env node
// sendMessageToSession 的存在性「双尺子」此前只有浏览器 smoke 覆盖（无 Chrome
// 即 SKIP）：把放宽的第二把尺子还原掉，node 套件照绿，而医生排查直达发送在
// 真实机器上会撞"目标会话不存在"（第八轮评审 P2）。本文件把两把尺子与配套的
// 回收纪律钉进 node 套件：
//   1) 后端列表命中（常规路径：pet 回复等，目标来自列表）；
//   2) 本地已有 buffer——**本端刚物化**的会话由 switchActiveTo 直接建 buffer，
//      state.sessions 只在 list_sessions 刷新时更新，物化完立刻发送（设备管理
//      排查直达）靠它放行；
//   3) 真删除必须清 buffer（purgeSessionBuffer）：否则第 2 把尺子会给已删除
//      会话放行（后端仍会拒，但前端失去快失败语义）。
// 源码形状钉（classic 脚本无模块边界可导入，与 pet_reply_contract 同风格）。
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const chatBridge = readFileSync(
  path.join(here, '..', 'src', 'shared', 'bridge-shared-helpers.js'),
  'utf8',
);
const sessionsBridge = readFileSync(
  path.join(here, '..', 'src', 'platform', 'tauri', 'bridge', 'sessions.js'),
  'utf8',
);

// 只检查 sendMessageToSession 函数体（起点到下一个同级函数声明），避免文件里
// 其他 sessions.some 用法稀释断言。函数正文自 FE14 wave 5 起收敛在
// shared/bridge-shared-helpers.js（web+tauriChat 共享），lane 侧只剩薄包装；
// sessionStates 在共享正文里是 cell 依赖（.value）。
const start = chatBridge.indexOf('async function sendMessageToSession');
assert.ok(start >= 0, 'sendMessageToSession not found in shared/bridge-shared-helpers.js');
let next = chatBridge.indexOf('\n  async function ', start + 10);
if (next === -1) next = chatBridge.length;
const sendBody = chatBridge.slice(start, next);

assert.match(sendBody, /state\.sessions\.some\(/, 'ruler 1: backend list hit');
assert.match(
  sendBody,
  /Object\.prototype\.hasOwnProperty\.call\(sessionStates\.value, sid\)/,
  'ruler 2: local buffer (own-property check, prototype-safe)',
);
// 第二把尺子必须排除 aux id：aux 会话就住 sessionStates（后端把它们过滤在
// list_sessions 之外），无条件放行会把 aux-chat 的隔离不变量（sendMessageToSession
// 要求 sid ∈ state.sessions，auxChatSend 才带 restrictTools）打穿（第二十轮评审
// P2）。把 `!auxChatIsAuxSession(sid) &&` 守卫删掉时本断言必须变红。
assert.match(
  sendBody,
  /!auxChatIsAuxSession\(sid\)\s*\n?[\s\S]{0,40}&&\s*Object\.prototype\.hasOwnProperty\.call\(sessionStates\.value, sid\)/,
  'ruler 2 must exclude aux ids (aux isolation invariant)',
);

const purgeStart = sessionsBridge.indexOf('function purgeSessionBuffer');
assert.ok(purgeStart >= 0, 'purgeSessionBuffer not found in sessions.js');
const purgeBody = sessionsBridge.slice(purgeStart, purgeStart + 400);
assert.match(purgeBody, /delete sessionStates\[/, 'real deletion must drop the buffer');

// 真删除路径的 purge 调用点（round-22 评审 P2 补钉）：上一版只钉了
// purgeSessionBuffer 的函数体——把 applyDeletedSession 里的调用删掉、
// "已删除会话不得被第 2 把尺子放行"的快失败语义就只剩后端拒绝兜底，本文件
// 照绿。函数体形状钉：purge 必须先于 sessions 过滤（先清 buffer 再摘列表，
// 否则中途读取会看到"列表已删、buffer 还在"的放行窗口）。
const applyStart = chatBridge.indexOf('function applyDeletedSession');
assert.ok(applyStart >= 0, 'applyDeletedSession not found in shared/bridge-shared-helpers.js');
let applyNext = chatBridge.indexOf('\n  function ', applyStart + 10);
if (applyNext === -1) applyNext = chatBridge.length;
const applyBody = chatBridge.slice(applyStart, applyNext);
const purgeCallAt = applyBody.indexOf('purgeSessionBuffer(id)');
const listFilterAt = applyBody.indexOf('state.sessions = state.sessions.filter');
assert.ok(purgeCallAt !== -1, 'applyDeletedSession must purge the deleted session buffer');
assert.ok(listFilterAt !== -1, 'applyDeletedSession must filter the session list');
assert.ok(
  purgeCallAt < listFilterAt,
  'the buffer purge must run before the session list is filtered',
);
