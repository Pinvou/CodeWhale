---
name: eip
version: 1.0.0
description: "【何时用:仅当用户问 H3C 员工门户(EIP)里的事——考勤打卡/请假假期/加班/待办审批/找同事/个人信息/集团新闻公告/食堂消费/报销/油卡/福利申报等。泛问通用知识不要用本技能。】H3C EIP 员工门户助手:查考勤、假期余额、加班时数、待办审批、搜人、看资讯、查消费报销等。"
metadata:
  requires:
    bins: ["eip"]
  cliHelp: "eip --help"
---

# EIP 员工门户(eip)

通过 `eip` 命令操作 H3C 员工门户。`eip` 是包装脚本(已在 PATH 上),内部自动处理凭证环境,**直接 `eip <子命令> ...` 即可**,不要写绝对路径、不要手动设环境变量。

## 认证

直接跑业务命令即可——未登录时 CLI 输出 `{"error":"not authenticated","message":"please connect EIP first"}`(退出码 3,不含登录地址)。此时引导用户在应用「工具商店」的「H3C 员工门户（EIP）」卡片点「连接」完成 EIP 登录后重试,不要自行编造数据。

## 调用方式

```
eip <子命令> [参数]
```
不确定参数时 `eip <子命令> --help`。

可选参数补充:`attendance calc-days` 默认按标准班次 08:30–18:00 计算天数,非标准班次可用 `--beg-time HH:MM:SS` / `--end-time HH:MM:SS` 覆盖;`survey submit` 对 1314 薪酬调研可附 `--emp-name` / `--id-card` / `--id-card-type`。

## 命令速查(按用户意图直接命中)

> 当前已接入「读」类场景。写类(审批/申报)见文末护栏,需用户确认且在测试账号验证后开放。

| 用户意图 | 命令 |
|---|---|
| 这个月考勤 | `eip attendance get-summary --date YYYY-MM` |
| 考勤异常 | `eip attendance list-abnormal --date YYYY-MM` |
| 忘刷卡 / 忘带卡 | `eip attendance list-forget-card --date YYYY-MM` / `list-forget-badge` |
| 班次 / 班车 | `eip attendance get-schedule` / `eip attendance list-shuttle-bus` |
| 弹性考勤 | `eip attendance list-flex --date YYYY-MM` |
| 还有几天年假 | `eip attendance get-annual-days` |
| 请假天数测算 | `eip attendance calc-days --beg-date DD --end-date DD --apply-type CODE`(CODE 取自 `eip attendance list-leave-types`) |
| 各类假期余额 | `eip leave get-balance` / `get-sick-balance` / `get-other-balance` |
| 假期列表 | `eip vacation list-annual` / `list-sick` / `list-compensatory` |
| 剩余工时 | `eip vacation get-remaining-hours --year YYYY` |
| 加班时数 | `eip overtime get-extra-hours` / `get-approving-hours` / `get-approved-hours` / `get-effectived-hours` |
| 有没有待办 | `eip todo` / `eip todo-count`;审批状态加 `--type`:`2` 已审批、`3` 我的申请、`4` 我的委托 |
| 待办详情 / 批量待办 | `eip todo-detail --system-id XXX --process-id XXX --docun-id XXX` / `eip todo-batch-list` / `eip todo-category` |
| 我的申请 / 本月申请流水 | `eip apply list` / `eip apply list-month-flow` |
| 个人信息 / 特殊纪念日 | `eip employee-profile` / `eip employee-special-day` |
| 搜人 / 找同事 | `eip employee-search --keyword XXX` |
| 联系方式 | `eip employee-contact --account XXX` |
| 应用 / 制度搜索 | `eip app-search --keyword XXX` / `eip ics-search --keyword XXX` |
| 集团新闻 / 公告 | `eip news [--keyword XXX]` / `eip announcement` / `eip announcement-detail --code XXX` |
| 课程 / 会议 / 社区 | `eip course` / `eip meeting` / `eip community-hot` |
| 站内消息 | `eip message count` / `eip message list` / `eip message get --id XXX` |
| 食堂消费 / 报销 / 油卡 | `eip consumption monthly` / `eip expense recent` / `eip expense monthly` / `eip fuel-card list` |
| 合同 / 保险年份 / 工会福利 / 调研 | `eip contract list` / `eip commercial-insurance list-years` / `eip union-welfare list --search-text XXX` / `eip survey list` |
| 远程办公 | `eip remote-work list-records` / `eip remote-work get-detail --doc-no XXX` |

## 输出规范

- 不要向用户展示 CLI 命令或原始 JSON;不要用 emoji。
- 自然语言,先结论后细节,不说"正在为您查询"之类过渡语。
- **单值/指标** → 一句话:"年假余额 5 天"、"待审批 3 条"。
- **汇总/列表** → 用表格,提取关键列、加中文表头;超过 10 条只展示前 10 条并标注"共 N 条,以下为前 10 条"。
- **无数据/出错** → 一句话:"暂无待审批事项"、"查询失败:网络超时,请稍后重试"。

## 护栏(重要)

- **审批(`todo-submit`)、各类申报与敏感写操作(`condolence`/`survey submit`/`union-apply join`/`employee-handbook sign`/`message read-all`/`message send-bless`/`condolence urge` 等)不可逆**——本版默认只做查询;如要执行写操作,**执行前必须明确向用户确认**,确认后才提交。
- 普通待办审批涉及详情/校验判断,复杂;优先展示待办详情让用户自行处理,不替用户判断能否直接批。
