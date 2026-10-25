# 个人与组织充值自动查询恢复边界

原 O/F 自动查询能力恢复时仍使用内联二维码，该历史窄修没有完成 PAY-08/UI-10 模态要求。当前实现将只读历史、个人待支付列表和临时扫码展示分别持有：创建确认回执或用户主动继续并经真实订单 GET 校验后，才在可关闭的扫码模态中展示原渠道、金额、二维码、链接与倒计时。关闭窗口只退役 UI 展示，不关闭订单。个人套餐区只列 `CREATED/PENDING + OPEN + 未超期` 订单，页面加载只恢复列表；历史详情始终只读、不恢复付款指令。

模态活跃期间，个人或组织财务页面仍以原 `2000ms + 次数×500ms`、上限 `5000ms`、±10% 抖动递归查询单订单。个人只要仍有待支付订单，就以同一节奏独立刷新订单集合，因而关闭模态后仍能接收回调状态；集合刷新不选单、不调用详情 GET、不保留或恢复集合回传的付款指令。服务端状态、非 OPEN、本地截止、权限或 owner/client/session epoch 变化立即撤下扫码。关闭时同步退役展示 token，旧详情响应不能重新开窗；当前视图同步保留已经接收的不可付款事实，不能被迟到列表、详情或创建回执中的 PENDING 覆盖；即使付款后刷新加入先前的列表请求，也不能重新插回待支付条目。个人公开订单/历史投影没有 summary_version，不臆造版本字段。已主动确认的服务端付款截止只作为原窗口边界保留，旧集合不能延长倒计时。所有保全事实随 client/owner/context/session epoch 退休，不进入全局存储。创建成功后即保留无扫码指令的待支付条目，不依赖随后的集合刷新成功。

同一 mounted client/owner/context/个人或组织授权版本/session epoch 的手动查询与自动查询共享同订单 GET。退役或卸载中止其请求，包括client epoch已变化但React尚未重渲染的间隙，旧结果不写入新身份或订单；401/403停止该轮询，其他错误按原节奏在付款窗口内重试。服务端终态、非 OPEN 和本地截止时间停止后续读取；付款窗口到达前已经开始的只读查询仍可归并准确属于原订单的终态回执。同一次 GET 的手动与自动消费者共同归并 PAID/PAID_LATE，按 GET Promise 去重刷新，后继同订单的新 GET 仍可刷新更新的履约事实。个人通过现有权益/订单 load 与父视图 refresh(false)刷新余额；组织通过当前显式组织的现有 wallet/packages load 刷新（仍尊重各自读取权限），不每tick重拉全部资源。现有原key未知创建恢复、重认证、关单确认和所有服务端状态规则保留。

生产 helper 与实际 BFF client 的受控 HTTP 测试（现有 npm 入口）：

```sh
npm --prefix pinvou3-app run test:node -- tests/account_platform_recharge_polling.test.mjs
```

真实 React 组件公开 fixture（可手动独立启动，自动浏览器门禁另见下方）：

```sh
node pinvou3-app/tests/account_platform_recharge_polling_server.mjs
```

输出 loopback URL。Vite只服务源代码公开投影和既有 React 依赖，投影在任务专属临时目录；不放宽默认 `.git` deny。使用完成后以 Ctrl+C停止并清理该服务自己的投影。没有数据库、真实账号、外部站点、Token 或付款凭据。`/__fixture__/control` 是仅此测试服务的模拟控制接口，不是产品 API；该 fixture 只提供受控订单 GET、商品/权益 GET 和原创建订单 POST，503 可模拟创建已记录但回执未知；不连接生产 API。

通过 DOM 可见按钮验收，无需注入或访问隐藏运行时：

1. Reset fixture 后订单列表出现公开 Pending 订单，可见 metrics 的 detailReads=0；尚未选择时不自动读取或显示二维码。
2. 点击产品组件自身 Details 只展示历史事实，不出现扫码。点击 Continue payment 后，真实 GET 仍可付款才显示独立扫码模态；打开期间 detailReads 定时增加且 productMutations=0。
3. 点击 Provider paid proof 后，下一轮 GET 更新组件为 COMPLETED、撤二维码，paidCallbacks=1；此后 detailReads保持不增长。
4. Reset、Continue payment、Hold order GET；等待 held=1，再点组件 Check payment，同订单 GET数量不得增加；先 Provider paid proof 再 Release old paid ACK，必须 COMPLETED、QR撤回且 paidCallbacks=1。只收到 PAID但服务端列表仍PENDING不等价履约完成。
5. 分别在 held=1 时点击 Switch to actor B、Retire login epoch、Revoke read permission 或 Unmount component，再 Release old paid ACK；不得把旧 Paid/QR/回调写入新身份或已退役组件，不得继续旧timer。每例用Reset重新开始。

可见 metrics 的请求计数只统计生产 `/api` 调用；fixture控制和只读指标请求另计，不伪装成产品写入。该公开 React 测试不能等价真实支付/Keycloak/Billing部署，更不能等价原生 Edge主题像素验证。实际执行情况由独立证据记录，不以源码或 Vite编译通过替代 DOM验收。

额外代际间隙验收：在held=1时点击`Retire client epoch without rendering`，只改变live client计数，不重渲染财务组件；独立metrics子组件显示observedClientEpoch增加而loginEpoch不变。再释放旧ACK，不得更新COMPLETED或增加paidCallbacks，也不得再发旧GET。此例不能由普通Retire login epoch（会触发重渲染）替代。


组织分支使用当前 `OrganizationClient.order(org, id, signal)` 的原 GET，不以当前隐式组织替代请求中的组织 ID。客户端继续验证订单 ID/beneficiary 组织；原服务端负责原组织钱包绑定。拥有 `org.entitlements.purchase` 即沿当前入口查询，不新增 `org.wallet.read` 前置条件；钱包/包刷新仍只在拥有现有读取权限时发起。切换组织、成员授权版本变化、权限撤销、actor/登录代际变化、卸载时退役原读取。

真实 React 组织验收：点击 Organization surface、Reset fixture，在生产组件订单输入中填入页面公开 reference，点击 Query order。出现 PENDING及QR后才自动查询。Provider paid proof 后下一轮应 PAID/CREDITED、QR撤回，metrics walletReads/packageReads各增加一次且全部org请求保持显式原组织，productMutations=0。held GET期间点生产 Query order应共享同GET，Provider paid proof→Release 后同样只刷新一次。Switch to organization B 后原选择清空、原ACK不能进入B；对 live client epoch无重渲染、actor切换、撤销权限和卸载执行同样隔离断言。Remove organization wallet read permission 后仍可填原订单并查询/轮询，walletReads不得新增。个人paidCallbacks只观察个人父刷新，组织使用实际walletReads/packageReads观察既有load，不能借个人回调认定组织完成。


自动真实 React/生产 BFF/公开 loopback HTTP 门禁：

```sh
npm --prefix pinvou3-app run test:account-platform-recharge-ui
```

该入口已进入现有 frontend 全量和 account-platform 变更选择器；不是只启动 fixture。覆盖三语 Provider 事实、焦点/Tab/Escape、刷新仅恢复列表、历史只读、关闭后列表回调、终态/非 OPEN/截止撤销、旧 PENDING ACK 与集合 PAID 竞争、owner/授权/session epoch 退役、401/403、原 UNKNOWN key/事实重试，以及组织既有 Create/Query 无 wallet.read 的购买边界。组织仅在既有显式订单调用上复用模态，不增加组织订单集合或钱包读取权限。

扫码关闭与原关闭订单确认是两个独立动作；后者仍要求既有 manage 权限、当前订单与最后确认事实，不因前者产生 POST。所有 Provider/资金/关闭能力和服务端合同保持原样；真实支付、IdP、数据库及部署验收不在这些受控测试内。

截止前已经发出的同订单查询，关闭扫码窗或本地到期后仍可消费当前 owner/client/session 的 PAID 回执并去重刷新；不恢复扫码指令、不继续发 GET，切换订单或会话后丢弃。

组织同一当前视图内，查询/轮询确认的不可付款事实也约束迟到的同订单 Create ACK；反向迟到 Query 不得恢复旧 PENDING。原 UNKNOWN facts/key 重试仍可完成并 settle，只有展示合并保持已知不可付款事实及较早截止。此内存围栏随 owner/client/context/session 退休清空，不新增组织列表、接口或钱包读取前置条件。

组织 PAID 读回后刷新余额/包的独立 wallet.read 403 只显示刷新错误，保留已确认订单事实与 purchase 读取；products/capabilities 自身403仍退役购买读取，401仍退役认证。未伪造或清零余额，也不改变原 UNKNOWN 同key settle。
