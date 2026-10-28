import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '..');
const read = file => fs.readFileSync(path.join(root, file), 'utf8');
const count = (source, pattern) => (source.match(pattern) || []).length;

// 正向形状 pin 必须在去注释文本上运行（与 scheduled_task_tool_card、
// ui_language_coverage 同一 stripComments 纪律）：否则把带 pin 的活代码行
// 注释掉也能满足匹配。以下 pin 全部为正向，过度去注释只会响亮失败。
// 先剥行注释再剥块注释：行注释里可能出现 `/*.png` 之类的文本，若先跑块
// 注释正则会从这里误开一个块并吞掉后续活代码；本仓三个被检文件的块注释
// 均不含 `//`，顺序反转安全（若未来引入，过度去注释只会让 pin 响亮失败）。
const stripComments = text => text
  .split('\n')
  .map(line => line.replace(/\/\/.*$/, ''))
  .join('\n')
  .replace(/\/\*[\s\S]*?\*\//g, comment => comment.replace(/[^\n]/g, ''));
const readStripped = file => stripComments(read(file));

// 负向锁：role="button" 折叠头行内嵌着真 <button>（shell 任务取消、产物定位、
// 浏览器标签关闭）。行级 onKeyDown 一旦对 Enter/Space 无条件 preventDefault，
// 冒泡上来的按键会被取消默认激活并转触发行自身的动作——嵌套按钮键盘不可用
// （任一处回归即红）。每个此类 handler 必须先以 target !== currentTarget 忽略
// 来自嵌套交互元素的事件，再接管行自身的 Enter/Space。
test('role=button rows with nested buttons guard Enter/Space against bubbled key events', () => {
  const toolRenderers = readStripped('src/features/tools/tool-renderers.jsx');
  // 守卫前置形状：target 判定必须是 handler 第一条语句，任何副作用
  // （包括 preventDefault）都不得泄漏到冒泡按键上。
  const guardedHandler = /onKeyDown=\{\(event\) => \{\s+if \(event\.target !== event\.currentTarget\) return;\s+if \(event\.key === 'Enter' \|\| event\.key === ' '\) \{\s+event\.preventDefault\(\);/g;
  assert.equal(
    count(toolRenderers, guardedHandler),
    3,
    'all three tool-card collapse headers (timeline/quiet/output) must ignore bubbled keys first, then handle their own Enter/Space',
  );
  assert.equal(
    count(toolRenderers, /if \(event\.target !== event\.currentTarget\) return;/g),
    3,
    'exactly three guarded handlers exist; bump this count together with the shape pin when adding a guarded header',
  );
  // 本树的 expanded 即“显示真相”：shouldAutoExpand（live shell output / 卡片类
  // 工具）通过初始 state 与 effect 直接把它撑开，因此 aria、chevron 与 quiet
  // 包裹层边框都跟随 expanded，不会在内容可见时低报。
  assert.equal(
    count(toolRenderers, /aria-expanded=\{expanded\}/g),
    3,
    'every collapse header must expose the visible expanded state to assistive tech',
  );
  assert.equal(
    count(toolRenderers, /\$\{expanded \? 'rotate-180' : ''\}/g),
    3,
    'every collapse header chevron must reflect the visible expanded state',
  );
  assert.equal(
    count(toolRenderers, /expanded \? `rounded-\[12px\] overflow-hidden border/g),
    1,
    'the quiet card wrapper must show its border whenever the detail is visibly open',
  );

  const artifactsPanel = readStripped('src/features/artifacts/ArtifactsPanel.jsx');
  assert.match(
    artifactsPanel,
    /onKeyDown=\{\(e\) => \{\s+if \(e\.target !== e\.currentTarget\) return;\s+if \(e\.key === 'Enter' \|\| e\.key === ' '\) \{\s+e\.preventDefault\(\);\s+preview\(a\);/,
    "the artifacts row hosts the locate <button>: ignore bubbled keys first and keep the row's own Enter/Space activation",
  );
  assert.equal(
    count(artifactsPanel, /group-hover:opacity-100 group-focus-within:opacity-100/g),
    2,
    'both hover-only locate buttons (list row and switcher menu) must also reveal on keyboard focus',
  );

  const browserView = readStripped('src/features/browser/BrowserView.jsx');
  assert.match(
    browserView,
    /onKeyDown=\{\(event\) => \{\s+if \(event\.target !== event\.currentTarget\) return;\s+if \(event\.key !== 'Enter' && event\.key !== ' '\) return;\s+event\.preventDefault\(\);/,
    "the browser tab row hosts the close <button>: the guard must precede everything, and the tab's own Enter/Space activation must survive",
  );
  assert.equal(
    count(browserView, /group-hover:opacity-100 group-focus-within:opacity-100/g),
    1,
    'the hover-only tab close button must also reveal on keyboard focus',
  );
});
