#!/usr/bin/env node
// features/model-store/useLmmTasks.js 的任务表单测。
//
// 这里盯的是一条具体的事故：模型商店的视图是条件挂载的，切一下视图整棵树就卸载。
// 任务态如果跟着组件走，正在下的几十 GB 会连同 run_id 索引一起消失——回来之后没有
// 进度、点不了取消、脚本请求输入也没人答（600 秒后被服务端判死），再点一次下载还会
// 撞上后端的卡片互斥，被渲染成一张说不清原因的失败卡。
//
// 所以两件事必须成立，而且必须有测试守着：
//   1. 任务表在模块级，轮询的存活判据是「任务还在表里吗」而不是「组件还挂着吗」；
//   2. 真丢了的时候（页面重载、轮询连续失败后放弃）能按后端的活跃运行登记接回来，
//      接回来之后的能力跟自己发起的完全一样（进度 / 取消 / 应答）。
import assert from 'node:assert/strict';
import test, { afterEach } from 'node:test';

// ---------------------------------------------------------------- 假后端
//
// 模块在 import 时就会取 __TAURI__，所以桩要先装好再动态 import。

/** 每个 run_id 的日志脚本：from 游标 -> 响应。 */
const scripts = new Map();
/** 每个命令被调了多少次，用来断言"轮询真的停了"。 */
const callCounts = new Map();

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/** 还没结束、也没有新行的那一档响应。模拟服务端长轮询挂起，免得测试里空转。 */
function idleChunk(from, extra = {}) {
  return { lines: [], next: from, done: false, awaiting_input: false, ...extra };
}

globalThis.__TAURI__ = {
  core: {
    invoke: async (command, payload) => {
      callCounts.set(command, (callCounts.get(command) || 0) + 1);
      if (command === 'model_store_run_log') {
        const script = scripts.get(payload.runId);
        if (!script) throw new Error(`没有这个运行记录：${payload.runId}`);
        const chunk = script(payload.from);
        // 没有新行的那一档要稍微挂一下：真实服务端 wait=25 秒，测试里挂几毫秒
        // 就够，否则轮询循环会变成忙等把这个文件跑成死循环。
        if (!chunk.done && (chunk.lines || []).length === 0) await sleep(5);
        return chunk;
      }
      if (command === 'model_store_cancel_run' || command === 'model_store_answer_run') {
        return { ok: true };
      }
      if (command === 'model_store_active_runs') return { runs: [] };
      throw new Error(`测试没准备这个命令：${command}`);
    },
  },
};

const {
  REATTACH_REPLAY_LINES,
  dismissTask,
  getTasks,
  reattachRuns,
  runTask,
  subscribeTaskSettled,
  subscribeTasks,
} = await import('../src/features/model-store/useLmmTasks.js');

/** 后端报回来的一次活跃运行，界面上叫什么。测试里直接用 card_id 当名字。 */
const describe = (run) => ({ title: `下载「${run.card_id}」`, key: `download:${run.card_id}` });

/**
 * 等某个条件成立。
 * @param {()=>boolean} predicate - 条件。
 * @param {string} message - 超时时的说法。
 * @returns {Promise<void>} 成立后 resolve。
 */
async function waitFor(predicate, message) {
  for (let i = 0; i < 400; i += 1) {
    if (predicate()) return;
    await sleep(5);
  }
  assert.fail(message);
}

const taskByRunId = (runId) => getTasks().find((task) => task.runId === runId) || null;

// 兜底：把这一条测试留下的任务全部关掉。
//
// 任务表是模块级的，轮询也是——一条断言挂掉会跳过它自己那句 dismissTask，于是
// 那个任务会**一直轮询下去**：定时器不散，node --test 永远不退出，本该是一句
// 「没有接回进度」的失败就变成 CI 上一个没有任何线索的超时；后面的测试还会被
// 它持续增长的轮询计数污染。这两件事都真的发生过。
afterEach(() => {
  for (const task of [...getTasks()]) dismissTask(task.id);
});

/**
 * 录下某次运行在任务表里经历过的每一个状态。
 *
 * 为什么不直接在某一刻去抓快照：假后端的每一档响应都在微任务里就绪，一次运行
 * 从接回到跑完根本不经过定时器，而成功的任务跑完就被移出面板。拿 setTimeout 去
 * 轮询"此刻进度是多少"必然扑空——看到的永远是运行开始前或者结束之后。
 *
 * 对着整条变更流断言比抓瞬时快照更强：它要求"进度确实出现过并且当时还在跑",
 * 而不是"某一刻恰好还没跑完"。
 * @param {string} runId - 运行 ID。
 * @returns {{history:Array<object>, stop:()=>void}} 历史与退订。
 */
function recordHistory(runId) {
  const history = [];
  const stop = subscribeTasks(() => {
    const task = taskByRunId(runId);
    if (task) history.push({ status: task.status, kind: task.kind, progress: task.progress, lines: [...task.lines] });
  });
  return { history, stop };
}

// ---------------------------------------------------------------- 再附着

test('再附着：把后端还在跑的运行接回面板，跑完后广播结局', async () => {
  scripts.set('run-reattach', (from) => {
    if (from === 0) return { lines: ['正在下载 …', 'LMM_PROGRESS_BYTES=50/100'], next: 2, done: false, awaiting_input: false };
    if (from === 2) {
      return { lines: ['下载完成'], next: 3, done: true, ok: true, cancelled: false, result: { size: 1 }, error: null, awaiting_input: false };
    }
    return { lines: [], next: from, done: true, ok: true, cancelled: false, result: { size: 1 }, error: null, awaiting_input: false };
  });

  const settled = [];
  const unsubscribe = subscribeTaskSettled((task, outcome) => settled.push({ task, outcome }));
  const recorded = recordHistory('run-reattach');

  const attached = reattachRuns([{ card_id: 'card-a', run_id: 'run-reattach', kind: 'download' }], describe);
  assert.equal(attached, 1);

  await waitFor(() => settled.length === 1, '任务跑完了却没有广播结局');
  recorded.stop();
  unsubscribe();

  // 进度必须真的回来，而且是在还在跑的时候就回来：这正是"切走再回来看不到进度"
  // 那条投诉。
  const withProgress = recorded.history.find((snapshot) => snapshot.progress?.done === 50);
  assert.ok(withProgress, '接回来的任务从头到尾都没拿到进度');
  assert.equal(withProgress.progress.total, 100);
  assert.equal(withProgress.status, 'running');
  assert.equal(withProgress.kind, 'download');
  // 进度行不该进日志正文（两秒一条会把真正有用的报错刷没）
  assert.deepEqual(withProgress.lines, ['正在下载 …']);
  // 接回来的任务没有等在 await 上的调用方，视图靠这个标记决定要不要去刷模型库
  assert.equal(settled[0].task.reattached, true);
  assert.deepEqual(settled[0].outcome, { ok: true, result: { size: 1 } });
  // 成功的任务直接从面板上移除，由调用方用一条 toast 报喜
  assert.equal(taskByRunId('run-reattach'), null);
});

test('再附着：同一次运行不会被接回两次', async () => {
  scripts.set('run-dup', (from) => idleChunk(from));

  const first = reattachRuns([{ card_id: 'card-dup', run_id: 'run-dup', kind: 'download' }], describe);
  const second = reattachRuns([{ card_id: 'card-dup', run_id: 'run-dup', kind: 'download' }], describe);
  assert.equal(first, 1);
  assert.equal(second, 0, '已经挂着轮询的运行再接一次就会多出一张一模一样的任务卡');

  const task = taskByRunId('run-dup');
  dismissTask(task.id);
});

test('再附着：认不出的操作不接——宁可不显示，也不要一张说不清自己在干什么的卡片', () => {
  const attached = reattachRuns(
    [{ card_id: 'card-x', run_id: 'run-x', kind: 'teleport' }],
    // 视图层对认不出的 kind 回 null
    () => null
  );
  assert.equal(attached, 0);
  assert.equal(taskByRunId('run-x'), null);
});

test('再附着：本地刚发起、run_id 还没落地时不重复接', async () => {
  let releaseStart = null;
  const started = new Promise((resolve) => {
    releaseStart = resolve;
  });
  scripts.set('run-local', (from) => {
    if (from === 0) return { lines: ['本地发起的'], next: 1, done: true, ok: true, cancelled: false, result: null, error: null, awaiting_input: false };
    return { lines: [], next: from, done: true, ok: true, cancelled: false, result: null, error: null, awaiting_input: false };
  });

  const pending = runTask({
    title: '下载「card-local」',
    kind: 'download',
    key: 'download:card-local',
    start: () => started,
  });

  // run_id 还在路上：activeKeys 已经同步占上了，attachedRuns 还没有。此刻接回去
  // 就会给同一次操作建两张卡。
  const attached = reattachRuns([{ card_id: 'card-local', run_id: 'run-local', kind: 'download' }], describe);
  assert.equal(attached, 0);

  releaseStart({ run_id: 'run-local' });
  const result = await pending;
  assert.equal(result.ok, true);
});

// ---------------------------------------------------------------- 回放

test('回放：已经答过的提示不再弹一次', async () => {
  // 服务端说现在没人在等输入 —— 日志里那条提示是上一轮答过的。
  scripts.set('run-answered', (from) => {
    if (from === 0) {
      return { lines: ['LMM_PROMPT=text|请输入从机 IP', '正在连接从机 …'], next: 2, done: false, awaiting_input: false };
    }
    return idleChunk(from);
  });

  reattachRuns([{ card_id: 'card-ans', run_id: 'run-answered', kind: 'download' }], describe);
  await waitFor(() => (taskByRunId('run-answered')?.lines.length ?? 0) > 0, '没有接回日志');
  const task = taskByRunId('run-answered');
  // 再弹一次不只是多点一下：用户填的答案会写进一个没人来取的应答槽，等脚本下一次
  // 真的提问时被这份陈答案顶掉（双机部署里那就是拿旧 IP 去连从机）。
  assert.equal(task.prompt, null, '服务端说没在等输入，回放里的提示就不该弹出来');
  dismissTask(task.id);
});

test('回放：还在等作答的提示必须弹出来——不答满 600 秒就被判死', async () => {
  scripts.set('run-waiting', (from) => {
    if (from === 0) {
      return { lines: ['LMM_PROMPT=text|请输入从机 IP'], next: 1, done: false, awaiting_input: true };
    }
    return idleChunk(from, { awaiting_input: true });
  });

  reattachRuns([{ card_id: 'card-wait', run_id: 'run-waiting', kind: 'download' }], describe);
  await waitFor(() => taskByRunId('run-waiting')?.prompt !== null, '切回来看不到脚本的提问');
  const task = taskByRunId('run-waiting');
  assert.deepEqual(task.prompt, { kind: 'text', message: '请输入从机 IP' });
  dismissTask(task.id);
});

test('回放：只回放末尾一段，不把几万行进度全喂给折叠器', async () => {
  const backlog = Array.from({ length: 1000 }, (_, i) => `第 ${i} 行`);
  scripts.set('run-long', (from) => {
    if (from === 0) return { lines: backlog, next: backlog.length, done: false, awaiting_input: false };
    return idleChunk(from);
  });

  reattachRuns([{ card_id: 'card-long', run_id: 'run-long', kind: 'download' }], describe);
  await waitFor(() => (taskByRunId('run-long')?.lines.length ?? 0) > 0, '没有接回日志');
  const task = taskByRunId('run-long');
  assert.equal(task.lines.length, REATTACH_REPLAY_LINES);
  // 留的是最后那一段——用户要看的永远是最近发生了什么
  assert.equal(task.lines[task.lines.length - 1], '第 999 行');
  dismissTask(task.id);
});

// ---------------------------------------------------------------- 存活判据

test('关掉任务卡才算真的不要了：轮询随之停下', async () => {
  scripts.set('run-stop', (from) => idleChunk(from));

  reattachRuns([{ card_id: 'card-stop', run_id: 'run-stop', kind: 'download' }], describe);
  await waitFor(() => taskByRunId('run-stop') !== null, '没有接回任务');
  dismissTask(taskByRunId('run-stop').id);

  // 停下来需要一轮：正挂着的那次长轮询要先返回，循环才会看到任务已经不在表里。
  await sleep(40);
  const before = callCounts.get('model_store_run_log');
  await sleep(60);
  assert.equal(callCounts.get('model_store_run_log'), before, '任务已经关掉，轮询还在继续');
});
