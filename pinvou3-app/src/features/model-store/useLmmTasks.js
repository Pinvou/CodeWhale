// 长操作(下载/部署/启用/停用/卸载)的任务驱动。
//
// 传输层是**带游标的长轮询**,不是事件推送:发起时立刻拿到 run_id,之后反复调
// `model_store_run_log(run_id, from, wait)`。游标握在前端手里,所以断线重连既不丢行
// 也不重复;`wait=25` 让服务端在没有新行时挂起,循环因此不是忙等。
//
// # 任务态为什么在模块级而不是 hook 局部
//
// 模型商店是**条件挂载**的(main.jsx 里 `currentView === 'modelStore' && <ModelStoreView/>`),
// 切一下视图整棵树就卸载。任务态跟着组件走的话,正在下的几十 GB 会连同 run_id 索引
// 一起消失:回来之后没有进度、点不了取消、脚本请求输入也没人答(600 秒后被服务端的
// PROMPT_TIMEOUT 判死),而再点一次下载会撞上后端的卡片互斥,被渲染成一张说不清原因
// 的失败卡。
//
// 所以任务表提到模块级,轮询循环也跟着提上来——它的存活判据从"组件还挂着吗"换成
// "这个任务还在表里吗",视图挂不挂载都不影响它继续读日志。真正丢了的情况(页面重载、
// 轮询连续失败后自己放弃)由 reattachActiveRuns 从后端的活跃运行登记里接回来。
//
// 提到模块级而不是引一个状态库/Provider:仓库里跨组件共享状态的既有做法就是
// 「模块级单例 + subscribe + useSyncExternalStore」(见 shared/syntax-highlighter.js),
// 沿用它不引新依赖;而拿 Provider 包整个应用会把一条业务链路的状态铺到 app 层去,
// 违反 features -> components 的依赖方向。
//
// 这里只有 React 绑定,没有 JSX;解析与折叠逻辑全在 model-store-logic.js 里。
import { useCallback, useSyncExternalStore } from 'react';
import { answerRun, cancelRun, fetchActiveRuns, pollRunLog } from './model-store-api.js';
import { reduceLogChunk } from './model-store-logic.js';

// 连续多少次轮询失败才判定这次运行真的断了。单次失败很常见(命令层短暂不可用、
// 系统挂起唤醒),直接报错会把一个还在正常下载的任务标成失败。
const MAX_POLL_FAILURES = 5;
const POLL_RETRY_MS = 2000;

// 再附着时最多回放多少行。
//
// 接回一次跑了十几小时的下载,服务端能给出几万行,其中绝大多数是两秒一条的进度行。
// 整段喂给 reduceLogChunk 会让测速采样在同一个时间戳上堆几万个点(而它们本来就算
// 不出速度——回放里所有行的时间戳都是"现在"),白白卡住主线程。
// 只回放末尾这一段:进度条要的是最后一条进度行,日志面板本来也只留最后 500 行。
const REATTACH_REPLAY_LINES = 200;

// ---------------------------------------------------------------- 模块级任务表

/** 任务快照。整体替换而不是就地改,`useSyncExternalStore` 靠引用变化判定更新。 */
let taskList = [];
const listeners = new Set();
/** 任务有结局(成功/失败/取消)时的旁路通知,见 subscribeTaskSettled。 */
const settledListeners = new Set();
/** taskId -> run_id。取消/应答要拿它,而这两件事不该依赖整张任务表。 */
const runIds = new Map();
/** 正在跑的任务 key 集合(比如 `download:<卡片id>`),用来拒掉重复发起。 */
const activeKeys = new Set();
/** 在 run_id 回来之前就点了取消的任务 id;run_id 落地时补发 cancelRun。 */
const cancelWanted = new Set();
/** 已经有轮询循环挂着的 run_id。再附着时靠它去重,不会给同一次运行建两张卡。 */
const attachedRuns = new Set();

let nextTaskId = 1;

function emit() {
  for (const listener of listeners) listener();
}

function setTaskList(next) {
  taskList = next;
  emit();
}

function subscribeTasks(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getTasks() {
  return taskList;
}

/**
 * 订阅「任务有结局了」。
 *
 * 再附着回来的任务没有等在 await 上的调用方(它是上一次挂载、甚至上一次页面加载
 * 发起的),跑完之后得有人去刷模型库——否则任务卡消失了,列表还停在「下载中」的
 * 那一版。
 * @param {(task:object, outcome:object)=>void} listener - 结局回调。
 * @returns {()=>void} 退订。
 */
function subscribeTaskSettled(listener) {
  settledListeners.add(listener);
  return () => settledListeners.delete(listener);
}

function notifySettled(task, outcome) {
  for (const listener of settledListeners) listener(task, outcome);
}

function findTask(id) {
  return taskList.find((task) => task.id === id) || null;
}

function patch(id, updater) {
  setTaskList(taskList.map((task) => (task.id === id ? { ...task, ...updater(task) } : task)));
}

function dropTask(id) {
  runIds.delete(id);
  setTaskList(taskList.filter((task) => task.id !== id));
}

/**
 * 建一张任务卡。
 * @param {{key:string|undefined, kind:string, title:string, reattached?:boolean}} spec - 任务描述。
 * @returns {number} 任务 id。
 */
function createTask({ key, kind, title, reattached = false }) {
  const id = nextTaskId++;
  setTaskList([
    ...taskList,
    {
      id,
      runId: null,
      key,
      // 记下是哪种操作:同一张卡的部署/启用/停用共用一个 key(同一时刻只许跑
      // 一件事),界面要靠它分辨「现在忙的到底是哪件事」,否则点了「重新部署」
      // 会让旁边的「启用」按钮显示成「启用中…」。
      kind,
      title,
      // 是不是从后端接回来的。接回来的那些没有等在 await 上的调用方,结局要靠
      // subscribeTaskSettled 广播出去才有人管。
      reattached,
      lines: [],
      status: 'running',
      error: null,
      progress: null,
      speed: null,
      prompt: null,
      samples: [],
      expanded: false,
    },
  ]);
  return id;
}

// ---------------------------------------------------------------- 轮询

/**
 * 等一会儿再重试。
 * @param {number} ms - 毫秒。
 * @returns {Promise<void>} 到点后 resolve。
 */
function delay(ms) {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

/**
 * 把一次运行从头读到尾。
 * @param {string} runId - 运行 ID。
 * @param {{onChunk:(lines:Array<string>, meta:{awaitingInput:boolean|undefined})=>void, isAlive:()=>boolean, replayLimit?:number|null}} hooks - 行回调、存活判定与回放上限。
 * @returns {Promise<{ok?:boolean, cancelled?:boolean, result?:unknown, error?:unknown, aborted?:boolean}>} 终态。
 */
async function drainRun(runId, { onChunk, isAlive, replayLimit = null }) {
  let cursor = 0;
  let failures = 0;
  // 第一批可能是整段回放(再附着时游标从 0 起)。只有这一批要裁。
  let pendingReplayTrim = replayLimit !== null;
  for (;;) {
    if (!isAlive()) return { aborted: true };
    let chunk;
    try {
      chunk = await pollRunLog(runId, cursor);
      failures = 0;
    } catch (error) {
      failures += 1;
      if (failures >= MAX_POLL_FAILURES) {
        return { ok: false, error: String(error?.message || error) };
      }
      await delay(POLL_RETRY_MS);
      continue;
    }
    cursor = Number(chunk?.next ?? cursor);
    let lines = chunk?.lines || [];
    if (pendingReplayTrim) {
      pendingReplayTrim = false;
      if (lines.length > replayLimit) lines = lines.slice(-replayLimit);
    }
    onChunk(lines, { awaitingInput: chunk?.awaiting_input });
    if (chunk?.done) {
      // done 之后再补一次零等待的轮询,把可能漏掉的收尾几行捞回来。
      //
      // 服务端取日志快照时,是先拿 lines 的锁、算完行数放掉,再去拿 outcome 的锁。
      // 工作线程正好在这两个动作之间 push 了最后一行然后 finish() 的话,这次响应
      // 就是「done:true,但行数少了最后那几行」。而 done 一到我们就不再轮询,
      // 那几行从此拿不到——偏偏它们通常就是真正的错误原文。
      //
      // 窗口很窄,代价也只是一次本机调用,而且整个任务只发这一次。
      try {
        const tail = await pollRunLog(runId, cursor, 0);
        if (tail?.lines?.length) onChunk(tail.lines, { awaitingInput: tail.awaiting_input });
      } catch {
        // 补捞失败不影响结论——结论在上一次响应里已经拿到了。
      }
      return { ok: !!chunk.ok, cancelled: !!chunk.cancelled, result: chunk.result, error: chunk.error };
    }
  }
}

/**
 * 把一个任务接到一次运行上,一直读到它结束。
 *
 * `runTask` 和「再附着」共用这一条:两者的区别只在 run_id 是刚拿到的还是从后端查
 * 回来的,接上之后的行为必须一模一样,否则接回来的任务会少掉一半能力(比如答不了
 * 脚本的提问,而那条路 600 秒不应答就被判死)。
 * @param {number} id - 任务 id。
 * @param {string} runId - 运行 ID。
 * @param {{replayLimit?:number|null}} [options] - 回放上限。
 * @returns {Promise<object>} drainRun 的终态。
 */
function pumpRun(id, runId, { replayLimit = null } = {}) {
  runIds.set(id, runId);
  attachedRuns.add(runId);
  patch(id, () => ({ runId }));
  // 用户可能在 run_id 回来之前就点了取消。那一刻 cancelRun 无处可发,意图被记在
  // cancelWanted 里,这里补发——否则作业会照跑到底,而卡片永远停在"取消中…"。
  if (cancelWanted.delete(id)) {
    cancelRun(runId).catch(() => {});
  }
  return drainRun(runId, {
    replayLimit,
    // 存活判据是「任务还在表里吗」,不是「组件还挂着吗」:视图卸载不该让一次
    // 几十 GB 的下载失去进度和取消入口。用户在面板上关掉任务卡才算真的不要了。
    isAlive: () => taskList.some((task) => task.id === id),
    onChunk: (lines, meta) => {
      if (lines.length === 0) return;
      patch(id, (task) => {
        const next = reduceLogChunk(task, lines, Date.now());
        // 服务端说现在没人在等输入,那日志里这条提示就是**上一轮已经答过的**。
        // 回放时再弹一次不只是多点一下:用户填的答案会写进一个没人来取的应答槽,
        // 等脚本下一次真的提问时被这份陈答案顶掉(双机部署里那就是拿旧 IP 去连
        // 从机)。只在服务端明确说"没在等"时清掉,不因为它说"在等"而凭空弹出来。
        return meta?.awaitingInput === false ? { ...next, prompt: null } : next;
      });
    },
  });
}

/**
 * 把一次运行的终态落到任务卡上,并广播出去。
 * @param {number} id - 任务 id。
 * @param {object} outcome - drainRun 的终态。
 * @returns {object} 给调用方的结果。
 */
function settleTask(id, outcome) {
  // 广播要带上任务本身(成功的那一支马上就把它从表里删了)。
  const task = findTask(id);
  // 任务被用户从面板上关掉了(isAlive 为假才会走到这里),没有任何状态要落。
  if (outcome.aborted) return { ok: false, aborted: true };
  // cancelled 必须**先于** ok 判断。服务端是分别记录这两个标记的,取消信号
  // 在作业已经返回 Ok 之后才落地时,会出现 `ok:true, cancelled:true`。
  // 先判 ok 就会把用户亲手取消的操作当成功移除、还弹一条报喜 toast——
  // 而"取消等于失败,绝不能标记成功"是这条链路上不能破的规矩。
  //
  // 用户点的取消在底层也是一个 Err,但对用户是两回事:一个是"你自己停的",
  // 一个是"它炸了"。界面不该把两者显示成一样,更不该把取消的任务展开日志吓唬人。
  if (outcome.cancelled) {
    patch(id, () => ({ status: 'cancelled', error: null, prompt: null }));
    const result = { ok: false, cancelled: true };
    notifySettled(task, result);
    return result;
  }
  if (outcome.ok) {
    // 成功的任务直接从面板上移除,由调用方用一条 toast 报喜。
    dropTask(id);
    const result = { ok: true, result: outcome.result };
    notifySettled(task, result);
    return result;
  }
  const message = outcome.error ? String(outcome.error) : '';
  // 失败的任务留在面板上并自动展开日志,用户可以看清原因再手动关掉。
  patch(id, () => ({ status: 'error', error: message, prompt: null, expanded: true }));
  const result = { ok: false, error: message };
  notifySettled(task, result);
  return result;
}

/**
 * 发起或接回时出错:把任务卡标成失败并广播。
 * @param {number} id - 任务 id。
 * @param {unknown} error - 错误。
 * @returns {string} 已归一的错误文本。
 */
function failTask(id, error) {
  const message = String(error?.message || error);
  const task = findTask(id);
  patch(id, () => ({ status: 'error', error: message, prompt: null, expanded: true }));
  notifySettled(task, { ok: false, error: message });
  return message;
}

// ---------------------------------------------------------------- 对外操作

/**
 * 跑一个长操作。
 *
 * `key` 标识「同一件事」(比如 `download:<卡片id>`):同一个 key 已经有任务在跑时
 * 直接拒掉。按钮禁用只是视觉上的第一道防线,连点和事件竞态都能绕过去,这里必须
 * 再拦一道 —— 同一张卡跑两份下载不会更快,只会让两个进程抢同一批文件的锁。
 * @param {{title:string, kind:string, key:string|undefined, start:()=>Promise<{run_id?:string}>}} options - 任务描述与发起函数。
 * @returns {Promise<{ok:boolean, duplicate?:boolean, aborted?:boolean, cancelled?:boolean, result?:unknown, error?:string}>} 结果。
 */
async function runTask(options) {
  const { title, kind, key, start } = options;
  if (key && activeKeys.has(key)) {
    return { ok: false, duplicate: true };
  }
  if (key) activeKeys.add(key);
  const id = createTask({ key, kind, title });
  let runId = null;
  try {
    const started = await start();
    runId = started?.run_id;
    if (!runId) throw new Error('missing run_id');
    return settleTask(id, await pumpRun(id, runId));
  } catch (error) {
    return { ok: false, error: failTask(id, error) };
  } finally {
    if (key) activeKeys.delete(key);
    if (runId) attachedRuns.delete(runId);
  }
}

/**
 * 把后端还在跑的运行接回任务面板。
 *
 * 已经有轮询挂着的运行会被跳过,所以重复调用是安全的——每次刷新列表顺手再问一次,
 * 就能把"轮询连续失败后前端自己放弃了、后端其实还在跑"这种失联也接回来。
 * @param {Array<{run_id?:string, card_id?:string, kind?:string}>} runs - 后端给的活跃运行。
 * @param {(run:object)=>({title:string, key:string}|null)} describe - 由视图层按 i18n 词典给出标题与 key;认不出的操作返回 null。
 * @returns {number} 这次真正接回来的数量。
 */
function reattachRuns(runs, describe) {
  let attached = 0;
  for (const run of runs || []) {
    const runId = run?.run_id;
    if (!runId || attachedRuns.has(runId)) continue;
    const meta = describe(run);
    // 认不出的操作不接:宁可不显示,也不要一张说不清自己在干什么、却点得了取消
    // 的任务卡。
    if (!meta) continue;
    // 这一刻本地刚发起同一件事、run_id 还没落地(activeKeys 是同步占上的,
    // attachedRuns 要等 start() 返回才占上)。接回去就会给同一次操作建两张卡。
    if (meta.key && activeKeys.has(meta.key)) continue;
    if (meta.key) activeKeys.add(meta.key);
    const id = createTask({ key: meta.key, kind: run.kind, title: meta.title, reattached: true });
    attached += 1;
    // 不 await:可能一次接回好几个,它们各自跑到结束。
    pumpRun(id, runId, { replayLimit: REATTACH_REPLAY_LINES })
      .then((outcome) => settleTask(id, outcome))
      .catch((error) => failTask(id, error))
      .finally(() => {
        if (meta.key) activeKeys.delete(meta.key);
        attachedRuns.delete(runId);
      });
  }
  return attached;
}

/**
 * 问一次后端有哪些运行还在跑,并把它们接回面板。
 * @param {(run:object)=>({title:string, key:string}|null)} describe - 同 reattachRuns。
 * @returns {Promise<number>} 接回来的数量;问不到时返回 0。
 */
async function reattachActiveRuns(describe) {
  try {
    const payload = await fetchActiveRuns();
    return reattachRuns(payload?.runs, describe);
  } catch {
    // 问不到不打扰用户:面板上本来就什么都没有,再弹一句报错只会让人以为刚才
    // 那次下载出了事。下一次刷新列表还会再问一遍。
    return 0;
  }
}

/**
 * 取消一个任务。
 * @param {number} id - 任务 id。
 * @returns {void} 无返回。
 */
function cancelTask(id) {
  const runId = runIds.get(id);
  patch(id, () => ({ status: 'cancelling', prompt: null }));
  if (runId) {
    cancelRun(runId).catch(() => {});
  } else {
    // run_id 还没回来。记下意图,等它落地时补发(见 pumpRun)。
    cancelWanted.add(id);
  }
}

/**
 * 回答脚本请求的那次输入。`value` 传 null 表示用户点了取消。
 * @param {number} id - 任务 id。
 * @param {string|null} value - 用户输入;null 表示取消。
 * @returns {void} 无返回。
 */
function answerTask(id, value) {
  const runId = runIds.get(id);
  patch(id, () => ({ prompt: null, status: value === null ? 'cancelling' : 'running' }));
  if (runId) answerRun(runId, value).catch(() => {});
}

/**
 * 从面板上关掉一个任务。还在跑的话,轮询循环下一轮就会停下(见 pumpRun 的 isAlive)。
 * @param {number} id - 任务 id。
 * @returns {void} 无返回。
 */
function dismissTask(id) {
  dropTask(id);
}

/**
 * 展开 / 收起日志。
 * @param {number} id - 任务 id。
 * @returns {void} 无返回。
 */
function toggleTaskExpand(id) {
  patch(id, (task) => ({ expanded: !task.expanded }));
}

/**
 * 订阅任务表。
 *
 * 只回读的那两样。发起/取消/应答都是模块级函数,直接引用即可——它们的身份恒定,
 * 调用方的 useCallback 依赖数组因此是稳的,不会跟着每一批日志重建。
 * @returns {{tasks:Array<object>, runningTask:(key:string)=>object|null}} 任务快照与忙碌查询。
 */
function useLmmTasks() {
  const tasks = useSyncExternalStore(subscribeTasks, getTasks);

  // 某件事是不是正在跑(供界面禁用按钮)。看 tasks 而不是 activeKeys,因为普通
  // 集合的变化不会触发重渲染,按钮不会跟着变灰。
  const runningTask = useCallback(
    (key) => tasks.find((task) => task.key === key && task.status !== 'error' && task.status !== 'cancelled') || null,
    [tasks]
  );

  return { tasks, runningTask };
}

export {
  useLmmTasks,
  // 任务表的读侧。hook 用它做 useSyncExternalStore 的两个参数,测试用它在没有
  // React 的情况下直接观察这张表。
  subscribeTasks,
  getTasks,
  runTask,
  cancelTask,
  answerTask,
  dismissTask,
  toggleTaskExpand,
  reattachRuns,
  reattachActiveRuns,
  subscribeTaskSettled,
  drainRun,
  MAX_POLL_FAILURES,
  REATTACH_REPLAY_LINES,
};
