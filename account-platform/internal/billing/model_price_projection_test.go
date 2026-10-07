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

type modelPriceProjectionStub struct {
	Repository
	projection ModelPriceProjection
	err        error
	modelID    uuid.UUID
	protocol   string
	formulaID  uuid.UUID
	creditUnit string
}

func (stub *modelPriceProjectionStub) AdminModelPriceProjection(_ context.Context, modelID uuid.UUID, protocol string, formulaID uuid.UUID, creditUnit string) (ModelPriceProjection, error) {
	stub.modelID, stub.protocol, stub.formulaID, stub.creditUnit = modelID, protocol, formulaID, creditUnit
	return stub.projection, stub.err
}

func TestModelPriceProjectionRequiresExactStableDimensions(t *testing.T) {
	modelID, formulaID := uuid.New(), uuid.New()
	stub := &modelPriceProjectionStub{projection: ModelPriceProjection{
		ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: formulaID, CreditUnit: "MICROCREDIT",
		AsOf: time.Now().UTC(), Current: ModelPriceProjectionPoint{EffectiveAt: time.Now().UTC(), CoverageStatus: "MISSING"},
		Source: "BILLING", SnapshotSemantics: "BILLING_REPEATABLE_READ_ONLY; TOKEN_CATALOG_NOT_READ; NO_CROSS_DOMAIN_ATOMICITY",
	}}
	service := NewService(stub).WithAdminModelPriceProjection(true)
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/models/ignored/price-projection?protocol_code=chat&formula_version_id="+formulaID.String()+"&credit_unit=microcredit", nil)
	request.SetPathValue("model_id", modelID.String())
	response := httptest.NewRecorder()
	service.adminModelPriceProjection(response, request)
	if response.Code != http.StatusOK || stub.modelID != modelID || stub.protocol != "CHAT" || stub.formulaID != formulaID || stub.creditUnit != "MICROCREDIT" {
		t.Fatalf("status=%d body=%s dimensions=%s/%s/%s/%s", response.Code, response.Body.String(), stub.modelID, stub.protocol, stub.formulaID, stub.creditUnit)
	}
	var payload ModelPriceProjection
	if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Current.CoverageStatus != "MISSING" || payload.Current.Price != nil {
		t.Fatalf("missing price was not explicit: %+v", payload)
	}

	invalid := httptest.NewRequest(http.MethodGet, "/?protocol_code=CHAT-DANGEROUS&formula_version_id="+formulaID.String()+"&credit_unit=MICROCREDIT", nil)
	invalid.SetPathValue("model_id", modelID.String())
	invalidResponse := httptest.NewRecorder()
	service.adminModelPriceProjection(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid dimension status=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestModelPriceProjectionGateAndUnknownResultsFailClosed(t *testing.T) {
	modelID, formulaID := uuid.New(), uuid.New()
	path := "/?protocol_code=CHAT&formula_version_id=" + formulaID.String() + "&credit_unit=MICROCREDIT"
	for _, testCase := range []struct {
		name    string
		service *Service
		status  int
		code    string
	}{
		{name: "disabled", service: NewService(nil), status: 503, code: "MODEL_PRICE_PROJECTION_DISABLED"},
		{name: "failed", service: NewService(&modelPriceProjectionStub{err: errors.New("ambiguous")}).WithAdminModelPriceProjection(true), status: 503, code: "MODEL_PRICE_PROJECTION_UNAVAILABLE"},
		{name: "timeout", service: NewService(&modelPriceProjectionStub{err: context.DeadlineExceeded}).WithAdminModelPriceProjection(true), status: 504, code: "MODEL_PRICE_PROJECTION_TIMEOUT"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.SetPathValue("model_id", modelID.String())
			response := httptest.NewRecorder()
			testCase.service.adminModelPriceProjection(response, request)
			var payload map[string]any
			if response.Code != testCase.status || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload["code"] != testCase.code || payload["current"] != nil {
				t.Fatalf("status=%d payload=%#v", response.Code, payload)
			}
		})
	}
}

func TestModelPricePointFingerprintIgnoresObservationTimeButNotVersion(t *testing.T) {
	priceID := uuid.New()
	first := ModelPriceProjectionPoint{EffectiveAt: time.Now(), CoverageStatus: "AVAILABLE", Price: &ModelPriceProjectionPrice{PriceVersionID: priceID, ScopeType: "MODEL", SpaceETag: 1, Units: []ModelPriceProjectionUnit{}}}
	second := first
	second.EffectiveAt = first.EffectiveAt.Add(time.Hour)
	if modelPricePointFingerprint(first) != modelPricePointFingerprint(second) {
		t.Fatal("observation time created a false price transition")
	}
	second.Price = &ModelPriceProjectionPrice{PriceVersionID: uuid.New(), ScopeType: "MODEL", SpaceETag: 2, Units: []ModelPriceProjectionUnit{}}
	if modelPricePointFingerprint(first) == modelPricePointFingerprint(second) {
		t.Fatal("price version transition was hidden")
	}
}
