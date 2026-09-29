// 模型商店卡片的厂商品牌图标解析。
//
// 商店里的条目来自权重仓库,不是「添加模型」目录里那种我们自己登记过的服务商,
// 所以 org 字段的写法完全由上游决定:同一家厂商可能叫 Qwen、Tongyi-MAI、
// deepseek-ai,还可能压根不是厂商(unsloth 只是做量化再发布的第三方)。
// 这里把这堆脏数据收敛成一个纯函数,视图层只管拿 URL 去渲染。
//
// 纯函数、无 React、不碰 platform,能直接单测。
import nvidiaIcon from '../../brand-icons/nvidia.png';
import ornithIcon from '../../brand-icons/ornith.png';
import { BRAND_ICON_BY_VENDOR } from '../settings/model-catalog.js';

// 「添加模型」目录只收录能配成对话服务商的厂商,而商店拉的是权重仓库,会冒出
// NVIDIA、Ornith 这类只发权重的上游。这些补在本地,不去污染 settings 的目录表。
//
// 两张图都是厂商自己的标记,不是我们画的 —— 商店里所有条目一律用对应厂商的图标。
// ornith.png 的来源:`ornith-ai/Ornith-1.5-35B-A3B` 仓库里的 `assets/ornith_logo.png`
// (3648x1046 的横版锁定),只取左边那枚 1046 见方的圆形标记再缩到 128px。
// 取标记不取整条锁定,是因为图标位是 44px 见方,横版塞进去字标会糊成一道灰边;
// brand-icons 里其它几家(qwen/deepseek/minimax)存的也都是方形标记。
const EXTRA_BRAND_ICONS = { nvidia: nvidiaIcon, ornith: ornithIcon };

// Hugging Face 的组织名习惯带机构后缀(deepseek-ai、ornith-ai、Tongyi-MAI),
// 而图标表的键是裸品牌名。只削尾部,不在中间乱切,避免把 minimax 之类削坏。
const ORG_SUFFIX = /-(?:ai|mai|team|lab|labs)$/;

// 按模型名兜底时的关键字表。顺序即优先级:先判别名多的厂商,再判通用词。
// 基线抄自 MonitorView 的 monitorModelIcon(同一批本地模型,两处结论必须一致),
// 后面几条是模型商店实测遇到的:
//  - tongyi/tongyi-mai:通义的组织名与 Qwen 是同一家,图标沿用 Qwen。
//  - z-image:通义出品的图像模型,名字里既没有 qwen 也没有 tongyi,只能单列。
//  - nvidia、ornith:只发权重的上游,settings 的目录表里没有,走本地补充图标。
//    ornith 这条还兼着挡住 sourceRepoId 兜底 —— 它的权重仓库有 nvidia 转发的版本
//    (`nvidia/Ornith-1.5-35B-A3B-FP8`),只看 owner 段会认成 NVIDIA。
// 注意这里不放 unsloth:它是量化发布方而非模型厂商,同一个 org 下既有 Qwen 也有
// 别家的权重,按 org 给图标一定会张冠李戴,必须靠模型名自己说出它是谁。
const NAME_RULES = [
  { keywords: ['deepseek'], vendor: 'deepseek' },
  { keywords: ['kimi', 'moonshot'], vendor: 'kimi' },
  { keywords: ['minimax'], vendor: 'minimax' },
  { keywords: ['glm', 'chatglm', 'zhipu'], vendor: 'glm' },
  { keywords: ['qwen', 'tongyi', 'tongyi-mai', '千问', 'z-image'], vendor: 'qwen' },
  { keywords: ['nvidia'], vendor: 'nvidia' },
  { keywords: ['ornith'], vendor: 'ornith' },
];

// 「转发方」:拿别家的模型做量化或写部署配方再发布的组织。它们的 org 回答不了
// 「这是谁家的模型」—— `nvidia/Qwen3.8-Flash-Next-NVFP4` 里的模型是 Qwen 的,
// NVIDIA 只是那份 NVFP4 部署配方的作者。
//
// unsloth 天然就落空(图标表里本来没有它),NVIDIA 却有图标,不特判就会让同一个
// Qwen 家族在列表里出现两种图标 —— unsloth 那张和 Qwen 自己那张是紫色 Qwen 标,
// NVIDIA 这张是绿色 NVIDIA 标,用户会以为是三家的模型。
//
// 不能改成「把 nvidia 的图标删掉」:Nemotron 是 NVIDIA 自己的模型,那时这个图标
// 是对的。所以这里做的是**降低优先级**(排到模型名之后),不是弃用。
const REPUBLISHER_ORGS = new Set(['nvidia', 'unsloth']);

/**
 * 把上游五花八门的组织名收敛成图标表的键。
 * @param {string} [raw] - 原始组织名,例如 `Tongyi-MAI`、`H3C MegaCube 本地验证`。
 * @returns {string} 归一化后的键;无法归一化时返回空串。
 */
function normalizeVendorKey(raw) {
  return String(raw || '').toLowerCase().replaceAll(/\s+/g, '').replace(ORG_SUFFIX, '');
}

/**
 * 查两张图标表:先查 settings 的目录表,再查商店本地补充的那几家。
 * @param {string} key - 已归一化的厂商键。
 * @returns {string|null} 图标 URL,查不到返回 null。
 */
function iconForVendor(key) {
  if (!key) return null;
  // 走 hasOwn 而不是直接取下标:org 来自上游仓库元数据,叫 constructor/toString
  // 的组织虽然离谱但不是不可能,原型链上摸到个函数再当 URL 往 <img src> 里塞,
  // 排查起来比直接没图标难受得多。
  if (Object.hasOwn(BRAND_ICON_BY_VENDOR, key)) return BRAND_ICON_BY_VENDOR[key];
  if (Object.hasOwn(EXTRA_BRAND_ICONS, key)) return EXTRA_BRAND_ICONS[key];
  return null;
}

/**
 * 按模型名做关键字兜底。
 *
 * 返回值刻意区分三态:命中有图标 / 命中但图标表里查不到(null) / 没命中(undefined)。
 * 中间那态是配置错误(规则里写了个不存在的厂商键),它跟「没命中」必须分开 ——
 * 命中了就说明模型名已经报出自己是谁家的,不该再让 sourceRepoId 的 owner 段去猜。
 * @param {string} [name] - 模型展示名,例如 `Qwen3.8-27B (NVFP4 · SGLang v0.5.19)`。
 * @returns {string|null|undefined} 图标 URL、null 或 undefined。
 */
function iconForModelName(name) {
  const text = String(name || '').toLowerCase();
  // 裸 return 就是 undefined。这里的三态是有意义的：
  // 有图标 / 确认这家没有图标（null，短路后续兜底）/ 没匹配上（undefined，继续往下找）。
  if (!text) return;
  for (const rule of NAME_RULES) {
    if (rule.keywords.some(keyword => text.includes(keyword))) return iconForVendor(rule.vendor);
  }
  return;
}

/**
 * 解析模型卡片该用哪个厂商品牌图标。
 *
 * 兜底顺序按「信息可信度」排:org 是上游自己声明的厂商,最准;模型名是人写给人
 * 看的,带品牌词但也带一堆版本号噪音;仓库 ID 的 owner 段最不可信(转发/镜像/
 * 量化都会换 owner),所以放最后。
 *
 * 唯一的例外是 REPUBLISHER_ORGS:转发方声明的是「谁做的这份包」而不是「谁做的
 * 这个模型」,这一条上它反而不如模型名可信,所以要排到模型名后面去。
 * @param {object} [input] - 卡片上能拿到的标识信息。
 * @param {string} [input.org] - 发布组织,例如 `deepseek-ai`、`unsloth`。
 * @param {string} [input.name] - 模型展示名。
 * @param {string} [input.sourceRepoId] - 源仓库 ID,例如 `unsloth/Qwen3.8-27B-NVFP4`。
 * @returns {string|null} 图标 URL;全未命中返回 null,由调用方渲染降级图标。
 */
function resolveBrandIcon({ org, name, sourceRepoId } = {}) {
  const orgKey = normalizeVendorKey(org);
  const byOrg = iconForVendor(orgKey);
  if (byOrg && !REPUBLISHER_ORGS.has(orgKey)) return byOrg;

  const byName = iconForModelName(name);
  if (byName !== undefined) return byName;

  // 转发方自己也可能是模型厂商(NVIDIA 的 Nemotron)。模型名没报出别家时,它声明
  // 的 org 就是眼下最好的线索了,这时候该用它的图标。
  if (byOrg) return byOrg;

  // owner 段而不是整个 ID:`unsloth/Qwen3.8-27B-NVFP4` 的品牌线索在斜杠左边。
  return iconForVendor(normalizeVendorKey(String(sourceRepoId || '').split('/')[0]));
}

export { resolveBrandIcon };
