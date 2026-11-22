# 当前模型与凭据类型

`get_effective_model_config` 返回当前会话的完整模型引用，未绑定会话时使用全局默认引用。平台目录模型不能只通过内置承载模型 ID 反查，否则会丢失会话所选的平台目录模型。已删除的显式模型引用返回 `model_selection_unavailable`，不以全局模型冒充原选择。

`credential_mode` 与实际运行时使用同一个判定函数：

- `backend_managed`：平台模型的账户托管凭据，由当前账户在每次运行准备时授权，无需用户填写 API Key。
- `user_managed`：用户自己的云端模型，需要其独立的 API Key；账户登录不会修复、替换或清除这个 Key。
- `none`：运行时允许无鉴权的端点。

`requires_user_api_key` 仅在 `user_managed` 时为真。平台承载记录的 `credential_state=missing` 表示它没有本地 API Key，不表示账户登录失效。界面必须结合 `credential_mode` 判断。平台模型不报告普通用户模型的进程环境覆盖，也不暴露内部承载地址。

此查询始终返回空 `api_key`，不读取或传递账户访问令牌。令牌有效性及平台权限继续由现有运行时认证边界校验。
