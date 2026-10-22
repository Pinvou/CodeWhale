// 「下载」筛选的集中视图：正在下载的任务卡 + 已中断的安装。
//
// 为什么是筛选而不是新页面：这个模块刚把「模型市场/我的模型」两个 Tab 合并成
// 一份列表（Tab 是实现视角的产物），再开顶层页面等于把同一条导航路径又劈开。
// 筛选胶囊是同一页面里的一个视图切换，零新增交互模式，徽标还能承担横幅之外
// 的主动提醒。
//
// 两个分区是**操作实例**的视图，不是卡片列表：进行中的来自模块级任务表，
// 已中断的来自耐久回执（IncompleteInstallPanel 原样复用，它是同一份数据、
// 同一组动作——横幅和这里不重复实现第二遍）。
import { EmptyState } from '../../components/EmptyState.jsx';
import { Package } from '../../components/icons.jsx';
import { IncompleteInstallPanel } from './IncompleteInstallPanel.jsx';
import { ModelStoreTaskCard } from './ModelStoreTaskPanel.jsx';
import { TEXT_SECONDARY } from './model-store-ui.jsx';

const EMPTY = Object.freeze([]);

// 正在下载 = 下载任务里还活着的那部分。取消完成/失败的是终态，归右下角任务
// 面板管，这里不重复摆——两处同时渲染同一张卡，用户会点两次「取消」。
const ACTIVE_STATUS = new Set(['running', 'cancelling']);

/**
 * 还在进行的下载任务。导出给徽标计数用：徽标 = 这个数 + 未完成安装数。
 * @param {Array<object>} tasks - 任务表快照。
 * @returns {Array<object>} 活跃下载任务。
 */
export function activeDownloadTasks(tasks) {
  return (tasks || []).filter((task) => task.kind === 'download' && ACTIVE_STATUS.has(task.status));
}

/**
 * 「下载」集中视图。
 * @param {object} props - 属性。
 * @param {Array<object>} props.tasks - 任务表快照。
 * @param {Array<object>} [props.installations] - 未完成安装列表。
 * @param {string|null} [props.incompleteError] - 未完成列表的读取错误。
 * @param {object} props.copy - t.uiModelStore。
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
  const empty = active.length === 0 && installations.length === 0;
  return (
    <div data-testid="model-store-download-center">
      {empty ? (
        // 空态要把「中断 ≠ 白下」这个产品事实说出来：用户对这个页面的第一印象
        // 决定了他取消下载时慌不慌。
        <EmptyState
          testId="model-store-download-empty"
          className="flex flex-col items-center py-24"
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
            installations={installations}
            error={incompleteError}
            copy={copy}
            onRetry={onRetry}
            onRefresh={onRefresh}
            isRetrying={isRetrying}
          />
        </>
      )}
    </div>
  );
}
