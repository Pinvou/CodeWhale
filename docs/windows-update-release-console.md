# Windows 更新 Web 发布台（第一阶段）

Web 发布台把原先的 SCP/rsync 和手工编辑清单收敛成一个受保护的操作界面。第一阶段覆盖：

- 管理令牌认证；
- 上传签名服务器交付的 NSIS 安装器；
- 服务端校验文件名、版本、大小及 SHA-256；
- 生成草稿并预览完整 `latest.json`；
- 先发布并回读安装器，再原子替换清单；
- 一键恢复上一 Windows 节点；
- NDJSON 审计日志和结构化发布历史。

本阶段不包含灰度分群、多角色审批、对象存储直传和签名服务集成。发布台不检查
Authenticode 状态、证书指纹或时间戳证书；签名结果继续以独立签名服务器上的沃通工具
退出码为准，发布台只比较交接单中的 SHA-256。

当前信任模型接受 HTTPS 更新源或发布权限失陷后，攻击者可同时替换安装器与清单并生成
自洽 SHA-256 的风险；这里不会以额外 Authenticode、证书指纹或时间戳校验改变既定签名
权威与发布流程。

## 安全边界

- 默认只监听 `127.0.0.1`，由现有 HTTPS Nginx 反向代理。
- API 使用至少 32 字符的 Bearer 管理令牌；令牌仅保存在浏览器当前页面内存，不写入
  Local Storage、Cookie、日志或历史记录。
- 草稿目录必须在静态公开目录之外，且权限为 `0700`。
- 历史安装器按版本命名且不可覆盖；同名不同内容会阻断发布。
- 发布锁阻止同一服务实例并发修改清单。部署只允许运行一个实例，并将公开目录挂载到
  与 Nginx 相同的本地文件系统；不要把第一阶段部署成多副本。
- `latest.json` 的临时文件与正式文件位于同一目录，Linux 本地文件系统上的 `rename(2)`
  提供原子替换。NFS/对象存储不属于第一阶段支持范围。
- 服务可以在 `UMask=0077` 下运行；私有草稿、审计和历史文件保持 `0600`，公开
  `latest.json` 和安装器在原子替换前显式设为 `0644`，不依赖进程 umask。启动时如果
  已有清单不是权限为 `0644` 的实际普通文件，服务将失败关闭而不会自动将其公开。

建议再通过 VPN、固定出口 IP 或 Nginx `allow`/`deny` 限制管理页访问来源。管理令牌应由
密码管理系统分发并定期轮换，不应提交到仓库。

## 环境变量

| 变量 | 必填 | 说明 |
|---|---:|---|
| `PINVOU_UPDATE_ADMIN_TOKEN` | 是 | 32–512 字符随机管理令牌 |
| `PINVOU_UPDATE_PUBLIC_ROOT` | 是 | Nginx 公开的更新目录，如 `/var/www/pinvou3` |
| `PINVOU_UPDATE_STATE_ROOT` | 是 | 非公开状态目录，如 `/var/lib/pinvou-update-console` |
| `PINVOU_UPDATE_PUBLIC_BASE_URL` | 是 | 安装器 HTTPS 基地址，如 `https://pinvou.com/pinvou3` |
| `PINVOU_UPDATE_HOST` | 否 | 监听地址，默认 `127.0.0.1` |
| `PINVOU_UPDATE_PORT` | 否 | 监听端口，默认 `8792` |
| `PINVOU_UPDATE_CONSOLE_BASE_PATH` | 否 | 反代路径，默认 `/` |
| `PINVOU_UPDATE_MAX_UPLOAD_BYTES` | 否 | 最大安装器字节数，默认 4 GiB |
| `PINVOU_UPDATE_UPLOAD_IDLE_TIMEOUT_MS` | 否 | 上传连续无数据进展的超时，范围 1000–120000 毫秒，默认 120000；必须低于随附 Nginx 的 150 秒 |
| `PINVOU_UPDATE_ONLINE_VERIFY_TIMEOUT_MS` | 否 | HTTPS 回读超时，默认 15 分钟 |
| `PINVOU_UPDATE_TRUSTED_PROXIES` | 否 | 可提供 `X-Real-IP` 的直接反向代理 IP/CIDR，逗号分隔；不配置时忽略该请求头 |
| `PINVOU_UPDATE_AUTH_FAILURE_LIMIT` | 否 | 同一客户端在窗口内允许的认证失败次数，默认 5 |
| `PINVOU_UPDATE_AUTH_FAILURE_WINDOW_MS` | 否 | 认证失败计数窗口，默认 5 分钟 |
| `PINVOU_UPDATE_AUTH_BLOCK_MS` | 否 | 超限后的 429 阻断时间，默认 15 分钟 |
| `PINVOU_UPDATE_AUTH_LIMITER_TTL_MS` | 否 | 闲置失败记录保留时间，默认 1 小时 |
| `PINVOU_UPDATE_AUTH_LIMITER_MAX_ENTRIES` | 否 | 内存中失败记录容量上限，默认 10000 |

服务启动时要求公开目录已有有效 `latest.json`，并逐一验证公开目录、状态目录、暂存目录与历史目录
都是真实目录且可写。这是有意的安全门禁：错误或只读挂载不会启动成健康服务，发布台也不会从空对象
重建清单，以免丢失 Linux 或 macOS 节点。公开 `/healthz` 只返回初始化阶段缓存的通用就绪状态，
不会因探活请求重复写盘、fsync 或回显内部路径。只有列入 `PINVOU_UPDATE_TRUSTED_PROXIES` 的直接代理才能提供审计日志里的
`X-Real-IP`；示例 Compose 仅信任 Docker 私有网段，Nginx 每次覆盖该请求头。
认证失败限流按上述可信代理解析后的客户端 IP 计数，达到阈值返回 `429` 和
`Retry-After`；成功认证会清除该 IP 的失败记录，TTL 与容量上限避免长期占用内存。
Nginx 的 `client_max_body_size` 设为 `4224m`，为 4 GiB 安装器之外的 multipart
字段、边界和请求头保留空间；`client_body_timeout` 是 150 秒无数据超时，应用自身默认在连续
120 秒没有收到请求数据时终止连接并清理本次独立暂存文件。只要上传持续有数据，
`server.requestTimeout`、`proxy_read_timeout` 和 `proxy_send_timeout` 仍允许最长 1 小时。
上传正文接收和文件校验不持有发布锁，只有校验完成后的草稿一致性检查与落盘进入短临界区，
因此单个半开连接不会阻塞后续上传、发布或回滚。
页面显示实时上传进度；连接中断时会先查询草稿状态，确认服务端尚未完成后自动重试一次，
连续失败则要求重新选择已经停止写入的安装器，避免构建或签名程序仍在改写文件。

## Docker Compose 部署

本地与生产默认仍从 Docker Hub 的 `node:20-bookworm-slim` 构建。
CI 为避免 Docker Hub 匿名拉取限流，使用 Google 官方
[`mirror.gcr.io` 缓存](https://cloud.google.com/artifact-registry/docs/pull-cached-dockerhub-images)，
并固定 PostgreSQL、Node、Nginx 的镜像索引摘要。CI 的 `PINVOU_UPDATE_NODE_IMAGE`
同时传给直接 Docker 构建和 Compose 构建；不设置时保留原默认来源。
更新摘要前应从 Docker Hub 与 Google 缓存分别读取索引及目标平台 manifest，
核对原始字节 SHA-256、配置和有序层摘要一致，再运行实际 Linux 容器门禁。
缓存可能清理未使用镜像，固定摘要不可拉取仍应让门禁失败；不通过跳过检查或隐式换源处理。
manifest 可读不能代替服务启动、Nginx 配置、健康检查和认证状态接口验收。

1. 确认 `/var/www/pinvou3/latest.json` 是经过复核的当前线上清单、是权限为 `0644` 的实际普通文件，
   且运行容器的 UID 对该目录有写权限。不得为了通过启动门禁而将未经复核的清单直接改为公开可读。
2. 生成令牌并写入不进 Git 的 `update-release-console/deploy/.env`：

   ```bash
   cd update-release-console/deploy
   printf 'PINVOU_UPDATE_ADMIN_TOKEN=%s\n' "$(openssl rand -base64 48)" > .env
   chmod 600 .env
   ```

3. 构建并启动单实例服务：

   ```bash
   docker compose up -d --build
   docker compose ps
   ```

4. 将 [`nginx-location.conf`](../update-release-console/deploy/nginx-location.conf) 加入
   `pinvou.com` HTTPS server block，执行 `nginx -t` 后 reload，再用
   `curl -fsS https://pinvou.com/pinvou3/latest.json >/dev/null` 验证公网清单可读。
5. 打开 `https://pinvou.com/pinvou3/release-console/`，输入管理令牌。

如果服务器不是 `pinvou.com`，先同步修改 Compose 中的 `PINVOU_UPDATE_PUBLIC_BASE_URL` 和
Nginx 路径。不要把管理服务端口绑定到公网地址。

## 发布操作

1. 在独立签名服务器完成构建与签名，取得：
   `pinvou3_<version>_x64-setup.exe`、Git commit、SHA-256。
2. 在发布页填写版本、commit、签名服务器 SHA-256、操作人和更新说明，并选择安装器。
3. 上传后核对草稿中的大小、摘要、下载 URL 和完整清单预览。此时客户端不受影响。
4. 点击“发布到客户端”并填写本次操作人。服务端将：
   - 再次校验暂存文件；
   - 解析 NSIS PE 启动外壳（Tauri 默认 I386/PE32，也兼容 AMD64/PE32+）的完整映像结构与
     VERSIONINFO（优先 ProductVersion），并匹配发布版本；stub 的 Machine 不用于推断安装 payload 架构；
   - 将暂存安装器流式复制到公开目录内随机临时文件，写盘并复验大小/SHA-256 后同目录原子切换，
     因而状态目录与公开目录可以位于不同挂载点；
   - 拒绝覆盖同名但不同内容的历史安装器；
   - 从公开目录回读并计算大小与 SHA-256；
   - 通过最终 HTTPS URL 再次下载并校验大小与 SHA-256；
   - 重新读取当前清单，仅合并 `platforms["windows-x86_64"]`；
   - 在同目录写临时清单并原子替换 `latest.json`。
5. 使用旧版 Windows 设备完成检查更新、下载、取消、UAC、覆盖安装与新版拉起验证。

## 回滚与审计

最近一次仍在线的 Windows 发布会显示“回滚此版本”。填写操作人和原因后，服务只恢复该次
发布前的 Windows 节点；当前 Linux/macOS 内容不会回退，安装器也不会删除。已安装问题版本
的设备不会自动降级，后续仍需发布版本号更高的修复包。

回滚非空的上一 Windows 节点前，服务会先按历史记录复验对应本地安装器的大小和 SHA-256，
再通过最终 HTTPS URL 回读复验；任一步失败都不会修改 `latest.json`。

状态目录包含：

- `windows-draft.json`：当前唯一草稿；
- `staging/`：未发布安装器；
- `history/*.json`：发布和回滚记录；
- `audit.ndjson`：追加式操作审计，不含管理令牌；
- `publish.lock`：发布/回滚互斥锁。

应将状态目录纳入受限备份，并接入主机日志采集。备份不得公开下载。
