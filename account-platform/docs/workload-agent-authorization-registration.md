# Workload 投递器授权入口登记补全

状态：定向验证通过。SECINF-02 的 `workload-token-agent` 已实现并有真实签发联验，但未同步登记中央 CLI 授权目录，全量门禁报 `unclassified_cli:workload-token-agent`。该遗漏早于原生 Compose 接线，本任务独立修复和提交。

需求与设计：将其精确登记为单客户端身份投递器，不是身份签发者、HTTP 服务或授权管理员。它通过受控 bootstrap 文件调用既有 Keycloak client-credentials，只能投递验证过的固定 issuer/audience/subject；`-check` 只读核验已有短期 token。复用原有入口扫描和缺失拒绝，禁止排除新命令、用通配策略或跳过门禁。

任务：增加一条 source/name/strategy 精确登记；将实际 CLI 数量从 18 更新为 19；增加删除此条后必须拒绝的回归。验收要求原 HTTP 339、任务 27、事件 6 数量不变，策略/未知权限/漏登记负例继续失败。该登记不修改任何运行权限或服务、不会签发身份，不代表正式 workload 部署完成。

验证：五项授权入口合同测试全部通过，真实源扫描确认 339 HTTP / 27 task / 6 event / 19 CLI；新命令漏登记负例及原权限/范围/事件缺失负例继续拒绝。未修改门禁实现、HTTP 权限或投递程序。
