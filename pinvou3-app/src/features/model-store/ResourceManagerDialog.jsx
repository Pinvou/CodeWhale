import { MsButton, MsDialog, MsDialogTitle, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';
import { Package, Database, Settings } from '../../components/icons.jsx';

/**
 * 资源管理：合并存储与导入、推理引擎、本地资源为单一入口。
 *
 * 设计：一个入口按钮 → 弹出菜单展示三个功能项 → 点击进入对应管理界面。
 * 不把三个完整弹窗硬塞进一个标签页（存储设置的路径配置逻辑复杂，
 * 嵌入会导致状态管理混乱）。后续可以逐步将三个面板迁移为标签页。
 */
function ResourceManagerDialog({ open, onClose, copy, theme,
  onOpenEngines, onOpenLocalResources, onOpenStorage }) {
  if (!open) return null;

  const items = [
    {
      id: 'engines',
      icon: Package,
      title: copy.navEngines,
      desc: copy.enginesIntro,
      onClick: () => { onClose(); onOpenEngines(); },
      testId: 'model-store-resource-engines',
    },
    {
      id: 'resources',
      icon: Database,
      title: copy.navLocalResources,
      desc: copy.localResIntro,
      onClick: () => { onClose(); onOpenLocalResources(); },
      testId: 'model-store-resource-local',
    },
    {
      id: 'storage',
      icon: Settings,
      title: copy.storageButton,
      desc: copy.storageHint,
      onClick: () => { onClose(); onOpenStorage(); },
      testId: 'model-store-resource-storage',
    },
  ];

  return (
    <MsDialog open={open} onClose={onClose} theme={theme} labelledBy="model-store-resource-title" testId="model-store-resource-dialog">
      <MsDialogTitle id="model-store-resource-title">{copy.resourceManager}</MsDialogTitle>
      <div className="space-y-2">
        {items.map((item) => (
          <button
            key={item.id}
            type="button"
            onClick={item.onClick}
            data-testid={item.testId}
            className="flex w-full items-start gap-3 rounded-[14px] border border-[#E1E5EA] p-4 text-left transition-colors hover:bg-slate-50 dark:border-[#2E3134] dark:hover:bg-white/5"
          >
            <item.icon size={20} className="mt-0.5 shrink-0 text-slate-500 dark:text-slate-400" />
            <div className="min-w-0">
              <div className={`text-[14px] font-semibold ${TEXT_PRIMARY}`}>{item.title}</div>
              <div className={`mt-1 line-clamp-2 text-[12px] leading-5 ${TEXT_SECONDARY}`}>{item.desc}</div>
            </div>
          </button>
        ))}
      </div>
      <div className="mt-5 flex justify-end gap-2">
        <MsButton tone="neutral" onClick={onClose}>{copy.dialogClose}</MsButton>
      </div>
    </MsDialog>
  );
}

export { ResourceManagerDialog };
