# 模型与 Token 固定域名 DNS 清单

2026-09-06 负责人批准的新域名；本清单是待实施输入，不是 DNS、TLS 或部署通过证明。实际本次目标为 PRODUCTION，CLOUD_UAT 保留独立预留配对。

| 用途 | 完整名称 | DNS 类型 | 主机记录（pinvou.com 区） | 目标 |
| --- | --- | --- | --- | --- |
| 正式隔离凭据入口 | credentials.pinvou.com | CNAME | credentials | account.pinvou.com |
| UAT Portal | account-uat.pinvou.com | CNAME | account-uat | account.pinvou.com |
| UAT 隔离凭据入口 | credentials-uat.pinvou.com | CNAME | credentials-uat | account.pinvou.com |

既有 `account.pinvou.com` 记录保持不变。实施者须先确认三名称没有冲突 A/AAAA/CNAME 或其他不兼容记录；不能覆盖未核实记录。三条别名沿用既有公共代理入口，但 DNS 同目标不等于同 origin 或相同 upstream：HTTPS 入口必须按精确 SNI/Host 区分三个新名称，Portal 与 credential 分别路由到批准的服务，不回落默认站点，不增加跨环境 Cookie/OIDC/CORS 信任。

需要覆盖各新域的有效证书链、独立 Host 路由、固定 OIDC 回调和安全代理日志策略，并在后续 APT-137 部署验收中逐项验证。旧 `credentials.account.pinvou.com` / `credentials-uat.account.pinvou.com` 从未上线，不新增它们的记录、证书或重定向，也不纳入新白名单。未取得真实 DNS/TLS/代理和身份等证据时保持 ENV_PENDING/NOT_DEPLOYED；不得把自签 fixture 或 CNAME 已填写当部署完成。
