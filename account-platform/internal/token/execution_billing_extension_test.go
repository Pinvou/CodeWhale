package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHTTPBillingExtensionOperationIdentity(t *testing.T) {
	usageID, holdID, walletID := uuid.New(), uuid.New(), uuid.New()
	operationID, otherOperation := uuid.New(), uuid.New()
	var captured []map[string]json.RawMessage
	var mu sync.Mutex
	snapshot := func() []map[string]json.RawMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]json.RawMessage(nil), captured...)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/holds/extend" {
			t.Error("wrong extension endpoint")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		captured = append(captured, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"hold_id": holdID, "usage_request_id": usageID, "wallet_id": walletID, "status": "ACTIVE", "max_microcredits": 20, "expires_at": time.Unix(1, 0).UTC()})
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-extension")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{operationID, operationID, otherOperation} {
		if err := client.Extend(context.Background(), usageID, holdID, 5, id); err != nil {
			t.Fatal(err)
		}
	}
	bodies := snapshot()
	if len(bodies) != 3 {
		t.Fatal("unexpected implicit retry", len(bodies))
	}
	for i, id := range []uuid.UUID{operationID, operationID, otherOperation} {
		want := map[string]any{"usage_request_id": usageID, "hold_id": holdID, "additional_microcredits": int64(5), "lease_seconds": 900, "operation_id": id}
		if len(bodies[i]) != len(want) {
			t.Fatal("changed extension request fields", bodies[i])
		}
		for key, value := range want {
			raw, _ := json.Marshal(value)
			if string(bodies[i][key]) != string(raw) {
				t.Fatal("operation facts changed", key)
			}
		}
	}
	if err := client.Extend(context.Background(), usageID, holdID, 5, uuid.Nil); !errors.Is(err, ErrForbidden) {
		t.Fatal("positive extension needs a caller-owned operation", err)
	}
	if len(snapshot()) != 3 {
		t.Fatal("missing operation reached Billing")
	}
	for _, invalid := range []struct {
		usageID, holdID uuid.UUID
		additional      int64
		operationID     uuid.UUID
	}{
		{usageID, holdID, 0, operationID},
		{uuid.Nil, holdID, 0, uuid.Nil},
		{usageID, uuid.Nil, 0, uuid.Nil},
		{usageID, holdID, -1, uuid.Nil},
	} {
		if err := client.Extend(context.Background(), invalid.usageID, invalid.holdID, invalid.additional, invalid.operationID); !errors.Is(err, ErrForbidden) {
			t.Fatal("invalid command accepted", invalid, err)
		}
	}
	if len(snapshot()) != 3 {
		t.Fatal("invalid command reached Billing")
	}
	if err := client.Extend(context.Background(), usageID, holdID, 0, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	bodies = snapshot()
	if len(bodies) != 4 || len(bodies[3]) != 4 || bodies[3]["operation_id"] != nil || string(bodies[3]["additional_microcredits"]) != "0" {
		t.Fatal("zero renewal contract changed", bodies)
	}
}

func TestHTTPBillingPositiveExtensionRequiresCompleteACK(t *testing.T) {
	usageID, holdID, walletID := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missingFields", func(m map[string]any) { clear(m) }},
		{"wrongHold", func(m map[string]any) { m["hold_id"] = uuid.New() }},
		{"wrongUsage", func(m map[string]any) { m["usage_request_id"] = uuid.New() }},
		{"missingWallet", func(m map[string]any) { delete(m, "wallet_id") }},
		{"missingMaximum", func(m map[string]any) { delete(m, "max_microcredits") }},
		{"negativeMaximum", func(m map[string]any) { m["max_microcredits"] = -1 }},
		{"maximumBelowAdditional", func(m map[string]any) { m["max_microcredits"] = 1 }},
		{"missingExpiry", func(m map[string]any) { delete(m, "expires_at") }},
		{"invalidPrice", func(m map[string]any) { m["price_version_id"] = uuid.Nil }},
		{"invalidActual", func(m map[string]any) { m["actual_microcredits"] = 11 }},
		{"settledZeroActual", func(m map[string]any) { m["actual_microcredits"] = 0 }},
		{"settledPositiveActual", func(m map[string]any) { m["actual_microcredits"] = 5 }},
		{"wrongState", func(m map[string]any) { m["status"] = "RELEASED" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				body := map[string]any{"hold_id": holdID, "usage_request_id": usageID, "wallet_id": walletID, "status": "ACTIVE", "max_microcredits": 10, "expires_at": time.Unix(1, 0).UTC()}
				tc.change(body)
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			client, err := NewHTTPBillingClient(server.URL, "controlled-extension")
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Extend(context.Background(), usageID, holdID, 5, uuid.New()); err == nil {
				t.Fatal("incomplete acknowledgement accepted")
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected HTTP retry", calls.Load())
			}
		})
	}
}

func TestFinalUsageExtensionIdentitySurvivesRecovery(t *testing.T) {
	usageID, holdID := uuid.New(), uuid.New()
	price := PriceVersion{OutputPerMillion: 1_000_000}
	first := &streamBillingFake{}
	repo := &streamRepositoryFake{}
	service := &Service{repository: repo, executionBilling: first}
	for i := 0; i < 2; i++ {
		// Re-created service and an unchanged frozen maximum model a recovered caller
		// after Billing ACK but before Token has durably projected that ACK.
		service = &Service{repository: repo, executionBilling: first}
		actual, notice, ok := service.authorizeAdditional(context.Background(), usageID, holdID, price, Usage{OutputTokens: 10}, 1)
		if !ok || actual != 10 || notice != "" {
			t.Fatal("authorization changed", actual, notice, ok)
		}
	}
	if first.extendCalls != 2 || first.extensionIDs[0] == uuid.Nil || first.extensionIDs[0] != first.extensionIDs[1] || repo.ensuredMaximum != 10 {
		t.Fatal("recovery created a new operation or added projection twice", first, repo)
	}
	service.authorizeAdditional(context.Background(), usageID, holdID, price, Usage{OutputTokens: 11}, 1)
	if first.extensionIDs[2] != first.extensionIDs[0] {
		t.Fatal("different facts silently created a second final slot")
	}
	for _, pair := range [][2]uuid.UUID{{uuid.New(), holdID}, {usageID, uuid.New()}} {
		if finalUsageReservationOperationID(pair[0], pair[1]) == first.extensionIDs[0] {
			t.Fatal("different durable owner reused the operation")
		}
	}
	failed := &streamBillingFake{extendErr: errors.New("controlled unknown ACK")}
	pending := &streamRepositoryFake{}
	service = &Service{repository: pending, executionBilling: failed}
	service.authorizeAdditional(context.Background(), usageID, holdID, price, Usage{OutputTokens: 10}, 1)
	if failed.extendCalls != 1 || pending.ensureCalls != 0 {
		t.Fatal("uncertain ACK was retried or projected", failed, pending)
	}
}
