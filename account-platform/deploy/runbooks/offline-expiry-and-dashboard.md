# 离线到期报告与账号平台面板

本页恢复既有运维交付载体，不安装、启用或部署服务；实际站点操作仍需单独授权。历史基线为 `828242031c42911099792ec78c1f746ed8620c49`，当前适配核对点为 `b05fb75cf2d5d23023aebe00c0fb217aec3a34fc`。仓库合同验证不代表 Linux/systemd、Prometheus、Grafana 或站点验收。

## 离线报告

`../production/pinvou-release-expiry.service` 与同目录 `.timer` 保留原计划：每日本机时间 08:00，最多随机延迟 5 分钟，持久补跑。没有新增自动轮换、数据库访问、凭据读取或撤权行为。

执行入口是当前仓库 `scripts/account-platform-release-expiry.py`。站点实施前须单独核对部署的脚本字节及 Python 版本；服务引用 `/opt/pinvou-release-expiry/account-platform-release-expiry.py`，不会自动从仓库复制或更新。配置及安装记录位于 `/opt/pinvou-release-expiry/config/check.json`、`config/installed.json`，状态仅写入 `state/latest.json`。

脚本要求绝对且无符号链接的路径、root:root 私有目录 0700、普通单链接文件 0600；状态目录同样受保护。配置只包含现有两名 runtime 角色及安装记录的 SHA256，不放密码、DSN、私钥或 Token。安装记录仅有角色名、生成时间与到期时间，记录周期保持原 90 天。此处不授予创建这些文件或读取既有站点配置的许可。

输出是 `RECORDED_EXPIRY_ONLY`，`live_database_checked=false`。退出码为 0=OK、1=14 天内 WARNING、2=7 天内 CRITICAL、3=EXPIRED、4=INVALID；非零退出在 systemd 中保留失败状态供运维检查，不增加重启或远程修复。记录失真、实际密码被提前改变、真实数据库状态均不能由此报告证明。

服务的原限制保持：PrivateNetwork、只允许 AF_UNIX、只可写状态目录、不可访问 Docker socket、PrivateTmp、ProtectHome、ProtectSystem=strict、NoNewPrivileges、UMask=0077、禁止 core dump，执行时限 30 秒、内存 64M、任务数 8。不要为得到一次“通过”而去掉脚本的 POSIX 所有者校验或服务沙箱。仓库测试只审载体和当前脚本的对应关系，不会调用 systemctl 或真实报告配置。

## 五个面板

`../dashboards/account-platform-overview.json` 是可审查的 Grafana JSON；没有自动导入或修改已有数据源。HTTP 两图使用 `internal/platform/httpserver/metrics.go` 发出的 service 标签与请求计数/延迟直方图；Outbox 三图使用 `cmd/event-relay/metrics.go` 发出的 source 标签与 pending、oldest age、dead-lettered gauges。原 PrometheusRules 保持独立，不因恢复面板改变阈值或告警。

实际部署前由授权操作方选择现有 Prometheus 数据源、核对抓取范围及标签，并确认五图可读取真实指标；本轮没有执行这些操作。多副本同源时各实例是独立序列，pending/age 按 source 展示，dead-lettered 图保持原 sum 视图；面板不是账务权威、业务 SLO 或零故障证明。

## Session lineage

Portal 保留始终启用的 lineage 注销保护；部署配置与安全回滚边界见[Session-lineage 版本与回滚](session-lineage-version-rollback.md)。本页的离线报告和指标面板不改变会话运行语义。

## 构建上下文保护

`account-platform/.dockerignore` 精确恢复原五条 `.git`、`deploy`、`test`、`web`、`**/*_test.go`，不引入新排除策略。当前 Go Dockerfile 的上下文仅需 go.mod/go.sum/cmd/internal/migrations，Portal 嵌入产物仍位于 internal 下，未被排除；Keycloak 采用独立 deploy/keycloak 上下文。工具/测试 Dockerfile 没有需要根上下文 deploy/test/web 的 COPY。恢复构建上下文过滤不证明曾经泄露秘密，本轮也没有构建或发送 Docker 上下文。
