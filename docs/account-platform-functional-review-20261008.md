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
