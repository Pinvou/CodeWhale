# 远控敏感证据扫描合同

`ControllerSensitiveScanner.scan/assertClean` 每次必须收到且只收到四个自有数据属性：
`logs`、`persistence`、`packet_capture`、`crash_diagnostics`。缺面、未知面、访问器或
`undefined/null` 等非法值均以固定 `controller_scan_*` 错误拒绝，不能得到 clean 回执。
数据支持 UTF-8 字符串、Buffer/Uint8Array 和可序列化的 JSON 对象/数组；序列化失败
不回显错误、属性名、凭据或 canary。显式空内容可扫描，但不能单独证明收集确实执行。
Buffer/Uint8Array 按内槽读取实际视图范围，跨 realm 或替换原型不会改变字节来源；
DataView 及其他元素类型的视图显式拒绝，不回退为 JSON。普通 JSON 对象的 `toJSON`
按实际序列化结果扫描；该结果不代表对象序列化前的全部内存。已登记检测规则由实例私有保存。
canary 列表必须为逐项有效的非空自有数据列表，稀疏项或访问器不能注册为空规则后返回 clean。

固定 canary 的检测范围包括完整值的原文、标准 Base64/Base64URL（带/不带 padding）、
大小写 hex、JSON 字符转义、全 UTF-16 `\uXXXX` 转义（大小写 hex）及 URI 百分号编码
（encodeURIComponent 或全部 UTF-8 字节编码）。不承诺识别任意混合/多重编码、压缩、
加密或只泄露部分值；生产仍须在源头脱敏。回执仅包含固定表面名称，不包含 canary。

`controller-sensitive-scan.test.js` 同时验证缺面与非法形状的失败关闭、上述编码在全部
四面上的污染拒绝，以及真实生命周期收集。发布资格仍须经过唯一的 deep evidence
validator，重新校验 artifact 摘要、签名和关联；scanner 或 Ajv 单独通过不等于可发布。
