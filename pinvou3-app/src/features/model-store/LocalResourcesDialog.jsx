import { useEffect, useState } from 'react';
import { fetchLocalResources } from './model-store-api.js';
import { formatBytes } from './model-store-logic.js';
import { MsButton, MsDialog, MsDialogTitle, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';

/**
 * 查看模型商店之外已有的权重候选。
 *
 * 这里故意不直接提供删除：扫描只能判断“像权重”，不能证明归属和完整性；删除未知
 * 目录会误伤用户手工导入或其他工具使用的模型。真正的模型商店资源仍在主列表管理。
 */
function LocalResourcesDialog({ open, onClose, onStorageSettings, theme }) {
  const [data, setData] = useState({ resources: [], scan_dirs: [] });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await fetchLocalResources();
      setData(next || { resources: [], scan_dirs: [] });
    } catch (cause) {
      setError(String(cause?.message || cause));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (open) load();
  }, [open]);

  const resources = Array.isArray(data.resources) ? data.resources : [];
  return (
    <MsDialog open={open} onClose={onClose} theme={theme} labelledBy="model-store-local-resources-title" testId="model-store-local-resources-dialog">
      <MsDialogTitle id="model-store-local-resources-title">本地资源</MsDialogTitle>
      <p className={`mb-4 text-[13px] leading-6 ${TEXT_SECONDARY}`}>
        这里列出模型商店在指定目录中发现的权重候选。下载时会由模型卡片再次核验是否属于该模型且文件完整；未知资源不会被自动删除。
      </p>
      {error ? <p className="mb-3 rounded-xl bg-rose-50 px-3 py-2 text-[12px] leading-5 text-rose-700 dark:bg-rose-500/10 dark:text-rose-300">{error}</p> : null}
      {loading ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>正在扫描本地资源…</p> : null}
      {!loading && resources.length === 0 ? <p className={`py-6 text-center text-[13px] ${TEXT_SECONDARY}`}>在当前扫描目录中未发现权重候选。</p> : null}
      {!loading && resources.length ? (
        <div className="max-h-[48vh] space-y-2 overflow-y-auto pr-1">
          {resources.map((resource) => (
            <div key={resource.path} className="rounded-[14px] border border-[#E1E5EA] p-3 dark:border-[#2E3134]">
              <div className={`break-all font-mono text-[12px] leading-5 ${TEXT_PRIMARY}`}>{resource.path}</div>
              <div className={`mt-1 text-[12px] ${TEXT_SECONDARY}`}>
                {formatBytes(resource.bytes || 0)}{resource.removable ? ' · 可移动介质' : ''}
              </div>
            </div>
          ))}
        </div>
      ) : null}
      <div className="mt-5 flex flex-wrap justify-end gap-2">
        <MsButton tone="ghost" disabled={loading} onClick={load}>刷新</MsButton>
        <MsButton tone="ghost" onClick={onStorageSettings}>管理扫描目录</MsButton>
        <MsButton tone="neutral" onClick={onClose}>关闭</MsButton>
      </div>
    </MsDialog>
  );
}

export { LocalResourcesDialog };
