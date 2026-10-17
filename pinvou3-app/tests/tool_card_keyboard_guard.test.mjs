import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '..');
const read = file => fs.readFileSync(path.join(root, file), 'utf8');

// 负向锁：role="button" 折叠头行内嵌着真 <button>（shell 任务取消、产物定位）。
// 行级 onKeyDown 一旦对 Enter/Space 无条件 preventDefault，冒泡上来的按键会
// 被取消默认激活并转触发行自身的动作——嵌套按钮键盘不可用（回归即红）。
// 每个此类 handler 必须先以 target !== currentTarget 忽略来自嵌套交互元素的
// 事件，再接管行自身的 Enter/Space。
test('role=button rows with nested buttons guard Enter/Space against bubbled key events', () => {
  const toolRenderers = read('src/features/tools/tool-renderers.jsx');
  const guardedInToolRenderers = toolRenderers.match(/if \(event\.target !== event\.currentTarget\) return;/g) || [];
  assert.ok(
    guardedInToolRenderers.length >= 3,
    'all three tool-card collapse headers (timeline/quiet/output) must ignore keys from nested buttons',
  );
  assert.equal(
    (toolRenderers.match(/aria-expanded=\{expanded\}/g) || []).length,
    3,
    'every collapse header must expose its expanded state to assistive tech',
  );

  const artifactsPanel = read('src/features/artifacts/ArtifactsPanel.jsx');
  assert.match(
    artifactsPanel,
    /onKeyDown=\{\(e\) => \{ if \(e\.target !== e\.currentTarget\) return;/,
    'the artifacts row hosts the locate <button>; its key handler must ignore bubbled keys',
  );
  assert.match(
    artifactsPanel,
    /group-hover:opacity-100 group-focus-within:opacity-100/,
    'the hover-only locate button must also reveal on keyboard focus',
  );
});
