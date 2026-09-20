// 「在品悟里使用这个模型」那条链路的取值，错一个就静默失效。
import assert from 'node:assert/strict';
import test from 'node:test';

import {
  buildSavedModel,
  checkLinkable,
  findLinkedRows,
  isLocalGatewayUrl,
  isOpenAiChatEndpoint,
  makeModelId,
  pickApiKey,
} from '../src/features/model-store/pinvou-model-link.js';

const OK = {
  modelName: 'deepseek-v4-flash-vision-exp',
  displayName: '本地 · DeepSeek',
  baseUrl: 'http://127.0.0.1:30001/v1',
};

test('credential_action 必须是 replace，api_key 不能留空', () => {
  // 对回环地址品悟认为"本地模型不需要 Key"，漏了这个字段模型会以
  // credential_state: missing 存下来，而品悟的探测会把这种状态误判成 generic。
  // 错得极其隐蔽，所以钉死。本地推理服务不校验 Key，占位值即可。
  assert.equal(buildSavedModel(OK).credential_action, 'replace');
  assert.equal(buildSavedModel(OK).api_key, 'local');
  assert.equal(buildSavedModel({ ...OK, apiKey: 'sk-x' }).api_key, 'sk-x');
});

test('preset 必须是 openai_compatible，不能是 local_vllm', () => {
  // local_vllm 会触发 vLLM 专属归一化（上下文窗口按模型名改写、
  // max_output_tokens 等于 24576 时被静默置空）。
  const m = buildSavedModel(OK);
  assert.equal(m.preset, 'openai_compatible');
  assert.equal(m.provider_kind, 'custom');
});

test('base_url 原样用库记录里的 endpoint_url', () => {
  // 直连推理容器，不经 API 网关：品悟和模型在同一台机器上，中间再架一层只是多
  // 一个会出故障的环节。/v1 不能丢——品悟探测时直接在其后拼 /models。
  assert.equal(buildSavedModel(OK).base_url, 'http://127.0.0.1:30001/v1');
  assert.equal(
    buildSavedModel({ ...OK, baseUrl: 'http://127.0.0.1:8000/v1' }).base_url,
    'http://127.0.0.1:8000/v1'
  );
});

test('model 填推理服务自己认的 id', () => {
  // 直连时必须填 endpoint_model。经网关才是填 model_name（网关按它查表再替换），
  // 两者混用就是 404 —— 而调用方那边只会看到一句"模型不存在"。
  assert.equal(buildSavedModel(OK).model, 'deepseek-v4-flash-vision-exp');
});

test('缺任何一个必要取值都要当场报错，不能拼出半截配置', () => {
  for (const missing of ['modelName', 'baseUrl']) {
    assert.throws(() => buildSavedModel({ ...OK, [missing]: undefined }), undefined, missing);
  }
});

test('id 每次不同且带 m_ 前缀', () => {
  const a = makeModelId();
  assert.match(a, /^m_/);
  assert.notEqual(a, makeModelId());
  assert.notEqual(buildSavedModel(OK).id, buildSavedModel(OK).id);
});

test('pickApiKey 只认绑定了这张卡的 Key', () => {
  const C = 'qwen38-27b-sglang0519';
  assert.equal(pickApiKey([{ card_id: C, key_value: 'lmm-1' }, { card_id: C, key_value: 'lmm-2' }], C), 'lmm-1');
  assert.equal(pickApiKey([{ card_id: C, key_value: '' }, { card_id: C, key_value: 'lmm-2' }], C), 'lmm-2');
  assert.equal(pickApiKey([], C), null);
  assert.equal(pickApiKey(null, C), null);
  assert.equal(pickApiKey([{ card_id: C, label: '没有 key_value' }], C), null);
});

test('未绑定模型的老 Key 必须跳过，宁可新建一把', () => {
  // LMM 的 /library/:card_id/keys 是 `WHERE card_id = ?1 OR card_id IS NULL`，
  // 会把改造前生成的、没绑定模型的老 Key 一起回来，而且按 created_at 倒序时
  // 可能排在前面。网关对这种 Key 直接 403（「这把 Key 没有绑定任何模型」）。
  // 挑中它的后果：保存成功、设为当前模型，用户发第一句话才拿到 403；
  // 而且调用方会因为拿到了非 null 而跳过"没有就新建"的分支。
  const C = 'qwen38-27b-sglang0519';
  const keys = [
    { card_id: null, key_value: 'legacy-未绑定' },
    { card_id: C, key_value: 'lmm-本卡的' },
  ];
  assert.equal(pickApiKey(keys, C), 'lmm-本卡的');
  // 只有老 Key 时必须返回 null，让调用方去建一把绑定好的
  assert.equal(pickApiKey([{ card_id: null, key_value: 'legacy-未绑定' }], C), null);
  // 别的卡的 Key 同样不能用
  assert.equal(pickApiKey([{ card_id: '别的卡', key_value: 'x' }], C), null);
  // 没有 cardId 时不许瞎猜
  assert.equal(pickApiKey([{ card_id: C, key_value: 'lmm-1' }], undefined), null);
});

const CHAT_EP = 'http://127.0.0.1:30001/v1/chat/completions';

test('没启用的模型不许接入', () => {
  // 网关只转发 running 的模型，没启用就接进去，用户第一句话就会拿到 503。
  const r = (o) => checkLinkable({ record: { endpoint_url: CHAT_EP, ...o } });
  assert.equal(r({ running: false, model_name: 'x' }).reason, 'linkNotRunning');
  assert.equal(r({ running: true, model_name: '' }).reason, 'linkNoModelName');
  assert.equal(checkLinkable(null).reason, 'linkNoRecord');
  assert.equal(r({ running: true, model_name: 'x' }).ok, true);
});

test('文生图这类非对话模型不许接入', () => {
  // 实测：z-image-turbo 的 endpoint_url 是 .../generate，往它发 messages 会拿到
  // 422 {"loc":["body","prompt"],"msg":"Field required"}，而品悟只会显示一句
  // 笼统的「当前模型服务不可用」。更糟的是接入那步会顺手把它设为当前模型，
  // 等于把用户正在用的对话模型也换掉了。所以必须在接入前就拦住。
  const zimage = { running: true, model_name: 'z-image-turbo-bf16', endpoint_url: 'http://127.0.0.1:8000/generate' };
  assert.equal(checkLinkable({ record: zimage }).reason, 'linkNotChatModel');
  // endpoint_url 缺失时也按不可接入处理：宁可让用户看到一句解释，
  // 也不要接进去之后一开新对话就炸。
  assert.equal(checkLinkable({ record: { running: true, model_name: 'x' } }).reason, 'linkNotChatModel');
});

test('isOpenAiChatEndpoint 认完整路径也认基址', () => {
  // 判据用 endpoint_url 而不是 engine_name：引擎名是开放集合（sglang/vllm/
  // diffusers/以后还会有），而"暴露的是哪套 HTTP 接口"才是真正的契约。
  for (const ok of [
    'http://127.0.0.1:30001/v1/chat/completions',
    'http://127.0.0.1:8888/v1/chat/completions/',
    'http://127.0.0.1:8001/v1',
    'http://127.0.0.1:8001/v1/',
  ]) {
    assert.equal(isOpenAiChatEndpoint(ok), true, ok);
  }
  for (const no of [
    'http://127.0.0.1:8000/generate',
    'http://127.0.0.1:8000/v1/images/generations',
    'http://127.0.0.1:8000/v2/chat/completions',
    '',
    null,
    undefined,
    123,
  ]) {
    assert.equal(isOpenAiChatEndpoint(no), false, String(no));
  }
});

test('卸载后只清指向本机 LMM 的那些行，不许误伤', () => {
  // 这条判定决定「删掉用户哪些模型配置」，误伤是不可接受的。
  // 光按 model 名匹配会连带删掉用户自己配的、指向别处同名模型的行——
  // 同名很常见，云端和本地跑同一个开源模型是最普通的用法。
  const rows = [
    { id: 'a', model: 'qwen3.8-27b-nvfp4', base_url: 'http://127.0.0.1:8090/v1' },
    { id: 'b', model: 'qwen3.8-27b-nvfp4', base_url: 'http://localhost:8090/v1' },
    // 换过网关端口之后接进去的行：端口对不上，但它仍然指向本机 LMM，一样要清
    { id: 'c', model: 'qwen3.8-27b-nvfp4', base_url: 'http://127.0.0.1:9999/v1' },
    // 同名但指向别处：用户自己配的，绝不能动
    { id: 'keep-1', model: 'qwen3.8-27b-nvfp4', base_url: 'https://api.example.com/v1' },
    { id: 'keep-2', model: 'qwen3.8-27b-nvfp4', base_url: 'http://10.214.74.210:8090/v1' },
    // 名字不同：不相干
    { id: 'keep-3', model: '别的模型', base_url: 'http://127.0.0.1:8090/v1' },
  ];
  assert.deepEqual(
    findLinkedRows(rows, 'qwen3.8-27b-nvfp4').map((r) => r.id),
    ['a', 'b', 'c']
  );
});

test('findLinkedRows：入参脏了就返回空，不许抛', () => {
  // savedModels 来自 bridge 快照，可能还没加载好。
  assert.deepEqual(findLinkedRows(null, 'x'), []);
  assert.deepEqual(findLinkedRows([], 'x'), []);
  assert.deepEqual(findLinkedRows([{ model: 'x' }], ''), [], '没有 model_name 时一行都不能删');
  assert.deepEqual(findLinkedRows([null, undefined, { model: 'x' }], 'x'), [], '缺 base_url 的不算');
});

test('isLocalGatewayUrl：认回环地址，不认局域网地址', () => {
  // 详情页展示给外部 Agent 的是局域网地址（10.214.74.210:8090），
  // 那种行可能是用户手动加的，不在清理范围内。
  for (const ok of ['http://127.0.0.1:8090/v1', 'http://localhost:8090/v1', 'http://[::1]:8090/v1']) {
    assert.equal(isLocalGatewayUrl(ok), true, ok);
  }
  for (const no of ['http://10.214.74.210:8090/v1', 'https://api.example.com/v1', '不是URL', '', null, 42]) {
    assert.equal(isLocalGatewayUrl(no), false, String(no));
  }
});
