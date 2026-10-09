# 自助注销与隐私清理（F08 私人装配候选）

此私有候选以已审 F07 完整 4155 文件组合为父，保留 F06 当前权限、成员投影和统一生成产物，以及 F01 6a 钱包提交前租约保护与 F07 永久赠额元组。实际分支尚未完成相邻承接，也没有部署批准。个人注销保留明确确认与近期重新认证；管理员只能读取阻塞队列和重试当前阶段，无直接 DELETE 用户、强制跳过阻塞或恢复匿名身份。

个人入口为 Account 页面中的共享三语言注销区域（Portal 与桌面同一业务组件）。GET /v1/me/account-closure-request 查询当前进度；POST /v1/me/account-closure-requests 使用确认 CLOSE_MY_ACCOUNT 与同一次逻辑命令的 Idempotency-Key。读取失败不能当没有请求；未知回包保留原输入和原键重放权威入口。重新认证后 return_to 的 account_closure=confirm 仅在成功查询后恢复确认流程。管理员使用 GET /admin/v1/account-closure-requests 和 POST /admin/v1/account-closure-requests/{id}/actions/retry，要求对应 read/repair 权限，重试保留原理由和原键。

账户生产启动同时配置 F07 注册资格解析器、现有钱包 worker 与有界注销 worker。Account→Billing assessment/isolate、Account→Token assessment/replay.delete 和 Account→Identity user.delete 只给已有 Account 工作负载身份。每次外呼从挂载的动态 token 文件读取当前凭据；不把 token 放入配置/日志。原 workload issuer/JWKS/CA/audience、固定接收规则、UID 私有只读挂载和服务网络隔离保持父版本。新增 compose.closure.yaml 仅覆盖五个同源镜像并向 Account 提供 Token HTTP URL。按当前父配置，服务网络内部使用 http://token-gateway:8080 等原有监听端口；公开入口 TLS、Keycloak HTTPS 和 SSL_CERT_FILE 根证书信任保持原 compose，不新增 TLS 代理或监听器。站点如另有已验证 HTTPS 服务 URL，原客户端使用部署信任根。

必须依次承接本波 Account/Token forward-only guard 的公开 Ordered/Up 和固定 hash、权限种子及 ACL，保持原 027/028/014 SQL 不变。migrate 的最高 forward-only Down 必须拒绝回退含永久清理事实的状态。同源 Dockerfile 与实际 cmd/healthcheck、Portal 项目构建产物一起交付；不得用旧镜像接新版 UI。

PROVISIONING 或赠额/钱包 ACK 未闭合时不允许开始注销。钱包完成事务先激活用户并清除临时指纹元组；永久手机号资格、claim 和 receipt 不随注销删除。注销每个阶段按 current claim/lease 身份处理：准备隔离→外呼取得精确元组 ACK/Token exact empty204→持久 ACK→匿名化。Token 清理先取得排他 user fence，晚 payload writer 共享 fence 后重查关闭 marker。匿名化后产生原 outbox；完成状态还要求原 published_at 权威事实，不能在未验证 NATS 发布的测试中假称 COMPLETED。

本轮公开迁移与非超级用户运行时实际验证包括：C02 worker 两个业务叶加一个 Go 父用例（child 共三 PASS），以及 signup/Gift 五个业务叶加一个 Go 父用例（child 共六 PASS）；两组外层协调用例另列，不累计为业务叶，回收 owner 八叶与父用例，Commerce 两个 Claim 锁等待叶、四个并发注销/晚回包叶及父用例。各组独立记录输入与计数，不把不同执行的输入合并冒称整模块重跑。回收的 Token 路由验证使用受控 Account 权限和 Billing 响应；owner 账务测试使用真实付款合并、额度分配、隔离和回收事务，但人类委托上下文受控，Account 匿名化状态精确 HTTP 验证另有有界响应/凭据轮转测试。匿名化后的旧 Commerce 回包、重试和重新 Claim 不得恢复映射或改变已清理事实。

Identity authority、Redis 与浏览器 HTTP 为受控夹具；未执行真实 issuer、Keycloak deletion、NATS/outbox 发布、整 Tauri/OS credential store、网络/TLS 部署或完整 E2E。前序不变的测试按精确组件输入继承，当前前端、Native 谓词及首次 canonical 结果由本轮冻结证据单独绑定；生成产物或仅编译成功不代替实际业务验证。

原 O 的 org.grants.reclaim_closed 在本波完整恢复：组织当前授权→Token 单笔/独立逐项批量→带人类委托的 Billing→Account 实际匿名化状态与生命周期版本核验→整笔可用剩额回收、永久回执、审计及 outbox。未匿名化、过期、冻结或有预占时拒绝。理由为合法 UTF-8 的 1..500 字符，公开幂等键为去除首尾空白后合法 UTF-8 的 16..128 字符；Native 自有生成键仍使用既有 ASCII 路径。重复批量标识在任何 owner 调用前拒绝；格式合法但不存在或不合格的标识保留逐项 207 与已成功效果，不变成整批原子操作。Portal 与 Native 使用同一精确路由，三语组织页保留原单笔卡片操作；批量能力由既有 API/客户端提供。未知回包必须以原输入和原键向权威写入口恢复，刷新读到已回收状态不能视为本次写入的回执。
