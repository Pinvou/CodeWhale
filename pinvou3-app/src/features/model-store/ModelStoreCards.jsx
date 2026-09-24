// 模型商店的引导态:还在问 / 这个平台没有 时显示什么。
//
// 这个文件原来还有「市场卡片」和「我的模型卡片」两个组件。2026-09-17 把两个 Tab
// 合并成一份列表之后,它们被 ModelRow.jsx 取代,已删除——留着的话下次有人搜
// 「重新部署」会同时搜到两处,改了没人用的那处然后以为修好了。
//
// 2026-09-18 模型商店从 sidecar 改成原生实现之后,引导态从五档缩成两档:原来的
// 'not-installed' / 'stopped' / 'error' 说的都是「另一个进程没装 / 没起来 / 起不来」,
// 现在没有那个进程了,也就没有「启动」按钮——按钮点了也无事可做。剩下的只有
// 「还在问」和「这个平台没有这个功能」。
import { AlertTriangle, Server } from '../../components/icons.jsx';
import { EmptyState } from '../../components/EmptyState.jsx';
import { CARD_SURFACE, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';

const GUIDE_ICONS = { loading: Server, unsupported: AlertTriangle };
const GUIDE_TITLE_KEYS = { loading: 'guideLoadingTitle', unsupported: 'guideUnsupportedTitle' };

/**
 * 模型商店不可用时的引导卡。
 *
 * 刻意不给任何按钮:两种状态用户都无事可做——「还在问」等一下就好,「这个平台没有」
 * 是编译期就定死的事实。摆一个点了没反应的按钮比不摆更糟。
 * @param {{state:'loading'|'unsupported', copy:object, theme:string, reason:string|null}} props 属性。
 * @returns {React.ReactElement} 引导卡。
 */
function StoreGuide({ state, copy, theme, reason }) {
  const isDark = theme === 'dark';
  const Icon = GUIDE_ICONS[state] || Server;
  return (
    <div
      data-testid="model-store-guide"
      data-guide-state={state}
      className={`mt-8 ${CARD_SURFACE}`}
      // isDark dynamic-value: 保留 (boxShadow)
      style={{ boxShadow: isDark ? '0 1px 3px rgba(0,0,0,.45)' : '0 1px 3px rgba(60,64,67,.12)' }}
    >
      <EmptyState
        testId="model-store-guide-empty"
        className="flex flex-col items-center px-6 py-20 max-sm:py-14"
        icon={<Icon size={28} />}
        iconClassName="mb-4 h-16 w-16 rounded-full bg-[#F0F4F9] text-[#0B57D0] dark:bg-[#282A2C] dark:text-[#A8C7FA]"
        title={copy[GUIDE_TITLE_KEYS[state]] || copy.guideLoadingTitle}
        titleClassName={`text-[18px] font-semibold ${TEXT_PRIMARY}`}
        // 「这个平台没有」的原因由后端给（它知道缺的是 docker 还是别的），不在前端
        // 重写一遍——两处各写一句，改了一处就会对不上。
        hint={state === 'unsupported' ? reason : null}
        hintClassName={`mt-2 max-w-[420px] text-[13px] leading-6 ${TEXT_SECONDARY}`}
      />
    </div>
  );
}

export { StoreGuide };
