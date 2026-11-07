# 熔断代次和只读恢复

熔断状态仍是进程内状态，按 provider、capability/action、route 隔离。连续五次基础设施失败或结果未知，或至少20个有效观测中失败不少于一半时打开；业务拒绝不进入失败比率。30秒起的指数退避和15分钟上限不变。分钟窗口改为61个固定秒桶，包含最老的部分秒，因此最多保留不足1秒的保守尾段；存储和锁内扫描不再随同一键的请求数增长。该界限按键计算，不改变不同键的生命周期。

生产调用保留 `AdmitHighRisk` / `AdmitReadOnly` 返回的 permit，并由它登记真实结果；本地派发前失败调用 `Cancel`。每个 permit 只能完成一次，绑定原熔断代次。只读半开 permit 最多同时三个，租期30秒，调用方丢失结果后预算可回收。过期或旧代结果不能关闭、重开或增加新代退避；三个有效只读响应才关闭当前半开代次。原有 `Before*` 接口只保留准入兼容，新的结果接线使用 permit；无绑定的 `Record` 只接受关闭状态观测，不能完成半开探测。

高风险派发保持 `BeforeHighRisk` 的禁止半开保护。Token 已领取的一次性 envelope 继续按原流程执行：`TrackAccepted` 只绑定其结果代次，不创建新准入或恢复权限。不得在消费授权后因为别的请求打开熔断就丢弃 envelope、重新领取或推断资金已释放。旧在途付费调用及其回放不作为恢复探测；已持久化失败回执在恢复后仍按原幂等键回放。

## MODEL_DISPATCH 的现有受信恢复入口

网关启动时，`WithRouteHealthCircuitRecovery` 在路由健康 worker 启动前接入同一个 repository、adapter 版本和派发 breaker。健康 worker 仍使用原凭据/Provider/adapter/URL白名单、专用CA及禁止重定向策略，发送原10秒预算的只读 `GET /v1/models`；不发送付费模型 POST。

恢复只作用于完全相同的 `provider + MODEL_DISPATCH + credential.BaseURL` 键。未知或关闭状态不新增熔断样本；正常健康查询不会稀释派发失败比率。冷却结束后，只有取得当前代次 permit、发现目标 model，并成功按原 route/health version 和 worker CAS 提交健康观测，才能计入恢复。当前有效失败重新打开退避；旧、过期、被替换的探测结果或未知数据库提交不计为恢复。

路由健康 worker 保留其独立的四并发只读职责，包括冷却期的常规健康检查；同键最多三个有效恢复 permit。无 permit 的常规健康结果没有恢复权限。探测频率、健康TTL和金融 hold 排除不变：健康路由原4分钟后再探测，失败按原退避，因此不能承诺冷却一结束就完成三个恢复观测。真实金融hold也不会被熔断恢复绕过。

Billing 的实际调用也绑定各自 permit，保留 provider版本/capability/route 以及原 provider-attempt 证据。一个查询键的响应不授权关闭另一个付款或退款键；本次没有新增资金重做或通用强制关闭入口。

## 验证

`TestDispatchCircuitReadOnlyRecoveryPublicPostgres` 通过公开Token迁移和真实运行角色、实际HTTP模型发现/route claim/CAS，覆盖真实冷却、只读三响应恢复、持续故障退避、原失败幂等回放及恢复后新请求。五次故障为受控熔断观测；授权、目录/计价及外部Billing为现有受控夹具，不冒称真实供应商扣款。另一个公开根保留已领取派发的原结果，并断言其付费成功不成为恢复探测。原 peer-effects 和未触碰接受状态回归保留。

核心race回归覆盖并发三探测、失联租期回收、重复完成、旧代回调、未知结果、业务拒绝、键隔离和窗口阈值。`BenchmarkCircuitBoundedWindow` 比较固定热键的观测开销，可在 `account-platform` 执行 `go test -run '^$' -bench '^BenchmarkCircuitBoundedWindow$' -benchmem ./internal/platform/resilience`；单键本机基准不代表全系统容量或生产SLA。现场供应商、跨实例隔离和发布验收分别核验。
