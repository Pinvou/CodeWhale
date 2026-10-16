#!/usr/bin/env node
// ModelStoreTaskPanel 的真实渲染回归。
//
// 为什么非得真渲染：这个面板此前**一行测试都没有**，任务相关的用例全在
// useLmmTasks 那一层（tests/model_store_tasks.test.mjs），于是「回灌一条
// errorKey、状态还是 running」这种组合在 hook 层断言全绿，而面板拿到同一份
// 任务对象时对 `task.error`（此刻是 null）调 `.split` 直接抛 TypeError——
// 被 main.jsx 的 ViewErrorBoundary 兜住之后，**整个模型商店视图**被替换成
// 「页面加载失败」，侧栏还会报一句指向完全错误方向的 viewLoadFailed。
//
// 也就是说：这条崩溃的触发条件恰恰是本轮要修的那个场景（取消 / 作答的 IPC
// 自己失败），而它正好落在两层测试之间的缝里。所以这里补的是**渲染级**的守卫。
//
// .jsx 与资源导入（.svg/.png）都由 tests/fixtures/jsx-module-hooks.mjs 处理。
import assert from 'node:assert/strict';
import test from 'node:test';

import { renderToString } from 'react-dom/server';

import { loadJsx } from './fixtures/jsx-module-hooks.mjs';

const { ModelStoreTaskPanel } = await loadJsx('../src/features/model-store/ModelStoreTaskPanel.jsx', import.meta.url);
const { dict } = await import('./helpers/i18n-all.js');

// 用**真实词典**，不用手搓的桩：这一条崩溃的根因之一就是「errorKey 过词典之后
// 非空、而 task.error 是 null」这个组合，桩词典很容易把它抹平掉。
const copy = dict.zh.uiModelStore;

test('下载字节达到100%但任务未结束时，不显示完成百分比和虚假的剩余0秒', () => {
  const html = render(task({ progress: { done: 100, total: 100 }, speed: 10 }));
  assert.ok(html.includes(copy.taskAwaitingCompletion));
  assert.match(html, /data-progress-pending="true"/);
  assert.doesNotMatch(html, /100\.0%|剩余约|aria-valuenow="100"/);
});

/**
 * 造一张任务卡。默认是一张最普通的、正在跑的下载卡。
 * @param {object} overrides - 要覆盖的字段。
 * @returns {object} 任务对象，字段与 useLmmTasks.createTask 的初始态一致。
 */
function task(overrides) {
  return {
    id: 1,
    runId: 'run-1',
    key: 'download:card-a',
    kind: 'download',
    title: '下载「card-a」',
    reattached: false,
    lines: ['正在下载 …'],
    status: 'running',
    error: null,
    errorKey: null,
    errorDetail: null,
    progress: null,
    speed: null,
    prompt: null,
    samples: [],
    expanded: false,
    ...overrides,
  };
}

/**
 * 渲染一张卡，返回 HTML。
 * @param {object} one - 任务对象。
 * @returns {string} HTML。
 */
function render(one) {
  const noop = () => {};
  return renderToString(
    ModelStoreTaskPanel({
      tasks: [one],
      copy,
      onCancel: noop,
      onDismiss: noop,
      onToggle: noop,
      onAnswer: noop,
      onForceRemove: noop,
      onStopRetained: noop,
    }),
  );
}

test('回灌态（error 为 null + errorKey 有值 + 还在跑）不能崩，红字要说人话', () => {
  // 这正是 cancelTask / answerTask / pumpRun 三处回灌写出来的状态：它们只写
  // errorKey / errorDetail，从不写 error（createTask 把 error 初始化成 null）。
  const html = render(
    task({ status: 'running', error: null, errorKey: 'taskCancelFailed', errorDetail: 'IPC 通道没了' }),
  );
  assert.match(
    html,
    /取消没有生效：IPC 通道没了/,
    '回灌的结构化错误没有过词典显示出来——用户会看到按钮恢复可点却不知道为什么',
  );
  // 副标题被红字顶掉是对的（一张卡只有一行说明位），这里顺手钉住「不是两行都出」。
  assert.doesNotMatch(html, /正在执行…/, '红字和普通副标题同时出现了');
});
test('作答没送达时同样不能崩：输入框还在，红字也在', () => {
  const html = render(
    task({
      status: 'running',
      error: null,
      errorKey: 'taskAnswerFailed',
      errorDetail: '后端连接断了',
      prompt: { kind: 'text', message: '请输入从机 IP' },
    }),
  );
  assert.match(html, /这次作答没有送达：后端连接断了/);
  assert.match(html, /model-store-prompt-text/, '提示框没放回来，用户没法再答一次');
});

test('终态失败只取错误首行，后面几行留给展开日志', () => {
  const html = render(
    task({ status: 'error', error: '磁盘空间不足\n详细堆栈：/var/lib/...\n第三行', expanded: false }),
  );
  assert.match(html, /磁盘空间不足/);
  assert.doesNotMatch(html, /详细堆栈/, '第二行不该挤进那条两行截断的红字里');
});

test('对照：普通运行中的卡片走副标题，不走红字', () => {
  const html = render(task({ lines: [] }));
  assert.match(html, /正在执行…/);
});

test('对照：已取消的卡片显示「已取消」，且不该有任何红字残留', () => {
  // settleTask 的 cancelled 分支现在把 error / errorKey / errorDetail 一起清掉；
  // 这条钉住「清干净之后面板确实显示的是已取消」，跟 hook 层那条清除断言配对。
  const html = render(task({ status: 'cancelled', error: null, errorKey: null, errorDetail: null }));
  assert.match(html, /已取消/);
  assert.doesNotMatch(html, /取消没有生效/);
});

// ---------------------------------------------------------------- 阶段化进度与简化日志

test('副标题优先给阶段名，不再拿最后一行日志去填', () => {
  // 原来算不出百分比就把最后一行日志顶上来。装引擎时那一行是 docker 的层进度，
  // 十几层并行、每层每秒重绘，副标题在原地乱跳——用户的原话是「我看到都一脸懵逼」。
  const html = render(task({ stage: '正在拉取引擎镜像', lines: ['7014a14111a1: Downloading [==>] 1GB/4GB'] }));
  assert.match(html, /正在拉取引擎镜像/);
  assert.doesNotMatch(html, /7014a14111a1/, '层进度又爬回副标题了');
});

test('估算进度必须标出来——它的分母会往上长，百分比会往回跳', () => {
  const layers = { aaaaaaaaaaaa: { status: 'Downloading', done: 1000, total: 4000, complete: false } };
  const html = render(task({ stage: '正在拉取引擎镜像', layers }));
  assert.match(html, /估算/, '估算没标出来，用户会以为进度条坏了');
  assert.match(html, /data-progress-estimated="true"/);
  // 脚本自己报了真分母时不许出现「估算」
  const real = render(task({ progress: { done: 1, total: 4 } }));
  assert.doesNotMatch(real, /估算/);
  assert.match(real, /data-progress-kind="bytes"/);
});

test('拿不到字节分母时给粗粒度步骤，不假装有字节级进度', () => {
  const html = render(task({ stage: '正在安装依赖', step: { index: 2, total: 4 } }));
  assert.match(html, /第 2 步 \/ 共 4 步/);
  assert.match(html, /data-progress-kind="step"/);
});

test('展开后：docker 的几十层聚合成一行，日志工具收进「更多」以保持底栏单行', () => {
  const layers = {
    aaaaaaaaaaaa: { status: 'Download complete', done: 4000, total: 4000, complete: true },
    bbbbbbbbbbbb: { status: 'Downloading', done: 1000, total: 4000, complete: false },
  };
  const html = render(task({ expanded: true, layers }));
  assert.match(html, /model-store-task-layers/);
  assert.match(html, /镜像层 1\/2/);
  // 「复制详情 / 打开日志目录」仍在工具菜单内；首屏只露“更多”，避免五个按钮
  // 在 360px 卡片底部被截断。
  assert.match(html, /model-store-task-tools/);
  assert.match(html, /更多/);
  assert.doesNotMatch(html, /model-store-task-copy-log/);
});

test('收起状态下不出「复制详情」——没有日志可看，多一个按钮只是噪声', () => {
  assert.doesNotMatch(render(task({ expanded: false })), /model-store-task-copy-log/);
  assert.doesNotMatch(render(task({ expanded: false })), /model-store-task-tools/);
});

test('启用失败且明确保留实例时提供停用入口；其他失败不臆测有进程', () => {
  const retained = task({ kind: 'enable', key: 'card:card-a', status: 'error', retainedNativeProcess: true });
  const html = render(retained);
  assert.match(html, /model-store-task-stop-retained/);
  assert.ok(html.includes(copy.stopRetainedProcess));
  assert.match(html, /flex-wrap items-center justify-end/);
  assert.doesNotMatch(render({ ...retained, retainedNativeProcess: false }), /model-store-task-stop-retained/);
  assert.doesNotMatch(render({ ...retained, status: 'running' }), /model-store-task-stop-retained/);
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(dict[language].uiModelStore.stopRetainedConfirm('MODEL').includes('MODEL'));
    assert.equal(typeof dict[language].uiModelStore.stopRetainedUnavailable, 'string');
  }
});

test('展开的任务卡提供「更多」入口，菜单默认收起（不渲染任何菜单项）', () => {
  // SSR 不执行事件：菜单条目（导出/打开目录/复制）在 toolsOpen 打开前本就不该
  // 出现在渲染结果里。这里断言的是**布局事实**——入口在、菜单内容不外漏。
  // 菜单项各自的显示条件由 task-panel-tools 的纯函数测试真实覆盖（本文件下方）。
  const failed = render(task({ expanded: true, status: 'error', error: '磁盘空间不足' }));
  assert.match(failed, /model-store-task-tools/);
  assert.doesNotMatch(failed, /role="menu"/);
  assert.doesNotMatch(failed, /model-store-task-export-log/);
  assert.doesNotMatch(failed, /model-store-task-open-log-dir/);
  // 收起的卡片没有工具入口（旧断言的另一半，语义保留）。
  assert.doesNotMatch(render(task({ expanded: false })), /model-store-task-tools/);
});

test('「导出日志（可外发）」的显示条件：失败且有运行日志', async () => {
  // 门控抽成了纯函数（task-panel-tools.js），这里是不依赖交互的真实断言——
  // 旧测试断言与标题相反且在 SSR 下恒真空（toolsOpen 永远 false，改什么都不红）。
  const { exportLogEligible } = await import('../src/features/model-store/task-panel-tools.js');
  assert.equal(exportLogEligible({ status: 'error', runId: 'run-1' }), true, '失败且有日志 → 给');
  assert.equal(exportLogEligible({ status: 'error', runId: null }), false, '失败但没有可导出的运行 → 不给');
  assert.equal(exportLogEligible({ status: 'running', runId: 'run-1' }), false, '在跑 → 不给（还没有要反馈的失败）');
  assert.equal(exportLogEligible({ status: 'completed', runId: 'run-1' }), false, '完成 → 不给');
  assert.equal(exportLogEligible({}), false, '空对象 → 不给（不得抛错）');
});

