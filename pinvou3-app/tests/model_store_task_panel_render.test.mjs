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
