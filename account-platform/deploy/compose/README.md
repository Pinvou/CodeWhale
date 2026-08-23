# 本地 Compose 环境

1. 将 `.env.example` 复制为 `.env`，把每个占位值替换为不同的随机值；`.env` 已被仓库忽略。
2. 从仓库根执行 `node scripts/account-platform.mjs dev up`。
3. Keycloak 开发入口为 `http://127.0.0.1:18080`；Account、Billing、Token Gateway、Payment Simulator、Token Portal、New API 和 Identity Adapter 分别绑定 `18101` 至 `18107` 的对应回环端口。
4. 真实 New API 通过 `real-newapi` profile 启动，仅绑定 `http://127.0.0.1:18106`；首次启动后需在本机完成管理员、上游 Channel 和最小权限 API Token 初始化，具体见 [`../runbooks/newapi-deployment.md`](../runbooks/newapi-deployment.md)。

## 平台管理台

平台管理台位于 Token Portal 的 `/admin.html`，复用 Keycloak 登录；实际业务授权由 Account 平台角色和原子权限码决定，Keycloak Realm Role 不再授予后台权限。管理台提供 Account、Billing、Token Gateway、系统运行与审计等受保护的运营功能。供应商凭据只展示状态、指纹和更新时间等元数据，真实密钥不会返回浏览器。

远程部署只向公网开放 Keycloak 的 `/realms/` 与 `/resources/` 登录所需路径，管理控制台 `/admin/` 必须通过受限网络或本地隧道访问，具体见 [`../runbooks/keycloak-admin-access.md`](../runbooks/keycloak-admin-access.md)。

首次部署或已有 Keycloak 数据卷升级后，在本机通过环境变量提供平台管理员账号和密码，再运行初始化脚本。密码不得写入仓库中的 `.env.example`、命令历史或部署日志：

```sh
docker compose --env-file account-platform/deploy/compose/.env \
  -f account-platform/deploy/compose/compose.yaml exec \
  -e PINVOU_ADMIN_USERNAME -e PINVOU_ADMIN_PASSWORD keycloak \
  sh /opt/keycloak/data/import/provision-platform-admin.sh
```

部署时应将 `provision-platform-admin.sh` 以只读方式挂载到容器，或复制脚本内容到受控的运维环境执行。该脚本只创建登录身份和迁移期 Realm Role，不授予 Account 业务权限；升级时必须按 [`../runbooks/account-rbac-migration.md`](../runbooks/account-rbac-migration.md) 将经审批主体明确映射到具体平台角色。

## 手机注册、登录与商城支付

开启 `MOBILE_AUTH_ENABLED=true` 前，需要配置阿里云号码认证服务 AccessKey、系统赠送签名和系统赠送模板（登录/注册模板 CODE 为 `100001`）。验证码由阿里云动态生成，并由 Account Service 调用 `CheckSmsVerifyCode` 核验；服务端不生成、保存或记录明文验证码。所有密钥仅放在部署环境的 Secret 中。

Commerce Customer 开通与手机认证相互独立。`COMMERCE_PROVIDER_ID` 和 `COMMERCE_ADAPTER_VERSION` 冻结任务所用 Provider/Adapter 身份；配置当前 H3C Adapter 的 `H3C_MALL_BASE_URL`、`H3C_MALL_APP_KEY` 和 `H3C_MALL_APP_SECRET` 后，Account Service 启动供应商中立同步 worker。未配置 Adapter 时注册、登录和模型调用仍可用，Mapping 保持待处理，只有依赖它的支付被关闭。启用商城支付时，Billing Service 直接读取数据库中已启用套餐的 SKU 和暂定 UAT 价格，避免额外环境变量形成第二份商品配置。

已有 Keycloak 数据卷升级后，还需要执行一次可重复运行的初始化脚本，以安装手机直登 Flow、服务账号权限和手机号 Token Claim：

```sh
docker compose --env-file account-platform/deploy/compose/.env \
  -f account-platform/deploy/compose/compose.yaml exec keycloak \
  sh /opt/keycloak/data/import/provision-mobile-auth.sh
```

该脚本还会将 Keycloak 用户资料规则调整为适配手机注册：邮箱、姓、名保持可选，手机号及其验证状态仅允许账号服务写入。脚本可重复执行，并会同步更新既有 Realm，避免新账号已创建却因资料不完整而无法在应用内登录。

注册短信由阿里云发送；注册成功后 Account Service 会在后台为同一手机号创建或确认商城用户。商城用户同步完成前，充值接口保持关闭。支付订单和二维码完全以商城返回值为准，客户端不会自行生成二维码；商城返回 `PAID` 后 Billing 才会把积分记入钱包。

当前商城合同没有关闭订单、退款、支付回调和对账单接口，因此这些能力不会伪造：订单关闭跟随商城的 `PAY_CLOSED`，退款和渠道账单会明确返回“供应商能力不可用”。上线退款前需补充商城退款合同。

确定性集成烟测使用独立 project：`node scripts/account-platform.mjs test integration`。命令会启动完整栈、验证五个 Readiness、请求/Trace 关联头和 Portal 安全头，然后仅删除该测试 project 的卷。

数据库、Redis、NATS 和服务间网络不对公网开放。`compose.yaml` 是可重复的开发/测试基础拓扑；当前单机环境在它之上叠加 `compose.remote.yaml` 和不入库的站点专用覆盖运行，实际流程见 [`../../docs/deployment.md`](../../docs/deployment.md)。Kubernetes 生产目标仍以 Helm Chart 为准，不能把基础 Compose 文件单独当作高可用生产模板。Payment Simulator 不得承接生产流量。
