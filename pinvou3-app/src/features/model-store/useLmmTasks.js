// 长操作(下载/部署/启用/停用/卸载)的任务驱动。
//
// 传输层是**带游标的长轮询**,不是事件推送:POST 立刻拿到 run_id,之后反复
// `GET /runs/<id>/log?from=N&wait=25`。游标握在前端手里,所以断线重连既不丢行
// 也不重复;`wait=25` 让服务端在没有新行时挂起,循环因此不是忙等。
//
// 任务状态刻意不做成全局 Provider:任务面板只活在模型商店页里,拿 Provider 包整个
// 应用会把一条业务链路的状态铺到 app 层去,违反 features -> components 的依赖方向。
//
// 这里只有 React,没有 JSX;解析与折叠逻辑全在 model-store-logic.js 里。
import { useCallback, useEffect, useRef, useState } from 'react';
import { answerRun, cancelRun, pollRunLog } from './model-store-api.js';
import { reduceLogChunk } from './model-store-logic.js';

// 连续多少次轮询失败才判定这次运行真的断了。单次失败很常见(sidecar 重启、
// 代理抖动),直接报错会把一个还在正常下载的任务标成失败。
const MAX_POLL_FAILURES = 5;
const POLL_RETRY_MS = 2000;

let nextTaskId = 1;

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
 * @param {{onChunk:(lines:Array<string>)=>void, isAlive:()=>boolean}} hooks - 行回调与存活判定。
 * @returns {Promise<{ok?:boolean, cancelled?:boolean, result?:unknown, error?:unknown, aborted?:boolean}>} 终态。
 */
async function drainRun(runId, { onChunk, isAlive }) {
  let cursor = 0;
  let failures = 0;
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
    onChunk(chunk?.lines || []);
    if (chunk?.done) {
      // done 之后再补一次零等待的轮询,把可能漏掉的收尾几行捞回来。
      //
      // 服务端取日志快照时,是先拿 lines 的锁、算完行数放掉,再去拿 outcome 的锁。
      // 工作线程正好在这两个动作之间 push 了最后一行然后 finish() 的话,这次响应
      // 就是「done:true,但行数少了最后那几行」。而 done 一到我们就不再轮询,
      // 那几行从此拿不到——偏偏它们通常就是真正的错误原文。
      //
      // 窗口很窄,代价也只是一次本机回环请求,而且整个任务只发这一次。
      try {
        const tail = await pollRunLog(runId, cursor, 0);
        if (tail?.lines?.length) onChunk(tail.lines);
      } catch {
        // 补捞失败不影响结论——结论在上一次响应里已经拿到了。
      }
      return { ok: !!chunk.ok, cancelled: !!chunk.cancelled, result: chunk.result, error: chunk.error };
    }
  }
}

/**
 * 任务面板的状态机。
 * @returns {object} run / cancel / answer / dismiss / toggle 以及 tasks 快照与忙碌查询。
 */
function useLmmTasks() {
  const [tasks, setTasks] = useState([]);
  // 组件卸载后轮询要停下来,否则 setState 会打到已卸载的树上。
  const aliveRef = useRef(true);
  // 正在跑的任务 key 集合。用 ref 而不是从 tasks 里推:run 是 useCallback 包的,
  // 闭包里的 tasks 会是旧值,连点两下就会双双漏过去。
  const activeKeysRef = useRef(new Set());
  // runId 索引:取消/回答要拿到它,而这两个回调不该依赖 tasks(依赖了就会在每次
  // 日志刷新时重建,进而让整个面板重渲染)。
  const runIdsRef = useRef(new Map());
  /** 在 run_id 回来之前就点了取消的任务 id;run_id 落地时补发 cancelRun。 */
  const cancelWantedRef = useRef(new Set());

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const patch = useCallback((id, updater) => {
    setTasks((prev) => prev.map((task) => (task.id === id ? { ...task, ...updater(task) } : task)));
  }, []);

  const drop = useCallback((id) => {
    runIdsRef.current.delete(id);
    setTasks((prev) => prev.filter((task) => task.id !== id));
  }, []);

  /**
   * 跑一个长操作。
   *
   * `key` 标识「同一件事」(比如 `download:<卡片id>`):同一个 key 已经有任务在跑时
   * 直接拒掉。按钮禁用只是视觉上的第一道防线,连点和事件竞态都能绕过去,这里必须
   * 再拦一道 —— 同一张卡跑两份下载不会更快,只会让两个进程抢同一批文件的锁。
   */
  const run = useCallback(
    async (options) => {
      const { title, kind, key, start } = options;
      if (key && activeKeysRef.current.has(key)) {
        return { ok: false, duplicate: true };
      }
      if (key) activeKeysRef.current.add(key);
      const id = nextTaskId++;
      setTasks((prev) => [
        ...prev,
        {
          id,
          runId: null,
          key,
          // 记下是哪种操作:同一张卡的部署/启用/停用共用一个 key(同一时刻只许跑
          // 一件事),界面要靠它分辨「现在忙的到底是哪件事」,否则点了「重新部署」
          // 会让旁边的「启用」按钮显示成「启用中…」。
          kind,
          title,
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

      try {
        const started = await start();
        const runId = started?.run_id;
        if (!runId) throw new Error('missing run_id');
        runIdsRef.current.set(id, runId);
        patch(id, () => ({ runId }));
        // 用户可能在 run_id 回来之前就点了取消。那一刻 cancelRun 无处可发,
        // 意图被记在 cancelWantedRef 里,这里补发——否则作业会照跑到底,
        // 而卡片永远停在"取消中…"。
        if (cancelWantedRef.current.delete(id)) {
          cancelRun(runId).catch(() => {});
        }

        const outcome = await drainRun(runId, {
          isAlive: () => aliveRef.current,
          onChunk: (lines) => {
            if (lines.length === 0) return;
            patch(id, (task) => reduceLogChunk(task, lines, Date.now()));
          },
        });

        if (outcome.aborted) return { ok: false, aborted: true };
        // cancelled 必须**先于** ok 判断。服务端是分别记录这两个标记的,取消信号
        // 在作业已经返回 Ok 之后才落地时,会出现 `ok:true, cancelled:true`。
        // 先判 ok 就会把用户亲手取消的操作当成功移除、还弹一条报喜 toast——
        // 而"取消等于失败,绝不能标记成功"是这条链路上不能破的规矩。
        //
        // 用户点的取消在底层也是一个 Err,但对用户是两回事:一个是"你自己停的",
        // 一个是"它炸了"。界面不该把两者显示成一样,更不该把取消的任务展开日志
        // 吓唬人。
        if (outcome.cancelled) {
          patch(id, () => ({ status: 'cancelled', error: null, prompt: null }));
          return { ok: false, cancelled: true };
        }
        if (outcome.ok) {
          // 成功的任务直接从面板上移除,由调用方用一条 toast 报喜。
          drop(id);
          return { ok: true, result: outcome.result };
        }
        const message = outcome.error ? String(outcome.error) : '';
        // 失败的任务留在面板上并自动展开日志,用户可以看清原因再手动关掉。
        patch(id, () => ({ status: 'error', error: message, prompt: null, expanded: true }));
        return { ok: false, error: message };
      } catch (error) {
        const message = String(error?.message || error);
        patch(id, () => ({ status: 'error', error: message, prompt: null, expanded: true }));
        return { ok: false, error: message };
      } finally {
        if (key) activeKeysRef.current.delete(key);
      }
    },
    [drop, patch]
  );

  const cancel = useCallback(
    (id) => {
      const runId = runIdsRef.current.get(id);
      patch(id, () => ({ status: 'cancelling', prompt: null }));
      if (runId) {
        cancelRun(runId).catch(() => {});
      } else {
        // run_id 还没回来。记下意图,等它落地时补发(见 runIdsRef.current.set 处)。
        cancelWantedRef.current.add(id);
      }
    },
    [patch]
  );

  /**
   * 回答脚本请求的那次输入。`answer` 传 null 表示用户点了取消。
   */
  const answer = useCallback(
    (id, value) => {
      const runId = runIdsRef.current.get(id);
      patch(id, () => ({ prompt: null, status: value === null ? 'cancelling' : 'running' }));
      if (runId) answerRun(runId, value).catch(() => {});
    },
    [patch]
  );

  const toggleExpand = useCallback((id) => patch(id, (task) => ({ expanded: !task.expanded })), [patch]);

  // 某件事是不是正在跑(供界面禁用按钮)。看 tasks 而不是 activeKeysRef,因为 ref
  // 变化不会触发重渲染,按钮不会跟着变灰。
  const runningTask = useCallback(
    (key) => tasks.find((task) => task.key === key && task.status !== 'error' && task.status !== 'cancelled') || null,
    [tasks]
  );

  return { tasks, run, cancel, answer, dismiss: drop, toggleExpand, runningTask };
}

export { useLmmTasks, drainRun, MAX_POLL_FAILURES };
