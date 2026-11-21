# Commit Message Convention

> 本文件为本仓（pinvou3）改编版，改编自社区仓 Pinvou/pinvou-agent 的同名文档：社区仓强制英文描述，本仓强制中文描述，两者不可互换。
> 本仓权威规范为 [`docs/Git Commit 信息规范文档.md`](Git%20Commit%20信息规范文档.md) 与 [`CONTRIBUTING.md`](../CONTRIBUTING.md)，由 `scripts/validate-commit-msg.py`（经 `.githooks/commit-msg` 在每次提交时执行）自动校验。本文是该规范的英文摘要，与自动校验同口径；冲突时以中文权威文档为准。

## Purpose and scope

This convention keeps the Git history readable, consistent, traceable, and suitable for automated release tooling. It applies to every new human-authored commit on every branch.

The project uses a focused form of [Conventional Commits](https://www.conventionalcommits.org/). Existing commit history does not need to be rewritten when this convention changes.

## Format

Use this subject format:

```text
<type>: <中文描述>
<type>(<scope>)!: <中文描述>
```

For changes that need more context, add an optional body and trailers:

```text
<type>: <中文描述>
<type>(<scope>)!: <中文描述>

说明改动原因、实现方式与影响范围。

Optional-Trailer: value
Signed-off-by: Name <email@example.com>
```

The fields have these meanings:

- `type` is required and must be one of the allowed values below.
- `scope` is optional and identifies the affected module, page, or capability, such as `session`, `search`, or `installer`.
- `!` is optional and marks a breaking change.
- `description` is required. Use concise **Chinese**, no more than 50 characters, as a declarative sentence without ending punctuation. The automated check validates both the format **and** the language: a description without any Chinese character is rejected.
- The GitHub-generated terminal suffix ` (#<PR number>)` on a squash commit is platform metadata and does not count toward the 50-character description limit.
- The optional body has no hard language gate; this repository's practice is to write it in Chinese, apart from standard identifiers, paths, URLs, code, and proper names.

Every human-authored commit must also include the DCO `Signed-off-by` trailer described in [`DCO.md`](../DCO.md), and it must match the commit author identity. Create it with `git commit -s` or add `--signoff` when amending or rebasing. Trusted Dependabot/GitHub Actions bot commits and merge commits (more than one parent) are exempt; the fixed historical squash-commit registry is documented in [the Chinese convention](Git%20Commit%20信息规范文档.md), is scoped to explicitly registered commit objects, and never exempts new commits.

## Allowed types

| Type | Use |
|---|---|
| `feat` | Add user-visible functionality, an API, interaction, or configuration option |
| `fix` | Correct a bug, failure, data issue, or compatibility problem |
| `refactor` | Restructure code without adding a feature or fixing a bug |
| `perf` | Improve performance, resource use, caching, or rendering |
| `docs` | Change documentation or comments only |
| `style` | Change formatting without changing behavior |
| `test` | Add, modify, or remove tests |
| `build` | Change build configuration, packaging, dependencies, or environment setup |
| `ci` | Change continuous integration or deployment automation |
| `chore` | Make focused maintenance changes not covered by another type |
| `revert` | Revert an earlier commit |

## Breaking changes

Add `!` after the type or scope when a change breaks compatibility, removes a public interface, or requires migration:

```text
feat(api)!: 重构用户登录接口，废弃旧版登录参数
fix(storage)!: 调整会话持久化结构，历史数据适配变更
```

Explain the migration and compatibility impact in the commit body and pull request.

## Examples

Features and fixes:

```text
feat(user): 新增手机号验证码登录功能
feat(search): 新增离线结果过滤能力
fix(installer): 修复升级时运行时资源丢失问题
fix(session): 解决会话同步重复触发问题
```

Maintenance changes:

```text
refactor(chat): 抽离消息重试公共逻辑
perf(list): 优化列表渲染卡顿、减少重复请求
docs: 更新发布验证流程说明
test(connector): 补充凭证缺失错误覆盖
chore(deps): 升级浏览器测试运行时
```

Reverts:

```text
revert: 回滚上一版会话超时调整
```

## Invalid examples

Do not use vague descriptions (`update`, `fix bug`, `修改代码`, `测试` are rejected outright):

```text
chore: update
fix: fix bug
chore: 修改代码
test: 测试
```

Do not use an unsupported type, omit the required separator, end the description with punctuation, exceed the 50-character limit, or write the description without Chinese:

```text
feature: add search filters
fix(session) prevent duplicate sync attempts
fix: prevent duplicate sync attempts.
fix: 修复重复同步尝试问题。
```

## Best practices

1. Keep each commit focused on one coherent change.
2. Choose the type that describes the result, not the files that happened to change.
3. State the concrete outcome in the subject; avoid process notes such as `WIP` or `address review`.
4. Use the body to explain motivation, tradeoffs, compatibility, and non-obvious implementation decisions.
5. Reference relevant issues or pull requests in the body or trailers when useful.
6. Never include credentials, tokens, customer data, private data, or internal-only addresses in a commit message.
