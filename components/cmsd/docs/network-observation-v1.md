# 只读网络事实

`local.network.read` 对应本机 Unix `GET /v1/network`。此能力不提供网络配置、组建、
RoCE/物理拓扑或集合通信验收。本单机版本不开放认证远程网络接口。

响应包含 device_id、boot_id、观测时间、采集耗时、`read_only=true`、
`physical_topology_verified=false` 和 `network_ready=null`：

- interfaces：内核接口名、索引、状态、MTU、IPv4/IPv6地址等原始 iproute2 字段。
- routes_ipv4、routes_ipv6：两个地址族各路由表的原始记录；不据此推断当前连接
  会选择哪条路径。策略规则、隧道/多路径和真实目的地址仍需单独诊断。
- queries：上述每项独立 available/error。查询失败为 null，成功空结果为 []，不可混用。
- rdma：`/sys/class/infiniband` 暴露的设备与端口属性。路径未暴露设备只说明内核
  本次无此记录，不能证明物理硬件不存在；`collective_validation=not_run`。

ip 命令只用固定查询参数、无 Shell，进程组及输出复用已有执行限制；三个命令共享三秒预算，
每项JSON不超过1MiB、65536结构值、4096记录。RDMA目录至多64设备/各64端口，单属性
读取至多1024字节。内核/sysfs读取不提供硬实时承诺；采集独占自己的单飞锁，始终不持有
全局Store，其他网络读取立即503 `network_query_busy`，不能继续堆积采集进程。
结果仅复用一秒并保留原观测时间；错误不改用上次成功数据冒充最新结果。

## 验证边界

单元测试覆盖查询故障、路由格式和未知状态。真实网络命令超时和权限边界需在独立实例另行核查；物理多机、RoCE 和集合通信需要另行验收。
