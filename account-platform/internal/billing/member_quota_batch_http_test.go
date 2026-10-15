package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/google/uuid"
)

type quotaBatchErrorRepository struct {
	Repository
	memberQuotaRepository
	errors   []error
	requests []MemberQuotaAllocation
}

func (r *quotaBatchErrorRepository) AllocateMemberQuota(_ context.Context, request MemberQuotaAllocation) (MemberQuotaGrant, bool, error) {
	r.requests = append(r.requests, request)
	return MemberQuotaGrant{}, false, r.errors[len(r.requests)-1]
}

func TestMemberQuotaBatchPreservesTypedFailures(t *testing.T) {
	org, human := uuid.New(), uuid.New()
	items := make([]memberQuotaBatchItem, 3)
	for i := range items {
		items[i] = memberQuotaBatchItem{MemberUserID: uuid.New(), PackageID: uuid.New(), ComponentID: uuid.New(), ComponentType: "BASE", AmountMicrocredits: 40, MembershipVersion: 1}
	}
	repo := &quotaBatchErrorRepository{errors: []error{ErrWalletUnavailable, fmt.Errorf("allocation: %w", ErrWalletUnavailable), errors.New("controlled unknown storage error")}}
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/controlled", strings.NewReader(string(body)))
	req.SetPathValue("organization_id", org.String())
	req.Header.Set("Idempotency-Key", "original-batch-key-0001")
	req = req.WithContext(humandelegation.WithClaims(req.Context(), humandelegation.Claims{HumanUserID: human}))
	response := httptest.NewRecorder()
	NewService(repo).allocateMemberQuotaBatch(response, req)
	var result struct {
		Items []memberQuotaBatchResult `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusMultiStatus || len(result.Items) != 3 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("batch failed: %d %s (%v)", response.Code, response.Body.String(), err)
	}
	for i, code := range []string{"WALLET_UNAVAILABLE", "WALLET_UNAVAILABLE", "INTERNAL_ERROR"} {
		item := result.Items[i]
		if item.Index != i || item.Status != "FAILED" || item.Code != code || item.Created || item.Grant != nil {
			t.Fatalf("item %d: %#v", i, item)
		}
		request := repo.requests[i]
		if request.OrganizationID != org || request.CreatedBy != human || request.MemberUserID != items[i].MemberUserID || request.AmountMicrocredits != 40 || request.MembershipVersion != 1 || request.IdempotencyKey != deriveMemberQuotaChildKey("original-batch-key-0001", i, items[i]) {
			t.Fatalf("batch facts changed: %#v", request)
		}
	}
}
