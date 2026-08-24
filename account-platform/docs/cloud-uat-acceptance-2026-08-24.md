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
