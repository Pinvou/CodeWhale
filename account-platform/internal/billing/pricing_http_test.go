package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type pricingBodyGuardRepository struct{ Repository }

func (pricingBodyGuardRepository) UpdatePriceDraft(context.Context, uuid.UUID, uuid.UUID, PriceDraftCommand) (ManagedPriceVersion, error) {
	panic("invalid request reached repository")
}
func (pricingBodyGuardRepository) CreateMeteringUnitVersion(context.Context, MeteringUnitDefinition, uuid.UUID, string) (MeteringUnitDefinition, error) {
	panic("invalid request reached repository")
}
func (pricingBodyGuardRepository) CreatePriceDraft(context.Context, PriceDraftCommand, uuid.UUID, string) (ManagedPriceVersion, error) {
	panic("invalid request reached repository")
}
func (pricingBodyGuardRepository) PublishPriceVersion(context.Context, uuid.UUID, uuid.UUID, int64, string) (ManagedPriceVersion, error) {
	panic("invalid request reached repository")
}
func (pricingBodyGuardRepository) TransitionPriceVersion(context.Context, uuid.UUID, uuid.UUID, string, string, string, *uuid.UUID, string) (ManagedPriceVersion, error) {
	panic("invalid request reached repository")
}

func TestPricingMalformedBodiesReturnStatic400(t *testing.T) {
	s := NewService(pricingBodyGuardRepository{})
	handlers := map[string]http.HandlerFunc{"update": s.adminUpdatePriceDraft, "unit": s.adminCreateMeteringUnit, "draft": s.adminCreatePriceDraftV2, "publish": s.adminPublishPrice, "transition": s.adminTransitionPrice}
	bodies := map[string]string{"malformed": "{", "unknown": `{"unexpected":true}`, "trailing": `{} garbage`, "two-values": `{} {}`, "oversize": `{"unexpected":"` + strings.Repeat("x", 1<<20) + `"}`}
	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			for kind, body := range bodies {
				t.Run(kind, func(t *testing.T) {
					request := httptest.NewRequest("POST", "/", strings.NewReader(body))
					request.SetPathValue("price_version_id", uuid.NewString())
					response := httptest.NewRecorder()
					handler(response, request)
					var result struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					}
					if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Code != "INVALID_REQUEST" {
						t.Fatalf("status=%d code=%q", response.Code, result.Code)
					}
					if strings.Contains(result.Message, "unexpected") || strings.Contains(result.Message, "garbage") {
						t.Fatal("request contents leaked")
					}
				})
			}
		})
	}
}
