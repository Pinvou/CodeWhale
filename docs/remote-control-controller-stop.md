# 主控停止与加密通道

主控 manager 的 stop 显式接收与其配对的 ControllingLaneState，在生命周期锁内先封住旧操作、清除该 A 通道的会话密钥，再退休租约和 transport。调用者必须提供实际 A 通道，类型上不能传入独立 B 通道。当前没有生产 stop 入口；内部调用和测试均按此合同传入通道，后续接入生产时不能再省略清理所有者。

replace_transport 在 stopped 状态拒绝并停止迟到 sender；正常 connect 重新开启新一轮生命周期。已有 replace_owned_transport 仍按精确 operation 检查，不引入旧操作回退。

测试使用真实握手派生的 A/B session keys 和加密 lane，核对 Connected/Reconnecting 的实际前态、stop 后 A 不再持有或使用密钥、B 不受影响、重复 stop 及下一合法连接。材料释放由既有 cipher/SessionKeys Drop 清理实现支持，测试不读取释放后的内存。传输围栏的单独负控去除新检查后，原回归实际失败，恢复后重验。
