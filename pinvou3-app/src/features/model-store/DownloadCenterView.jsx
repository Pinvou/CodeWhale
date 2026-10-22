// 「下载」筛选的集中视图：正在下载的任务卡 + 已中断的安装 + 残留文件。
//
// 为什么是筛选而不是新页面：这个模块刚把「模型市场/我的模型」两个 Tab 合并成
// 一份列表（Tab 是实现视角的产物），再开顶层页面等于把同一条导航路径又劈开。
// 筛选胶囊是同一页面里的一个视图切换，零新增交互模式，徽标还能承担横幅之外
// 的主动提醒。
//
// 前两个分区是**操作实例**的视图，不是卡片列表：进行中的来自模块级任务表，
// 已中断的来自耐久回执（IncompleteInstallPanel 原样复用，它是同一份数据、
// 同一组动作——横幅和这里不重复实现第二遍）。残留文件分区不同：它列的是
// **目录**（无库记录的半截权重），扫描显式触发、删除逐条确认——它是本页
// 唯一能真正删数据的入口，交互上必须和"只读"的两个分区拉开距离。
import { useState } from 'react';
import { EmptyState } from '../../components/EmptyState.jsx';
import { Package } from '../../components/icons.jsx';
import { IncompleteInstallPanel } from './IncompleteInstallPanel.jsx';
import { ModelStoreTaskCard } from './ModelStoreTaskPanel.jsx';
import { formatBytes } from './model-store-logic.js';
import { removeOrphan, scanOrphans } from './model-store-api.js';
import { MsButton, TEXT_DANGER, TEXT_SECONDARY } from './model-store-ui.jsx';

const EMPTY = Object.freeze([]);

// 正在下载 = 下载任务里还活着的那部分。取消完成/失败的是终态，归右下角任务
// 面板管，这里不重复摆——两处同时渲染同一张卡，用户会点两次「取消」。
// install_local（AI/MCP 代办的下载类安装）与 UI 下载按同一张卡归一
// （与 describeActiveRun 同一规则），否则同一件事出现两种形态。
const ACTIVE_STATUS = new Set(['running', 'cancelling']);
const DOWNLOAD_KINDS = new Set(['download', 'install_local']);

/**
 * 还在进行的下载任务。
 * @param {Array<object>} tasks - 任务表快照。
 * @returns {Array<object>} 活跃下载任务。
 */
export function activeDownloadTasks(tasks) {
  return (tasks || []).filter((task) => DOWNLOAD_KINDS.has(task.kind) && ACTIVE_STATUS.has(task.status));
}

/** 任务 key（`download:<cardId>` / `card:<cardId>`）→ 卡片 ID。 */
function taskCardId(task) {
  const key = task?.key || '';
  const at = key.indexOf(':');
  return at >= 0 ? key.slice(at + 1) : '';
}

/**
 * 徽标计数 = 活跃下载任务数 + 未完成安装数，**按卡片去重**：续传进行中时，
 * 同一张卡既有活跃任务又有它作为依据的失败回执，直接相加会把一次下载数成
 * 两次。有活跃任务的卡片整体由任务侧代表。
 * @param {Array<object>} tasks - 任务表快照。
 * @param {Array<object>} installations - 未完成安装列表。
 * @returns {number} 徽标数。
 */
export function downloadBadgeCount(tasks, installations) {
  const active = activeDownloadTasks(tasks);
  const activeCards = new Set(active.map(taskCardId));
  return active.length + (installations || []).filter((item) => !activeCards.has(item.cardId)).length;
}

/**
 * 残留目录的后端错误码 → 人话。与 describeIncompleteError 同一套路。
 *
 * 先查本分区的 orphanErrors；查不到回落 incompleteErrors——删除前的进程
 * 核查复用的是安装链路的那组错误码（installation_process_still_alive 等），
 * 两张表各查各的等于让用户看到一半的原始错误码。
 */
export function describeOrphanError(copy, value) {
  const detail = String(value);
  const tables = [copy.orphanErrors, copy.incompleteErrors];
  for (const table of tables) {
    const translated = Object.entries(table || EMPTY).find(([key]) => detail.includes(key))?.[1];
    if (translated) return `${translated} (${detail})`;
  }
  return detail;
}

/**
 * 残留文件分区。扫描显式触发（不自动扫：进页面就 du 上百 GB 等于惩罚打开
 * 这个视图的人）；删除两步确认，确认文案必须把「不可恢复」说出口。
 */
function OrphansSection({ copy, onToast }) {
  const [result, setResult] = useState(null);
  const [scanning, setScanning] = useState(false);
  const [requestError, setRequestError] = useState('');
  const [removeTarget, setRemoveTarget] = useState(null);
  const [removing, setRemoving] = useState(false);
  const scan = async () => {
    setScanning(true);
    setRequestError('');
    try {
      setResult(await scanOrphans());
    } catch (failure) {
      setRequestError(describeOrphanError(copy, failure));
    } finally {
      setScanning(false);
    }
  };
  const remove = async target => {
    setRemoving(true);
    setRequestError('');
    try {
      const freed = await removeOrphan(target.path);
      setRemoveTarget(null);
      onToast?.(copy.orphansDeleted(formatBytes(freed?.freedBytes ?? 0)));
      // 删完重扫一次：列表要反映删除后的世界。重扫失败就把列表整个收回
      // 未扫描态——攥着删除前的快照，用户会对着幽灵条目再点一次删除。
      try {
        setResult(await scanOrphans());
      } catch {
        setResult(null);
      }
    } catch (failure) {
      setRequestError(describeOrphanError(copy, failure));
    } finally {
      setRemoving(false);
    }
  };
  const entries = result?.entries || EMPTY;
  return <section data-testid="model-store-orphans" className="mb-8">
    <h3 className={`mb-2 px-3 text-[13px] font-bold uppercase tracking-wider ${TEXT_SECONDARY}`}>
      {copy.orphansTitle}
      {result ? <span className="ml-1.5 font-semibold opacity-60">{entries.length}</span> : null}
    </h3>
    <p className="mb-3 px-3 text-xs opacity-75">{copy.orphansHint}</p>
    {requestError && <p role="alert" className="mb-3 break-words px-3 text-xs text-red-600 dark:text-red-400">{requestError}</p>}
    {result?.unresolvedInFlight > 0 && <p role="alert" className="mb-3 break-words px-3 text-xs text-red-600 dark:text-red-400">{copy.orphansUnresolved}</p>}
    {result?.partial && <p className="mb-3 px-3 text-xs opacity-75">{copy.orphansPartial}</p>}
    {result && entries.length === 0 && <p className="px-3 text-xs opacity-75" data-testid="model-store-orphans-empty">{copy.orphansEmpty}</p>}
    {entries.map(entry => <div key={entry.path} className="mx-3 mb-2 rounded-xl border border-[#E1E5EA] p-3 text-xs dark:border-[#2E3134]">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="font-semibold">{entry.name}</div>
          <div className="break-all opacity-75">{formatBytes(entry.bytes)}</div>
        </div>
        <MsButton tone="danger" disabled={removing} data-testid="model-store-orphan-delete" onClick={() => setRemoveTarget(entry)}>{copy.orphansDelete}</MsButton>
      </div>
      {removeTarget?.path === entry.path && <div role="alertdialog" aria-modal="false" aria-label={copy.orphansDeleteConfirmTitle} data-testid="model-store-orphan-confirm" className="mt-3 rounded-lg border border-rose-400 p-3">
        <p className={`font-medium ${TEXT_DANGER}`}>{copy.orphansDeleteConfirmTitle}</p>
        <p className="mt-2 break-all font-mono opacity-75">{entry.path}</p>
        <p className="mt-2 opacity-75">{copy.orphansDeleteConfirmHint}</p>
        <div className="mt-3 flex gap-3">
          <MsButton tone="ghost" disabled={removing} onClick={() => setRemoveTarget(null)}>{copy.taskCancel}</MsButton>
          <MsButton tone="danger" busy={removing} data-testid="model-store-orphan-confirm-button" onClick={() => remove(entry)}>{copy.orphansDelete}</MsButton>
        </div>
      </div>}
    </div>)}
    <div className="px-3">
      <MsButton tone="neutral" busy={scanning} disabled={removing} data-testid="model-store-orphan-scan" onClick={scan}>
        {scanning ? copy.orphansScanning : copy.orphansScan}
      </MsButton>
    </div>
  </section>;
}

/**
 * 「下载」集中视图。
 * @param {object} props - 属性。
 * @param {Array<object>} props.tasks - 任务表快照。
 * @param {Array<object>} [props.installations] - 未完成安装列表。
 * @param {string|null} [props.incompleteError] - 未完成列表的读取错误。
 * @param {object} props.copy - t.uiModelStore。
 * @param {(message: string) => void} props.onToast - 轻提示（删除完成等）。
 * @param {(...args: unknown[]) => void} props.onCancel - 任务卡取消。
 * @param {(...args: unknown[]) => void} props.onDismiss - 任务卡关闭。
 * @param {(...args: unknown[]) => void} props.onToggle - 任务卡展开。
 * @param {(...args: unknown[]) => void} props.onAnswer - 任务卡应答。
 * @param {(...args: unknown[]) => void} props.onForceRemove - 任务卡强制移除。
 * @param {(preview: object) => Promise<void>} props.onRetry - 中断安装续传。
 * @param {() => void} props.onRefresh - 刷新未完成列表。
 * @param {(cardId: string) => boolean} props.isRetrying - 该卡是否有任务在跑。
 * @returns {React.ReactElement} 视图。
 */
export function DownloadCenterView({
  tasks,
  installations = EMPTY,
  incompleteError = null,
  copy,
  onToast,
  onCancel,
  onDismiss,
  onToggle,
  onAnswer,
  onForceRemove,
  onRetry,
  onRefresh,
  isRetrying,
}) {
  const active = activeDownloadTasks(tasks);
  // 有活跃任务的卡片由「正在下载」分区代表：它的失败回执（续传的依据）或
  // 未终结行不再重复出现在「已中断」，一页两次同样让人分不清哪个是现状。
  const activeCards = new Set(active.map(taskCardId));
  const interrupted = installations.filter((item) => !activeCards.has(item.cardId));
  // 列表读取失败时不能走空态：EmptyState 没有错误行，incompleteError 会
  // 被整个吞掉。让 IncompleteInstallPanel 渲染，由它展示 role=alert 的错误。
  const empty = active.length === 0 && interrupted.length === 0 && !incompleteError;
  return (
    <div data-testid="model-store-download-center">
      {empty ? (
        // 空态要把「中断 ≠ 白下」这个产品事实说出来：用户对这个页面的第一印象
        // 决定了他取消下载时慌不慌。残留分区照常渲染——它有自己的扫描开关，
        // 和「有没有任务在跑」是两件事。
        <EmptyState
          testId="model-store-download-empty"
          className="flex flex-col items-center py-16"
          icon={<Package size={28} />}
          iconClassName="mb-4 flex h-16 w-16 items-center justify-center rounded-full bg-slate-100 text-slate-400 dark:bg-slate-800"
          title={copy.downloadsEmptyTitle}
          titleClassName="mb-2 text-xl font-semibold text-slate-800 dark:text-slate-200"
          hint={copy.downloadsEmptyHint}
          hintClassName="text-slate-500 dark:text-slate-400"
        />
      ) : (
        <>
          {active.length > 0 ? (
            <section className="mb-8" data-testid="model-store-downloading">
              <h3 className={`mb-2 px-3 text-[13px] font-bold uppercase tracking-wider ${TEXT_SECONDARY}`}>
                {copy.downloadsTitle}
                <span className="ml-1.5 font-semibold opacity-60">{active.length}</span>
              </h3>
              <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
                {active.map((task) => (
                  <ModelStoreTaskCard
                    key={task.id}
                    inline
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
            </section>
          ) : null}
          <IncompleteInstallPanel
            installations={interrupted}
            error={incompleteError}
            copy={copy}
            onRetry={onRetry}
            onRefresh={onRefresh}
            isRetrying={isRetrying}
          />
        </>
      )}
      <OrphansSection copy={copy} onToast={onToast} />
    </div>
  );
}
