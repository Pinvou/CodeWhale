# Breakglass 临时凭据与会话收尾

本手册补齐应急恢复后的人工收尾流程。执行者使用站点已有、当前有效的独立运维权限；文档不是生产处置授权，不安装自动停用任务，不删除用户、业务数据或安全证据。

Account 的临时角色按数据库时间即时失效，和 Keycloak 密码、SSO/离线会话、桌面及 Portal 缓存分别处理。角色到期不能代替凭据和会话回收；`break_glass_operations.expires_at` 也不能代替实际角色绑定的到期判断。

## 1. 先建立唯一对象与原始基线

在受控事件记录中保存以下**非秘密引用**，凭据正文仍在原保管系统：

- 站点/环境、同源版本或镜像摘要、Account 数据库受控标识、用户 issuer、Keycloak 用户 realm；运维认证 realm 与用户 realm 分开记录，不能用 workload realm 代替用户 realm。
- 原 `incident_ticket`、签名操作凭据的 `credential_id`、目标 Account `user_id`、对应 Keycloak `subject`。Account、Keycloak 和当前事件必须相互核对；用户名只是辅助显示，不用姓名、前缀、相似名字或列表位置选对象。
- 定位后的 `break_glass_operations.id`、`role_assignment_id`、数据库时间、操作及角色各自的到期时间、原不可变审计/告警引用。不得导出 reason、凭据、密码、Token、Cookie 或原始证据正文到公开日志。
- 事件前正常用户的 enabled 状态、常驻角色绑定及其来源；事件所用客户端、在线/离线 session ID、Portal 浏览器/lineage 和本机凭据保管条目的私密引用。缺失的基线标记为未知，不补造时间或归属。

按下面规则分类，**不能仅因 `pinvou_bootstrap_managed=true` 就认定可以停用**：

| 身份类别 | 必须有的正面证据 | 收尾边界 |
| --- | --- | --- |
| 已确认应急专用身份 | 既有受控清单明确其专用用途，且核实没有正常用户/常驻业务用途；精确 issuer + subject 与本次记录一致 | 由获授权人员手工回收该身份的本次凭据、会话，并确认停用；不能批量按名称处理 |
| 既有正常或混用身份 | 原本承担正常职责，或恢复时复用了既有身份/角色 | 保留该用户及正常权限；由账户所有者按第 3 节确认改密的全用户会话注销影响后恢复长期密码；其余单项会话/授权回收只处理已证明属于事件的对象 |
| 归属无法证明 | 仅有命名、创建时间、managed 标志或不完整截图 | 按正常用户保护；保留事件为待处置，不自动 disable、不删除、不用全站注销兜底 |

`account-security break-glass` 只恢复已存在的目标，不负责创建新的临时用户名。当前实现可能复用已有有效的 `PLATFORM_SUPER_ADMIN` 绑定；若该绑定原本永久有效，不能把它当成此次新建临时绑定撤销。实际 `FormalPassword` 写入 Keycloak 时为 `temporary=false`，不会随 Account 角色自然到期。

## 2. 只读确认 Account 事实

用站点已批准的只读数据库连接配置，在受控终端/数据库客户端传入下列 psql 变量：`operator_credential_id`、`incident_ticket`、`issuer`、`subject`。变量来自原事件和已核身份，不把密码或 DSN 放进命令行。查询需要相应表的只读权限；拒绝、超时、空结果或多结果均不是“已收尾”。

```sql
BEGIN READ ONLY;
SET LOCAL statement_timeout = '5s';
SELECT o.id AS operation_id, o.target_user_id, o.role_assignment_id,
       o.created_at, o.expires_at AS operation_expires_at,
       now() AS observed_at, now() >= o.expires_at AS operation_window_ended,
       i.issuer, i.subject,
       ra.user_id = o.target_user_id AS assignment_belongs_to_target,
       ra.role_code, ra.organization_id, ra.unit_id,
       ra.source, ra.grant_source, ra.starts_at,
       ra.expires_at AS assignment_expires_at, ra.revoked_at,
       (ra.revoked_at IS NULL AND ra.starts_at <= now()
        AND (ra.expires_at IS NULL OR ra.expires_at > now())) AS assignment_effective_now,
       EXISTS (
         SELECT 1 FROM role_assignments other
         WHERE other.user_id = o.target_user_id
           AND other.role_code = 'PLATFORM_SUPER_ADMIN'
           AND other.organization_id IS NULL AND other.id <> o.role_assignment_id
           AND other.revoked_at IS NULL AND other.starts_at <= now()
           AND (other.expires_at IS NULL OR other.expires_at > now())
       ) AS another_effective_super_admin_binding
FROM break_glass_operations o
JOIN identities i ON i.user_id = o.target_user_id
                 AND i.issuer = :'issuer' AND i.subject = :'subject'
LEFT JOIN role_assignments ra ON ra.id = o.role_assignment_id
WHERE o.operator_credential_id = :'operator_credential_id'
  AND o.incident_ticket = :'incident_ticket';
COMMIT;
```

要求唯一命中并核对原审计 `security.break-glass.activated`、告警 `BREAK_GLASS_ACTIVATED` 与相同 operation ID。查询空结果不能证明之前没有 Keycloak 副作用；特别是 CLI 失败或提交回复未知时，不因“未查到”就重跑 break-glass、另签一份操作凭据或改另一个用户。

只把本次确认为 `source/grant_source=BREAK_GLASS`、用户吻合、`role_code=PLATFORM_SUPER_ADMIN`、organization/unit 均为空且具有明确到期时间的绑定，按其真实生效条件核验退出。仍有效时等待原到期，或由已有恢复授权流程处理；不直接 UPDATE/DELETE 角色或不可变操作表。复用的正常绑定及其他合法绑定分别保留，不能要求该正常用户的全部权限变成零。

## 3. 回收凭据与 Keycloak 会话

先确认此刻执行者的独立运维授权仍有效；应急用户自身角色已过期，不代表它继续有管理权限。使用与部署版本匹配的 Keycloak Admin Console 或镜像内 `/opt/keycloak/bin/kcadm.sh`；当前同源镜像为 Keycloak 26.7.0。下列命令仅为**人工选择后单项执行的示例**，本手册不提供循环停用脚本。

`KCADM` 为该镜像的 CLI 路径；`KC_ADMIN_CONFIG` 指向已认证的私有配置，按文件权限/Windows ACL 限制本人的访问。认证不使用 `--password` 参数，不开 shell trace，不输出配置文件。CLI 操作同一配置须串行；realm、subject、客户端内部 UUID 和 session ID 均先与原事件核对。命令输出只留在受控终端，回执记录所需最小字段。

```bash
# 只读：每个变量先取自已验证的站点与事件记录。
set -euo pipefail
: "${KCADM:?}" "${TARGET_SUBJECT:?}" "${USER_REALM:?}" "${KC_ADMIN_CONFIG:?}" "${CLIENT_UUID:?}"
"$KCADM" get "users/$TARGET_SUBJECT" -r "$USER_REALM" --config "$KC_ADMIN_CONFIG" --fields id,enabled
"$KCADM" get "users/$TARGET_SUBJECT/credentials" -r "$USER_REALM" --config "$KC_ADMIN_CONFIG" --fields id,type,createdDate
"$KCADM" get "users/$TARGET_SUBJECT/sessions" -r "$USER_REALM" --config "$KC_ADMIN_CONFIG" --fields id,userId,clients
"$KCADM" get "users/$TARGET_SUBJECT/offline-sessions/$CLIENT_UUID" -r "$USER_REALM" --config "$KC_ADMIN_CONFIG" --fields id,userId,clients
```

离线查询中的 `CLIENT_UUID` 是该站点已确认的**内部客户端 UUID**，不是显示用 clientId。按事件客户端清单逐个核查，不能把一个客户端空列表外推为全部清理。若此版本提供分页或返回截断，须完整读取并核对，未读完不得填“零”。

1. **签名操作凭据**：核对对应唯一已消费记录或原有效期，撤回原保管系统中的临时分发/挂载引用。只记录凭据 ID、有效期及撤回回执，不回显签名包，不轮换全站签名公钥作为单事件收尾。消费唯一性和最长 15 分钟时效不能证明正式密码已撤回。
2. **正式密码**：专用身份通过受控 Admin Console 设置新的隔离密码并撤回本次密码分发，随后手工停用；正常/混用身份由所有者经既有 Account 安全改密/恢复流程建立其长期密码。**该正常安全流程会调用 Keycloak 的全用户会话注销，并删除该用户全部 Account `user_sessions`，也影响非应急的正常会话；它不是按事件精确清理。** 执行前须由账户所有者按既有授权确认并协调这一完整影响；授权或协调不足时保持待处置，不以手工绕过注销步骤、禁用正常用户或反复重置替代。完成后由所有者重新登录确认正常访问。不得恢复已暴露旧密码，不把新密码写入手册、事件附件或 shell 参数。由受控客户端按站点风险限制做有限验证：正常身份的新长期凭据可用，旧应急密码被拒绝；credential ID/createdDate 变化不是密码已替换的证明。无法确认密码效果时保留待核实，不反复尝试登录、不再次盲目重置或禁用正常用户。
3. **在线会话**：以下单项删除仅处理已证明属于事件的会话，与前项改密的全用户注销分开记录。从目标用户当前会话清单核实精确 ID、用户归属和本次用途，再删除该单个会话；之后重新查询。SSO session 可能覆盖多个客户端；若当前 clients 已包含正常用途或复用会话，须有覆盖全部受影响用途的既有授权，否则保持待处置。不能把 `/users/{id}/logout` 的全用户效果误写成只注销一次应急登录，也不能按旧事件清单删除所有者改密后新建的正常会话。
4. **离线会话/令牌**：独立盘点。退出在线 SSO 不能作为离线令牌已失效的证明。对证实属于事件的离线 session 使用明确的 offline 删除参数并复查；若与正常用途复用、无法分离，由所有者授权该用户/客户端的既有离线授权恢复流程，否则保持待处置。

以下三段分别是独立单项操作，不要整页批量执行。每次写操作前，都重新核当前对象归属及已有处置授权。

```bash
# 单项：已证实归属和用途的在线 session。
set -euo pipefail
: "${KCADM:?}" "${USER_REALM:?}" "${KC_ADMIN_CONFIG:?}" "${INCIDENT_ONLINE_SESSION:?}"
"$KCADM" delete "sessions/$INCIDENT_ONLINE_SESSION" -r "$USER_REALM" --config "$KC_ADMIN_CONFIG"
```

```bash
# 单项：已证实归属和用途的离线 session，明确 offline 参数。
set -euo pipefail
: "${KCADM:?}" "${USER_REALM:?}" "${KC_ADMIN_CONFIG:?}" "${INCIDENT_OFFLINE_SESSION:?}"
"$KCADM" delete "sessions/$INCIDENT_OFFLINE_SESSION" -r "$USER_REALM" --config "$KC_ADMIN_CONFIG" -q isOffline=true
```

只有正面证明为专用、可回收身份时，授权人员才可单项手工停用。`BG_IDENTITY_CLASS=INCIDENT_ONLY` 和 `CONFIRMED_RETIRE_SUBJECT` 是对当前受控记录的人工确认，不是对象归属证明；禁止按名称、到期时间或 managed 标志自动填入。正常/混用/未知身份不使用此段。

```bash
# 单项：仅人工核实的专用身份；默认 UNKNOWN/确认对象不一致均不调用写接口。
set -euo pipefail
: "${KCADM:?}" "${TARGET_SUBJECT:?}" "${USER_REALM:?}" "${KC_ADMIN_CONFIG:?}"
if [[ "${BG_IDENTITY_CLASS:-UNKNOWN}" != "INCIDENT_ONLY" || "${CONFIRMED_RETIRE_SUBJECT:-}" != "$TARGET_SUBJECT" ]]; then
  printf '%s\n' '未确认事件专用身份或对象不一致，停止停用。' >&2
  exit 2
fi
"$KCADM" update "users/$TARGET_SUBJECT" -r "$USER_REALM" --config "$KC_ADMIN_CONFIG" -s enabled=false
```

删除某用户对某客户端的 consent 会同时影响该用户该客户端的其他离线授权，不能代替上述精确会话删除自动执行；它需要相应范围的既有授权。停用专用身份后重新 GET 该精确用户确认 `enabled=false`；正常用户则确认 enabled 状态和原正常绑定未被收尾改变。不能只凭写请求 204、超时或一个 404 就认定完成；404 先排除错站点/realm/用户，再用原归属清单及权威后态确认。

## 4. 收尾所有实际消费者

| 层 | 处置与复验 | 不能据此推断 |
| --- | --- | --- |
| Account | 核本次临时绑定按数据库生效条件已退出；在受控原客户端验证相应管理员操作被拒绝，保留正常身份原权利 | `user_sessions` 为空或角色到期不等于其他系统的缓存都清除 |
| Keycloak | 对原事件清单的在线/离线会话分别复查；专用身份核 enabled=false；正常身份记录已授权改密的全用户注销影响并确认重新登录可用，其余单项回收只处理已授权对象 | 在线 logout 不保证已签发 JWT 在所有离线校验方立刻失效，也不证明 Portal/桌面缓存全部清除 |
| 桌面 | 在每个受控应急设备正常退出账户，按原保管流程撤回该设备的临时凭据；验证原会话无法继续 refresh。不得清空正常用户整个凭据库 | 单个设备退出不覆盖未登记设备或已复制的令牌 |
| Portal | 每个应急浏览器走原带 CSRF 的注销入口，确认该 lineage 退出、原刷新/迟到回调不能恢复它；保留 [session-lineage 规则](session-lineage-version-rollback.md) | 不能因 Keycloak logout 就删除猜测的 Redis session/refresh/flow/handoff key，也不能全库 SCAN/FLUSH 清理 |
| 途中与缓存 | 等待本次已签发 access token、已接受 flow/handoff、相关客户端缓存的**部署实际最长有效上界**及已核时钟偏差窗口，再复验；用受控配置/到期元数据确定，不导出令牌正文 | 未知有效期、客户端清单缺失或无法定位 lineage 时不得猜“默认几分钟已足够” |
| 额外授权/运维临时配置 | 逐项核事件中实际创建/分发的额外 API 凭据、授权及临时 Admin CLI 配置；只撤回证明属于本事件的对象，依原工具和保管流程操作，全部后验和记录完成后，按其实际认证方式撤销本事件专用 CLI 会话/等待已证令牌有效上界，再清理本事件私有临时配置 | 不以本手册授权删除常驻服务密钥、正常用户授权、审计或备份 |

原 CLI 激活时删除 Account `user_sessions` 并调用 Keycloak 会话注销，是**激活步骤**，不是此次结束时再次清理所有消费者的回执。Portal 没有在本手册新增全用户会话扫描 API；无法用现有受控浏览器完整收尾时，保留待处置并补齐最长有效期/后验，禁止读取或复制缓存中的 access/refresh Token 来凑证据。

## 5. 部分成功与未知结果继续处理

每项分别追加“计划对象/已发出/已确认/拒绝/结果未知”、操作者、时间、非秘密回执和权威后态。客户端断连、CLI 非零或服务重启不证明没有效果；诊断或回执写入失败也不能改成完成。

| 情况 | 下一步 |
| --- | --- |
| 权限拒绝、依赖不可用、只读查询失败 | 保留原对象与记录，恢复既有受控权限/依赖后重新核对；不扩大角色、不换到另一个疑似用户 |
| 密码更新、disable 或会话删除回复丢失 | 先查询同站点、同 realm、同 subject/会话的后态；未知前不以新用户、新 session ID 或新操作凭据替代原对象 |
| 原会话已不在列表，用户有新会话 | 比对原事件清单和新会话归属；不能因同一用户名就删除后续正常会话 |
| 原专用身份被转为其他用途，或出现新的合法角色/授权 | 暂停身份级回收，重新按当前归属和授权核定；不按旧快照 disable |
| 跨站点、记录相矛盾、CLI 提交结果未知或查无操作记录 | 保留原证据并人工核实，不用“未查到”结案，不重放 break-glass 来探测 |

任何重试都先重新核对即时权限和当前对象归属，只重试已有依据的精确动作；不自动重新授权、延长角色、重签凭据、清空会话仓库或撤销全 realm。原 `break_glass_operations`、安全告警和审计保持不可变；补充手工回执放在关联的受控事件记录，不修改历史表来标“已关闭”。

## 6. 可验收的结束条件

仅当下列证据均成立，才能在关联事件中确认本次临时访问收尾：

1. 精确事件、issuer/subject 与角色来源/有效期已核；本次临时授权不再生效，正常身份原角色按既有权限保留。
2. 签名凭据的消费/到期及分发撤回有据；本次密码分发已撤回，专用身份受控隔离/停用，或正常身份已由所有者完成长期凭据恢复。
3. 每个已登记客户端的在线、离线、桌面和 Portal 处置及相应最长有效期后验完整；原会话不能继续应急操作/恢复临时访问，正常用户未被误停用。
4. 所有已发出动作都有后态；未知、冲突、未覆盖客户端及回执失败均已明确处理；不可变原证据与最小必要回执已关联保留。

任何一项证据不足保持待处置，不能仅凭到期、空列表、204、CLI 成功文字或单实例测试填完成。本仓库文档修复不等于已有事件完成，也不等于现场授权或演练通过。

## 版本与核对依据

本流程按 `cmd/account-security/main.go` 的真实参数、`internal/securityadmin/security.go` 的角色复用/不可变回执、`break_glass_identity.go` 的签名凭据时效、`internal/account/role_bindings.go` 的即时有效性、`internal/account/profile_changes.go` 的改密全用户会话注销以及 `internal/identityadapter/keycloak.go` 的正式密码/会话调用核对；不增加自动处置或更改用户授权。

Keycloak 命令和作用域见 [26.7.0 Admin REST API](https://www.keycloak.org/docs-api/26.7.0/rest-api/index.html)；私有 CLI 配置和离线访问边界见 [26.7.0 管理手册](https://www.keycloak.org/docs/26.7.0/server_admin/index.html)。其他部署版本先核对应版本契约；不要把本手册当作可跳过版本、TLS、最小权限和真实现场验收的证明。
