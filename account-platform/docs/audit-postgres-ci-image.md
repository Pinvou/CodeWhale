# 审计导出 CI PostgreSQL 镜像验收

对应 [#1977](https://github.com/Pinvou/pinvou3/issues/1977)。2026-11-07 核验时，主线 `4696a6ac17b65d1cd0c6fa77dd7988698b0dcd7a` 已固定镜像；本记录补齐来源及真实 CI 兼容性证据，不修改镜像或生产部署。

## 固定来源与适用边界

`.github/workflows/pr-check.yml` 的 `audit-export-postgres`、`scripts/account-ci-postgres.py` 的 audit 服务准入和 `scripts/tests/pr-check-routing.test.mjs` 的字面守卫使用同一镜像：

```text
mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
```

对 Docker Official Image 的 `library/postgres` registry 只读核验了上述索引、所选 `linux/amd64` manifest 和 config 的 SHA-256 链。该 config 声明 `PG_MAJOR=16`、`PG_VERSION=16.15`。这证明固定来源及平台，不证明原可变 `16-alpine` tag 与该镜像逐字节等价。

审计和财务仍是独立服务；端口仅映射 runner 的 `127.0.0.1`。实际准入核对容器镜像、映射端口、匿名卷及容器内/端口连接的 PostgreSQL 身份，消费者使用准入输出。这里的 trust 仅用于一次性 CI 服务，不是部署 HBA 或 runtime grants 配置。

## 实际 Linux CI 验收

[PR Check 42355513062 / account-platform-test](https://github.com/Pinvou/pinvou3/actions/runs/42355513062/job/128352794117) 在提交 `2057c9b36df6daeba013113f8863888c557f36b8` 成功。该提交相对上述主线只修改路由测试诊断；受影响的 workflow、服务准入和审计 Go 代码字节相同。

`Audit export finite PostgreSQL rollback and persisted boundaries` 步骤实际以 `-race -json -tags=nomsgpack -mod=readonly -count=1 -parallel=1` 执行：

- `./internal/accountschema`：`TestAuditExportRollbackRoleStorageGuard`、`TestAuditExportPublicDownRetainsOriginalHistory`、`TestAuditExportGuardedRollbackTransactions`。
- `./internal/account`：`TestAuditExportPersistedBoundaries`。

原始 Go JSON 确认四个必需根及全部观测到的子例完整 run/pass，共34个具名结果，失败/skip 均为0；具名父子数量不代表34个独立业务场景。job 的初始化、服务隔离、审计步骤、停止自有 PostgreSQL 及最终资源退役检查均成功。其 `account-resource-lifecycle-1` artifact 内832份 JSON 收据均为 `released`；PostgreSQL 服务收据的剩余容器、卷和未登记项均为空。

此验收属于该固定提交的临时 Linux CI，不能替代其他架构、后续提交的门禁、最新 main 组合或生产发布验收。更换镜像时须同步上述三个载体，重新核验可信 digest/目标平台，并实际运行原四根和退役检查；不能删字面守卫、放宽根选择或增加全局镜像豁免取绿。
