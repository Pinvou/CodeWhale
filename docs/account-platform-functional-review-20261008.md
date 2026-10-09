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

F00最终状态：1cf612b394dca746375daa8262f2c0ccf766fba3已通过run37877556881全部适用检查及required-gate（job113653143161）。JensenChen28对该head复核确认代码问题关闭，原COMMENT仅等待门禁；现已补充成功结果并继续申请原审核者批准。非阻塞Issue1618仍开放。按用户流程普通merge传播至相邻F01，不改主线或受保护底座分支。

## F01 / PR #1528

F00修复通过普通merge引入：96899c70及6077d33fc。F00当前head16184c38d的required-gate已通过，外部重审已申请、尚待回复。两份原审核逐项核实：钱包ACK测试fixture零租约确实造成16项失败；Rust handoff ticket字符边界、未认证入口限流、全类审计DELETE保护、登录错误/冷却信息、生产Keycloak硬化、NGINX动态DNS、Account可信来源及LEASED索引缺口均真实存在。

原评语修正：Account当前问题是无条件信任来源头而可伪造桶，Portal已有受信peer配置；非security审计UPDATE旧触发器返回OLD导致无声取消，DELETE可绕过；付费SMS已有多维额度，公开入口缺限流及图片可逆映射仍需修；Portal ticket消费端也仅验证长度。删除用户代码无注册路由，未证实可达安全问题。

确认的非阻塞后台错误诊断和记录清理登记 [Issue #1560](https://github.com/Pinvou/pinvou3/issues/1560)。用户明确批准保留24小时验证码、90天短信验证/发送，保留所有业务引用与活动/不确定投递；新增分批清理及前向索引迁移。

验证进展：钱包ACK及过期租约回归通过；Node相关测试、真实浏览器20项、UI build/lint、架构守卫通过。public limiter真实隔离PG并发、重启、共享来源、TTL、随机键基数测试及独立复审通过；事件Claim范围/字面前缀/租约测试通过；部署配置Go来源边界与3项真实脚本mock/结构契约通过且独立代码复审通过。清理5项真实PG子测试通过，覆盖所有业务引用、未来投递租约、89/91天与24小时边界、501条分批和锁行跳过，独立复审通过；主模块全量test/vet通过。官方Windows nginx 1.30.5隔离 nginx -t通过，未启动服务；真实容器DNS重建和Keycloak realm行为未运行验证，不能将mock或syntax检查作为部署验收。Rust完整app lib实际MSVC链接成功，并执行account_platform模块27项测试全部通过、无跳过；仅命令行临时将CodeWhale TUI测试编译opt-level设0，未修改Cargo配置或gitlink。

状态：确认问题修复完成、独立代码复审及本轮适用本地验证通过；推送后回复两位原审核人并申请重审，当前head CI通过后向F02普通merge传播。真实外部服务与组合部署验收仍保持原先披露的延期范围。
