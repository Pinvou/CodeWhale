# Keycloak Realm 升级

`deploy/keycloak/Dockerfile` 产出的 Pinvou Keycloak 镜像同时包含认证 SPI、原子身份安全端点和
`/opt/keycloak/bin/pinvou-provision-mobile-auth.sh`。只替换为上游 Keycloak 镜像不构成有效发布。

每次部署该镜像都必须运行幂等 Realm 升级。升级会更新认证流、Provisioner 权限以及 declarative
user profile；后者显式声明 `pinvou_identity_security_*` 与 `pinvou_password_reset_*` 为仅管理员可见、
可写属性。升级失败必须阻断业务发布，不能仅重启 Keycloak 或跳过该 Job。

## Docker Compose

`compose.keycloak.yaml` 的 `keycloak-realm-upgrade` 一次性服务在 Keycloak 健康后自动运行镜像内脚本。
`KEYCLOAK_REALM_UPGRADE_SECRET_DIRECTORY` 必须是 UID 1000 可读的私有目录，并包含：

- `admin-username`、`admin-password`：本次受控升级窗口的临时 Realm 管理身份；
- `provisioner-client-secret`：投递给 Identity Adapter 的同一 Provisioner 密钥；
- `admin-truststore.p12`、`admin-truststore-password`：只信任 Keycloak 管理端证书的 truststore。

升级成功并核对 user profile 后，立即撤销临时管理身份并按既有密钥投递流程清除副本。退出码非零时，
保留日志和现场，禁止启动或继续更新业务 Compose 项目。

## Helm

生产 values 固定 `keycloak.realmUpgradeEnabled: true`。`post-install,post-upgrade` Job 使用同一 Pinvou
Keycloak 镜像，并从 `global.runtimeSecret` 读取 `keycloak-realm-upgrade-admin-username`、
`keycloak-realm-upgrade-admin-password` 和 `keycloak-provisioner-client-secret`。发布系统必须等待 Hook
成功；缺少 Secret、使用不含脚本的镜像或 Job 失败都会令 Helm 发布失败。完成后应撤销临时管理员，
但不得删除 Provisioner 客户端密钥。

CI 的 `Keycloak identity security two-node PostgreSQL gate` 使用两个真实 Keycloak 节点和 PostgreSQL，
验证 SPI 路由、鉴权、事务回滚、凭证与 marker 同事务，以及迟到旧 fence 不覆盖新操作；该门禁不允许跳过。
