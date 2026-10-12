# Portal 可信边缘与 Account 风险来源

`PORTAL_TRUSTED_PROXY_IPS` 默认为空，Portal 只使用 TCP 直接 peer 的 IP。需要经过受控边缘时，部署者显式配置逗号分隔的准确 IPv4/IPv6 地址；不接受域名、端口、CIDR、通配符、空列表项或 IPv6 zone。非法配置在进程启动配置解析时拒绝，不能降级成信任所有代理。

只有直接 peer 精确匹配该集合时，Portal 才读取一个合法单 IP 的 `X-Pinvou-Client-Source`。缺失、重复、逗号列表或非法值继续使用直接 peer。IPv4-mapped IPv6 与对应 IPv4 地址规范化为同一个 IP 身份。该来源只在现有 Account SMS/注册/密码及移动号码资料操作的风险请求上生成，不扩展其他路由或改变限流策略。

受信边缘必须强覆盖该头为它实际观察到的客户端 IP，不能追加或沿用浏览器值。网络和监听器必须保证其他调用方不能冒充名单中的 peer；边缘配置与隔离由部署接线验证。Portal 不读取 `X-Forwarded-For` 或 `X-Real-IP`，也不向 Account 转发浏览器 Authorization、Cookie 或内部凭据。仅把任意私网 peer 当可信不满足此约束。

名单在 Service 构造时复制为私有固定集合，变更需要按进程配置重新构造，不由请求头更新。真实 loopback 测试覆盖生产 Portal Handler/forward 与受控 Account HTTP 消费者，包括可信/非可信、来源伪造、多值、IPv6、非法配置与非风险路由；这不证明实际 NGINX 配置、网络隔离、Redis、OIDC、生产 Account 风险引擎或完整 E2E 已通过。
