# 已发布迁移的回滚包校验

迁移二进制嵌入现有 `migrations/manifest.json`。该不可变清单同时固定公开 Account、Billing、Token、payment-simulator 的 UpSQL 与 DownSQL；更新仍走原 `account-platform-migration-manifest.mjs append/check` 及发布审核。

提供回滚脚本且版本号与规范化 UpSQL 摘要匹配清单内的发布身份时，Load、Up、VerifyApplied 和 DownOne 都要求同一身份的原 DownSQL。即使迁移目录被重命名、SQL 被挂载到其他目录或调用方直接构造非空 DownSQL，也不能接受变化的回滚内容。领域历史兼容转换在替换 SQL 前先校验原发布包，不能让旧库适配丢失当前输入的回滚身份。

既有生产历史工厂只提供 UpSQL，这类正向描述继续允许 Load、Up 和前向账本验证，不认证缺失的回滚内容。DownOne 仍以不可逆拒绝，不能执行回滚或删除账本。Load 保留是否实际提供 Down 文件的元数据，所以“没有回滚文件”与“提供空回滚文件”不同；对已提供的空脚本和已加载脚本改空仍会拒绝。

执行回滚前还核对账本中已有的非空前向摘要，不以传入 UpSQL 是否命中清单决定该义务。给待执行 UpSQL 增加注释或伪造其 Checksum，不能将原已应用的发布历史降为自定义历史。LF 与旧 CRLF 摘要均按原内容核对；真正没有摘要的旧自定义账本保留原行为。

数据库中的原 checksum、name、applied_at 和账本位置不变，不新增列、不回填摘要、不追加 DDL 或角色权限。LF 与旧 CRLF 摘要的兼容规则保留。校验依据是既有发布清单，不将当前 DownSQL 自动认定为数据库过去使用过的内容。

未匹配该发布清单的第三方自定义历史保留原通用迁移行为；本校验不证明其历史 DownSQL。已有受支持的旧 SQL 兼容实现及领域回滚围栏继续负责各自契约，不把本校验当作允许回滚的授权。账本 schema/search_path 的另一项缺口单独处理，此修复不搬迁账本到 public。

`TestPublishedRollbackPackageIntegrityPostgres` 使用真实非超级用户迁移身份和原 Billing 001，验证篡改 DownSQL 在三个操作中均被拒绝且原 inbox/账本不变、旧 CRLF 摘要及无 schema CREATE 升级可继续、错误前向摘要及“改 Up 后降为自定义”均拒绝回滚、恢复身份后可继续原可逆回滚，以及空摘要和有效摘要的自定义历史仍兼容。旧026的 LF/CRLF 公开升级回归还覆盖直接领域 Up/Verify 接收改变的当前 Down 时拒绝、旧财务事实及账本不变、原包继续升级和验证。CI owners 的 schema 阶段要求对应根及精确子例完成，跳过或只报父用例成功不算通过。
