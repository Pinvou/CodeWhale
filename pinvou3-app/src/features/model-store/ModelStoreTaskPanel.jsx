// 底部任务面板:正在跑的长操作、实时日志行、进度条、取消按钮,以及脚本要求
// 输入时就地展开的输入框。
//
// 输入框刻意不做成全局模态:面板本来就是「不阻塞界面」的设计,而且可以同时有几个
// 任务各自在等输入,模态框既要排队又容易把答案答到别的任务上去。
import { useCallback, useEffect, useRef, useState } from 'react';
import { ChevronDown, ChevronRight, MoreHorizontal } from '../../components/icons.jsx';
import { Spinner } from '../../components/Spinner.jsx';
import { copyClipboardText } from '../../shared/clipboard.js';
import { exportRunLog, revealRunLogsDir } from './model-store-api.js';
import {
  canForceRemove,
  canStopRetainedProcess,
  formatBytes,
  layerSummary,
  missingFormFields,
  taskProgressView,
  etaSeconds,
  splitDuration,
} from './model-store-logic.js';
import { HOVER_SURFACE, INPUT_CLASS, MsButton, TEXT_DANGER, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';

const PROMPT_SURFACE = 'rounded-[14px] border border-[#E1E5EA] bg-[#F0F4F9] p-3 dark:border-[#2E3134] dark:bg-[#282A2C]';

// 「复制详情」按钮那一瞬间的反馈。复制失败**不能谎报成功**——内容本来就是可见可手选的,
// 说一句"已复制"会让用户以为剪贴板里有东西,粘出来才发现是空的。
const COPY_LABEL_KEYS = { ok: 'copied', fail: 'copyFailed' };

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
 * 任务卡的副标题。
 *
 * # 这里**刻意不再显示「最后一行日志」**
 *
 * 原来算不出百分比时就把最后一行日志顶上来。装引擎时那一行是 docker 自己的层进度
 * (`7014a14111a1: Downloading [====>] 1.967GB/4.07GB`),十几层并行、每层每秒重绘,
 * 于是副标题在原地乱跳——用户的原话是「别说用户看得头痛,我看到都一脸懵逼」。
 *
 * 现在按可信度取:阶段名(脚本用 `LMM_STAGE=` 说的人话) → 字节进度 / 粗粒度步骤 →
 * 层汇总 → 「正在执行…」。**没有信息就说没有**,不拿一行乱码去填这个位置。原始日志
 * 一个字没少,在展开区里,还多了一个复制按钮。
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
  const view = taskProgressView(task);
  if (view?.awaitingCompletion) return copy.taskAwaitingCompletion;
  const parts = [];
  if (task.stage) parts.push(task.stage);
  if (view?.kind === 'bytes') {
    parts.push(copy.taskProgress(formatBytes(view.done), formatBytes(view.total)));
    // 「估算」这两个字不是客套:这一档的分母是 docker 已经露面的那些层之和,会随
    // pull 推进往上长,百分比因此会往回跳。不标出来,用户会以为进度条坏了。
    if (view.estimated) parts.push(copy.taskProgressEstimated);
    if (task.speed) parts.push(copy.taskSpeed(formatBytes(task.speed)));
    const eta = renderEta(copy, etaSeconds({ done: view.done, total: view.total }, task.speed));
    if (eta) parts.push(copy.taskEta(eta));
  } else if (view?.kind === 'step') {
    parts.push(copy.taskStep(view.index, view.total));
  }
  if (parts.length > 0) return parts.join(' · ');
  const layers = layerSummary(task.layers);
  if (layers) return copy.taskLayers(layers.complete, layers.count);
  return copy.taskRunning;
}

/**
 * 单个任务卡。
 * @returns {React.ReactElement} 任务卡。
 */
function ModelStoreTaskCard({ task, copy, onCancel, onDismiss, onToggle, onAnswer, onForceRemove, onStopRetained }) {
  const logRef = useRef(null);
  // 要不要跟着新行滚到底。**初值是 false**:用户的原话是「不要滚动的闪日志,这样很乱」。
  // 原来每来一行就无条件 `scrollTop = scrollHeight`,而装引擎时日志是一秒几十行——
  // 视觉上就是一片抖动的字。现在展开后停在顶部,只有用户自己滚到底部才开始跟随
  // (常见做法:跟随是"我在看最新的"这个意图的表达,而不是默认行为)。
  const followRef = useRef(false);
  useEffect(() => {
    const element = logRef.current;
    if (!element || !followRef.current) return;
    element.scrollTop = element.scrollHeight;
  }, [task.lines, task.expanded]);
  // 滚到距底部这么近就算"在看最新的"。给一点余量:whitespace-pre-wrap 下
  // scrollHeight 会有亚像素误差,严格判等于会让跟随随机失效。
  const onLogScroll = useCallback(() => {
    const element = logRef.current;
    if (!element) return;
    followRef.current = element.scrollHeight - element.scrollTop - element.clientHeight <= 24;
  }, []);

  // 复制结果就地反馈,不走全局 toast:面板本来就是"不阻塞界面"的设计,为一个按钮
  // 往下透传一个 onToast 会让任务面板多认识一个跟它无关的概念。
  const [copied, setCopied] = useState(null);
  useEffect(() => {
    if (!copied) return undefined;
    const timer = setTimeout(() => setCopied(null), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  // 要显示的错误文本。`errorKey` 是 useLmmTasks 回灌的结构化错误（那个模块按本
  // feature 的分层约定不拼文案，只给键和原文），在这里过词典；`task.error` 是
  // 轮询循环写进来的后端原文，照旧直接显示。
  const errorText = task.errorKey && copy[task.errorKey]
    ? copy[task.errorKey](task.errorDetail || '')
    : task.error;
  const isError = task.status === 'error';
  // 终态 = 不会再变了。取消完成的任务也留在面板上（用户要看得见自己停了什么），
  // 它和失败一样需要一个「关闭」按钮，也一样不该再显示进度条——只判 isError
  // 会让取消掉的卡片渲染出「取消」按钮，点一下变 cancelling 就被 disabled，
  // 卡片从此关不掉。
  const isTerminal = isError || task.status === 'cancelled';
  const view = taskProgressView(task);
  const layers = layerSummary(task.layers);
  const ChevronIcon = task.expanded ? ChevronDown : ChevronRight;
  // 展开区里能复制的全文 = 日志 + 错误原文。「撤简日志」的前提就是这个按钮:
  // 简化之后用户没法再靠肉眼从面板里抄走排查信息,必须有一个一键拿全文的出口。
  const logText = (errorText ? [...task.lines, '', errorText] : task.lines).join('\n');
  const copyLog = () => {
    copyClipboardText(logText || '').then((ok) => setCopied(ok ? 'ok' : 'fail'));
  };
  // 日志相关操作的就地反馈（成功给路径、失败给原因）。失败静静地什么都不发生的话，
  // 用户只会反复点那个按钮。
  const [logNotice, setLogNotice] = useState(null);
  // 日志相关动作收进一个二级菜单：任务卡宽度固定为 360px，而“导出诊断日志 /
  // 打开日志目录 / 复制详情 / 强制从列表移除 / 关闭”并排必然溢出。把支持工具
  // 和当前任务的主动作分层后，底栏始终只有一行，且每个按钮完整可点。
  const [toolsOpen, setToolsOpen] = useState(false);
  // 「打开日志目录」。面板上的日志被简化了、还只留最后 500 行，而磁盘上那份是完整的
  // （`~/.pinvou3/model-store/logs/`）。
  const openLogDir = () => {
    setLogNotice(null);
    setToolsOpen(false);
    revealRunLogsDir().catch((e) =>
      setLogNotice({ tone: 'error', text: copy.taskOpenLogDirFailed(String(e?.message || e)) })
    );
  };
  // 「导出日志（可外发）」：只在失败的卡片上给。
  //
  // 它存在的理由是脱敏分了两道——落盘那份留着路径和内网地址（不留就没法诊断），
  // 导出那份把它们一起抹掉。没有这个入口，"更严的那一道"就没有任何出口，用户要把
  // 失败反馈给我们时只能直接发原始日志。
  const exportLog = () => {
    setLogNotice(null);
    setToolsOpen(false);
    exportRunLog(task.runId).then(
      (result) => setLogNotice({ tone: 'ok', text: copy.taskExportLogDone(result?.path || '') }),
      (e) => setLogNotice({ tone: 'error', text: copy.taskExportLogFailed(String(e?.message || e)) })
    );
  };
  const copyDetails = () => {
    setToolsOpen(false);
    copyLog();
  };

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
          {/* 终态失败、以及「还在跑但有错要说」的情况，都走这一条红字。
              后者是取消/作答的 IPC 自己失败时回灌的：任务其实还在跑、状态是
              running，但用户必须看见「取消没有生效」，否则按钮恢复可点却不知道
              为什么。原先只判 isError，那条错误写进了 state 却没有任何出口。

              渲染的必须是 `errorText` 而不是 `task.error`：上面那三处回灌只写
              `errorKey`/`errorDetail`，`task.error` 此刻就是 `createTask` 给的
              `null`。条件判 errorText、渲染体取 task.error 的话这里是一次
              `null.split`，被 main.jsx 的 ViewErrorBoundary 兜住之后**整个模型
              商店视图**会被替换成「页面加载失败」。守卫见
              tests/model_store_task_panel_render.test.mjs。 */}
          {errorText ? (
            <div className={`mt-0.5 line-clamp-2 text-[11px] ${TEXT_DANGER}`}>{errorText.split('\n')[0]}</div>
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

      {view === null || isTerminal ? null : (
        <div className="mt-2 px-4">
          <div className="h-1.5 w-full overflow-hidden rounded-full bg-[#E1E5EA] dark:bg-[#2E3134]">
            <div
              data-testid="model-store-progress-bar"
              data-progress-kind={view.kind}
              data-progress-estimated={view.estimated ? 'true' : undefined}
              data-progress-pending={view.awaitingCompletion ? 'true' : undefined}
              role="progressbar"
              aria-label={view.awaitingCompletion ? copy.taskAwaitingCompletion : copy.taskRunning}
              aria-valuenow={view.awaitingCompletion ? undefined : view.percent}
              className={`h-full rounded-full bg-[#0B57D0] transition-[width] duration-500 dark:bg-[#A8C7FA] ${view.awaitingCompletion ? 'animate-pulse' : ''}`}
              style={{ width: view.awaitingCompletion ? '60%' : `${view.percent}%` }}
            />
          </div>
          <div className={`mt-1 text-right text-[10px] ${TEXT_SECONDARY}`}>{view.awaitingCompletion ? copy.taskAwaitingCompletion : `${view.percent.toFixed(1)}%`}</div>
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
        <div className="mx-4 mt-2">
          {/* docker pull 的几十层聚合成这**一行**,原地更新。
              原来每层每次刷新都往日志里追加一行,一个镜像十几层同时下,展开日志
              就是一片滚动的乱码 —— 用户的原话是「不要滚动的闪日志,这样很乱」。
              逐层的原文在这里被 reduceLogChunk 拦掉了,不进 task.lines。 */}
          {/* 日志操作的结果。放在这里而不是按钮标签里：报错原文可能很长（路径 +
              errno），塞进按钮会把整张卡撑变形。 */}
          {logNotice ? (
            <div
              data-testid="model-store-task-log-notice"
              className={`mb-1 line-clamp-2 text-[10px] ${
                logNotice.tone === 'error' ? TEXT_DANGER : TEXT_SECONDARY
              }`}
            >
              {logNotice.text}
            </div>
          ) : null}
          {layers ? (
            <div
              data-testid="model-store-task-layers"
              className={`mb-1 truncate text-[10px] ${TEXT_SECONDARY}`}
            >
              {copy.taskLayers(layers.complete, layers.count)}
              {layers.total > 0
                ? ` · ${copy.taskProgress(formatBytes(layers.done), formatBytes(layers.total))}`
                : ''}
            </div>
          ) : null}
          <pre
            ref={logRef}
            onScroll={onLogScroll}
            data-testid="model-store-task-log"
            className="h-40 overflow-auto whitespace-pre-wrap break-all rounded-[12px] bg-[#202124] p-2.5 text-[11px] leading-4 text-[#E3E3E3] dark:bg-[#131314]"
          >
            {logText || copy.taskNoOutput}
          </pre>
        </div>
      ) : null}

      <div className="relative flex min-w-0 flex-wrap items-center justify-end gap-2 px-4 py-2.5">
        {/* 展开时才提供日志工具；收进「更多」后不再挤坏底栏。这个菜单是支持工具，
            不是把「取消/关闭/继续处理」藏起来：用户的当前主动作始终常驻在右侧。 */}
        {task.expanded ? (
          <>
            {toolsOpen ? (
              <div
                className="absolute bottom-full right-4 z-10 mb-1 flex w-44 flex-col gap-1 rounded-[12px] border border-[#E1E5EA] bg-white p-1.5 shadow-lg dark:border-[#2E3134] dark:bg-[#282A2C]"
                role="menu"
                aria-label={copy.taskMore}
              >
                {isError && task.runId ? (
                  <button
                    type="button"
                    role="menuitem"
                    className={`w-full rounded-lg px-2.5 py-2 text-left text-[12px] font-medium ${HOVER_SURFACE} ${TEXT_PRIMARY}`}
                    onClick={exportLog}
                    data-testid="model-store-task-export-log"
                  >
                    {copy.taskExportLog}
                  </button>
                ) : null}
                <button
                  type="button"
                  role="menuitem"
                  className={`w-full rounded-lg px-2.5 py-2 text-left text-[12px] font-medium ${HOVER_SURFACE} ${TEXT_PRIMARY}`}
                  onClick={openLogDir}
                  data-testid="model-store-task-open-log-dir"
                >
                  {copy.taskOpenLogDir}
                </button>
                <button
                  type="button"
                  role="menuitem"
                  className={`w-full rounded-lg px-2.5 py-2 text-left text-[12px] font-medium ${HOVER_SURFACE} ${TEXT_PRIMARY}`}
                  onClick={copyDetails}
                  data-testid="model-store-task-copy-log"
                >
                  {copy[COPY_LABEL_KEYS[copied] ?? 'taskCopyLog']}
                </button>
              </div>
            ) : null}
            <MsButton
              tone="neutral"
              className="h-8 px-2.5 text-[12px]"
              aria-haspopup="menu"
              aria-expanded={toolsOpen}
              onClick={() => setToolsOpen((open) => !open)}
              data-testid="model-store-task-tools"
            >
              <MoreHorizontal size={15} />
              {copy.taskMore}
            </MsButton>
          </>
        ) : null}
        {/* 卸载失败时多给一个「强制从列表移除」。
            它只在这里出现,不做成常驻按钮:强制移除不可逆,而且会把磁盘上的权重
            原样留下,常驻等于给用户第二个长得像卸载的按钮。放在失败的任务卡上,
            用户刚好正对着失败日志——他是在看清了「为什么删不掉」之后才做这个
            决定的,而不是在列表里随手点到。任务一关,入口就跟着消失。
            文案也刻意不叫「卸载」:那个词已经被真删文件的操作占了。 */}
        {canStopRetainedProcess(task) && onStopRetained ? (
          <MsButton
            tone="neutral"
            className="h-8 px-2.5 text-[12px]"
            onClick={() => onStopRetained(task)}
            data-testid="model-store-task-stop-retained"
          >
            {copy.stopRetainedProcess}
          </MsButton>
        ) : null}
        {canForceRemove(task) ? (
          <MsButton
            tone="danger"
            className="h-8 px-2.5 text-[12px]"
            onClick={() => onForceRemove(task)}
            data-testid="model-store-task-force-remove"
          >
            {copy.forceRemoveEntry}
          </MsButton>
        ) : null}
        {isTerminal ? (
          <MsButton className="h-8 px-2.5 text-[12px]" tone="neutral" onClick={() => onDismiss(task.id)}>{copy.taskClose}</MsButton>
        ) : (
          <MsButton
            tone="neutral"
            className="h-8 px-2.5 text-[12px]"
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
function ModelStoreTaskPanel({ tasks, copy, onCancel, onDismiss, onToggle, onAnswer, onForceRemove, onStopRetained }) {
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
          onStopRetained={onStopRetained}
        />
      ))}
    </div>
  );
}

export { ModelStoreTaskPanel, renderEta, taskSubtitle };
