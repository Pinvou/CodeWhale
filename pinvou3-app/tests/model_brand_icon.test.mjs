// resolveBrandIcon 的匹配链契约测试。
//
// 用例全部取自模型商店实测抓到的 org / 模型名 / 仓库 ID,不用编造值:这个函数存在
// 的唯一理由就是上游元数据不规范,拿规范化的假数据测它等于没测。
//
// 图标资产是 Vite 的资源导入(.svg/.png),Node 解析不了,所以下面注册一个同步
// 模块钩子把资源换成「导出自身文件名」的桩。这样断言可以直接写成 'qwen.svg',
// 既可读,又能钉住「解析到了哪一家」而不是只钉住「非空」。
import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';
import test from 'node:test';

const ASSET = /\.(?:svg|png)(?:\?.*)?$/;

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (!ASSET.test(specifier)) return nextResolve(specifier, context);
    return { url: new URL(specifier, context.parentURL).href, format: 'module', shortCircuit: true };
  },
  load(url, context, nextLoad) {
    if (!ASSET.test(url)) return nextLoad(url, context);
    const filename = decodeURIComponent(url.split('/').pop());
    return { format: 'module', source: `export default ${JSON.stringify(filename)};`, shortCircuit: true };
  },
});

const { resolveBrandIcon } = await import('../src/features/model-store/model-brand-icon.js');

// ── a) org 直接命中厂商图标表 ────────────────────────────────────────
test('org 命中:裸品牌名与带机构后缀的写法必须等价', () => {
  assert.equal(
    resolveBrandIcon({ org: 'Qwen', name: 'Qwen3.8-27B (NVFP4 · SGLang v0.5.19)' }),
    'qwen.svg',
    'Qwen 是最常见的 org,大小写归一后必须直接命中',
  );
  // deepseek-ai 是 Hugging Face 上的真实组织名。图标表的键是裸品牌名 deepseek,
  // 不削 -ai 后缀就会退化到模型名兜底 —— 结果碰巧也对,所以这条要单独钉住,
  // 否则后缀削除逻辑坏掉了也没人发现。
  assert.equal(
    resolveBrandIcon({ org: 'deepseek-ai', name: '' }),
    'deepseek.svg',
    '-ai 后缀必须在查表前削掉',
  );
  // NVIDIA 不在 settings 的「添加模型」目录里(它不提供对话服务),图标只能由
  // 模型商店本地补充。这条守的是「补充表真的被查了」。
  //
  // nvidia 是转发方(见下面那条),org 命中会被降到模型名之后 —— 但模型名什么都
  // 没说时它仍要拿到 NVIDIA 图标,否则 Nemotron 这类 NVIDIA 自家的模型就没图标了。
  assert.equal(
    resolveBrandIcon({ org: 'nvidia', name: '' }),
    'nvidia.png',
    '只发权重的上游厂商要走商店本地补充的图标表',
  );
});

// ── b) org 没用,靠模型名兜底 ────────────────────────────────────────
test('name 兜底:量化发布方 unsloth 必须靠模型名才认得出是 Qwen', () => {
  // unsloth 是做量化再发布的第三方,同一个 org 下什么家的权重都有。
  // 按 org 给它配图标必然张冠李戴,所以 org 一定要落空、由模型名接管。
  assert.equal(
    resolveBrandIcon({
      org: 'unsloth',
      name: 'Qwen3.8-27B (NVFP4 · SGLang v0.5.19)',
      sourceRepoId: 'unsloth/Qwen3.8-27B-NVFP4',
    }),
    'qwen.svg',
  );
  // 同一个 org 换成别家的权重就必须换图标 —— 这才是「不能按 org 配」的证明。
  assert.equal(
    resolveBrandIcon({ org: 'unsloth', name: 'DeepSeek-V4-Flash-Vision-Exp' }),
    'deepseek.svg',
  );
});

test('转发方的 org 排在模型名之后:NVFP4 配方的作者不是模型的厂商', () => {
  // 真实卡片 qwen38fn-nvfp4-single:org 写的是 nvidia,模型却是 Qwen 家的
  // Qwen3.8-Flash-Next,NVIDIA 只是那份 NVFP4 部署配方的作者。
  // org 优先会让这一行显示绿色 NVIDIA 标,而同一列表里 unsloth 那张和 Qwen 自己
  // 那张都是紫色 Qwen 标 —— 同一个模型家族出现两种图标,用户会当成三家的模型。
  assert.equal(
    resolveBrandIcon({
      org: 'nvidia',
      name: 'Qwen3.8-Flash-Next',
      sourceRepoId: 'nvidia/Qwen3.8-Flash-Next-NVFP4',
    }),
    'qwen.svg',
    'NVIDIA 转发的 Qwen 模型要显示 Qwen 图标',
  );
  // 反面:NVIDIA 自己的模型仍然是 NVIDIA 图标。降优先级不等于弃用,
  // 这条一旦挂了,说明有人把 nvidia 从图标表里删掉了事。
  assert.equal(
    resolveBrandIcon({ org: 'nvidia', name: 'Nemotron-5-49B', sourceRepoId: 'nvidia/Nemotron-5-49B' }),
    'nvidia.png',
    'NVIDIA 自产的模型不能因为「nvidia 是转发方」就丢掉图标',
  );
});

test('name 兜底:通义系的 org 与模型名对不上,只能靠关键字表补', () => {
  // Tongyi-MAI 削掉 -mai 后是 tongyi,而图标表的键是 qwen,查不到。
  // 模型名 Z-Image-Turbo 里既没有 qwen 也没有 tongyi,不单列 z-image 就会掉到降级图标。
  assert.equal(
    resolveBrandIcon({ org: 'Tongyi-MAI', name: 'Z-Image-Turbo' }),
    'qwen.svg',
    'Z-Image 是通义出品,应当显示 Qwen 图标',
  );
  // 模型名里直接写 tongyi-mai 的情况也出现过,同样归到 Qwen。
  assert.equal(resolveBrandIcon({ org: '', name: 'Tongyi-MAI-Chat' }), 'qwen.svg');
});

test('name 兜底:与 MonitorView 的关键字表保持同一批结论', () => {
  // 本地模型在监控页和商店页会同时出现,两处认人结论不一致会被用户当成 bug。
  assert.equal(resolveBrandIcon({ name: 'MiniMax-H3' }), 'minimax.svg');
  assert.equal(resolveBrandIcon({ name: 'Kimi-K3-Instruct' }), 'kimi.svg');
  assert.equal(resolveBrandIcon({ name: 'moonshot-v2-128k' }), 'kimi.svg');
  assert.equal(resolveBrandIcon({ name: 'ChatGLM4-9B' }), 'glm.svg');
  assert.equal(resolveBrandIcon({ name: '通义千问-Max' }), 'qwen.svg');
});

test('name 兜底:SGLang 这类运行时后缀不能被误判成 GLM', () => {
  // 关键字表里 glm 排在 qwen 前面,而商店里的名字大量带 `SGLang vX` 后缀。
  // 一旦有人把 glm 的匹配放宽(比如改成按词切分再模糊比),这条会立刻炸。
  assert.equal(
    resolveBrandIcon({ org: '', name: 'Qwen3.8-27B (NVFP4 · SGLang v0.5.19)' }),
    'qwen.svg',
  );
});

test('name 兜底:ornith 走本地补充的厂商图标,不能被转发方的 owner 段顶掉', () => {
  // 图标取自 ornith-ai 自己仓库里的 assets/ornith_logo.png(只裁圆形标记)。
  // sourceRepoId 那一段刻意写成 nvidia 转发的版本:模型名已经报出是 Ornith 家的,
  // 就不该再让 owner 段把它认成 NVIDIA。这条同时钉住「有图标」和「命中即短路」。
  assert.equal(
    resolveBrandIcon({
      org: 'ornith-ai',
      name: 'Ornith-1.5-35B-A3B',
      sourceRepoId: 'nvidia/Ornith-1.5-35B-A3B-FP8',
    }),
    'ornith.png',
  );
  // org 直接命中也是同一张图 —— 补充表的键是削掉 -ai 之后的裸品牌名。
  assert.equal(resolveBrandIcon({ org: 'ornith-ai', name: '' }), 'ornith.png');
});

// ── c) org 与模型名都没用,靠仓库 ID 的 owner 段 ──────────────────────
test('sourceRepoId 兜底:只取 owner 段,不拿整个 ID 去匹配', () => {
  // 本地上传的条目常常只有一个内部代号当 org、模型名也被改成内部叫法,
  // 唯一还带品牌线索的就是源仓库 ID 的 owner 段。
  assert.equal(
    resolveBrandIcon({
      org: 'H3C MegaCube 本地验证',
      name: 'megacube-verify-001',
      sourceRepoId: 'deepseek-ai/DeepSeek-V4-Flash',
    }),
    'deepseek.svg',
  );
  // owner 段同样要过后缀削除与大小写归一。
  assert.equal(resolveBrandIcon({ sourceRepoId: 'Tongyi-MAI/Z-Image-Turbo' }), null,
    'owner 段 tongyi 不在图标表里,这一步不该凭空造出 Qwen —— 该由模型名负责');
  assert.equal(resolveBrandIcon({ sourceRepoId: 'nvidia/Ornith-1.5-35B-A3B-FP8' }), 'nvidia.png');
});

// ── d) 全未命中 / 脏输入 ────────────────────────────────────────────
test('全未命中返回 null,由调用方渲染降级图标', () => {
  assert.equal(
    resolveBrandIcon({ org: 'H3C MegaCube 本地验证', name: 'megacube-verify-001' }),
    null,
    '内部验证条目没有品牌可言,必须干净地降级而不是硬套一个图标',
  );
  assert.equal(resolveBrandIcon({}), null);
  assert.equal(resolveBrandIcon(), null, '调用方可能在数据到位前就先渲染一次');
  assert.equal(resolveBrandIcon({ org: null, name: undefined, sourceRepoId: '' }), null);
});

test('原型链上的键不得被当成图标 URL', () => {
  // org 是上游元数据,不是我们能约束的输入。直接取下标会让 constructor/toString
  // 摸到 Object.prototype 上的函数,再被塞进 <img src>,现象是图标区域莫名空白。
  for (const org of ['constructor', 'toString', 'hasOwnProperty', '__proto__', 'valueOf']) {
    assert.equal(resolveBrandIcon({ org }), null, `org=${org} 必须落空`);
    assert.equal(resolveBrandIcon({ sourceRepoId: `${org}/whatever` }), null);
  }
});
