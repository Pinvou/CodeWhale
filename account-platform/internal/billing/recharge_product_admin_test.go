package billing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAdminUpdateRechargeProductDisplayNamesRejectsPathAndIdempotencyBeforeSideEffects(t *testing.T) {
	service := &Service{} // A nil catalog makes accidental command execution fail loudly.
	for _, fixture := range []struct {
		name, id, key string
	}{
		{"malformed resource wins", "not-a-uuid", "valid-idempotency-key"},
		{"missing key", "11111111-1111-4111-8111-111111111111", ""},
		{"short key", "11111111-1111-4111-8111-111111111111", "short"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/admin/v1/recharge-products/"+fixture.id+"/display-names", strings.NewReader(`{"expected_version":1,"display_name_zh_cn":"中","display_name_en":"English","display_name_ja":"日本語","reason":"rename"}`))
			request.SetPathValue("catalog_item_id", fixture.id)
			request.Header.Set("Idempotency-Key", fixture.key)
			response := httptest.NewRecorder()
			service.adminUpdateRechargeProductDisplayNames(response, request)
			if response.Code < 400 || response.Code >= 500 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestCatalogResourceRequestHashBindsCatalogItem(t *testing.T) {
	body := catalogDisplayNamesRequest{ExpectedVersion: 1, DisplayNameZhCN: "中", DisplayNameEN: "English", DisplayNameJA: "日本語", Reason: "rename"}
	left := catalogResourceRequestHash("BILLING_RECHARGE_PRODUCT_DISPLAY_NAMES_UPDATE", uuid.MustParse("11111111-1111-4111-8111-111111111111"), body)
	right := catalogResourceRequestHash("BILLING_RECHARGE_PRODUCT_DISPLAY_NAMES_UPDATE", uuid.MustParse("22222222-2222-4222-8222-222222222222"), body)
	if left == right {
		t.Fatal("resource identity is missing from request fingerprint")
	}
}
