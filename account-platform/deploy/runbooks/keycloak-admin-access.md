# Keycloak 受控管理访问

Keycloak 公网入口只承载 `/realms/` 和 `/resources/`。管理控制台 `/admin/` 不经过公网 Ingress 或 Nginx 代理，管理员必须先建立受控网络通道，再从本机访问 `http://127.0.0.1:18081/admin/`。

## Compose 远程部署

确认远端 Keycloak 只绑定 `127.0.0.1:18080`，然后在管理员本机建立 SSH 隧道：

```sh
ssh -N -T -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -L 18081:127.0.0.1:18080 <ssh-user>@<server>
```

Windows 可运行 `deploy/keycloak/open-admin-tunnel.ps1`。隧道保持运行期间，在浏览器打开 `http://127.0.0.1:18081/admin/`；操作结束后按 Ctrl+C 关闭隧道。

## Kubernetes 部署

从能够访问集群 API 的受控终端执行：

```sh
kubectl -n <namespace> port-forward \
  service/<release>-account-platform-keycloak 18081:8080 \
  --address 127.0.0.1
```

如果 Helm release 名称不同，先用 `kubectl -n <namespace> get service` 确认 Keycloak Service 名称。也可以通过受访问控制的 VPN 连接集群内网，但不得为管理控制台新增公网 Ingress 路径。

## 安全检查

1. 公网 Keycloak 主机只配置 `/realms` 和 `/resources` 两条 Ingress 路径，且 Nginx 不存在 `/admin/` 代理。
2. `KC_HOSTNAME_ADMIN` 指向管理员本机隧道地址或仅限受控网络访问的内部地址。
3. 管理员密码只在 Keycloak 登录页输入，不写入命令、脚本、工单或日志。
4. 完成操作后关闭 SSH、VPN 或端口转发，并记录操作者、变更内容和关联工单。

若公网 `/admin/` 返回 Keycloak 管理控制台，应立即视为配置回归，停止外网入口并检查 Ingress、反向代理和 DNS。
