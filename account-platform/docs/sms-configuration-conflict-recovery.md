# 短信配置冲突恢复

## 阿里云模板用途与兼容迁移

`aliyun-managed` 的四种用途分别是注册、登录、更换手机号和重置密码。`PROFILE_MOBILE` 指用户更换手机号的验证，不是修改所有资料时都发送短信；`PASSWORD_RESET` 不改变登录或注册用途。模板选择不会替代挑战用途、主体、来源及验证码的既有检查。

| 用途 / 配置 JSON 键 | Account 启动环境变量 | Helm 既有 runtime Secret 中的键 | 部署默认值 |
| --- | --- | --- | --- |
| `REGISTER` | `ALIYUN_SMS_REGISTER_TEMPLATE` | `aliyun-sms-register-template` | 必须显式提供 |
| `LOGIN` | `ALIYUN_SMS_LOGIN_TEMPLATE` | `aliyun-sms-login-template` | 必须显式提供 |
| `PROFILE_MOBILE` | `ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE` | `aliyun-sms-profile-mobile-template` | 缺少、空串或仅空白时沿用 LOGIN |
| `PASSWORD_RESET` | `ALIYUN_SMS_PASSWORD_RESET_TEMPLATE` | `aliyun-sms-password-reset-template` | 缺少、空串或仅空白时沿用 LOGIN |

以上回退是已有部署兼容行为：启动装配与 Aliyun adapter 均保留它，不会因为升级而要求旧站点立即补齐两个独立模板。独立设置任一可选模板只改变对应用途；注册、登录继续使用各自模板。签名和模板仍应由站点与供应商核对用途与实际内容；当前发送使用 `code` 和 `min` 参数，不能把本地测试当作供应商模板验收。

配置入口需区分：

- 基础 `deploy/compose/compose.yaml` 将上述四个宿主变量传给 Account；两个新增可选变量默认空串。生产 `deploy/production/compose.personal.yaml` 从 `ACCOUNT_SERVICE_PRIVATE_ENV_FILE` 的私密文件读取，主机上单独导出变量不会自动进入该容器。
- Helm 使用 `global.runtimeSecret` 指向的既有 Secret；两个用途覆盖键始终为可选引用，缺少键的旧 Secret 仍可使用 login 回退。启用 ExternalSecret 的站点通过既有远端配置对象同步对应键；更新已验证的发布输入后按原发布流程重启 Account，环境变量不会因 Secret 修改而自动热更新。不得为检查配置而输出整个 Secret、私密文件、Pod 环境或带凭据的 Compose 渲染。
- 管理配置的 `GET/PUT /admin/v1/sms-configuration` 分别受 `platform.sms-config.read` / `platform.sms-config.manage` 保护。Aliyun 管理写入要求 `credential_profile=aliyun-default`、签名及四个非空 `templates` 值。这里**没有空白字段回退**：希望复用 login 时，明确将对应用途填写为同一个模板。HTTP-generated provider 的配置不使用 Aliyun 签名和模板。

迁移步骤：

1. 先通过已有管理读取查看 `source`、`revision`、`configuration_fingerprint` 及四种有效模板，确认当前 provider/profile 可用。`source=DEPLOYMENT_DEFAULT`、`revision=0` 使用启动默认；存在数据库管理版本后，修改环境变量不会覆盖当前活动数据库版本。保持原签名、凭据及模板有效，不将读取失败当作空配置。
2. 为拟分离的用途确认可用模板与参数；仅需要分离一个用途时，另一个用途继续显式或通过部署默认复用 login，不顺带修改注册、登录或 SMS provider。
3. 部署默认站点通过相应 Compose 私密文件或 Helm Secret 输入设置可选覆盖，在授权发布窗口受控重启 Account 并重新读取有效配置。已有数据库版本的站点使用管理界面或既有管理 API，以读取到的 `expected_revision`、新幂等键和理由保存完整四用途配置；后续冲突、未知结果按下文规则处置。不要直接修改配置历史表或只改环境后宣称数据库版本已更新。
4. 在获授权的验收环境分别验注册、登录、更换手机号、密码重置的实际模板与验证结果；保存修订号/指纹、用途和脱敏结果，不保存验证码或凭据。旧挑战按原 provider descriptor 解析；数据库修订保留原模板配置，但凭据 profile 仍来自当前部署，环境默认 `v1` 也不等同于不可变历史快照，不能据此贸然撤销旧模板或凭据。
5. 恢复时，部署默认使用原受控输入并重启；数据库版本通过当前修订号发布恢复旧模板取值的**新版本**，不倒改修订或历史。若先前保存结果未知，先按同一冻结命令/幂等键恢复结果；确认前不得另起恢复写命令。

## 管理保存冲突与未知结果

短信配置使用修订号 CAS 和幂等键保存。只有 HTTP 409 且错误码为 `SMS_CONFIGURATION_CONFLICT` 才表示本次明确没有写入。此后保存暂停；管理员刷新成功、读取比旧命令更新的修订号后，再次填写理由并明确点击保存，才能建立绑定新修订号的新命令与幂等键。刷新本身不执行保存。读取失败或仍返回原修订号时，保留冲突提示及刷新入口。

5xx、网络异常、取消或无效确认回执都可能丢失已完成操作的结果，继续重试原冻结命令与同一幂等键。刷新、语言切换或编辑当前表单都不能把这个未知结果自动改成另一条写命令。服务端先检查成功命令的幂等回放，再检查修订冲突；确认成功后才释放旧意图。

短信读取各自拥有取消控制和代次。客户端/会话、上下文或平台授权版本、读写权限变化会退役旧读取及旧意图；迟到响应不能改变新会话配置或把旧冲突附到新意图。服务端权限、近期认证、配置验证和回执检查继续执行。

回归场景由既有 `npm run test:account-platform-personal-ui` 入口执行，使用实际编译的 SettingsWorkspace、AdminPlatformClient 和受控 HTTP 响应，覆盖三语、明确冲突刷新后再次确认、失败刷新、未知结果相同事实/键，以及会话和授权退役。最终 Portal 静态资源应在合成源码上通过 `npm run build:account-portal` 统一生成并审核。
