// 合并后的模型行卡：一个模型一行，不分「市场」和「我的模型」。
//
// 形态照品悟插件中心（features/tools/ToolStoreView.jsx 的工具行）来：
// 64px 图标 + 标题/副标题/徽章 + 右侧动作按钮，两列网格，行间 border-b 分隔。
// 原来那套方块网格卡是从 LMM 桌面端搬过来的，跟品悟其它页面对不上——用户会把
// 模型商店和插件中心放在一起看，两个商店长得不一样是最扎眼的。
//
// 两处刻意的取舍：
//
// 1. **副标题放参数而不是描述。** 描述是一段一两百字的散文，在一行里只能截断成
//    半句废话。用户在列表阶段真正要判断的是「这东西跑不跑得起来、占多大」——
//    引擎、量化、体积、单机还是双机。描述留给详情页。
// 2. **动作按钮按状态只出一个主动作。** 一个模型在任意时刻只有一件"下一步该做的
//    事"：没下就下载、下完就部署、部署好就启用、跑着就停用。把四个按钮同时摆出来
//    再灰掉三个，是把状态机的复杂度甩给用户。
import { Download, Play, StopCircle, Trash2 } from '../../components/icons.jsx';
import { ProviderIcon } from '../../components/ProviderIcon.jsx';
import { formatBytes } from './model-store-logic.js';
import { resolveBrandIcon } from './model-brand-icon.js';
import { BTN_DANGER, BTN_NEUTRAL, BTN_PRIMARY } from './model-store-ui.jsx';

// 主动作按钮的最小宽度。四个主动作里只有「部署」不带图标，不兜底的话它会比
// 「下载/启用/停用」窄一个图标加间距（约 19px），同一列上下两行的按钮左边缘对不齐。
// 这是**下限**不是固定宽度：英文/日文的 Downloading… 之类更长，照常撑开。
const ACTION_MIN_W = 'min-w-[96px]';

// 右侧尾部那个卸载图标按钮的占位尺寸。**每一行都要占住这块**，哪怕没有卸载按钮：
// 「本机已有」的行有它、「可下载」的行没有，不占位的话两个分区的主按钮右边缘会差
// 出整整一个按钮的宽度——用户一眼就看出来两块没对齐。与 BTN_BASE 同高（h-9）。
const TRAILING_SLOT = 'h-9 w-9 shrink-0';

/** 状态 → 徽章文案键与配色。取值对齐插件中心的状态徽章（ToolStoreView 的 sky/emerald/amber）。 */
const STATE_BADGE = {
  running: { key: 'statusRunning', className: 'bg-emerald-50 text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-300' },
  deployed: { key: 'statusDeployed', className: 'bg-sky-50 text-sky-700 dark:bg-sky-500/10 dark:text-sky-300' },
  downloaded: { key: 'statusPending', className: 'bg-amber-50 text-amber-700 dark:bg-amber-500/10 dark:text-amber-300' },
  available: { key: 'statusAvailable', className: 'bg-slate-100 text-slate-500 dark:bg-slate-800 dark:text-slate-400' },
};

/**
 * 副标题：提供方 · 引擎 版本 · 量化 · 体积。
 *
 * 逐段拼而不是模板串，是因为每一段都可能缺——商店卡片没有 engine_version 的情况
 * 很常见，硬拼会出现「sglang  · NVFP4」这种多余的空格和分隔符。
 * @param {object} model - 合并后的模型项。
 * @returns {string} 副标题。
 */
function specLine(model) {
  const parts = [];
  if (model.org) parts.push(model.org);
  if (model.engine_name) {
    parts.push(model.engine_version ? `${model.engine_name} ${model.engine_version}` : model.engine_name);
  }
  if (model.quantization) parts.push(model.quantization);
  if (model.size_bytes) parts.push(formatBytes(model.size_bytes));
  return parts.join(' · ');
}

/**
 * 右侧的主动作。返回 null 表示这一档没有主动作（正在跑长操作时）。
 * @param {object} params - 参数。
 * @returns {React.ReactElement|null} 按钮。
 */
function PrimaryAction({ model, copy, busyKind, onDownload, onDeploy, onEnable, onStop }) {
  const busy = !!busyKind;
  // 正在跑的那个操作才显示进行时文案。不分命令一律显示「启用中…」的话，点「重新
  // 部署」时界面会写着「启用中…」而任务卡写着「部署…」，自相矛盾。
  const label = (kind, running, idle) => (busyKind === kind ? running : idle);

  if (model.state === 'available') {
    return (
      <button
        type="button"
        className={`${BTN_PRIMARY} ${ACTION_MIN_W}`}
        disabled={busy}
        data-testid="model-store-download"
        onClick={(event) => { event.stopPropagation(); onDownload(model); }}
      >
        {busyKind === 'download' ? null : <Download size={14} />}
        {label('download', copy.downloading, copy.download)}
      </button>
    );
  }
  if (model.state === 'downloaded') {
    return (
      <button
        type="button"
        className={`${BTN_PRIMARY} ${ACTION_MIN_W}`}
        disabled={busy}
        data-testid="model-store-deploy"
        onClick={(event) => { event.stopPropagation(); onDeploy(model.record); }}
      >
        {label('deploy', copy.deploying, copy.deploy)}
      </button>
    );
  }
  if (model.state === 'deployed') {
    return (
      <button
        type="button"
        className={`${BTN_PRIMARY} ${ACTION_MIN_W}`}
        disabled={busy}
        data-testid="model-store-enable"
        onClick={(event) => { event.stopPropagation(); onEnable(model.record); }}
      >
        {busyKind === 'enable' ? null : <Play size={13} />}
        {label('enable', copy.enabling, copy.enable)}
      </button>
    );
  }
  return (
    <button
      type="button"
      className={`${BTN_DANGER} ${ACTION_MIN_W}`}
      disabled={busy}
      data-testid="model-store-stop"
      onClick={(event) => { event.stopPropagation(); onStop(model.record); }}
    >
      {busyKind === 'stop' ? null : <StopCircle size={13} />}
      {label('stop', copy.stopping, copy.stop)}
    </button>
  );
}

/**
 * 一行模型。整行可点开详情；右侧按钮各自阻止冒泡。
 * @param {object} params - 参数。
 * @returns {React.ReactElement} 行卡。
 */
function ModelRow({ model, copy, busyKind, onOpen, onDownload, onDeploy, onEnable, onStop, onUninstall }) {
  const badge = STATE_BADGE[model.state] || STATE_BADGE.available;
  const busy = !!busyKind;
  const isDual = model.deploy_topology === 'dual';

  return (
    // 两条规则都要压：仓库里所有「只响应指针、不进 tab 序」的元素都是这么写的
    // （见 components/ComposerPopover.jsx、features/chat/ChatView.jsx）。
    // biome-ignore lint/a11y/useKeyWithClickEvents: 整行是详情的快捷入口；键盘路径由右侧按钮承担，整行不进 tab 序
    // biome-ignore lint/a11y/noStaticElementInteractions: role="presentation" 的容器，交互语义由内部按钮承担
    <div
      data-testid="model-store-row"
      data-card-id={model.id}
      data-state={model.state}
      role="presentation"
      className="group flex cursor-pointer items-center gap-4 border-b border-slate-100 px-3 py-3 last:border-0 dark:border-white/5"
      onClick={() => onOpen(model)}
    >
      {/* 图标是装饰性的（旁边紧跟着模型名），所以 ProviderIcon 内部把 img 的 alt
          写死成空串；这里不用再传。认不出厂商时它自己降级成 Cpu 图标 —— 但商店里
          每一张卡都该走到对应厂商的真图标，降级是兜底，不是常态。 */}
      <ProviderIcon
        size="lg"
        src={resolveBrandIcon({ org: model.org, name: model.name, sourceRepoId: model.source_repo_id })}
      />

      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate text-[17px] font-semibold tracking-tight text-slate-900 dark:text-white">
            {model.name}
          </span>
          <span
            data-testid="model-store-row-state"
            className={`shrink-0 rounded-full px-1.5 py-0.5 text-[10px] font-semibold ${badge.className}`}
          >
            {copy[badge.key]}
          </span>
          {/* 双机是「这张卡能不能装」的前提，属于身份信息，不能混进下面那排参数里被略过 */}
          {isDual ? (
            <span
              data-testid="model-store-row-dual"
              className="shrink-0 rounded-full bg-amber-50 px-1.5 py-0.5 text-[10px] font-semibold text-amber-700 dark:bg-amber-500/10 dark:text-amber-400"
            >
              {copy.topologyDual}
            </span>
          ) : null}
        </div>
        <div className="mt-0.5 truncate text-[13px] font-medium text-slate-500 dark:text-slate-400">
          {specLine(model)}
        </div>
      </div>

      {/* biome-ignore lint/a11y/noStaticElementInteractions: 只是拦住冒泡，免得点按钮顺带打开详情 */}
      <div className="flex shrink-0 items-center gap-2" role="presentation" onClick={(event) => event.stopPropagation()}>
        {/* 「重新部署」只在**脚本更新过、跟上次部署用的那份对不上**时才出现
            （LMM 的 needs_redeploy，由管理 API 随库记录一起带下来）。以前它对
            所有已部署模型常驻，用户根本不知道自己为什么要点它——它是修复入口，
            日常路径是启用，不是重来一次。

            不用再判 running：运行中的卡 state 是 'running' 而不是 'deployed'
            （见 model-store-logic.js 的 libraryStateOf），这里的 'deployed'
            已经隐含了「没在跑」。这个条件不是漏了——重新部署必须先停用，否则会
            撞上还占着端口的旧容器。 */}
        {model.state === 'deployed' && model.record?.needs_redeploy ? (
          <button
            type="button"
            className={BTN_NEUTRAL}
            disabled={busy}
            onClick={() => onDeploy(model.record)}
          >
            {busyKind === 'deploy' ? copy.deploying : copy.redeploy}
          </button>
        ) : null}
        <PrimaryAction
          model={model}
          copy={copy}
          busyKind={busyKind}
          onDownload={onDownload}
          onDeploy={onDeploy}
          onEnable={onEnable}
          onStop={onStop}
        />
        {/* 没有卸载按钮时留一个等宽空位，别让这一行的主按钮往右跑。
            「可下载」的模型还没装到本机，没有可卸载的东西；但它和「本机已有」
            上下摆在同一列里，少了这块占位就会整体错开一个按钮的宽度。 */}
        {model.record ? (
          <button
            type="button"
            title={copy.uninstall}
            aria-label={copy.uninstall}
            disabled={busy}
            data-testid="model-store-uninstall"
            className={`${TRAILING_SLOT} inline-flex items-center justify-center rounded-full text-slate-400 transition-colors hover:bg-rose-50 hover:text-rose-600 disabled:opacity-40 dark:text-slate-500 dark:hover:bg-rose-500/10 dark:hover:text-rose-400`}
            onClick={() => onUninstall(model.record)}
          >
            <Trash2 size={15} />
          </button>
        ) : (
          <span className={TRAILING_SLOT} aria-hidden="true" data-testid="model-store-trailing-spacer" />
        )}
      </div>
    </div>
  );
}

export { ModelRow, specLine };
