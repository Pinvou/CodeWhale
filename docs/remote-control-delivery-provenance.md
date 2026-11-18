# 远控交付追溯补遗（2026-11-16）

本文件对现行说明作前向补遗，保留原提交、失败和审核数字。核对基线为 `acb6bd20cbd4b7c36cd6543e1199715a58b3d11c`。它不替代当前 PR 固定树的验证、外审、CI 或发布资格。

## DEV 与实际波次

DEV 是原计划任务，A/S 是实际审核/整合波次，二者不是一对一。下表按相邻波次**最终 head 的树差异**核对实际载体，并结合原 PR 的范围说明区分传播与自身变更。每行固定 head 的完整值可从链接 PR 获取；可用 `git diff <上一行 head> <本行 head> -- <路径>`复查，A01 的比较起点是 `b9ec50c0f^1`。同一 DEV 在数波新增、接线、修复和记载，不重复认领为独立交付。

没有查到 A00 的独立 PR；原计划冻结/拆分起点用 A01 的实际基线 `4f6b6fd6e` 标识，整合终点用 `e58a4cbee8542f1f67a4e033145c0fa3e6915701` 标识，均不冒称 A00 独立实现/验收。PR #1317 是无关的 SSO 修复，不能充当 A00。

| 波次 / PR | 固定最终 head | DEV 交叉范围 | 实际交付载体与边界 |
|---|---|---|---|
| [A01 #1318](https://github.com/Pinvou/pinvou3/pull/1318) | `6f41c7fff19a` | DEV-00～31 需求/计划 | 需求与任务两文档，含冻结向量；没有后续实现资格 |
| [A02 #1319](https://github.com/Pinvou/pinvou3/pull/1319) | `b8640793b240` | DEV-00 | protocol.rs、protocol-controller.js 与冻结 fixture；撤回的生产信任根不算净交付 |
| [A03 #1320](https://github.com/Pinvou/pinvou3/pull/1320) | `28867c9a5f31` | DEV-01 | enrollment-registry.js、relay-state-store.js；另含主线传播，不能全算 registry 新增 |
| [A04 #1321](https://github.com/Pinvou/pinvou3/pull/1321) | `dd62ee498158` | DEV-05 | crypto.rs、固定向量与敏感值清理；protocol.rs 是修改既有文件 |
| [A05 #1322](https://github.com/Pinvou/pinvou3/pull/1322) | `a43655b3c330` | DEV-02～04、DEV-06，及 DEV-08～09 Relay 侧 | migration/gateway、pairing-store、reservation/lease/router、server 组合及 liveness/resume 模块 |
| [A06 #1323](https://github.com/Pinvou/pinvou3/pull/1323) | `177c94f34619` | DEV-07～19 内核/命令端口 | relay_transport、controlled/controlling、task/permission、模型/journal/transfer；后续宿主装配仍须另验 |
| [A07 #1324](https://github.com/Pinvou/pinvou3/pull/1324) | `d6cd4df8ec27` | DEV-10～19、DEV-20 状态桥 | 模型/审批/附件内核与状态投影、desktop-remote-control.js；不能把 mock IPC 当真实双端执行 |
| [A08 #1325](https://github.com/Pinvou/pinvou3/pull/1325) | `474db697575e` | DEV-07～24 接线；DEV-26～31 历史方案替代 | 真实业务端口、SessionStore remote_proxy/application、统一 ChatView 与 UI/遥测；删除独立 history；发布文档仅提前规划 |
| [A09 #1326](https://github.com/Pinvou/pinvou3/pull/1326) | `d1757ea167b7` | DEV-10/13/16/20 修复与历史记录 | panic_scope、任务/ACP 精确所有权、transport 与状态接缝；新增两份历史审核文档 |
| [A10 #1327](https://github.com/Pinvou/pinvou3/pull/1327) | `937317751554` | DEV-03/06、早期 E2E-01/02 历史记录 | 仅端到端补齐任务文档；不重新认领前波产品代码 |
| [A11 #1328](https://github.com/Pinvou/pinvou3/pull/1328) | `aa4090cc28c7` | DEV-13/14、早期 E2E-04/05 历史记录 | 仅端到端补齐任务文档；独立旧 history 只是历史证据 |
| [A12 #1329](https://github.com/Pinvou/pinvou3/pull/1329) | `286be09f3f45` | DEV-08/09 | liveness 截止、Relay/Rust 恢复及相关回归；不是纯历史引用变更 |
| [A13 #1330](https://github.com/Pinvou/pinvou3/pull/1330) | `0cfadf20533c` | DEV-09/10、DEV-25 规划/回归 | relay_runtime、manager 恢复接线和审核/发布文档；非实际发布 |
| [A14 #1331](https://github.com/Pinvou/pinvou3/pull/1331) | `330027160099` | DEV-20/25 部署与端点 profile | 部署脚本、verify-web-dist、受控身份、双机清单；非实际部署证明 |
| [A15 #1332](https://github.com/Pinvou/pinvou3/pull/1332) | `20d893de62c6` | DEV-21/22 早期 E2E-07 记录 | 仅端到端补齐任务文档；不重新认领设置页产品实现 |
| [A16 #1333](https://github.com/Pinvou/pinvou3/pull/1333) | `136f2b9b23d6` | DEV-07/20 开发验证接线 | run-dev.sh、remote_dev_profile.test.mjs 与端点说明；非公网真机验收 |
| [A17 #1334](https://github.com/Pinvou/pinvou3/pull/1334) | `4092a64f5f85` | DEV-21～23、DEV-10/20 集成 | ChatView/设置页、controlled reaper 与UI合同；mock浏览器不等于真实已安装应用 |
| [A18 #1335](https://github.com/Pinvou/pinvou3/pull/1335) | `18062590247f` | DEV-25 部署预检/回滚边界 | deploy-controller-preflight、hardening、route smoke 和部署脚本；不重新认领历史 DEV-15 模型修复 |
| [A19 #1336](https://github.com/Pinvou/pinvou3/pull/1336) | `2c63067c3312` | DEV-26～31 历史边界/DEV-20～23 合同 | 代码架构/需求文档、desktop_remote_document_contract.test.mjs；保留 merge 中首次成文事实 |
| [A20 #1337](https://github.com/Pinvou/pinvou3/pull/1337) | `1c8e08d9c8bd` | DEV-20 跨平台门禁及共享架构 | 包含主线传播与平台/CI差异；不能把跨分支继承全算远控新增 |
| [A21 #1338](https://github.com/Pinvou/pinvou3/pull/1338) | `645e9fc0abb1` | DEV-00/13 与本地隔离回归 | app command tests、gateway heartbeat/idle合同；后者属于 DEV-02 维护 |
| [A22 #1339](https://github.com/Pinvou/pinvou3/pull/1339) | `5c0353b9001f` | DEV-20/22/23、DEV-26～31 当前替代合同 | ChatView/代际绑定、旧历史退役说明及 gateway Origin 合同 |
| [A23 #1340](https://github.com/Pinvou/pinvou3/pull/1340) | `1b7d34b92a35` | 无净产品增量 | 相邻 A22 最终 head 到本 head 的树差异为空；保留历史空节点，不补造独立实现 |
| [A24 #1341](https://github.com/Pinvou/pinvou3/pull/1341) | `7c203818e055` | DEV-00/20、DEV-26～31 契约 | 协议旧字段拒绝及产物合同文档/回归 |
| [A25 #1342](https://github.com/Pinvou/pinvou3/pull/1342) | `984156d1b7c3` | DEV-25，交叉 DEV-20/24 | 发布开关、证据 schema/validator、release gate、真实双机脚本与工作流；有工具不等于旅程通过 |
| [A26 #1343](https://github.com/Pinvou/pinvou3/pull/1343) | `bbfa13fd023a` | DEV-25 资格合同 | 发布/双机文档及门禁测试；没有新增真实部署证据 |
| [S01 #1344](https://github.com/Pinvou/pinvou3/pull/1344) | `23baa5af05e2` | DEV-20/25 组合 | pr-check.yml 分平台合同与 runtime 能力字段；非单独业务任务 |
| [S02 #1345](https://github.com/Pinvou/pinvou3/pull/1345) | `64ce55a75da1` | DEV-13/20 组合 | engine_pool、ACP、remote manager 测试同步；保留有效回归，不新建状态所有者 |
| [A27 #1346](https://github.com/Pinvou/pinvou3/pull/1346) | `bad904bd0140` | DEV-23 展示，交叉共享 Chat 消费者 | 内部上下文过滤；邻波传播也包含 registry/capacity 维护，非 A27 自有新增 |

DEV-00～25 在上表按能力定位；DEV-26～31 原独立历史/旧接口设计已由 A08 的 SessionStore RemoteProxy、共享用例和现有 ChatView 替代，A19/A22/A24 记录/约束这一退役，不作为仍待开发或需要兼容的旧实现。A08 前移的连接、任务、权限、文件、会话和遥测依赖须组合验收；DEV-25 的真实发布验收仍归发布 owner。历史“基础出口”也必须递归满足 DAG 中 DEV-19/21 等传递前置，不能按波次或列表省略。

## 历史数字的可核实边界

A11 文档原记 `cargo test remote_control::` 的 298 和 295 均保留。未追回两次原始完整日志、精确采样 SHA、平台、features 和过滤器，差异原因**不可核实**。现时重跑不能证明过去的数量，不采用“新增遥测测试”等猜测。

A12 旧正文“36 历史提交引用”和 A13 旧正文“48 历史引用”缺少完整统计口径，也不可据此推算本波净增量。现在提供只读清单命令（仅 `docs/` diff 新增行，重复 token 分别计数，必须解析为真实 commit；不同缩写指向同一 commit 时按完整 SHA 去重）：

```sh
git -c core.quotePath=false diff --no-ext-diff --unified=0 aa4090cc28c79950aa919a6da17aeb6673e4d7db 286be09f3f45a3457d5f4c7b13316391ea29705c -- docs/
git -c core.quotePath=false diff --no-ext-diff --unified=0 286be09f3f45a3457d5f4c7b13316391ea29705c 0cfadf20533cea1841efb6d1a00b23452f451953 -- docs/
```

只扫描 diff 中以单个 `+` 开头的新增内容行（排除 `+++` 文件头），正则为 `(?<![0-9A-Za-z])[0-9a-f]{7,40}(?![0-9A-Za-z])`；每个 token 必须以 `git rev-parse --verify "<token>^{commit}"` 成功解析。按输出完整 SHA 去重，不以缩写拼写去重。

本次只读输出分别为 **19 次 / 12 个唯一 commit** 与 **49 次 / 19 个唯一 commit**；这是所示固定范围，不是旧“36/48”的原因解释或历史测试数。清单含路径、原 token、完整 SHA，无法解析的 SHA-like 英文片段（如 `cceeded`）列为拒绝项。完整树总量、净新增行与每次出现/唯一提交不能混用；重新运行必须保留同样范围和规则。

固定范围清单（数字为新增行中的出现次数，0 表示该范围未新增引用）：

| 可解析 commit | A11 → A12 | A12 → A13 |
|---|---:|---:|
| `1987dd2eb4e66ecc2a0de179ec898d20054d6c1d` | 0 | 3 |
| `22929522a8593d521ef14407172156dd08d07600` | 0 | 4 |
| `352997f15b8b8b2b1bb88d153514ec2e4b1d9cda` | 1 | 0 |
| `4b0b0283e6dafd8915484f598f26d305614aceda` | 2 | 1 |
| `4bfa63e3c7cb77ce2d7e7420951c49e76e892b37` | 0 | 4 |
| `50ed50244aa04de368bac8a5339891b869f451eb` | 1 | 1 |
| `582cba9382fce0ab68d828d83d7329f9d1484c3e` | 1 | 0 |
| `5a246985ac186756f4e1a92d5f1e702ab49bf26d` | 0 | 2 |
| `5b42e1d87d6e8354a70efae56679ad108d54045c` | 0 | 4 |
| `6e84a097d979c6fa8b1619b12ba61d978db53ea1` | 0 | 3 |
| `75b30972f11d72a8886d046cd45ebb28ebf4ee6a` | 0 | 2 |
| `8510525bd9951295a7190c04ce3ccf40569d5ebf` | 0 | 5 |
| `88832f0f303d4b6df1f7998ba104367d5839a588` | 1 | 0 |
| `8d372e8a9b42a4745e6e6526967cec46aa1fbfcb` | 2 | 2 |
| `8d5ae57f2887c5caaaa32b05a5b0998212211ad9` | 2 | 2 |
| `94141a2e8bb5a815aeb305526fe913facb5ce947` | 2 | 2 |
| `9dc5f9e51e7eec203278b7bc3308343df6d33d58` | 1 | 0 |
| `a345b6a080843a951ed0ed694544fc0b1e6d9043` | 0 | 4 |
| `a416ad41eff6e3088d577d79a93b3e87c9386f7c` | 2 | 0 |
| `a8a56a49038f0ae6914e0b83912786fc3655445a` | 2 | 1 |
| `be913049483df3a83e7a67eacc4215ab29cf8871` | 0 | 3 |
| `c686550dc006cd99bca388b5fca9f5b4623aa4ed` | 0 | 2 |
| `e8fea59db88538122e0468a97b47f03628df47c3` | 2 | 2 |
| `f4c3ea9bd4a123d966c90f2a89a80f82964dc66a` | 0 | 2 |

## 保留历史的提交归因

A19 保留 `634c2ee49a276523cf7334c581d08c66a2b7608c` 与 `228555a761dab57292bf642f3290c4763f35a525` 两次默认英文 merge 主题；文档合同测试 `pinvou3-app/tests/desktop_remote_document_contract.test.mjs` 首次成文在 `37f343602f6ec81352ea3308593cfa4d27b8cebf` 的合并树中（两个父树均无该文件），不是后来补造的独立非合并提交。A12/A16 历史也含规范外 `merge(remote-control)` 主题及单亲提交。它们是明确的历史流程例外，保留原 SHA/父链，不借改格式重排或改写历史，也不把整合作者认作所有原始功能作者。今后文档/合同新增尽量独立成文，传播 merge 主题与新提交按 CONTRIBUTING 的允许类型、中文主题和有效 DCO 执行。

## 已合并 PR 的历史回执与最终锚点

[A27 #1346](https://github.com/Pinvou/pinvou3/pull/1346) 最终 head 为 `bad904bd0140a27ec57425119441ec8395020fc5`，2026-10-13 合入，merge 为 `e58a4cbee8542f1f67a4e033145c0fa3e6915701`，已包含在本次基线。原正文中的“当前提交 `22b55c598e99d94c475cb436ef69079d98c84147`”和 `remote-control-r26` marker 是旧 R26 历史回执，其父关系、tree、数字和CI待运行时态只属于当时版本，不是最终 head 当前状态；本补遗不把它们改写成最终版实跑或人审证据。

[PR #1783](https://github.com/Pinvou/pinvou3/pull/1783) 最终 head 为 `f1b64ebbb4f570b563534f95a5faf5f378029ede`，2026-10-14 合入，merge 为 `b38dfde54b068f7cac563922924a8e711b23f515`。当前正文已更新为该末版，不应再把早期 a303/eaff 的 121 项称为整个 PR 最终结果。原七测试文件与五修改路径是不同 scope；89 项阶段三项红例与另一 34 项审核七项红例不能互换；580、13、600 warnings 属不同版本/范围，不取最低值声称末版 lint。旧失败记录保留。当前正文记录的 1517 项、515 项及继承范围仍是历史末版回执，本次没有重新执行。

只读 GitHub 核对时，#1783 最终 head 对应 [PR Check run 38549421564](https://github.com/Pinvou/pinvou3/actions/runs/38549421564) 为 completed/success；不能把合入后另一 run 的 skip 当作该次实跑。这里记录终态，旧“仍需运行”不再代表当前操作待办；不重新请求已关闭 PR 的代码复审或宣称发布资格。远端正文的独立补遗须经过本次审核后追加并精确回读，本文本身不是已完成远端更新的证据。
