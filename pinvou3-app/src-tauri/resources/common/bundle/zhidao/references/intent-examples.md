# 意图判断正反例

## qa（知识问答）正反例

| 用户输入 | 正确做法 | 错误做法 |
|---------|---------|---------|
| "如何申请话费报销？" | `qa --question "如何申请话费报销？"` | ❌ 用 search |
| "怎么配置VLAN？" | `qa --question "怎么配置VLAN？"` | ❌ 用 search |
| "VXLAN和VLAN有什么区别？" | `qa --question "VXLAN和VLAN有什么区别？"` | ❌ 用 search |
| "防火墙故障怎么排查？" | `qa --question "防火墙故障怎么排查？"` | ❌ 用 search |
| "报销流程是什么？" | `qa --question "报销流程是什么？"` | ❌ 用 search |
| "IT热线怎么联系？" | `qa --question "IT热线怎么联系？"` | ❌ 用 search |
| "链路聚合的原理" | `qa --question "链路聚合的原理"` | ❌ 用 search |
| "能不能用交换机做路由？" | `qa --question "能不能用交换机做路由？"` | ❌ 用 search |
| "如何申请权限？" | `qa --question "如何申请权限？"` | ❌ 用 search |
| "R4900 G5尺寸大小" | `qa --question "R4900 G5尺寸大小"` | ❌ 用 search 搜"R4900 G5 尺寸" |
| "S6800功耗是多少" | `qa --question "S6800功耗是多少"` | ❌ 用 search 搜"S6800 功耗" |
| "R5300重量" | `qa --question "R5300重量"` | ❌ 用 search 搜"R5300" |
| "防火墙吞吐量" | `qa --question "防火墙吞吐量"` | ❌ 用 search 搜"防火墙吞吐量" |
| "交换机支持几块硬盘" | `qa --question "交换机支持几块硬盘"` | ❌ 用 search 搜"交换机 硬盘" |

| "姜志阳担任什么职位？" | `qa --question "姜志阳担任什么职位？"` | ❌ 用 search 搜"姜志阳" |
| "谁负责网络安全？" | `qa --question "谁负责网络安全？"` | ❌ 用 search |
| "测试中心是干什么的？" | `qa --question "测试中心是干什么的？"` | ❌ 用 search |
| "李四担任什么职位" | `qa --question "李四担任什么职位"` | ❌ 用 search 搜"李四 职位" |
| "研发部领导是谁" | `qa --question "研发部领导是谁"` | ❌ 用 search 搜"研发部 领导" |
| "市场部负责人是谁" | `qa --question "市场部负责人是谁"` | ❌ 用 search 搜"市场部 负责人" |
| "张三的任命通知" | `qa --question "张三的任命通知"` | ❌ 用 search 搜"张三 任命" |
| "最新的免职发文" | `qa --question "最新的免职发文"` | ❌ 用 search 搜"免职发文" |
| "研发部部门职责" | `qa --question "研发部部门职责"` | ❌ 用 search 搜"研发部部门职责" |
| "公司组织架构" | `qa --question "公司组织架构"` | ❌ 用 search 搜"组织架构" |
| "发文流程是什么" | `qa --question "发文流程是什么"` | ❌ 用 search 搜"发文流程" |
| "市场部部门职能" | `qa --question "市场部部门职能"` | ❌ 用 search 搜"市场部部门职能" |

| "年终奖什么时候发" | `qa --question "年终奖什么时候发"` | ❌ 用 search 搜"年终奖" |
| "PBC怎么写" | `qa --question "PBC怎么写"` | ❌ 用 search 搜"PBC" |
| "加班调休怎么申请" | `qa --question "加班调休怎么申请"` | ❌ 用 search 搜"加班调休" |
| "入职流程是什么" | `qa --question "入职流程是什么"` | ❌ 用 search 搜"入职流程" |
| "T5晋升条件" | `qa --question "T5晋升条件"` | ❌ 用 search 搜"T5 晋升" |
| "五险一金缴纳比例" | `qa --question "五险一金缴纳比例"` | ❌ 用 search 搜"五险一金" |
| "户口落户怎么办理" | `qa --question "户口落户怎么办理"` | ❌ 用 search 搜"户口落户" |
| "四个坚持是什么" | `qa --question "四个坚持是什么"` | ❌ 用 search 搜"四个坚持" |
| "弹性打卡规则" | `qa --question "弹性打卡规则"` | ❌ 用 search 搜"弹性打卡" |

| "工卡补办怎么办理" | `qa --question "工卡补办怎么办理"` | ❌ 用 search 搜"工卡补办" |
| "会议室怎么预约" | `qa --question "会议室怎么预约"` | ❌ 用 search 搜"会议室预约" |
| "充电桩在哪里" | `qa --question "充电桩在哪里"` | ❌ 用 search 搜"充电桩" |
| "物业报修怎么操作" | `qa --question "物业报修怎么操作"` | ❌ 用 search 搜"物业报修" |
| "食堂夜宵几点开始" | `qa --question "食堂夜宵几点开始"` | ❌ 用 search 搜"食堂夜宵" |
| "工位怎么申请" | `qa --question "工位怎么申请"` | ❌ 用 search 搜"工位申请" |
| "协议酒店有哪些" | `qa --question "协议酒店有哪些"` | ❌ 用 search 搜"协议酒店" |
| "快递代收在哪里取" | `qa --question "快递代收在哪里取"` | ❌ 用 search 搜"快递代收" |
| "体检怎么预约" | `qa --question "体检怎么预约"` | ❌ 用 search 搜"体检预约" |

| "差旅报销怎么申请" | `qa --question "差旅报销怎么申请"` | ❌ 用 search 搜"差旅报销" |
| "话费报销标准是多少" | `qa --question "话费报销标准是多少"` | ❌ 用 search 搜"话费报销" |
| "发票抬头是什么" | `qa --question "发票抬头是什么"` | ❌ 用 search 搜"发票抬头" |
| "酒店报销需要什么材料" | `qa --question "酒店报销需要什么材料"` | ❌ 用 search 搜"酒店报销" |
| "报销截止时间是什么时候" | `qa --question "报销截止时间是什么时候"` | ❌ 用 search 搜"报销截止" |
| "贴票规则是什么" | `qa --question "贴票规则是什么"` | ❌ 用 search 搜"贴票规则" |
| "对公打款怎么操作" | `qa --question "对公打款怎么操作"` | ❌ 用 search 搜"对公打款" |
| "住宿报销标准" | `qa --question "住宿报销标准"` | ❌ 用 search 搜"住宿报销标准" |

| "域密码过期怎么重置" | `qa --question "域密码过期怎么重置"` | ❌ 用 search 搜"域密码" |
| "Outlook邮件转发怎么设置" | `qa --question "Outlook邮件转发怎么设置"` | ❌ 用 search 搜"Outlook转发" |
| "VPN连接失败怎么办" | `qa --question "VPN连接失败怎么办"` | ❌ 用 search 搜"VPN" |
| "电脑蓝屏怎么处理" | `qa --question "电脑蓝屏怎么处理"` | ❌ 用 search 搜"蓝屏" |
| "云桌面脱域怎么办" | `qa --question "云桌面脱域怎么办"` | ❌ 用 search 搜"脱域" |
| "FTP上传报permission denied" | `qa --question "FTP上传报permission denied"` | ❌ 用 search 搜"FTP" |
| "腾讯会议屏幕共享不了" | `qa --question "腾讯会议屏幕共享不了"` | ❌ 用 search 搜"腾讯会议" |
| "C盘满了怎么清理" | `qa --question "C盘满了怎么清理"` | ❌ 用 search 搜"C盘" |
| "WiFi连不上怎么办" | `qa --question "WiFi连不上怎么办"` | ❌ 用 search 搜"WiFi" |
| "Excel宏怎么启用" | `qa --question "Excel宏怎么启用"` | ❌ 用 search 搜"Excel宏" |

## search（文档搜索）例表

| 用户输入 | 正确做法 | 错误做法 |
|---------|---------|---------|
| "搜一下服务器相关彩页" | `search --query "服务器彩页"` | ❌ 用 qa |
| "查找VLAN配置的文档" | `search --query "VLAN配置文档"` | ❌ 用 qa |
| "PMO里有没有防火墙资料？" | `search --query "防火墙资料" --source pmo` | ❌ 漏掉 --source 或把PMO混入query |
| "搜索PMO里面的配置手册" | `search --query "配置手册" --source pmo` | ❌ `--query "pmo配置手册"` 或漏掉 --source |
| "搜索藏经阁里面的配置手册" | `search --query "配置手册" --source cjg` | ❌ 漏掉 --source 或把藏经阁混入query |
| "搜索品牌营销资料里面的营销资料" | `search --query "营销资料" --source cmp` | ❌ `--query "品牌营销资料营销资料"` 或漏掉 --source |
| "帮我找一下产品营销手册" | `search --query "产品营销手册"` | ❌ 用 qa |
| "有哪些交换机白皮书？" | `search --query "交换机白皮书"` | ❌ 用 qa |
| "搜一下R4900的规格书" | `search --query "R4900规格书"` | ❌ 用 qa |
| "搜索PMO中的彩页并按下载量排序" | `search --query "彩页" --source pmo --sort download` | ❌ `--query "pmo彩页" --sort download` 或漏掉 --source |
| "藏经阁里最新的白皮书" | `search --query "白皮书" --source cjg --sort time` | ❌ `--query "藏经阁白皮书" --sort time` 或漏掉 --source |
| "产品知识中浏览量最高的文档" | `search --query "文档" --source knowledge --sort visit` | ❌ `--query "产品知识文档" --sort visit` 或漏掉 --source |
| "搜索藏经阁里面的产品知识" | `search --query "产品知识" --source cjg` | ❌ `--query "产品知识"`（漏掉 --source cjg）|
| "在PMO搜交换机" | `search --query "交换机" --source pmo` | ❌ `--query "交换机"`（漏掉 --source pmo）|
