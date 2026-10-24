// 设备管理外部模型行的 armed/busy 身份契约（外部模型列表的业务身份键）。
// 错一个，一次 arm 会同时打开两个确认面板共享同一个「删除模型文件」勾选框，
// 或翻转对在静默登记换 id 时静默解除武装——都极难从界面察觉。
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { externalRowIdentity } from '../src/features/device-manager/externalRowIdentity.js';

const VIEW_PATH = new URL('../src/features/device-manager/DeviceManagerView.jsx', import.meta.url);

test('非端点型类别：同模型不同端点是两个身份（外部评审 2026-10-23 P2 场景）', () => {
  // 存量两条手写记录：同名同模型、分别挂在 8000 与 8001 —— 设计文档定义的
  // 两个独立服务。行键必须不同，一次 arm 只打开其中一个确认面板。
  const a = { id: 'm-a', name: 'qwen3', kind: 'openai_endpoint', base_url: 'http://127.0.0.1:8000/v1' };
  const b = { id: 'm-b', name: 'qwen3', kind: 'openai_endpoint', base_url: 'http://127.0.0.1:8001/v1' };
  assert.notEqual(externalRowIdentity(a), externalRowIdentity(b));
  // 同端点才同身份。
  assert.equal(externalRowIdentity(a), externalRowIdentity({ ...a, id: 'm-a2' }));
});

test('非端点型类别：显式 model 压过展示名；缺 base_url 的存量行是独立身份', () => {
  const named = { name: '我的别名', kind: 'vllm', model: 'qwen3-8b', base_url: 'http://127.0.0.1:8000/v1' };
  assert.notEqual(externalRowIdentity(named), externalRowIdentity({ ...named, model: null }));
  // 合法载荷里端点型必有 base_url；手写/旧版本行可能缺失——缺省端点不与任何
  // 显式端点撞键。
  const legacy = { name: 'qwen3', kind: 'openai_endpoint' };
  assert.notEqual(externalRowIdentity(legacy), externalRowIdentity({ ...legacy, base_url: 'http://127.0.0.1:8000/v1' }));
  // 身份键用 JSON 编码二元组：模型名里出现 '@' 或 URL 字符不能伪造他行键。
  assert.notEqual(
    externalRowIdentity({ name: 'x', kind: 'vllm', model: 'qwen3@http://y', base_url: '' }),
    externalRowIdentity({ name: 'x', kind: 'vllm', model: 'qwen3', base_url: 'http://y' }),
  );
});

test('ollama：翻转对共享身份且端点不参与（静默登记换 id 时 armed/busy 不掉）', () => {
  // 未登记 daemon 行（自定义端口守护进程）与其注册行：身份必须相等——
  // confirmRemove 的静默登记会换 id 重挂列表，端点进身份会让自定义端口场景
  // 在这一瞬静默解除武装。
  const daemon = { id: 'ollama-1', name: 'qwen3:8b', kind: 'ollama', base_url: 'http://127.0.0.1:11500/v1', registered: false };
  const registered = { id: 'm-reg', name: 'qwen3:8b', kind: 'ollama', base_url: 'http://127.0.0.1:11500/v1', registered: true };
  assert.equal(externalRowIdentity(daemon), externalRowIdentity(registered));
  // 后端回填前的存量注册行 base_url 为空，身份仍不变（端点被忽略的直接证据）。
  assert.equal(externalRowIdentity(registered), externalRowIdentity({ ...registered, base_url: null }));
  // 裸名折叠 :latest、大小写折叠（与后端 registry_identity 同一把尺子）。
  assert.equal(externalRowIdentity({ name: 'Qwen3', kind: 'ollama' }), externalRowIdentity({ name: 'qwen3:latest', kind: 'ollama' }));
  // 别名记录按有效模型计。
  assert.equal(externalRowIdentity({ name: 'foo:latest', kind: 'ollama', model: 'BAR' }), externalRowIdentity({ name: 'bar:latest', kind: 'ollama' }));
});

test('视图接线锚：armed/busy 键必须走 externalRowIdentity（改回内联键必红）', () => {
  // 与 chat_send_session_contract 同款 source-shape 锚：函数本体直打测试照绿
  // 也能被这里抓住接线回退。
  const source = readFileSync(VIEW_PATH, 'utf8');
  assert.match(source, /import \{ externalRowIdentity \} from '\.\/externalRowIdentity\.js'/);
  assert.match(source, /const rowIdentity = externalRowIdentity;/);
});
