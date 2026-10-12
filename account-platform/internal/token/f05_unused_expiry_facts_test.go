package token

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func f05ExpiryFrozenFixture(t *testing.T) (UsageRequest, DispatchEnvelope, map[string]any) {
	t.Helper()
	model, capability, protocol, wallet, account, credential := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	version, generation := int64(1), int64(1)
	price, err := new(readinessBillingFake).Price(context.Background(), model.String())
	if err != nil {
		t.Fatal(err)
	}
	for i := range price.Units {
		price.Units[i].RoundingRule = "CEILING"
	}
	body := []byte(`{"model":"frozen-upstream","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`)
	measured, err := EstimateChatUsage(body, 10)
	if err != nil {
		t.Fatal(err)
	}
	cost, err := Cost(price, measured)
	if err != nil {
		t.Fatal(err)
	}
	usage := UsageRequest{ID: uuid.New(), UserID: uuid.New(), SessionID: "controlled-expiry-session", IdempotencyNamespace: "model-inference", IdempotencyKeyDigest: strings.Repeat("a", 64), CanonicalVersion: 1, RequestFingerprint: strings.Repeat("b", 64), TokenAccountID: account, CredentialID: credential, WalletID: wallet, Model: model.String(), ModelID: &model, CapabilityVersionID: &capability, ProtocolDefinitionID: &protocol, CatalogVersion: &version, RouteGeneration: &generation, PriceVersionID: &price.ID, MaximumMicrocredits: cost, Status: "RESERVED", ExecutionStatus: "DISPATCH_INTENT_PERSISTED", SettlementStatus: "HOLD_ACTIVE", DeliveryStatus: "NOT_STARTED", CancelStatus: "NOT_REQUESTED"}
	envelope := DispatchEnvelope{BaseURL: "https://provider.invalid", Provider: "controlled", AdapterVersion: "controlled-v1", Credential: []byte("controlled-expiry-secret"), RequestBody: body, Price: price, Stream: false, MaximumCost: cost, MaximumOutput: 10, ContextType: "PERSONAL"}
	priceJSON, _ := json.Marshal(price)
	snapshot := map[string]any{"request_sha256": strings.Repeat("c", 64), "model_id": model, "upstream_model_ref": "frozen-upstream", "catalog_version": version, "model_capability_version_id": capability, "protocol_definition_id": protocol, "protocol_code": "CHAT", "protocol_version": 1, "route_generation": generation, "stream": false, "estimated_input_tokens": measured.InputTokens, "maximum_output_tokens": int64(10), "estimator": map[string]any{"code": "UTF8_BYTES_DIV4_CEIL", "version": 1}, "context_type": "PERSONAL", "organization_id": nil, "can_purchase_organization_entitlements": false, "wallet_id": wallet, "token_account_id": account, "credential_id": credential, "formula_version_id": price.FormulaVersionID, "price_version_id": price.ID, "pricing_snapshot": json.RawMessage(priceJSON), "provider_id": "controlled", "adapter_version": "controlled-v1", "wallet_owner_type": "USER"}
	usage.RequestSnapshot, _ = json.Marshal(snapshot)
	return usage, envelope, snapshot
}

func TestF05UnusedExpiryFrozenDispatchFacts(t *testing.T) {
	t.Run("canonical", func(t *testing.T) {
		u, e, _ := f05ExpiryFrozenFixture(t)
		if !unusedExpiryFrozenFactsMatch(u, e) {
			t.Fatal("complete current frozen tuple denied")
		}
	})
	t.Run("localStopShapeStillRequiresDatabaseProof", func(t *testing.T) {
		u, e, _ := f05ExpiryFrozenFixture(t)
		now := time.Now().UTC()
		u.UserStoppedAt = &now
		u.CancelRequestedAt = &now
		u.DeliveryStatus = "USER_STOPPED"
		u.CancelStatus = "UNSUPPORTED"
		if !unusedExpiryFrozenFactsMatch(u, e) {
			t.Fatal("known local stop shape denied before database authority validation")
		}
	})
	for _, scenario := range []string{"legacy", "futureCanonical", "missingField", "nullField", "price", "provider", "adapter", "bodyModel", "bodyMetering", "bodyMaximum", "stream", "actual", "sequence", "modelID", "context", "releasedWithoutProof", "foreignReview", "stopMissingTime", "stopProviderStatus", "stopTimeMismatch", "futureProtocol"} {
		t.Run(scenario, func(t *testing.T) {
			u, e, s := f05ExpiryFrozenFixture(t)
			switch scenario {
			case "legacy":
				u.CanonicalVersion = 0
			case "futureCanonical":
				u.CanonicalVersion = 2
			case "missingField":
				delete(s, "price_version_id")
			case "nullField":
				s["stream"] = nil
			case "price":
				e.Price.InputPerMillion++
			case "provider":
				e.Provider = "other"
			case "adapter":
				e.AdapterVersion = "other"
			case "bodyModel":
				e.RequestBody = []byte(`{"model":"other","messages":[{"content":"hello"}],"max_tokens":10}`)
			case "bodyMetering":
				e.RequestBody = []byte(`{"model":"frozen-upstream","messages":[{"content":"much longer request body"}],"max_tokens":10}`)
			case "bodyMaximum":
				e.MaximumOutput++
			case "stream":
				e.Stream = true
			case "actual":
				v := int64(0)
				u.ActualMicrocredits = &v
			case "sequence":
				u.LastPersistedSequence = 1
			case "modelID":
				id := uuid.New()
				u.ModelID = &id
			case "context":
				e.ContextType = "ORGANIZATION"
			case "releasedWithoutProof":
				u.ExecutionStatus = "CANCELLED_BEFORE_DISPATCH"
				u.Status = "RELEASED"
				u.SettlementStatus = "RELEASED"
			case "foreignReview":
				u.ExecutionStatus = "REVIEW"
				v := "other"
				u.ErrorClass = &v
			case "stopMissingTime":
				u.DeliveryStatus = "USER_STOPPED"
				u.CancelStatus = "UNSUPPORTED"
			case "stopProviderStatus":
				now := time.Now().UTC()
				u.DeliveryStatus = "USER_STOPPED"
				u.CancelStatus = "PROVIDER_SENT"
				u.UserStoppedAt = &now
				u.CancelRequestedAt = &now
			case "stopTimeMismatch":
				now := time.Now().UTC()
				later := now.Add(time.Second)
				u.DeliveryStatus = "USER_STOPPED"
				u.CancelStatus = "UNSUPPORTED"
				u.UserStoppedAt = &now
				u.CancelRequestedAt = &later
			case "futureProtocol":
				s["protocol_version"] = 2
			}
			u.RequestSnapshot, _ = json.Marshal(s)
			if unusedExpiryFrozenFactsMatch(u, e) {
				t.Fatal("unproven dispatch tuple accepted", scenario)
			}
		})
	}
	t.Run("frozenJSONOrderAndInt64", func(t *testing.T) {
		if !expiryJSONEqual([]byte(`{"b":9007199254740993,"a":1}`), []byte(`{"a":1,"b":9007199254740993}`)) || expiryJSONEqual([]byte(`{"b":9007199254740993}`), []byte(`{"b":9007199254740992}`)) {
			t.Fatal("JSON ordering or integer precision changed frozen comparison")
		}
	})
}
