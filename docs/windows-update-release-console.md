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
| `PINVOU_UPDATE_ONLINE_VERIFY_TIMEOUT_MS` | 否 | HTTPS 回读超时，默认 15 分钟 |

服务启动时要求公开目录已有有效 `latest.json`。这是有意的安全门禁：发布台不会从空对象
重建清单，以免丢失 Linux 或 macOS 节点。

## Docker Compose 部署

1. 确认 `/var/www/pinvou3/latest.json` 是当前线上清单，且运行容器的 UID 对该目录有写权限。
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
   `pinvou.com` HTTPS server block，执行 `nginx -t` 后 reload。
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
   - 把安装器移动到公开目录且拒绝覆盖不同内容；
   - 从公开目录回读并计算大小与 SHA-256；
   - 通过最终 HTTPS URL 再次下载并校验大小与 SHA-256；
   - 重新读取当前清单，仅合并 `platforms["windows-x86_64"]`；
   - 在同目录写临时清单并原子替换 `latest.json`。
5. 使用旧版 Windows 设备完成检查更新、下载、取消、UAC、覆盖安装与新版拉起验证。

## 回滚与审计

最近一次仍在线的 Windows 发布会显示“回滚此版本”。填写操作人和原因后，服务只恢复该次
发布前的 Windows 节点；当前 Linux/macOS 内容不会回退，安装器也不会删除。已安装问题版本
的设备不会自动降级，后续仍需发布版本号更高的修复包。

状态目录包含：

- `windows-draft.json`：当前唯一草稿；
- `staging/`：未发布安装器；
- `history/*.json`：发布和回滚记录；
- `audit.ndjson`：追加式操作审计，不含管理令牌；
- `publish.lock`：发布/回滚互斥锁。

应将状态目录纳入受限备份，并接入主机日志采集。备份不得公开下载。
