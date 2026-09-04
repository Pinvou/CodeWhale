package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type stubImpactPriceRepository struct {
	prices map[uuid.UUID]ImpactPriceCoverage
	err    error
}

func (stub stubImpactPriceRepository) BatchCurrentPriceCoverage(_ context.Context, queries []ImpactPriceQuery) ([]ImpactPriceCoverage, time.Time, int64, string, error) {
	if stub.err != nil {
		return nil, time.Time{}, 0, "", stub.err
	}
	result := make([]ImpactPriceCoverage, 0, len(queries))
	for _, query := range queries {
		price, ok := stub.prices[query.ModelID]
		if !ok {
			price = ImpactPriceCoverage{ModelID: query.ModelID, ProtocolCode: query.ProtocolCode, CreditUnit: query.CreditUnit}
		}
		result = append(result, price)
	}
	return result, time.Now().UTC(), 7, strings.Repeat("a", 64), nil
}

func TestBillingImpactPriceCoverageIsServiceAuthenticatedCompleteAndRedacted(t *testing.T) {
	modelID, missingID, priceID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	formulaID := uuid.New()
	priceVersion := int64(1)
	service := NewService(nil).WithImpactPriceRepository(stubImpactPriceRepository{prices: map[uuid.UUID]ImpactPriceCoverage{
		modelID: {ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: pointerTime(now.Add(-time.Hour))},
	}})
	handler := service.Handler("impact-service-token")
	body, _ := json.Marshal(map[string]any{"items": []ImpactPriceQuery{{ModelID: missingID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}, {ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/pricing/impact-coverage", bytes.NewReader(body))
	if err := serviceauth.SetAuthorization(request, "impact-service-token"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"covered":true`) || !strings.Contains(response.Body.String(), `"covered":false`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"provider", "base_url", "secret", "formula_snapshot", "input_per_million"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/internal/v1/pricing/impact-coverage", bytes.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized && unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
}

func TestBillingImpactPriceCoverageFailsWholeRequest(t *testing.T) {
	modelID := uuid.New()
	service := NewService(nil).WithImpactPriceRepository(stubImpactPriceRepository{err: errors.New("database detail")})
	handler := service.Handler("impact-service-token")
	body, _ := json.Marshal(map[string]any{"items": []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/pricing/impact-coverage", bytes.NewReader(body))
	if err := serviceauth.SetAuthorization(request, "impact-service-token"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "database detail") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBillingImpactPriceCoverageLimitsAndStableErrors(t *testing.T) {
	items := make([]ImpactPriceQuery, maximumImpactPriceModels+1)
	for index := range items {
		items[index] = ImpactPriceQuery{ModelID: uuid.New(), ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}
	}
	service := NewService(nil).WithImpactPriceRepository(stubImpactPriceRepository{})
	handler := service.Handler("impact-service-token")
	body, _ := json.Marshal(map[string]any{"items": items})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/pricing/impact-coverage", bytes.NewReader(body))
	if err := serviceauth.SetAuthorization(request, "impact-service-token"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_REQUEST") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, repositoryError := range []error{context.DeadlineExceeded, ErrPriceAmbiguous, ErrPriceUnavailable} {
		service = NewService(nil).WithImpactPriceRepository(stubImpactPriceRepository{err: repositoryError})
		handler = service.Handler("impact-service-token")
		body, _ = json.Marshal(map[string]any{"items": []ImpactPriceQuery{{ModelID: uuid.New(), ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}})
		request = httptest.NewRequest(http.MethodPost, "/internal/v1/pricing/impact-coverage", bytes.NewReader(body))
		if err := serviceauth.SetAuthorization(request, "impact-service-token"); err != nil {
			t.Fatal(err)
		}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "PRICING_UNAVAILABLE") {
			t.Fatalf("err=%v status=%d body=%s", repositoryError, response.Code, response.Body.String())
		}
	}
}

func pointerTime(value time.Time) *time.Time { return &value }
