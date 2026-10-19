import { useEffect, useState } from 'react';
import { deleteInstalledEngine, fetchInstalledEngines } from './model-store-api.js';
import { formatBytes } from './model-store-logic.js';
import { MsButton, MsDialog, MsDialogTitle, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';

/**
 * 推理引擎是共享资源，不跟某张模型卡片混在一起管理。
 *
 * 后端已经过滤了“数据库说装过、实体早没了”的遗留记录；删除仍在后端复核一次，
 * 防止弹框打开后另一个模型刚好下载完成、把它重新绑定到同一引擎。
 */
function EngineManagementDialog({ open, onClose, onChanged, theme, copy }) {
  const [engines, setEngines] = useState([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);
  const [confirm, setConfirm] = useState(null);
  const [busy, setBusy] = useState(false);

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const list = await fetchInstalledEngines();
      setEngines(Array.isArray(list) ? list : []);
    } catch (cause) {
      setError(String(cause?.message || cause));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (open) {
      setDeleted(null);
      load();
    }
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps

  const [deleted, setDeleted] = useState(null);
  const remove = async () => {
    if (!confirm) return;
    setBusy(true);
    setError(null);
    try {
      const result = await deleteInstalledEngine(confirm.engine_name, confirm.engine_version || null);
      setConfirm(null);
      setDeleted(result || {});
      await load();
      onChanged?.();
    } catch (cause) {
      setError(String(cause?.message || cause));
    } finally {
      setBusy(false);
    }
  };

  return (
    <MsDialog open={open} onClose={onClose} theme={theme} labelledBy="model-store-engines-title" testId="model-store-engines-dialog">
      <MsDialogTitle id="model-store-engines-title">{copy.enginesTitle}</MsDialogTitle>
      <p className={`mb-4 text-[13px] leading-6 ${TEXT_SECONDARY}`}>
        {copy.enginesIntro}
      </p>

      {error ? <p className="mb-3 rounded-xl bg-rose-50 px-3 py-2 text-[12px] leading-5 text-rose-700 dark:bg-rose-500/10 dark:text-rose-300">{error}</p> : null}
      {loading ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>{copy.enginesChecking}</p> : null}
      {!loading && engines.length === 0 ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>{copy.enginesEmpty}</p> : null}
      {!loading && engines.length ? (
        <div className="max-h-[48vh] space-y-2 overflow-y-auto pr-1">
          {engines.map((engine) => {
            const bound = engine.affected_models || [];
            const canDelete = !!engine.cleanup_supported && bound.length === 0;
            return (
              <div key={`${engine.engine_name}:${engine.engine_version || ''}`} className="rounded-[14px] border border-[#E1E5EA] p-3 dark:border-[#2E3134]">
                <div className="flex min-w-0 items-start justify-between gap-3">
                  <div className="min-w-0">
                    <div className={`truncate text-[14px] font-semibold ${TEXT_PRIMARY}`}>
                      {engine.engine_name}{engine.engine_version ? ` · ${engine.engine_version}` : ''}
                    </div>
                    <div className={`mt-1 break-all text-[11px] leading-5 ${TEXT_SECONDARY}`}>
                      {engine.docker_image || copy.enginesVerifiedLocal}
                    </div>
                    <div className={`mt-1 text-[12px] ${TEXT_SECONDARY}`}>
                      {copy.enginesSizeLabel}{engine.size_bytes == null ? copy.enginesSizeUnknown : formatBytes(engine.size_bytes)}
                    </div>
                    {bound.length ? <div className="mt-1 text-[12px] leading-5 text-amber-700 dark:text-amber-300">{copy.enginesBound(bound.join(copy.listJoin))}</div> : null}
                    {!bound.length && !engine.cleanup_supported ? <div className="mt-1 text-[12px] leading-5 text-amber-700 dark:text-amber-300">{copy.enginesNoCleanup}</div> : null}
                  </div>
                  <MsButton tone="danger" disabled={!canDelete || busy} onClick={() => setConfirm(engine)}>{copy.enginesDelete}</MsButton>
                </div>
              </div>
            );
          })}
        </div>
      ) : null}

      {deleted && !confirm ? (
        <div className="mt-4 rounded-[14px] bg-emerald-50 p-3 dark:bg-emerald-500/10" data-testid="model-store-engines-deleted">
          <p className={`text-[13px] leading-6 ${TEXT_PRIMARY}`}>
            {copy.enginesDeleted(
              deleted.freed_bytes ? formatBytes(deleted.freed_bytes) : '—',
              deleted.purged_model_data,
            )}
          </p>
        </div>
      ) : null}

      {confirm ? (
        <div className="mt-4 rounded-[14px] bg-amber-50 p-3 dark:bg-amber-500/10">
          <p className={`text-[13px] leading-6 ${TEXT_PRIMARY}`}>
            {confirm.engine_name === 'ollama'
              ? copy.enginesConfirmOllama(confirm.engine_name)
              : copy.enginesConfirm(`${confirm.engine_name}${confirm.engine_version ? ` · ${confirm.engine_version}` : ''}`)}
          </p>
          <div className="mt-3 flex justify-end gap-2">
            <MsButton tone="ghost" disabled={busy} onClick={() => setConfirm(null)}>{copy.dialogCancel}</MsButton>
            <MsButton tone="danger" busy={busy} onClick={remove}>{copy.enginesConfirmDelete}</MsButton>
          </div>
        </div>
      ) : null}

      <div className="mt-5 flex justify-end gap-2">
        <MsButton tone="ghost" disabled={loading || busy} onClick={load}>{copy.dialogRefresh}</MsButton>
        <MsButton tone="neutral" disabled={busy} onClick={onClose}>{copy.dialogClose}</MsButton>
      </div>
    </MsDialog>
  );
}

export { EngineManagementDialog };
