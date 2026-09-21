/**
 * 连接器异步收尾不得清空别人的忙碌槽位（PR #1043 审核补充 P1）。
 *
 * 背景：点遮罩只收起 EIP/知道的登录弹窗、不取消后台流程。收起后用户可以发起
 * 另一个工具操作 B（busyId 变成 B），而之前的 EIP/知道流程稍后收到
 * connected/error 时会无条件 setBusyId(null)，把 B 尚未结束的忙碌态清掉——
 * 按钮只按 `busyId === tool.backendId` 判断禁用，于是 B 会提前放闸，可以被
 * 重复触发（重复安装/卸载/导入）。
 *
 * 这里既跑 releaseBusy 的行为断言（复刻"收起 A → 启动 B → A 完成 → B 仍 busy"），
 * 也用源契约锁住调用点，防止回归。
 */
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

import { localizeConnectorAlert, releaseBusy } from '../src/features/tools/connector-ui-state.js';
import { dictEn } from '../src/shared/i18n/en.js';
import { dictJa } from '../src/shared/i18n/ja.js';
import { dictZh } from '../src/shared/i18n/zh.js';

const toolStoreSource = await readFile(
  new URL('../src/features/tools/ToolStoreView.jsx', import.meta.url),
  'utf8',
);
const dialogSource = await readFile(
  new URL('../src/features/tools/ConnectorSsoDialog.jsx', import.meta.url),
  'utf8',
);

// ── 1. 行为：复刻报告里的复现链路 ──────────────────────────────────────────
// 开始 EIP 登录 → 点遮罩收起(busyId 释放) → 开始耗时操作 B(busyId = 'wecom') →
// EIP 登录成功/失败 → B 必须仍然是 busy。
assert.equal(releaseBusy('eip', 'eip'), null, 'A 自己的完成事件必须释放自己的槽位');
assert.equal(
  releaseBusy('wecom', 'eip'),
  'wecom',
  'B 在跑时，A 的完成事件绝不能清空 B 的忙碌态',
);
assert.equal(releaseBusy(null, 'eip'), null, '空槽位释放后仍为空');

// ── 2. 契约：连接器事件监听块全部走 releaseBusy，没有裸 setBusyId(null) ──────
const listenersStart = toolStoreSource.indexOf("ev.listen('wecom:qr'");
// 收尾段用 unlisten.forEach 定位:该 effect 的 return 体量过（disposed 置位 + 释放监听）
// 且随重构变动，锚在稳定的一行上。
const listenersEnd = toolStoreSource.indexOf('unlisten.forEach', listenersStart);
assert.ok(listenersStart > 0 && listenersEnd > listenersStart, '必须能定位连接器事件监听块');
const listenerBlock = toolStoreSource.slice(listenersStart, listenersEnd);

assert.doesNotMatch(
  listenerBlock,
  /setBusyId\(null\)/,
  '连接器完成/失败事件不得无条件清空共享 busyId（会抹掉别的工具未完成的忙碌态）',
);
for (const [toolId, handlers] of [['wecom', 2], ['eip', 2], ['zhidao', 2]]) {
  const scoped = listenerBlock.match(new RegExp(`releaseBusy\\(busy, '${toolId}'\\)`, 'g')) || [];
  assert.equal(
    scoped.length,
    handlers,
    `${toolId} 的 connected 与 error 收尾都必须用 releaseBusy(busy, '${toolId}') 限定作用域`,
  );
}

// ── 3. 契约：共用订阅工厂(feishu/wecom/dingtalk/tmeet)同样限定作用域 ─────────
const factoryStart = toolStoreSource.indexOf('const useConnectorFlowSubscription = ');
const factoryEnd = toolStoreSource.indexOf('}, [enabled]);', factoryStart);
assert.ok(factoryStart > 0 && factoryEnd > factoryStart, '必须能定位 useConnectorFlowSubscription');
const factoryBlock = toolStoreSource.slice(factoryStart, factoryEnd);
assert.doesNotMatch(
  factoryBlock,
  /setBusyId\(null\)/,
  'done/error 收尾不得无条件清空共享 busyId',
);
assert.match(
  factoryBlock,
  /setBusyId\(busy => releaseBusy\(busy, toolId\)\)/,
  'done/error 收尾必须按 toolId 限定作用域',
);

// ── 4. 契约：EIP/知道的收起与取消也只释放自己的槽位 ─────────────────────────
const dialogStart = toolStoreSource.indexOf('<ConnectorSsoDialog state={eipSso}');
const dialogEnd = toolStoreSource.indexOf('{/* 回收站子页面', dialogStart);
assert.ok(dialogStart > 0 && dialogEnd > dialogStart, '必须能定位 SSO 弹窗接线');
const dialogBlock = toolStoreSource.slice(dialogStart, dialogEnd);
assert.doesNotMatch(dialogBlock, /setBusyId\(null\)/, '收起/取消失效不得无条件清空 busyId');
for (const toolId of ['eip', 'zhidao']) {
  const scoped = dialogBlock.match(new RegExp(`releaseBusy\\(busy, '${toolId}'\\)`, 'g')) || [];
  assert.equal(scoped.length, 2, `${toolId} 的 onDismiss 与 onCancel 都要限定作用域`);
}

// ── 5. 契约：后端"等旧流程归还门闩超时"回 busy，不报连接失败 ────────────────
for (const toolId of ['eip', 'zhidao']) {
  assert.match(
    toolStoreSource,
    new RegExp(`const res = await invokeTauri\\('${toolId}_connect_begin'\\)`),
    `${toolId}_connect_begin 必须接住结构化结果`,
  );
}
const busyBranches = toolStoreSource.match(/if \(res && res\.busy\)/g) || [];
assert.equal(busyBranches.length, 2, 'EIP/知道都要把 busy 结果当成"稍后重试"而非失败');
assert.match(
  toolStoreSource,
  /connectorAlert: 'retry'/,
  "busy 结果必须走 'retry' 提示（非 isError 的失败框）",
);
// 等待旧流程期间用户点取消：后端放弃起步并回 {cancelled:true}，前端必须静默收尾
// ——不弹提示，也不能因为这次"没有 started"就把忙碌态漏在那。
const cancelledBranches = toolStoreSource.match(/if \(res && res\.cancelled\)/g) || [];
assert.equal(cancelledBranches.length, 2, 'EIP/知道都要处理等待期间的取消结果');
// 'retry' 是提示而非失败:文案取 connectBusy，且 subtitle 必须非空——TsAlert 在
// subtitle 为空且 isError=false 时会兜底成 installHint/removeHint（卸载提示），
// 挂在"稍后重试"上牛头不对马嘴。
const retryAlert = localizeConnectorAlert(
  { visible: true, connectorAlert: 'retry', toolId: 'eip', isError: false },
  dictZh.uiToolStore,
  { actions: {} },
);
assert.equal(retryAlert.title, dictZh.uiToolStore.connectBusy(dictZh.uiToolStore.toolNames.eip));
assert.ok(
  retryAlert.subtitle && retryAlert.subtitle.length > 0,
  'retry 提示必须有 subtitle，否则会落到卸载/安装兜底文案',
);
assert.notEqual(
  retryAlert.title,
  dictZh.uiToolStore.connectFailed(dictZh.uiToolStore.toolNames.eip),
  'retry 不是连接失败，不得套用失败文案',
);
// "新建会话"按钮是给"配置成功、需新建会话才生效"的提示用的，挂在"稍后重试"上
// 语义不符：retry 必须显式抑制它，且 TsAlert 要认这个开关。
assert.equal(retryAlert.suppressNewChat, true, 'retry 提示必须抑制"新建会话"按钮');
assert.match(
  toolStoreSource,
  /!alert\.isError && !alert\.suppressNewChat && \(/,
  'TsAlert 必须按 suppressNewChat 决定是否渲染"新建会话"按钮',
);
assert.equal(
  localizeConnectorAlert({ visible: true, connectorAlert: 'connected', toolId: 'eip' }, dictZh.uiToolStore, { actions: {} }).suppressNewChat,
  undefined,
  'connected 提示仍需保留"新建会话"按钮',
);

// ── 5b. 遮罩点击只收起：不得回退成"取消"（那会静默杀掉后台登录流程） ───────
assert.match(
  dialogSource,
  /onClick=\{onDismiss\}/,
  '遮罩点击必须只收起（onDismiss）',
);
assert.doesNotMatch(
  dialogSource,
  /onDismiss \|\| onCancel/,
  '不得保留 onDismiss || onCancel 回退链：漏传 onDismiss 时会静默退化成取消',
);

// ── 6. 契约：限流等待也要有进度与出口（监听 sso_wait + 弹窗占位文案） ────────
for (const toolId of ['eip', 'zhidao']) {
  assert.match(
    listenerBlock,
    new RegExp(`ev\\.listen\\('${toolId}:sso_wait'`),
    `${toolId} 必须监听 sso_wait，否则节流等待期间前端无反馈`,
  );
}
assert.match(
  dialogSource,
  /copy\.sso\.previousFlow/,
  '弹窗必须能显示"等上一个流程退出"',
);
assert.match(
  dialogSource,
  /copy\.sso\.throttled\(state\.waitSeconds\)/,
  '弹窗必须能显示限流剩余秒数',
);
assert.match(
  dialogSource,
  /disabled=\{!state\.url\}/,
  '还没拿到 SSO 地址时"在浏览器打开"必须不可点',
);

// ── 7. 三语齐全：新增文案缺一门就会在对应语言下漏字 ───────────────────────
for (const [lang, dict] of [['zh', dictZh], ['en', dictEn], ['ja', dictJa]]) {
  const copy = dict.uiToolStore;
  assert.equal(
    typeof copy.connectBusy,
    'function',
    `${lang}.uiToolStore.connectBusy 必须存在（busy 提示的标题）`,
  );
  assert.ok(
    typeof copy.connectBusyHint === 'string' && copy.connectBusyHint.length > 0,
    `${lang}.uiToolStore.connectBusyHint 必须存在（busy 提示的正文）`,
  );
  assert.equal(
    typeof copy.sso.throttled,
    'function',
    `${lang}.uiToolStore.sso.throttled 必须存在（限流等待占位文案）`,
  );
  assert.ok(
    typeof copy.sso.previousFlow === 'string' && copy.sso.previousFlow.length > 0,
    `${lang}.uiToolStore.sso.previousFlow 必须存在（等旧流程退出的占位文案）`,
  );
}

// ── 8. 启动闸门：busy 期间拒绝重叠启动，防止 setBusyId(id) 抢占他人槽位 ──────
// （回流社区 pinvou-agent#548 不变量②：释放侧已由上文 1-4 节钉住，这里钉住
// 启动侧。）EIP/知道走后端 busy 门闩的"稍后重试"语义，不适用 busyRef 闸门。
// 禁令扫描剥掉行注释只看代码行；正向钉住用原始源码。
function stripTrailingLineComment(line) {
  let quote = null;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (quote) {
      if (ch === "\\") { i += 1; continue; }
      if (ch === quote) quote = null;
    } else if (ch === "'" || ch === '"' || ch === "`") {
      quote = ch;
    } else if (ch === "/" && line[i + 1] === "/") {
      return line.slice(0, i);
    }
  }
  return line;
}
const toolStoreCode = toolStoreSource.split("\n")
  .filter((line) => { const t = line.trimStart(); return !t.startsWith("//") && !t.startsWith("*"); })
  .map(stripTrailingLineComment)
  .join("\n");

// 全文件禁裸清空（上文 2-4 节按块扫描，这里兜底整文件）。
assert.doesNotMatch(
  toolStoreCode,
  /setBusyId\(null\)/,
  '裸 setBusyId(null) 全文件禁止：所有释放必须走 releaseBusy 归属检查',
);

// flowDeps 把 busyRef 送进连接器四件套，connect/disconnect 占槽前拒绝重叠启动
// （retry 复用 connect，同获保护）。
assert.match(
  toolStoreSource,
  /const flowDeps = \{ setBusyId, busyRef, storeCopy, detailCopy, loadBackendState, setAlert, beginToolConfiguration, finishToolConfiguration, findLocalizedTool \};/,
  'flowDeps 必须把 busyRef 送进连接器流程工厂',
);
const startGuards = toolStoreSource.match(/if \(busyRef\.current\) return;/g) || [];
assert.ok(
  startGuards.length >= 3,
  `连接器 connect/disconnect（+handleAction 连接器/卸载分支）必须在忙碌期拒绝启动，实际 ${startGuards.length}`,
);

// 无守卫历史重灾区：五个直接启动的操作入口必须在能力闸门同一行里拒绝忙碌期
// 启动（安装 / 技能安装卸载 / zip 导入 / ima 连接 / ima 断开）；更新确认走
// !updateConfirm 同款闸门。
const entryGuards = toolStoreSource.match(/if \(!canMutateToolStore \|\| busyRef\.current\) return;/g) || [];
assert.ok(
  entryGuards.length >= 5,
  `doInstall/handleSkillAction/doImportSkillZip/connectIma/disconnectIma 必须在忙碌期拒绝启动，实际 ${entryGuards.length}`,
);
assert.match(
  toolStoreSource,
  /if \(!updateConfirm \|\| busyRef\.current\) return;/,
  'doSkillUpdate 必须在忙碌期拒绝启动',
);
