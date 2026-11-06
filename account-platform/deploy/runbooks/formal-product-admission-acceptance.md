# 正式商品准入现场验收

在获授权的隔离 Linux 站点，用同一候选版本完成可信原件入库、双人核验、首次发布和失效演练。指定商品、商户资料、支付渠道和两名实际审核人。演练站点须与生产流量、数据库和支付回调隔离，使用获授权的真实商户资料和供应商验收环境。校验器仅识别文件格式，由两名审核人核验资料来源和商品事实。

## 装配与预检

1. 记录候选提交、Billing、Portal 和校验器镜像摘要、站点部署 ID、目标架构和执行时间。按受支持的 Billing 迁移入口核验已安装 `900037` 和 `900038`。真实资料只在受控系统保管，工单只引用受限证据位置。
2. 使用同源的现行个人金融或模型 Compose 基线。仅在隔离验收中最后追加 [`compose.product-admission-acceptance.yaml`](../compose/compose.product-admission-acceptance.yaml)，用 Compose `config --quiet` 检查完整组合和站点私有配置。装配使用独立验收项目名，保留原数据库和审批服务配置契约。核对实际项目名、外部网络和所有依赖均属于隔离站点。生产发布沿用[生产装配预检](production-render-preflight.md)的固定允许清单。该验收 overlay 不在清单中，生产编排及其独立批准仍是正式启用前置。
3. 在站点私有 Compose 插值文件中填写下表。固定校验器镜像的依赖、目标架构与 PDF 验证按[校验器说明](../../cmd/evidence-image-validator/README.md)核验。隔离组装后的 `config --quiet` 只证明模板可解析。

个人金融装配的只读配置预检从仓库根目录执行。模型装配按[模型部署说明](../production/README.models.md)追加其基线文件，再将验收 overlay 放在最后。

```sh
docker compose --env-file "$SITE_ENV_FILE" \
  -f account-platform/deploy/production/compose.personal.yaml \
  -f account-platform/deploy/production/compose.personal-workload.yaml \
  -f account-platform/deploy/production/compose.finance.yaml \
  -f account-platform/deploy/compose/compose.product-admission-acceptance.yaml \
  config --quiet
```

| 变量 | 站点前置 |
| --- | --- |
| `PRODUCT_ADMISSION_ACCEPTANCE_SOCKET_DIRECTORY` | 专属已挂载的空 tmpfs 目录。用 `findmnt`、`stat` 和容器实际 UID/GID 映射核对挂载和所有者。容器内目录为 `65533:65533`、`0750`，祖先由 root 或 worker 拥有，组和其他用户不可写。只挂给 Billing 和校验器，Billing 侧只读。 |
| `PRODUCT_ADMISSION_ACCEPTANCE_TRANSIT_TOKEN_DIRECTORY` | Billing 专属可轮换 sink 目录，含 `token`。只向 Billing 只读投递，校验器不挂载任何业务凭据。 |
| `PRODUCT_ADMISSION_ACCEPTANCE_TRANSIT_URL`、`PRODUCT_ADMISSION_ACCEPTANCE_TRANSIT_KEY` | 真实 OpenBao Transit HTTPS origin 和独立原件 key 名。Billing 复用站点信任包 `/run/pinvou/trust/ca.crt`。授权限当前原件 key 加密及仍有保留期的原件 key 解密。 |
| `OPENBAO_NETWORK` | 已按 [OpenBao 部署说明](../openbao/README.md)单独建立的内部网络。预检 `Internal` 和实际成员，确认与业务 `backend` 不同，且目标 OpenBao 在该网络。验收 overlay 仅为 Billing 增加这一网络，保留模型装配的原有成员和 TLS、token 校验。 |
| `PRODUCT_ADMISSION_ACCEPTANCE_VALIDATOR_IMAGE` | 已核验且本地可用的不可变 Linux PDF 校验器镜像。 |
| `PRODUCT_ADMISSION_ACCEPTANCE_VALIDATOR_MEMORY`、`PRODUCT_ADMISSION_ACCEPTANCE_VALIDATOR_CPUS`、`PRODUCT_ADMISSION_ACCEPTANCE_VALIDATOR_PIDS` | 本站点的内存、CPU 和进程限制。核对实际 cgroup 应用了限制，内存与内存加 swap 上限相同。 |

先确认专用 Transit sink 可用和校验器 socket 已监听，再验证 Billing 上传。校验器以 `65533:65533` 运行且无网络，Billing 继续用 `65532:65532`，仅增加连接 socket 所需的组。验证正确及错误 UID 的两向 peer 校验、只读挂载和无凭据投递。替换 socket 目录前暂停验收上传，确认旧 Billing、校验器及其子进程退出。在站点私有配置指向经验证的新空 tmpfs 目录后，**重建 Billing 和校验器两个容器**，核对两侧绑定的是同一新目录，再恢复上传。容器原地 restart 会保留旧挂载。按[重启边界](../../docs/evidence-validator-socket-restart-draft.md)演练遗留 socket、子进程超时和 OOM，确认拒绝上传、原件无部分写入，并在受控重建后恢复。记录实测资源预算和目标架构。

经实际 Portal/边缘路由验证 10 MiB 原始字节边界、多 1 字节拒绝以及超出普通请求时限但仍在原件处理预算内的成功上传。F01 NGINX 只为原件上传的精确路径配置 10 MiB 和 120 秒代理读间隔，并关闭该路径的请求缓冲。Billing 在接收原件、校验、加密和落库阶段设 60 秒总预算，Portal 对此上传的内部 Billing 请求设 70 秒预算。保留其他路由的限制。实际站点若使用其他 Ingress，验收相同的路径、容量、时限和原件不落代理临时文件条件。

## 操作与证据

| 场景 | 预期结果 |
| --- | --- |
| 合法 PNG、JPEG、PDF 通过管理台上传 | 保留原字节及其摘要；原件以站点专属 Transit key 保管，按授权下载时摘要相同。格式无效、需密码的 PDF、错误 peer、Transit 不可用和越权下载均被拒绝。 |
| 同一 `FORMAL` 草稿完成双人核验 | 核对商品版本、渠道、外部 SKU/BOM、金额和原件来源。上传或创建申请后无批准票。两人用不同实际用户身份在近期认证后分别确认，第二票完成才签发。 |
| 核验反例与回执恢复 | 同一人换会话投第二票、越权、超过近期认证窗口、原件或版本变化、过期以及自报证据均不能签发。受控中断后按原事实查询原回执，按原幂等键恢复，签发结果和审计保持单一效果。 |
| 首次发布与后续失效 | 首次发布使用原管理入口并校验原证据、商品版本及真实验收渠道。确认 `TEST_ONLY` 不能授权正式发布。分别演练证据到期和撤销，确认当前绑定停止接收新订单。续证不会自动解除暂停，历史订单和原证据保留。 |
| 留存、备份和清理 | 核对留存期限、法律保留与恢复副本覆盖。使用符合现行规则的隔离历史样本证明有效留存、保留生效和到期后清理，保存备份、快照、归档和副本的独立复核记录。 |

每个场景保留执行人与复核人、版本及部署标识、开始和完成时间、通过或拒绝结果、原始回执的受限位置及摘要。上传成功或健康检查通过仅证明对应环节。站点须取得真实 Linux、OpenBao、渠道、有效留存和失效闭环的完整记录，再由商品运营、安全与发布负责人确认正式启用。缺项时保持原件与人工准入关闭。
