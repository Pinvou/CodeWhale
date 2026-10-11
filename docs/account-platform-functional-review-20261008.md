# Account Platform 功能波次审核记录

按 F00 至 F18 顺序核实、修复、独立复审，并仅通过普通 merge 向相邻下一波次传播。确认存在的非阻塞问题也登记 Issue；未证实的建议记录依据。

## F00 / PR #1527

审核来源： https://github.com/Pinvou/pinvou3/pull/1527#pullrequestreview-5453328209

确认并修复的阻塞问题：半开熔断器业务拒绝耗尽探测额度；Relay 未在 Claim 前限制 subject，可能处理其他领域事件；Account Platform 缺少 Go CI 门禁。Relay 存储接口增加明确 subject 范围，后续波次的存储实现须同步适配。

确认的非阻塞问题逐项登记在 [Issue #1559](https://github.com/Pinvou/pinvou3/issues/1559)：时钟初始化与生产判定、HTTP panic/流式响应边界、认证初始化诊断、NTP 校验、后台 panic 脱敏、KMS 间接依赖标记、并发和流式回归覆盖。

未确认的缺陷：Gin 已安全记录异常，nil writer 避免输出原始 panic；普通重试不递增 PublicationGeneration，人工 retained replay 有意递增；QUIC/Mongo 是 Gin 的真实传递依赖；CA helper 复用和外部 recipient 指纹为维护/威胁模型建议，未证明约定内安全漏洞。

验证记录：主模块全部 13 个包 test 和 vet 通过；嵌套 KMS 模块 test 和 vet 通过；新增 HTTP 与熔断器回归通过；CI 路由 11 项通过；CI policy 35 项通过；actionlint 与架构守卫通过；diff whitespace 检查通过。Windows 无 CGO 编译器，race 由新增 Linux CI 验证，不计作本地已通过。

状态：最终独立复审通过，确认问题均已修复；提交后推送 F00、回复并请求原审核人重审，待当前提交 CI 验证后向 F01 普通 merge 传播。
补充 CI 验证：ab81a6087 的 Account Platform Linux race/vet 已通过；其余适用检查除 Hub 依赖审计均通过。Hub 当前锁定 Sharp 0.35.4 命中公开 GHSA-wq5f-xc86-pv6w，补丁升级至 0.35.5，锁文件同步更新；npm ci、55 项 Hub 测试（含独立临时 PostgreSQL E2E）及零漏洞 audit 通过。独立复审确认改动仅涉及 Sharp 依赖闭包。

### F00 重新复核（2026-10-09）

重新读取当前 head 16184c38 上 JensenChen28 与 asto18089 最新审核，确认此前三项 P1 的修复仍有效。本轮新增两项阻断已修：父目录系统别名不再因路径字符串不等而被拒绝，末级符号链接与不安全权限仍拒绝；bootstrap secret 与投影目录使用解析后的实际目录核对，新增双向别名/嵌套目录泄露拒绝回归。原子替换夹具明确设置0700，避免 umask 影响。

空或仅空白 APP_ENV 不再自动选择 development，配置加载及启动日志初始化拒绝缺失环境；静态服务 token 仅在显式 development/test 接受。README 提供既有无环境本地启动的迁移方式，不改变 staging OIDC 要求或 production 前缀门禁。

作者主模块13包 test、vet、架构守卫与差异检查通过。配置/鉴权/HTTP范围独立非作者实际复跑3包通过。workload范围独立源码复审通过，Windows包测试通过但明确有4项跳过：原子替换、POSIX权限、Keycloak opt-in、父目录别名（主机缺符号链接权限）。不声称别名回归、Linux race或macOS实测已通过；新增别名回归仅允许Windows权限不足跳过，Linux/macOS无法创建符号链接则失败，须由当前提交Linux CI无跳过执行后完成该边界终审。

新增确认非阻塞缺口登记于 [Issue #1618](https://github.com/Pinvou/pinvou3/issues/1618)，保持开放：breaker恢复/容量、relay scope及lease契约、migration down审计及schema定位、NTP偏移/质量、限流清理、metrics超时/中断维度、JSON编码错误。此前#1559仍为完成状态。本轮不成立的意见：KMS go mod tidy -diff实际退出0无差异；普通未写响应的panic已计500，不能泛称所有panic都计200。CA重复与recipient pin仍按既有维护/威胁模型建议记录。

本轮修复只向F00分支提交，待精确head独立终审和适用CI/required-gate通过后，普通merge向相邻F01传播；申请原审核者重审，不以本地内部结论替代外部批准。
补充运行层终审：1396088a 的 Ubuntu 22.04 CI（run 37876196848、job 113645527899）实际执行主模块及嵌套模块 Go race 测试与 vet，全部通过。独立非作者核对该 head 的 GitHub 合并组合和测试源码：POSIX 路径别名、原子替换及权限回归不可跳过，workload 包实际通过，因此该修复运行边界通过终审。未运行真实 macOS 或外部 Keycloak。

该 head 的 fast-gate 失败来自公开维护分支已前进至0bc868ff、冻结登记 gitlink 仍为44921cf02；不是上述 Go 回归失败。F00 独立修复 CI 冻结版本校验，保持默认发布校验、固定标签和运行时不变，并要求匿名新 bare 仓库证明固定标签→登记 pin→公开维护历史。新增控制仓库回归及精确提交复审完成后另行提交，不据旧 head 的 Go 通过向 F01 提前传播。
冻结底座门禁修复验证完成：作者18项测试（11真实受控bare历史场景+7默认/失败重试场景）全部通过、零跳过；CI gate/merge_queue/rust三个策略模块合计71项通过。独立非作者在冻结源码上实际复跑18项全部通过、零跳过，另单独运行CI gate策略35项通过。真实匿名GitHub公开快照验证通过，控制仓库的file transport测试不冒充公网验证。隔离首次ls-remote及fetch的本地rewrite、凭证、对象和禁用TLS校验环境；固定标签、精确登记pin、维护祖先及commit类型必须成立，公开候选未入维护历史仍失败。默认发布和rN收口严格规则保留，gitlink/登记计数/指纹不变。fork-guard --fast、架构、文档锚点与diff检查通过；完成签名后仍须该新head全部适用CI及required-gate成功再传播F01。

### F00 新增复审复核（2026-10-11）

针对已发布 `055f680d` 的新复审，实际复现 Gin 在部分 JSON、流式文本和二进制响应已写出后追加错误 JSON，以及吞掉主动 `http.ErrAbortHandler` 的问题。恢复边界改为显式中止：已写出、Flush 或 Hijack 后的异常向外抛出 `ErrAbortHandler`，由外层 HTTP 恢复逻辑中止响应并关闭已接管连接；未写响应的普通异常仍返回脱敏 JSON 500。保留未写响应时 EPIPE/ECONNRESET 不写正文的原行为，已写响应后的断连则同样中止，避免吞掉外层关闭信号。日志不包含原始异常或请求内容。

确认的非阻塞配置问题登记为 [Issue #1752](https://github.com/Pinvou/pinvou3/issues/1752)：进程启动前明确 Gin 模式，未设置 `GIN_MODE` 时 development 使用 debug、test 使用 test、其他环境默认 release；保留显式诊断设置。实际注册路由的七场景测试验证路由日志边界。非法模式测试仅验证包初始化后的配置函数；Gin 自身 import 初始化也读取该环境变量，未声明真实非法环境子进程的错误脱敏已经验证。

其余意见按实际目标分支核对：当前 go-oidc/v3 3.17 内部使用 `context.WithoutCancel`，构造上下文取消导致后续 JWKS 永久失效的推断不成立；当前 F00 无 Billing 数据库调用方，不将后续波次的 half-open 早退问题算作此处已修。文件读取副本差异没有证明越过现有可信运维目录边界，不按文件数量机械拆分。fork 文档登记的是实际冻结 gitlink 44921cf02，不因主线日常更新改写基线。

保留两项已明确登记的 CI 必要范围：Sharp 补丁升级修复此前唯一依赖审计阻断 GHSA-wq5f-xc86-pv6w；发布控制台镜像参数化使 CI 构建与 Compose 使用同一已验证镜像来源，修复 [Issue #1676](https://github.com/Pinvou/pinvou3/issues/1676) 的真实匿名拉取 429。生产默认 Node 20 来源及容器业务行为不变，不恢复漏洞依赖或隐藏门禁失败；此前 CI/55 项 Hub 测试和零漏洞审计是历史证据，不冒充本轮复跑。

本轮作者主模块 13 个包测试通过，HTTP 范围 vet、架构与 diff 检查通过。非作者在五项末版源码前后 SHA/blob 不变的条件下实际复跑完整 HTTP 模块：32 主例、23 子例，零失败、零跳过。Windows 未执行 race，真实 Linux、macOS、外部 Keycloak 和部署仍不在本轮本地通过声明内。全部前序未合入 PR 已完整分页回查；签名提交后回复原审核并申请重审，新提交适用 CI 仍须跟踪，不以旧 head 的绿灯替代。
