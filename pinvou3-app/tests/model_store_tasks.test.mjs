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
/** 命令名 -> 要抛的错误文本。用来模拟 IPC 自己失败。 */
const failCommands = new Map();
/**
 * 命令名 -> 一个先要 await 的 Promise。用来把某个 IPC **按住**，好让用例排出
 * 「上一次作答还悬在半空、用户已经点了取消而取消又失败了」这种真实的交错——
 * 那正是「作答成功后清 errorKey」必须带条件的理由。
 */
const holdCommands = new Map();

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
        const hold = holdCommands.get(command);
        if (hold) await hold;
        // 让用例能指定「取消/应答这个命令自己失败」——不是「运行已结束」那种正常
        // 落空，而是 IPC 真的没送达。这条路径原先被 `.catch(() => {})` 整个吃掉。
        const boom = failCommands.get(command);
        if (boom) throw new Error(boom);
        return { ok: true };
      }
      if (command === 'model_store_active_runs') return { runs: [] };
      throw new Error(`测试没准备这个命令：${command}`);
    },
  },
};

const {
  REATTACH_REPLAY_LINES,
  answerTask,
  cancelTask,
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

test('启用首次失败保留原始错误及后续身份错误日志，不重新启动', async () => {
  const first = 'ERROR: 引擎在建立运行记录前已退出；请查看诊断日志';
  const later = 'ERROR: 发现未被本卡片可靠记录的模型进程，未认领或停止它；请联系管理员核对';
  const error = `脚本执行失败（退出码 70）：${later}\n${first}\n${later}`;
  let starts = 0;
  scripts.set('run-enable-first-error', (from) => from === 0
    ? { lines: [first], next: 1, done: false, awaiting_input: false }
    : { lines: [later], next: 2, done: true, ok: false, cancelled: false, error });
  const result = await runTask({
    title: '启用模型', kind: 'enable', key: 'lifecycle:first-error',
    start: async () => { starts += 1; return { run_id: 'run-enable-first-error' }; },
  });
  const task = taskByRunId('run-enable-first-error');
  assert.equal(result.ok, false);
  assert.equal(starts, 1);
  assert.equal(task.status, 'error');
  assert.equal(task.error, error);
  assert.deepEqual(task.lines, [first, later]);
});

// 兜底：把这一条测试留下的任务全部关掉。
//
// 任务表是模块级的，轮询也是——一条断言挂掉会跳过它自己那句 dismissTask，于是
// 那个任务会**一直轮询下去**：定时器不散，node --test 永远不退出，本该是一句
// 「没有接回进度」的失败就变成 CI 上一个没有任何线索的超时；后面的测试还会被
// 它持续增长的轮询计数污染。这两件事都真的发生过。
afterEach(() => {
  for (const task of [...getTasks()]) dismissTask(task.id);
  failCommands.clear();
  holdCommands.clear();
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

// ---------------------------------------------------------------- 取消/应答失败

/**
 * 起一个「一直在跑、永不结束」的任务，返回它的任务 id。
 *
 * 这才是这两个 bug 真正咬人的场景：几十分钟到十几小时的下载。用会结束的运行测不
 * 出来——任务一落地就被移出面板，回灌上去的错误没人看得见。
 * @param {string} runId - 假后端里的运行 id。
 * @param {string} kind - 操作类型。
 * @returns {Promise<number>} 任务 id。
 */
async function startNeverEndingTask(runId, kind) {
  const before = new Set(getTasks().map((t) => t.id));
  // 故意不 await：这个 Promise 到运行结束才 resolve，而它永不结束。
  runTask({ title: `${kind}「测试卡」`, kind, key: `${kind}:${runId}`, start: async () => ({ run_id: runId }) });
  await waitFor(() => getTasks().some((t) => !before.has(t.id)), '任务卡没建起来');
  return getTasks().find((t) => !before.has(t.id)).id;
}

test('取消命令自己失败时要浮出来，不能让卡片顶着「正在取消」跑完', async () => {
  scripts.set('run-cancel-fails', (from) => idleChunk(from));
  failCommands.set('model_store_cancel_run', 'IPC 通道没了');

  const id = await startNeverEndingTask('run-cancel-fails', 'download');
  cancelTask(id);

  // 关键断言：失败必须浮到卡片上，而且状态要退回 running 让按钮重新可点。
  // 原先这里是 `.catch(() => {})`，卡片会永远停在 cancelling、错误一个字不显示。
  await waitFor(
    () => {
      const t = getTasks().find((x) => x.id === id);
      return t && t.status === 'running' && t.errorKey === 'taskCancelFailed' && String(t.errorDetail).includes('IPC 通道没了');
    },
    '取消失败既没有回灌错误、也没有把状态退回 running——用户会一直看着「正在取消」',
  );

  // 而且不能顺手把整张卡判死：运行还在继续，轮询循环仍然会送回它真正的结局。
  assert.notEqual(
    getTasks().find((x) => x.id === id).status,
    'error',
    '取消失败不等于任务失败，判死这张卡的权力属于轮询循环',
  );
});

test('作答没送达时把提示放回去，别让输入框凭空消失', async () => {
  scripts.set('run-answer-fails', (from) => {
    if (from === 0) {
      return { lines: ['LMM_PROMPT=text|请输入从机 IP'], next: 1, done: false, awaiting_input: true };
    }
    // 脚本还阻塞在它那次 read 上，所以「在等作答」这个标志一直立着
    return idleChunk(from, { awaiting_input: true });
  });
  failCommands.set('model_store_answer_run', '后端连接断了');

  const id = await startNeverEndingTask('run-answer-fails', 'deploy');
  await waitFor(() => getTasks().find((t) => t.id === id)?.prompt, '提示没出现');

  answerTask(id, '10.0.0.2');

  // 提示要回来（用户得能再答一次），原因要看得见。否则用户看到的是「输入框没了，
  // 然后十分钟后任务莫名失败」——脚本那边 600 秒后被 PROMPT_TIMEOUT 判死。
  await waitFor(
    () => {
      const t = getTasks().find((x) => x.id === id);
      return t && t.prompt && t.errorKey === 'taskAnswerFailed' && String(t.errorDetail).includes('后端连接断了');
    },
    '作答失败后提示没有放回去、或者错误没有浮出来',
  );
});

test('run_id 还没落地就点取消：补发失败也要回灌', async () => {
  // **这条盖的是第三处**（pumpRun 里 run_id 落地后补发 cancelWanted 的那次）。
  //
  // 上面两条测试都走不到这里：`startNeverEndingTask` 的 start() 是同步 resolve 的，
  // `runTask` 的续体（内含 runIds.set）在微任务里先于测试续体执行，所以 cancelTask
  // 时 runIds 必有值，直达路径就返回了。要覆盖补发路径，start() 必须挂起——用本文件
  // :183 那条测试的门控写法。
  let releaseStart = null;
  const started = new Promise((resolve) => { releaseStart = resolve; });
  scripts.set('run-late', (from) => idleChunk(from));
  failCommands.set('model_store_cancel_run', '补发时通道也断了');

  const before = new Set(getTasks().map((t) => t.id));
  runTask({ title: '下载「晚到的卡」', kind: 'download', key: 'download:late', start: () => started });
  await waitFor(() => getTasks().some((t) => !before.has(t.id)), '任务卡没建起来');
  const id = getTasks().find((t) => !before.has(t.id)).id;

  // run_id 还在路上就点取消 —— 意图记进 cancelWanted，等落地再补发
  cancelTask(id);
  await waitFor(() => getTasks().find((t) => t.id === id)?.status === 'cancelling', '没进入取消中');

  releaseStart({ run_id: 'run-late' });

  // 补发的那次 cancelRun 失败，同样要回灌：状态退回 running、错误看得见
  await waitFor(
    () => {
      const t = getTasks().find((x) => x.id === id);
      return t && t.status === 'running' && t.errorKey === 'taskCancelFailed'
        && String(t.errorDetail).includes('补发时通道也断了');
    },
    '补发取消失败后没有回灌——卡片会一直停在「取消中」，而这正是那段注释自己警告的事',
  );
});

// ------------------------------------------- 回灌的 errorKey 必须在该清时清掉
//
// 上面那三条只证明了「IPC 自己失败时错误能浮出来」。反面同样要有守卫：那条错误
// 是**临时**的，说的是「刚才那条命令没送到」，不是这次运行的结局。从来没有代码
// 清它的话，会出三种自相矛盾的界面（都是渲染实证过的）：
//   1. 取消失败后运行真的取消掉了 —— 终态卡挂着陈旧的「取消没有生效」，还把
//      「已取消」这个副标题顶掉；
//   2. 作答重试成功后 —— 卡片一路挂着「这次作答没有送达」直到终态；
//   3. 真实失败落下来时 —— 红字行和展开日志各说一套（一个是真实原因，一个是
//      陈旧的取消失败）。
// 对应的清除点：settleTask 的两个终态分支、failTask、以及 answerRun 的成功支。

/**
 * 造一个「按 finish 标志决定要不要收尾」的日志脚本。
 * @param {string} runId - 运行 ID。
 * @param {()=>object|null} outcome - 返回终态字段；返回 null 表示还没结束。
 * @returns {void} 无返回。
 */
function settleOnDemand(runId, outcome) {
  scripts.set(runId, (from) => {
    const done = outcome();
    if (!done) return idleChunk(from);
    return {
      lines: [],
      next: from,
      done: true,
      ok: false,
      cancelled: false,
      result: null,
      error: null,
      awaiting_input: false,
      ...done,
    };
  });
}

test('取消失败留下的 errorKey，到了 cancelled 终态必须清掉', async () => {
  let cancelled = false;
  settleOnDemand('run-stale-cancel', () => (cancelled ? { cancelled: true } : null));
  failCommands.set('model_store_cancel_run', 'IPC 通道没了');

  const id = await startNeverEndingTask('run-stale-cancel', 'download');
  cancelTask(id);
  await waitFor(
    () => getTasks().find((x) => x.id === id)?.errorKey === 'taskCancelFailed',
    '取消失败没有回灌，后面这条断言就无从谈起',
  );

  // 取消其实生效了（或者运行自己跑完、服务端照样记了 cancelled 标记）。
  cancelled = true;
  await waitFor(() => getTasks().find((x) => x.id === id)?.status === 'cancelled', '没落到 cancelled 终态');

  const t = getTasks().find((x) => x.id === id);
  assert.equal(
    t.errorKey,
    null,
    '终态卡还挂着「取消没有生效」——而这会儿恰恰是真取消成功了，红字还会把「已取消」顶掉',
  );
  assert.equal(t.errorDetail, null, 'errorDetail 留着，下次词典换个 key 就会把它再显示出来');
  assert.equal(t.error, null);
});

test('取消失败留下的 errorKey，到了失败终态也要清 —— 否则真实原因被它盖住', async () => {
  let failed = false;
  settleOnDemand('run-stale-fail', () => (failed ? { error: '磁盘空间不足' } : null));
  failCommands.set('model_store_cancel_run', 'IPC 通道没了');

  const id = await startNeverEndingTask('run-stale-fail', 'download');
  cancelTask(id);
  await waitFor(() => getTasks().find((x) => x.id === id)?.errorKey === 'taskCancelFailed', '取消失败没有回灌');

  failed = true;
  await waitFor(() => getTasks().find((x) => x.id === id)?.status === 'error', '没落到失败终态');

  const t = getTasks().find((x) => x.id === id);
  assert.equal(t.error, '磁盘空间不足');
  assert.equal(
    t.errorKey,
    null,
    'errorKey 还在：面板的词典优先级会让红字显示「取消没有生效」，而展开日志里是真实原因，两处打架',
  );
  assert.equal(t.errorDetail, null);
});

test('作答重试成功后，「这次作答没有送达」要自己撤掉', async () => {
  scripts.set('run-answer-retry', (from) => {
    if (from === 0) return { lines: ['LMM_PROMPT=text|请输入从机 IP'], next: 1, done: false, awaiting_input: true };
    return idleChunk(from, { awaiting_input: true });
  });
  failCommands.set('model_store_answer_run', '后端连接断了');

  const id = await startNeverEndingTask('run-answer-retry', 'deploy');
  await waitFor(() => getTasks().find((t) => t.id === id)?.prompt, '提示没出现');

  answerTask(id, '10.0.0.2');
  await waitFor(
    () => getTasks().find((x) => x.id === id)?.errorKey === 'taskAnswerFailed',
    '第一次作答失败没有回灌',
  );

  // 用户按回车再答一次，这次送到了。
  failCommands.delete('model_store_answer_run');
  await waitFor(() => getTasks().find((t) => t.id === id)?.prompt, '提示没放回来，用户没法重答');
  answerTask(id, '10.0.0.2');

  await waitFor(
    () => {
      const t = getTasks().find((x) => x.id === id);
      return t && t.errorKey === null && t.errorDetail === null;
    },
    '作答已经送达了，卡片还挂着「这次作答没有送达」——这句话会一直挂到终态',
  );
});

test('作答成功不能顺手抹掉更晚发生的取消失败', async () => {
  // 这条守的是清除的**条件**：只在 errorKey 仍是 taskAnswerFailed 时才清。
  // 交错顺序是真实的——作答那次 IPC 还悬着，用户等不及点了取消，取消失败，
  // 然后作答那次才回来说「其实送到了」。无条件清的话，那条更晚、更要紧的
  // 「取消没有生效」会被一起抹掉，用户以为取消成功了。
  scripts.set('run-answer-then-cancel', (from) => {
    if (from === 0) return { lines: ['LMM_PROMPT=text|请输入从机 IP'], next: 1, done: false, awaiting_input: true };
    return idleChunk(from, { awaiting_input: true });
  });
  let releaseAnswer = null;
  holdCommands.set('model_store_answer_run', new Promise((resolve) => { releaseAnswer = resolve; }));
  failCommands.set('model_store_cancel_run', '取消通道断了');

  const id = await startNeverEndingTask('run-answer-then-cancel', 'deploy');
  await waitFor(() => getTasks().find((t) => t.id === id)?.prompt, '提示没出现');

  answerTask(id, '10.0.0.2');            // 这次 IPC 被按住，还没回来
  cancelTask(id);                        // 用户等不及，点了取消 —— 而取消失败
  await waitFor(
    () => getTasks().find((x) => x.id === id)?.errorKey === 'taskCancelFailed',
    '取消失败没有回灌',
  );

  releaseAnswer();                       // 作答那次这才回来，而且是成功
  // 给成功支的微任务留出执行机会：这里不能用 waitFor（要断言的是「什么都没变」）。
  await sleep(20);

  const t = getTasks().find((x) => x.id === id);
  assert.equal(
    t.errorKey,
    'taskCancelFailed',
    '作答成功把更晚的「取消没有生效」一起抹掉了——用户会以为取消生效了，而作业还在跑',
  );
  assert.equal(String(t.errorDetail), '取消通道断了');
});
