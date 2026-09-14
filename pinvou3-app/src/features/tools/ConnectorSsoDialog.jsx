import React from 'react';
import { createPortal } from 'react-dom';

function ConnectorSsoDialog({ state, name, copy, onCancel, onOpen, onDismiss }) {
  if (!state) return null;
  // 还没出码时的占位文案:限流等待、等上一个流程退出,都让用户知道在等什么、
  // 大约还要多久——后端节流睡眠期间弹窗已经不出现了,不能只留一个无反馈的忙碌态。
  const pendingCopy = state.previousFlow
    ? copy.sso.previousFlow
    : (state.waitSeconds ? copy.sso.throttled(state.waitSeconds) : copy.sso.generating);
  return createPortal((
    // 遮罩点击**只**收起界面(onDismiss),不取消后台登录流程——收起后扫码照样能
    // 连上;快速重开由后端单飞门闩重放同一 SSO 会话,避免二次 auth login 撞 429。
    // 不做回退到 onCancel:将来漏传 onDismiss 的调用方会静默把"点空白收起"
    // 退化成"杀掉后台流程",而弹窗里本来就有显式的取消按钮。
    <div className="fixed inset-0 z-[200] flex items-center justify-center p-4" style={{ backgroundColor: 'rgba(0,0,0,0.5)', backdropFilter: 'blur(8px)' }} onClick={onDismiss}>
      <div className="bg-white dark:bg-[#1C1C1E] rounded-3xl p-7 w-full max-w-[360px] flex flex-col items-center text-center shadow-2xl" onClick={event => event.stopPropagation()}>
        <h3 className="text-[19px] font-bold text-slate-900 dark:text-white mb-1">{copy.sso.title(name)}</h3>
        <p className="text-[13px] text-slate-500 dark:text-slate-400 mb-5">{copy.sso.hint}</p>
        {state.qr
          ? <img src={state.qr} alt={copy.sso.qrAlt(name)} className="w-52 h-52 rounded-2xl border border-slate-200 dark:border-white/10 bg-white" />
          : <div className="w-52 h-52 rounded-2xl border border-dashed border-slate-300 dark:border-white/10 flex items-center justify-center px-4 text-[13px] text-slate-500 dark:text-slate-400 bg-slate-50 dark:bg-white/5">{pendingCopy}</div>}
        {/* 还没拿到 SSO 地址(限流等待/等旧流程)时没有可打开的链接,按钮保持占位但不可点。 */}
        <button onClick={onOpen} disabled={!state.url} className="mt-4 px-6 py-2.5 rounded-full text-[14px] font-semibold bg-blue-600 hover:bg-blue-700 text-white disabled:opacity-40 disabled:cursor-not-allowed">{copy.sso.openBrowser}</button>
        <div className="flex items-center gap-1.5 mt-5 text-[13px] text-slate-500 dark:text-slate-400">
          <span className="w-2 h-2 rounded-full bg-amber-400 animate-pulse"></span> {copy.sso.waiting}
        </div>
        <button onClick={onCancel} className="mt-3 px-6 py-2 rounded-full text-[14px] font-semibold bg-slate-100 dark:bg-[#2C2C2E] text-slate-600 dark:text-slate-300">{copy.cancel}</button>
      </div>
    </div>
  ), document.body);
}

export { ConnectorSsoDialog };
