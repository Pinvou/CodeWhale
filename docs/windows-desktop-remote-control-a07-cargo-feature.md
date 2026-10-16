# A07 Cargo feature 清理

移除未发布的 `desktop-controller = []` 空标记。该标记没有条件编译、构建脚本、依赖或目标消费者，不曾提供远控编译边界；默认 features 与依赖保持原样。

远控能力仍由实际平台能力及运行时策略判断。WebSocket 的 `pinvou-desktop-controller-v1` 是协议名，与 Cargo feature 无关，保留原协议和处理路径。

仓库构建脚本和 CI 未使用该 feature。未知外部开发脚本若显式传入 `--features desktop-controller`，应删除这个无效参数；不为未发布的空标记增加兼容别名。移除标记不表示远控已经接线、获得权限或通过发布验收。
