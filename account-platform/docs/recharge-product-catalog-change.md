# 充值商品目录运营化需求变更

> 状态：待实现
>
> 变更日期：2026-08-26
>
> 适用范围：账号平台 Billing、运营后台、权限系统和发布门禁

## 1. 变更结论

充值商品不再通过修改业务代码、数据库迁移、需求文档、架构文档或任务文档来添加、上架、下架和删除。具有对应系统权限的运营角色必须在后台完成这些动作；运行中的 Billing 从数据库读取当前目录与 Provider 映射，动作提交后无需重新构建或重启服务即可生效。

本文和三份基线文档只描述稳定能力、状态机、权限及验收，不保存当前在售 `sku_bom`、金额、权益或商品数量。具体商品事实属于环境数据，只能从后台、受控导出或审计记录查询。

## 2. 业务范围

后台新增供应商中立的充值商品管理能力：

1. 添加商品：创建不带准入证据的 `DRAFT`，同时编排通用商品和一个 Provider 映射草稿。通用商品录入稳定 `product_id`、中英日展示名称、权益组成、有效期、发票品名和展示顺序；映射录入 Provider/Adapter 版本、外部 `sku_bom`、含税金额、币种和渠道。
2. 上架商品：将有效草稿发布为 `ON_SALE`，原子生成不可变 Provider 商品映射版本并启用目录；上架后用户端可售性仍须通过 Provider 和商品运行时安全控制。
3. 下架商品：将 `ON_SALE` 转为 `OFF_SALE`，原子结束当前映射有效期并停止新订单；既有订单、支付查询、回调、履约和财务证据继续收敛。
4. 删除商品：只允许删除 `DRAFT` 或 `OFF_SALE`。删除采用逻辑墓碑；曾上架商品的业务标识、映射、订单快照和审计永久保留且不得复用。
5. 重新上架：`OFF_SALE` 可重新发布，但必须生成新的不可变映射版本，不能恢复或改写旧版本。

本次不提供“编辑已上架商品事实”。SKU/BOM、金额、权益、Provider 或发票事实需要变化时，运营人员应添加新商品并按顺序上架新商品、下架旧商品；并发期间每个订单始终冻结自己受理时的完整商品和映射快照。

## 3. 生命周期与约束

```text
DRAFT -> ON_SALE -> OFF_SALE -> ON_SALE
DRAFT -> DELETED
OFF_SALE -> DELETED
```

- `ON_SALE` 不得直接删除。
- 所有写动作要求稳定幂等键、`expected_version`（添加固定为 0）、15 分钟内的近期重新认证、非空原因（添加和首次上架可使用业务说明）及不可抵赖审计。
- 旧版本或非法状态返回稳定冲突，不允许最后写入者覆盖。
- `catalog_item_id` 和 `product_id` 创建后永不复用。同一 Provider/Adapter 版本下活动 `sku_bom` 不得歧义；只有从未上架的 DRAFT 删除后，其外部 `sku_bom` 才可用于纠正录入错误。
- 正式收款商品上架前必须具有不可变 `admission_evidence_ref`。证据只能由具有 `internal.billing.product-admission.verify` 的 `billing-admission-verifier` 工作负载在现场校验 Provider 后写入 Billing 追加表；保存签发工作负载、验证方式、受控来源引用、签发/到期时间和 canonical publish fingerprint。指纹覆盖环境、`catalog_item_id/product_id`、中英日名称、BASE/BONUS、有效期、发票、Provider/Adapter、外部 SKU/BOM、金额、币种、渠道和正式准入类型。发布事务重新计算并恒定时间比较指纹；缺失、过期、存在权威撤销事实、范围不一致或 `TEST_ONLY` 证据在生产均失败关闭。
- 撤销不是修改证据状态，而是向 `product_admission_revocations` 追加权威事实。具有 `platform.products.publish` 且近期认证的人员可提交带原因和幂等键的撤销命令；Provider 侧撤销只能由 `internal.billing.product-admission.revoke` 工作负载写入。每条事实保存证据 ID、单调序号、来源类型/主体、原因和 observed_at，不可撤回或删除；`REVOKED` 是“存在有效撤销事实”的派生状态。
- 准入证据是持续收款门禁。不可变映射永久保留发布时 `admission_evidence_ref`；独立追加式 `product_admission_bindings` 以版本和生效区间指向当前运行时证据，不改写历史映射。DRAFT 可对当前草稿版本验证，ON_SALE/OFF_SALE 可对指定不可变发布映射快照续证；新 VERIFIED 证据原子追加更高绑定版本。到期前续证可无缝接续，到期或撤销后监控任务立即关闭商品级运行时新单控制并返回 `PRODUCT_PAUSED`，事务内新单还须现场校验当前绑定；不得改写业务目录生命周期。失效后只有取得匹配的新 VERIFIED 绑定并完成既有支付控制双人恢复，才可重新开放新单。
- 商品业务上/下架与支付故障止损是两类事实。自动止损和双人恢复不得改写目录生命周期。

## 4. 权限与审批分类

新增五个原子系统权限：

- `platform.products.read`
- `platform.products.create`
- `platform.products.publish`
- `platform.products.unpublish`
- `platform.products.delete`

`PLATFORM_PRICING_ADMIN` 默认拥有全部五项；`PLATFORM_BILLING_OPERATOR` 和 `PLATFORM_AUDITOR` 仅拥有读取权限；超级管理员按系统角色真值表显式拥有全部五项。接口和 UI 只能按权限码判定，不得按角色名放行。

添加、上架、下架和删除均属于正常目录治理，登记为 `AUDIT_ONLY`，不进入财务事实双人审批；它们不能改写已确认订单、支付、权益或账本。支付故障后的 Provider/商品新单恢复继续遵守既有双人恢复流程。

## 5. 接口与界面

Billing 后台提供供应商中立接口：

- `GET /admin/v1/recharge-products`
- `POST /admin/v1/recharge-products`
- `POST /admin/v1/recharge-products/{catalog_item_id}/publish`
- `POST /admin/v1/recharge-products/{catalog_item_id}/unpublish`
- `POST /admin/v1/recharge-products/{catalog_item_id}/delete`
- `GET /admin/v1/recharge-products/{catalog_item_id}/admission-verifications`
- `POST /admin/v1/recharge-products/{catalog_item_id}/admission-verifications`
- `POST /admin/v1/recharge-products/{catalog_item_id}/admission-verifications/{verification_id}/retry`
- `POST /admin/v1/recharge-products/{catalog_item_id}/admission-verifications/{verification_id}/revoke`

写接口统一使用 `Idempotency-Key` 请求头；请求体包含 `expected_version` 和 `reason`，添加请求还包含通用商品及 Provider 映射草稿。商品后台写接口只接受 Billing 直接完成签名、issuer、audience 和 exp 校验的终端用户 OIDC Access Token，并只读取其中由 IdP 签发的 `auth_time`；不接受 Account 人类委托、Portal、请求头或请求体转述/自报认证时间。`auth_time` 超过 15 分钟时 Portal 以带防重放 state/nonce/PKCE 的 `prompt=login&max_age=900` 进入 IdP step-up，更新服务端会话后返回原页面；原写请求、幂等键和确认状态全部丢弃，用户必须重新检查资源版本并再次确认，禁止自动重放。列表使用 `cursor`、`limit`（默认 50、最大 200）、可选 `lifecycle_status` 与 `include_deleted`，按 `updated_at DESC, catalog_item_id DESC` 稳定排序；DELETED 默认隐藏，显式查询后可供有读取权限的运营人员查看。

验证流程固定为：DRAFT 选择当前草稿版本，ON_SALE/OFF_SALE 选择当前或指定历史发布映射版本；具有 `platform.products.publish` 的人员以幂等命令发起验证；验证工作负载冻结目标快照并异步现场校验；后台以读取权限展示 `PENDING/VERIFIED/FAILED/EXPIRED/REVOKED`、脱敏失败原因、证据有效期和绑定版本；发布权限可对 FAILED/EXPIRED 发起新验证轮次或追加撤销事实，不能改写原记录。首次发布只绑定当前草稿完整指纹一致的 VERIFIED 证据；草稿变化使旧证据不可用。续证只对完全一致的不可变发布快照追加运行时绑定，不能借续证改变商品或映射事实。

撤销、续证绑定和新订单按 `catalog_item_id` 使用同一数据库串行锁与单调 `admission_version`。撤销当前证据时同一事务追加撤销事实、结束当前绑定、递增版本并关闭商品新单控制；续证激活重新检查目标证据无撤销事实并追加更高绑定，但绝不自动打开控制。并发顺序无论如何都不能短暂恢复新单；订单事务同时锁定/检查当前绑定、撤销反连接和控制版本。撤销非当前历史证据只追加事实，不影响当前绑定。

运营 Portal 通过既有 Billing 管理代理访问这些接口。商品目录主视图只展示 `ON_SALE` 的当前在售充值包；运行时支付止损不会改写目录生命周期，暂停原因仍在支付渠道控制中查看。添加商品通过独立按钮打开模态页，创建成功的 `DRAFT` 进入独立的“草稿与历史商品治理”模态页继续完成验证和上架；该治理入口同时承载全生命周期筛选、准入验证状态/重试和上架/下架/删除操作，不能因精简主视图而隐藏非在售商品。后台页面完整提供简体中文、英文和日文；无读取权限时不请求或显示商品数据，无对应写权限时按钮不可见，添加选项只在打开添加模态页时加载，配置故障不得阻断在售目录读取。新旧商品允许在受控切换窗口短暂并售，运营界面必须展示 `replacement_product_id`、展示顺序和并售确认提示；用户目录按 `display_order, product_id` 稳定排序。

## 6. 数据迁移与发布约束

- 首次迁移使用 `INSERT ... SELECT` 把部署环境当前有效目录和映射导入运营目录，不在迁移中写任何具体 SKU/BOM、金额或权益。
- 已执行的历史迁移作为部署事实保留，不回写、不删除；从本变更生效后，新的商品变化禁止再创建含具体商品事实的代码迁移。
- 需求、架构和任务门禁检查三份基线文档不出现具体在售 SKU/BOM、金额、权益或商品数量；实现状态只报告能力和部署版本，不充当商品清单。
- 生产商品清单通过后台接口和审计查询，部署不以文档中商品值作为验收输入。

## 7. 对既有行为的影响

- 用户端接口仍只接受 `product_id`，金额、权益和 `sku_bom` 由服务端派生。
- 新订单实时读取当前有效目录及映射；管理动作无需重启 Billing。
- `recharge_product_catalog_items.lifecycle_status` 是业务上架的唯一事实源。旧 `recharge_products.enabled` 仅是兼容投影：生产运行身份无直接列写权限，只能由受控安全定义事务函数随目录转换更新；约束触发器拒绝任何漂移，所有新读路径必须联结权威生命周期。旧接口、任务和普通 SQL 不能单独改 `enabled`。
- 创建订单的第一笔本地事务锁定并冻结目录/映射快照、写入稳定商户订单号和 Provider 调用意图后提交并释放锁；之后才以该稳定订单号作为外部幂等键调用 Provider，再用第二笔事务归并结果。提交后崩溃或网络超时保留 `RESULT_UNKNOWN`，恢复 Worker 先查询外部事实；只有 Provider 明确支持创建幂等时才可用同一键重投，绝不持有数据库锁等待网络或生成第二个外部订单。
- 订单创建后，后续下架、删除或重新上架不改变订单、支付流水、权益包、退款/拒付或发票快照。
- 回调和主动查询仍按订单冻结的 Provider、版本、客户、金额及 `sku_bom` 校验，不按当前目录反查历史事实。
