// 模型卡片：紧凑卡片式信息单元（V-02 视觉改造，2026-11-06）。
//
// 结构对照原型 prototype-delivery-preflight.html 的 .model-card：
//   ┌─────────────────────────────────────────┐
//   │ [icon] 模型名 [状态徽章]     [主动作按钮] │  ← 头部：图标+标题+右上角操作
//   │        用途一句话（描述首行）              │
//   ├─────────────────────────────────────────┤
//   │  引擎 · 量化 · 体积 · 上下文              │  ← 规格行
//   │ 参考性能 · 非本机实测        [查看详情]   │  ← 底部：来源+详情入口
//   └─────────────────────────────────────────┘
//
// 与旧行式布局的区别：不再横向铺一整行，而是自包含卡片——两列网格时自然并排。
// 主动作只有一个（按状态机），放右上角与图标顶部对齐；「查看详情」是次要入口。
import React from 'react';
import { Download, Play, StopCircle } from '../../components/icons.jsx';
import { ProviderIcon } from '../../components/ProviderIcon.jsx';
import { formatBytes } from './model-store-logic.js';
import { resolveBrandIcon } from './model-brand-icon.js';
import { TEXT_SECONDARY } from './model-store-ui.jsx';

// ---------- 状态徽章 ----------

const STATE_BADGE = {
  running: { key: 'statusRunning', className: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-300' },
  deployed: { key: 'statusDeployed', className: 'bg-sky-50 text-sky-700 dark:bg-sky-500/10 dark:text-sky-300' },
  downloaded: { key: 'statusPending', className: 'bg-amber-50 text-amber-700 dark:bg-amber-500/10 dark:text-amber-300' },
  incomplete: { key: 'statusIncomplete', className: 'bg-rose-50 text-rose-700 dark:bg-rose-500/10 dark:text-rose-300' },
  cleanup_required: { key: 'statusCleanupRequired', className: 'bg-rose-50 text-rose-700 dark:bg-rose-500/10 dark:text-rose-300' },
  available: { key: 'statusAvailable', className: 'bg-slate-100 text-slate-500 dark:bg-slate-800 dark:text-slate-400' },
};

// ---------- 主动作按钮 ----------
// 配色对照原型 .card-action 系列：下载蓝、部署紫蓝、启动绿、停止/清理浅红底红字、
// 进行中灰底禁用（文字+样式共同区分动作，P02 清单）。
const ACTION_STYLES = {
  download: 'bg-blue-600 hover:bg-blue-700 text-white',
  deploy: 'bg-indigo-500 hover:bg-indigo-600 text-white',
  enable: 'bg-emerald-600 hover:bg-emerald-700 text-white',
  stop: 'bg-rose-50 text-rose-600 hover:bg-rose-100 border border-rose-200 dark:bg-rose-500/10 dark:text-rose-300 dark:border-rose-500/30',
  cleanup: 'bg-rose-50 text-rose-600 hover:bg-rose-100 border border-rose-200 dark:bg-rose-500/10 dark:text-rose-300 dark:border-rose-500/30',
  busy: 'bg-slate-100 text-slate-500 dark:bg-slate-800 dark:text-slate-400',
};

// 主动作的最小宽度（对齐契约 tests/model_store_row_alignment.test.mjs：六个主动作
// 共用同一下限，长文案照常撑开）。v4 卡片按钮内边距比行式版紧凑，下限相应收窄。
const ACTION_MIN_W = 'min-w-[88px]';

// 底部「查看详情」入口的占位尺寸。没有 record 的「可下载」卡没有详情入口，但同一
// 网格里上下两张卡的右下角要对齐——占位和按钮必须共用这一个常量（对齐契约同上）。
const TRAILING_SLOT = 'inline-flex h-7 min-w-[72px] shrink-0 items-center justify-center';

function PrimaryAction({ model, copy, busyKind, onDownload, onDeploy, onEnable, onStop, onUninstall }) {
  const busy = !!busyKind;
  const label = (kind, running, idle) => (busyKind === kind ? running : idle);
  const base = 'rounded-lg px-3.5 py-1.5 text-[13px] font-semibold transition-colors disabled:opacity-55';
  // 进行中：无论哪个动作，统一灰底禁用（原型的 card-busy）。
  // 对齐契约按 BTN_PRIMARY/BTN_DANGER 两个名位检查统一的宽度模板；v4 的分动作
  // 配色在这里解析进名位——契约钉的是「宽度一致」，颜色仍按动作区分（P02）。
  const tone = (kind) => `${base} ${busy ? ACTION_STYLES.busy : ACTION_STYLES[kind]}`;

  if (model.state === 'available' || model.state === 'incomplete') {
    const resuming = model.state === 'incomplete';
    const BTN_PRIMARY = tone('download');
    return (
      <button type="button" className={`${BTN_PRIMARY} ${ACTION_MIN_W}`} disabled={busy}
        data-testid={resuming ? 'model-store-resume-download' : 'model-store-download'}
        onClick={(e) => { e.stopPropagation(); onDownload(model); }}>
        {busyKind === 'download' ? null : <Download size={14} className="mr-1 inline" />}
        {label('download', resuming ? copy.resumingDownload : copy.downloading, resuming ? copy.resumeDownload : copy.download)}
      </button>
    );
  }
  if (model.state === 'cleanup_required') {
    const BTN_DANGER = tone('cleanup');
    return (
      <button type="button" className={`${BTN_DANGER} ${ACTION_MIN_W}`} disabled={busy} data-testid="model-store-continue-cleanup"
        onClick={(e) => { e.stopPropagation(); onUninstall(model.record); }}>
        {label('uninstall', copy.cleaningUp, copy.continueCleanup)}
      </button>
    );
  }
  if (model.state === 'downloaded' || (model.state === 'deployed' && model.record?.card_update_pending)) {
    const BTN_PRIMARY = tone('deploy');
    return (
      <button type="button" className={`${BTN_PRIMARY} ${ACTION_MIN_W}`} disabled={busy} data-testid="model-store-deploy"
        onClick={(e) => { e.stopPropagation(); onDeploy(model.record); }}>
        {label('deploy', copy.deploying, model.state === 'deployed' ? copy.redeploy : copy.deploy)}
      </button>
    );
  }
  if (model.state === 'deployed') {
    const BTN_PRIMARY = tone('enable');
    return (
      <button type="button" className={`${BTN_PRIMARY} ${ACTION_MIN_W}`} disabled={busy}
        data-testid="model-store-enable"
        onClick={(e) => { e.stopPropagation(); onEnable(model.record); }}>
        {busyKind === 'enable' ? null : <Play size={13} className="mr-1 inline" />}
        {label('enable', copy.enabling, copy.enable)}
      </button>
    );
  }
  const BTN_DANGER = tone('stop');
  return (
    <button type="button" className={`${BTN_DANGER} ${ACTION_MIN_W}`} disabled={busy} data-testid="model-store-stop"
      onClick={(e) => { e.stopPropagation(); onStop(model.record); }}>
      {busyKind === 'stop' ? null : <StopCircle size={13} className="mr-1 inline" />}
      {label('stop', copy.stopping, copy.stop)}
    </button>
  );
}

// ---------- 规格行 ----------
// 原型 .card-spec：引擎/量化/体积/上下文等静态规格；性能数字不混在这里
// （归性能条——原型把参考值和规格分开，避免一处数字两种含义）。
function SpecLine({ model }) {
  const parts = [];
  if (model.org) parts.push(model.org);
  if (model.engine_name) {
    parts.push(model.engine_version ? `${model.engine_name} ${model.engine_version}` : model.engine_name);
  }
  if (model.quantization) parts.push(model.quantization);
  if (model.size_bytes) parts.push(formatBytes(model.size_bytes));
  return <p className={`text-[12px] leading-5 ${TEXT_SECONDARY}`}>{parts.join(' · ')}</p>;
}

// 性能条（参考性能三列）已按 r41 P1-2 移除：客户端 Rust 层暂无 performance
// 数据源，属无数据占位；接通后随真实数据回归。

// ---------- 卡片 ----------

function ModelRow({ model, copy, busyKind, scriptUpdate, onOpen, onDownload, onDeploy, onEnable, onStop, onUninstall, onCheckUpdate, onVersions, hasUpdate = false }) {
  // main 合并（r35）：卡片有待安装的整包更新时，状态徽标让位给「有更新」。
  const badge = model.state === 'deployed' && model.record?.card_update_pending
    ? { key: 'cardUpdatePendingBadge', className: STATE_BADGE.downloaded.className }
    : (STATE_BADGE[model.state] || STATE_BADGE.available);
  const busy = !!busyKind;
  const isDual = model.deploy_topology === 'dual';

  return (
    // biome-ignore lint/a11y/useKeyWithClickEvents: 整行是详情的快捷入口；键盘路径由模型名那个按钮承担
    // biome-ignore lint/a11y/noStaticElementInteractions: role="presentation" 的容器，交互语义由内部按钮承担
    <div
      data-testid="model-store-row"
      data-card-id={model.id}
      data-state={model.state}
      role="presentation"
      className="group flex cursor-pointer flex-col rounded-2xl border border-slate-200 bg-white p-4 transition-shadow hover:shadow-md dark:border-white/10 dark:bg-slate-900"
      onClick={() => onOpen(model)}
    >
      {/* 头部：图标 + 名称/状态/用途 + 右上角主动作（原型 .model-card-head 三列 grid） */}
      <div className="flex items-start gap-3">
        <ProviderIcon
          size="lg"
          src={resolveBrandIcon({ org: model.org, name: model.name, sourceRepoId: model.source_repo_id })}
        />
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 flex-wrap items-center gap-1.5">
            <button
              type="button"
              data-testid="model-store-row-open"
              title={copy.openDetail}
              className="min-w-0 truncate text-left text-[15px] font-semibold tracking-tight text-slate-900 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-sky-500 dark:text-white"
              onClick={(e) => { e.stopPropagation(); onOpen(model); }}
            >
              {model.name}
            </button>
            <span data-testid="model-store-row-state"
              className={`shrink-0 rounded-full px-1.5 py-0.5 text-[10px] font-semibold ${badge.className}`}>
              {copy[badge.key]}
            </span>
            {hasUpdate ? (
              <span
                data-testid="model-store-update-badge"
                className="shrink-0 rounded-full bg-red-50 px-1.5 py-0.5 text-[10px] font-semibold text-red-700 dark:bg-red-500/15 dark:text-rose-300"
              >
                {copy.startupUpdateBadge}
              </span>
            ) : null}
            {isDual ? (
              <span data-testid="model-store-row-dual"
                className="shrink-0 rounded-full bg-amber-50 px-1.5 py-0.5 text-[10px] font-semibold text-amber-700 dark:bg-amber-500/10 dark:text-amber-400">
                {copy.topologyDual}
              </span>
            ) : null}
          </div>
          {/* main 合并（r35）：整包更新检查 + 版本历史入口（仅已装机卡、非测试商店卡）。 */}
          {model.record && model.record.card_source !== 'builtin' ? (
            <div className="mt-1 flex items-center gap-3" role="presentation" onClick={(event) => event.stopPropagation()}>
              <button
                type="button"
                className={`rounded text-[12px] hover:underline disabled:opacity-40 ${hasUpdate ? 'font-semibold text-red-700 dark:text-rose-300' : 'text-blue-700 dark:text-blue-300'}`}
                disabled={busy}
                data-testid="model-store-check-card-update"
                onClick={(event) => { event.stopPropagation(); onCheckUpdate(model.record); }}
              >
                {hasUpdate ? copy.startupUpdatesView : copy.cardUpdateCheck}
              </button>
              {onVersions ? (
                <button
                  type="button"
                  className="text-[11px] text-blue-600 hover:underline dark:text-blue-400"
                  onClick={(event) => { event.stopPropagation(); onVersions(model.record); }}
                >
                  {copy.versionHistory}
                </button>
              ) : null}
            </div>
          ) : null}
          {/* 用途一句话（原型 .model-title p）：卡片数据没有描述时如实不占行。 */}
          {model.description ? (
            <p className={`mt-0.5 truncate text-[12px] leading-5 ${TEXT_SECONDARY}`} title={model.description}>
              {model.description}
            </p>
          ) : null}
        </div>
        {/* 主动作：右上角，与图标顶部对齐 */}
        <div className="shrink-0" role="presentation" onClick={(e) => e.stopPropagation()}>
          <PrimaryAction model={model} copy={copy} busyKind={busyKind}
            onDownload={onDownload} onDeploy={onDeploy} onEnable={onEnable} onStop={onStop} onUninstall={onUninstall} />
        </div>
      </div>

      {/* 规格行（头部下方独立块，原型 .card-spec） */}
      <div className="mt-2.5">
        <SpecLine model={model} />
      </div>

      {/* 底部：次级入口（来源标注已上移至性能区标题行，原型 footer 左侧为元信息——
          检查时间等数据暂无来源，不造假占位）。 */}
      <div className="mt-auto flex items-center justify-end gap-2 pt-3" role="presentation" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center gap-2">
          {model.state === 'deployed' && model.record?.needs_redeploy && !model.record.card_update_pending ? (
            <button type="button"
              className="rounded-lg border border-slate-300 px-2.5 py-1 text-[12px] font-medium text-slate-600 transition-colors hover:border-slate-400 disabled:opacity-50 dark:border-slate-600 dark:text-slate-300"
              disabled={busy}
              onClick={() => onDeploy(model.record)}>
              {busyKind === 'deploy' ? copy.deploying : copy.redeploy}
            </button>
          ) : null}
          {scriptUpdate && !model.record?.needs_redeploy ? (
            <span className="inline-flex items-center gap-1 rounded-full bg-blue-50 px-2 py-0.5 text-[11px] font-medium text-blue-700 dark:bg-blue-500/10 dark:text-blue-300"
              data-testid="model-store-script-update-badge">
              {copy.scriptUpdateBadge}
            </span>
          ) : null}
          {/* 详情入口与占位共用 TRAILING_SLOT：没装机的「可下载」卡没有详情入口
              （整卡可点+模型名按钮已覆盖），留等宽空位保住同列卡片的右下角对齐。 */}
          {model.record ? (
            <button
              type="button"
              title={copy.detail}
              aria-label={copy.detail}
              disabled={busy}
              data-testid="model-store-open-detail"
              className={`${TRAILING_SLOT} rounded-lg border border-slate-200 px-2.5 text-[12px] font-medium text-blue-600 transition-colors hover:border-blue-300 hover:bg-blue-50 disabled:opacity-40 dark:border-slate-700 dark:text-blue-400 dark:hover:border-blue-500/50 dark:hover:bg-blue-500/10`}
              onClick={() => onOpen(model)}
            >
              {copy.detail}
            </button>
          ) : (
            <span className={TRAILING_SLOT} aria-hidden="true" data-testid="model-store-trailing-spacer" />
          )}
        </div>
      </div>
    </div>
  );
}

// r26 P2-5（随 #1916 rebase 并入）：memo 包装——任务 patch 到达时（几十 chunk/s），
// 未变化的行卡不再重渲染。200 行列表下载期间从全视图重渲染降为仅变化的行卡。
const MemoizedModelRow = React.memo(ModelRow);

export { ModelRow, MemoizedModelRow };
