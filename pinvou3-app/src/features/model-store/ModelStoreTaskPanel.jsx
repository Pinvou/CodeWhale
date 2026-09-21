// 底部任务面板:正在跑的长操作、实时日志行、进度条、取消按钮,以及脚本要求
// 输入时就地展开的输入框。
//
// 输入框刻意不做成全局模态:面板本来就是「不阻塞界面」的设计,而且可以同时有几个
// 任务各自在等输入,模态框既要排队又容易把答案答到别的任务上去。
import { useEffect, useRef, useState } from 'react';
import { ChevronDown, ChevronRight } from '../../components/icons.jsx';
import { Spinner } from '../../components/Spinner.jsx';
import {
  canForceRemove,
  formatBytes,
  missingFormFields,
  progressPercent,
  etaSeconds,
  splitDuration,
} from './model-store-logic.js';
import { HOVER_SURFACE, INPUT_CLASS, MsButton, TEXT_DANGER, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';

const PROMPT_SURFACE = 'rounded-[14px] border border-[#E1E5EA] bg-[#F0F4F9] p-3 dark:border-[#2E3134] dark:bg-[#282A2C]';

/**
 * 把剩余秒数交给词典渲染,避免在组件里拼时间单位。
 * @param {object} copy - t.uiModelStore。
 * @param {number|null} seconds - 剩余秒数。
 * @returns {string} 已本地化的时长;算不出时返回空串。
 */
function renderEta(copy, seconds) {
  const parts = splitDuration(seconds);
  if (!parts) return '';
  if (parts.unit === 'sec') return copy.etaSec(parts.value);
  if (parts.unit === 'min') return copy.etaMin(parts.value);
  return copy.etaHour(parts.value, parts.rest);
}

/**
 * 表单式输入:一次问齐多项,答案作为一行 JSON 送回脚本。
 *
 * 双机部署要问从机 IP、用户名、密码、本机密码,一问一答会连弹四个框,而那个流程的
 * 目标恰恰是「填一次就不用管了」。
 * @returns {React.ReactElement} 表单。
 */
function MsFormPrompt({ prompt, copy, onSubmit, onCancel }) {
  const [values, setValues] = useState(() =>
    Object.fromEntries(prompt.fields.map((field) => [field.id, field.value ?? '']))
  );
  const firstRef = useRef(null);
  useEffect(() => {
    if (firstRef.current) firstRef.current.focus();
  }, [prompt]);

  const missing = missingFormFields(prompt.fields, values);
  const submit = () => {
    if (missing.length > 0) return;
    // 一行 JSON:脚本那边一次 read 就能拿全,不用担心换行把答案切断。
    onSubmit(JSON.stringify(values));
  };

  return (
    <div className={`mt-2 ${PROMPT_SURFACE}`} data-testid="model-store-prompt-form">
      <div className={`mb-2 text-[12px] font-medium ${TEXT_PRIMARY}`}>{prompt.title || copy.promptTitle}</div>
      {prompt.message ? (
        <div className={`mb-2 rounded-[10px] bg-[#FCE8E6] px-2 py-1 text-[11px] dark:bg-[#3A2321] ${TEXT_DANGER}`}>
          {prompt.message}
        </div>
      ) : null}
      <div className="space-y-2">
        {prompt.fields.map((field, index) => (
          <div key={field.id}>
            <span className={`text-[11px] ${TEXT_SECONDARY}`}>{field.label || field.id}</span>
            <input
              ref={index === 0 ? firstRef : null}
              type={field.kind === 'password' ? 'password' : 'text'}
              value={values[field.id] ?? ''}
              placeholder={field.placeholder || ''}
              className={INPUT_CLASS}
              onChange={(event) => setValues((prev) => ({ ...prev, [field.id]: event.target.value }))}
              onKeyDown={(event) => {
                if (event.key === 'Enter') submit();
              }}
            />
            {field.hint ? <div className={`mt-0.5 text-[10px] ${TEXT_SECONDARY}`}>{field.hint}</div> : null}
          </div>
        ))}
      </div>
      <div className="mt-3 flex justify-end gap-2">
        <MsButton tone="neutral" onClick={onCancel}>{copy.promptCancel}</MsButton>
        <MsButton disabled={missing.length > 0} onClick={submit} data-testid="model-store-prompt-submit">
          {copy.promptSubmit}
        </MsButton>
      </div>
    </div>
  );
}

/**
 * 脚本请求输入时,在任务卡片里就地展开的一小段表单。
 * @returns {React.ReactElement} 输入区。
 */
function MsPromptForm({ prompt, copy, onSubmit, onCancel }) {
  const [value, setValue] = useState('');
  const inputRef = useRef(null);
  useEffect(() => {
    if (inputRef.current) inputRef.current.focus();
  }, [prompt]);

  if (prompt.kind === 'form') {
    return <MsFormPrompt prompt={prompt} copy={copy} onSubmit={onSubmit} onCancel={onCancel} />;
  }

  if (prompt.kind === 'confirm') {
    return (
      <div className={`mt-2 ${PROMPT_SURFACE}`} data-testid="model-store-prompt-confirm">
        <div className={`text-[12px] ${TEXT_PRIMARY}`}>{prompt.message}</div>
        <div className="mt-2 flex justify-end gap-2">
          <MsButton tone="neutral" onClick={onCancel}>{copy.promptCancel}</MsButton>
          <MsButton tone="neutral" onClick={() => onSubmit('no')}>{copy.promptNo}</MsButton>
          <MsButton onClick={() => onSubmit('yes')}>{copy.promptYes}</MsButton>
        </div>
      </div>
    );
  }

  const submit = () => {
    if (!value.trim()) return;
    onSubmit(value);
  };

  return (
    <div className={`mt-2 ${PROMPT_SURFACE}`} data-testid="model-store-prompt-text">
      <div className={`text-[12px] ${TEXT_PRIMARY}`}>{prompt.message}</div>
      <input
        ref={inputRef}
        type={prompt.kind === 'password' ? 'password' : 'text'}
        value={value}
        className={INPUT_CLASS}
        onChange={(event) => setValue(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === 'Enter') submit();
        }}
      />
      <div className="mt-2 flex justify-end gap-2">
        <MsButton tone="neutral" onClick={onCancel}>{copy.promptCancel}</MsButton>
        <MsButton disabled={!value.trim()} onClick={submit} data-testid="model-store-prompt-submit">
          {copy.promptSubmit}
        </MsButton>
      </div>
    </div>
  );
}

/**
 * 任务卡的副标题:出错给首行错误,等输入/取消中给状态,有进度给「已下载/总量 ·
 * 网速 · 剩余」,都没有就退化成最后一行日志。
 * @param {object} task - 任务。
 * @param {object} copy - t.uiModelStore。
 * @returns {string} 副标题。
 */
function taskSubtitle(task, copy) {
  // 已取消要有自己的说法。以前后端不回 cancelled 标记，取消完只能显示成失败，
  // 用户会以为是出错了。
  if (task.status === 'cancelled') return copy.taskCancelled;
  if (task.status === 'cancelling') return copy.taskCancelling;
  if (task.prompt) return copy.taskWaitingInput;
  const percent = progressPercent(task.progress);
  if (percent === null) return task.lines[task.lines.length - 1] || copy.taskRunning;
  const eta = renderEta(copy, etaSeconds(task.progress, task.speed));
  return [
    copy.taskProgress(formatBytes(task.progress.done), formatBytes(task.progress.total)),
    task.speed ? copy.taskSpeed(formatBytes(task.speed)) : '',
    eta ? copy.taskEta(eta) : '',
  ].filter(Boolean).join(' · ');
}

/**
 * 单个任务卡。
 * @returns {React.ReactElement} 任务卡。
 */
function ModelStoreTaskCard({ task, copy, onCancel, onDismiss, onToggle, onAnswer, onForceRemove }) {
  const logRef = useRef(null);
  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [task.lines, task.expanded]);

  const isError = task.status === 'error';
  // 终态 = 不会再变了。取消完成的任务也留在面板上（用户要看得见自己停了什么），
  // 它和失败一样需要一个「关闭」按钮，也一样不该再显示进度条——只判 isError
  // 会让取消掉的卡片渲染出「取消」按钮，点一下变 cancelling 就被 disabled，
  // 卡片从此关不掉。
  const isTerminal = isError || task.status === 'cancelled';
  const percent = progressPercent(task.progress);
  const ChevronIcon = task.expanded ? ChevronDown : ChevronRight;

  return (
    <div
      data-testid="model-store-task"
      data-task-kind={task.kind}
      data-task-status={task.status}
      className="w-[360px] max-w-full overflow-hidden rounded-[16px] border border-[#E1E5EA] bg-white dark:border-[#2E3134] dark:bg-[#202124]"
      style={{ boxShadow: '0 6px 20px rgba(60,64,67,.18)' }}
    >
      <div className="flex items-start gap-2 px-4 pt-3">
        {task.status === 'running' ? <Spinner size={14} tone="brand" className="mt-1" /> : null}
        <div className="min-w-0 flex-1">
          <div className={`truncate text-[13px] font-medium ${TEXT_PRIMARY}`}>{task.title}</div>
          {isError ? (
            <div className={`mt-0.5 line-clamp-2 text-[11px] ${TEXT_DANGER}`}>{task.error.split('\n')[0]}</div>
          ) : (
            <div className={`mt-0.5 truncate text-[11px] ${TEXT_SECONDARY}`}>{taskSubtitle(task, copy)}</div>
          )}
        </div>
        <button
          type="button"
          title={task.expanded ? copy.taskCollapse : copy.taskExpand}
          aria-label={task.expanded ? copy.taskCollapse : copy.taskExpand}
          className={`shrink-0 rounded-full p-1 ${TEXT_SECONDARY} ${HOVER_SURFACE}`}
          onClick={() => onToggle(task.id)}
        >
          <ChevronIcon size={14} />
        </button>
      </div>

      {percent === null || isTerminal ? null : (
        <div className="mt-2 px-4">
          <div className="h-1.5 w-full overflow-hidden rounded-full bg-[#E1E5EA] dark:bg-[#2E3134]">
            <div
              data-testid="model-store-progress-bar"
              className="h-full rounded-full bg-[#0B57D0] transition-[width] duration-500 dark:bg-[#A8C7FA]"
              style={{ width: `${percent}%` }}
            />
          </div>
          <div className={`mt-1 text-right text-[10px] ${TEXT_SECONDARY}`}>{`${percent.toFixed(1)}%`}</div>
        </div>
      )}

      {task.prompt && !isError ? (
        <div className="px-4">
          <MsPromptForm
            prompt={task.prompt}
            copy={copy}
            onSubmit={(value) => onAnswer(task.id, value)}
            onCancel={() => onAnswer(task.id, null)}
          />
        </div>
      ) : null}

      {task.expanded ? (
        <pre
          ref={logRef}
          data-testid="model-store-task-log"
          className="mx-4 mt-2 h-40 overflow-auto whitespace-pre-wrap break-all rounded-[12px] bg-[#202124] p-2.5 text-[11px] leading-4 text-[#E3E3E3] dark:bg-[#131314]"
        >
          {(isError ? [...task.lines, '', task.error] : task.lines).join('\n') || copy.taskNoOutput}
        </pre>
      ) : null}

      <div className="flex justify-end gap-2 px-4 py-2.5">
        {/* 卸载失败时多给一个「强制从列表移除」。
            它只在这里出现,不做成常驻按钮:强制移除不可逆,而且会把磁盘上的权重
            原样留下,常驻等于给用户第二个长得像卸载的按钮。放在失败的任务卡上,
            用户刚好正对着失败日志——他是在看清了「为什么删不掉」之后才做这个
            决定的,而不是在列表里随手点到。任务一关,入口就跟着消失。
            文案也刻意不叫「卸载」:那个词已经被真删文件的操作占了。 */}
        {canForceRemove(task) ? (
          <MsButton
            tone="danger"
            onClick={() => onForceRemove(task)}
            data-testid="model-store-task-force-remove"
          >
            {copy.forceRemoveEntry}
          </MsButton>
        ) : null}
        {isTerminal ? (
          <MsButton tone="neutral" onClick={() => onDismiss(task.id)}>{copy.taskClose}</MsButton>
        ) : (
          <MsButton
            tone="neutral"
            disabled={task.status === 'cancelling'}
            onClick={() => onCancel(task.id)}
            data-testid="model-store-task-cancel"
          >
            {copy.taskCancel}
          </MsButton>
        )}
      </div>
    </div>
  );
}

/**
 * 底部任务面板。任务多的时候会顶出屏幕(真机上叠了 5 个就把窗口撑穿了),所以
 * 限高 + 内部滚动。
 * @returns {React.ReactElement|null} 面板。
 */
function ModelStoreTaskPanel({ tasks, copy, onCancel, onDismiss, onToggle, onAnswer, onForceRemove }) {
  if (tasks.length === 0) return null;
  // 有任务在等用户输入时，把面板抬到弹窗遮罩（z-[200]）之上。
  //
  // 平时面板是 z-[120]，低于遮罩——这是对的，任务卡不该盖住用户正在看的弹窗。
  // 但脚本请求输入时（双机部署要填从机 IP/账号密码），那个输入框**就在这个面板
  // 里**：此刻用户随手打开任意弹窗，输入框会被遮罩盖死，而脚本那头还在等，
  // 一直等到 PROMPT_TIMEOUT 把整次运行判死。用户看不到任何提示，只会觉得
  // 「部署莫名其妙失败了」。
  //
  // 只在等待输入时抬升，不是一直抬：那样任务卡会常驻在所有弹窗之上，挡住的
  // 面积比解决的问题还大。
  const waitingInput = tasks.some((task) => task.prompt);
  return (
    <div
      data-testid="model-store-task-panel"
      data-waiting-input={waitingInput ? 'true' : undefined}
      className={`pointer-events-auto fixed bottom-4 right-4 ${
        waitingInput ? 'z-[210]' : 'z-[120]'
      } flex max-h-[calc(100vh-120px)] flex-col items-end gap-2 overflow-y-auto overscroll-contain pl-1`}
    >
      {tasks.map((task) => (
        <ModelStoreTaskCard
          key={task.id}
          task={task}
          copy={copy}
          onCancel={onCancel}
          onDismiss={onDismiss}
          onToggle={onToggle}
          onAnswer={onAnswer}
          onForceRemove={onForceRemove}
        />
      ))}
    </div>
  );
}

export { ModelStoreTaskPanel, renderEta, taskSubtitle };
