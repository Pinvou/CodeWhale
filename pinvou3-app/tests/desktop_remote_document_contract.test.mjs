import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const requirements = readFileSync(new URL('../../docs/Windows双端远程控制需求文档.md', import.meta.url), 'utf8');
const architecture = readFileSync(new URL('../../docs/Windows双端远程控制代码架构.md', import.meta.url), 'utf8');
const currentArchitecture = architecture.split('## 历史设计附录')[0];
const section = (title, nextTitle) => requirements.split(title)[1]?.split(nextTitle)[0] || '';

test('current desktop requirements agree with the no-development-compatibility architecture', () => {
  assert.match(currentArchitecture, /历史设计中的兼容迁移计划已废弃，不实施、不作为后续技术债/u);
  const rule = requirements.split('\n').find(line => line.startsWith('| RC-T-025 |'));
  assert.match(rule, /直接移除，不提供兼容代理、旧格式迁移、双写或稳定版本保留窗口/u);
  const compatibility = section('## 15.', '## 16.');
  assert.match(compatibility, /旧 `RemoteHistoryStore` 格式不提供迁移或兼容读取/u);
  assert.match(compatibility, /当前 outbox\/投影的原子持久化、幂等与崩溃恢复必须独立验收/u);
  assert.doesNotMatch(compatibility, /迁移必须先在|旧源数据至少在|保留一个版本的稳定兼容窗口/u);
});

test('current journeys and DoD do not require removed history migration fixtures', () => {
  const journeys = section('### 18.3', '### 18.4');
  assert.match(journeys, /24\. 对旧远程正文入口、旧 Command 和独立旧历史存储执行删除验收/u);
  assert.match(journeys, /27\. 使用当前 `SessionStore.RemoteProxy` fixture/u);
  assert.doesNotMatch(journeys, /执行兼容窗口验收|旧 `RemoteHistoryStore` fixture/u);
  assert.match(requirements, /不实施兼容代理、旧格式迁移、双写或保留窗口，已发布 Web v2 回归和安全迁移不变/u);
});

test('unit, integration and risk requirements also remove old migration obligations', () => {
  const unit = section('### 18.1', '### 18.2');
  const integration = section('### 18.2', '### 18.3');
  const risk = section('## 20.', '## 21.');
  assert.match(unit, /当前 `SessionStore.RemoteProxy` 投影与 outbox 的原子持久化/u);
  assert.match(unit, /已删除旧远程 API 无残余调用方/u);
  assert.match(integration, /旧存储\/适配器已删除且不能被功能开关恢复/u);
  assert.match(risk, /删除未发布旧入口/u);
  for (const current of [unit, integration, risk]) {
    assert.doesNotMatch(current, /历史迁移的 schema\/migration revision|兼容代理只调用|迁移前、迁移中|切换后的旧适配器|旧兼容入口重试|旧入口只做代理/u);
  }
});

test('released Legacy Web enrollment and safe rollback obligations stay mandatory', () => {
  assert.match(currentArchitecture, /保留已发布的 Legacy Web v2 行为/u);
  const compatibility = section('## 15.', '## 16.');
  assert.match(compatibility, /现有 WebUI v2 分享链接、二维码和浏览器访问功能保持兼容/u);
  assert.match(compatibility, /最低回滚版本必须原生支持 registry 门禁/u);
  assert.match(compatibility, /不得删除其数据或绕过 enrollment/u);
});

test('reviewed A18 version entries survive and A19 corrections append instead of renumbering', () => {
  assert.ok(requirements.includes('| 1.8 | 2026-10-01 | 记录 Finished receipt 固定向量勘误，澄清 reservation 原子提升、模块落位与模型路由判别符，并登记生产容量定标门禁 |'));
  assert.match(requirements, /\| 1\.9 \| 2026-10-07 \|/u);
  assert.match(requirements, /A19 原草稿把前序 1.8 勘误重编号为 1.10/u);
  assert.doesNotMatch(requirements, /^\| 1\.10 \|/mu);
});

test('every historical architecture section is locally marked obsolete', () => {
  const history = architecture.split('## 历史设计附录')[1];
  assert.ok(history);
  const chapters = [...history.matchAll(/^## \d+\. [^\r\n]+\r?\n\r?\n([^\r\n]+)/gmu)];
  assert.equal(chapters.length, 12);
  for (const chapter of chapters) assert.match(chapter[1], /^> 历史快照：/u);
  assert.ok(currentArchitecture.includes('`ControlledStatusKind`'));
});
