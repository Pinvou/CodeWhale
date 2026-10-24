# 未确认注册操作的只读定位

具有既有 platform.registrations.repair 权限的运营人员，可在注册修复页或 GET /admin/v1/registration-operations 查看已记录创建尝试、仍处于 IDENTITY_CREATING、尚未确认 Identity 或提交 Account 的操作。operation_id 参数按原注册操作 UUID 精确筛选；未知 ID 返回空列表，格式无效或全零 ID 返回400，权限边界不变。

状态、阶段、责任服务、创建/更新时间、下次自动查询时间与创建尝试登记时间来自持久事实。identity_create_attempted_at 是网络创建前登记的耐久尝试事实，不代表 Identity 已创建或远端已确认接收。操作年龄仅由真实 created_at 展示；原 older_than_minutes 仍表示距 updated_at 的最小时间，后台查询更新后会影响这一筛选。

未知 Identity 没有已确认的 identity_created_at，不从操作创建或尝试时间制造30分钟/24小时SLA。已有SLA来源与过滤不变：WITHIN/OVERDUE不包含无SLA的未知操作，界面显示空值。列表仍受原100条上限约束，可用原操作ID精确定位，不推算总数。

这些未知行只读，不提供重试或补偿操作。后台继续按原关联查询，只有原durable首个grant可进入Identity创建；本项不改变该恢复流程，不释放预约、不删除或重建Identity。已有REPAIR_REQUIRED重试和满足原证明门禁的补偿、资料变更重试保持。

读取不返回手机号、用户名、Identity subject、密码/引用、验证码、指纹或查询凭据。拒绝的写请求和未授权访问可能按原规则写安全审计；这不代表注册或身份领域效果发生。当前未知行retry的既有通用错误分类仍为500拒绝，补偿为409拒绝；本项不改该分类。
