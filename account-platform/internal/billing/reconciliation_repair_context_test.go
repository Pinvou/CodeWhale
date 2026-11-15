package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReconciliationRepairSourceAmountDoesNotInventZero(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		available bool
		amount    int64
	}{
		{`{"amount_fen":9007199254740993,"currency":"CNY","channel_transaction_id":"source-tx"}`, true, 9007199254740993},
		{`{"amount_fen":9223372036854775807}`, true, 9223372036854775807},
		{`{"amount_fen":-9223372036854775808}`, true, -9223372036854775808},
		{`{"amount_fen":0}`, true, 0},
		{`{}`, false, 0}, {`null`, false, 0}, {`{"amount_fen":null}`, false, 0},
		{`{"amount_fen":9223372036854775808}`, false, 0},
		{`{"amount_fen":1.5}`, false, 0}, {`{"amount_fen":"9007199254740993"}`, false, 0},
	} {
		amount, _, _, available := reconciliationRepairSnapshotContext([]byte(tc.raw))
		if available != tc.available || available && (amount == nil || *amount != tc.amount) || !available && amount != nil {
			t.Fatalf("source %s: amount=%v available=%v", tc.raw, amount, available)
		}
	}
	_, currency, transaction, ok := reconciliationRepairSnapshotContext([]byte(`{"amount_fen":9007199254740993,"currency":"CNY","channel_transaction_id":"source-tx"}`))
	if !ok || currency != "CNY" || transaction != "source-tx" {
		t.Fatal("source metadata changed")
	}
}

type repairContextReadStub struct {
	Repository
	item   ReconciliationRepairContext
	err    error
	calls  int
	target uuid.UUID
	ctx    context.Context
}

func (s *repairContextReadStub) AdminReconciliationRepairContext(ctx context.Context, id uuid.UUID) (ReconciliationRepairContext, error) {
	s.calls++
	s.target = id
	s.ctx = ctx
	return s.item, s.err
}

// In-memory handler/repository boundary only: no server, database or auth claim.
func TestReconciliationRepairContextHandlerReadContract(t *testing.T) {
	id := uuid.MustParse("01a00000-0000-7000-8000-000000000038")
	run := uuid.MustParse("01a00000-0000-7000-8000-000000000039")
	amount := int64(9007199254740993)
	source := &repairContextReadStub{item: ReconciliationRepairContext{CaseID: id, RunID: run, CaseVersion: 9223372036854775807, Channel: "ALIPAY", BusinessDate: "2026-10-09", AmountFen: &amount, Currency: "CNY", SnapshotAvailable: true}}
	for _, tc := range []struct {
		name, pathID string
		cause        error
		status       int
		code         string
	}{
		{"read", id.String(), nil, 200, ""},
		{"invalid", "invalid", nil, 400, "INVALID_REQUEST"},
		{"nil-id", uuid.Nil.String(), nil, 400, "INVALID_REQUEST"},
		{"missing", id.String(), ErrNotFound, 404, "NOT_FOUND"},
		{"storage-failure", id.String(), errors.New("controlled storage failure"), 500, "INTERNAL_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source.calls = 0
			source.err = tc.cause
			request := httptest.NewRequest(http.MethodGet, "/admin/v1/reconciliation-cases/"+tc.pathID+"/repair-context", nil)
			request.SetPathValue("case_id", tc.pathID)
			response := httptest.NewRecorder()
			NewService(source).adminReconciliationRepairContext(response, request)
			if response.Code != tc.status {
				t.Fatalf("status %d body %s", response.Code, response.Body.String())
			}
			if tc.status != 200 {
				var got struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(response.Body.Bytes(), &got) != nil || got.Code != tc.code {
					t.Fatal("read error classification changed")
				}
			}
			if tc.status == 400 {
				if source.calls != 0 {
					t.Fatal("invalid target reached repository")
				}
				return
			}
			if source.calls != 1 || source.target != id {
				t.Fatal("wrong read target")
			}
			deadline, ok := source.ctx.Deadline()
			if !ok || time.Until(deadline) > 3*time.Second || source.ctx.Err() != context.Canceled {
				t.Fatal("read budget or cancellation missing")
			}
			if tc.status == 200 {
				var got map[string]json.RawMessage
				if json.Unmarshal(response.Body.Bytes(), &got) != nil || string(got["amount_fen"]) != `"9007199254740993"` || string(got["case_version"]) != `"9223372036854775807"` || string(got["business_date"]) != `"2026-10-09"` || string(got["reconciliation_run_id"]) != `"01a00000-0000-7000-8000-000000000039"` {
					t.Fatal("exact source wire changed")
				}
			}
		})
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetPathValue("case_id", id.String())
	NewService(nil).adminReconciliationRepairContext(response, request)
	if response.Code != 503 {
		t.Fatal("missing read capability was not unavailable")
	}
}
