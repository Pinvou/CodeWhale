package billing

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRechargeProductMicrocreditsAreDerivedFromPoints(t *testing.T) {
	for _, fixture := range []struct {
		name                  string
		base, bonus, provided int64
		providedPresent       bool
		want                  int64
		wantErr               bool
	}{
		{name: "omitted compatibility field", base: 100, bonus: 10, want: 1100000},
		{name: "matching legacy field", base: 100, bonus: 10, provided: 1100000, providedPresent: true, want: 1100000},
		{name: "mismatched legacy field", base: 100, bonus: 10, provided: 110000, providedPresent: true, wantErr: true},
		{name: "explicit zero legacy field", base: 100, bonus: 10, providedPresent: true, wantErr: true},
		{name: "zero total", wantErr: true},
		{name: "addition overflow", base: math.MaxInt64, bonus: 1, wantErr: true},
		{name: "multiplication overflow", base: math.MaxInt64 / 10000, bonus: 1, wantErr: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input := CreateRechargeProduct{BasePoints: fixture.base, BonusPoints: fixture.bonus}
			if fixture.providedPresent {
				provided := fixture.provided
				input.CreditMicrocredits = &provided
			}
			got, err := normalizeRechargeProductMicrocredits(&input)
			if fixture.wantErr {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("err=%v, want ErrConflict", err)
				}
				return
			}
			if err != nil || got != fixture.want {
				t.Fatalf("got=%d err=%v, want=%d", got, err, fixture.want)
			}
		})
	}
}

func TestRechargeProductCreateHashCanonicalizesLegacyMicrocredits(t *testing.T) {
	omitted := createRechargeProductRequest{CreateRechargeProduct: CreateRechargeProduct{BasePoints: 100, BonusPoints: 10}, Reason: "create product"}
	legacyValue := int64(1100000)
	legacy := createRechargeProductRequest{CreateRechargeProduct: CreateRechargeProduct{BasePoints: 100, BonusPoints: 10, CreditMicrocredits: &legacyValue}, Reason: "create product"}
	if _, err := normalizeRechargeProductMicrocredits(&omitted.CreateRechargeProduct); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeRechargeProductMicrocredits(&legacy.CreateRechargeProduct); err != nil {
		t.Fatal(err)
	}
	if left, right := catalogRequestHash("CREATE", omitted), catalogRequestHash("CREATE", legacy); left != right {
		t.Fatalf("equivalent create requests produced different hashes: %s != %s", left, right)
	}
}

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
