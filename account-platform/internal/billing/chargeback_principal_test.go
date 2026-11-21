package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type legacyChargebackPrincipalRepository struct {
	Repository
	calls int
}

func (r *legacyChargebackPrincipalRepository) RecordConfirmedChargebackForReview(context.Context, uuid.UUID, string, json.RawMessage) (ChargebackCase, error) {
	r.calls++
	return ChargebackCase{}, nil
}

func TestChargebackPrincipalCannotFallBackToLegacyWriter(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	repository := &legacyChargebackPrincipalRepository{}
	handler := NewService(repository).Handler("controlled-fixture")
	for _, principal := range []string{`,"principal_evidence":{"amount_fen":"40","currency":"CNY","provider_transaction_id":"controlled-original","supplier_evidence_reference":"controlled-receipt"}`, ""} {
		request := httptest.NewRequest("POST", "/internal/v1/chargebacks/confirmed", strings.NewReader(`{"payment_transaction_id":"11111111-1111-4111-8111-111111111111","provider_chargeback_id":"controlled-chargeback","payload_snapshot":{}`+principal+`}`))
		request.Header.Set("Authorization", "Bearer controlled-fixture")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := 422
		if principal == "" {
			want = 200
		}
		if response.Code != want {
			t.Fatal("principal capability was silently discarded", response.Code, want)
		}
	}
	if repository.calls != 1 {
		t.Fatal("principal input reached legacy writer")
	}
	// Defense in depth at the repository boundary must precede any database work.
	repositoryWithNoPool := &PostgresRepository{}
	if _, err := repositoryWithNoPool.RecordChargebackWithPrincipal(context.Background(), uuid.New(), "controlled", json.RawMessage(`{}`), &ChargebackPrincipalInput{AmountFen: "40", Currency: "CNY", ProviderTransactionID: "controlled-original", SupplierEvidenceReference: "controlled-receipt"}); !errors.Is(err, ErrConflict) {
		t.Fatal("unauthenticated principal reached storage", err)
	}
}
