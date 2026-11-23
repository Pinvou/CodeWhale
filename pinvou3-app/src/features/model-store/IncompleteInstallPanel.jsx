import { useState } from 'react';
import { dismissIncomplete, previewIncomplete } from './model-store-api.js';
import { MsButton, TEXT_DANGER } from './model-store-ui.jsx';

const EMPTY = Object.freeze([]);

export function describeIncompleteError(copy, value) {
  const detail = String(value);
  const translated = Object.entries(copy.incompleteErrors).find(([key]) => detail.includes(key))?.[1];
  return translated ? `${translated} (${detail})` : detail;
}

export function IncompleteInstallPanel({ installations = EMPTY, error, copy, onRetry, onRefresh, isRetrying }) {
  const [preview, setPreview] = useState(null);
  const [loading, setLoading] = useState(false);
  const [requestError, setRequestError] = useState('');
  // 两步确认停在「哪一条被点了放弃」上。放弃不可逆（这条记录从此不再提示、
  // 也不能重试），但后果又不涉及任何文件——所以用就地确认条而不是再弹一层框：
  // 用户要防的只是手滑，不是没看清后果。
  const [confirmingId, setConfirmingId] = useState(null);
  const [dismissing, setDismissing] = useState(false);
  // 库快照可能先于任务状态刷新，旧宿主也可能把尚在下载的回执列为未完成。
  // 已确认活跃的非终态任务归正常任务卡；终态失败和无运行归属的记录仍保留。
  const visibleInstallations = installations.filter(item => item.terminal || !isRetrying?.(item.cardId));
  const confirmingItem = visibleInstallations.find(item => item.runId === confirmingId);
  if (visibleInstallations.length === 0 && !error) return null;
  const describe = value => describeIncompleteError(copy, value);
  const inspect = async item => {
    setLoading(true);
    setRequestError('');
    setPreview(null);
    try {
      setPreview(await previewIncomplete(item.runId));
    } catch (failure) {
      setRequestError(describe(failure));
    } finally {
      setLoading(false);
    }
  };
  const dismiss = async item => {
    // 确认条打开期间列表可能被整表刷新（同卡卸载完成/新失败顶掉），这条记录
    // 已经不在了：安静收回确认条，而不是对着 undefined 崩掉组件树。
    if (!item) {
      setConfirmingId(null);
      return;
    }
    // 确认期间也可能开始了同卡重试，不能再放弃它所依赖的原始回执。
    if (isRetrying?.(item.cardId)) return;
    setDismissing(true);
    setRequestError('');
    try {
      await dismissIncomplete(item.runId);
      // 面板内容归父组件所有，放弃成功后由它重拉一次列表，这条就会消失。
      onRefresh?.();
    } catch (failure) {
      setRequestError(describe(failure));
    } finally {
      setDismissing(false);
      setConfirmingId(null);
    }
  };
  return <section data-testid="incomplete-installations" className="mx-3 mb-5 rounded-2xl border border-amber-300/50 bg-amber-50/60 p-4 dark:bg-amber-950/20">
    <h3 className="text-sm font-semibold">{copy.incompleteTitle}</h3>
    <p className="mt-1 text-xs opacity-75">{copy.incompleteHint}</p>
    {(error || requestError) && <p role="alert" className="mt-2 break-words text-xs">{requestError || describe(error)}</p>}
    {visibleInstallations.map(item => <div key={item.runId} className="mt-3 flex flex-wrap items-center justify-between gap-3">
      <div className="min-w-0 text-xs">
        <div className="font-semibold">{item.cardId}</div>
        <p className="mt-1 break-all opacity-75">{item.runId}</p>
        {item.error && <p className="mt-1 break-words">{describe(item.error)}</p>}
        {!item.terminal && <p className="mt-1 opacity-75">{copy.incompletePending}</p>}
        {item.terminal && !item.canPreview && <p className="mt-1 opacity-75">{copy.incompleteUnknownSource}</p>}
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {/* 只放已终结的记录：还在跑的任务没有「放弃」语义，后端也会拒绝。 */}
        {item.terminal && <MsButton tone="ghost" data-testid="incomplete-dismiss-button" data-run-id={item.runId}
          disabled={loading || dismissing || Boolean(confirmingId && confirmingId !== item.runId) || isRetrying?.(item.cardId)}
          onClick={() => setConfirmingId(item.runId)}>{copy.incompleteDismiss}</MsButton>}
        <MsButton disabled={loading || !item.canPreview || isRetrying?.(item.cardId)} onClick={() => inspect(item)}>{copy.incompletePreview}</MsButton>
      </div>
    </div>)}
    {confirmingId && <div role="alertdialog" aria-modal="false" aria-label={copy.incompleteDismiss} data-testid="incomplete-dismiss-confirm" className="my-3 rounded-xl border border-amber-500 p-3 text-xs">
      <p className={`font-medium ${TEXT_DANGER}`}>{copy.incompleteDismissConfirmTitle}</p>
      <p className="mt-2 break-all font-semibold">{confirmingItem?.cardId}</p>
      <p className="mt-1 break-all font-mono">{confirmingId}</p>
      <p className="mt-2 opacity-75">{copy.incompleteDismissConfirmHint}</p>
      <div className="mt-3 flex gap-3">
        <MsButton tone="ghost" disabled={dismissing} onClick={() => setConfirmingId(null)}>{copy.taskCancel}</MsButton>
        <MsButton disabled={!confirmingItem || dismissing || isRetrying?.(confirmingItem.cardId)} busy={dismissing} data-testid="incomplete-dismiss-confirm-button"
          onClick={() => dismiss(confirmingItem)}>{copy.incompleteDismiss}</MsButton>
      </div>
    </div>}
    {preview && <div role="alertdialog" aria-modal="false" aria-label={copy.incompletePreview} className="my-3 rounded-xl border border-amber-500 p-3 text-xs">
      <p className="font-semibold">{preview.cardName || preview.cardId}</p>
      <p className="mt-2">{preview.kind === 'download' ? copy.incompleteDownloadOnly : copy.incompleteLocalDeploy}</p>
      <p className="mt-2">{copy.incompleteConfirmHint}</p>
      {preview.legacyContext && <p className="mt-2">{copy.incompleteLegacy}</p>}
      <p className="mt-2 break-all">{copy.incompletePackage} {preview.packageSha256}</p>
      <p className="mt-2 break-all">{copy.incompleteStorage} {preview.modelsDir} · {preview.stagingDir}</p>
      <div className="mt-3 flex gap-3">
        <MsButton disabled={loading || isRetrying?.(preview.cardId)} onClick={() => {
          const acceptedPreview = preview;
          setPreview(null);
          onRetry(acceptedPreview);
        }}>{copy.incompleteRetry}</MsButton>
        <MsButton tone="ghost" onClick={() => setPreview(null)}>{copy.taskCancel}</MsButton>
      </div>
    </div>}
    <MsButton tone="ghost" className="mt-3" disabled={loading} onClick={onRefresh}>{copy.refresh}</MsButton>
  </section>;
}
