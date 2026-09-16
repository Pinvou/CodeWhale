# EIP 官方 eip-cli 逐命令 API 黄金报文(Golden Reference)

> 用途:作为 `pinvou3-app/src-tauri/resources/common/bundle/eip/linux-src/main.go`(自研 fallback
> CLI)路由表重写的**唯一事实来源**。fallback 现行路由表与本文件逐条对齐前,业务接口大面积
> 404/405 的根因即二者不一致。
>
> 数据来源:官方 iClaw `eip-cli.exe`(版本 `0.0.1-go1.25.0.windows-amd64`,取自本仓库 git
> 历史 `d6fb2b6b~1:pinvou3-app/src-tauri/resources/platforms/windows/x86_64/bundle/eip/bin/eip-cli.exe`),
> 等长 patch base URL 指向本地假服务端后逐命令抓包(2026-09-15)。所有请求均真实发出并记录,
> 非从字符串表推测。复现方法见文末。

## 1. 全局约定

| 项 | 值 |
|---|---|
| 业务 base URL(prod) | `http://apieip.h3c.com/eip` |
| 业务 base URL(test) | `http://apineweip-ts.h3c.com/eip`(注意是 **new** eip,提示存在新版网关) |
| 认证 base URL(prod) | `http://api.eos.h3c.com/auth/v1.0` |
| 认证 base URL(test) | `http://api.eos-ts.h3c.com/auth/v1.0` |
| Authorization | 直接发保存的原始 token 字符串,**不主动加 `Bearer ` 前缀**(`auth save --token` 存什么发什么;官方示例保存的就是 `Bearer eyJ...`)。**fallback 的有意偏差**:保存时 stripBearer 归一、发送时统一加 `Bearer ` 前缀——存量两种形态上线报文一致;改为原样透传需在真实网关分别验证,出现真实 401 个例前维持现状 |
| Content-Type | `application/json`(GET 也带) |
| 认证域额外头 | `Auth-Type: SSO_NEXT`(仅 auth base 上的登录/轮询/换 token) |
| 可配置前缀 | `attendance_prefix`(prod `/hrss`,test `/hrss-ts`)、`hr_prefix`(`/hr`);其余前缀硬编码 |

**双段前缀的成因**:URL = `eipBase + 组前缀 + path`,而 app/external/announcement/organisation/message
五组的 path 本身以组名开头,官方并未去重,所以线上真实路径就是双段
(`/eip/app/app/...`、`/eip/external/external/...`、`/eip/announcement/announcement/...`、
`/eip/organisation/organisation/...`、`/eip/message/message/...`)。fallback 曾将其"去重"为单段
(commit 5c37c662),与官方实测不符。例外:`employee-contact` 的 `/organisation/user/tel/info/{account}`
**不**加 organisation 组前缀,是单段。

**分页风格不统一**:`pageNum`/`pageSize`(多数)、`PageIndex`/`PageSize`(vacation 列表)、
`PageIndex`/`PageCount`(consumption list),须逐条按下表。

## 2. RSA 参数加密(get-summary / get-calendar 专用)

`attendance get-summary` 与 `attendance get-calendar` 的 `date` query 参数不是明文,而是:

1. 明文为 `YYYY-MM`(如 `2026-09`);
2. 用内嵌 RSA **1024 位**公钥、**PKCS#1 v1.5** 填充加密(官方二进制含 `crypto/rsa.EncryptPKCS1v15`,
   无 OAEP),输出 128 字节;
3. base64(标准 alphabet,含 `+` `/` `=`)后再 URL encode 放入 `date` 参数。
   每次加密结果不同(PKCS1v15 随机填充),抓包样本形如
   `?date=E5sXKyj0DjdSoVELSX74ANa%2FkM04NdfCX...%3D`(base64 长 172 字符)。

内嵌公钥(PEM,SPKI 格式,从官方 exe 偏移 4655489 处原样提取,已用 Node `crypto.publicEncrypt`
验证可加密、密文 128 字节):

```
-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQCghu4j7NRaPkuotWUs/n5adIvOs19GpAfwhs/nTaaYp/paheKNc00Ns7xGbUAJli+Qog8x1iuv/MBoTx7WbjVCz8ZgpqFBb4OkVtG2FRWMkiC1qEsr0Vm1ljHxEW61oDQcI/L66i/BzlMUBIzWC6ogzgqnz5RTGPZKzncofzpqjQIDAQAB
-----END PUBLIC KEY-----
```

提取方法:`grep -aboE -e '-----BEGIN PUBLIC KEY-----' eip-cli.exe` 定位,截取至
`-----END PUBLIC KEY-----`;或任意二进制编辑器搜索 PEM 头。私钥在服务端,无法也不必解密。

其余接口均明文传参,无加密。

## 3. 认证域(auth base,3 条)

| 命令 | Method | 路径 | Query | Body(JSON 键示例) | 备注 |
|---|---|---|---|---|---|
| `auth login` | POST | `{authBase}/api/v2/user/agent/session/login` | — | `{"deviceId":"...","metaData":{"name":"...","os":"...","arch":"...","client":"..."}}` | 响应 `data` 含 `ssoLoginUrl`/`sessionId`;`data.pollInterval` 服务端已改为 number,官方二进制反序列化即崩,这是弃用官方 exe 的直接原因 |
| `auth poll` | GET | `{authBase}/api/v2/user/agent/session/poll/{sessionId}` | — | — | 认证成功后响应含 `token`/`ait` |
| (token 刷新) | POST | `{authBase}/api/v2/user/agent/token/exchange` | — | `{"ait":"...","deviceId":"..."}` | AT 过期(Code 51)时用 AIT 换新 AT |

## 4. 考勤 attendance(15 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `attendance get-summary --date 2026-09` | GET | `/eip/hrss/api/ApiAttendance/GetEmpCalendarSummaryInfo` | `date=<RSA加密>` | — | date 需 RSA 加密,见 §2 |
| `attendance get-calendar --date 2026-09` | GET | `/eip/hrss/api/ApiAttendance/GetEmpCalendarDetailInfo` | `date=<RSA加密>` | — | date 需 RSA 加密,见 §2 |
| `attendance get-overview --date 2026-09` | GET | `/eip/hrss/api/ApiAttendance/GetEmpMonthSummaryInfo` | `date=2026-09` | — | 明文 date |
| `attendance get-annual-days` | GET | `/eip/hrss/api/ApiAttendance/GetAnnualDays` | — | — | |
| `attendance get-schedule` | GET | `/eip/hrss/api/ApiAttendance/GetWorkSchduleList` | `applyNo=` | — | 服务端拼写即 `Schdule`(原拼写错误) |
| `attendance list-shuttle-bus` | GET | `/eip/hrss/api/ApiAttendance/GetShuttleBusLineList` | — | — | |
| `attendance list-leave-types` | GET | `/eip/hrss/api/apiAttendance/GetLeaveApplyTypeList` | — | — | 注意是**小写** `apiAttendance` |
| `attendance calc-days --beg-date 15 --end-date 16 --apply-type 01` | POST | `/eip/hrss/api/ApiAttendance/CalcApplyItemDays` | `funcType=Leavetime` | `{"ApplyType":"01","ApplyTypeText":"","BegDate":"15","Begzt":"08:30:00","EndDate":"16","Endzt":"18:00:00"}` | 键全大写开头;`Begzt`/`Endzt` 为官方原拼写 |
| `attendance get-annual-calc` | GET | `/eip/hrss/api/ApiAttendance/GetAnnualCaclData` | `caclDate=2026-09-15` | — | 缺省当天;`Cacl` 为官方原拼写 |
| `attendance get-person-summary` | GET | `/eip/hrss/api/ApiAttendance/GetPersonCaclAttendanceModelList` | `applyNo=&flowType=a` | — | 无 `--date` 之类的 flag |
| `attendance get-effectived-summary --date 2026-09` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetEmpOCLAbnormalData` | `Date=2026-09` | `{"pageNum":1,"pageSize":100}` | pageSize 固定 100;fallback 曾把此端点配给 list-flex |
| `attendance list-abnormal --date 2026-09` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetAbTimeRecordTable` | `Date=2026-09` | `{"pageNum":1,"pageSize":10}` | `Date` 大写 |
| `attendance list-forget-card --date 2026-09` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetEmpWSKCS` | `Date=2026-09` | `{"pageNum":1,"pageSize":10}` | |
| `attendance list-forget-badge --date 2026-09` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetEmpWDKCS` | `Date=2026-09` | `{"pageNum":1,"pageSize":10}` | |
| `attendance list-flex --date 2026-09` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetEmpTXCS` | `Date=2026-09` | `{"pageNum":1,"pageSize":10}` | fallback 曾把 GetEmpTXCS 配给 vacation get-remaining-hours |

## 5. 假期余额 leave(5 条,全部同一端点)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `leave get-balance` | GET | `/eip/hrss/api/ApiLeavetime/LeavetimeApply` | — | — | 预配年假余额 |
| `leave get-other-balance` | GET | `/eip/hrss/api/ApiLeavetime/LeavetimeApply` | — | — | 补休假余额;响应同上,客户端侧取数 |
| `leave get-sick-balance` | GET | `/eip/hrss/api/ApiLeavetime/LeavetimeApply` | — | — | 全薪病假余额 |
| `leave get-sick-used` | GET | `/eip/hrss/api/ApiLeavetime/LeavetimeApply` | — | — | 病假已用 |
| `leave get-compassionate-used` | GET | `/eip/hrss/api/ApiLeavetime/LeavetimeApply` | — | — | 事假已用 |

fallback 路由表里的 `GetAnnualLeaveBalance`/`GetOtherLeaveBalance`/`GetFullPaySickLeaveBalance`/
`GetSikeLeaveSAPHours`/`GetCompassionateLeaveApprovedDays` 均存在于官方二进制字符串表但**不被任何命令使用**
(疑为旧版遗留),官方统一走 `ApiLeavetime/LeavetimeApply`。

## 6. 假期明细 vacation(4 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `vacation list-annual` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetVacationInfoTableList` | `calcDate=2026-09-15&resignationCode=` | `{"PageIndex":1,"PageSize":10}` | calcDate 缺省当天;分页键**大写** |
| `vacation list-sick` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetVacationInfoLeaveTableList` | — | `{"PageIndex":1,"PageSize":10}` | |
| `vacation list-compensatory` | GET | `/eip/hrss/api/ApiSapWorkAttendance/GetWorkVacationLeaveList` | `PageIndex=1&PageSize=10` | — | GET 且分页在 query |
| `vacation get-remaining-hours --year 2026` | POST | `/eip/hrss/api/ApiSapWorkAttendance/GetWorkAttendanceTotalTable` | `Date=2026` | `{"pageNum":1,"pageSize":10}` | Date 取年份;此端点被 fallback 错配给 attendance get-effectived-summary |

## 7. 加班 overtime(6 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `overtime list-types` | GET | `/eip/hrss/api/ApiAttendance/GetOvertimeApplyTypeList` | — | — | |
| `overtime get-extra-hours` | GET | `/eip/hrss/api/ApiOvertime/OvertimeApply` | — | — | 额外工时 |
| `overtime get-approving-hours` | GET | `/eip/hrss/api/ApiOvertime/OvertimeApply` | — | — | 同上端点,客户端侧区分 |
| `overtime get-approved-hours` | GET | `/eip/hrss/api/ApiOvertime/OvertimeApply` | — | — | 同 |
| `overtime get-effectived-hours` | GET | `/eip/hrss/api/ApiOvertime/OvertimeApply` | — | — | 同 |
| `overtime get-limit-hours` | GET | `/eip/hrss/api/ApiOvertime/OvertimeApply` | — | — | 同 |

fallback 的 `GetOvertimeApplyExtraHours` 等 5 个 `ApiAttendance/GetOvertime*` 路径同为字符串表遗留,
官方实际只用 `ApiOvertime/OvertimeApply`。

## 8. 待办 todo*(6 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `todo` | POST | `/eip/app/app/workFlow/page` | — | `{"condition":{"flowType":1},"pageNum":1,"pageSize":10}` | 双段 /app/app |
| `todo-count` | POST | `/eip/app/app/workFlow/total/count` | — | (空) | |
| `todo-detail --system-id S1 --process-id P1 --docun-id D1` | POST | `/eip/external/external/workflow/detail` | — | `{"systemId":"S1","processId":"P1","docunId":"D1"}` | |
| `todo-batch-list [--system-id]` | POST | `/eip/external/external/workflow/batch/flow/list` | — | `{"condition":{},"pageNum":1,"pageSize":10}` | system-id 进 condition |
| `todo-category [--page-size]` | POST | `/eip/external/external/workflow/batch/category/list` | — | `{"pageSize":10}` | |
| `todo-submit --action agree/reject/transfer --flow-list '[...]'` | POST | `/eip/external/external/workflow/submit` | — | `{"buttonType":"Agree","flowInfoList":[{"systemId":"BPM","processId":"P1","docunId":"D1","nodeId":"N1"}]}`;reject 加 `"opinion":"..."`;transfer 加 `"targetReceiver":"z12345","targetReceiverName":"张三"` | **写操作**;buttonType 首字母大写;flow-list 元素须含 nodeId |

## 9. 我的申请 apply(4 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `apply list` | POST | `/eip/hrss/api/ApiAttendance/GetWorkFlowList` | `flowCode=a` | `{"SearchText":"a","pageNum":1,"pageSize":10}` | flowCode/SearchText 固定 "a" |
| `apply get-detail --doc-no DOC001` | GET | `/eip/hrss/api/ApiAttendance/GetApplyWorkFlowInfo` | `docNo=DOC001` | — | |
| `apply list-attachments --doc-no DOC001` | GET | `/eip/hrss/api/ApiAttendance/GetApplyBillAttachFileList` | `docNo=DOC001` | — | |
| `apply list-month-flow [--scope personal/all]` | POST | `/eip/hrss/api/ApiCalendarMonthly/ApplyFlowPageList` | `queryType=0`(personal)/`1`(all) | `{"pageNum":1,"pageSize":10}` | 这是 fallback `attendance get-summary` 错配的端点——它本身合法返回空列表,造成"有响应但空记录" |

## 10. 员工 employee-* / app-search / ics-search(8 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `employee-profile` | GET | `/eip/organisation/organisation/user/info` | — | — | 双段 organisation;识别当前用户(姓名/工号)走这里 |
| `employee-search --keyword test` | POST | `/eip/organisation/organisation/user/page` | — | `{"condition":{"keywords":"test"},"pageNum":1,"pageSize":10}` | 键是 `keywords` 复数 |
| `employee-contact --account l36908` | GET | `/eip/organisation/user/tel/info/l36908` | — | — | **单段** organisation(不加组前缀) |
| `employee-special-day [--date 2026-09-15]` | GET | `/eip/organisation/organisation/user/specialDay` | `date=2026-09-15`(可选) | — | |
| `employee-handbook get-status` | GET | `/eip/hrss/api/ApiStaffNavigation/NewStaffStatus` | — | — | 注意:在 /hrss 不在 /hr;fallback 配的 /hr 前缀错误 |
| `employee-handbook sign` | GET | `/eip/hrss/api/ApiStaffNavigation/SignForReceipt` | `type=1` | — | **写操作**却用 GET |
| `app-search --keyword 请假` | POST | `/eip/app/app/appPool/page` | — | `{"condition":{"keywords":"请假"},"pageNum":1,"pageSize":10}` | |
| `ics-search --keyword 报销` | POST | `/eip/external/external/ics/page` | — | `{"condition":{"keywords":"报销"},"pageNum":1,"pageSize":10}` | |

## 11. 资讯/公告/学习(7 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `news [--keyword 测试] [--with-image]` | POST | `/eip/app/app/companyNews/page` | — | `{"condition":{"isImage":true,"keyword":"测试"},"pageNum":1,"pageSize":10}` | 无 keyword 时 condition 为 `{"isImage":false,"keyword":""}` |
| `announcement [--keyword 测试] [--type 1]` | POST | `/eip/announcement/announcement/page` | — | `{"condition":{"keyword":"测试","type":1},"pageNum":1,"pageSize":10}` | type 缺省 2(集团发文) |
| `announcement-detail --code GG2026001` | GET | `/eip/announcement/announcement/GG2026001` | — | — | code 进路径 |
| `course --type mine` | POST | `/eip/external/external/lms/my/course` | — | `{"pageNum":1,"pageSize":10}` | mine 为缺省 |
| `course --type hot` | POST | `/eip/external/external/lms/special/course` | — | `{"pageNum":1,"pageSize":10}` | fallback 的 type 分流方向反了(它把 mine 当缺省之外的特例) |
| `meeting [--date 2026-09-15]` | GET | `/eip/external/external/meeting/list/2026-09-15` | — | — | 缺省今天 |
| `community-hot [--page-size 20]` | GET | `/eip/external/external/bbs/list/20` | — | — | pageSize 进路径,缺省 10 |

## 12. 消费 consumption(3 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `consumption monthly` | GET | `/eip/hrss/api/ApiMyConsumption/MyConsumptionMonthly` | `Date=2026-09` | — | 在 /hrss 不在 /hr |
| `consumption list` | GET | `/eip/hrss/api/ApiMyConsumption/MyConsumptionPageList` | `Date=2026/09/15&PageCount=10&PageIndex=1` | — | Date 用 `YYYY/MM/DD` 斜杠格式;分页键 PageIndex/**PageCount** |
| `consumption trend` | GET | `/eip/hrss/api/ApiMyConsumption/MyConsumptionMonthlyList` | — | — | |

## 13. 报销 expense(4 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `expense recent` | GET | `/eip/external/external/sse/recent` | — | — | fallback 错配成 ApplyFlowPageList |
| `expense monthly [--currency-name]` | POST | `/eip/external/external/sse/statistics/month` | — | `{"currencyName":""}` | |
| `expense statistics [--currency-name]` | POST | `/eip/external/external/sse/statistics/status` | — | `{"currencyName":""}` | |
| `expense currency` | GET | `/eip/external/external/sse/currency` | — | — | |

## 14. 福利/油卡/合同/保险(10 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `fuel-card list` | POST | `/eip/hrss/api/ApiBenefits/MyOilCardPageList` | — | `{"pageNum":1,"pageSize":10}` | 在 /hrss 不在 /hr |
| `fuel-card seasons` | GET | `/eip/hrss/api/ApiBenefits/OilCardInvestSeasonly` | — | — | |
| `fuel-card apply-records` | GET | `/eip/hrss/api/ApiBenefits/OilCardApplyRecords` | — | — | |
| `contract list` | POST | `/eip/hrss/api/ApiBenefits/MyContractPageList` | — | `{"pageNum":1,"pageSize":10}` | |
| `commercial-insurance list --year 2026 [--type 2]` | GET | `/eip/hrss/api/ApiStaffNavigation/GetMyInsuranceList` | `type=1&year=2026` | — | type 1=本人 2=家人 |
| `commercial-insurance list-years` | GET | `/eip/hrss/api/ApiStaffNavigation/GetAllInsuranceYear` | — | — | |
| `union-welfare list` | POST | `/eip/hrss/api/ApiBenefits/MyUnionWelfarePageList` | — | `{"SearchText":"","pageNum":1,"pageSize":10}` | |
| `union-apply get-status` | GET | `/eip/hrss/api/ApiStaffNavigation/UnionApply` | — | — | |
| `union-apply join` | POST | `/eip/hrss/api/ApiStaffNavigation/JoinUnion` | — | (空) | **写操作** |
| `survey list` | GET | `/eip/hrss/api/ApiOpinionTemplate/GetOpenOpinionTemplateList` | — | — | 见 §15 其余 survey |

## 15. 调研 survey(其余 2 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `survey get --id T1` | GET | `/eip/hrss/api/ApiOpinionTemplate/OpinionCollection/T1` | — | — | id 进路径 |
| `survey submit --id T1 --opinion 同意` | POST | `/eip/hrss/api/ApiOpinionTemplate/SaveOpinionCollectionInfo` | — | `{"Id":"T1","Opinion":"同意"}` | **写操作**;键大写开头;1314Salary 类型另有 emp-name/id-card 等键 |

## 16. 远程办公 remote-work(3 条,唯一用 /hr 前缀的组)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `remote-work get-allow-check` | GET | `/eip/hr/tempHomeWork/allowInitiate` | — | — | path 不含 /api |
| `remote-work list-records` | GET | `/eip/hr/tempHomeWork/recordInfoList` | — | — | |
| `remote-work get-detail --doc-no RW001` | GET | `/eip/hr/tempHomeWork/mainDocModel` | `docNo=RW001` | — | |

## 17. 消息中心 message(5 条)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `message count` | GET | `/eip/message/message/readFlag` | — | — | 双段 message |
| `message list [--type SYSTEM]` | POST | `/eip/message/message/page` | — | `{"condition":{"typeList":["SYSTEM"]},"pageNum":1,"pageSize":10,"sortList":[{"asc":false,"column":"notice_time"}]}` | 无 type 时 condition 为 `{}` |
| `message get --id M1` | GET | `/eip/message/message/M1` | — | — | |
| `message read-all` | POST | `/eip/message/message/read/all` | — | (空) | **写操作** |
| `message send-bless --id M1 [--statement/--content]` | GET×2 或 POST | `--statement`:GET `/eip/message/message/M1` + GET `/eip/message/message/config/statement/1`;`--content`:POST `/eip/message/message/bless` | — | POST 时 `{"id":"M1","message":"生日快乐"}` | **写操作**;--statement 先自动取祝福语再发。抓包只覆盖段 1;官方按消息类型选祝福语池(生日/周年/新员工鼓励语,段号运行期计算),fallback 固定段 1,补抓后再对齐 |

## 18. 慰问申报 condolence(5 条,均为写/敏感操作)

| 命令 | Method | 路径 | Query | Body | 备注 |
|---|---|---|---|---|---|
| `condolence list` | POST | `/eip/hrss/api/ApiBenefits/CondolenceDeclarationPageList` | — | `{"pageNum":1,"pageSize":10}` | |
| `condolence validate --type 1 --certification-date ... --id-card ... --spouse-id-card ... --spouse-name ...` | POST | `/eip/hrss/api/ApiBenefits/VaildCondolenceDeclarationApply` | — | `{"CertificationDate":"2026-01-01","IDCard":"X1","SpouseIDCard":"Y1","SpouseName":"Z1","Type":"1"}` | `Vaild` 为官方原拼写;Type 为字符串 |
| `condolence urge --apply-id A1 --approver-no B1` | GET | `/eip/hrss/api/ApiBenefits/CondolenceDeclarationMailUrging` | `ApplyId=A1&ApproverNo=B1` | — | 邮催审批人 |
| `condolence create-funeral ...` | POST | `/eip/hrss/api/ApiBenefits/FuneralInfoApply` | — | `{"DirectSupervisor":"S1","FuneralDate":"2026-01-01","FuneralPersonIdCard":"Y1","FuneralPersonName":"Z1","FuneralPersonRelation":"R1","IDCard":"X1","Type":"3"}` | Type 固定 "3" |
| `condolence create-wedding --type 1 ...` | POST | `/eip/hrss/api/ApiBenefits/WeddingFuneralInfoApply` | — | `{"CertificationDate":"2026-01-01","IDCard":"X1","SpouseIDCard":"Y1","SpouseName":"Z1","Type":"1"}` | type=2(新生儿)另有 birth-date/birth-type/children-name 等键 |

## 19. 与 fallback 路由表的主要差异(修复清单摘要)

> 2026-09-15 起 fallback 路由表已覆盖本文件全部 87 条业务命令（官方命令名与
> 既有兼容别名并存，如 `survey list`↔`survey`、`remote-work list-records`↔`remote-work`），
> 并由 `linux-src/main_test.go` 逐命令固化本文件的 Method/路径/Query/Body 列；
> 下表保留为重写时的差异清单，供回溯。

1. **方法错误(服务端 405"方法不支持")**:fallback 几乎全 POST;官方大量 GET(考勤查询、余额、
   加班、消费、油卡季充/资格、保险、工会状态、调研列表、班次/班车/假期类型等)。
2. **端点错误(服务端 404)**:get-summary、todo/todo-count、leave 全部、overtime 时数全部、
   expense recent、apply list、employee-profile/employee-search、employee-handbook 等,fallback
   用的是字符串表里的遗留路径或张冠李戴(GetEmpOCLAbnormalData/GetEmpTXCS/
   GetWorkAttendanceTotalTable 三条互换)。
3. **前缀组错误**:`ApiBenefits`/`ApiMyConsumption`/`ApiOpinionTemplate`/`ApiStaffNavigation`
   官方都在 `/hrss` 下,fallback 的 `apiPrefix()` 错配到 `/hr`;`/hr` 仅 remote-work(tempHomeWork)使用。
4. **双段被去重**:app/external/announcement/organisation/message 五组必须保留双段。
5. **缺 RSA 加密**:get-summary/get-calendar 的 date 必须按 §2 加密,否则改对方法也会失败。
6. **分页/日期键**:按各表 Query/Body 列对齐(pageNum vs PageIndex/PageCount;Date vs calcDate vs
   caclDate;consumption list 的 `YYYY/MM/DD`)。
7. **黄金报文未覆盖的官方 flag**:本文件只抓了各命令的缺省调用,官方 help 里有而报文无证据的
   flag(todo `--keyword`、apply list `--status/--type`)fallback 一律显式拒绝(`unverifiedFlags`),
   不静默吞——吞掉会拿缺省数据冒充用户要的数据;线上键名已被缺省报文或官方 help 直接证实的除外
   (todo `--type`→`condition.flowType`、union-welfare list `--search-text`→`SearchText`)。
   补抓报文补入本文件后逐个转正。

## 20. 复现方法

1. 取官方二进制:`git show d6fb2b6b~1:pinvou3-app/src-tauri/resources/platforms/windows/x86_64/bundle/eip/bin/eip-cli.exe > eip-cli-official.exe`
2. 等长 patch:`http://apieip.h3c.com/eip`(25 字节)→ `http://127.0.0.1:8765/eip`;
   `http://api.eos.h3c.com/auth/v1.0`(32 字节)→ `http://127.0.0.1:8765/auth/v1.0/`。
3. 本地起记录型 HTTP 服务端(记录 method/path/headers/body,统一回 `{"Code":1,"Msg":"ok","Data":{...}}`)。
4. `AGENT_CREDENTIALS_DIR=<dir> AGENT_DEVICE_ID=dev1 ./eip-cli-patched.exe auth save --token fake --ait fake`
   后逐命令执行并收集日志。`config set-env` 的预设会覆盖 yaml 自定义地址,故必须 patch 而非改配置。
