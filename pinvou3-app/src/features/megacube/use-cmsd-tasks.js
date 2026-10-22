/**
 * 换胎任务监听（维护模式窗口的数据源）。
 *
 * 轮询 cmsd /v1/tasks：有 running 的 switch 任务就浮出维护窗。只有「切换模型」
 * （停旧启新挪别名）会打断当前对话，需要挡住操作；商店启停有自己的 run 日志
 * 长轮询呈现进度，MCP 启停有工具应答，都不弹窗（用户拍板 2026-10-13）。
 * 任务从 running 翻成终结态的瞬间回调一次（自动切换模型选择用）。
 * 管理面不在场时完全不轮询（无感缺席）。
 */
import { useEffect, useRef, useState } from 'react';
import { fetchCmsStatus, fetchCmsTasks, probeCmsAvailable } from './cms-api.js';

const POLL_MS = 3000;
// 只盯 switch：唯一会打断当前对话的任务类型。enable/disable/set_default
// 由发起方（商店 run 日志 / MCP 应答）自行呈现结果，维护窗不掺和。
const WATCHED_KINDS = ['switch'];

/**
 * @param {{ onTaskSettled?: (task: object) => void, onActiveChanged?: (card: string) => void, onTaskObserved?: (task: object) => void }} opts -
 *   任务终结回调 + 默认模型（活跃指针）变化回调。
 * @returns {{ cmsAvailable: boolean|null, activeTask: import('./CmsdTaskModal.jsx').CmsdTask|null, settledTask: import('./CmsdTaskModal.jsx').CmsdTask|null, dismissSettled: () => void }}
 */
export function useCmsdTaskWatch({ onTaskSettled, onActiveChanged, onTaskObserved } = {}) {
  const [cmsAvailable, setCmsAvailable] = useState(/** @type {boolean|null} */ (null));
  const [activeTask, setActiveTask] = useState(null);
  const [recoveredActiveTask, setRecoveredActiveTask] = useState(null);
  const [settledTask, setSettledTask] = useState(null);
  const [hiddenTaskId, setHiddenTaskId] = useState(null);
  const settledRef = useRef(new Set());
  // 首轮基线：已在终结态的历史任务只登记不回调——否则每次启动都把昨天的
  // 失败/成功重放成弹窗（真机实证：启动后弹出昨天 failtest 的熔断失败卡）。
  const firstPollDoneRef = useRef(false);
  const callbackRef = useRef(onTaskSettled);
  callbackRef.current = onTaskSettled;
  const activeCallbackRef = useRef(onActiveChanged);
  activeCallbackRef.current = onActiveChanged;
  const observedCallbackRef = useRef(onTaskObserved);
  observedCallbackRef.current = onTaskObserved;
  // 活跃指针基线：首轮记录不回溯（不在场的变化不追），之后每次变化跟一次
  //（跟完用户再手选是用户的自由，不强制回压）。
  const lastActiveRef = useRef(null);

  useEffect(() => {
    let alive = true;
    const probe = () => { void probeCmsAvailable().then(ok => { if (alive) setCmsAvailable(ok); }); };
    probe();
    const timer = setInterval(probe, 15000);
    return () => { alive = false; clearInterval(timer); };
  }, []);

  useEffect(() => {
    if (cmsAvailable !== true) return undefined;
    let alive = true;
    let inFlight = false;
    const tick = async () => {
      if (inFlight) return;
      inFlight = true;
      try {
        const res = await fetchCmsTasks();
        if (!alive) return;
        if (!res?.available) return;
        setCmsAvailable(true);
        const tasks = Array.isArray(res.data?.tasks) ? res.data.tasks : [];
        const running = tasks.find(
          (task) => task && task.status === 'running' && WATCHED_KINDS.includes(task.kind)
        ) || null;
        setActiveTask(running);
        // 终结检测：running 集合里没有了的被跟踪任务 → 找它的最新记录回调一次。
        // 首轮只建基线不回调（历史终结任务登记为已见）。
        const isFirstPoll = !firstPollDoneRef.current;
        for (const task of tasks) {
          if (!task || !task.id || !WATCHED_KINDS.includes(task.kind)) continue;
          observedCallbackRef.current?.(task);
          if (task.status === 'running') continue;
          if (settledRef.current.has(task.id)) continue;
          settledRef.current.add(task.id);
          if (isFirstPoll) continue;
          setSettledTask(task);
          callbackRef.current?.(task);
        }
        firstPollDoneRef.current = true;
        // 活跃指针跟随（set_default 这类无任务长操作的唯一可见通道）
        const statusRes = await fetchCmsStatus();
        if (alive && statusRes?.available) {
          const current = statusRes.data?.active_model || null;
          if (lastActiveRef.current === null) {
            lastActiveRef.current = current ?? '';
          } else if ((current ?? '') !== lastActiveRef.current) {
            lastActiveRef.current = current ?? '';
            if (current) activeCallbackRef.current?.(current);
          }
        }
      } catch (error) {
        console.warn('[megacube] 任务轮询失败：', error);
      } finally {
        inFlight = false;
      }
    };
    tick();
    const timer = setInterval(tick, POLL_MS);
    return () => { alive = false; clearInterval(timer); };
  }, [cmsAvailable]);

  const currentTask = activeTask || recoveredActiveTask;
  // Details of an accepted receipt may be older than the public index. Running
  // work keeps the normal backgroundable overlay; old terminal work must never
  // replay the completion dialog or the first-poll history callbacks.
  const observeRecoveredTask = task => {
    if (task?.kind !== 'switch') return;
    if (task.status === 'running') setRecoveredActiveTask(task);
    else if (['succeeded', 'failed', 'cancelled'].includes(task.status)) {
      setRecoveredActiveTask(current => current?.id === task.id ? null : current);
    }
  };
  const dismissSettled = () => { setSettledTask(null); if (currentTask) setHiddenTaskId(currentTask.id); };
  return { cmsAvailable, activeTask: currentTask?.id === hiddenTaskId ? null : currentTask, settledTask, dismissSettled, observeRecoveredTask };
}
