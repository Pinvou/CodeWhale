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
const { dict } = await import('../src/shared/i18n.js');

// 用**真实词典**，不用手搓的桩：这一条崩溃的根因之一就是「errorKey 过词典之后
// 非空、而 task.error 是 null」这个组合，桩词典很容易把它抹平掉。
const copy = dict.zh.uiModelStore;

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

test('展开后：docker 的几十层聚合成一行，且给出「复制详情」', () => {
  const layers = {
    aaaaaaaaaaaa: { status: 'Download complete', done: 4000, total: 4000, complete: true },
    bbbbbbbbbbbb: { status: 'Downloading', done: 1000, total: 4000, complete: false },
  };
  const html = render(task({ expanded: true, layers }));
  assert.match(html, /model-store-task-layers/);
  assert.match(html, /镜像层 1\/2/);
  // 「复制详情」是撤简日志的前提：面板上现在只剩阶段化的少量人话，用户要把排查
  // 信息交给别人时不该靠在一个 160px 高的框里手选。
  assert.match(html, /model-store-task-copy-log/);
  assert.match(html, /复制详情/);
  // 面板上只留最后 500 行、而且过滤过；盘上那份是完整的。
  assert.match(html, /model-store-task-open-log-dir/);
});

test('收起状态下不出「复制详情」——没有日志可看，多一个按钮只是噪声', () => {
  assert.doesNotMatch(render(task({ expanded: false })), /model-store-task-copy-log/);
});

test('「导出日志（可外发）」只在失败的卡片上给', () => {
  // 它存在的理由是脱敏分了两道：落盘那份留着路径和内网地址（不留就没法诊断），
  // 导出那份把它们一起抹掉。没有这个入口，更严的那一道就没有任何出口。
  const failed = render(task({ expanded: true, status: 'error', error: '磁盘空间不足' }));
  assert.match(failed, /model-store-task-export-log/);
  // 正在跑的卡片不给：这会儿还没有"要反馈的失败"，多一个按钮只是噪声
  assert.doesNotMatch(render(task({ expanded: true })), /model-store-task-export-log/);
});

