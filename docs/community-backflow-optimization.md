# Community 回流处理优化

## 目标与基线

本方案只优化 Issue 被领取后的回流处理，不把 `backflow:pending` 等待领取时间计入处理耗时，
也不把一次性 Windows 文本编码事故计入可重复收益。

历史样本的处理阶段中位数约 51.6 分钟；排除明显依赖阻塞项后的平均值约 42.5 分钟。
主要耗时来自依赖等待与重复 rebase、本地 Rust 冷构建、CI 慢路径，而不是 Issue 收尾。

目标：

| 指标 | 目标 |
|---|---:|
| 准备阶段中位数 | 不超过 10 分钟 |
| 正常 PR 阶段中位数 | 不超过 15 分钟 |
| 最终 CI 中位数 | 7～9 分钟 |
| 无依赖回流总耗时 | 30～35 分钟 |
| 回流总耗时 P90 | 60～75 分钟 |

## 两维执行模型

回流阶段不能按 Ubuntu、Windows、macOS 分层。分层描述的是验证目的，平台描述的是执行位置：

1. 本地 L0/L1/L2 在实际创建或处理 PR 的平台执行。
2. GitHub CI 用 Windows、macOS、Linux 三平台轻门禁独立复核同一个 head。
3. 公共重型检查只运行一次；原生检查按实际改动路由到对应平台。
4. Merge Queue 验证最新 `main` 与 PR 的组合提交，主线日常前进不触发人工 rebase。

```text
Windows / macOS / Linux 处理节点
  -> 本地 preflight
  -> push + PR provenance
  -> 三平台 backflow-portability（并行）
  -> 按路径选择的产品门禁（并行）
  -> required-gate
  -> Merge Queue
  -> 自动收尾
```

## 跨平台 CLI

`scripts/community-backflow.mjs` 仅依赖 Node.js 22 和 Git：

```shell
# 从开放的 pending/processing Issue 聚合 community-backflow-plan marker
node scripts/community-backflow.mjs queue \
  --repo Pinvou/pinvou3 \
  --output queue.json

# 根据显式依赖、路径重叠和 Release 顺序生成可并行波次
node scripts/community-backflow.mjs plan --input queue.json --output plan.json

# 在实际处理平台运行；不安装依赖、不编译 Rust
node scripts/community-backflow.mjs preflight --base origin/main --source 244

# 推送固定分支、创建或更新 PR、写后回读，并评论关联 Issue
node scripts/community-backflow.mjs submit \
  --repo Pinvou/pinvou3 \
  --issue 602 \
  --source 244 \
  --title "chore(release): 回流版本升级至 0.8.0" \
  --body-file pr-body.md
```

`queue` 和 `submit` 优先使用 `GITHUB_TOKEN` 或 `GH_TOKEN`；使用 Git Credential Manager
时由操作者显式传入 `--credential <github-username>`，工具不内置个人账号。GitHub API 请求
使用 15 秒超时，并对网络错误、限流和临时 5xx 最多重试 3 次。请求正文始终以 UTF-8 字节发送，创建或更新后逐字回读
标题和正文。Token 不写入输出、文件或 PR。

PR 正文包含与当前 head 绑定的 provenance，记录实际处理 OS、CLI 版本、base/head SHA
和轻量检查。任何后续 push 都必须重新运行 `submit` 更新 provenance。

## 事件驱动自动推进

默认分支上的 `.github/workflows/community-backflow-automation.yml` 监听两个受信事件：

1. `PR Check` 的 `workflow_run.completed` 成功事件触发 current-head 复核并加入 Merge Queue，
   不再由操作者轮询 required checks。
2. `pull_request_target.closed` 且 PR 已合并时触发普通回流 Issue 的幂等收尾。

自动器默认关闭。只有仓库变量 `COMMUNITY_BACKFLOW_AUTOMATION_ENABLED` 精确设置为 `true`
才允许写操作；变量缺失或其他值均输出 `automation-disabled` 并成功退出。工作流显式 checkout
仓库默认分支，绝不 checkout 或执行 PR head 的代码。

每次推进都从 GitHub 回读并同时验证：Official 同仓、base=`main`、固定
`backflow/community-<source>` 分支、来源 marker/provenance/current head 一致、来源 Community
PR 仍 merged、分页读取的真实 changed files、唯一关联 Official Issue、不带
blocked/dependency-wait，以及该 head 的 `required-gate` 已 completed/success。只有来源根目录
`VERSION` 发生变更（含重命名前路径）才判定为 Release，并与 Issue plan 交叉验证；响应未知或
两者不一致时 fail closed。PR 来源/provenance 与 Issue 来源/plan 四类 marker 必须各恰好一个；
重复、冲突或缺失一律停止，不根据事件载荷猜测状态。

```shell
# 本地或 Actions 事件载荷的完整只读演练；仍需显式启用变量才会进入验证路径
COMMUNITY_BACKFLOW_AUTOMATION_ENABLED=true \
node scripts/community-backflow.mjs automation \
  --repo Pinvou/pinvou3 \
  --event-file event.json \
  --dry-run
```

入队和收尾均可安全重放：`enqueuePullRequest.expectedHeadOid` 绑定已通过门禁的 head，校验后
分支推进会由 GitHub 原子拒绝；已经在 Merge Queue/已启用 auto-merge 的 PR 不重复入队。普通
Issue 评论含稳定 marker；收尾各阶段重新回读 PR、Issue、head 和标签。先添加并回读确认
`backflow:synced`，确认前始终保留 `backflow:processing`，随后才定向移除旧回流标签并关闭；
`open+synced` 是可恢复中间态，重放会全面复核后继续关闭，不使用旧标签数组覆盖并发更新。
Release 回流第一阶段用幂等评论明确返回
`deferred-release`，不删除分支、不改标签、不关闭 Issue；其 `release-packages.yml` 去重触发与
构建链接回读仍按后文
人工契约完成，避免缺失固件构建证据时错误收尾。

GitHub 的 `enqueuePullRequest` 接口本身不接受逐 PR merge method，不能据此声称 squash。
自动器在每次推进前通过 GraphQL 回读 Official `main` 的
`mergeQueue.configuration.mergeMethod`，只接受 `SQUASH`；缺失、未知或实际为 `MERGE`/`REBASE`
时给出管理员可操作的配置提示并拒绝入队，绝不修改远端规则或绕过 Queue。远端回流分支仅在
人工收尾或具备服务端 SHA 原子绑定的安全 reaper 中删除。GitHub REST ref DELETE 没有
compare-and-delete，事件自动器不得执行 GET→DELETE，以免校验后分支推进时误删新 head。

## 依赖调度

监控器在同一次扫描中提前读取所有新来源 PR 的文件列表并生成 DAG：

- 没有依赖且路径不重叠的回流位于同一波次，可以由不同平台并行处理。
- 修改相同路径的回流按来源合并时间串行，减少冲突和重复 rebase。
- Release 回流依赖同批次更早的非 Release 回流。
- Issue 中的 `community-backflow-plan` 是可恢复的远端 checkpoint；`queue` 命令从全部开放的
  `backflow:pending`/`backflow:processing` Issue 聚合 marker，换处理机器时无需重做扫描。

依赖等待应单独统计为 `blocked_dependency`，不能混入代码准备或 CI。只有依赖满足后才创建
最终 PR；可以提前分析和准备补丁，但依赖合入后只做一次最终整合。

## CI 路由

可信回流 PR 和纯回流 Merge Group 固定运行 `backflow-portability`：

- `ubuntu-latest`、`windows-latest`、`macos-15` 并行。
- 不运行 `npm ci`，不初始化 CodeWhale，不调用 Cargo。
- 检查调度算法、Git diff、跨平台路径和 PR provenance。
- 目标时长 2～4 分钟，超时上限 5 分钟。
- 修改回流基础设施自身（包括 `pr-check.yml`）的普通 PR 也会触发三平台轻门禁；这是为了让
  门禁定义在影响回流前先完成跨平台自验证，会增加少量并行 runner 计算分钟。

现有前端、Rust、Windows runtime 和 macOS runtime 任务继续按路径路由。三平台轻门禁与这些
任务并行，并由 `required-gate` 汇总，因此不会把三个操作系统串成三倍墙钟时间。

## Rust 与缓存

- 本地只运行受影响 package 的快速检查，不对同一 head 重复全仓冷构建。
- 完整 Rust 测试交给有缓存的 CI；可信回流仍按既有策略在 PR 阶段跳过完整 `rust-test`，
  由最新 `main` 做累计验证。
- npm/Cargo 缓存必须包含 OS、架构、工具链和 lockfile；不得跨平台复用编译产物。
- 同一 PR 的旧 SHA 由 `concurrency.cancel-in-progress` 取消。

## 计时字段

自动化记录以下互斥阶段，避免把重叠 CI 重复计入总耗时：

| 字段 | 起止事件 |
|---|---|
| `prepare_active` | 依赖满足到首次 push |
| `blocked_dependency` | 已领取但等待前置回流 |
| `ci_critical` | 当前 head push 到 required checks 完成 |
| `merge_queue` | 入队到 merge |
| `cleanup` | merge 到 Issue synced/closed |

优化看墙钟时间、P50、P90 和失败重试次数；runner 计算分钟单独统计。三平台轻门禁会增加少量
计算分钟，但不应显著增加关键路径。
