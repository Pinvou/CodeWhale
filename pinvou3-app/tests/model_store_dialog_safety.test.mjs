#!/usr/bin/env node
// 模型商店两个弹窗的「失败态」契约，外加存储文案与 Rust 侧口径的一致性。
//
// 这三件事共同点是**失败时才看得见**，而失败路径没人手测：
//   1. 存储设置读取失败 -> 表单是空的，保存会把已存的查找目录清成空列表；
//   2. 详情页 load() 失败 -> 整页塌成一行红字，没有标题栏也没有重试入口；
//   3. 三语文案承诺"本机副本会被复用"，而卡片脚本根本不读那两个环境变量。
// 都属于"界面看着正常、事实是反的"那一类，只能靠契约钉住。
//
// 按源码文本断言：这三个文件是 JSX / 带 i18n 词典的大对象，node --test 没有
// loader 也没有 DOM。仓库里已有先例（tests/model_store_guide_copy.test.mjs 就是
// 这么钉 ModelStoreCards.jsx 的）。
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (...parts) => fs.readFileSync(path.join(ROOT, ...parts), 'utf8');

const STORAGE_DIALOG = read('src', 'features', 'model-store', 'StorageSettingsDialog.jsx');
const DETAIL_DIALOG = read('src', 'features', 'model-store', 'ModelDetailDialog.jsx');
const CARD_DIALOG = read('src', 'features', 'model-store', 'StoreCardDialog.jsx');
const STORE_UI = read('src', 'features', 'model-store', 'model-store-ui.jsx');
const LANGS = ['zh', 'en', 'ja'];
const DICTS = new Map(LANGS.map((lang) => [lang, read('src', 'shared', 'i18n', `${lang}.js`)]));

/**
 * 取出词典里某个键的字符串字面量值。
 *
 * 只认单引号字面量（这几个键都是），模板串和函数值取不到——取不到就让调用方
 * 的断言失败，而不是悄悄返回空串当作"通过"。
 * @param {string} text - 词典源码。
 * @param {string} key - 键名。
 * @returns {string|null} 值，取不到时 null。
 */
function literal(text, key) {
  const match = new RegExp(`${key}:\\s*'((?:[^'\\\\]|\\\\.)*)'`).exec(text);
  return match ? match[1] : null;
}

// ——— 20. 存储设置：读取失败时不许保存 ———

test('存储设置的保存按钮绑定 canSave，而不是只看 modelsDir 非空', () => {
  assert.match(
    STORAGE_DIALOG,
    /disabled=\{!canSave\}/,
    '保存按钮的 disabled 不再是 canSave —— 只看 modelsDir 非空的话，读取失败后随手'
      + '填个目录点保存，空的 importText 会被解析成 [] 把已存的查找目录静默清空'
  );
  const canSave = /const canSave = ([^;]+);/.exec(STORAGE_DIALOG);
  assert.ok(canSave, 'StorageSettingsDialog.jsx 里找不到 canSave —— 改名了就把这个测试一起改');
  // loaded 同时覆盖 modelsDir 和 importText 两个框：它们是同一次请求填的，
  // 少了它就等于默许"空 importText 也能提交"。
  assert.match(canSave[1], /\bloaded\b/, 'canSave 没把加载态算进去 —— importText 的加载态会被漏掉');
  assert.match(canSave[1], /modelsDir\.trim\(\)/, 'canSave 丢了 modelsDir 非空的判据');
});

test('loaded 只在读取成功后置真，且每次打开都先归假', () => {
  assert.match(
    STORAGE_DIALOG,
    /\.then\(\(data\) => \{[\s\S]*?setLoaded\(true\);[\s\S]*?\}\)/,
    'setLoaded(true) 不在 fetchStorageSettings 的成功分支里'
  );
  assert.match(
    STORAGE_DIALOG,
    /if \(!open\) return undefined;[\s\S]*?setLoaded\(false\);/,
    '每次打开必须把 loaded 归假：组件不随弹窗卸载，上一次的 loaded 会留到下一次'
  );
  // catch 分支不许碰 loaded（保持假），否则读取失败照样能存。
  const katch = /\.catch\(\(e\) => \{([\s\S]*?)\}\);/.exec(STORAGE_DIALOG);
  assert.ok(katch, '找不到读取失败的 catch 分支');
  assert.doesNotMatch(katch[1], /setLoaded\(true\)/, '读取失败的分支把 loaded 置真了');
});

test('保存失败不影响 loaded —— 用户要能改完再存一次', () => {
  const submit = /async function submit\(\)([\s\S]*?)\n  \}/.exec(STORAGE_DIALOG);
  assert.ok(submit, '找不到 submit()');
  assert.doesNotMatch(
    submit[1],
    /setLoaded\(false\)/,
    'submit() 把 loaded 置假了：保存失败后表单是用户自己填的内容，禁用保存会让他无法重试'
  );
});

// ——— 21. 详情页：标题栏常驻 + 重试 ———

test('详情页的标题栏在 error/loading/正常三个分支之上，只有一处', () => {
  const titles = DETAIL_DIALOG.match(/<MsDialogTitle/g) || [];
  assert.equal(
    titles.length,
    1,
    '标题栏出现了多处或一处也没有 —— 它必须常驻在分支之上，右上角的关闭按钮就在里面'
  );
  const titleAt = DETAIL_DIALOG.indexOf('<MsDialogTitle');
  const errorBranchAt = DETAIL_DIALOG.indexOf('{error ? (');
  assert.ok(errorBranchAt > 0, '找不到 error 分支');
  assert.ok(
    titleAt < errorBranchAt,
    '标题栏落在 error 分支之后 —— 加载失败时整个弹窗又会只剩一行红字，连关闭按钮都没有'
  );
  // 标题拿不到卡片名时要有兜底，不能渲染出空标题。
  assert.match(DETAIL_DIALOG, /record\?\.card_name \|\| copy\.detailTitle/, '标题少了兜底');
});

test('详情页的错误态有重试按钮，重试就是再跑一次 load()', () => {
  assert.match(DETAIL_DIALOG, /data-testid="model-store-detail-retry"/, '错误态没有重试按钮');
  const retry = /const retry = async \(\) => \{([\s\S]*?)\n  \};/.exec(DETAIL_DIALOG);
  assert.ok(retry, '找不到 retry()');
  assert.match(retry[1], /await load\(\)/, 'retry() 没有重新调用 load()');
  // 重试自己一个忙态，不借用 busy（那是建 Key / 接入品悟的）。
  assert.match(retry[1], /setRetrying\(true\)/, 'retry() 没有自己的忙态');
  assert.match(retry[1], /finally \{\s*setRetrying\(false\);/, 'retry() 的忙态没有在 finally 里归位');
});

test('详情页错误态同时留有关闭入口', () => {
  const errorBranch = /\{error \? \(([\s\S]*?)\) : null\}/.exec(DETAIL_DIALOG);
  assert.ok(errorBranch, '找不到 error 分支的内容');
  assert.match(errorBranch[1], /copy\.detailClose/, '错误态里没有关闭按钮');
});

// ——— 14. 存储文案不许替卡片脚本下断言 ———
//
// 复用与否**不由宿主决定**。宿主只答「去哪找」（LMM_IMPORT_DIRS 下发扫描根、
// LMM_IMPORT_CANDIDATES 交出命中、两者都写进运行日志），卡片脚本答「这份对不对、
// 要不要用」。而两套卡片行为不同：
//   - 正式商店的远端卡片已消费 LMM_IMPORT_DIRS（2026-09-21 上架生产，在架 7 张全接）；
//   - 内置测试商店的卡片（data/test-store/）没接，仍会联网下载。
//
// 所以这条文案在两个方向上各翻过一次车，两边都要钉死：
//   - 「会被自动找到并复用」 -> 对内置测试卡片是假的；
//   - 「当前版本不会自动复用」 -> 对正式商店的用户是假的。
// 唯一安全的写法是描述宿主做了什么，再把复用与否归给卡片。
const BANNED = {
  zh: [
    '会被自动找到并复用', '不再联网下载', // 过度承诺
    '不会自动复用', '暂不自动复用', // 反向断言
  ],
  en: [
    'picked up automatically', 'instead of downloading',
    'does not reuse', 'do not reuse',
  ],
  ja: [
    '自動で見つけて再利用', 'ダウンロードせずそのまま使います',
    '自動で再利用されず', '自動で再利用しない',
  ],
};

// 两套卡片都要点到名，否则又变成替其中一套下断言。
const BOTH_TRACKS = {
  zh: [/正式商店/, /测试商店/],
  en: [/official store/i, /test store/i],
  ja: [/正式ストア/, /テストストア/],
};

// 换了权重目录之后，旧目录**不会**自动进扫描范围（`import_scan::roots()` 收的是
// 当前权重目录、暂存目录、~/models、下载/桌面和可移动介质），所以这条必须把
// 「填到额外的查找目录里」这个补救动作说出来，否则等于让用户以为旧副本还在被找。
const POINTS_AT_REMEDY = {
  zh: /额外的查找目录/,
  en: /Additional search directories/i,
  ja: /追加の検索ディレクトリ/,
};

for (const lang of LANGS) {
  test(`${lang} 的存储文案不替卡片脚本下任何一个方向的断言`, () => {
    const text = DICTS.get(lang);
    for (const key of ['storageModelsDirHint', 'storageScanHint']) {
      const value = literal(text, key);
      assert.ok(value, `${lang}.js 里取不到 ${key}`);
      for (const phrase of BANNED[lang]) {
        assert.ok(
          !value.includes(phrase),
          `${lang}.js 的 ${key} 里有绝对化措辞「${phrase}」：复用与否取决于卡片脚本，`
            + '正式商店的卡片会复用、内置测试卡片不会，任何一边的断言都会对另一边撒谎'
        );
      }
    }

    const scanHint = literal(text, 'storageScanHint');
    for (const re of BOTH_TRACKS[lang]) {
      assert.match(scanHint, re, `${lang}.js 的 storageScanHint 没说清两套卡片的差别`);
    }

    assert.match(
      literal(text, 'storageModelsDirHint'),
      POINTS_AT_REMEDY[lang],
      `${lang}.js 的 storageModelsDirHint 没告诉用户旧目录要自己加进查找目录`
    );
  });
}

test('新增的两个文案键三语齐全', () => {
  for (const key of ['storageLoadFailedHint', 'detailTitle']) {
    for (const lang of LANGS) {
      assert.ok(
        literal(DICTS.get(lang), key),
        `${lang}.js 里缺 ${key} —— 缺的那一语会渲染成 undefined`
      );
    }
  }
});

// 这里**刻意不去扫** `data/test-store/` 下的脚本有没有消费 LMM_IMPORT_DIRS。
// 曾经写过那么一条，前提是错的：真正决定用户看到什么的是**正式商店服务器上那份
// seed 脚本**（不在这两个 git 仓库里），仓库内置的那份只是测试商店用的旧副本，
// 两者本来就不同步。拿仓库里的副本去推断线上行为，只会把文案又带回某一个方向的
// 断言。上面那组「两边都不许断言」的约束不依赖任何一份脚本副本，才是稳的。


// ——— 22. 权重校验档位：不达标的卡必须在详情页认得出来 ———
//
// 内置卡的权重完整性差着两个数量级：`qwen38-27b-sglang0519` 会拿卡片自带的
// weights.sha256 把 23GB 逐个文件算一遍；`ornith15-bf16` / `qwen38-27b-nvfp4-mtp`
// 的"下载完成"判据只是分片文件存不存在，revision 还是浮动的 main。在加这条黄条
// 之前，两类卡在详情页长得一模一样——用户没有任何自我保护的依据。
//
// 判据落在纯逻辑里（weightVerificationOf / weightVerificationNotice，见
// model_store_logic.test.mjs）；这里只钉"两个详情弹窗真的把它渲染出来了"，
// 那是纯逻辑测不到、而且掉了也不会有人发现的一环。

test('两个详情弹窗都渲染 WeightVerifyNotice', () => {
  // 「我的模型」那个详情页：档位来自商店卡片，所以必须把 card 传进去。
  // 传成 detail/record 的话永远是 undefined —— 于是每张卡都出黄条，等于没做。
  assert.match(
    DETAIL_DIALOG,
    /<WeightVerifyNotice\s+card=\{card\}/,
    'ModelDetailDialog 没把 card 传给 WeightVerifyNotice —— 库记录里没有档位这一列'
  );
  // 商店那个详情页更要紧：它是下载决策点。等下完几十 GB 再说就晚了。
  assert.match(
    CARD_DIALOG,
    /<WeightVerifyNotice\s+card=\{card\}/,
    'StoreCardDialog 不出这条提示的话，用户是在付出几十 GB 之后才知道的'
  );
});

test('WeightVerifyNotice 对达标卡什么都不渲染，且用的是提示色不是危险色', () => {
  assert.match(
    STORE_UI,
    /const notice = weightVerificationNotice\(card\);[\s\S]{0,40}?if \(!notice\) return null;/,
    'WeightVerifyNotice 丢了"达标就不渲染"那一步 —— 每张卡都挂一条，黄条就成了背景噪音'
  );
  // 这是知情权不是恐吓：用户不该因为这条提示就不敢用几张完全正常的卡。
  assert.match(STORE_UI, /#FEF7E0/, '黄条换掉了模块里既有的提示底色');
  const start = STORE_UI.indexOf('function WeightVerifyNotice');
  assert.ok(start >= 0, 'model-store-ui.jsx 里找不到 WeightVerifyNotice —— 改名了就把这条测试一起改');
  assert.doesNotMatch(
    STORE_UI.slice(start, STORE_UI.indexOf('function MsToast')),
    /TEXT_DANGER|text-rose|#C5221F/,
    '这条提示被做成了危险色。它说的是"这条下载链路上少了一道防线"，不是"这张卡有问题"'
  );
});

test('黄条的三句话三语齐全，而且不止一句"未校验"', () => {
  for (const key of ['weightVerifyTitle', 'weightVerifyNone', 'weightVerifyManifest', 'weightVerifyAdvice']) {
    for (const lang of LANGS) {
      assert.ok(
        literal(DICTS.get(lang), key),
        `${lang}.js 里缺 ${key} —— 缺的那一语会渲染成 undefined`
      );
    }
  }
  for (const lang of LANGS) {
    const text = DICTS.get(lang);
    // none 和 manifest 必须是两句不同的话：manifest 确实逐文件比对过摘要，
    // 只是参照跟权重同源。把它也说成"没校验"是不实的。
    assert.notEqual(
      literal(text, 'weightVerifyNone'),
      literal(text, 'weightVerifyManifest'),
      `${lang}.js 把两档说成了同一句话`
    );
    // 只说"这意味着什么"不够，还得说"你可以怎么办"，否则留给用户的只有不安。
    assert.ok(
      literal(text, 'weightVerifyAdvice').length > 40,
      `${lang}.js 的 weightVerifyAdvice 短得不像一条可操作的建议`
    );
  }
});
