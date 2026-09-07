# 私有 KMS 配置

状态：SECINF-03B 隔离实测通过，NOT_DEPLOYED。这里只包含无秘密配置，不自动初始化，不是正式部署完成证明。

固定镜像：`quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641`（2.6.2）。

- `server.hcl`：真实 Raft、TLS、声明式脱敏审计；非 dev，不自动解封。KMS 只接入专用私网，正式环境没有公网管理入口。
- `agent.hcl`：每服务独立 Agent 与 AppRole bootstrap，只投递文件，不开启代理或 HTTP listener。
- `runtime-policy.hcl`：只访问 `pinvou-token-data` 的加解密与自身租约；生产初始化将相同权限分别注册为两个服务策略，AppRole 仅关联其自身策略，不附加 default/root。
- `approle.json`：只包含通用约束；初始化时加入唯一该服务的 `token_policies`。网关和凭据 Portal 分别创建角色、RoleID、SecretID、sink 目录，不能共用秘密。

正式 token 5 分钟有效，续租最多累计 15 分钟后必须重新认证。SecretID 本身是长寿命机器 bootstrap 秘密，仅 Agent 读取，并非业务短期 token；至少每 30 天轮换，泄露或角色停用立即销毁 SecretID accessor 并撤销该角色的现存 token accessor。换新后强制 Agent 重新认证，确认新身份可用，再撤销旧 SecretID；不要在新值投递前盲删旧值造成中断。role-id/secret-id 文件由运维写入 0600，Agent 只读；业务容器不能挂载这些文件。

KMS/Agent 固定 Linux UID/GID 65532:65532。数据、审计、单服务 sink 目录由部署步骤预创建并限定所有者；配置、TLS、信任根和 bootstrap 只读挂载。sink 位于受控 `/run` 内存文件系统，每服务单独目录，业务只读目录而非单文件，文件由 Agent 写为 0600。重启后可重新签发，不备份短期 token。

根 CA 私钥离线加密保管；KMS 仅挂载服务器叶证书私钥。不关闭 TLS 校验，不把 root token 或解封材料放入业务镜像/环境/日志。单节点 Raft 不是高可用，服务器重启后需保管人参与解封。禁用未加密 swap、休眠及系统 crash dump，宿主 root 仍在信任边界内。

审计目录持久化并按容量告警。外部轮转采用 rename/create（0600、同 UID），向固定 KMS 进程发送 SIGHUP 重开日志，再实际调用并验证新文件持续有 HMAC 审计；不使用 copytruncate、`log_raw=true` 或 discard/stdout 替代。轮转与磁盘空间门禁未接线前不放量。应用普通日志与 KMS 审计必须分开。

Raft 快照是含加密密钥的数据备份，不等于解封材料。正式恢复必须同时具备验证过的快照和离线解封材料，在隔离恢复实例上实际读回旧密文；仅重启解封或备份文件存在不算恢复演练。初始化、加密响应托管、快照外层加密、真实恢复以及旧 root 撤销仍归 SECINF-03C/04。

参考：[Raft](https://openbao.org/docs/configuration/storage/raft/)、[声明式审计](https://openbao.org/docs/configuration/audit/)、[AppRole](https://openbao.org/docs/agent-and-proxy/autoauth/methods/approle/)、[审计轮转](https://openbao.org/docs/audit/file/)。

## 隔离验证

Linux Docker 操作者先取得上述固定镜像，运行 `python3 scripts/account-platform-openbao-fixture.py --isolated-fixture --config-directory account-platform/deploy/openbao`。只创建独立临时网络、容器和目录，无业务挂载及公开端口；测试结束精确清理，不 prune。需要宿主可路由新建的私有 Docker 网络。

2026-09-07 实际完整运行输出 `OPENBAO_INFRASTRUCTURE_FIXTURE_PASS`：可信 TLS 及证书负例、双 AppRole 隔离、真实 Agent 续租/重认证/旧 token 实际过期、权限拒绝与强制 AAD、v1→v2 轮换、HMAC 审计和 SIGHUP 重开、Raft 重启后封存拒绝与解封后旧密文读回、撤销测试 root 后拒绝。正式租期模板不变；测试只缩短隔离角色租期。另有 4 项静态配置测试通过。此结果不包含快照恢复、正式初始化和离线恢复交接。
