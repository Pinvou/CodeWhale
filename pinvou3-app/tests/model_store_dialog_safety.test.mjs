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
  // The update confirmation has its own title; this contract guards the
  // persistent detail title across error/loading/success branches.
  const titles = DETAIL_DIALOG.match(/<MsDialogTitle id=\{TITLE_ID\}/g) || [];
  assert.equal(
    titles.length,
    1,
    '标题栏出现了多处或一处也没有 —— 它必须常驻在分支之上，右上角的关闭按钮就在里面'
  );
  const titleAt = DETAIL_DIALOG.indexOf('<MsDialogTitle id={TITLE_ID}');
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
// 要不要用」——而卡片脚本只存在于商店服务器上，不随任何 git 仓库走，所以宿主这边
// 在任何一个方向上都没有资格下断言（2026-09-21 在架那 7 张都接了 LMM_IMPORT_DIRS，
// 但那是那一批的情况，不是对将来每一张卡的保证）。
//
// 这条文案在两个方向上各翻过一次车，两边都要钉死：
//   - 「会被自动找到并复用」 -> 对没接这个约定的卡片是假的；
//   - 「当前版本不会自动复用」 -> 对接了的卡片是假的。
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

// 必须把「谁来决定复用」这件事说出来，否则这句话读起来又像是宿主在承诺。
//
// 这里原来要求两套卡片（正式商店 / 内置测试商店）都点到名——那是因为当时真有两个
// 数据源、行为还不一样。内置测试商店 2026-09-30 拿掉之后只剩一个数据源，"点名两套"
// 已经无从谈起；约束改成钉住那句归属本身，它才是这条文案不撒谎的真正理由。
const DEFERS_TO_THE_CARD = {
  zh: /由卡片脚本决定，不由品悟决定/,
  en: /up to the card script, not up to Pinvou/i,
  ja: /品悟ではなくカードのスクリプトが決めます/,
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
            + '而卡片脚本不在这个仓库里，任何一个方向的断言都可能对用户撒谎'
        );
      }
    }

    assert.match(
      literal(text, 'storageScanHint'),
      DEFERS_TO_THE_CARD[lang],
      `${lang}.js 的 storageScanHint 没把「复用与否由卡片脚本决定」说出来 —— `
        + '不说清归属，这句话读起来就是宿主在承诺复用'
    );

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

// 这里**刻意不去扫任何一份卡片脚本副本**有没有消费 LMM_IMPORT_DIRS。
// 曾经按 `data/test-store/`（内置测试商店的旧副本）写过那么一条，前提是错的：真正
// 决定用户看到什么的是**正式商店服务器上那份 seed 脚本**，它不在任何 git 仓库里。
// 内置那份副本 2026-09-30 已经整个删掉，连"拿它推断线上行为"这个诱惑都没了。
// 上面那组「两个方向都不许断言」的约束不依赖任何一份脚本副本，才是稳的。


// ——— 22. 权重校验档位：不达标的卡必须在详情页认得出来 ———
//
// 内置卡的权重完整性差着两个数量级：`qwen38-27b-sglang0519` 会拿卡片自带的
// weights.sha256 把 23GB 逐个文件算一遍；`ornith15-bf16` / `qwen38-27b-nvfp4-mtp`
// 的"下载完成"判据只是分片文件存不存在，revision 还是浮动的 main。在加这条黄条
// 之前，两类卡在详情页长得一模一样——用户没有任何自我保护的依据。
//
test('权重校验黄条已按产品决定整体移除', () => {
  // 2026-10-22 用户验证反馈：所有详情卡的黄色区块「一点存在的意义都没有」。
  // 组件、逻辑函数、i18n 键全部删除；此测试钉住「不再回流」。
  const STORE_UI = read('src', 'features', 'model-store', 'model-store-ui.jsx');
  const LOGIC = read('src', 'features', 'model-store', 'model-store-logic.js');
  assert.ok(!STORE_UI.includes('WeightVerifyNotice'), '组件不应回流');
  assert.ok(!LOGIC.includes('weightVerificationNotice'), '判定函数不应回流');
  for (const key of ['weightVerifyTitle', 'weightVerifyNone', 'weightVerifyManifest', 'weightVerifyAdvice']) {
    assert.ok(!read('src', 'shared', 'i18n', 'zh.js').includes(key), `i18n 键 ${key} 不应回流`);
  }
});
