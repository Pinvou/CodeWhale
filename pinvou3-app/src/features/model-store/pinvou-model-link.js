// 把 LMM 里启用好的本地模型接进品悟自己的模型列表。
//
// 这是整条链路的最后一公里：用户装完品悟没有本地模型 -> 进模型商店 -> 下载、
// 部署、启用 -> **一键在品悟里用起来**。用户全程不该看见、更不该复制粘贴那把
// API Key。
//
// 品悟的模型配置只有一个存储（~/.pinvou3/settings.json 的 advanced.saved_models），
// 本地模型就是其中一行，没有第二套表。Key 不落这个文件（字段是 skip_serializing），
// 由 Rust 侧塞进系统密钥环。所以这里只要把一行 SavedModel 交给 bridge 就够了。

/** 品悟前端生成模型 id 的既有写法（见 features/settings/SettingsView.jsx 的 doSave）。 */
function makeModelId() {
  // eslint-disable-next-line sonarjs/pseudo-random -- 跟品悟既有写法一致（SettingsView.jsx 的 makeModelId）：id 只在创建时生成一次，不用于任何安全用途
  return `m_${Date.now().toString(36)}${Math.random().toString(36).slice(2, 7)}`;
}

/**
 * 拼出一行 SavedModel。
 *
 * 几个取值是有讲究的，改之前先读注释：
 *
 * - `preset` 必须是 `openai_compatible`，**不能**用 `local_vllm`。后者会触发一堆
 *   vLLM 专属的归一化（上下文窗口被按模型名改写、`max_output_tokens` 在等于
 *   24576 时被静默置空）。而且 `ModelPreset` 是闭合枚举，自己造一个 `"lmm"`
 *   会让 serde 在下次读配置时整份解析失败、把用户的设置视图清空。
 * - `model` 填**推理服务自己认的那个 id**（库记录里的 `endpoint_model`）。
 *   经 API 网关时要填的是 `model_name`（网关按它查表、转发时才替换），但这里是
 *   **直连容器**，填 model_name 就是 404。
 * - `base_url` 直接用库记录里的 `endpoint_url`，它本来就是 `127.0.0.1:<port>/v1`
 *   这种本机地址——品悟和模型在同一台机器上。**不要**用详情页展示给外部应用的
 *   那个地址。
 * - `credential_action: 'replace'` **不能省**，哪怕本地服务不要 Key。对回环地址
 *   品悟认为"本地模型不需要 Key"（api_key_required 返回 false），漏了这个字段
 *   模型会以 `credential_state: missing` 存下来，而品悟的探测会把这种状态误判成
 *   generic 类型。错得很隐蔽。
 * @param {{modelName: string, displayName: string, baseUrl: string,
 *          apiKey?: string|null, contextWindow?: number|null}} input 入参。
 * @returns {object} 可直接交给 bridge.models.saveModel 的一行配置。
 */
function buildSavedModel({ modelName, displayName, baseUrl, apiKey = null, contextWindow = null }) {
  if (!modelName) throw new Error('missing modelName');
  if (!baseUrl) throw new Error('missing baseUrl');
  return {
    id: makeModelId(),
    name: displayName,
    preset: 'openai_compatible',
    provider_kind: 'custom',
    model: modelName,
    base_url: baseUrl,
    context_window_tokens: contextWindow || null,
    max_output_tokens: null,
    reasoning_effort: null,
    // 本地推理服务默认不校验 Key，给一个占位值即可。留空会走上面说的
    // credential_state: missing 那条岔路。
    api_key: apiKey || 'local',
    credential_action: 'replace',
    image_capability_override: 'pinvou',
    vision_model_id: null,
  };
}

/**
 * 从一批 Key 里挑一把**这张卡真正能用**的。
 *
 * 必须按 card_id 过滤，不能"取第一把非空的"。LMM 的
 * `GET /library/:card_id/keys` 回的不只是这个模型专属的 Key，还包括**改造前
 * 生成的、没绑定任何模型的老 Key**（SQL 是 `WHERE card_id = ?1 OR card_id IS
 * NULL`）。而网关对未绑定的 Key 是直接 403 的：
 * 「这把 Key 没有绑定任何模型，已拒绝；请在模型详情里重新生成」。
 *
 * 老 Key 按 created_at 倒序可能排在前面，于是「在品悟里使用这个模型」会一路
 * 成功——保存、设为当前模型——然后用户发的第一句话拿到 403。而且因为这里
 * 返回了非 null，调用方还会跳过"没有就新建一把"的分支，等于把自己救回来的
 * 机会也堵死了。
 *
 * 返回 null 表示还没有可用的，调用方需要先建一把。
 * @param {Array<{key_value?: string, card_id?: string|null}>} keys Key 列表。
 * @param {string} cardId 当前卡片 id。
 * @returns {string|null} Key 明文。
 */
function pickApiKey(keys, cardId) {
  if (!Array.isArray(keys) || !cardId) return null;
  const hit = keys.find(
    (k) => k?.card_id === cardId && typeof k?.key_value === 'string' && k.key_value.length > 0
  );
  return hit ? hit.key_value : null;
}

/**
 * 这个模型暴露的是不是 OpenAI 那套对话接口。
 *
 * 判据用 `endpoint_url` 而不是 `engine_name`：引擎名是个会不断增加的开放集合
 * （现在有 sglang / vllm / diffusers，以后还会有别的），而"暴露的是哪套 HTTP
 * 接口"才是真正的契约。实测各卡片的取值：
 *
 *   sglang / vllm 的对话模型   http://127.0.0.1:30001/v1/chat/completions
 *   diffusers 的文生图         http://127.0.0.1:8000/generate
 *
 * 也接受只记到 `/v1` 的写法——有些卡片记的是基址而不是完整路径。
 * @param {unknown} url endpoint_url。
 * @returns {boolean} 是不是 OpenAI 兼容的对话接口。
 */
function isOpenAiChatEndpoint(url) {
  if (typeof url !== 'string' || !url) return false;
  return /\/v1\/chat\/completions\/?$/i.test(url) || /\/v1\/?$/i.test(url);
}

/**
 * 这个模型现在能不能接进品悟；不能的话给出原因键（供 i18n 取文案）。
 *
 * 除了"在跑"和"有名字"，还必须确认它**是个对话模型**。品悟的模型列表里每一行
 * 都会被当成对话模型使用：选中之后新建会话就往 `/v1/chat/completions` 发
 * `messages`。把文生图模型接进去，用户一开新对话就会撞上引擎的 422
 * （`{"loc":["body","prompt"],"msg":"Field required"}`），而品悟只会显示一句
 * 笼统的"当前模型服务不可用"——现象离原因很远，用户根本不会想到是模型类型不对。
 *
 * 这个检查必须在**接入之前**做：接入那一步会顺手把它设为当前模型，等于把用户
 * 正在用的对话模型也一起换掉了。
 * @param {{record?: object}} detail 模型详情。
 * @returns {{ok: boolean, reason?: string}} 判定结果。
 */
function checkLinkable(detail) {
  const record = detail?.record;
  if (!record) return { ok: false, reason: 'linkNoRecord' };
  if (!record.running) return { ok: false, reason: 'linkNotRunning' };
  if (!record.model_name) return { ok: false, reason: 'linkNoModelName' };
  if (!isOpenAiChatEndpoint(record.endpoint_url)) {
    return { ok: false, reason: 'linkNotChatModel' };
  }
  return { ok: true };
}

/** 回环主机名的全部写法。IPv6 那个在这里是**去掉方括号后**的形态。 */
const LOOPBACK_HOSTS = ['127.0.0.1', 'localhost', '::1'];

/**
 * base_url 是不是指向本机的 LMM 网关。
 *
 * 判回环地址而不是判具体端口：网关端口用户能在设置里改，改完之前接进去的那些行
 * 端口就对不上了，但它们**仍然**是指向本机 LMM 的行，卸载时同样要清掉。
 * @param {unknown} baseUrl - 模型行的 base_url。
 * @returns {boolean} 是不是本机 LMM。
 */
function isLocalGatewayUrl(baseUrl) {
  if (typeof baseUrl !== 'string' || !baseUrl) return false;
  try {
    // IPv6 的 hostname 带方括号（`[::1]`），不是裸的 `::1`——直接比会漏掉。
    const hostname = new URL(baseUrl).hostname.replaceAll(/^\[|\]$/g, '');
    return LOOPBACK_HOSTS.includes(hostname);
  } catch {
    return false;
  }
}

/**
 * 找出品悟模型列表里指向某个已卸载 LMM 模型的那些行。
 *
 * 为什么必须清：LMM 侧卸载会连库记录一起删掉，此后网关对这个 model_name 一律
 * 回 **404「未找到模型」**——而 404 在大多数 OpenAI 客户端眼里是"地址写错了"，
 * 不是"模型没了"。更糟的是接入时会顺手把它设为当前模型，所以这行死数据很可能
 * 正是默认模型：用户下一次新建会话，第一句话就失败，而且从那个报错里找不到任何
 * 通往模型商店的路。
 *
 * 匹配用 `model` + 回环 base_url 两个条件：光看 model 会误伤用户自己配的、指向
 * 别处同名模型的行。
 * @param {Array<object>|null} savedModels - 品悟的模型列表（bs.savedModels）。
 * @param {string} modelName - LMM 的 model_name。
 * @returns {Array<object>} 需要清掉的行。
 */
function findLinkedRows(savedModels, modelName) {
  if (!Array.isArray(savedModels) || !modelName) return [];
  return savedModels.filter((row) => row?.model === modelName && isLocalGatewayUrl(row?.base_url));
}

export {
  buildSavedModel,
  checkLinkable,
  findLinkedRows,
  isLocalGatewayUrl,
  isOpenAiChatEndpoint,
  makeModelId,
  pickApiKey,
};
