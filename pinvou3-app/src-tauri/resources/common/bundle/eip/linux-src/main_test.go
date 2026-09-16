package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 逐命令线上契约测试：以 docs/eip-official-api-golden.md 为唯一事实来源，
// 对路由表 key 断言 Method/路径/Query/Body。黄金报文由官方 eip-cli 逐命令
// 实测抓包获得（抓包方法见黄金表 §20）；本测试等价于对该文档的可执行固化，
// 防止路由表与官方报文再次漂移（#1119 之前大面积 404/405 的根因即缺乏此类
// 把关：方法全错、端点张冠李戴、前缀错配、缺 RSA 加密）。

type recordedRequest struct {
	method string
	path   string
	query  url.Values
	body   []byte
}

type recordingServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	req []recordedRequest
}

// newRecordingServer 起录制假服务端并准备隔离凭证环境。
// 全部子测试共享同一 server 与请求队列（take 按序取），依赖测试顺序执行——
// 本文件任何测试都不要加 t.Parallel，否则队列会静默错位。
func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rs.mu.Lock()
		rs.req = append(rs.req, recordedRequest{r.Method, r.URL.Path, r.URL.Query(), b})
		rs.mu.Unlock()
		// 祝福语配置返回可提取文案，其余统一回黄金表 §20 的固定信封。
		if strings.Contains(r.URL.Path, "/config/statement/") {
			_, _ = io.WriteString(w, `{"Code":1,"Msg":"ok","Data":{"content":"生日快乐"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"Code":1,"Msg":"ok","Data":{}}`)
	}))
	t.Cleanup(rs.srv.Close)
	t.Setenv("EIP_BASE_URL", rs.srv.URL)
	t.Setenv("EIP_AUTH_BASE_URL", rs.srv.URL+"/auth/v1.0")
	t.Setenv("AGENT_DEVICE_ID", "contract-test-device")
	t.Setenv("AGENT_CREDENTIALS_DIR", t.TempDir())
	if err := saveAT("test-token"); err != nil {
		t.Fatalf("saveAT: %v", err)
	}
	if err := saveAIT("test-ait"); err != nil {
		t.Fatalf("saveAIT: %v", err)
	}
	return rs
}

func (rs *recordingServer) take(t *testing.T) recordedRequest {
	t.Helper()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.req) == 0 {
		t.Fatalf("no request recorded")
	}
	req := rs.req[0]
	rs.req = rs.req[1:]
	return req
}

func (rs *recordingServer) remaining(t *testing.T) int {
	t.Helper()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.req)
}

// routeCase 描述一条命令的期望线上行为。query 为 nil 期望无 query；
// body 为 nil 期望无 body（GET 或无 body 的 POST），非 nil 时做 JSON 语义比较。
type routeCase struct {
	args       []string
	method     string
	path       string
	query      url.Values
	body       any
	paged      bool // 语法糖：{"pageNum":1,"pageSize":10}
	pagedUpper bool // 语法糖：{"PageIndex":1,"PageSize":10}
	paged100   bool // 语法糖：{"pageNum":1,"pageSize":100}
}

func runCase(t *testing.T, rs *recordingServer, tc routeCase) {
	t.Helper()
	if got := runBusiness(tc.args); got != 0 {
		t.Fatalf("runBusiness(%v) = %d", tc.args, got)
	}
	rec := rs.take(t)
	if rec.method != tc.method {
		t.Errorf("method = %s, want %s", rec.method, tc.method)
	}
	if rec.path != tc.path {
		t.Errorf("path = %s, want %s", rec.path, tc.path)
	}
	if tc.query == nil {
		if len(rec.query) != 0 {
			t.Errorf("query = %v, want none", rec.query)
		}
	} else if !reflect.DeepEqual(rec.query, tc.query) {
		t.Errorf("query = %v, want %v", rec.query, tc.query)
	}
	var want any
	switch {
	case tc.body != nil:
		want = tc.body
	case tc.paged:
		want = map[string]any{"pageNum": 1, "pageSize": 10}
	case tc.pagedUpper:
		want = map[string]any{"PageIndex": 1, "PageSize": 10}
	case tc.paged100:
		want = map[string]any{"pageNum": 1, "pageSize": 100}
	}
	if want == nil {
		if len(rec.body) != 0 {
			t.Errorf("body = %s, want none", rec.body)
		}
		return
	}
	var got map[string]any
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Errorf("body not JSON: %q", rec.body)
		return
	}
	wantJSON, _ := json.Marshal(want)
	var wantMap map[string]any
	_ = json.Unmarshal(wantJSON, &wantMap)
	if !reflect.DeepEqual(got, wantMap) {
		t.Errorf("body = %s, want %s", rec.body, wantJSON)
	}
}

func TestBusinessRoutesMatchGoldenReference(t *testing.T) {
	rs := newRecordingServer(t)
	today, month, year := todayNow(), monthNow(), yearNow()

	cases := map[string]routeCase{
		// ---- §4 attendance（15 条；get-summary/get-calendar 含 RSA date，见下方专项测试）----
		"attendance get-overview":     {args: []string{"attendance", "get-overview", "--date", month}, method: "GET", path: "/hrss/api/ApiAttendance/GetEmpMonthSummaryInfo", query: url.Values{"date": {month}}},
		"attendance get-annual-days":  {args: []string{"attendance", "get-annual-days"}, method: "GET", path: "/hrss/api/ApiAttendance/GetAnnualDays"},
		"attendance get-schedule":     {args: []string{"attendance", "get-schedule"}, method: "GET", path: "/hrss/api/ApiAttendance/GetWorkSchduleList", query: url.Values{"applyNo": {""}}},
		"attendance list-shuttle-bus": {args: []string{"attendance", "list-shuttle-bus"}, method: "GET", path: "/hrss/api/ApiAttendance/GetShuttleBusLineList"},
		"attendance list-leave-types": {args: []string{"attendance", "list-leave-types"}, method: "GET", path: "/hrss/api/apiAttendance/GetLeaveApplyTypeList"},
		"attendance calc-days": {args: []string{"attendance", "calc-days", "--beg-date", "15", "--end-date", "16", "--apply-type", "01"}, method: "POST", path: "/hrss/api/ApiAttendance/CalcApplyItemDays",
			query: url.Values{"funcType": {"Leavetime"}},
			body:  map[string]any{"ApplyType": "01", "ApplyTypeText": "", "BegDate": "15", "Begzt": "08:30:00", "EndDate": "16", "Endzt": "18:00:00"}},
		"attendance get-annual-calc": {args: []string{"attendance", "get-annual-calc"}, method: "GET", path: "/hrss/api/ApiAttendance/GetAnnualCaclData", query: url.Values{"caclDate": {today}}},
		"attendance get-person-summary": {args: []string{"attendance", "get-person-summary"}, method: "GET", path: "/hrss/api/ApiAttendance/GetPersonCaclAttendanceModelList",
			query: url.Values{"applyNo": {""}, "flowType": {"a"}}},
		"attendance get-effectived-summary": {args: []string{"attendance", "get-effectived-summary", "--date", month}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetEmpOCLAbnormalData",
			query: url.Values{"Date": {month}}, paged100: true},
		"attendance list-abnormal": {args: []string{"attendance", "list-abnormal", "--date", month}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetAbTimeRecordTable",
			query: url.Values{"Date": {month}}, paged: true},
		"attendance list-forget-card": {args: []string{"attendance", "list-forget-card", "--date", month}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetEmpWSKCS",
			query: url.Values{"Date": {month}}, paged: true},
		"attendance list-forget-badge": {args: []string{"attendance", "list-forget-badge", "--date", month}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetEmpWDKCS",
			query: url.Values{"Date": {month}}, paged: true},
		"attendance list-flex": {args: []string{"attendance", "list-flex", "--date", month}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetEmpTXCS",
			query: url.Values{"Date": {month}}, paged: true},

		// ---- §5 leave（5 条，同一端点）----
		"leave get-balance":            {args: []string{"leave", "get-balance"}, method: "GET", path: "/hrss/api/ApiLeavetime/LeavetimeApply"},
		"leave get-other-balance":      {args: []string{"leave", "get-other-balance"}, method: "GET", path: "/hrss/api/ApiLeavetime/LeavetimeApply"},
		"leave get-sick-balance":       {args: []string{"leave", "get-sick-balance"}, method: "GET", path: "/hrss/api/ApiLeavetime/LeavetimeApply"},
		"leave get-sick-used":          {args: []string{"leave", "get-sick-used"}, method: "GET", path: "/hrss/api/ApiLeavetime/LeavetimeApply"},
		"leave get-compassionate-used": {args: []string{"leave", "get-compassionate-used"}, method: "GET", path: "/hrss/api/ApiLeavetime/LeavetimeApply"},

		// ---- §6 vacation（4 条）----
		"vacation list-annual": {args: []string{"vacation", "list-annual"}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetVacationInfoTableList",
			query: url.Values{"calcDate": {today}, "resignationCode": {""}}, pagedUpper: true},
		"vacation list-sick": {args: []string{"vacation", "list-sick"}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetVacationInfoLeaveTableList", pagedUpper: true},
		"vacation list-compensatory": {args: []string{"vacation", "list-compensatory"}, method: "GET", path: "/hrss/api/ApiSapWorkAttendance/GetWorkVacationLeaveList",
			query: url.Values{"PageIndex": {"1"}, "PageSize": {"10"}}},
		"vacation get-remaining-hours": {args: []string{"vacation", "get-remaining-hours", "--year", year}, method: "POST", path: "/hrss/api/ApiSapWorkAttendance/GetWorkAttendanceTotalTable",
			query: url.Values{"Date": {year}}, paged: true},

		// ---- §7 overtime（6 条，同一端点）----
		"overtime list-types":           {args: []string{"overtime", "list-types"}, method: "GET", path: "/hrss/api/ApiAttendance/GetOvertimeApplyTypeList"},
		"overtime get-extra-hours":      {args: []string{"overtime", "get-extra-hours"}, method: "GET", path: "/hrss/api/ApiOvertime/OvertimeApply"},
		"overtime get-approving-hours":  {args: []string{"overtime", "get-approving-hours"}, method: "GET", path: "/hrss/api/ApiOvertime/OvertimeApply"},
		"overtime get-approved-hours":   {args: []string{"overtime", "get-approved-hours"}, method: "GET", path: "/hrss/api/ApiOvertime/OvertimeApply"},
		"overtime get-effectived-hours": {args: []string{"overtime", "get-effectived-hours"}, method: "GET", path: "/hrss/api/ApiOvertime/OvertimeApply"},
		"overtime get-limit-hours":      {args: []string{"overtime", "get-limit-hours"}, method: "GET", path: "/hrss/api/ApiOvertime/OvertimeApply"},

		// ---- §8 todo（6 条）----
		"todo":        {args: []string{"todo"}, method: "POST", path: "/app/app/workFlow/page", body: map[string]any{"condition": map[string]any{"flowType": 1}, "pageNum": 1, "pageSize": 10}},
		"todo-count":  {args: []string{"todo-count"}, method: "POST", path: "/app/app/workFlow/total/count"},
		"todo-detail": {args: []string{"todo-detail", "--system-id", "S1", "--process-id", "P1", "--docun-id", "D1"}, method: "POST", path: "/external/external/workflow/detail", body: map[string]any{"systemId": "S1", "processId": "P1", "docunId": "D1"}},
		"todo-batch-list": {args: []string{"todo-batch-list", "--system-id", "BPM"}, method: "POST", path: "/external/external/workflow/batch/flow/list",
			body: map[string]any{"condition": map[string]any{"systemId": "BPM"}, "pageNum": 1, "pageSize": 10}},
		"todo-category": {args: []string{"todo-category"}, method: "POST", path: "/external/external/workflow/batch/category/list", body: map[string]any{"pageSize": 10}},
		"todo-submit": {args: []string{"todo-submit", "--action", "agree", "--flow-list", `[{"systemId":"BPM","processId":"P1","docunId":"D1","nodeId":"N1"}]`}, method: "POST", path: "/external/external/workflow/submit",
			body: map[string]any{"buttonType": "Agree", "flowInfoList": []any{map[string]any{"systemId": "BPM", "processId": "P1", "docunId": "D1", "nodeId": "N1"}}}},

		// ---- §9 apply（4 条）----
		"apply list": {args: []string{"apply", "list"}, method: "POST", path: "/hrss/api/ApiAttendance/GetWorkFlowList",
			query: url.Values{"flowCode": {"a"}}, body: map[string]any{"SearchText": "a", "pageNum": 1, "pageSize": 10}},
		"apply get-detail":       {args: []string{"apply", "get-detail", "--doc-no", "DOC001"}, method: "GET", path: "/hrss/api/ApiAttendance/GetApplyWorkFlowInfo", query: url.Values{"docNo": {"DOC001"}}},
		"apply list-attachments": {args: []string{"apply", "list-attachments", "--doc-no", "DOC001"}, method: "GET", path: "/hrss/api/ApiAttendance/GetApplyBillAttachFileList", query: url.Values{"docNo": {"DOC001"}}},
		"apply list-month-flow": {args: []string{"apply", "list-month-flow"}, method: "POST", path: "/hrss/api/ApiCalendarMonthly/ApplyFlowPageList",
			query: url.Values{"queryType": {"0"}}, paged: true},

		// ---- §10 employee / app-search / ics-search（8 条）----
		"employee-profile":             {args: []string{"employee-profile"}, method: "GET", path: "/organisation/organisation/user/info"},
		"employee-search":              {args: []string{"employee-search", "--keyword", "test"}, method: "POST", path: "/organisation/organisation/user/page", body: map[string]any{"condition": map[string]any{"keywords": "test"}, "pageNum": 1, "pageSize": 10}},
		"employee-contact":             {args: []string{"employee-contact", "--account", "l36908"}, method: "GET", path: "/organisation/user/tel/info/l36908"},
		"employee-special-day":         {args: []string{"employee-special-day"}, method: "GET", path: "/organisation/organisation/user/specialDay"},
		"employee-handbook get-status": {args: []string{"employee-handbook", "get-status"}, method: "GET", path: "/hrss/api/ApiStaffNavigation/NewStaffStatus"},
		"employee-handbook sign":       {args: []string{"employee-handbook", "sign"}, method: "GET", path: "/hrss/api/ApiStaffNavigation/SignForReceipt", query: url.Values{"type": {"1"}}},
		"app-search":                   {args: []string{"app-search", "--keyword", "请假"}, method: "POST", path: "/app/app/appPool/page", body: map[string]any{"condition": map[string]any{"keywords": "请假"}, "pageNum": 1, "pageSize": 10}},
		"ics-search":                   {args: []string{"ics-search", "--keyword", "报销"}, method: "POST", path: "/external/external/ics/page", body: map[string]any{"condition": map[string]any{"keywords": "报销"}, "pageNum": 1, "pageSize": 10}},

		// ---- §11 资讯/公告/学习（7 条）----
		"news":                {args: []string{"news"}, method: "POST", path: "/app/app/companyNews/page", body: map[string]any{"condition": map[string]any{"isImage": false, "keyword": ""}, "pageNum": 1, "pageSize": 10}},
		"announcement":        {args: []string{"announcement"}, method: "POST", path: "/announcement/announcement/page", body: map[string]any{"condition": map[string]any{"keyword": "", "type": 2}, "pageNum": 1, "pageSize": 10}},
		"announcement-detail": {args: []string{"announcement-detail", "--code", "GG2026001"}, method: "GET", path: "/announcement/announcement/GG2026001"},
		"course":              {args: []string{"course"}, method: "POST", path: "/external/external/lms/my/course", paged: true},
		"meeting":             {args: []string{"meeting"}, method: "GET", path: "/external/external/meeting/list/" + today},
		"community-hot":       {args: []string{"community-hot"}, method: "GET", path: "/external/external/bbs/list/10"},

		// ---- §12 consumption（3 条）----
		"consumption monthly": {args: []string{"consumption", "monthly", "--date", month}, method: "GET", path: "/hrss/api/ApiMyConsumption/MyConsumptionMonthly", query: url.Values{"Date": {month}}},
		"consumption list": {args: []string{"consumption", "list"}, method: "GET", path: "/hrss/api/ApiMyConsumption/MyConsumptionPageList",
			query: url.Values{"Date": {strings.ReplaceAll(today, "-", "/")}, "PageCount": {"10"}, "PageIndex": {"1"}}},
		"consumption trend": {args: []string{"consumption", "trend"}, method: "GET", path: "/hrss/api/ApiMyConsumption/MyConsumptionMonthlyList"},

		// ---- §13 expense（4 条）----
		"expense recent":     {args: []string{"expense", "recent"}, method: "GET", path: "/external/external/sse/recent"},
		"expense monthly":    {args: []string{"expense", "monthly"}, method: "POST", path: "/external/external/sse/statistics/month", body: map[string]any{"currencyName": ""}},
		"expense statistics": {args: []string{"expense", "statistics"}, method: "POST", path: "/external/external/sse/statistics/status", body: map[string]any{"currencyName": ""}},
		"expense currency":   {args: []string{"expense", "currency"}, method: "GET", path: "/external/external/sse/currency"},

		// ---- §14 福利/油卡/合同/保险（10 条）----
		"fuel-card list":          {args: []string{"fuel-card", "list"}, method: "POST", path: "/hrss/api/ApiBenefits/MyOilCardPageList", paged: true},
		"fuel-card seasons":       {args: []string{"fuel-card", "seasons"}, method: "GET", path: "/hrss/api/ApiBenefits/OilCardInvestSeasonly"},
		"fuel-card apply-records": {args: []string{"fuel-card", "apply-records"}, method: "GET", path: "/hrss/api/ApiBenefits/OilCardApplyRecords"},
		"contract list":           {args: []string{"contract", "list"}, method: "POST", path: "/hrss/api/ApiBenefits/MyContractPageList", paged: true},
		"commercial-insurance list": {args: []string{"commercial-insurance", "list", "--year", year}, method: "GET", path: "/hrss/api/ApiStaffNavigation/GetMyInsuranceList",
			query: url.Values{"type": {"1"}, "year": {year}}},
		"commercial-insurance list-years": {args: []string{"commercial-insurance", "list-years"}, method: "GET", path: "/hrss/api/ApiStaffNavigation/GetAllInsuranceYear"},
		"union-welfare list":              {args: []string{"union-welfare", "list"}, method: "POST", path: "/hrss/api/ApiBenefits/MyUnionWelfarePageList", body: map[string]any{"SearchText": "", "pageNum": 1, "pageSize": 10}},
		"union-apply get-status":          {args: []string{"union-apply", "get-status"}, method: "GET", path: "/hrss/api/ApiStaffNavigation/UnionApply"},
		"union-apply join":                {args: []string{"union-apply", "join"}, method: "POST", path: "/hrss/api/ApiStaffNavigation/JoinUnion"},
		"survey list":                     {args: []string{"survey", "list"}, method: "GET", path: "/hrss/api/ApiOpinionTemplate/GetOpenOpinionTemplateList"},

		// ---- §15 survey（2 条）----
		"survey get":    {args: []string{"survey", "get", "--id", "T1"}, method: "GET", path: "/hrss/api/ApiOpinionTemplate/OpinionCollection/T1"},
		"survey submit": {args: []string{"survey", "submit", "--id", "T1", "--opinion", "同意"}, method: "POST", path: "/hrss/api/ApiOpinionTemplate/SaveOpinionCollectionInfo", body: map[string]any{"Id": "T1", "Opinion": "同意"}},

		// ---- §16 remote-work（3 条，唯一 /hr 组）----
		"remote-work get-allow-check": {args: []string{"remote-work", "get-allow-check"}, method: "GET", path: "/hr/tempHomeWork/allowInitiate"},
		"remote-work list-records":    {args: []string{"remote-work", "list-records"}, method: "GET", path: "/hr/tempHomeWork/recordInfoList"},
		"remote-work get-detail":      {args: []string{"remote-work", "get-detail", "--doc-no", "RW001"}, method: "GET", path: "/hr/tempHomeWork/mainDocModel", query: url.Values{"docNo": {"RW001"}}},

		// ---- §17 message（5 条）----
		"message count": {args: []string{"message", "count"}, method: "GET", path: "/message/message/readFlag"},
		"message list --type": {args: []string{"message", "list", "--type", "SYSTEM"}, method: "POST", path: "/message/message/page",
			body: map[string]any{"condition": map[string]any{"typeList": []any{"SYSTEM"}}, "pageNum": 1, "pageSize": 10, "sortList": []any{map[string]any{"asc": false, "column": "notice_time"}}}},
		"message list": {args: []string{"message", "list"}, method: "POST", path: "/message/message/page",
			body: map[string]any{"condition": map[string]any{}, "pageNum": 1, "pageSize": 10, "sortList": []any{map[string]any{"asc": false, "column": "notice_time"}}}},
		"message get":                  {args: []string{"message", "get", "--id", "M1"}, method: "GET", path: "/message/message/M1"},
		"message read-all":             {args: []string{"message", "read-all"}, method: "POST", path: "/message/message/read/all"},
		"message send-bless --content": {args: []string{"message", "send-bless", "--id", "M1", "--content", "生日快乐"}, method: "POST", path: "/message/message/bless", body: map[string]any{"id": "M1", "message": "生日快乐"}},

		// ---- §18 condolence（5 条）----
		"condolence list": {args: []string{"condolence", "list"}, method: "POST", path: "/hrss/api/ApiBenefits/CondolenceDeclarationPageList", paged: true},
		"condolence validate": {args: []string{"condolence", "validate", "--type", "1", "--certification-date", "2026-01-01", "--id-card", "X1", "--spouse-id-card", "Y1", "--spouse-name", "Z1"}, method: "POST", path: "/hrss/api/ApiBenefits/VaildCondolenceDeclarationApply",
			body: map[string]any{"CertificationDate": "2026-01-01", "IDCard": "X1", "SpouseIDCard": "Y1", "SpouseName": "Z1", "Type": "1"}},
		"condolence urge": {args: []string{"condolence", "urge", "--apply-id", "A1", "--approver-no", "B1"}, method: "GET", path: "/hrss/api/ApiBenefits/CondolenceDeclarationMailUrging",
			query: url.Values{"ApplyId": {"A1"}, "ApproverNo": {"B1"}}},
		"condolence create-funeral": {args: []string{"condolence", "create-funeral", "--id-card", "X1", "--person-name", "Z1", "--person-id-card", "Y1", "--relation", "R1", "--funeral-date", "2026-01-01", "--supervisor", "S1"}, method: "POST", path: "/hrss/api/ApiBenefits/FuneralInfoApply",
			body: map[string]any{"DirectSupervisor": "S1", "FuneralDate": "2026-01-01", "FuneralPersonIdCard": "Y1", "FuneralPersonName": "Z1", "FuneralPersonRelation": "R1", "IDCard": "X1", "Type": "3"}},
		"condolence create-wedding": {args: []string{"condolence", "create-wedding", "--type", "1", "--certification-date", "2026-01-01", "--id-card", "X1", "--spouse-id-card", "Y1", "--spouse-name", "Z1"}, method: "POST", path: "/hrss/api/ApiBenefits/WeddingFuneralInfoApply",
			body: map[string]any{"CertificationDate": "2026-01-01", "IDCard": "X1", "SpouseIDCard": "Y1", "SpouseName": "Z1", "Type": "1"}},

		// ---- 既有兼容别名与分流/可选参数（行为不变性把关）----
		"commercial-insurance":        {args: []string{"commercial-insurance"}, method: "GET", path: "/hrss/api/ApiStaffNavigation/GetMyInsuranceList", query: url.Values{"type": {"1"}, "year": {year}}},
		"contract":                    {args: []string{"contract"}, method: "POST", path: "/hrss/api/ApiBenefits/MyContractPageList", paged: true},
		"union-welfare":               {args: []string{"union-welfare"}, method: "POST", path: "/hrss/api/ApiBenefits/MyUnionWelfarePageList", body: map[string]any{"SearchText": "", "pageNum": 1, "pageSize": 10}},
		"union-apply status":          {args: []string{"union-apply", "status"}, method: "GET", path: "/hrss/api/ApiStaffNavigation/UnionApply"},
		"survey":                      {args: []string{"survey"}, method: "GET", path: "/hrss/api/ApiOpinionTemplate/GetOpenOpinionTemplateList"},
		"survey detail":               {args: []string{"survey", "detail", "--id", "T1"}, method: "GET", path: "/hrss/api/ApiOpinionTemplate/OpinionCollection/T1"},
		"remote-work":                 {args: []string{"remote-work"}, method: "GET", path: "/hr/tempHomeWork/recordInfoList"},
		"course --type hot":           {args: []string{"course", "--type", "hot"}, method: "POST", path: "/external/external/lms/special/course", paged: true},
		"news --keyword --image":      {args: []string{"news", "--keyword", "测试", "--with-image"}, method: "POST", path: "/app/app/companyNews/page", body: map[string]any{"condition": map[string]any{"isImage": true, "keyword": "测试"}, "pageNum": 1, "pageSize": 10}},
		"announcement --type":         {args: []string{"announcement", "--type", "1", "--keyword", "测试"}, method: "POST", path: "/announcement/announcement/page", body: map[string]any{"condition": map[string]any{"keyword": "测试", "type": 1}, "pageNum": 1, "pageSize": 10}},
		"apply list-month-flow all":   {args: []string{"apply", "list-month-flow", "--scope", "all"}, method: "POST", path: "/hrss/api/ApiCalendarMonthly/ApplyFlowPageList", query: url.Values{"queryType": {"1"}}, paged: true},
		"employee-special-day --date": {args: []string{"employee-special-day", "--date", today}, method: "GET", path: "/organisation/organisation/user/specialDay", query: url.Values{"date": {today}}},
		"todo-batch-list no-system":   {args: []string{"todo-batch-list"}, method: "POST", path: "/external/external/workflow/batch/flow/list", body: map[string]any{"condition": map[string]any{}, "pageNum": 1, "pageSize": 10}},
		"todo-submit reject": {args: []string{"todo-submit", "--action", "reject", "--opinion", "不同意", "--flow-list", `[{"systemId":"BPM","processId":"P1","docunId":"D1","nodeId":"N1"}]`}, method: "POST", path: "/external/external/workflow/submit",
			body: map[string]any{"buttonType": "Reject", "opinion": "不同意", "flowInfoList": []any{map[string]any{"systemId": "BPM", "processId": "P1", "docunId": "D1", "nodeId": "N1"}}}},
		"todo-submit transfer": {args: []string{"todo-submit", "--action", "transfer", "--target-receiver", "z12345", "--target-receiver-name", "张三", "--flow-list", `[{"systemId":"BPM","processId":"P1","docunId":"D1","nodeId":"N1"}]`}, method: "POST", path: "/external/external/workflow/submit",
			body: map[string]any{"buttonType": "Transfer", "targetReceiver": "z12345", "targetReceiverName": "张三", "flowInfoList": []any{map[string]any{"systemId": "BPM", "processId": "P1", "docunId": "D1", "nodeId": "N1"}}}},
		"condolence create-wedding type2": {args: []string{"condolence", "create-wedding", "--type", "2", "--certification-date", "2026-01-01", "--id-card", "X1", "--spouse-id-card", "Y1", "--spouse-name", "Z1", "--birth-date", "2024-06-01", "--birth-type", "1", "--single-or-twin", "1", "--first-children-name", "C1"}, method: "POST", path: "/hrss/api/ApiBenefits/WeddingFuneralInfoApply",
			body: map[string]any{"CertificationDate": "2026-01-01", "IDCard": "X1", "SpouseIDCard": "Y1", "SpouseName": "Z1", "Type": "2", "BirthDate": "2024-06-01", "BirthType": "1", "SingleOrTwin": "1", "FirstChildrenName": "C1"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { runCase(t, rs, tc) })
	}
	if n := rs.remaining(t); n != 0 {
		t.Fatalf("%d requests left over", n)
	}
}

// get-summary / get-calendar 的 date 是 RSA(PKCS#1 v1.5)+base64（黄金表 §2），
// 密文每次随机，只断言形态：128 字节密文、标准 base64、不含明文月份。
func TestRSADateQueries(t *testing.T) {
	rs := newRecordingServer(t)
	for _, cmd := range []string{"get-summary", "get-calendar"} {
		args := []string{"attendance", cmd, "--date", monthNow()}
		if got := runBusiness(args); got != 0 {
			t.Fatalf("runBusiness(%v) = %d", args, got)
		}
		rec := rs.take(t)
		enc := rec.query.Get("date")
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("%s: date not base64: %v (%q)", cmd, err, enc)
		}
		if len(raw) != 128 {
			t.Errorf("%s: RSA ciphertext = %d bytes, want 128", cmd, len(raw))
		}
		if strings.Contains(enc, monthNow()) {
			t.Errorf("%s: date leaked plaintext month", cmd)
		}
		if rec.method != "GET" || rec.path != "/hrss/api/ApiAttendance/GetEmpCalendarSummaryInfo" && rec.path != "/hrss/api/ApiAttendance/GetEmpCalendarDetailInfo" {
			t.Errorf("%s: unexpected %s %s", cmd, rec.method, rec.path)
		}
	}
}

func TestMessageSendBlessStatementFlow(t *testing.T) {
	rs := newRecordingServer(t)
	args := []string{"message", "send-bless", "--id", "M1", "--statement"}
	if got := runBusiness(args); got != 0 {
		t.Fatalf("runBusiness(%v) = %d", args, got)
	}
	detail := rs.take(t)
	if detail.method != "GET" || detail.path != "/message/message/M1" {
		t.Errorf("step1 = %s %s, want GET /message/message/M1", detail.method, detail.path)
	}
	config := rs.take(t)
	if config.method != "GET" || config.path != "/message/message/config/statement/1" {
		t.Errorf("step2 = %s %s, want GET /message/message/config/statement/1", config.method, config.path)
	}
	bless := rs.take(t)
	if bless.method != "POST" || bless.path != "/message/message/bless" {
		t.Errorf("step3 = %s %s, want POST /message/message/bless", bless.method, bless.path)
	}
	var body map[string]any
	if err := json.Unmarshal(bless.body, &body); err != nil {
		t.Fatalf("bless body not JSON: %q", bless.body)
	}
	want := map[string]any{"id": "M1", "message": "生日快乐"}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("bless body = %s, want %v", bless.body, want)
	}
	if n := rs.remaining(t); n != 0 {
		t.Fatalf("%d requests left over", n)
	}
}

func TestTodoSubmitFlowListFileAndValidation(t *testing.T) {
	// flow-list 文件仅认当前目录下的相对常规文件;本测试 chdir 到临时目录,
	// 进程 cwd 是共享的——不要给本函数加 t.Parallel。
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	list := `[{"systemId":"BPM","processId":"P1","docunId":"D1","nodeId":"N1"}]`
	if err := os.WriteFile("flows.json", []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := todoSubmitBody(map[string]string{"action": "agree", "flow-list": "./flows.json"})
	if err != nil {
		t.Fatalf("file mode: %v", err)
	}
	if body["buttonType"] != "Agree" {
		t.Errorf("file mode buttonType = %v", body["buttonType"])
	}
	if _, err := todoSubmitBody(map[string]string{"action": "agree", "flow-list": `[{"systemId":"BPM"}]`}); err == nil {
		t.Error("missing nodeId: want error, got nil")
	}
	if _, err := todoSubmitBody(map[string]string{"action": "agree"}); err == nil {
		t.Error("missing --flow-list: want error, got nil")
	}
	// 路径约束:绝对路径与 .. 越界都拒绝（CLI 运行在模型工具上下文，
	// 不给任意文件读取面）。
	if _, err := todoSubmitBody(map[string]string{"action": "agree", "flow-list": filepath.Join(dir, "flows.json")}); err == nil {
		t.Error("absolute path: want error, got nil")
	}
	if _, err := todoSubmitBody(map[string]string{"action": "agree", "flow-list": "../flows.json"}); err == nil {
		t.Error("parent-escaping path: want error, got nil")
	}
	// 目录不是常规文件（顺带覆盖 FIFO/设备文件分支）。
	if _, err := todoSubmitBody(map[string]string{"action": "agree", "flow-list": "."}); err == nil {
		t.Error("directory path: want error, got nil")
	}
	// 空列表:null 字面量（按路径读必然失败）与 [] 都显式拒绝,不发空 flowInfoList。
	if _, err := todoSubmitBody(map[string]string{"action": "agree", "flow-list": `null`}); err == nil {
		t.Error("null literal: want error, got nil")
	}
	if _, err := todoSubmitBody(map[string]string{"action": "agree", "flow-list": `[]`}); err == nil {
		t.Error("empty array: want error, got nil")
	}
	// transfer 缺接收人参数必须显式报错,不得空串照发。
	if _, err := todoSubmitBody(map[string]string{"action": "transfer", "flow-list": list}); err == nil {
		t.Error("transfer without receiver: want error, got nil")
	}
}

// token 过期重试语义（评审二轮 #2）:过期信号只认 Code==51 / HTTP 401;
// 读命令 exchange 后自动重试一次,写命令绝不自动重放。
func TestTokenExpiryRetrySemantics(t *testing.T) {
	var mu sync.Mutex
	var business, exchanged int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token/exchange") {
			mu.Lock()
			exchanged++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"data":{"token":"fresh-at"}}`)
			return
		}
		mu.Lock()
		business++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"Code":51,"Msg":"Token已失效"}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("EIP_BASE_URL", srv.URL)
	t.Setenv("EIP_AUTH_BASE_URL", srv.URL+"/auth/v1.0")
	t.Setenv("AGENT_DEVICE_ID", "contract-test-device")
	t.Setenv("AGENT_CREDENTIALS_DIR", t.TempDir())
	if err := saveAT("test-token"); err != nil {
		t.Fatal(err)
	}
	if err := saveAIT("test-ait"); err != nil {
		t.Fatal(err)
	}

	// 读命令:Code 51 → 删 AT、exchange 换新、重试一次;第二次仍 51 则原样输出。
	if got := runBusiness([]string{"todo"}); got != 0 {
		t.Fatalf("read retry exit = %d, want 0", got)
	}
	mu.Lock()
	b, e := business, exchanged
	mu.Unlock()
	if b != 2 || e != 1 {
		t.Fatalf("read: business=%d exchanged=%d, want 2/1", b, e)
	}

	// 写命令:Code 51 → exchange 后不重放,显式要求重试(exit 3)。
	mu.Lock()
	business, exchanged = 0, 0
	mu.Unlock()
	if err := saveAT("test-token"); err != nil {
		t.Fatal(err)
	}
	if got := runBusiness([]string{"todo-submit", "--flow-list", `[{"nodeId":"N1"}]`}); got != 3 {
		t.Fatalf("write no-replay exit = %d, want 3", got)
	}
	mu.Lock()
	b, e = business, exchanged
	mu.Unlock()
	if b != 1 || e != 1 {
		t.Fatalf("write: business=%d exchanged=%d, want 1/1", b, e)
	}
}

// authExpired 只认显式过期信号:业务文案含「失效」但 Code≠51 时不判过期
// (此前会误删 AT 并自动重放已成功处理的写请求,评审二轮实测复现双发)。
func TestAuthExpiredOnlyTrustsExplicitExpiry(t *testing.T) {
	if authExpired([]byte(`{"Code":1,"Msg":"该申请已失效,请勿重复操作"}`), nil) {
		t.Error("Code=1 with misleading Msg must not count as expired")
	}
	if !authExpired([]byte(`{"Code":51,"Msg":"Token已失效"}`), nil) {
		t.Error("Code=51 must count as expired")
	}
	if !authExpired(nil, fmt.Errorf("EIP API /x returned HTTP 401: denied")) {
		t.Error("HTTP 401 must count as expired")
	}
	if authExpired([]byte(`{"Code":404,"Msg":"not found"}`), nil) {
		t.Error("Code=404 must not count as expired")
	}
	if authExpired([]byte(`<html>Token已失效</html>`), nil) {
		t.Error("non-JSON body must not count as expired")
	}
}

func TestEnvelopeCode(t *testing.T) {
	for raw, want := range map[string]string{
		`{"Code":51}`:   "51",
		`{"Code":"51"}`: "51",
		`{"code":1}`:    "1",
		`{"msg":"x"}`:   "",
		`not json`:      "",
		`{"Code":null}`: "",
	} {
		if got := envelopeCode([]byte(raw)); got != want {
			t.Errorf("envelopeCode(%s) = %q, want %q", raw, got, want)
		}
	}
}

// send-bless 的前置读步骤遇到业务错误(HTTP 200 + Code≠1)必须中止,祝福不得照发。
func TestSendBlessStopsOnBusinessError(t *testing.T) {
	var mu sync.Mutex
	var bless int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bless") {
			mu.Lock()
			bless++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"Code":1}`)
			return
		}
		_, _ = io.WriteString(w, `{"Code":404,"Msg":"消息不存在"}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("EIP_BASE_URL", srv.URL)
	t.Setenv("EIP_AUTH_BASE_URL", srv.URL+"/auth/v1.0")
	t.Setenv("AGENT_DEVICE_ID", "contract-test-device")
	t.Setenv("AGENT_CREDENTIALS_DIR", t.TempDir())
	if err := saveAT("test-token"); err != nil {
		t.Fatal(err)
	}
	if got := runBusiness([]string{"message", "send-bless", "--id", "M1", "--statement"}); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if bless != 0 {
		t.Fatalf("bless sent %d times after business error, want 0", bless)
	}
}

// auth poll --timeout 超界报用法错误:超大值曾使 Duration 乘法回绕、
// deadline 失效,轮询永不退出。
func TestCmdPollTimeoutRange(t *testing.T) {
	t.Setenv("AGENT_CREDENTIALS_DIR", t.TempDir())
	if got := cmdPoll(map[string]string{"session-id": "S", "timeout": "99999999999999"}); got != 2 {
		t.Fatalf("oversized timeout exit = %d, want 2", got)
	}
	if got := cmdPoll(map[string]string{"session-id": "S", "timeout": "301"}); got != 2 {
		t.Fatalf("timeout>300 exit = %d, want 2", got)
	}
}

// poll 的凭证落盘失败必须上抛:否则 poll 报 connected 但凭证未持久化,
// 后续业务命令全部 not authenticated。
func TestCmdPollSurfacesCredentialWriteFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"token":"t1","ait":"a1"}}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("EIP_AUTH_BASE_URL", srv.URL)
	// 凭证目录被同名文件占据:MkdirAll/WriteFile 都会失败(ENOTDIR)。
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_CREDENTIALS_DIR", blocker)
	if got := cmdPoll(map[string]string{"session-id": "S"}); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}
}

// survey submit 的 1314Salary 附加键(官方 --emp-name/--id-card/--id-card-type,
// 黄金表 §15 备注):提供 flag 才附加。
func TestSurveySubmitExtraKeys(t *testing.T) {
	rs := newRecordingServer(t)
	if got := runBusiness([]string{"survey", "submit", "--id", "T1", "--opinion", "同意",
		"--emp-name", "张三", "--id-card", "330106", "--id-card-type", "1"}); got != 0 {
		t.Fatalf("exit = %d", got)
	}
	rec := rs.take(t)
	var body map[string]any
	if err := json.Unmarshal(rec.body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"Id": "T1", "Opinion": "同意", "EmpName": "张三", "IDCard": "330106", "IDCardType": "1"}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %s, want %v", rec.body, want)
	}
}

// calc-days 班次时间可用 --beg-time/--end-time 覆盖(缺省仍为官方 08:30/18:00)。
func TestCalcDaysShiftOverride(t *testing.T) {
	rs := newRecordingServer(t)
	if got := runBusiness([]string{"attendance", "calc-days", "--beg-date", "15", "--end-date", "16",
		"--apply-type", "01", "--beg-time", "09:00:00", "--end-time", "17:30:00"}); got != 0 {
		t.Fatalf("exit = %d", got)
	}
	rec := rs.take(t)
	var body map[string]any
	if err := json.Unmarshal(rec.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["Begzt"] != "09:00:00" || body["Endzt"] != "17:30:00" {
		t.Errorf("shift override = %v/%v, want 09:00:00/17:30:00", body["Begzt"], body["Endzt"])
	}
}

// RSA 加密的 date 参数本地先校验 YYYY-MM,非法输入直接报错不发包。
func TestRSADateRejectsBadFormat(t *testing.T) {
	newRecordingServer(t)
	if got := runBusiness([]string{"attendance", "get-summary", "--date", "2026-13-01"}); got != 1 {
		t.Fatalf("exit = %d, want 1", got)
	}
	if got := runBusiness([]string{"attendance", "get-calendar", "--date", "2026/09"}); got != 1 {
		t.Fatalf("slash format exit = %d, want 1", got)
	}
}

// stripBearer 兼容大小写与多空格的存量凭证书写形态。
func TestStripBearerVariants(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer x":    "x",
		"bearer x":    "x",
		"BEARER   x ": "x",
		" Bearer x ":  "x",
		"plain":       "plain",
		"":            "",
	} {
		if got := stripBearer(in); got != want {
			t.Errorf("stripBearer(%q) = %q, want %q", in, got, want)
		}
	}
}
