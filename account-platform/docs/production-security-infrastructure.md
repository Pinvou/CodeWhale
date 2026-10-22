# 正式环境安全基础设施与验收边界

本文件恢复需求、任务和实现状态文档引用的安全基础设施入口，并按当前模块化部署结构说明职责。原始设计及当时的现场记录保存在提交 `828242031c42911099792ec78c1f746ed8620c49` 的同名文件中；旧记录中的部署结果、数据清除授权和人员授权只适用于其原定对象及时间，不能作为本轮部署、删除、轮换或发布的授权，也不能证明当前环境状态。

## 设计约束

- 原生 KMS 使用 OpenBao/Vault Transit，沿用 AES-256-GCM、随机 nonce、强制 AAD、稳定密钥身份、不可导出/不可明文备份和最小权限边界，不自研加密中转或在生产退回 local/静态 token。网关与凭据 Portal 使用各自短期身份、独立 Agent sink 和经验证的 TLS；旧版本解密能力须满足既有密文及加密备份恢复，不能把 remote 模式的旧 key 禁用条件套用于原生版本。
- Workload 身份复用独立 Keycloak realm 与逐服务 client-credentials：短期 JWT 自动续取，目标五分钟、最长十五分钟；issuer、JWKS、audience、subject、精确 permissions 均由接收端校验。不共享长期业务 token，不用通配 subject 或权限替代逐服务绑定。投递器只取得并校验可信签发器的 token，不签名或扩权；业务只读单服务 token 目录，不能读取 Agent bootstrap 秘密。
- PostgreSQL、Redis、NATS、KMS 及入口保留 TLS、独立身份和受控网络。KMS 不开放公网管理入口；凭据 Portal 继续使用独立 origin、OIDC client 和 audience。生产 NATS 资源由受控初始化准备，业务只核验和使用既定资源，不创建或覆盖共享 stream/consumer。
- 解封材料、root token、CA 私钥、数据库密码和服务身份通过仓库外受控流程保管。初始化不等于可用性与恢复验收；保管人交接、恢复演练和现场切换须有独立证据。初始化后须确认临时 root token 已撤销；重启后的人工解封限制必须交接，宿主 root 仍在信任边界内。
- 历史测试数据清除豁免不延伸到其他数据库。本轮不支持 Billing 016 前数据库升级：明确拒绝并保留数据，不推断原始期限或改写已有迁移账本。新环境仍需备份与恢复验收。

## 串行任务与验收

| 原任务 | 当前实现与入口 | 必须另行取得的环境证据 |
| --- | --- | --- |
| SECINF-01 原生密钥接口 | `internal/token/crypto*.go` 中现有 SecretProvider/Transit/AAD接线；`deploy/openbao/` | 真实 Transit 身份、权限拒绝、当前及历史密文解密、错误 AAD 拒绝 |
| SECINF-02 服务身份签发与轮换 | `deploy/keycloak/`、`deploy/production/workload-*.json.template`、各服务既有身份校验 | 真实 issuer/JWKS/audience/subject/精确权限、续取、过期与跨角色拒绝 |
| SECINF-03 密钥托管、Agent 与恢复 | `tools/kms-recovery/cmd/` 中 initializer、operator、snapshot 等既有工具，根 `scripts/account-platform-kms-agent-check.py` 与 `account-platform-kms-monitor.py`；`deploy/openbao/` | 实际保管人交接、独立双身份续租、受控轮换未知结果恢复、加密快照及恢复 |
| SECINF-04 生产依赖及业务接线 | `deploy/production/compose*.yaml`、`deploy/helm/account-platform/`、[NATS资源手册](../deploy/runbooks/nats-resources.md)、[原生KMS门禁](../deploy/runbooks/native-kms-security-gate.md) | 实际渲染与镜像/身份绑定、最小数据库权限、TLS、迁移顺序、真实 readiness、外部入口和结算回归 |
| SECINF-05 切换与交接 | [模型及Token发布手册](../deploy/runbooks/model-token-release.md)、[备份恢复手册](../deploy/runbooks/backup-restore.md) | 精确目标范围、当前数据边界、物理数据库及KMS恢复、灰度/回滚、明确的切换与发布授权 |

按前序依赖推进；单个工具或合同测试通过不能关闭整行环境验收。当前 Compose 的完成依赖和 Helm 迁移 hooks 保持有效，不能恢复旧 business profile 来替代现有模块边界。单机布局也不自动具备跨主机高可用或 HSM 保护。

## 手动安全门禁与证据

从仓库根运行 `npm --prefix account-platform run security:prepare-native-rotation` 或 `security:production-readiness`，详细配置、最小数据库读取权限及 prepare→保管人轮换→verify 顺序见[原生KMS门禁手册](../deploy/runbooks/native-kms-security-gate.md)。工具不会自动部署、轮换、迁移或删除数据。prepare 的 `PREPARED_NOT_GO` 不是切换许可；verify 的局部 GO 也不代替身份、业务和恢复门禁。

本轮合同测试使用真实本机 TLS/HTTP 客户端与受控 Transit 响应，不能冒称真实 OpenBao/Agent 联验。另有完整公开 Token schema 上的真实 PostgreSQL TLS、只读权限、RLS 拒绝与计数验证，不能代替真实 KMS 或现场恢复。数据库实例重启会使旧 prepare 证明失效，须保留原证据并按手册重新准备，不能覆盖未知结果。

本机 Node v24.19.0 对 literal IPv6 TLS 名称验证的实测失败保持拒绝，没有加入身份校验绕过；当前 DNS/IPv4 测试结果不扩展为所有入口兼容性证明。正式环境应在其实际 Node、DNS、CA、身份和部署结构上执行门禁。

恢复工具的 Python 单元和 shell 语法检查不等同于真实 pgBackRest、Linux 进程清理、systemd 调度或 PITR 验收。本轮未执行现场操作，旧现场成功记录不继承为本轮通过。

离线到期报告的原 service/timer、五项指标面板及其证据边界见[离线到期报告与账号平台面板](../deploy/runbooks/offline-expiry-and-dashboard.md)。恢复仓库载体不表示已安装计时器、导入面板或完成现场验收。

Portal 的 session-lineage 注销保护始终启用；无效部署开关已移除，同等保护版本的回滚核对见[Session-lineage 版本与回滚](../deploy/runbooks/session-lineage-version-rollback.md)。此文档不授权实际切换。
