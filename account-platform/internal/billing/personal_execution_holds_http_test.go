package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type exactSettlementRepository struct {
	Repository
	snapshot map[string]any
	calls    int
}

func (r *exactSettlementRepository) Commit(_ context.Context, usage, hold uuid.UUID, amount int64, snapshot map[string]any) (Hold, error) {
	r.calls++
	r.snapshot = snapshot
	return Hold{ID: hold}, nil
}
func TestUsageSettlementHTTPPreservesExactSnapshot(t *testing.T) {
	for _, number := range []string{"0", "128000", "9007199254740993", "9223372036854775807", "-9223372036854775808"} {
		t.Run(number, func(t *testing.T) {
			repository := &exactSettlementRepository{}
			payload := fmt.Sprintf(`{"usage_request_id":%q,"hold_id":%q,"actual_microcredits":1,"usage_snapshot":{"prompt_tokens":%s,"unknown":"preserved"}}`, uuid.New(), uuid.New(), number)
			response := httptest.NewRecorder()
			NewService(repository).commit(response, httptest.NewRequest(http.MethodPost, "/internal/v1/usage/commit", strings.NewReader(payload)))
			stored, err := json.Marshal(repository.snapshot)
			if response.Code != http.StatusOK || repository.calls != 1 || err != nil || !strings.Contains(string(stored), `"prompt_tokens":`+number) || repository.snapshot["unknown"] != "preserved" {
				t.Fatalf("snapshot changed: status=%d snapshot=%s err=%v", response.Code, stored, err)
			}
		})
	}
}
func TestUsageSettlementHTTPRejectsIncompleteBody(t *testing.T) {
	valid := fmt.Sprintf(`{"usage_request_id":%q,"hold_id":%q,"actual_microcredits":1,"usage_snapshot":{"prompt_tokens":1}}`, uuid.New(), uuid.New())
	for name, payload := range map[string]string{"second": valid + ` {}`, "garbage": valid + `x`, "oversize": valid + strings.Repeat(" ", 1<<20), "unknown": strings.TrimSuffix(valid, "}") + `,"unexpected":true}`, "null": strings.Replace(valid, `{"prompt_tokens":1}`, `null`, 1)} {
		t.Run(name, func(t *testing.T) {
			repository := &exactSettlementRepository{}
			response := httptest.NewRecorder()
			NewService(repository).commit(response, httptest.NewRequest(http.MethodPost, "/internal/v1/usage/commit", strings.NewReader(payload)))
			if response.Code != http.StatusBadRequest || repository.calls != 0 {
				t.Fatalf("invalid settlement accepted: status=%d calls=%d", response.Code, repository.calls)
			}
		})
	}
}
