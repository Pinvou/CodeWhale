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
function EngineManagementDialog({ open, onClose, onChanged, theme }) {
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
    if (open) load();
  }, [open]);

  const remove = async () => {
    if (!confirm) return;
    setBusy(true);
    setError(null);
    try {
      await deleteInstalledEngine(confirm.engine_name, confirm.engine_version || null);
      setConfirm(null);
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
      <MsDialogTitle id="model-store-engines-title">推理引擎</MsDialogTitle>
      <p className={`mb-4 text-[13px] leading-6 ${TEXT_SECONDARY}`}>
        只显示已在本机验证存在的引擎。只有未被任何模型绑定的引擎可以删除；删除后，下次下载需要该引擎的模型时会自动重新安装。
      </p>

      {error ? <p className="mb-3 rounded-xl bg-rose-50 px-3 py-2 text-[12px] leading-5 text-rose-700 dark:bg-rose-500/10 dark:text-rose-300">{error}</p> : null}
      {loading ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>正在检查本机引擎…</p> : null}
      {!loading && engines.length === 0 ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>未发现可管理的推理引擎。</p> : null}
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
                      {engine.docker_image || '已验证本机安装'}
                    </div>
                    <div className={`mt-1 text-[12px] ${TEXT_SECONDARY}`}>
                      占用空间：{engine.size_bytes == null ? '暂无法精确统计' : formatBytes(engine.size_bytes)}
                    </div>
                    {bound.length ? <div className="mt-1 text-[12px] leading-5 text-amber-700 dark:text-amber-300">已绑定：{bound.join('、')}。请先处理这些模型。</div> : null}
                    {!bound.length && !engine.cleanup_supported ? <div className="mt-1 text-[12px] leading-5 text-amber-700 dark:text-amber-300">该引擎没有安全的独立清理方式，暂不提供自动删除。</div> : null}
                  </div>
                  <MsButton tone="danger" disabled={!canDelete || busy} onClick={() => setConfirm(engine)}>删除</MsButton>
                </div>
              </div>
            );
          })}
        </div>
      ) : null}

      {confirm ? (
        <div className="mt-4 rounded-[14px] bg-amber-50 p-3 dark:bg-amber-500/10">
          <p className={`text-[13px] leading-6 ${TEXT_PRIMARY}`}>
            删除“{confirm.engine_name}{confirm.engine_version ? ` · ${confirm.engine_version}` : ''}”？此操作会释放引擎空间；模型权重不会被删除。
          </p>
          <div className="mt-3 flex justify-end gap-2">
            <MsButton tone="ghost" disabled={busy} onClick={() => setConfirm(null)}>取消</MsButton>
            <MsButton tone="danger" busy={busy} onClick={remove}>确认删除</MsButton>
          </div>
        </div>
      ) : null}

      <div className="mt-5 flex justify-end gap-2">
        <MsButton tone="ghost" disabled={loading || busy} onClick={load}>刷新</MsButton>
        <MsButton tone="neutral" disabled={busy} onClick={onClose}>关闭</MsButton>
      </div>
    </MsDialog>
  );
}

export { EngineManagementDialog };
