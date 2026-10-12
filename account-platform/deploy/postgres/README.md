# Billing 数据库角色与升级

Billing 必须使用独立数据库，不能与其他应用共享 public schema。首次本地开发、Compose 部署或已有环境升级前，由数据库管理员在目标数据库执行 `billing-roles.sql`：psql 使用 `-X -v ON_ERROR_STOP=1 -v billing_database=<实际独立数据库名> -f`。脚本核对连接数据库与显式目标一致，并在脚本内启用 ON_ERROR_STOP；缺失目标变量或连接到其他数据库均以非零退出码停止，不创建或修改角色。管理员、migrator 和 runtime 连接分别通过本机私有 pgpass 或部署 Secret 提供；脚本不设置密码，不复制凭据。本阶段没有 Helm/chart 或本地数据库 Compose 制品；以后新增这些制品须显式执行同一初始化步骤。

`billing_schema_owner` 为 NOLOGIN；`billing_migrator` 为 LOGIN/NOINHERIT，迁移连接选择 `MIGRATION_ROLE=billing_service`；runtime 使用受限 `billing_service`。只有 owner 单向继承 runtime 的旧对象权限，runtime 不得属于 owner，包括间接成员关系。管理员须通过部署认证机制配置两种 LOGIN 身份，不能将测试的 loopback trust 用于生产。先完成备份并保留可核验回执，再执行原 `cmd/migrate -domain billing` 流程；必须使用 `billingschema.Up`，不能绕过兼容与权限收回。

900003 安装独立 schema、table、sequence 和 function owner，收回 runtime CREATE、TRIGGER、TRUNCATE 和迁移账本写权。runtime 保留业务 DML、账本只读、proof 的 SELECT/INSERT，以及已审核生命周期函数的 EXECUTE。所有 Billing 表都有 statement TRUNCATE guard。SECURITY DEFINER 生命周期函数固定 owner 和可信 search_path；runtime 不能在 public 创建对象覆盖其依赖。旧 SQL 要求 runtime 拥有部分对象；独立 migrator 仅在同一未提交事务中临时借回，执行后收回，再由独立 owner 记账。后继较小编号增量仍按每个 version/checksum 执行，新增对象在同一事务收归 owner。未知 SECURITY DEFINER 函数会阻断收回，须先审核并加入显式许可。

旧 RESOLVED chargeback 保持状态和原始证据，不自动执行资金动作。029 兼容执行只在存在旧行且冻结 SQL/hash 完全匹配时，将新 CHECK 安装为 NOT VALID；新写和更新为 RESOLVED 仍由数据库拒绝。人工核实前不得运行 VALIDATE 或批量改状态。检查待核实记录：`SELECT id,status FROM chargeback_cases WHERE status='RESOLVED'`。检查约束：`SELECT conname,convalidated FROM pg_constraint WHERE conrelid='public.chargeback_cases'::regclass AND conname='chargeback_cases_status_check'`。未 validated 的约束和只读历史属于明确的人工运维待办。

日期 CHECK 会拒绝已有无效 entitlement 有效期并回滚，操作员须调查事实，不能自动扩期或删除记录。900004 将旧退款非终态标记为 RESULT_UNKNOWN；无供应商终态证据不重发、不释放冻结。900005 为回调通知新增 provider_id/provider_version，旧行保持 NULL，不猜测或回填验签来源；缺少来源的待处理通知保留事实并拒绝资金处置，须独立人工对账。通知验签来源、冻结订单来源和当前适配器身份必须一致才允许继续。三项硬化均为 forward-only；回退应用版本须先核实权限和 SQL 兼容，不得恢复 runtime owner。

隔离 PostgreSQL 回归覆盖历史状态与证据保留、新写拒绝、runtime DML 与禁止 DDL、生命周期 EXECUTE、未来增量和失败回滚。测试仅使用专属 UUID 数据库，不代表真实供应商或生产部署验收。

已冻结的 Account 000050 down 与 Billing 900002 up 原文件带末尾空行，不能为格式门禁重写其历史 checksum。`.gitattributes` 仅对这两个精确路径豁免 `blank-at-eof`；其他空白问题及新迁移仍受原检查，迁移内容与校验值保持不变。
