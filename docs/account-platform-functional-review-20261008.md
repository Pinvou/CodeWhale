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

## F01 / PR #1528

F00修复通过普通merge引入：96899c70及6077d33fc。F00当前head16184c38d的required-gate已通过，外部重审已申请、尚待回复。两份原审核逐项核实：钱包ACK测试fixture零租约确实造成16项失败；Rust handoff ticket字符边界、未认证入口限流、全类审计DELETE保护、登录错误/冷却信息、生产Keycloak硬化、NGINX动态DNS、Account可信来源及LEASED索引缺口均真实存在。

原评语修正：Account当前问题是无条件信任来源头而可伪造桶，Portal已有受信peer配置；非security审计UPDATE旧触发器返回OLD导致无声取消，DELETE可绕过；付费SMS已有多维额度，公开入口缺限流及图片可逆映射仍需修；Portal ticket消费端也仅验证长度。删除用户代码无注册路由，未证实可达安全问题。

确认的非阻塞后台错误诊断和记录清理登记 [Issue #1560](https://github.com/Pinvou/pinvou3/issues/1560)。用户明确批准保留24小时验证码、90天短信验证/发送，保留所有业务引用与活动/不确定投递；新增分批清理及前向索引迁移。

验证进展：钱包ACK及过期租约回归通过；Node相关测试、真实浏览器20项、UI build/lint、架构守卫通过。public limiter真实隔离PG并发、重启、共享来源、TTL、随机键基数测试及独立复审通过；事件Claim范围/字面前缀/租约测试通过；部署配置Go来源边界与3项真实脚本mock/结构契约通过且独立代码复审通过。清理5项真实PG子测试通过，覆盖所有业务引用、未来投递租约、89/91天与24小时边界、501条分批和锁行跳过，独立复审通过；主模块全量test/vet通过。官方Windows nginx 1.30.5隔离 nginx -t通过，未启动服务；真实容器DNS重建和Keycloak realm行为未运行验证，不能将mock或syntax检查作为部署验收。Rust完整app lib实际MSVC链接成功，并执行account_platform模块27项测试全部通过、无跳过；仅命令行临时将CodeWhale TUI测试编译opt-level设0，未修改Cargo配置或gitlink。

状态：确认问题修复完成、独立代码复审及本轮适用本地验证通过；推送后回复两位原审核人并申请重审，当前head CI通过后向F02普通merge传播。真实外部服务与组合部署验收仍保持原先披露的延期范围。