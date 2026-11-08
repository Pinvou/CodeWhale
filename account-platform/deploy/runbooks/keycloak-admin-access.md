# Keycloak 受控管理访问

> 来源：原完整候选 `828242031c42911099792ec78c1f746ed8620c49` 的成熟手册。以下保留原安全合同及历史证据；恢复文档不等于当前发布、部署或外部审批通过。当前任务不执行生产命令。

## 当前入口与历史边界

以下均为授权 operator 在受控网络上的手动操作，不是本轮已部署证据。命令从仓库根目录执行；真实配置需保持管理面无公网入口，证书/网络/RBAC/管理员身份由站点独立核验。本轮只恢复原手册，不建立隧道或访问管理面。

所需 Secret/受控身份仅由已有安全设施提供，不能写进 argv、仓库、工单或日志。当前缺载体、签署、可信来源或真实执行记录时保持未验收；不能以历史 DONE/PASS、JSON 格式或自造收据代替。恢复工件绑定与现场独立审批的区别见 [运维证据](operational-evidence.md)。

## 保留的原手册

Keycloak 公网入口只承载 `/realms/` 和 `/resources/`。管理控制台 `/admin/` 不经过公网 Ingress 或 Nginx 代理，管理员必须先建立受控网络通道，再从本机访问 `http://127.0.0.1:18081/admin/`。

## Compose 远程部署

确认远端 Keycloak 只绑定 `127.0.0.1:18080`，然后在管理员本机建立 SSH 隧道：

```sh
ssh -N -T -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -L 18081:127.0.0.1:18080 <ssh-user>@<server>
```

Windows 可从仓库根目录运行 `account-platform/deploy/keycloak/open-admin-tunnel.ps1`，显式提供其 `-Server`、`-User` 参数。隧道保持运行期间，在浏览器打开 `http://127.0.0.1:18081/admin/`；操作结束后按 Ctrl+C 关闭隧道。

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
