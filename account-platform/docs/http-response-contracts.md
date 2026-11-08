# 公共 HTTP 响应与指标边界

`api.WriteJSON` 在提交状态前完成 JSON 编码，保留原 HTML 转义和末尾换行。无法编码的值返回固定的 `500 INTERNAL_ERROR`，不返回值或编码错误的内部细节；204/304 保持无响应体。写出失败或短写会中断不完整响应，不追加第二份错误 JSON，也不修改已经发出的状态。客户端仍按原接口的幂等键、只读回执及未知结果合同处理交付不确定性。

HTTP 指标区分响应中断和连接接管：

- `pinvou_http_response_aborts_total` 保留原中断计数及已有 5xx 统计语义。
- `pinvou_http_response_hijacks_total` 仅在 `Hijack` 成功后计数，失败不计；接管完成不证明应用已交付完整响应。
- 没有经公共 writer 观察到响应头的接管使用 `pinvou_http_requests_total{status="hijacked"}`；已观察到的状态仍保留其数值，并同时计入接管指标。后续普通请求独立统计。
- 日志以固定 `HTTP_RESPONSE_HIJACKED` 标识接管，不输出连接或业务内容。

独立 metrics 监听器的读头、完整读取、写出和 idle 时限分别为3、10、10、30秒。该配置不修改业务 HTTP/SSE 的现有时限。

本文对应 [#1618](https://github.com/Pinvou/pinvou3/issues/1618) 的 HTTP 子项；时间校验、限流容量、熔断恢复、迁移及事件存储合同仍须分别验收。
