import { useEffect, useState } from 'react';
import { X } from '../../components/icons.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { AcShieldCheck, AcSparkles } from './tool-common.jsx';

// 品悟角色配色（与产物卡一致）：品=盾·橙 #FF9500/#FF9F0A，悟=闪光·紫 #5E5CE6。
// 返回 { name, accentHex(inline-style 原色,品需 isDark), text(类), softBg(类), Icon }。
const pvRole = (isWu, isDark) => isWu
  ? { name: '悟', accentHex: '#5E5CE6', text: 'text-[#5E5CE6]',
      softBg: 'bg-[#5E5CE6]/[0.10] dark:bg-[#5E5CE6]/15', Icon: AcSparkles }
  : { name: '品', accentHex: isDark ? '#FF9F0A' : '#FF9500', text: 'text-[#FF9500] dark:text-[#FF9F0A]',
      softBg: 'bg-[#FF9500]/[0.10] dark:bg-[#FF9F0A]/15', Icon: AcShieldCheck };

// ==========================================
// PinvouSummonCard — 🧭 召唤式检阅（Boss 主动呼叫 Pinvou）
// 自报家门人格(单主+alternates,§3.3) + trace + issues(severity 分色)。
// ==========================================
// 逐条裁决行(§2 + kind 分流):每条按本质给对应动作,不再一刀切判断题——
//   recommendation(决策点/缺信息,Boss 才能定)→ 采纳建议 / 让 AI 问我;
//   issue.needs_verify(外部事实,AI 无知识)→ 让 AI 核实 / 我确认没问题;
//   issue 其他(产物缺陷,AI 改得动)→ 让 AI 改 / 接受现状(high 默认勾)。
// 「交给 AI 处理」按各条动作组装定向指令走 B1。单独子组件:useState 放这避 hooks 错位。
const PinvouRows = ({ review, t, role }) => {
  role = role || pvRole(false, false);
  const body = 'text-[#000] dark:text-[#fff]';
  const muted = 'text-[#3C3C43]/60 dark:text-[#EBEBF5]/60';
  // iOS 语义色：high 红 / medium 橙 / low 灰
  const sevDot = (s) => s === 'high' ? '#FF3B30' : s === 'medium' ? '#FF9500' : '#C7C7CC';
  const rows = [
    ...(review.recommendations || []).map((x, i) => ({
      k: 'r' + i, raw: x, kind: 'rec', dot: '#FF9500',
      head: (x.topic ? x.topic + '：' : '') + t.pvSuggest + x.pick, sub: x.why,
    })),
    ...(review.issues || []).map((x, i) => ({
      k: 'i' + i, raw: x, kind: x.kind === 'needs_verify' ? 'verify' : 'fix',
      dot: sevDot(x.severity), sev: x.severity, nv: x.kind === 'needs_verify',
      head: x.text, sub: x.suggestion,
    })),
    ...(review.coverage || []).map((x, i) => ({
      k: 'c' + i, raw: x, kind: 'gap', dot: '#5E5CE6',
      sev: x.severity, head: x.dimension + (x.text ? '：' + x.text : ''), sub: x.suggestion,
    })),
  ];
  // 每类二选一 [值,文案]:第一个=「要 AI 做」(高亮),第二个=Boss 自己消化(灰)。
  const ACT = {
    rec: [['adopt', t.pvActAdopt], ['ask', t.pvActAsk]],
    verify: [['verify', t.pvActVerify], ['confirmed', t.pvActConfirmed]],
    fix: [['modify', t.pvActModify], ['accept', t.pvActAccept]],
    gap: [['fill', t.pvActFill], ['skip', t.pvActSkip]],
  };
  const ACTIVE = { adopt: 1, ask: 1, verify: 1, modify: 1, fill: 1 }; // 需转交给 AI 的动作
  const [res, setRes] = useState(() => {
    const m = {};
    rows.forEach(it => {
      let def = null;
      if (it.sev === 'high') def = it.kind === 'fix' ? 'modify' : it.kind === 'gap' ? 'fill' : null;
      m[it.k] = it.raw.resolution || def;
    });
    return m;
  });
  const setOne = (k, v) => setRes(p => ({ ...p, [k]: p[k] === v ? null : v }));
  const activeCount = rows.filter(it => ACTIVE[res[it.k]]).length;
  // iOS 分段按钮风：选中且需转交 AI=填充角色色(背景走 style)；选中但自行消化=灰填充；未选=描边。
  const chip = (on, active) => `text-[12px] px-2.5 py-1 rounded-full font-medium transition-all active:scale-[0.96] ${on
    ? (active ? 'text-white border border-transparent'
              : 'bg-black/[0.08] text-[#000] border border-transparent dark:bg-white/15 dark:text-[#fff]')
    : 'border border-black/[0.12] text-[#3C3C43]/80 hover:bg-black/5 dark:border-white/15 dark:text-[#EBEBF5]/70 dark:hover:bg-white/5'}`;
  const onResolve = () => {
    if (!bridge.available) return;
    // 弹窗里 review 是 notify 深拷贝,写它的 resolution 落不到原 state;把裁决按下标传给 bridge,
    // 由 bridge 在 state.pinvouModal.review(原 state)上写、再落盘(根治 resolution 不持久化)。
    const resolutions = {
      recs: (review.recommendations || []).map((_, i) => res['r' + i] || 'pending'),
      issues: (review.issues || []).map((_, i) => res['i' + i] || 'pending'),
      coverage: (review.coverage || []).map((_, i) => res['c' + i] || 'pending'),
    };
    const actions = [];
    rows.forEach(it => {
      const a = res[it.k];
      if (a === 'modify') actions.push({ t: 'fix', text: it.head + (it.sub ? '（' + it.sub + '）' : '') });
      else if (a === 'verify') actions.push({ t: 'verify', text: it.head + (it.sub ? '（' + it.sub + '）' : '') });
      else if (a === 'adopt') actions.push({ t: 'adopt', topic: it.raw.topic || '', pick: it.raw.pick || '' });
      else if (a === 'ask') actions.push({ t: 'ask', topic: it.raw.topic || it.head });
      else if (a === 'fill') actions.push({ t: 'fill', dimension: it.raw.dimension || '', suggestion: it.raw.suggestion || '' });
    });
    bridge.interaction.resolvePinvouReview(resolutions, actions);
  };
  return (
    <div>
      <div className="space-y-2">
        {rows.map(it => {
          const decided = res[it.k];
          const passive = decided === 'accept' || decided === 'confirmed' || decided === 'skip';
          return (
            <div key={it.k} className={`rounded-[12px] px-3 py-2.5 transition-opacity ${passive ? 'opacity-40' : ''} bg-[#F2F2F7] dark:bg-white/[0.06]`}>
              <div className="flex gap-2.5">
                <span className="mt-[7px] w-[7px] h-[7px] rounded-full shrink-0" style={{ background: it.dot }} />
                <div className="flex-1 min-w-0">
                  <div className={`text-[14px] leading-relaxed ${body}`}>
                    {it.nv && <span className={`text-[10.5px] font-medium mr-1.5 px-1.5 py-px rounded-full align-[1px] bg-[#FFF8E1] text-[#B25000] dark:bg-[#FFD60A]/20 dark:text-[#FFD60A]`}>{t.pvNeedsVerify}</span>}
                    {it.head}
                  </div>
                  {it.sub && <div className={`text-[13px] mt-0.5 ${muted}`}>{it.sub}</div>}
                  <div className="flex gap-2 mt-2">
                    {ACT[it.kind].map(([v, label]) => (
                      <button key={v} onClick={() => setOne(it.k, v)} className={chip(decided === v, !!ACTIVE[v])}
                        style={decided === v && ACTIVE[v] ? { background: role.accentHex } : undefined}>{label}</button>
                    ))}
                  </div>
                </div>
              </div>
            </div>
          );
        })}
      </div>
      <div className="flex items-center gap-2 mt-4 pt-1">
        {activeCount > 0 && (
          <button onClick={onResolve}
            className="px-4 py-2 rounded-full text-[14px] font-semibold text-white active:scale-[0.97] transition-transform"
            style={{ background: role.accentHex }}>
            {t.pvHandToAi(activeCount)}
          </button>
        )}
        <button onClick={() => bridge.available && bridge.interaction.dismissPinvouReview()} title={t.pvSkipTitle}
          className={`px-4 py-2 rounded-full text-[14px] font-medium transition-colors text-[#3C3C43]/70 hover:bg-black/5 dark:text-[#EBEBF5]/70 dark:hover:bg-white/5`}>
          {t.pvSkip}
        </button>
      </div>
    </div>
  );
};

// 检阅 loading:本地模型 5-30s / 在线模型通常更快,iOS 旋转菊花 spinner + 计时 + 安抚文字,别让 Boss 干等焦虑。
const PinvouLoading = ({ isWu, isDark, t, isLocal }) => {
  const [secs, setSecs] = useState(0);
  useEffect(() => {
    const b = setInterval(() => setSecs(s => s + 1), 1000);
    return () => clearInterval(b);
  }, []);
  const role = pvRole(isWu, isDark);
  // isDark 保留——SVG circle stroke 与 pvRole 品·accentHex(inline-style 原色)仍需它。
  const muted = 'text-[#3C3C43]/60 dark:text-[#EBEBF5]/60';
  return (
    <div className="py-8 flex flex-col items-center text-center">
      {/* iOS activity spinner：底环 + 角色色弧，匀速旋转 */}
      <svg className="w-9 h-9" viewBox="0 0 24 24" fill="none" style={{ animation: 'tsSpinner 0.8s linear infinite' }}>
        <circle cx="12" cy="12" r="9" stroke={isDark ? 'rgba(255,255,255,.12)' : 'rgba(0,0,0,.08)'} strokeWidth="3" />
        <path d="M12 3a9 9 0 0 1 9 9" stroke={role.accentHex} strokeWidth="3" strokeLinecap="round" />
      </svg>
      <div className={`flex items-center gap-1.5 mt-4 text-[15px] font-semibold ${role.text}`}>
        <role.Icon className="w-[18px] h-[18px]" />
        <span>{isWu ? t.pvLoadingWu : t.pvLoadingPin}</span>
      </div>
      <div className={`text-[13px] mt-1.5 ${muted}`}>
        {isWu ? t.pvLoadingWuSub : t.pvLoadingPinSub}
        {secs > 0 && <span className="ml-1.5 tabular-nums opacity-70">{secs}s</span>}
      </div>
      <div className={`text-[12px] mt-1 ${muted}`} style={{ opacity: 0.6 }}>{t.pvLoadingHint(isLocal)}</div>
    </div>
  );
};

// 检阅结果卡（在底部 sheet 内渲染，无外层卡框；品=橙 / 悟=紫，与产物卡一致）。
export const PinvouSummonCard = ({ item, theme, t, isLocal }) => {
  const isDark = theme === 'dark';
  const isWu = !!item.coverage; // 悟=发散(coverage)；品=查错
  const role = pvRole(isWu, isDark);
  const muted = 'text-[#3C3C43]/60 dark:text-[#EBEBF5]/60';
  const body = 'text-[#000] dark:text-[#fff]';
  // isDark 保留——pvRole 品·accentHex 与 PinvouLoading SVG stroke 仍需它。
  if (item.loading) return <PinvouLoading isWu={isWu} isDark={isDark} t={t} isLocal={isLocal} />;
  if (item.error) return (
    <div className="py-2">
      <div className={`flex items-center gap-1.5 text-[15px] font-semibold ${role.text}`}><role.Icon className="w-[18px] h-[18px]" /><span>Pinvou {role.name}</span></div>
      <div className={`text-[14px] mt-2 text-[#FF3B30] dark:text-[#FF453A]`}>{t.pvFail}{item.error}</div>
    </div>
  );
  const r = item.review || {};
  if (r.dismissed) return (
    <div className={`py-2 flex items-center gap-1.5 text-[14px] ${muted}`}><role.Icon className="w-4 h-4" /><span>{'Pinvou · ' + role.name + ' · ' + t.pvSkipped}</span></div>
  );
  const personas = r.personas || [];
  const primary = personas.find(p => p && p.primary) || personas[0] || {};
  const alts = r.alternates || [];
  const hasRows = (r.recommendations || []).length > 0 || (r.issues || []).length > 0 || (r.coverage || []).length > 0;
  return (
    <div>
      <div className="flex items-center flex-wrap gap-x-2 gap-y-1 mb-2.5">
        <span className={`inline-flex items-center justify-center w-7 h-7 rounded-full ${role.softBg}`}>
          <role.Icon className={`w-[17px] h-[17px] ${role.text}`} />
        </span>
        <span className={`text-[16px] font-semibold ${body}`}>
          {'Pinvou · ' + role.name}
          {primary.label && <span className={`text-[14px] font-normal ${muted}`}> · {primary.label + t.pvPerspective}</span>}
        </span>
        {r.verdict === 'pass' && <span className={`text-[11px] font-semibold px-2 py-0.5 rounded-full bg-[#34C759]/15 text-[#248A3D] dark:bg-[#30D158]/20 dark:text-[#30D158]`}>{t.pvVerdictPass}</span>}
      </div>
      {alts.length > 0 && <div className={`text-[12px] -mt-1 mb-2 ${muted}`}>{t.pvAlsoInvolves} {alts.join(' / ')}</div>}
      {r.trace && <div className={`text-[14px] leading-relaxed mb-3 ${body}`}>{r.trace}</div>}
      {(r.framework || []).length > 0 && (
        <div className={`text-[12px] mb-3 px-3 py-2 rounded-[12px] leading-relaxed ${role.softBg} ${role.text}`}>
          <span className="opacity-70">{t.pvFramework} · {(r.framework || []).length}{t.pvDims}: </span>{(r.framework || []).join(' · ')}
        </div>
      )}
      {hasRows && <PinvouRows review={r} t={t} role={role} />}
    </div>
  );
};

export function PinvouSummonModal({ item, theme, t, isLocal }) {
  return (
    // biome-ignore lint/a11y/useKeyWithClickEvents: keyboard users close the dialog through its real close button
    // biome-ignore lint/a11y/noStaticElementInteractions: this is a pointer-only backdrop around an accessible dialog card
    <div className="fixed inset-0 z-[55] flex items-center justify-center p-6"
         style={{ background: theme === 'dark' ? 'rgba(0,0,0,.45)' : 'rgba(255,255,255,.35)', backdropFilter: 'blur(20px) saturate(140%)', WebkitBackdropFilter: 'blur(20px) saturate(140%)' }}
         onClick={() => { if (!item.loading) bridge.interaction.dismissPinvouReview(); }}>
      <div className="relative w-full max-w-[720px] overflow-hidden bg-white dark:bg-[#1C1C1E] rounded-[20px] shadow-[0_20px_60px_rgba(0,0,0,0.28)] ts-modal-in"
           onClick={(event) => event.stopPropagation()}
           style={{ fontFamily:'-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Microsoft YaHei", sans-serif' }}>
        <button onClick={() => bridge.available && bridge.interaction.dismissPinvouReview()} aria-label={t.pvSkip}
          className="absolute top-3.5 right-3.5 z-10 w-7 h-7 flex items-center justify-center rounded-full bg-black/[0.06] dark:bg-white/10 text-[#8E8E93] hover:bg-black/10 dark:hover:bg-white/15 active:scale-90 transition-colors">
          <X size={16} />
        </button>
        <div className="max-h-[90vh] overflow-y-auto custom-scrollbar px-5 pt-5 pb-6">
          <PinvouSummonCard item={item} theme={theme} t={t} isLocal={isLocal} />
        </div>
      </div>
    </div>
  );
}
