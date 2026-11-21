import React, { useEffect, useState } from 'react';
import { RefreshCw, X } from '../../components/icons.jsx';
import { StatusDot } from '../../components/StatusDot.jsx';

/**
 * @typedef {object} CmsdTask
 * @property {string} id
 * @property {string} kind - switch / enable / disable / set_default / purge
 * @property {string} card
 * @property {string} status - running / succeeded / failed
 * @property {number} [created_at]
 * @property {{message: string}|null} [last_event]
 * @property {string|null} [result]
 * @property {string|null} [error]
 */
/** 只有 switch 任务会到达这里（useCmsdTaskWatch 已过滤）；其余兜底换胎文案。 @type {Record<string, string>} */
const KIND_KEY = {
  switch: 'taskSwitching',
};

/**
 * 换胎维护模式窗口：switch 任务进行中不许关（挡住对话区——换胎停旧启新会
 * 打断当前对话），终结后给结果 + 确认键。数据由 useCmsdTaskWatch 轮询驱动。
 * 商店/MCP 的启停任务不经过这里（各自有进度呈现）。
 * @param {{ task: CmsdTask|null, settled: CmsdTask|null, onDismiss: () => void, copy: Record<string, any> }} props
 */
export function CmsdTaskModal({ task, settled, onDismiss, copy }) {
  const [now, setNow] = useState(() => Date.now());
  const running = !!task;
  useEffect(() => {
    if (!running) return undefined;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [running]);

  if (!running && !settled) return null;
  // 收敛后再渲染 settled 分支：收窄 null
  const settledTask = settled;
  if (!running && !settledTask) return null;

  const elapsed = task?.created_at ? Math.max(0, Math.floor(now / 1000 - task.created_at)) : 0;
  const kindKey = (task && KIND_KEY[task.kind]) || 'taskSwitching';
  const lastMessage = task?.last_event?.message || '';

  return (
    <div
      data-testid="cmsd-task-modal"
      className="fixed inset-0 z-[120] flex items-center justify-center bg-black/45 backdrop-blur-[8px]"
      // 进行中不响应背板点击（维护模式的全部意义就是不让用户乱操作）
      onClick={(event) => event.stopPropagation()}
    >
      <div className="w-[min(460px,calc(100vw-48px))] rounded-[20px] border border-white/70 bg-white p-5 shadow-[0_22px_58px_rgba(0,0,0,0.34)] dark:border-white/[0.14] dark:bg-[#1C1C1E]">
        {running ? (
          <>
            <div className="flex items-center gap-2.5">
              <RefreshCw size={17} className="animate-spin text-sky-600 motion-reduce:animate-none dark:text-sky-400" />
              <div className="text-[15px] font-semibold text-[#1C1C1E] dark:text-[#F2F2F7]">
                {copy[kindKey](task.card)}
              </div>
            </div>
            <button type="button" data-testid="cmsd-task-background" onClick={onDismiss} className="mt-3 text-sm text-sky-600">{copy.taskContinueBackground}</button>
            <div className="mt-2.5 text-[13px] leading-relaxed text-[#6C6C70] dark:text-[#8E8E93]">
              {copy.taskBusyNote}
            </div>
            <div className="mt-3 rounded-xl bg-black/[0.04] px-3 py-2.5 text-[12px] text-[#6C6C70] dark:bg-white/[0.06] dark:text-[#8E8E93]">
              <div className="flex items-center gap-1.5">
                <StatusDot tone="run" />
                <span className="font-medium">{copy.taskElapsed(elapsed)}</span>
              </div>
              {lastMessage && <div className="mt-1 break-all">{lastMessage}</div>}
            </div>
          </>
        ) : settledTask ? (
          <>
            <div className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-2.5">
                <StatusDot tone={settledTask.status === 'succeeded' ? 'ok' : 'fail'} size="md" />
                <div className="text-[15px] font-semibold text-[#1C1C1E] dark:text-[#F2F2F7]">
                  {settledTask.status === 'succeeded' ? copy.taskDone : copy.taskFailedTitle}
                </div>
              </div>
              <button
                type="button"
                data-testid="cmsd-task-modal-close"
                onClick={onDismiss}
                className="h-8 w-8 rounded-full flex items-center justify-center bg-black/[0.05] text-[#636366] hover:bg-black/[0.09] dark:bg-white/[0.08] dark:text-[#C7C7CC]"
              >
                <X size={15} />
              </button>
            </div>
            <div className="mt-2.5 max-h-40 overflow-y-auto text-[13px] leading-relaxed text-[#6C6C70] dark:text-[#8E8E93] break-all">
              {settledTask.status === 'succeeded' ? (settledTask.result || '') : (settledTask.error || '')}
            </div>
          </>
        ) : null}
      </div>
    </div>
  );
}
