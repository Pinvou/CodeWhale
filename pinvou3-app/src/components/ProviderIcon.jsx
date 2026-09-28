// 厂商品牌图标的圆角贴片。
//
// 这份实现是从 SettingsView.jsx 里那个同名私有组件提取出来的共享版本:视觉规格
// (白底 + 极淡描边、kimi 的深底特例、无图标时降级成 Cpu)一比一照搬,只额外把
// 尺寸从 compact 布尔量升级成三档,好让模型商店的 64px 卡片图标也能复用同一套贴片。
// SettingsView 目前仍在用它自己的私有副本,迁移是独立的一步 —— 那一步要连带处理
// 它的 preset/vendor/providerKind 解析与 coding-plan 的 Code 图标分支,和本次
// 「给模型商店提供图标能力」是两件事,混在一起改会让回归范围失控。
//
// 本组件只负责「把一个已经解析好的图标 URL 画出来」,不负责解析。谁用谁自己决定
// 图标从哪来(settings 走 BRAND_ICON_BY_PRESET/VENDOR,模型商店走 resolveBrandIcon),
// 因为两边的输入形状完全不同,硬塞进一个组件只会催生一堆互斥的 props。
import { Cpu } from './icons.jsx';

// 三档尺寸。box/image 的具体数值来自两处既有设计:sm/md 抄 SettingsView 的
// compact/非 compact,lg 抄 ToolStoreView 的 TsToolIcon 卡片图标(64px)。
// 描边也跟着尺寸走:小尺寸贴在列表行里,要靠 black/[0.08] 把白底从白卡里托出来;
// 64px 卡片自带 shadow-sm,描边就得压到 black/5,否则边框会抢过 logo 本身。
const SIZE_SPECS = {
  sm: { box: 'h-8 w-8 rounded-[9px]', image: 'h-6 w-6', border: 'border border-black/[0.08] dark:border-transparent', fallbackSize: 18 },
  md: { box: 'h-9 w-9 rounded-[10px]', image: 'h-7 w-7', border: 'border border-black/[0.08] dark:border-transparent', fallbackSize: 20 },
  lg: { box: 'h-16 w-16 rounded-[16px] shadow-sm', image: 'h-11 w-11', border: 'border border-black/5 dark:border-white/5', fallbackSize: 30 },
};

// 降级贴片的配色。沿用 iOS 系统蓝的 tint 而不是灰:没有品牌图标不代表这个条目
// 是禁用态,灰底会读成「不可用」。
const FALLBACK_SURFACE = 'bg-[#007AFF]/10 text-[#007AFF] dark:bg-[#0A84FF]/18 dark:text-[#64B5F6]';

/**
 * 品牌图标贴片:有图标画图标,没有就画降级图标。
 * @param {object} props - 组件入参。
 * @param {string|null} [props.src] - 已解析好的图标 URL;falsy 时走降级分支。
 * @param {'sm'|'md'|'lg'} [props.size] - 尺寸档位,缺省由 compact 推导(sm=32/md=36/lg=64)。
 * @param {boolean} [props.compact] - SettingsView 旧签名的兼容入口,等价于 size='sm'。
 * @param {string} [props.vendor] - 厂商键,仅用于判定是否需要深底(目前只有 kimi)。
 * @param {boolean} [props.darkBacked] - 显式覆盖深底判定,给 vendor 未知但已知需要深底的调用方。
 * @param {React.ComponentType<{size:number,strokeWidth:number}>} [props.fallbackIcon] - 无图标时画的降级图标。
 * @param {string} [props.className] - 追加到外层贴片上的类名(定位、flex-shrink 等)。
 * @returns {React.ReactElement} 固定尺寸的方形贴片。
 */
export function ProviderIcon({
  src,
  size,
  compact = false,
  vendor,
  darkBacked,
  fallbackIcon: FallbackIcon = Cpu,
  className = '',
}) {
  const spec = SIZE_SPECS[size] || SIZE_SPECS[compact ? 'sm' : 'md'];
  // kimi 的 logo 是深色描边的蓝白图形,压在白底上会糊成一团,品牌规范也是深底。
  // 调用方能给出 vendor 就按 vendor 判,给不出就允许直接拍板。
  const onDarkPlate = darkBacked === undefined ? vendor === 'kimi' : darkBacked;

  if (!src) {
    return (
      <span className={`${spec.box} shrink-0 flex items-center justify-center overflow-hidden ${FALLBACK_SURFACE} ${className}`}>
        <FallbackIcon size={spec.fallbackSize} strokeWidth={2.2} />
      </span>
    );
  }

  const surface = onDarkPlate ? 'bg-[#111827]' : `bg-white ${spec.border}`;
  return (
    <span className={`${spec.box} shrink-0 flex items-center justify-center overflow-hidden ${surface} ${className}`}>
      <img src={src} alt="" className={`${spec.image} object-contain`} loading="lazy" />
    </span>
  );
}
