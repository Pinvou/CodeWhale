// 模型商店内部复用的视觉零件:配色常量、按钮、标签、弹窗外壳、轻提示。
//
// 为什么不直接进 components/:这些都是本模块的局部形态(卡片网格的标签、
// 长操作面板的小按钮),提到公共层只会让公共组件长出一堆只有一处用到的分支。
// 真正通用的那几个(EmptyState / Spinner / IosSearchField / IosSegmentedControl)
// 依旧直接复用公共实现。
//
// 配色按品悟调色板写死字面量:LMM 原来用的 `accent` token 和 `rounded-pill` 在
// 品悟的 Tailwind 配置里不存在,分别换成 #0B57D0 和 rounded-full;LMM 一个 dark:
// 变体都没有,这里每个颜色都补齐了暗色对应值。
import { useEffect } from 'react';
import { X } from '../../components/icons.jsx';
import { Spinner } from '../../components/Spinner.jsx';

const CARD_SURFACE = 'rounded-[20px] border border-[#E1E5EA] bg-white dark:border-[#2E3134] dark:bg-[#202124]';
const TEXT_PRIMARY = 'text-[#1F1F1F] dark:text-[#E3E3E3]';
const TEXT_SECONDARY = 'text-[#5F6368] dark:text-[#9AA0A6]';
const TEXT_ACCENT = 'text-[#0B57D0] dark:text-[#A8C7FA]';
const TEXT_DANGER = 'text-[#C5221F] dark:text-[#F28B82]';
const HOVER_SURFACE = 'hover:bg-[#E1E5EA] dark:hover:bg-[#282A2C]';
const SUBTLE_SURFACE = 'bg-[#F0F4F9] dark:bg-[#282A2C]';

// 模块内所有文本输入框共用一份规格(任务面板的脚本输入、口令框),不然同一个
// 模块里会出现两种边框和两种聚焦色。
const INPUT_CLASS =
  'mt-1 w-full rounded-[10px] border border-[#E1E5EA] bg-white px-2.5 py-1.5 text-[12px] outline-none focus:border-[#0B57D0] dark:border-[#2E3134] dark:bg-[#202124] dark:text-[#E3E3E3] dark:focus:border-[#A8C7FA]';

// 品悟的标准按钮规格。这一串在插件中心、定时任务、知识库三处**逐字相同**
// （ToolStoreView.jsx / ScheduledTasksView.jsx / KnowledgeView.jsx），是全站
// 事实上的按钮基线：h-9、rounded-full、font-semibold、shadow-sm。
// 原来这里是 h-8 / font-medium / 无阴影，矮 4px、轻一档，跟别的页面摆在一起就露馅。
const BTN_BASE =
  'inline-flex h-9 shrink-0 items-center justify-center gap-1.5 rounded-full px-4 text-[13px] font-semibold shadow-sm transition-colors disabled:cursor-not-allowed disabled:opacity-40';
const BTN_PRIMARY = `${BTN_BASE} bg-blue-600 text-white hover:bg-blue-700`;
const BTN_NEUTRAL = `${BTN_BASE} bg-slate-100 text-[#1D1D1F] hover:bg-slate-200 dark:bg-[#2C2C2E] dark:text-white dark:hover:bg-[#3A3A3C]`;
const BTN_DANGER = `${BTN_BASE} bg-slate-100 text-rose-600 hover:bg-slate-200 dark:bg-[#2C2C2E] dark:text-rose-400 dark:hover:bg-[#3A3A3C]`;
const BTN_GHOST = `${BTN_BASE} bg-transparent text-blue-600 shadow-none hover:bg-slate-100 dark:text-[#64B5F6] dark:hover:bg-[#2C2C2E]`;

const BUTTON_TONES = {
  primary: BTN_PRIMARY,
  ghost: BTN_GHOST,
  neutral: BTN_NEUTRAL,
  danger: BTN_DANGER,
};

/**
 * 模块内统一的按钮。
 *
 * 规格跟品悟其它页面对齐（见上面 BTN_BASE 的注释）。弹窗里还在用它；列表行卡
 * 直接写类名，因为那里要按状态换 tone 且不需要 busy 转圈。
 * @returns {React.ReactElement} 按钮。
 */
function MsButton({ tone = 'primary', children, className = '', busy = false, disabled = false, ...rest }) {
  return (
    <button
      type="button"
      disabled={disabled || busy}
      className={`${BUTTON_TONES[tone] || BUTTON_TONES.primary} ${className}`}
      {...rest}
    >
      {busy ? <Spinner size={12} tone={tone === 'primary' ? 'inverse' : 'brand'} /> : null}
      {children}
    </button>
  );
}

const PILL_TONES = {
  neutral: `${SUBTLE_SURFACE} ${TEXT_SECONDARY}`,
  accent: `bg-[#E8F0FE] ${TEXT_ACCENT} dark:bg-[#1F3760]`,
  ok: 'bg-[#E6F4EA] text-[#137333] dark:bg-[#1E3A2A] dark:text-[#81C995]',
  warn: 'bg-[#FEF7E0] text-[#A15C07] dark:bg-[#3D3323] dark:text-[#FDD663]',
};

/**
 * 小标签。需要额外条件/额外设备的用醒目色(warn),别让它混在普通标签里被略过。
 * @returns {React.ReactElement} 标签。
 */
function MsPill({ tone = 'neutral', children, className = '', testId }) {
  return (
    <span
      data-testid={testId}
      className={`inline-flex shrink-0 items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium ${PILL_TONES[tone] || PILL_TONES.neutral} ${className}`}
    >
      {children}
    </span>
  );
}

/**
 * 弹窗外壳。样式对齐导航层的 iOS 风确认框:半透明毛玻璃背板 + 圆角卡。
 *
 * 键盘路径靠 Escape 监听和弹窗内的关闭按钮,背板只做点击关闭。
 * @returns {React.ReactElement|null} 弹窗。
 */
function MsDialog({ open, onClose, theme, labelledBy, wide = false, testId, children }) {
  const isDark = theme === 'dark';
  useEffect(() => {
    if (!open) return () => {};
    const onKey = (event) => {
      if (event.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, onClose]);

  if (!open) return null;
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: 背板只负责点击关闭;键盘路径由 Escape 监听与弹窗内的关闭按钮承担
    <div
      role="presentation"
      className="fixed inset-0 z-[200] flex items-center justify-center p-4"
      style={{
        background: 'rgba(0,0,0,.34)',
        backdropFilter: 'blur(14px) saturate(140%)',
        WebkitBackdropFilter: 'blur(14px) saturate(140%)',
      }}
      onClick={onClose}
    >
      {/* biome-ignore lint/a11y/useKeyWithClickEvents: 弹窗主体只阻止冒泡,避免误触背板关闭;它本身不是可交互控件 */}
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={labelledBy}
        data-testid={testId}
        className={`max-h-[calc(100vh-96px)] ${wide ? 'w-[560px]' : 'w-[420px]'} max-w-[calc(100vw-48px)] overflow-y-auto rounded-[20px] bg-white p-6 dark:bg-[#202124] ${TEXT_PRIMARY}`}
        style={{
          // isDark dynamic-value: 保留 (boxShadow)
          boxShadow: isDark ? '0 24px 60px rgba(0,0,0,.55)' : '0 24px 60px rgba(0,0,0,.22)',
        }}
        onClick={(event) => event.stopPropagation()}
      >
        {children}
      </div>
    </div>
  );
}

/**
 * 弹窗标题行。
 * @returns {React.ReactElement} 标题行。
 */
function MsDialogTitle({ id, children, onClose, closeLabel }) {
  return (
    <div className="mb-4 flex items-start justify-between gap-3">
      <h2 id={id} className={`text-[18px] font-semibold leading-6 ${TEXT_PRIMARY}`}>
        {children}
      </h2>
      {onClose ? (
        <button
          type="button"
          aria-label={closeLabel}
          title={closeLabel}
          className={`-mr-1 -mt-1 shrink-0 rounded-full p-1.5 ${TEXT_SECONDARY} ${HOVER_SURFACE}`}
          onClick={onClose}
        >
          <X size={16} />
        </button>
      ) : null}
    </div>
  );
}

/**
 * 详情弹窗里的一行「标签:值」。
 * @returns {React.ReactElement} 行。
 */
function MsRow({ label, children }) {
  return (
    <div className="flex gap-3 text-[13px] leading-6">
      <span className={`w-20 shrink-0 ${TEXT_SECONDARY}`}>{label}</span>
      <span className={`min-w-0 flex-1 break-all ${TEXT_PRIMARY}`}>{children}</span>
    </div>
  );
}

/**
 * 轻提示。样式对齐导航层的 ArchiveToast:底部居中、暗色胶囊。
 * @returns {React.ReactElement|null} 提示。
 */
function MsToast({ toast }) {
  if (!toast) return null;
  return (
    <div
      data-testid="model-store-toast"
      className="pointer-events-none fixed bottom-6 left-1/2 z-[240] -translate-x-1/2 rounded-full px-4 py-2.5 text-[13px] font-medium text-white bg-[rgba(32,33,36,.94)] dark:bg-[rgba(232,234,237,.94)] dark:text-[#202124]"
      style={{ boxShadow: '0 8px 24px rgba(0,0,0,.28)' }}
    >
      {toast.text}
    </div>
  );
}

export {
  BTN_BASE,
  BTN_PRIMARY,
  BTN_NEUTRAL,
  BTN_DANGER,
  BTN_GHOST,
  CARD_SURFACE,
  TEXT_PRIMARY,
  TEXT_SECONDARY,
  TEXT_ACCENT,
  TEXT_DANGER,
  HOVER_SURFACE,
  SUBTLE_SURFACE,
  INPUT_CLASS,
  MsButton,
  MsPill,
  MsDialog,
  MsDialogTitle,
  MsRow,
  MsToast,
};
