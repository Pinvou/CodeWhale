# Session-lineage 版本与回滚

本页说明当前始终启用的会话保护及版本兼容边界，不安装、启停、部署或回滚任何实例。用户已选择保留 Portal 始终启用并移除无效开关；没有恢复旧版 `LogoutSession` / `DeleteSession` 可切换分支。

## 当前配置

`/health/session-lineage` 如实返回 `schema_version=1, capable=true, enabled=true`。登录记录前驱 session 与 lineage root；OIDC callback 经原子 `CommitLoginSession` 验证前驱注销标记再保存；显式注销经 `LogoutSession` 原子标记整条 lineage，并删除所有代际 session 及其 approval-wire。刷新写入同时验证 refresh 锁 owner、未变化旧 session 和 logout 标记，不能重新创建已删除 session。

当前 `PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED` / Helm `portal.sessionLineageLogoutEnabled` 从未被这套 Config/启动入口消费，原 false 值不会关闭保护。本轮从 Compose、Helm deployment、默认 values 与严格 schema 移除这些误导项。应用始终启用的 Go 实现和 capability 不变。更新部署输入时应移除自己的旧 values 覆盖；严格 schema 会拒绝仍携带旧键的 values，而不会悄悄接受一个无效安全开关。外部自定义清单也不能把该环境变量当作停用机制。既有 Helm release 保存的 values 或 `--reuse-values` 也可能携带旧键；只改仓库默认值不会清除这类输入。授权操作方应在受控输入中明确移除旧 release 覆盖并先校验最终合并 values，未消除旧键时停止升级；本页不自动修改 release 或给出可能展开秘密的命令。

## 安全版本回滚的检查责任

操作方先取得针对准确环境和镜像 digest 的另行部署/回滚授权，记录当前及候选镜像身份、配置和实例范围，保存既有部署证据。只允许回到**同等保护且兼容当前存储的版本**；健康入口的布尔值只是诊断信号，不能单独证明目标二进制符合以下语义。

1. 核对候选的实际登录、callback、注销、刷新与桌面 handoff 实现及相应验证。目标须保留前驱/lineage root 传播，callback-after-logout 和 stale refresh 拒绝，整条 lineage 注销、幂等注销未知 ACK 重试，以及 handoff 建立新独立 lineage、使旧流程失效的边界。不能退回只删除单一 session 的实现。
2. 核对 Session/LoginFlow JSON 字段兼容，包括 `LineageRoot` 对应的 `lineage_root`、`PreviousSession` 对应的 `previous_session`、会话期限及 nonce，以及 `portal:flow:*`、`portal:session:*`、`portal:session-root:*`、`portal:lineage-members:*`、`portal:logout:*`、`portal:refresh:*`、`portal:session-handoff:*` 和 `portal:approval-wire:*` 的读写/ACL/原子操作语义。旧 session 缺少 root 时当前只回退其自身 ID，不代表任意旧二进制与现存多代际数据兼容。
3. 保留当前 tombstone/映射及 TTL 不缩短规则：注销覆盖尚在有效期的 lineage、refresh 会话及未完成的十分钟登录流程，重试不能缩短已有标记。不能通过删除 Redis key、清空库、修改 TTL 或恢复旧 Redis 快照把已注销身份复活。不得为完成回滚而削弱 Redis TLS/ACL 或丢弃其他业务状态。
4. 在另行授权的隔离依赖环境中，以实际目标版本验证多代际注销、迟到 callback、logout 与 refresh 交错、注销 ACK 丢失重试、handoff 与旧注销隔离；核对当前数据与迁移兼容。仓库已有 `retained_lineage_proxy_test.go`、`retained_login_test.go`、`store_integration_test.go`、`session_handoff_test.go` 是对应验证入口，不是本页已经执行的站点证据。没有可证明同等保护的目标版本时停止二进制回滚计划，交付具体兼容差异等待决定。

旧提交 `828242031c42911099792ec78c1f746ed8620c49` 的 phase1/phase2、false 回切以及固定 600 秒观察/30 天二进制回滚脚本基于可切换实现和旧 business Compose；本轮不交付这些旧操作入口。等待这些旧窗口过去，也不能授权退回弱保护版本；不得凭旧现场 PASS、ConfigMap/container 记录继承当前操作或清数据授权。本页不新增替代等待窗口、自动部署或恢复 SLA。

本轮只做源码与受控部署文件合同核对，没有执行 Redis/Keycloak 多副本、systemd、Docker、Helm、现场启停或任何真实账户操作。运行时既有保护的历史验证与本次移除无效配置的静态验证分别列证。
