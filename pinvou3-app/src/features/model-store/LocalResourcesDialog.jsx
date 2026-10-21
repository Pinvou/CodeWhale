import { useEffect, useRef, useState } from 'react';
import { fetchLocalResources } from './model-store-api.js';
import { formatBytes } from './model-store-logic.js';
import { MsButton, MsDialog, MsDialogTitle, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';

/**
 * 查看模型商店之外已有的权重候选。
 *
 * 这里故意不直接提供删除：扫描只能判断“像权重”，不能证明归属和完整性；删除未知
 * 目录会误伤用户手工导入或其他工具使用的模型。真正的模型商店资源仍在主列表管理。
 */
function LocalResourcesDialog({ open, onClose, onStorageSettings, theme, copy }) {
  const [data, setData] = useState({ resources: [], scan_dirs: [] });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);
  // 过期响应闸门（评审 P2）：快速开关弹窗时，旧一轮扫描的慢响应不得覆盖新一轮
  // 的结果/错误态。照搬 ModelDetailDialog 的 loadToken 模式。
  const loadTokenRef = useRef(0);

  const load = async () => {
    const token = (loadTokenRef.current += 1);
    setLoading(true);
    setError(null);
    try {
      const next = await fetchLocalResources();
      if (token !== loadTokenRef.current) return;
      setData(next || { resources: [], scan_dirs: [] });
    } catch (cause) {
      if (token !== loadTokenRef.current) return;
      setError(String(cause?.message || cause));
    } finally {
      if (token === loadTokenRef.current) setLoading(false);
    }
  };

  useEffect(() => {
    if (open) load();
  }, [open]);

  const resources = Array.isArray(data.resources) ? data.resources : [];
  return (
    <MsDialog open={open} onClose={onClose} theme={theme} labelledBy="model-store-local-resources-title" testId="model-store-local-resources-dialog">
      <MsDialogTitle id="model-store-local-resources-title">{copy.localResTitle}</MsDialogTitle>
      <p className={`mb-4 text-[13px] leading-6 ${TEXT_SECONDARY}`}>
        {copy.localResIntro}
      </p>
      {error ? <p className="mb-3 rounded-xl bg-rose-50 px-3 py-2 text-[12px] leading-5 text-rose-700 dark:bg-rose-500/10 dark:text-rose-300">{error}</p> : null}
      {loading ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>{copy.localResScanning}</p> : null}
      {!loading && resources.length === 0 ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>{copy.localResEmpty}</p> : null}
      {!loading && resources.length ? (
        <div className="max-h-[48vh] space-y-2 overflow-y-auto pr-1">
          {resources.map((resource) => (
            <div key={resource.path} className="rounded-[14px] border border-[#E1E5EA] p-3 dark:border-[#2E3134]">
              <div className={`break-all font-mono text-[12px] leading-5 ${TEXT_PRIMARY}`}>{resource.path}</div>
              <div className={`mt-1 text-[12px] ${TEXT_SECONDARY}`}>
                {formatBytes(resource.bytes || 0)}{resource.removable ? ` · ${copy.localResRemovable}` : ''}
              </div>
            </div>
          ))}
        </div>
      ) : null}
      <div className="mt-5 flex flex-wrap justify-end gap-2">
        <MsButton tone="ghost" disabled={loading} onClick={load}>{copy.dialogRefresh}</MsButton>
        <MsButton tone="ghost" onClick={onStorageSettings}>{copy.localResManage}</MsButton>
        <MsButton tone="neutral" onClick={onClose}>{copy.dialogClose}</MsButton>
      </div>
    </MsDialog>
  );
}

export { LocalResourcesDialog };
