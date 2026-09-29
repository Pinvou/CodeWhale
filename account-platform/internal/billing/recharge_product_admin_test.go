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

func TestRechargeProductCreateSelectionUsesAuthoritativeOptions(t *testing.T) {
	options := rechargeProductCreateOptions{
		Adapters:            []rechargeProductAdapterOption{{ProviderID: "provider-a", ProviderVersion: "adapter-v2", Channels: []string{"ALIPAY", "WECHAT"}}},
		InvoiceProductNames: []string{"AI service"},
	}
	valid := CreateRechargeProduct{InvoiceProductName: "AI service", Mapping: RechargeProductMappingDraft{ProviderID: "provider-a", ProviderVersion: "adapter-v2", Channels: []string{"ALIPAY", "wechat"}}}
	if err := validateRechargeProductCreateSelection(valid, options); err != nil {
		t.Fatalf("valid selection rejected: %v", err)
	}
	for _, fixture := range []struct {
		name   string
		mutate func(*CreateRechargeProduct)
	}{
		{name: "free text invoice", mutate: func(value *CreateRechargeProduct) { value.InvoiceProductName = "new invoice" }},
		{name: "unknown adapter version", mutate: func(value *CreateRechargeProduct) { value.Mapping.ProviderVersion = "adapter-v3" }},
		{name: "unsupported channel", mutate: func(value *CreateRechargeProduct) { value.Mapping.Channels = []string{"CARD"} }},
		{name: "duplicate channel", mutate: func(value *CreateRechargeProduct) { value.Mapping.Channels = []string{"ALIPAY", "alipay"} }},
		{name: "empty channel", mutate: func(value *CreateRechargeProduct) { value.Mapping.Channels = nil }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input := valid
			input.Mapping.Channels = append([]string(nil), valid.Mapping.Channels...)
			fixture.mutate(&input)
			if !errors.Is(validateRechargeProductCreateSelection(input, options), ErrConflict) {
				t.Fatal("selection outside authoritative options was accepted")
			}
		})
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

func validRechargeProductForEditTests() CreateRechargeProduct {
	return CreateRechargeProduct{
		ProductID: "PRODUCT_V2", DisplayNameZhCN: "中文", DisplayNameEN: "English", DisplayNameJA: "日本語",
		BasePoints: 100, BonusPoints: 10, ValidityDays: 30, InvoiceProductName: "AI service",
		Mapping: RechargeProductMappingDraft{ProviderID: "provider", ProviderVersion: "v1", ExternalSKUBOM: "SKU-V2", AmountFen: 100, Currency: "CNY", Channels: []string{"ALIPAY"}, AdmissionClass: "TEST_ONLY"},
	}
}

func TestReplacementCreateRequiresDistinctPublishedSourceVersionBeforeDatabaseAccess(t *testing.T) {
	repository := &PostgresRepository{}
	for _, fixture := range []struct {
		name      string
		configure func(*CreateRechargeProduct)
	}{
		{name: "missing source version", configure: func(value *CreateRechargeProduct) { source := "PRODUCT_V1"; value.ReplacementProduct = &source }},
		{name: "self replacement", configure: func(value *CreateRechargeProduct) {
			source, version := value.ProductID, int64(1)
			value.ReplacementProduct, value.ReplacementExpectedVersion = &source, &version
		}},
		{name: "version without source", configure: func(value *CreateRechargeProduct) { version := int64(1); value.ReplacementExpectedVersion = &version }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input := validRechargeProductForEditTests()
			fixture.configure(&input)
			if _, err := repository.CreateRechargeProduct(t.Context(), input); !errors.Is(err, ErrConflict) {
				t.Fatalf("err=%v, want ErrConflict before database access", err)
			}
		})
	}
}

func TestDraftUpdateRequiresBothCatalogAndDraftVersionsBeforeDatabaseAccess(t *testing.T) {
	repository := &PostgresRepository{}
	input := validRechargeProductForEditTests()
	command := RechargeProductDraftCommand{
		CatalogItemID: uuid.New(), ExpectedVersion: 1, DisplayNameZhCN: input.DisplayNameZhCN, DisplayNameEN: input.DisplayNameEN, DisplayNameJA: input.DisplayNameJA,
		BasePoints: input.BasePoints, BonusPoints: input.BonusPoints, ValidityDays: input.ValidityDays, InvoiceProductName: input.InvoiceProductName, Mapping: input.Mapping,
		IdempotencyKey: "draft-update-test-key", RequestHash: "hash", Actor: "actor", Reason: "update draft",
	}
	if _, err := repository.ExecuteRechargeProductDraftUpdate(t.Context(), command); !errors.Is(err, ErrConflict) {
		t.Fatalf("err=%v, want ErrConflict before database access", err)
	}
}
