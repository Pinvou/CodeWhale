# 云端 UAT 部署与外部验收记录（2026-08-24）

> 部署编号：`cloud-uat-20260824-9271c307`
>
> 候选版本：`0.8.0` / `9271c3074a24ef461e9d02246b0fca4969bd7495`
>
> 证据边界：现有单节点云端 UAT。本文只保存版本、时间、公开证书属性、计数和验收结论，不保存服务器地址、Secret、业务正文、备份内容或私有 Provider 地址。

## 发布事实

- 从受控版本包创建独立版本目录；发布前为 PostgreSQL globals、Account、Billing、Token 和 Keycloak 分别生成备份，逐文件 SHA-256 校验与最小可读性检查通过。
- Account、Billing、Token 迁移依次成功，运行库分别收敛到 Account 32、Billing 34、Token 21。
- Identity、Account、Billing、Token Gateway、Portal 和三个 Event Relay 全部健康；健康响应固定返回 `0.8.0` 和完整候选 Commit。
- 公网 Portal、Account readiness 和 OIDC discovery 均返回 200。非生产 Payment Simulator 容器及公开路由已移除，原站点配置保留在主机受限备份目录，数据卷未删除。

## APT-035：HTTPS 与生产密钥管理

- 通过：公网 Portal、Account/Identity 边界和 OIDC discovery 使用 HTTPS；公开证书由受信 CA 签发，有效期为 2026-08-13 至 2026-11-11，验收时剩余有效期超过 14 天。
- 通过：部署 Secret 文件权限为 `0600`，新增审计、身份、注册与风险密钥均独立生成，未进入源码、命令输出或发布证据。
- 未通过：环境仍为 `APP_ENV=development`，Token Gateway 使用 `TOKEN_SECRET_PROVIDER=local`；没有生产 KMS/Vault SecretStore、新旧 Key 双读重加密、旧密文归零和旧 Key 停用证据。
- 结论：NFR-01 已取得本次云端 UAT 运行证据，NFR-03 未满足生产验收；APT-035 保持 `BLOCKED_EXTERNAL`，不得据此开放正式生产流量。

## APT-037：迁移、备份与恢复

- 通过：迁移前在候选版本和稳定链接均未切换时创建受限备份目录，分别保存 PostgreSQL globals、Account、Billing、Token 和 Keycloak；目录权限为 `0700`，五个文件的 SHA-256 校验与 `pg_restore --list`/gzip 最小可读性检查通过。
- 通过：迁移按 Account、Billing、Token 顺序执行且退出码均为 0；运行库再次校验已执行迁移，最终版本分别为 32、34、21。
- 未通过：本次仅有同机迁移快照，没有连续 WAL、日增量/周全量计划、TLS/AES-256 备份链、独立 KMS、异地不可变副本或隔离恢复计时。
- 未通过：没有月度、季度、年度演练历史，也没有三年财务证据、90 天普通日志、法律保全和到期清除扫描收据。
- 结论：NFR-09 获得云端 UAT 迁移与快照证据；NFR-10、NFR-34–NFR-38 的生产恢复事实仍缺失，APT-037 保持 `BLOCKED_EXTERNAL`。

## APT-038：高可用与故障切换

- 实测拓扑只有一台 Docker 主机；Identity、Account、Billing、Token Gateway、Portal、三个 Relay、PostgreSQL、Redis、NATS 和 Keycloak 均各运行一个实例。
- 当前 Compose 发布验证了单节点滚动替换与健康恢复，但不存在第二故障域、故障后 30% 容量余量、PDB、跨区数据复制或依赖主节点自动切换。
- 未执行 Pod、节点、区域、滚动升级中断和 PostgreSQL/Redis/NATS 主节点丢失场景，不能生成同一候选版本的 HA 门禁证据。
- 结论：NFR-11 保持部分实现，APT-038 保持 `BLOCKED_EXTERNAL`；需要真实多故障域 Kubernetes 与外部 HA 数据依赖后重新验收。

## APT-039：性能、容量与公平准入

- 基础烟测通过，但环境是单节点 `development` UAT，且 APT-038 未通过，不符合生产等价性能测试的前置条件。
- 验收时数据规模为 0 个 Hold、4 个订单、0 个支付流水；NATS 为 1 个流、0 条消息，远低于十万 Hold、百万订单/三百万流水和十万积压门槛。
- 未执行冻结资源档位下的 30 分钟、每类一万/流式一千样本、200 流式并发、500 Hold 突发、Provider 分段计时或积压排空测试，也没有发布/数据集/报告三类摘要。
- 结论：NFR-26–NFR-33 没有生产等价容量证据，APT-039 保持 `BLOCKED_EXTERNAL`；不得以本次健康烟测推导生产 SLO 或声明容量。

## APT-043：Provider 熔断与可信时钟

- 主机 Chrony 报告 NTP 已同步，验收时可见 9 个授时源，当前样本偏差约在 -2.3ms 至 +2.9ms；这只证明宿主机当前授时正常。
- 应用仍运行在 `development` Compose，没有 production-new 的三源中位偏差配置，也未注入 100ms、250ms 和大于 1 秒偏差；没有对应告警投递、readiness 失败和写请求 `CLOCK_UNSAFE` 拒绝证据。
- 当前没有支付流水和真实 Provider 错误样本，未覆盖支付/模型各能力与路由的业务拒绝排除、连续失败、滚动窗口、只读半开探针、指数重开、跨副本一致性和告警。
- 结论：NFR-44、NFR-46 保持部分实现，APT-043 保持 `BLOCKED_EXTERNAL`；宿主机 NTP 正常不能替代应用层故障注入和 Provider 验收。

## APT-047：工作负载身份与 NATS Subject

- 通过：真实 OIDC discovery/JWKS 可通过公网 HTTPS 访问；Account、Billing、Token 三个 Relay 为独立容器，代码版本和健康状态均可独立核对。
- 未通过：Account、Billing 与 Token Gateway 的内部 HTTP 身份摘要相同，确认当前 Compose 仍共用一个静态 `INTERNAL_SERVICE_TOKEN`，不是逐工作负载最长 15 分钟 OIDC 投射令牌。
- 未通过：NATS 仅以 JetStream、存储目录和监控端口参数启动，没有 Operator/Account/User 凭据；三个 Relay 未取得精确到本域 Subject 的外部授权证据。
- 未执行：issuer/JWKS 中断、投射令牌轮换/撤销、跨服务身份调用和 Relay 越域发布拒绝的真实环境故障注入。
- 结论：ADM-03、NFR-12 保持部分实现，APT-047 保持 `BLOCKED_EXTERNAL`；需要正式 Kubernetes、工作负载 issuer 和 NATS Operator 后重新验收。

## APT-007：首个超级管理员与 Break-glass

- 运行库只读核对结果为：0 个有效 `PLATFORM_SUPER_ADMIN`、0 个 `BOOTSTRAP_PENDING` 用户、0 条 Break-glass 操作和 0 条安全告警。
- 首个管理员必须通过正常 `account-security bootstrap` 由受控终端直接设定正式密码，或签发一次性引导凭据后由受控终端兑换；不得复用部署 Secret、把密码写入环境变量，或用 Break-glass 绕过首次初始化。
- 因尚无有效超级管理员，本次未声明“全部管理员异常失联”事故，也未执行 Break-glass；否则会把计划内首次初始化错误地伪装成事故恢复。
- 仍缺并发兑换、Identity 激活后 Account 提交失败补偿、Keycloak/数据库故障、CRITICAL 告警真实投递、5–60 分钟临时授权到期和正常管理员交接演练。
- 结论：ADM-01、ADM-02 保持部分实现，APT-007 保持 `BLOCKED_EXTERNAL`；先由用户控制正式密码完成首个管理员激活，再在另有正常恢复路径和告警接收方的生产等价环境演练。
