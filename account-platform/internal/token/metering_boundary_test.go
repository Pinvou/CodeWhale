package token

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func versionedTestPrice(price PriceVersion) PriceVersion {
	if price.ID == uuid.Nil {
		price.ID = uuid.New()
	}
	price.FormulaVersionID = uuid.MustParse("00000000-0000-7000-8200-000000000001")
	price.ProtocolCode, price.CreditUnit, price.ScopeType = "CHAT", "MICROCREDIT", "MODEL"
	price.FormulaSnapshot = json.RawMessage(`{"evaluator_version":"SAFE_PARAMETER_MAP_V1","required_unit_versions":{"INPUT_TOKEN":"00000000-0000-7000-8100-000000000001","CACHED_INPUT_TOKEN":"00000000-0000-7000-8100-000000000002","OUTPUT_TOKEN":"00000000-0000-7000-8100-000000000003"},"expression":{"INPUT_TOKEN":"estimated_input_tokens","CACHED_INPUT_TOKEN":"cached_input_tokens","OUTPUT_TOKEN":"max_output_tokens"}}`)
	price.EffectiveFrom, price.ResolvedAt = time.Now().Add(-time.Hour).UTC(), time.Now().UTC()
	price.Units = []ResolvedUnitPrice{
		{UnitCode: "INPUT_TOKEN", UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000001"), UnitVersion: 1, MinimumPrecision: 1, PriceDenominator: 1_000_000, RoundingRule: "CEILING", ResolvedPrice: price.InputPerMillion, ConfigurationType: "FIXED_PRICE", PriceSourceScope: "MODEL", SourcePriceVersionID: price.ID},
		{UnitCode: "CACHED_INPUT_TOKEN", UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000002"), UnitVersion: 1, MinimumPrecision: 1, PriceDenominator: 1_000_000, RoundingRule: "CEILING", ResolvedPrice: price.CachedInputPerMillion, ConfigurationType: "FIXED_PRICE", PriceSourceScope: "MODEL", SourcePriceVersionID: price.ID},
		{UnitCode: "OUTPUT_TOKEN", UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000003"), UnitVersion: 1, MinimumPrecision: 1, PriceDenominator: 1_000_000, RoundingRule: "CEILING", ResolvedPrice: price.OutputPerMillion, ConfigurationType: "FIXED_PRICE", PriceSourceScope: "MODEL", SourcePriceVersionID: price.ID},
	}
	return price
}

type streamExtensionBoundaryBilling struct {
	*fixtureBilling
	client *HTTPBillingClient
	wallet uuid.UUID
}

func (b *streamExtensionBoundaryBilling) ReservePriced(ctx context.Context, usageID, wallet uuid.UUID, maximum int64, priceID uuid.UUID, _ HoldPricingContext) (BillingHold, error) {
	b.mu.Lock()
	b.wallet = wallet
	b.mu.Unlock()
	// Reserve runs on the Gateway handler before the later HTTP extension.
	return b.Reserve(ctx, usageID, wallet, maximum, priceID)
}

func (b *streamExtensionBoundaryBilling) Extend(ctx context.Context, usageID, holdID, wallet uuid.UUID, maximum, additional int64) error {
	if err := b.client.Extend(ctx, usageID, holdID, wallet, maximum, additional); err != nil {
		return err
	}
	b.mu.Lock()
	b.maximum = maximum + additional
	b.mu.Unlock()
	return nil
}

func TestPostgresStreamUsageCostMustBeAuthorizedBeforeCompletionDelivery(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "accepted"}[accepted], func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"earlier-content\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			service, repo, original := proxyFixture(t, provider.URL)
			original.price = &PriceVersion{ID: uuid.New(), InputPerMillion: 1_000_000}
			boundary := &streamExtensionBoundaryBilling{fixtureBilling: original}
			var extensions atomic.Int64
			billingPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					UsageID    uuid.UUID `json:"usage_request_id"`
					HoldID     uuid.UUID `json:"hold_id"`
					Additional int64     `json:"additional_microcredits"`
				}
				boundary.mu.Lock()
				expectedHold, expectedWallet := original.holdID, boundary.wallet
				boundary.mu.Unlock()
				if r.URL.Path != "/internal/v1/holds/extend" || json.NewDecoder(r.Body).Decode(&request) != nil || request.Additional != 100 || request.HoldID != expectedHold {
					t.Error("extension did not carry full actual usage cost")
					w.WriteHeader(500)
					return
				}
				extensions.Add(1)
				if !accepted {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				maximum := int64(100)
				_ = json.NewEncoder(w).Encode(BillingHold{ID: request.HoldID, UsageRequestID: request.UsageID, WalletID: expectedWallet, Status: "ACTIVE", MaxMicrocredits: &maximum, ExpiresAt: time.Now().Add(time.Minute)})
			}))
			defer billingPeer.Close()
			var err error
			boundary.client, err = NewHTTPBillingClient(billingPeer.URL, "controlled-token")
			if err != nil {
				t.Fatal(err)
			}
			service.billing = boundary
			gateway := httptest.NewServer(http.HandlerFunc(service.chatCompletions))
			defer gateway.Close()
			request := modelRequest(`{"model":"model-test","stream":true}`)
			request.URL, _ = request.URL.Parse(gateway.URL)
			request.RequestURI = ""
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if extensions.Load() != 1 || !strings.Contains(string(payload), "earlier-content") {
				t.Fatal("did not exercise actual usage after earlier delivered content")
			}
			if accepted {
				if !strings.Contains(string(payload), `"usage"`) || !strings.Contains(string(payload), "[DONE]") || usageStatus(t, repo) != "COMMITTED" || original.charge() != 100 {
					t.Fatal("confirmed extension did not complete and settle actual usage")
				}
			} else if strings.Contains(string(payload), `"usage"`) || strings.Contains(string(payload), "[DONE]") || usageStatus(t, repo) != "REVIEW" || original.commitCount() != 0 || original.releaseCount() != 0 {
				t.Fatal("unconfirmed funds exposed completion or changed protected funds")
			}
		})
	}
}

func TestPostgresStreamLaterInvalidUsageCannotHideBehindEarlierEvidence(t *testing.T) {
	for _, scenario := range []struct {
		name, later string
		complete    bool
	}{
		{"invalid-later-usage", `{"usage":{"prompt_tokens":-1,"completion_tokens":0}}`, false},
		{"ordinary-null-usage", `{"choices":[],"usage":null}`, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\ndata: "+scenario.later+"\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			service, repo, original := proxyFixture(t, provider.URL)
			original.price = &PriceVersion{ID: uuid.New()}
			service.billing = &streamExtensionBoundaryBilling{fixtureBilling: original}
			gateway := httptest.NewServer(http.HandlerFunc(service.chatCompletions))
			defer gateway.Close()
			request := modelRequest(`{"model":"model-test","stream":true}`)
			request.URL, _ = request.URL.Parse(gateway.URL)
			request.RequestURI = ""
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if scenario.complete {
				if usageStatus(t, repo) != "COMMITTED" || original.commitCount() != 1 || original.charge() != 0 {
					t.Fatal("ordinary null usage frame rejected complete free usage")
				}
			} else if usageStatus(t, repo) != "REVIEW" || original.commitCount() != 0 || original.releaseCount() != 0 {
				t.Fatal("later malformed usage was masked by earlier evidence")
			}
		})
	}
}

func TestHTTPResolvedPriceCannotTurnMissingConfigurationIntoFreePrice(t *testing.T) {
	price := versionedTestPrice(PriceVersion{Model: "controlled-model"})
	encoded, err := json.Marshal(price)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		edit  func(map[string]any)
		valid bool
	}{
		{"explicit-free", func(map[string]any) {}, true},
		{"missing-amount", func(v map[string]any) { delete(v["units"].([]any)[0].(map[string]any), "resolved_price") }, false},
		{"null-amount", func(v map[string]any) { v["units"].([]any)[0].(map[string]any)["resolved_price"] = nil }, false},
		{"missing-definition", func(v map[string]any) { delete(v["units"].([]any)[0].(map[string]any), "unit_definition_id") }, false},
		{"missing-rounding", func(v map[string]any) { delete(v["units"].([]any)[0].(map[string]any), "rounding_rule") }, false},
		{"missing-source", func(v map[string]any) { delete(v["units"].([]any)[0].(map[string]any), "source_price_version_id") }, false},
		{"wrong-exact-version", func(v map[string]any) {
			v["units"].([]any)[0].(map[string]any)["unit_definition_id"] = uuid.NewString()
		}, false},
		{"swapped-unit-map", func(v map[string]any) {
			versions := v["formula_snapshot"].(map[string]any)["required_unit_versions"].(map[string]any)
			versions["INPUT_TOKEN"], versions["OUTPUT_TOKEN"] = versions["OUTPUT_TOKEN"], versions["INPUT_TOKEN"]
		}, false},
		{"duplicate-definition", func(v map[string]any) {
			id := v["units"].([]any)[0].(map[string]any)["unit_definition_id"]
			v["units"].([]any)[2].(map[string]any)["unit_definition_id"] = id
			v["formula_snapshot"].(map[string]any)["required_unit_versions"].(map[string]any)["OUTPUT_TOKEN"] = id
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			test.edit(value)
			payload, _ := json.Marshal(value)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
			defer peer.Close()
			client, err := NewHTTPBillingClient(peer.URL, "controlled-token")
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Price(context.Background(), price.Model)
			if (err == nil) != test.valid {
				t.Fatalf("accepted=%v want=%v", err == nil, test.valid)
			}
			if test.valid && (len(result.Units) != 3 || result.Units[0].UnitDefinitionID != price.Units[0].UnitDefinitionID) {
				t.Fatal("lost immutable unit facts")
			}
		})
	}
}

func TestPostgresProxyMissingOutputStaysMissingOnActualProviderRequest(t *testing.T) {
	for _, body := range []string{`{"model":"model-test","messages":[{"content":"hello"}],"metadata":{"counter":9007199254740993}}`, `{"model":"model-test","messages":[{"content":"hello"}],"max_tokens":11}`} {
		t.Run(body, func(t *testing.T) {
			var received map[string]json.RawMessage
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
			}))
			defer peer.Close()
			service, repo, _ := proxyFixture(t, peer.URL)
			response := httptest.NewRecorder()
			service.chatCompletions(response, modelRequest(body))
			if response.Code != 200 || usageStatus(t, repo) != "COMMITTED" {
				t.Fatalf("status=%d", response.Code)
			}
			if strings.Contains(body, "max_tokens") {
				if string(received["max_tokens"]) != "11" {
					t.Fatal("changed explicit output bound")
				}
			} else {
				if _, found := received["max_tokens"]; found {
					t.Fatal("injected a missing output limit")
				}
				if _, found := received["max_completion_tokens"]; found {
					t.Fatal("injected a missing completion limit")
				}
				if !strings.Contains(string(received["metadata"]), "9007199254740993") {
					t.Fatal("lost integer precision")
				}
			}
		})
	}
}
