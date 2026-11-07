# 已发布迁移的回滚包校验

迁移二进制嵌入现有 `migrations/manifest.json`。该不可变清单同时固定公开 Account、Billing、Token、payment-simulator 的 UpSQL 与 DownSQL；更新仍走原 `account-platform-migration-manifest.mjs append/check` 及发布审核。

当版本号与规范化 UpSQL 摘要匹配清单内的发布身份时，Load、Up、VerifyApplied 和 DownOne 都要求同一身份的原 DownSQL。即使迁移目录被重命名、SQL 被挂载到其他目录或调用方直接构造 Migration，也不能以未修改 UpSQL 为由接受变化的 DownSQL。执行此类回滚前还核对已记录的 UpSQL 摘要，拒绝对其他前向历史执行本包的回滚。

数据库中的原 checksum、name、applied_at 和账本位置不变，不新增列、不回填摘要、不追加 DDL 或角色权限。LF 与旧 CRLF 摘要的兼容规则保留。校验依据是既有发布清单，不将当前 DownSQL 自动认定为数据库过去使用过的内容。

未匹配该发布清单的第三方自定义历史保留原通用迁移行为；本校验不证明其历史 DownSQL。已有受支持的旧 SQL 兼容实现及领域回滚围栏继续负责各自契约，不把本校验当作允许回滚的授权。账本 schema/search_path 的另一项缺口单独处理，此修复不搬迁账本到 public。

`TestPublishedRollbackPackageIntegrityPostgres` 使用真实非超级用户迁移身份和原 Billing 001，验证篡改 DownSQL 在三个操作中均被拒绝且原 inbox/账本不变、旧 CRLF 摘要及无 schema CREATE 升级可继续、错误前向摘要拒绝回滚、恢复身份后可完成原可逆回滚，以及无预填摘要的自定义历史仍兼容。CI owners 的 schema 阶段要求根及三个子例完成，跳过或只报父用例成功不算通过。
