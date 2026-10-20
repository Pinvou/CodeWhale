# 运维证据：格式、工件绑定与独立审批

以下命令均从仓库根目录运行。无参数的 HA/recovery/performance gate 只验证策略；导出的纯字段验证函数是 `FORMAT_ONLY`，合规 JSON 不证明现场事实。

有证据的 CLI 必须取得独立受控清单和原始工件。`--trust-file` 由复核人或受控发布系统提供，放在 `--artifact-root` 之外；不能让待验收报告自行生成可信清单。发布摘要和部署 ID 由受控发布计划提供，不能从报告反推。清单来源的访问控制和人工复核仍属于站点责任，脚本没有新增签名机构或采集器。

外部清单为封闭 JSON，字段固定如下（示例是格式说明，不能作为现场证明）：

```json
{
  "schema_version": 1,
  "kind": "HA",
  "environment": "production-new",
  "deployment_id": "deployment-example",
  "release_sha256": "<candidate-artifact-sha256>",
  "evidence_sha256": "<raw-evidence-file-sha256>",
  "artifacts": [
    {"claim": "/scenarios/0/evidence_sha256", "path": "receipts/pod-loss.txt", "sha256": "<raw-receipt-file-sha256>"}
  ]
}
```

`kind` 分别为 `HA`、`RECOVERY`、`LOCAL_RECOVERY`、`PERFORMANCE`；环境分别沿原合同 `production-new` 或性能的 `production-equivalent`。每个证据中的 `*_sha256`（除由可信候选直接绑定的 `release_sha256`）均需唯一 `claim` 项，使用 JSON Pointer 表示原字段位置；`path` 是导出根内普通原文件的相对路径。收据、备份校验对象、报告、数据集和分页轨迹不能只填写摘要而不提供文件。目录逃逸、软链接、硬链接、重复声明、遗漏、变更及摘要不符都拒绝。脚本只读文件，流式计算大工件摘要，不解密备份或连接服务；现场秘密不能进入普通证据导出。

```sh
node scripts/account-platform-ha-gate.mjs "$EVIDENCE_EXPORT_ROOT/ha-evidence.json" \
  --trust-file="$CONTROLLED_TRUST_FILE" --artifact-root="$EVIDENCE_EXPORT_ROOT" \
  --deployment-id="$EXPECTED_DEPLOYMENT_ID" --expected-release-sha256="$CANDIDATE_RELEASE_SHA256"
node scripts/account-platform-recovery-gate.mjs "$EVIDENCE_EXPORT_ROOT/recovery-evidence.json" \
  --trust-file="$CONTROLLED_TRUST_FILE" --artifact-root="$EVIDENCE_EXPORT_ROOT" \
  --deployment-id="$EXPECTED_DEPLOYMENT_ID" --expected-release-sha256="$CANDIDATE_RELEASE_SHA256"
node scripts/account-platform-recovery-gate.mjs --local-only --deployment-id "$EXPECTED_DEPLOYMENT_ID" --bootstrap "$EVIDENCE_EXPORT_ROOT/local-evidence.json" \
  --trust-file="$CONTROLLED_TRUST_FILE" --artifact-root="$EVIDENCE_EXPORT_ROOT" \
  --expected-release-sha256="$CANDIDATE_RELEASE_SHA256"
```

证据路径也必须在导出根内。全本机业务恢复去掉 `--bootstrap`，仍需原完整恢复集、域审计和保留扫描，不以基础恢复替代。命令成功仅输出 `ARTIFACT_BINDINGS_VERIFIED`，明确 `exercise_verified=false`、`release_authorized=false`、`deployment_status=NOT_DEPLOYED`；本机原状态仅作为 `declared_status` 输出。摘要相同不证明收据中的测量真实，复核人必须核原始来源、目标集群/备份、实际执行及权限，另行批准发布。未经该独立审查的自造 JSON 和自造工件不能放行部署。

本仓库没有通用现场备份/恢复采集器，也未提供固定 `account-restore-audit.sql`、`billing-ledger-audit.sql`、`token-restore-audit.sql`、`outbox-inventory.sql` 四个审计脚本。站点必须使用已有受控只读审计程序完整核对原领域守恒、权限、Outbox/Inbox 和恢复点，并由独立复核人留原始工件；缺少可执行审计或真实记录时保持恢复验收未完成，不能用门禁单元测试替代。完整部署/E2E 当前延期，离线格式及绑定测试不代表现场验收通过。
