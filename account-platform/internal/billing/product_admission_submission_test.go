package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProductAdmissionSubmissionFreezesReviewFacts(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	item := RechargeProductCatalogItem{
		CatalogItemID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), ProductID: "merchant-product", Version: 7,
		DisplayNameZhCN: "商品", DisplayNameEN: "Product", DisplayNameJA: "商品",
		Mapping: RechargeProductMappingDraft{DraftVersion: 3, ProviderID: "merchant", ProviderVersion: "v1", ExternalSKUBOM: "sku-1", AmountFen: 100, Currency: "CNY", Channels: []string{"WECHAT"}, AdmissionClass: "FORMAL"},
	}
	product := Product{ProductID: item.ProductID, BasePoints: 100, BonusPoints: 20, ValidityDays: 30, InvoiceProductName: "Service"}
	original := ProductAdmissionOriginal{DocumentID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), Version: 1, MediaType: "application/pdf", Size: 1234, SHA256: strings.Repeat("a", 64)}
	expires := now.Add(30 * 24 * time.Hour)
	freeze := func(env string, candidate RechargeProductCatalogItem, p Product, o ProductAdmissionOriginal, expiry time.Time) ([]byte, string) {
		t.Helper()
		body, digest, err := freezeProductAdmissionSubmission(env, candidate, p, o, expiry, now)
		if err != nil {
			t.Fatal(err)
		}
		return body, digest
	}
	body, baseline := freeze("production", item, product, original, expires)
	// Check the human-readable JSON independently of the submission Go tags and
	// the legacy fingerprint. A digest change alone cannot prove this projection.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	var gotPublication, wantPublication map[string]any
	wantJSON := `{"catalog_item_id":"11111111-1111-4111-8111-111111111111","catalog_version":7,"product_id":"merchant-product","display_name_zh_cn":"商品","display_name_en":"Product","display_name_ja":"商品","base_points":100,"bonus_points":20,"validity_days":30,"invoice_product_name":"Service","mapping":{"draft_version":3,"provider_id":"merchant","provider_version":"v1","sku_bom":"sku-1","amount_fen":100,"currency":"CNY","channels":["WECHAT"],"admission_class":"FORMAL"}}`
	if err := json.Unmarshal(raw["publication"], &gotPublication); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &wantPublication); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotPublication, wantPublication) {
		t.Fatal("readable publication facts differ from the independently specified merchant facts")
	}
	var displayed productAdmissionSubmission
	if err := json.Unmarshal(body, &displayed); err != nil {
		t.Fatal(err)
	}
	if displayed.Publication.Mapping.AmountFen != 100 || displayed.Publication.Mapping.ExternalSKUBOM != "sku-1" || displayed.Publication.BasePoints != 100 || displayed.Publication.BonusPoints != 20 || displayed.Original.SHA256 != original.SHA256 || !displayed.ExpiresAt.Equal(expires) {
		t.Fatalf("review facts were not preserved: %+v", displayed)
	}
	sum := sha256.Sum256(body)
	if baseline != hex.EncodeToString(sum[:]) {
		t.Fatal("review digest must commit to the exact persisted readable snapshot")
	}
	_, sameInstant := freeze("production", item, product, original, expires.In(time.FixedZone("offset", 8*60*60)))
	if sameInstant != baseline {
		t.Fatal("the same absolute expiry must not split the review identity")
	}
	cases := map[string]func(*RechargeProductCatalogItem, *Product, *ProductAdmissionOriginal, *time.Time){
		"sku": func(i *RechargeProductCatalogItem, _ *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			i.Mapping.ExternalSKUBOM = "sku-2"
		},
		"price": func(i *RechargeProductCatalogItem, _ *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			i.Mapping.AmountFen++
		},
		"currency": func(i *RechargeProductCatalogItem, _ *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			i.Mapping.Currency = "USD"
		},
		"channel": func(i *RechargeProductCatalogItem, _ *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			i.Mapping.Channels = []string{"ALIPAY"}
		},
		"provider": func(i *RechargeProductCatalogItem, _ *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			i.Mapping.ProviderVersion = "v2"
		},
		"catalog-version": func(i *RechargeProductCatalogItem, _ *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			i.Version++
		},
		"entitlement": func(_ *RechargeProductCatalogItem, p *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			p.BasePoints++
		},
		"invoice": func(_ *RechargeProductCatalogItem, p *Product, _ *ProductAdmissionOriginal, _ *time.Time) {
			p.InvoiceProductName = "Other"
		},
		"original": func(_ *RechargeProductCatalogItem, _ *Product, o *ProductAdmissionOriginal, _ *time.Time) {
			o.SHA256 = strings.Repeat("b", 64)
		},
		"original-version": func(_ *RechargeProductCatalogItem, _ *Product, o *ProductAdmissionOriginal, _ *time.Time) {
			o.Version++
		},
		"expiry": func(_ *RechargeProductCatalogItem, _ *Product, _ *ProductAdmissionOriginal, e *time.Time) {
			*e = e.Add(time.Second)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			i, p, o, e := item, product, original, expires
			change(&i, &p, &o, &e)
			_, changed := freeze("production", i, p, o, e)
			if changed == baseline {
				t.Fatal("a changed review fact retained the old approval identity")
			}
		})
	}
	_, otherEnvironment := freeze("staging", item, product, original, expires)
	if otherEnvironment == baseline {
		t.Fatal("admission review must not be reusable across environments")
	}
	for _, invalid := range []ProductAdmissionOriginal{{}, {DocumentID: original.DocumentID, Version: 1, MediaType: "text/html", Size: 1, SHA256: original.SHA256}, {DocumentID: original.DocumentID, Version: 1, MediaType: original.MediaType, Size: 10<<20 + 1, SHA256: original.SHA256}} {
		if _, _, err := freezeProductAdmissionSubmission("production", item, product, invalid, expires, now); err == nil {
			t.Fatal("untrusted or unsupported original metadata accepted")
		}
	}
	if _, _, err := freezeProductAdmissionSubmission("production", item, product, original, now, now); err == nil {
		t.Fatal("already expired evidence accepted")
	}
}
