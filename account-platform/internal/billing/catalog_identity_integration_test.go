package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Uses the actual HTTP handler and PostgreSQL command history. Human identity
// decoding is controlled; this does not claim real OIDC or permission middleware.
func TestRetainedCatalogTransitionKeyBindsTarget(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	makeDraft := func() RechargeProductCatalogItem {
		input := validRechargeProductForEditTests()
		input.ProductID = "P_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		input.Mapping.ExternalSKUBOM = input.ProductID
		item, err := r.CreateRechargeProduct(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	first, second := makeDraft(), makeDraft()
	s := &Service{catalog: r}
	actor := uuid.NewString()
	call := func(target uuid.UUID) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodDelete, "/admin/v1/recharge-products/"+target.String(), strings.NewReader(`{"expected_version":1,"reason":"controlled deletion"}`))
		request.SetPathValue("catalog_item_id", target.String())
		request.Header.Set("Idempotency-Key", "controlled-catalog-target-key")
		request.Header.Set("Authorization", "Bearer controlled")
		response := httptest.NewRecorder()
		identity.Middleware(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled", Subject: actor}, nil
		}, http.HandlerFunc(s.adminDeleteRechargeProduct)).ServeHTTP(response, request)
		return response
	}
	initial, replay := call(first.CatalogItemID), call(first.CatalogItemID)
	if initial.Code != http.StatusOK || replay.Code != http.StatusOK {
		t.Fatalf("legitimate delete/replay statuses: %d/%d", initial.Code, replay.Code)
	}
	var initialReceipt, replayReceipt RechargeProductCatalogItem
	if err := json.Unmarshal(initial.Body.Bytes(), &initialReceipt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayReceipt); err != nil {
		t.Fatal(err)
	}
	initialJSON, _ := json.Marshal(initialReceipt)
	replayJSON, _ := json.Marshal(replayReceipt)
	if !bytes.Equal(initialJSON, replayJSON) || initialReceipt.CatalogItemID != first.CatalogItemID {
		t.Fatal("same target did not replay its complete typed receipt")
	}
	if response := call(second.CatalogItemID); response.Code != http.StatusConflict {
		t.Fatalf("changed target with reused key: %d", response.Code)
	}
	current, err := r.RechargeProductCatalogItem(ctx, second.CatalogItemID)
	if err != nil || current.LifecycleStatus != "DRAFT" {
		t.Fatal("changed target mutated")
	}
}
