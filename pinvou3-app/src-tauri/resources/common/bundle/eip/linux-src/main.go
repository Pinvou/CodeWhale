// Portable fallback reimplementation of the H3C EIP employee-portal CLI.
//
// 五端统一由本源码构建（Windows x64 / Linux x64+arm64 / macOS arm64+x64，见
// build.sh）：官方 eip-cli 二进制因 SSO 服务端把登录响应 data.pollInterval 由
// string 改为 number 后 Go struct 反序列化必失败，已弃用不再打包。
//
// 业务路由表（方法/路径/query/body）已按官方 eip-cli 逐命令实测抓包报文对齐，
// 唯一事实来源：docs/eip-official-api-golden.md。
// Plain stdlib Go, no OS-specific syscalls.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	authBaseDefault = "http://api.eos.h3c.com/auth/v1.0"
	eipBaseDefault  = "http://apieip.h3c.com/eip"
	credSalt        = "eip-cli-cred-v1"
)

// os/arch 随构建目标走（linux/arm64 与 darwin 构建共用本源码）。
var userAgent = "eip-cli (pinvou3 " + runtime.GOOS + " " + runtime.GOARCH + ")"

type envelope struct {
	Code        any             `json:"code"`
	Msg         string          `json:"msg"`
	Message     string          `json:"message"`
	Data        json.RawMessage `json:"data"`
	SsoLoginURL string          `json:"ssoLoginUrl"`
	SessionID   string          `json:"sessionId"`
	Token       string          `json:"token"`
	Ait         string          `json:"ait"`
}

type sessionData struct {
	SessionID   string `json:"sessionId"`
	SsoLoginURL string `json:"ssoLoginUrl"`
	Token       string `json:"token"`
	Ait         string `json:"ait"`
}

type route struct {
	method string
	path   string                                      // 相对 eipBase 的完整路径（黄金表路径去掉开头的 /eip）
	query  func(map[string]string) (url.Values, error) // nil = 无 query
	body   func(map[string]string) map[string]any      // nil = 无 body
}

// 路由表：每行一个字面量，格式 "key": {"<METHOD>", "<path>", <query 构造器>, <body 构造器>}。
// key 集合是公开命令契约（eip.rs 的契约测试按行解析本表），逐条对齐
// docs/eip-official-api-golden.md §4-§18 的 Method/路径/Query/Body 列。
// 解析器硬约束:条目必须单行书写、key 仅含小写字母/空格/连字符——条目换行、
// 值改命名字段、key 引入其他字符都会静默掉出契约集合。
// 与官方命令名不同的既有 key（如 survey→survey list）保留为兼容别名，并补官方名别名，
// 使本 CLI 与官方 eip-cli 命令面完整对齐（黄金表全部 87 条业务命令）。
var routes = map[string]route{
	"attendance get-summary":            {"GET", "/hrss/api/ApiAttendance/GetEmpCalendarSummaryInfo", rsaMonthQuery, nil},
	"attendance get-calendar":           {"GET", "/hrss/api/ApiAttendance/GetEmpCalendarDetailInfo", rsaMonthQuery, nil},
	"attendance get-overview":           {"GET", "/hrss/api/ApiAttendance/GetEmpMonthSummaryInfo", plainMonthQuery, nil},
	"attendance get-annual-days":        {"GET", "/hrss/api/ApiAttendance/GetAnnualDays", nil, nil},
	"attendance get-schedule":           {"GET", "/hrss/api/ApiAttendance/GetWorkSchduleList", emptyApplyNoQuery, nil},
	"attendance list-shuttle-bus":       {"GET", "/hrss/api/ApiAttendance/GetShuttleBusLineList", nil, nil},
	"attendance list-leave-types":       {"GET", "/hrss/api/apiAttendance/GetLeaveApplyTypeList", nil, nil},
	"attendance calc-days":              {"POST", "/hrss/api/ApiAttendance/CalcApplyItemDays", funcTypeLeavetimeQuery, calcDaysBody},
	"attendance get-annual-calc":        {"GET", "/hrss/api/ApiAttendance/GetAnnualCaclData", caclDateQuery, nil},
	"attendance get-person-summary":     {"GET", "/hrss/api/ApiAttendance/GetPersonCaclAttendanceModelList", personSummaryQuery, nil},
	"attendance get-effectived-summary": {"POST", "/hrss/api/ApiSapWorkAttendance/GetEmpOCLAbnormalData", capDateMonthQuery, pagedBody100},
	"attendance list-abnormal":          {"POST", "/hrss/api/ApiSapWorkAttendance/GetAbTimeRecordTable", capDateMonthQuery, pagedBody},
	"attendance list-forget-card":       {"POST", "/hrss/api/ApiSapWorkAttendance/GetEmpWSKCS", capDateMonthQuery, pagedBody},
	"attendance list-forget-badge":      {"POST", "/hrss/api/ApiSapWorkAttendance/GetEmpWDKCS", capDateMonthQuery, pagedBody},
	"attendance list-flex":              {"POST", "/hrss/api/ApiSapWorkAttendance/GetEmpTXCS", capDateMonthQuery, pagedBody},
	"leave get-balance":                 {"GET", "/hrss/api/ApiLeavetime/LeavetimeApply", nil, nil},
	"leave get-other-balance":           {"GET", "/hrss/api/ApiLeavetime/LeavetimeApply", nil, nil},
	"leave get-sick-balance":            {"GET", "/hrss/api/ApiLeavetime/LeavetimeApply", nil, nil},
	"leave get-sick-used":               {"GET", "/hrss/api/ApiLeavetime/LeavetimeApply", nil, nil},
	"leave get-compassionate-used":      {"GET", "/hrss/api/ApiLeavetime/LeavetimeApply", nil, nil},
	"vacation list-annual":              {"POST", "/hrss/api/ApiSapWorkAttendance/GetVacationInfoTableList", calcDateQuery, pagedUpperBody},
	"vacation list-sick":                {"POST", "/hrss/api/ApiSapWorkAttendance/GetVacationInfoLeaveTableList", nil, pagedUpperBody},
	"vacation list-compensatory":        {"GET", "/hrss/api/ApiSapWorkAttendance/GetWorkVacationLeaveList", pagedUpperQuery, nil},
	"vacation get-remaining-hours":      {"POST", "/hrss/api/ApiSapWorkAttendance/GetWorkAttendanceTotalTable", yearDateQuery, pagedBody},
	"overtime list-types":               {"GET", "/hrss/api/ApiAttendance/GetOvertimeApplyTypeList", nil, nil},
	"overtime get-extra-hours":          {"GET", "/hrss/api/ApiOvertime/OvertimeApply", nil, nil},
	"overtime get-approving-hours":      {"GET", "/hrss/api/ApiOvertime/OvertimeApply", nil, nil},
	"overtime get-approved-hours":       {"GET", "/hrss/api/ApiOvertime/OvertimeApply", nil, nil},
	"overtime get-effectived-hours":     {"GET", "/hrss/api/ApiOvertime/OvertimeApply", nil, nil},
	"overtime get-limit-hours":          {"GET", "/hrss/api/ApiOvertime/OvertimeApply", nil, nil},
	"todo":                              {"POST", "/app/app/workFlow/page", nil, todoBody},
	"todo-count":                        {"POST", "/app/app/workFlow/total/count", nil, nil},
	"todo-detail":                       {"POST", "/external/external/workflow/detail", nil, todoDetailBody},
	"todo-batch-list":                   {"POST", "/external/external/workflow/batch/flow/list", nil, todoBatchListBody},
	"todo-category":                     {"POST", "/external/external/workflow/batch/category/list", nil, todoCategoryBody},
	// todo-submit 为写操作，body 需解析 --flow-list（JSON 数组或文件路径），在
	// runBusiness 特判（表内仅登记方法/路径作为契约锚点）。
	"todo-submit":            {"POST", "/external/external/workflow/submit", nil, nil},
	"apply list":             {"POST", "/hrss/api/ApiAttendance/GetWorkFlowList", flowCodeQuery, applyListBody},
	"apply get-detail":       {"GET", "/hrss/api/ApiAttendance/GetApplyWorkFlowInfo", docNoQuery, nil},
	"apply list-attachments": {"GET", "/hrss/api/ApiAttendance/GetApplyBillAttachFileList", docNoQuery, nil},
	"apply list-month-flow":  {"POST", "/hrss/api/ApiCalendarMonthly/ApplyFlowPageList", scopeQueryTypeQuery, pagedBody},
	"employee-profile":       {"GET", "/organisation/organisation/user/info", nil, nil},
	"employee-contact":       {"GET", "/organisation/user/tel/info/%s", nil, nil},
	"employee-special-day":   {"GET", "/organisation/organisation/user/specialDay", optionalDateQuery, nil},
	// 黄金表 §10：get-status 在 /hrss 不在 /hr（fallback 旧 apiPrefix 曾错配）。
	"employee-handbook sign":       {"GET", "/hrss/api/ApiStaffNavigation/SignForReceipt", typeOneQuery, nil},
	"employee-handbook get-status": {"GET", "/hrss/api/ApiStaffNavigation/NewStaffStatus", nil, nil},
	"employee-search":              {"POST", "/organisation/organisation/user/page", nil, employeeSearchBody},
	// app-search / ics-search 与 employee-search 同构：condition.keywords + 分页。
	"app-search":                      {"POST", "/app/app/appPool/page", nil, employeeSearchBody},
	"ics-search":                      {"POST", "/external/external/ics/page", nil, employeeSearchBody},
	"commercial-insurance":            {"GET", "/hrss/api/ApiStaffNavigation/GetMyInsuranceList", insuranceQuery, nil},
	"commercial-insurance list":       {"GET", "/hrss/api/ApiStaffNavigation/GetMyInsuranceList", insuranceQuery, nil},
	"commercial-insurance list-years": {"GET", "/hrss/api/ApiStaffNavigation/GetAllInsuranceYear", nil, nil},
	"contract":                        {"POST", "/hrss/api/ApiBenefits/MyContractPageList", nil, pagedBody},
	"contract list":                   {"POST", "/hrss/api/ApiBenefits/MyContractPageList", nil, pagedBody},
	"consumption monthly":             {"GET", "/hrss/api/ApiMyConsumption/MyConsumptionMonthly", capDateMonthQuery, nil},
	"consumption list":                {"GET", "/hrss/api/ApiMyConsumption/MyConsumptionPageList", consumptionListQuery, nil},
	"consumption trend":               {"GET", "/hrss/api/ApiMyConsumption/MyConsumptionMonthlyList", nil, nil},
	"expense recent":                  {"GET", "/external/external/sse/recent", nil, nil},
	"expense monthly":                 {"POST", "/external/external/sse/statistics/month", nil, currencyNameBody},
	"expense statistics":              {"POST", "/external/external/sse/statistics/status", nil, currencyNameBody},
	"expense currency":                {"GET", "/external/external/sse/currency", nil, nil},
	"fuel-card list":                  {"POST", "/hrss/api/ApiBenefits/MyOilCardPageList", nil, pagedBody},
	"fuel-card seasons":               {"GET", "/hrss/api/ApiBenefits/OilCardInvestSeasonly", nil, nil},
	"fuel-card apply-records":         {"GET", "/hrss/api/ApiBenefits/OilCardApplyRecords", nil, nil},
	"union-welfare":                   {"POST", "/hrss/api/ApiBenefits/MyUnionWelfarePageList", nil, unionWelfareBody},
	"union-welfare list":              {"POST", "/hrss/api/ApiBenefits/MyUnionWelfarePageList", nil, unionWelfareBody},
	"union-apply join":                {"POST", "/hrss/api/ApiStaffNavigation/JoinUnion", nil, nil},
	"union-apply status":              {"GET", "/hrss/api/ApiStaffNavigation/UnionApply", nil, nil},
	"union-apply get-status":          {"GET", "/hrss/api/ApiStaffNavigation/UnionApply", nil, nil},
	"survey":                          {"GET", "/hrss/api/ApiOpinionTemplate/GetOpenOpinionTemplateList", nil, nil},
	"survey list":                     {"GET", "/hrss/api/ApiOpinionTemplate/GetOpenOpinionTemplateList", nil, nil},
	"survey detail":                   {"GET", "/hrss/api/ApiOpinionTemplate/OpinionCollection/%s", nil, nil},
	"survey get":                      {"GET", "/hrss/api/ApiOpinionTemplate/OpinionCollection/%s", nil, nil},
	"survey submit":                   {"POST", "/hrss/api/ApiOpinionTemplate/SaveOpinionCollectionInfo", nil, surveySubmitBody},
	"remote-work":                     {"GET", "/hr/tempHomeWork/recordInfoList", nil, nil},
	"remote-work list-records":        {"GET", "/hr/tempHomeWork/recordInfoList", nil, nil},
	"remote-work get-allow-check":     {"GET", "/hr/tempHomeWork/allowInitiate", nil, nil},
	"remote-work get-detail":          {"GET", "/hr/tempHomeWork/mainDocModel", docNoQuery, nil},
	// 资讯/学习类公开命令（SKILL.md 命令速查表有载，契约测试把关不漂移）。
	// app/announcement/external/organisation 组的 path 本身以组名开头，官方未去重，
	// 线上真实路径就是双段（黄金表 §1）；employee-contact 是不加组前缀的单段例外。
	"news":         {"POST", "/app/app/companyNews/page", nil, newsBody},
	"announcement": {"POST", "/announcement/announcement/page", nil, announcementBody},
	// code 进路径（黄金表 §11），runBusiness 的 %s 填充按 --code 取值。
	"announcement-detail": {"GET", "/announcement/announcement/%s", nil, nil},
	"course":              {"POST", "/external/external/lms/my/course", nil, pagedBody},
	"meeting":             {"GET", "/external/external/meeting/list/%s", nil, nil},
	"community-hot":       {"GET", "/external/external/bbs/list/%s", nil, nil},
	// 消息中心（黄金表 §17）：路径双段 message/message；read-all/send-bless 为写操作。
	"message count":    {"GET", "/message/message/readFlag", nil, nil},
	"message list":     {"POST", "/message/message/page", nil, messageListBody},
	"message get":      {"GET", "/message/message/%s", nil, nil},
	"message read-all": {"POST", "/message/message/read/all", nil, nil},
	// send-bless 为多请求编排（--statement 先取祝福语再发），runBusiness 特判。
	"message send-bless": {"POST", "/message/message/bless", nil, nil},
	// 慰问申报（黄金表 §18）：均为写/敏感操作；Vaild 为官方原拼写。
	"condolence list":           {"POST", "/hrss/api/ApiBenefits/CondolenceDeclarationPageList", nil, pagedBody},
	"condolence validate":       {"POST", "/hrss/api/ApiBenefits/VaildCondolenceDeclarationApply", nil, condolenceValidateBody},
	"condolence urge":           {"GET", "/hrss/api/ApiBenefits/CondolenceDeclarationMailUrging", condolenceUrgeQuery, nil},
	"condolence create-funeral": {"POST", "/hrss/api/ApiBenefits/FuneralInfoApply", nil, condolenceFuneralBody},
	"condolence create-wedding": {"POST", "/hrss/api/ApiBenefits/WeddingFuneralInfoApply", nil, condolenceWeddingBody},
}

// ---- query 构造器（严格对齐黄金表各命令的 Query 列；GET 参数全部进 query）----

func monthNow() string { return time.Now().Format("2006-01") }
func todayNow() string { return time.Now().Format("2006-01-02") }
func yearNow() string  { return time.Now().Format("2006") }

// q 把键值对打包成 url.Values（Encode 时自动 URL encode）。
func q(pairs ...string) (url.Values, error) {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v, nil
}

// date=<RSA 加密的 YYYY-MM>（attendance get-summary / get-calendar 专用，黄金表 §2）。
// 本地先校验 YYYY-MM 形态：非法日期加密后服务端照样拒绝，白费一次请求。
func rsaMonthQuery(f map[string]string) (url.Values, error) {
	date := firstNonEmpty(f["date"], monthNow())
	if _, err := time.Parse("2006-01", date); err != nil {
		return nil, fmt.Errorf("--date must be YYYY-MM, got %q", date)
	}
	enc, err := encryptRSADate(date)
	if err != nil {
		return nil, err
	}
	return q("date", enc)
}

// date=YYYY-MM 明文（attendance get-overview）。
func plainMonthQuery(f map[string]string) (url.Values, error) {
	return q("date", firstNonEmpty(f["date"], monthNow()))
}

// Date=YYYY-MM 大写键（ApiSapWorkAttendance 系与 consumption monthly）。
func capDateMonthQuery(f map[string]string) (url.Values, error) {
	return q("Date", firstNonEmpty(f["date"], monthNow()))
}

// applyNo=（attendance get-schedule）。
func emptyApplyNoQuery(map[string]string) (url.Values, error) { return q("applyNo", "") }

// funcType=Leavetime（attendance calc-days）。
func funcTypeLeavetimeQuery(map[string]string) (url.Values, error) {
	return q("funcType", "Leavetime")
}

// caclDate=YYYY-MM-DD 缺省当天（attendance get-annual-calc；Cacl 为官方原拼写）。
func caclDateQuery(f map[string]string) (url.Values, error) {
	return q("caclDate", firstNonEmpty(f["date"], todayNow()))
}

// applyNo=&flowType=a（attendance get-person-summary）。
func personSummaryQuery(map[string]string) (url.Values, error) {
	return q("applyNo", "", "flowType", "a")
}

// calcDate=YYYY-MM-DD&resignationCode=（vacation list-annual，calcDate 缺省当天）。
func calcDateQuery(f map[string]string) (url.Values, error) {
	return q("calcDate", firstNonEmpty(f["date"], todayNow()), "resignationCode", "")
}

// PageIndex=1&PageSize=10 在 query（vacation list-compensatory，GET 且分页大写键）。
func pagedUpperQuery(f map[string]string) (url.Values, error) {
	return q("PageIndex", strconv.Itoa(atoiDefault(f["page"], 1)),
		"PageSize", strconv.Itoa(atoiDefault(f["page-size"], 10)))
}

// Date=YYYY 年份（vacation get-remaining-hours）。
func yearDateQuery(f map[string]string) (url.Values, error) {
	return q("Date", firstNonEmpty(f["year"], yearNow()))
}

// flowCode=a（apply list，固定 "a"）。
func flowCodeQuery(map[string]string) (url.Values, error) { return q("flowCode", "a") }

// docNo=<--doc-no>（apply get-detail / list-attachments）。
func docNoQuery(f map[string]string) (url.Values, error) { return q("docNo", f["doc-no"]) }

// type=1（employee-handbook sign）。
func typeOneQuery(map[string]string) (url.Values, error) { return q("type", "1") }

// type=<1 本人/2 家人>&year=YYYY（commercial-insurance 对应黄金表 commercial-insurance list）。
func insuranceQuery(f map[string]string) (url.Values, error) {
	return q("type", firstNonEmpty(f["type"], "1"), "year", firstNonEmpty(f["year"], yearNow()))
}

// consumption list：Date 用 YYYY/MM/DD 斜杠格式，分页键 PageIndex/PageCount 在 query。
func consumptionListQuery(f map[string]string) (url.Values, error) {
	return q("Date", strings.ReplaceAll(firstNonEmpty(f["date"], todayNow()), "-", "/"),
		"PageCount", strconv.Itoa(atoiDefault(f["page-size"], 10)),
		"PageIndex", strconv.Itoa(atoiDefault(f["page"], 1)))
}

// date=YYYY-MM-DD 可选（employee-special-day，不传则无 query）。
func optionalDateQuery(f map[string]string) (url.Values, error) {
	if d := f["date"]; d != "" {
		return q("date", d)
	}
	return nil, nil
}

// queryType=0(personal)/1(all)（apply list-month-flow，缺省 personal）。
// 非法值显式报错而非静默按 personal 降级——拼错的 scope 会把「全公司」当成
// 「仅本人」的数据陈述出去,属数据正确性误导。
func scopeQueryTypeQuery(f map[string]string) (url.Values, error) {
	switch strings.ToLower(f["scope"]) {
	case "", "personal":
		return q("queryType", "0")
	case "all":
		return q("queryType", "1")
	default:
		return nil, fmt.Errorf("apply list-month-flow: --scope must be personal or all, got %q", f["scope"])
	}
}

// ApplyId=<--apply-id>&ApproverNo=<--approver-no>（condolence urge）。
func condolenceUrgeQuery(f map[string]string) (url.Values, error) {
	return q("ApplyId", f["apply-id"], "ApproverNo", f["approver-no"])
}

// ---- body 构造器（严格对齐黄金表各命令的 Body 列）----

// 多数分页接口：{"pageNum":1,"pageSize":10}（--page/--page-size 可覆盖，缺省与官方一致）。
func pagedBody(f map[string]string) map[string]any {
	return map[string]any{"pageNum": atoiDefault(f["page"], 1), "pageSize": atoiDefault(f["page-size"], 10)}
}

// attendance get-effectived-summary：pageSize 官方固定 100。
func pagedBody100(f map[string]string) map[string]any {
	return map[string]any{"pageNum": atoiDefault(f["page"], 1), "pageSize": 100}
}

// vacation list-annual / list-sick：分页键大写 PageIndex/PageSize。
func pagedUpperBody(f map[string]string) map[string]any {
	return map[string]any{"PageIndex": atoiDefault(f["page"], 1), "PageSize": atoiDefault(f["page-size"], 10)}
}

// attendance calc-days：键全大写开头，Begzt/Endzt 为官方原拼写；班次时间官方
// 缺省 08:30:00/18:00:00，--beg-time/--end-time 可覆盖（官方同款 flag，
// 非标准班次下算天才不失真）。
func calcDaysBody(f map[string]string) map[string]any {
	return map[string]any{
		"ApplyType":     f["apply-type"],
		"ApplyTypeText": "",
		"BegDate":       f["beg-date"],
		"Begzt":         firstNonEmpty(f["beg-time"], "08:30:00"),
		"EndDate":       f["end-date"],
		"Endzt":         firstNonEmpty(f["end-time"], "18:00:00"),
	}
}

func todoBody(f map[string]string) map[string]any {
	// flowType 1-4（官方 help：1=待审批 2=已审批 3=我的申请 4=我的委托）；
	// runBusiness 已拒绝 1-4 之外与非法值，这里缺省 1 与官方一致。
	flowType := 1
	if n, err := strconv.Atoi(f["type"]); err == nil && n >= 1 && n <= 4 {
		flowType = n
	}
	return map[string]any{
		"condition": map[string]any{"flowType": flowType},
		"pageNum":   atoiDefault(f["page"], 1),
		"pageSize":  atoiDefault(f["page-size"], 10),
	}
}

func todoDetailBody(f map[string]string) map[string]any {
	return map[string]any{
		"systemId":  f["system-id"],
		"processId": f["process-id"],
		"docunId":   f["docun-id"],
	}
}

// apply list：SearchText 固定 "a"。
func applyListBody(f map[string]string) map[string]any {
	return map[string]any{
		"SearchText": "a",
		"pageNum":    atoiDefault(f["page"], 1),
		"pageSize":   atoiDefault(f["page-size"], 10),
	}
}

// employee-search：condition 键是 keywords 复数。
func employeeSearchBody(f map[string]string) map[string]any {
	return map[string]any{
		"condition": map[string]any{"keywords": f["keyword"]},
		"pageNum":   atoiDefault(f["page"], 1),
		"pageSize":  atoiDefault(f["page-size"], 10),
	}
}

// news：无 keyword 时 condition 为 {"isImage":false,"keyword":""}；--with-image 时 isImage:true。
func newsBody(f map[string]string) map[string]any {
	return map[string]any{
		"condition": map[string]any{"isImage": boolFlag(f, "with-image"), "keyword": f["keyword"]},
		"pageNum":   atoiDefault(f["page"], 1),
		"pageSize":  atoiDefault(f["page-size"], 10),
	}
}

// announcement：type 缺省 2（集团发文）。
func announcementBody(f map[string]string) map[string]any {
	return map[string]any{
		"condition": map[string]any{"keyword": f["keyword"], "type": atoiDefault(f["type"], 2)},
		"pageNum":   atoiDefault(f["page"], 1),
		"pageSize":  atoiDefault(f["page-size"], 10),
	}
}

// union-welfare 对应黄金表 union-welfare list：SearchText 为 --search-text
// （键名与空串缺省已由黄金报文证实；官方 help：按福利名称搜索）。
func unionWelfareBody(f map[string]string) map[string]any {
	return map[string]any{
		"SearchText": f["search-text"],
		"pageNum":    atoiDefault(f["page"], 1),
		"pageSize":   atoiDefault(f["page-size"], 10),
	}
}

// survey submit 对应黄金表 survey submit：键大写开头；1314Salary 类型另有
// EmpName/IDCard/IDCardType（官方 --emp-name/--id-card/--id-card-type，黄金表
// §15 备注），提供 flag 时才附加。
func surveySubmitBody(f map[string]string) map[string]any {
	body := map[string]any{"Id": f["id"], "Opinion": f["opinion"]}
	for key, flag := range map[string]string{
		"EmpName":    "emp-name",
		"IDCard":     "id-card",
		"IDCardType": "id-card-type",
	} {
		if v := f[flag]; v != "" {
			body[key] = v
		}
	}
	return body
}

// todo-batch-list：--system-id 提供时进 condition，否则 condition 为空对象（黄金表 §8）。
func todoBatchListBody(f map[string]string) map[string]any {
	condition := map[string]any{}
	if s := f["system-id"]; s != "" {
		condition["systemId"] = s
	}
	return map[string]any{
		"condition": condition,
		"pageNum":   atoiDefault(f["page"], 1),
		"pageSize":  atoiDefault(f["page-size"], 10),
	}
}

// todo-category：官方只发 pageSize（黄金表 §8）。
func todoCategoryBody(f map[string]string) map[string]any {
	return map[string]any{"pageSize": atoiDefault(f["page-size"], 10)}
}

// todo-submit（写操作）：buttonType 首字母大写；reject 附加 opinion；
// transfer 附加 targetReceiver/targetReceiverName（缺一不可）；flow-list 元素
// 须含 nodeId，且不得为空（null/[] 显式拒绝）。
func todoSubmitBody(f map[string]string) (map[string]any, error) {
	listRaw := f["flow-list"]
	if listRaw == "" {
		return nil, fmt.Errorf("todo-submit: missing --flow-list")
	}
	// 官方同时支持内联 JSON 数组与 ./file.json 路径：以 [ 开头按字面量，否则当路径读。
	// 路径仅限当前目录下的相对常规文件——CLI 运行在模型工具上下文，不能给它
	// 任意文件读取面（绝对路径、.. 越界、FIFO/设备文件都在拒绝之列）。
	if !strings.HasPrefix(listRaw, "[") {
		if filepath.IsAbs(listRaw) || filepath.VolumeName(listRaw) != "" {
			// VolumeName 兜 Windows 盘符相对路径("C:foo" 非 IsAbs,却按 C: 盘
			// 自己的 cwd 解析,会绕开当前目录约束)。
			return nil, fmt.Errorf("todo-submit: --flow-list file must be a relative path under the current directory")
		}
		clean := filepath.Clean(listRaw)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("todo-submit: --flow-list file must stay under the current directory")
		}
		// Windows 无盘符 rooted path("/windows/win.ini"):IsAbs 为 false 且
		// VolumeName 为空,但 os.Open 按当前盘根解析,同样越出 cwd,一并拒绝。
		if os.IsPathSeparator(clean[0]) {
			return nil, fmt.Errorf("todo-submit: --flow-list file must be a relative path under the current directory")
		}
		// EvalSymlinks 归一全部组件后做 cwd 前缀判断:Lstat 只查末级组件,
		// cwd 内的中间目录符号链接(subdir -> /tmp/outside)仍会把读取面导出
		// cwd 外。归一后的真实路径必须仍在 cwd 内。
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("todo-submit: resolve working directory: %w", err)
		}
		wdReal, err := filepath.EvalSymlinks(wd)
		if err != nil {
			return nil, fmt.Errorf("todo-submit: resolve working directory: %w", err)
		}
		real, err := filepath.EvalSymlinks(filepath.Join(wd, clean))
		if err != nil {
			return nil, fmt.Errorf("todo-submit: read --flow-list file: %w", err)
		}
		if real != wdReal && !strings.HasPrefix(real, wdReal+string(filepath.Separator)) {
			return nil, fmt.Errorf("todo-submit: --flow-list file must stay under the current directory after resolving symlinks")
		}
		fi, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("todo-submit: read --flow-list file: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("todo-submit: --flow-list file is not a regular file")
		}
		fh, err := os.Open(real)
		if err != nil {
			return nil, fmt.Errorf("todo-submit: read --flow-list file: %w", err)
		}
		defer fh.Close()
		b, err := io.ReadAll(io.LimitReader(fh, maxRespBytes))
		if err != nil {
			return nil, fmt.Errorf("todo-submit: read --flow-list file: %w", err)
		}
		if len(b) >= maxRespBytes {
			return nil, fmt.Errorf("todo-submit: --flow-list file exceeds %d bytes limit", maxRespBytes)
		}
		listRaw = string(b)
	}
	var flowList []map[string]any
	if err := json.Unmarshal([]byte(listRaw), &flowList); err != nil {
		return nil, fmt.Errorf("todo-submit: --flow-list is not a JSON array: %w", err)
	}
	if len(flowList) == 0 {
		return nil, fmt.Errorf("todo-submit: --flow-list is empty (null or [])")
	}
	for i, item := range flowList {
		if v, ok := item["nodeId"]; !ok || v == nil || v == "" {
			// null/空串同样拒绝:官方要求每个元素携带 nodeId,空值提交没有意义。
			return nil, fmt.Errorf("todo-submit: flow-list[%d] missing nodeId", i)
		}
	}
	// action 白名单:官方仅 agree/reject/transfer。不设白名单时多字节/带尾随
	// 空格的值会拼出脏 buttonType 照发服务端,且 transfer 的必填校验被绕过。
	action := strings.ToLower(strings.TrimSpace(firstNonEmpty(f["action"], "agree")))
	switch action {
	case "agree", "reject", "transfer":
	default:
		return nil, fmt.Errorf("todo-submit: --action must be agree, reject or transfer")
	}
	buttonType := strings.ToUpper(action[:1]) + action[1:]
	body := map[string]any{"buttonType": buttonType, "flowInfoList": flowList}
	switch action {
	case "reject":
		body["opinion"] = f["opinion"]
	case "transfer":
		if f["target-receiver"] == "" || f["target-receiver-name"] == "" {
			return nil, fmt.Errorf("todo-submit: transfer requires --target-receiver and --target-receiver-name")
		}
		body["targetReceiver"] = f["target-receiver"]
		body["targetReceiverName"] = f["target-receiver-name"]
	}
	return body, nil
}

// expense monthly / statistics：{"currencyName":"..."}（黄金表 §13）。
func currencyNameBody(f map[string]string) map[string]any {
	return map[string]any{"currencyName": f["currency-name"]}
}

// message list：condition 无 --type 时为空对象；sortList 恒定 notice_time 倒序（黄金表 §17）。
func messageListBody(f map[string]string) map[string]any {
	condition := map[string]any{}
	if t := f["type"]; t != "" {
		condition["typeList"] = []string{t}
	}
	return map[string]any{
		"condition": condition,
		"pageNum":   atoiDefault(f["page"], 1),
		"pageSize":  atoiDefault(f["page-size"], 10),
		"sortList":  []map[string]any{{"asc": false, "column": "notice_time"}},
	}
}

// condolence validate：Type 为字符串（黄金表 §18）；Vaild 为官方原拼写（路由侧）。
func condolenceValidateBody(f map[string]string) map[string]any {
	return map[string]any{
		"CertificationDate": f["certification-date"],
		"IDCard":            f["id-card"],
		"SpouseIDCard":      f["spouse-id-card"],
		"SpouseName":        f["spouse-name"],
		"Type":              firstNonEmpty(f["type"], "1"),
	}
}

// condolence create-funeral：Type 固定 "3"（黄金表 §18）。
func condolenceFuneralBody(f map[string]string) map[string]any {
	return map[string]any{
		"DirectSupervisor":      f["supervisor"],
		"FuneralDate":           f["funeral-date"],
		"FuneralPersonIdCard":   f["person-id-card"],
		"FuneralPersonName":     f["person-name"],
		"FuneralPersonRelation": f["relation"],
		"IDCard":                f["id-card"],
		"Type":                  "3",
	}
}

// condolence create-wedding：type=1 结婚 / type=2 新生儿（附出生与子女键，黄金表 §18）。
func condolenceWeddingBody(f map[string]string) map[string]any {
	body := map[string]any{
		"CertificationDate": f["certification-date"],
		"IDCard":            f["id-card"],
		"SpouseIDCard":      f["spouse-id-card"],
		"SpouseName":        f["spouse-name"],
		"Type":              firstNonEmpty(f["type"], "1"),
	}
	if body["Type"] == "2" {
		for key, flag := range map[string]string{
			"BirthDate":          "birth-date",
			"BirthType":          "birth-type",
			"SingleOrTwin":       "single-or-twin",
			"FirstChildrenName":  "first-children-name",
			"SecondChildrenName": "second-children-name",
		} {
			if v := f[flag]; v != "" {
				body[key] = v
			}
		}
	}
	return body
}

// ---- RSA date 加密（attendance get-summary / get-calendar 专用，黄金表 §2）----

// 官方 eip-cli 内嵌的 1024 位 SPKI 公钥，从官方 exe 原样提取（私钥在服务端）。
const rsaPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQCghu4j7NRaPkuotWUs/n5adIvOs19GpAfwhs/nTaaYp/paheKNc00Ns7xGbUAJli+Qog8x1iuv/MBoTx7WbjVCz8ZgpqFBb4OkVtG2FRWMkiC1qEsr0Vm1ljHxEW61oDQcI/L66i/BzlMUBIzWC6ogzgqnz5RTGPZKzncofzpqjQIDAQAB
-----END PUBLIC KEY-----`

// 明文 YYYY-MM → PKCS#1 v1.5 加密（128 字节）→ base64 标准编码。
// query 值由 url.Values.Encode 负责 URL encode。
func encryptRSADate(plain string) (string, error) {
	block, _ := pem.Decode([]byte(rsaPublicKeyPEM))
	if block == nil {
		return "", fmt.Errorf("invalid embedded RSA public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("embedded key is not RSA")
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	args = stripGlobalFlags(args)
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return 0
	}
	if args[0] == "version" {
		emitJSON(map[string]any{"name": "eip-cli", "version": "0.1.0-" + runtime.GOOS + "-" + runtime.GOARCH + "-pinvou3"})
		return 0
	}
	if args[0] == "config-get" {
		emitJSON(map[string]any{"authBase": authBase(), "eipBase": eipBase(), "deviceId": deviceID()})
		return 0
	}
	if args[0] == "config" {
		if len(args) > 1 && args[1] == "device-id" {
			fmt.Println(deviceID())
			return 0
		}
		emitJSON(map[string]any{"ok": true, "message": "config set-env is not persisted by this fallback build"})
		return 0
	}
	if args[0] == "auth" {
		return runAuth(args[1:])
	}
	return runBusiness(args)
}

func stripGlobalFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--debug", "--verbose", "--non-interactive":
			continue
		case "--output", "--config":
			i++
			continue
		default:
			if strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "--config=") {
				continue
			}
			out = append(out, a)
		}
	}
	return out
}

func runAuth(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("eip-cli auth <login|poll|save|status|logout>")
		return 0
	}
	flags := parseFlags(args[1:])
	switch args[0] {
	case "login":
		return cmdLogin(flags)
	case "poll":
		return cmdPoll(flags)
	case "save":
		return cmdSave(flags)
	case "status":
		emitJSON(map[string]any{"hasToken": loadAT() != "", "hasAIT": loadAIT() != "", "sessionId": loadSessionID()})
		return 0
	case "logout":
		_ = os.Remove(atFile())
		_ = os.Remove(aitFile())
		_ = os.Remove(sessionFile())
		emitJSON(map[string]any{"ok": true})
		return 0
	default:
		emitJSON(map[string]any{"error": "unknown auth command: " + args[0]})
		return 2
	}
}

func cmdLogin(flags map[string]string) int {
	name := firstNonEmpty(flags["name"], "eip-cli-agent")
	body := map[string]any{
		"deviceId": deviceID(),
		"metaData": map[string]string{"name": name, "os": runtime.GOOS, "arch": runtime.GOARCH, "client": "pinvou3"},
	}
	env, raw, err := postJSON(authBase()+"/api/v2/user/agent/session/login", body)
	if err != nil {
		emitJSON(map[string]any{"error": err.Error()})
		return 1
	}
	d := env.session()
	if d.SessionID != "" {
		// session id 落盘失败不阻断登录（ssoLoginUrl 已取得，poll 可显式传
		// --session-id），仅 stderr 告警。
		if werr := os.WriteFile(sessionFile(), []byte(d.SessionID), 0o600); werr != nil {
			fmt.Fprintf(os.Stderr, "warn: persist session id: %v\n", werr)
		}
	}
	if d.SsoLoginURL == "" {
		emitJSON(map[string]any{"error": "login did not return ssoLoginUrl", "message": firstNonEmpty(env.Msg, env.Message), "raw": string(raw)})
		return 1
	}
	emitJSON(map[string]any{"ssoLoginUrl": d.SsoLoginURL, "sessionId": d.SessionID, "message": "open ssoLoginUrl to finish SSO"})
	if boolFlag(flags, "no-poll") {
		return 0
	}
	return cmdPoll(map[string]string{"session-id": d.SessionID, "timeout": firstNonEmpty(flags["timeout"], "30")})
}

func cmdPoll(flags map[string]string) int {
	sid := firstNonEmpty(flags["session-id"], loadSessionID())
	if sid == "" {
		emitJSON(map[string]any{"error": "missing --session-id"})
		return 2
	}
	// range 校验：超大值会让 time.Duration 乘法回绕、deadline 失效，轮询永不退出。
	// 缺省 30s 与官方 eip-cli 一致（官方 help：auth poll [--timeout 30]）。
	timeout := atoiDefault(flags["timeout"], 30)
	if timeout < 1 || timeout > 300 {
		emitJSON(map[string]any{"error": "--timeout must be 1-300 seconds"})
		return 2
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for {
		env, _, err := getURL(authBase() + "/api/v2/user/agent/session/poll/" + url.PathEscape(sid))
		if err == nil {
			d := env.session()
			if d.Token != "" {
				// 凭证落盘失败必须上抛：否则 poll 报 connected 但凭证未持久化，
				// 后续业务命令全部 not authenticated。
				if err := saveAT(d.Token); err != nil {
					emitJSON(map[string]any{"error": "persist token: " + err.Error()})
					return 1
				}
				if err := saveAIT(d.Ait); err != nil {
					emitJSON(map[string]any{"error": "persist ait: " + err.Error()})
					return 1
				}
				emitJSON(map[string]any{"hasToken": true, "connected": true})
				return 0
			}
		}
		if time.Now().After(deadline) {
			emitJSON(map[string]any{"connected": false, "pending": true})
			return 8
		}
		time.Sleep(2 * time.Second)
	}
}

func cmdSave(flags map[string]string) int {
	if raw := flags["json"]; raw != "" {
		var env envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			emitJSON(map[string]any{"error": err.Error()})
			return 2
		}
		d := env.session()
		flags["token"] = firstNonEmpty(flags["token"], d.Token)
		flags["ait"] = firstNonEmpty(flags["ait"], d.Ait)
	}
	if flags["token"] == "" && flags["ait"] == "" {
		emitJSON(map[string]any{"error": "missing --token or --ait"})
		return 2
	}
	// 退化输入显式拒绝:剥掉 Bearer 后为空的 token 会「报 ok 却没存上」,含控制
	// 字符的 token 会让之后每条业务命令死在非法 header 上——都远比保存失败难排查。
	if flags["token"] != "" {
		tok := stripBearer(flags["token"])
		if tok == "" || strings.EqualFold(tok, "bearer") {
			emitJSON(map[string]any{"error": "auth save: --token is empty after stripping Bearer prefix"})
			return 2
		}
		if strings.ContainsFunc(tok, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			emitJSON(map[string]any{"error": "auth save: --token contains control characters"})
			return 2
		}
		if err := saveAT(tok); err != nil {
			emitJSON(map[string]any{"error": err.Error()})
			return 1
		}
	}
	if flags["ait"] != "" {
		if strings.ContainsFunc(flags["ait"], func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			emitJSON(map[string]any{"error": "auth save: --ait contains control characters"})
			return 2
		}
		if err := saveAIT(flags["ait"]); err != nil {
			emitJSON(map[string]any{"error": err.Error()})
			return 1
		}
	}
	emitJSON(map[string]any{"ok": true})
	return 0
}

// unverifiedFlags：官方命令面有、但黄金报文尚未覆盖其线上键名的 flag（runBusiness
// 对其显式拒绝）。todo --type 走的是已证实映射，不在此列。值用 []string 字面量
// 而非复合字面量，避免 eip.rs collect_fallback_routes 把本表误解析成路由行。
var unverifiedFlags = map[string][]string{
	"todo":       []string{"keyword"},
	"apply list": []string{"type", "status"},
}

// allowedFlags：每条命令的完整 flag 面（官方 help + 黄金表 Query/Body 列消费的
// flag）。未知/拼错的 flag 显式拒绝而非静默吞——「--Date 大写拼错」曾以缺省
// 数据冒充用户要的数据 exit 0。unverifiedFlags 中的 flag 不在此列（先行拦截、
// 报更准确的「未接线」错）。同款 []string 字面量,防 eip.rs 误解析。
var allowedFlags = map[string][]string{
	"attendance get-summary":            []string{"date"},
	"attendance get-calendar":           []string{"date"},
	"attendance get-overview":           []string{"date"},
	"attendance get-annual-days":        nil,
	"attendance get-schedule":           nil,
	"attendance list-shuttle-bus":       nil,
	"attendance list-leave-types":       nil,
	"attendance calc-days":              []string{"apply-type", "beg-date", "end-date", "beg-time", "end-time"},
	"attendance get-annual-calc":        []string{"date"},
	"attendance get-person-summary":     nil,
	"attendance get-effectived-summary": []string{"date", "page"},
	"attendance list-abnormal":          []string{"date", "page", "page-size"},
	"attendance list-forget-card":       []string{"date", "page", "page-size"},
	"attendance list-forget-badge":      []string{"date", "page", "page-size"},
	"attendance list-flex":              []string{"date", "page", "page-size"},
	"leave get-balance":                 nil,
	"leave get-other-balance":           nil,
	"leave get-sick-balance":            nil,
	"leave get-sick-used":               nil,
	"leave get-compassionate-used":      nil,
	"vacation list-annual":              []string{"date", "page", "page-size"},
	"vacation list-sick":                []string{"page", "page-size"},
	"vacation list-compensatory":        []string{"page", "page-size"},
	"vacation get-remaining-hours":      []string{"year", "page", "page-size"},
	"overtime list-types":               nil,
	"overtime get-extra-hours":          nil,
	"overtime get-approving-hours":      nil,
	"overtime get-approved-hours":       nil,
	"overtime get-effectived-hours":     nil,
	"overtime get-limit-hours":          nil,
	"todo":                              []string{"type", "page", "page-size"},
	"todo-count":                        nil,
	"todo-detail":                       []string{"system-id", "process-id", "docun-id"},
	"todo-batch-list":                   []string{"system-id", "page", "page-size"},
	"todo-category":                     []string{"page-size"},
	"todo-submit":                       []string{"action", "opinion", "flow-list", "target-receiver", "target-receiver-name"},
	"apply list":                        []string{"page", "page-size"},
	"apply get-detail":                  []string{"doc-no"},
	"apply list-attachments":            []string{"doc-no"},
	"apply list-month-flow":             []string{"scope", "page", "page-size"},
	"employee-profile":                  nil,
	"employee-contact":                  []string{"account"},
	"employee-special-day":              []string{"date"},
	"employee-handbook sign":            nil,
	"employee-handbook get-status":      nil,
	"employee-search":                   []string{"keyword", "page", "page-size"},
	"app-search":                        []string{"keyword", "page", "page-size"},
	"ics-search":                        []string{"keyword", "page", "page-size"},
	"commercial-insurance":              []string{"type", "year"},
	"commercial-insurance list":         []string{"type", "year"},
	"commercial-insurance list-years":   nil,
	"contract":                          []string{"page", "page-size"},
	"contract list":                     []string{"page", "page-size"},
	"consumption monthly":               []string{"date"},
	"consumption list":                  []string{"date", "page", "page-size"},
	"consumption trend":                 nil,
	"expense recent":                    nil,
	"expense monthly":                   []string{"currency-name"},
	"expense statistics":                []string{"currency-name"},
	"expense currency":                  nil,
	"fuel-card list":                    []string{"page", "page-size"},
	"fuel-card seasons":                 nil,
	"fuel-card apply-records":           nil,
	"union-welfare":                     []string{"search-text", "page", "page-size"},
	"union-welfare list":                []string{"search-text", "page", "page-size"},
	"union-apply join":                  nil,
	"union-apply status":                nil,
	"union-apply get-status":            nil,
	"survey":                            nil,
	"survey list":                       nil,
	"survey detail":                     []string{"id"},
	"survey get":                        []string{"id"},
	"survey submit":                     []string{"id", "opinion", "emp-name", "id-card", "id-card-type"},
	"remote-work":                       nil,
	"remote-work list-records":          nil,
	"remote-work get-allow-check":       nil,
	"remote-work get-detail":            []string{"doc-no"},
	"news":                              []string{"keyword", "with-image", "page", "page-size"},
	"announcement":                      []string{"keyword", "type", "page", "page-size"},
	"announcement-detail":               []string{"code"},
	"course":                            []string{"type", "page", "page-size"},
	"meeting":                           []string{"date"},
	"community-hot":                     []string{"page-size"},
	"message count":                     nil,
	"message list":                      []string{"type", "page", "page-size"},
	"message get":                       []string{"id"},
	"message read-all":                  nil,
	"message send-bless":                []string{"id", "content", "statement"},
	"condolence list":                   []string{"page", "page-size"},
	"condolence validate":               []string{"type", "certification-date", "id-card", "spouse-id-card", "spouse-name"},
	"condolence urge":                   []string{"apply-id", "approver-no"},
	"condolence create-funeral":         []string{"id-card", "person-name", "person-id-card", "relation", "funeral-date", "supervisor"},
	"condolence create-wedding":         []string{"type", "certification-date", "id-card", "spouse-id-card", "spouse-name", "birth-date", "birth-type", "single-or-twin", "first-children-name", "second-children-name"},
}

// requiredFlags：写命令的必填 flag——写请求不应带着空参离开本机（与 todo-submit
// transfer 的必填校验同款先例）。create-wedding type=2 的附加必填在
// checkRequiredFlags 内特判。
func checkRequiredFlags(key string, f map[string]string) error {
	var required []string
	switch key {
	case "survey submit":
		required = []string{"id", "opinion"}
	case "condolence urge":
		required = []string{"apply-id", "approver-no"}
	case "condolence create-funeral":
		required = []string{"id-card", "person-name", "person-id-card", "relation", "funeral-date", "supervisor"}
	case "condolence create-wedding":
		required = []string{"certification-date", "id-card", "spouse-id-card", "spouse-name"}
		if firstNonEmpty(f["type"], "1") == "2" {
			// 新生儿类型:出生与子女信息是申报主体,缺了必被服务端打回。
			required = append(required, "birth-date", "birth-type", "single-or-twin")
		}
	default:
		return nil
	}
	for _, fl := range required {
		if strings.TrimSpace(f[fl]) == "" {
			return fmt.Errorf("%s: missing required flag --%s", key, fl)
		}
	}
	return nil
}

// writeCommands：会改变服务端状态的命令（与 SKILL.md 护栏同款清单）。
// token 过期时刷新凭证但不自动重放——重发可能让服务端重复受理（重复审批/
// 重复发送），提示调用方确认后重试。
var writeCommands = map[string]bool{
	"todo-submit":               true,
	"message read-all":          true,
	"message send-bless":        true,
	"condolence urge":           true,
	"condolence validate":       true,
	"condolence create-funeral": true,
	"condolence create-wedding": true,
	"survey submit":             true,
	"union-apply join":          true,
	"employee-handbook sign":    true,
}

// statusedError 携带 HTTP 状态码的错误：过期判定据此精确识别 401，
// 不对错误字符串做子串匹配（排错页正文里出现"HTTP 401"字样的 500 不再误判）。
type statusedError struct {
	path   string
	status int
	body   string
}

func (e *statusedError) Error() string {
	return fmt.Sprintf("EIP API %s returned HTTP %d: %s", e.path, e.status, e.body)
}

// authExpired 判定响应是否为 token 失效，需刷新凭证。
// 仅认服务端明确的过期信号：业务信封 Code==51，或 HTTP 401（经 statusedError
// 结构化携带）。不按 Msg 子串猜测：业务文案（如「该申请已失效，请勿重复操作」）
// 命中子串时请求实际已成功处理，自动重放会构成重复提交面。
func authExpired(raw []byte, err error) bool {
	var se *statusedError
	if err != nil {
		return errors.As(err, &se) && se.status == http.StatusUnauthorized
	}
	return envelopeCode(raw) == "51"
}

// envelopeCode 提取业务信封 Code（兼容 number/string 形态）；缺失或非 JSON
// 返回空串。
func envelopeCode(raw []byte) string {
	var probe struct {
		Code json.RawMessage `json:"Code"`
	}
	if json.Unmarshal(raw, &probe) != nil || len(probe.Code) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(probe.Code, &s) == nil {
		return s
	}
	return strings.Trim(string(probe.Code), `"`)
}

func runBusiness(args []string) int {
	key := args[0]
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		key = key + " " + rest[0]
		rest = rest[1:]
	}
	r, ok := routes[key]
	if !ok {
		emitJSON(map[string]any{"error": "unsupported EIP command in this fallback build", "command": strings.Join(args, " ")})
		return 2
	}
	token := ensureToken()
	if token == "" {
		emitJSON(map[string]any{"error": "not authenticated", "message": "please connect EIP first"})
		return 3
	}
	flags := parseFlags(rest)
	// --help:打印该命令的 flag 面(allowedFlags 即命令契约)后退出。须在
	// 白名单之前特判,否则 --help 自身会被当未知 flag 拒绝,SKILL.md 教的
	// 「不确定参数时 --help」就成了死路。
	if _, ok := flags["help"]; ok {
		fmt.Printf("eip %s [flags]\n", key)
		if allowed := allowedFlags[key]; len(allowed) > 0 {
			for _, fl := range allowed {
				fmt.Printf("  --%s\n", fl)
			}
		}
		return 0
	}
	// 官方命令面存在、但黄金报文未覆盖其线上键名的 flag：显式拒绝而非静默吞。
	// 静默吞会让「已审批」查询拿回「待审批」缺省数据且无任何报错；拒绝让调用方
	// 大声失败。补抓报文证实键名后按 todo --type 的方式逐个转正（见黄金表 §19）。
	if banned, ok := unverifiedFlags[key]; ok {
		for _, flag := range banned {
			if flags[flag] != "" {
				emitJSON(map[string]any{"error": key + ": --" + flag + " is not wired in this fallback build (official wire format not captured yet); omit it"})
				return 2
			}
		}
	}
	// 未知/拼错的 flag 一律拒绝（官方 Go flag 同款行为）,不放行到构造器——
	// 构造器取不到的 flag 只会被静默吞,以缺省数据冒充用户要的数据。
	if allowed, ok := allowedFlags[key]; ok {
		known := make(map[string]bool, len(allowed))
		for _, fl := range allowed {
			known[fl] = true
		}
		for fl := range flags {
			if !known[fl] {
				emitJSON(map[string]any{"error": key + ": unknown flag --" + fl})
				return 2
			}
		}
	}
	// 写命令必填参数校验:空参照发必被服务端打回,不如在本机大声失败。
	if err := checkRequiredFlags(key, flags); err != nil {
		emitJSON(map[string]any{"error": err.Error()})
		return 2
	}
	// todo --type：官方 help 即以线上键名描述语义（flowType 1=待审批 2=已审批
	// 3=我的申请 4=我的委托），键名与 body 结构已由黄金缺省报文证实，可直接映射；
	// 1-4 之外显式拒绝，不外推。
	if key == "todo" && flags["type"] != "" {
		if n, err := strconv.Atoi(flags["type"]); err != nil || n < 1 || n > 4 {
			emitJSON(map[string]any{"error": "todo: --type must be 1-4 (1=pending, 2=approved, 3=mine, 4=delegated)"})
			return 2
		}
	}
	var query url.Values
	if r.query != nil {
		var err error
		query, err = r.query(flags)
		if err != nil {
			emitJSON(map[string]any{"error": err.Error()})
			return 2
		}
	}
	var body map[string]any
	if r.body != nil {
		body = r.body(flags)
	}
	// todo-submit：body 依赖 --flow-list 解析（JSON 数组或文件路径），失败要显式报错。
	if key == "todo-submit" {
		b, err := todoSubmitBody(flags)
		if err != nil {
			emitJSON(map[string]any{"error": err.Error()})
			return 2
		}
		body = b
	}
	// message send-bless：多请求编排（黄金表 §17）——--statement 先取
	// 消息详情与官方祝福语配置再 POST bless；--content 直接 POST bless。
	if key == "message send-bless" {
		return runSendBless(flags, token)
	}
	path := r.path
	// course 分流（黄金表 §11）：缺省 mine 走 /lms/my/course，--type hot 走
	// /lms/special/course——官方缺省即 mine，勿反。
	if key == "course" && flags["type"] == "hot" {
		path = "/external/external/lms/special/course"
	}
	// 路径参数式命令：employee-contact(--account)、survey detail/get(--id)、
	// message get(--id)、announcement-detail(--code)、meeting(--date 缺省今天)、
	// community-hot(--page-size 缺省 10 进路径)。
	if strings.Contains(path, "%s") {
		fill := firstNonEmpty(flags["account"], flags["id"], flags["template-id"])
		switch key {
		case "meeting":
			fill = firstNonEmpty(flags["date"], todayNow())
		case "community-hot":
			fill = strconv.Itoa(atoiDefault(flags["page-size"], 10))
		case "announcement-detail":
			fill = flags["code"]
		}
		if fill == "" {
			emitJSON(map[string]any{"error": "missing required path parameter"})
			return 2
		}
		path = fmt.Sprintf(path, url.PathEscape(fill))
	}
	raw, err := callEIP(r.method, path, query, body, token)
	// token 过期(服务端 Code 51 / HTTP 401)→ 清 AT、用 AIT 换新。
	// 读命令直接重试一次;写命令绝不自动重放(重复受理风险),刷新后提示重试。
	// 刷新失败、拿回原 token、重试仍过期,都不能把过期信封当成功输出(exit 0
	// 会让模型把「未提交/未查询到」转告成已成功)——统一 exit 3 交还调用方。
	if authExpired(raw, err) {
		_ = os.Remove(atFile())
		nt := ensureToken()
		if writeCommands[key] || nt == "" || nt == token {
			emitJSON(expireReportPayload(nt))
			return 3
		}
		raw, err = callEIP(r.method, path, query, body, nt)
		if authExpired(raw, err) {
			emitJSON(map[string]any{"error": "token expired after refresh and retry", "message": "please reconnect EIP"})
			return 3
		}
	}
	if err != nil {
		emitJSON(map[string]any{"error": err.Error()})
		return 1
	}
	os.Stdout.Write(raw)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		fmt.Println()
	}
	return 0
}

// runSendBless 处理 message send-bless 的多请求编排（黄金表 §17）：
//   - --statement：GET /message/message/{id} 取详情 → GET /message/message/config/statement/1
//     取官方祝福语文案 → POST /message/message/bless 发送；
//   - --content：直接 POST /message/message/bless，body 为 {"id":..,"message":..}。
//
// 已知限制：官方 --statement 会按消息类型选祝福语池（生日/周年/新员工鼓励语，
// 段号运行期计算），黄金抓包只覆盖了段 1 的报文，这里固定取 1——非生日消息
// 可能取到生日池文案（发送本身仍需用户确认，见 SKILL.md 护栏）；补抓各段报文
// 后再对齐。前两步是读操作，但业务错误（HTTP 200 + Code≠1，如消息不存在）
// 必须中止，否则祝福会照发；整个编排属写命令，token 过期不自动重放（见
// writeCommands）。
// expireRefreshAndReport 处理写编排(message send-bless)路径上的 token 过期:
// 刷新凭证但不重放(重复受理风险),统一 exit 3 交还调用方。
func expireRefreshAndReport() int {
	_ = os.Remove(atFile())
	emitJSON(expireReportPayload(ensureToken()))
	return 3
}

// expireReportPayload 按刷新结果区分 exit 3 的提示:刷新成功请重试,刷新失败
// 提示重连——统一报 refreshed 会让 exchange 失败的排障方向跑偏。
func expireReportPayload(refreshedToken string) map[string]any {
	if refreshedToken != "" {
		return map[string]any{"error": "token expired and refreshed", "message": "please retry this command"}
	}
	return map[string]any{"error": "token expired and refresh failed", "message": "please reconnect EIP"}
}

func runSendBless(flags map[string]string, token string) int {
	id := flags["id"]
	if id == "" {
		emitJSON(map[string]any{"error": "message send-bless: missing --id"})
		return 2
	}
	content := flags["content"]
	if content == "" && boolFlag(flags, "statement") {
		detail, err := callEIP("GET", "/message/message/"+url.PathEscape(id), nil, nil, token)
		if authExpired(detail, err) {
			return expireRefreshAndReport()
		}
		if err != nil {
			emitJSON(map[string]any{"error": "fetch message detail: " + err.Error()})
			return 1
		}
		if code := envelopeCode(detail); code != "" && code != "1" {
			emitJSON(map[string]any{"error": "fetch message detail: business error", "code": code})
			return 1
		}
		cfg, err := callEIP("GET", "/message/message/config/statement/1", nil, nil, token)
		if authExpired(cfg, err) {
			return expireRefreshAndReport()
		}
		if err != nil {
			emitJSON(map[string]any{"error": "fetch statement config: " + err.Error()})
			return 1
		}
		if code := envelopeCode(cfg); code != "" && code != "1" {
			emitJSON(map[string]any{"error": "fetch statement config: business error", "code": code})
			return 1
		} else if text := extractStatement(cfg); text != "" {
			content = text
		}
	}
	if content == "" {
		emitJSON(map[string]any{"error": "message send-bless: provide --content or --statement"})
		return 2
	}
	raw, err := callEIP("POST", "/message/message/bless", nil,
		map[string]any{"id": id, "message": content}, token)
	// 编排属写命令:过期只刷新不重放(exit 3),业务拒绝(Code≠1)显式报错——
	// 都不得让「发送未发生」以 exit 0 假冒成功。
	if authExpired(raw, err) {
		return expireRefreshAndReport()
	}
	if err != nil {
		emitJSON(map[string]any{"error": err.Error()})
		return 1
	}
	if code := envelopeCode(raw); code != "" && code != "1" {
		emitJSON(map[string]any{"error": "bless rejected: business error", "code": code})
		return 1
	}
	os.Stdout.Write(raw)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		fmt.Println()
	}
	return 0
}

// extractStatement 从祝福语配置响应里宽容提取文案：Data 为字符串直接用；
// 为对象时依次尝试常见键。取不到返回空串，由调用方回退 --content。
func extractStatement(raw []byte) string {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(env.Data, &text) == nil {
		return text
	}
	var obj map[string]any
	if json.Unmarshal(env.Data, &obj) == nil {
		for _, key := range []string{"content", "statement", "text", "message"} {
			if v, ok := obj[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// callEIP 发业务请求：full = eipBase + path（route 已存黄金表完整路径，前缀错配从结构上消除）。
// GET 参数全部进 query；无 body 的 POST 不发送 body，但保留 Content-Type（官方如此，
// GET 也同样带 Content-Type，见黄金表 §1）。响应体限制在 maxRespBytes 内，错误信息
// 截断——异常服务端不能借超长 body 撑爆内存或污染模型上下文。
func callEIP(method, path string, query url.Values, body map[string]any, token string) ([]byte, error) {
	full := strings.TrimRight(eipBase(), "/") + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reader io.Reader
	if method == "POST" && body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if !strings.HasPrefix(token, "Bearer ") {
		token = "Bearer " + token
	}
	req.Header.Set("Authorization", token)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readBodyCapped(resp)
	if err != nil {
		return raw, err
	}
	if resp.StatusCode >= 400 {
		return raw, &statusedError{path: path, status: resp.StatusCode, body: truncateForError(raw)}
	}
	return raw, nil
}

func authBase() string { return firstNonEmpty(os.Getenv("EIP_AUTH_BASE_URL"), authBaseDefault) }
func eipBase() string  { return firstNonEmpty(os.Getenv("EIP_BASE_URL"), eipBaseDefault) }

// 共享的 http.Client/Transport：复用连接池。每次请求新建 Transport 会把空闲
// 连接遗留给 GC/idle timeout，poll 长轮询（最长 300s / 2s 一次）可持续累积 fd。
var sharedHTTPClient = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil}}

func httpClient() *http.Client { return sharedHTTPClient }

// maxRespBytes 响应体读取上限（2MiB）：EIP JSON 响应远小于此量级，仅防
// 异常服务端返回超大 body。
const maxRespBytes = 2 << 20

// truncateForError 把 body 截到错误信息安全长度，避免整个响应进 error 字符串。
func truncateForError(b []byte) string {
	const max = 512
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

// readBodyCapped 读取响应体并防静默截断：恰好读满上限视为超限报错，而不是把
// 半截 JSON 当完整数据输出（截断的列表/表单会污染模型上下文且被当真）。
func readBodyCapped(resp *http.Response) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return raw, err
	}
	if len(raw) >= maxRespBytes {
		return raw, fmt.Errorf("response body exceeds %d bytes limit", maxRespBytes)
	}
	return raw, nil
}

func postJSON(uri string, body any) (*envelope, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", uri, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	authHeaders(req)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := readBodyCapped(resp)
	if err != nil {
		return nil, raw, err
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return &env, raw, nil
}

func getURL(uri string) (*envelope, []byte, error) {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return nil, nil, err
	}
	authHeaders(req)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := readBodyCapped(resp)
	if err != nil {
		return nil, raw, err
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return &env, raw, nil
}

// authHeaders 认证域公共头（黄金表 §1：Auth-Type 仅 auth base 请求携带；
// login/poll/exchange 均无需 Authorization）。
func authHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Auth-Type", "SSO_NEXT")
	req.Header.Set("User-Agent", userAgent)
}

func (e *envelope) session() sessionData {
	var d sessionData
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &d)
	}
	if d.SessionID == "" {
		d.SessionID = e.SessionID
	}
	if d.SsoLoginURL == "" {
		d.SsoLoginURL = e.SsoLoginURL
	}
	if d.Token == "" {
		d.Token = e.Token
	}
	if d.Ait == "" {
		d.Ait = e.Ait
	}
	return d
}

func deviceID() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_DEVICE_ID")); v != "" {
		return v
	}
	return "eip-cli-default-device"
}

func credDir() string {
	d := os.Getenv("AGENT_CREDENTIALS_DIR")
	if d == "" {
		home, _ := os.UserHomeDir()
		d = filepath.Join(home, ".pinvou3", "eip", "credentials")
	}
	_ = os.MkdirAll(d, 0o700)
	return d
}

func atFile() string      { return filepath.Join(credDir(), "credentials_at.enc") }
func aitFile() string     { return filepath.Join(credDir(), "credentials_ait.enc") }
func sessionFile() string { return filepath.Join(credDir(), "session_id") }

func deriveKey() []byte {
	h := sha256.Sum256([]byte(deviceID() + "|" + credSalt))
	return h[:]
}

func encryptToFile(path, plain string) error {
	block, err := aes.NewCipher(deriveKey())
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return os.WriteFile(path, ct, 0o600)
}

func decryptFromFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("credential too short")
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func loadAT() string        { s, _ := decryptFromFile(atFile()); return s }
func loadAIT() string       { s, _ := decryptFromFile(aitFile()); return s }
func saveAT(v string) error { return encryptToFile(atFile(), stripBearer(v)) }
func saveAIT(v string) error {
	if v == "" {
		return nil
	}
	return encryptToFile(aitFile(), v)
}
func loadSessionID() string {
	b, _ := os.ReadFile(sessionFile())
	return strings.TrimSpace(string(b))
}

func ensureToken() string {
	if t := loadAT(); t != "" {
		return t
	}
	ait := loadAIT()
	if ait == "" {
		return ""
	}
	body := map[string]any{"ait": ait, "deviceId": deviceID()}
	env, _, err := postJSON(authBase()+"/api/v2/user/agent/token/exchange", body)
	if err != nil {
		return ""
	}
	d := env.session()
	if d.Token == "" {
		return ""
	}
	// 落盘失败仅 stderr 告警：下次 ensureToken 发现 AT 为空会用 AIT 重换，
	// 行为自愈，不值得让读路径的业务命令报错。
	if err := saveAT(d.Token); err != nil {
		fmt.Fprintf(os.Stderr, "warn: persist refreshed token: %v\n", err)
	}
	if d.Ait != "" {
		if err := saveAIT(d.Ait); err != nil {
			fmt.Fprintf(os.Stderr, "warn: persist refreshed ait: %v\n", err)
		}
	}
	return d.Token
}

func parseFlags(args []string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		a = strings.TrimPrefix(a, "--")
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			m[a[:eq]] = a[eq+1:]
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			m[a] = args[i+1]
			i++
		} else {
			m[a] = "true"
		}
	}
	return m
}

func boolFlag(m map[string]string, k string) bool {
	v := strings.ToLower(m[k])
	return v == "true" || v == "1" || v == "yes"
}

func atoiDefault(s string, d int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return d
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// stripBearer 剥掉 Bearer 前缀（大小写不敏感、容忍多空格），兼容存量凭证的
// 各种书写形态。
func stripBearer(s string) string {
	for {
		t := strings.TrimSpace(s)
		if len(t) >= 7 && strings.EqualFold(t[:7], "bearer ") {
			s = t[7:]
			continue
		}
		return t
	}
}

func emitJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func printHelp() {
	fmt.Println("EIP CLI (pinvou3 fallback build)")
	fmt.Println("Usage: eip-cli <auth|attendance|leave|vacation|overtime|todo|news|announcement|course|meeting|community-hot|message|condolence|employee-profile|employee-search|expense|remote-work|...> [flags]")
}
