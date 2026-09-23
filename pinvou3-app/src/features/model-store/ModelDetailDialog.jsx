// 已下载模型的详情。自上而下三段:这个模型是什么 -> 现在跑得怎么样 -> 别人怎么接进来。
//
// 「供 Agent 应用接入」放在最后一段,并且把 Key 的管理也并进去:模型 ID、API 地址、
// API Key 本来就是同一件事要凑齐的三样东西,拆开会让 Key 看着像两个不同的东西。
import { useCallback, useEffect, useRef, useState } from 'react';
import { KeyRound, Plus } from '../../components/icons.jsx';
import { EmptyState } from '../../components/EmptyState.jsx';
import { Spinner } from '../../components/Spinner.jsx';
import { createApiKey, fetchApiKeys, fetchModelDetail } from './model-store-api.js';
import { tryGetTauriBridge } from '../../platform/tauri/client.js';
import { buildSavedModel, checkLinkable, findLinkedRows, pickApiKey } from './pinvou-model-link.js';
import { formatBytes, libraryStatusOf } from './model-store-logic.js';
import {
  HOVER_SURFACE,
  MsButton,
  MsDialog,
  MsDialogTitle,
  MsRow,
  SUBTLE_SURFACE,
  TEXT_DANGER,
  TEXT_PRIMARY,
  TEXT_SECONDARY,
  WeightVerifyNotice,
} from './model-store-ui.jsx';

const TITLE_ID = 'model-store-detail-title';

const STATUS_LABEL_KEYS = { running: 'statusRunning', deployed: 'statusDeployed', pending: 'statusPending' };

/**
 * 一行可直接复制的接入信息。值必须是**对方那边原样粘贴就能生效**的东西,
 * 不能是只在本机成立的回环地址之类。
 * @returns {React.ReactElement} 可复制行。
 */
function CopyRow({ label, value, hint, copyLabel, onCopy, testId }) {
  return (
    <div className="flex items-start gap-3">
      <span className={`w-20 shrink-0 pt-1.5 text-[13px] ${TEXT_SECONDARY}`}>{label}</span>
      <div className="min-w-0 flex-1">
        <div className={`flex items-center gap-2 rounded-[12px] px-3 py-1.5 ${SUBTLE_SURFACE}`}>
          <code data-testid={testId} className={`min-w-0 flex-1 break-all text-[12px] ${TEXT_PRIMARY}`}>
            {value || '—'}
          </code>
          <button
            type="button"
            disabled={!value}
            className={`shrink-0 rounded-full px-2 py-0.5 text-[12px] disabled:opacity-40 ${TEXT_SECONDARY} ${HOVER_SURFACE}`}
            onClick={() => onCopy(value)}
          >
            {copyLabel}
          </button>
        </div>
        {hint ? <p className={`mt-1 text-[11px] ${TEXT_SECONDARY}`}>{hint}</p> : null}
      </div>
    </div>
  );
}

/**
 * 模型详情弹窗。
 *
 * `card` 是合并后的那张商店卡片，只为了拿权重校验档位。为什么不从 `detail` 里读:
 * `fetchModelDetail` 回的是**库记录**(sqlite)，而档位是卡片元数据，库里没有这一
 * 列。拿不到 card(商店列表还没拉回来、卡片已下架)时,`weightVerificationOf` 会
 * 按 `none` 处理——宁可多出一条黄条,不能少出一条。
 * @returns {React.ReactElement} 弹窗。
 */
function ModelDetailDialog({ cardId, card, copy, theme, onClose, onRedeploy, onToast, savedModels }) {
  const [detail, setDetail] = useState(null);
  const [keys, setKeys] = useState([]);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  // 重试单独一个忙态,不借用 busy:busy 是「建 Key / 接入品悟」那两个动作的,
  // 借用会让重试转圈时把那两个按钮也一起禁掉(虽然此刻它们并不在屏幕上,
  // 但重试成功后 busy 若未归位就会留下一排灰按钮)。
  const [retrying, setRetrying] = useState(false);

  // 当前有效的那张卡。换卡时自增，让上一张卡在途的响应落地时能认出自己已经过期。
  //
  // 没有这道闸门时:A 的响应如果落在「切到 B、effect 已清空 detail」之后、「B 的响应
  // 回来」之前,弹窗会短暂显示 A 的名字、运行时和接入地址——用户以为自己看的是 B。
  // ModelStoreView 里那个测试商店的 effect 用的是同一个套路(cancelled 标志)。
  const loadTokenRef = useRef(0);

  const load = useCallback(async () => {
    if (!cardId) return;
    const token = (loadTokenRef.current += 1);
    const stale = () => token !== loadTokenRef.current;
    try {
      // Key 拿不到不算致命:模型本身的信息照样该显示出来。
      const [modelDetail, keyList] = await Promise.all([
        fetchModelDetail(cardId),
        fetchApiKeys(cardId).catch(() => []),
      ]);
      if (stale()) return;
      setDetail(modelDetail);
      setKeys(Array.isArray(keyList) ? keyList : []);
      // 成功了就把上一次的错误擦掉。load() 不只在打开弹窗时跑——建 Key、接入品悟
      // 之后都会重拉一次。少了这一句,任何一次瞬时失败都会把 error 永久钉住:渲染
      // 分支是 `{error ? … }` / `{!error && detail ? … }`,于是整个详情页(状态、
      // 运行时、接入信息、Key 列表、所有按钮)塌成一行「加载失败」,而且没有重试
      // 入口,必须关掉弹窗重开。用户看到的是"我刚把这个模型搞坏了"。
      setError(null);
    } catch (loadError) {
      // 过期请求的失败也要丢掉:否则切到 B 之后,A 的超时会把 B 的详情页打成错误态。
      if (stale()) return;
      setError(String(loadError?.message || loadError));
    }
  }, [cardId]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- 换一张卡时先把上一张的详情/Key 清空再拉新的;不清会让新卡短暂显示旧卡的运行时和 Key
    setDetail(null);
    setKeys([]);
    setError(null);
    load();
  }, [load]);

  // 错误态的重试入口。就是再跑一次 load()——失败原因基本都是瞬时的
  // (后端刚重启、请求超时、Key 接口抖了一下),重来一次往往就好了。
  const retry = async () => {
    setRetrying(true);
    try {
      await load();
    } finally {
      setRetrying(false);
    }
  };

  const copyText = async (text) => {
    try {
      await navigator.clipboard.writeText(text);
      onToast(copy.copied);
    } catch {
      // 没有剪贴板权限时不谎报成功 —— 内容本来就是可见可手选的。
      onToast(copy.copyFailed);
    }
  };

  const addKey = async () => {
    setBusy(true);
    try {
      await createApiKey(cardId, null);
      onToast(copy.keyCreated);
      await load();
    } catch (keyError) {
      onToast(String(keyError?.message || keyError));
    } finally {
      setBusy(false);
    }
  };

  // 一键把这个模型接进品悟自己的模型列表。
  //
  // **直连推理容器**，不经 API 网关。模型商店改成原生实现之后，品悟和模型在同一台
  // 机器上，`endpoint_url` 就是现成的、可直接调的地址；中间再架一层网关只是多一个
  // 会出故障的环节（sidecar 时代要网关，是因为它同时承担了给**局域网其他应用**
  // 分发的职责——那件事排在后面的阶段，跟"品悟自己用"是两回事）。
  //
  // `model` 要填 `endpoint_model` 而不是 `model_name`：前者是推理服务真正认的 id。
  // 经网关时填 model_name 是对的（网关按它查表再替换），直连时填错就是 404。
  const useInPinvou = async () => {
    const bridge = tryGetTauriBridge();
    if (!bridge?.models?.saveModel) {
      onToast(copy.linkBridgeMissing);
      return;
    }
    // 按钮已经按同一判据禁用了，这里是第二道闸：detail 可能在对话框开着的时候
    // 被 load() 刷新（比如模型正好被停用），点下去的那一刻未必还成立。
    if (!linkable.ok) {
      onToast(copy[linkable.reason] || copy.linkFailed);
      return;
    }
    setBusy(true);
    try {
      const modelName = record.endpoint_model || record.model_name;
      const saved = buildSavedModel({
        modelName,
        displayName: copy.linkModelName(record.card_name || record.model_name),
        baseUrl: record.endpoint_url,
        contextWindow: null,
        // 已经接入过就**沿用那一行的 id**，让这次保存变成更新而不是新增。
        //
        // 不查重的话，`makeModelId()` 每次都生成新 id：用户连点两次、或者隔天
        // 再点一次，品悟的模型列表里就多一行指向同一个本地服务的重复项，而且
        // 每次都会被设为当前模型。判据跟移除侧的 findLinkedRows 完全一致
        // （model + 回环 base_url），两边用同一把尺子才不会出现"接得进、清不掉"。
        existingId: findLinkedRows(savedModels, modelName)[0]?.id,
      });
      await bridge.models.saveModel(saved);
      // 选为当前模型。注意这只影响**新建的会话**，不会打断正在生成的那些。
      if (bridge.models.setActiveModel) await bridge.models.setActiveModel(saved.id);
      onToast(copy.linkDone);
      await load();
    } catch (linkError) {
      onToast(String(linkError?.message || linkError));
    } finally {
      setBusy(false);
    }
  };

  const record = detail?.record;
  // 能不能接进品悟。算一次给按钮的禁用态和下面那行原因共用——两处各算一次的话,
  // 很容易出现"按钮灰着但没给原因"或者反过来。
  const linkable = checkLinkable(detail);
  // 推理服务自己的地址。**这是本机地址**,粘到别的机器上连不通——要给局域网里的
  // 其他应用用,得先有 API 网关,那一块还没做（见 docs/模型商店-框架与移植基线.md
  // 的分期表）。这里如实显示本机地址,并在下面那行提示里说清楚。
  const apiUrl = record?.endpoint_url ?? '';
  // 直连推理服务时要填的是它自己认的那个 id。
  const modelId = record?.endpoint_model || record?.model_name || '';
  const runtimeLines = detail?.runtime_lines ?? [];

  return (
    <MsDialog open={!!cardId} onClose={onClose} theme={theme} labelledBy={TITLE_ID} wide testId="model-store-detail-dialog">
      {/* 标题栏**常驻**,在错误/加载/正常三个分支之上。
          原先它只长在成功分支里,于是 load() 一失败,整个弹窗就只剩一行红字:
          没有标题、没有关闭按钮(右上角那个 X 也在标题栏里)、没有重试。而
          load() 不只在打开时跑——建 Key、接入品悟之后都会重拉一次,任何一次
          瞬时失败都会把用户手里这个弹窗打成一行红字,只能靠 Esc 或点背板离开。
          card_name 在错误态下通常仍然拿得到(catch 分支不清 detail),拿不到时
          用通用标题兜底,总之标题栏本身不会消失。 */}
      <MsDialogTitle id={TITLE_ID} onClose={onClose} closeLabel={copy.detailClose}>
        {record?.card_name || copy.detailTitle}
      </MsDialogTitle>

      {error ? (
        <div data-testid="model-store-detail-error">
          <div className={`text-[13px] ${TEXT_DANGER}`}>{copy.loadFailed(error)}</div>
          <div className="mt-4 flex items-center gap-2">
            <MsButton
              tone="primary"
              busy={retrying}
              onClick={retry}
              data-testid="model-store-detail-retry"
            >
              {copy.retry}
            </MsButton>
            <MsButton tone="ghost" onClick={onClose}>{copy.detailClose}</MsButton>
          </div>
        </div>
      ) : null}

      {!error && !detail ? (
        <div className="flex items-center justify-center gap-2 py-10">
          <Spinner size={16} tone="brand" />
          <span className={`text-[13px] ${TEXT_SECONDARY}`}>{copy.loading}</span>
        </div>
      ) : null}

      {!error && detail ? (
        <>
          <div className="space-y-1">
            <MsRow label={copy.detailStatus}>{copy[STATUS_LABEL_KEYS[libraryStatusOf(record)]]}</MsRow>
            <MsRow label={copy.detailEngine}>
              {record.engine_name}
              {record.engine_version ? ` ${record.engine_version}` : ''}
            </MsRow>
            <MsRow label={copy.detailSize}>{formatBytes(record.size_bytes)}</MsRow>
            {record.endpoint_url ? <MsRow label={copy.detailEndpoint}>{record.endpoint_url}</MsRow> : null}
          </div>

          {/* 紧跟在「这个模型是什么」那几行后面:权重完整性跟引擎、体积是同一类
              事实,不是一条飘在页尾的免责声明。达标的卡这里什么都不长。 */}
          <WeightVerifyNotice card={card} copy={copy} />

          <div className="mt-5">
            <h3 className={`mb-2 text-[13px] font-medium ${TEXT_PRIMARY}`}>{copy.runtimeTitle}</h3>
            {runtimeLines.length > 0 ? (
              <div className={`space-y-1 rounded-[12px] p-3 ${SUBTLE_SURFACE}`}>
                {runtimeLines.map((line) => (
                  <div key={line} className={`text-[12px] ${TEXT_SECONDARY}`}>{line}</div>
                ))}
              </div>
            ) : (
              <div className={`text-[12px] ${TEXT_SECONDARY}`}>{detail.runtime_note || copy.runtimeEmpty}</div>
            )}
            {runtimeLines.length > 0 && detail.runtime_note ? (
              <p className={`mt-1 text-[11px] ${TEXT_SECONDARY}`}>{detail.runtime_note}</p>
            ) : null}
          </div>

          <div className="mt-5">
            <h3 className={`mb-2 text-[13px] font-medium ${TEXT_PRIMARY}`}>{copy.accessTitle}</h3>
            <div className="space-y-2">
              <CopyRow
                label={copy.accessModelId}
                value={modelId}
                copyLabel={copy.copy}
                onCopy={copyText}
                testId="model-store-access-model-id"
              />
              <CopyRow
                label={copy.accessApiUrl}
                value={apiUrl}
                copyLabel={copy.copy}
                onCopy={copyText}
                hint={copy.accessLocalOnly}
                testId="model-store-access-url"
              />

              <div className="flex items-start gap-3">
                <span className={`w-20 shrink-0 pt-1.5 text-[13px] ${TEXT_SECONDARY}`}>{copy.accessApiKey}</span>
                <div className="min-w-0 flex-1">
                  {keys.length === 0 ? (
                    <EmptyState
                      testId="model-store-keys-empty"
                      className={`rounded-[12px] px-3 py-2 text-left text-[12px] ${SUBTLE_SURFACE} ${TEXT_SECONDARY}`}
                      title={copy.keysEmpty}
                    />
                  ) : (
                    <div className="space-y-1.5">
                      {keys.map((key) => (
                        <div key={key.id} className={`flex items-center gap-2 rounded-[12px] px-3 py-1.5 ${SUBTLE_SURFACE}`}>
                          <KeyRound size={13} className={TEXT_SECONDARY} />
                          <code className={`min-w-0 flex-1 break-all text-[12px] ${TEXT_PRIMARY}`}>{key.key_value}</code>
                          <button
                            type="button"
                            className={`shrink-0 rounded-full px-2 py-0.5 text-[12px] ${TEXT_SECONDARY} ${HOVER_SURFACE}`}
                            onClick={() => copyText(key.key_value)}
                          >
                            {copy.copy}
                          </button>
                        </div>
                      ))}
                    </div>
                  )}
                  <p className={`mt-1 text-[11px] ${TEXT_SECONDARY}`}>{copy.keysHint}</p>
                </div>
              </div>
            </div>

            {record.running ? null : (
              <p className="mt-2 text-[11px] text-[#A15C07] dark:text-[#FDD663]">{copy.notRunningHint}</p>
            )}

            <div className="mt-3 flex flex-wrap items-center gap-2">
              <MsButton tone="ghost" busy={busy} onClick={addKey} data-testid="model-store-add-key">
                <Plus size={14} />
                {copy.addKey}
              </MsButton>
            </div>
          </div>

          <div className="mt-6 flex items-center justify-between">
            {/* 修复入口:部署被外部破坏(比如手删了 vendor 目录)时需要能重来一次。
                运行中的模型要先停用,否则重来一次只会撞上还占着端口的旧容器。 */}
            {/* 接不进去时按钮禁用，并且**把原因说出来**。只禁用不解释的话，
                用户面对一个灰掉的按钮只能猜——尤其"这不是对话模型"这种原因，
                光看界面根本想不到。 */}
            <div className="min-w-0 flex-1">
              <MsButton
                tone="primary"
                busy={busy}
                disabled={!linkable.ok}
                onClick={useInPinvou}
                data-testid="model-store-use-in-pinvou"
              >
                {copy.linkUse}
              </MsButton>
              {linkable.ok ? null : (
                <div
                  className={`mt-1.5 max-w-[420px] text-[11px] leading-4 ${TEXT_SECONDARY}`}
                  data-testid="model-store-link-blocked-reason"
                >
                  {copy[linkable.reason] || copy.linkFailed}
                </div>
              )}
            </div>
            {/* 这个「重新部署」**刻意常驻**，跟列表行那个不是一回事：
                - 列表行（ModelRow）只在 needs_redeploy 时出现——脚本更新了、跟上次
                  部署用的版本对不上。那是日常路径，不该让用户对着一个常亮的按钮
                  猜自己为什么要点它（LMM 早年就是这么错的，注释留在 LibraryView.jsx）。
                - 这里是**修复入口**：部署被外部破坏时（典型是手删了 vendor 目录、
                  或容器配置被改过）脚本哈希没变、needs_redeploy 是 false，但用户
                  确实需要能重来一次。详情页是主动打开、带着上下文的那一面，
                  放一个随时可用的修复动作是合适的。
                运行中禁用：重新部署会撞上还占着端口的旧容器，必须先停用。 */}
            <MsButton
              tone="ghost"
              disabled={record.status !== 'deployed' || record.running}
              onClick={() => onRedeploy(record)}
            >
              {copy.redeploy}
            </MsButton>
            <MsButton tone="ghost" onClick={onClose}>{copy.detailClose}</MsButton>
          </div>
        </>
      ) : null}
    </MsDialog>
  );
}

export { ModelDetailDialog };
