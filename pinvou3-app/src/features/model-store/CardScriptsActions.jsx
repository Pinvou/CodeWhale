import { MsButton, TEXT_SECONDARY } from './model-store-ui.jsx';

/**
 * 卡片脚本更新入口。仅在服务器有新版脚本时由父组件渲染——用户不需要
 * 手动"检查更新"，列表页已经自动比对了服务器哈希与本地缓存。
 * 回退功能已移除（2026-10-19 用户决策）：脚本更新有全包哈希校验和原子
 * 替换保障，不需要用户手动回退到旧版本。
 */
export function CardScriptsActions({ record, copy, busy, onAction }) {
  const disabled = !!busy || !!record?.running;
  return (
    <section className="mt-5" data-testid="model-store-card-scripts">
      <h3 className="mb-2 text-[13px] font-medium">{copy.cardScriptsTitle}</h3>
      <p className={`text-[12px] ${TEXT_SECONDARY}`}>{record?.running ? copy.cardScriptsStopFirst : copy.cardScriptsHint}</p>
      <div className="mt-2 flex flex-wrap gap-2">
        <MsButton disabled={disabled} onClick={() => onAction(record, false)} data-testid="model-store-update-scripts">{copy.cardScriptsUpdate}</MsButton>
      </div>
    </section>
  );
}
