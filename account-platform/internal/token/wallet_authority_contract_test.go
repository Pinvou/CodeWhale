package token

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestBalanceRequiresExactWalletAndExplicitIntegerAmount(t *testing.T) {
	wallet, owner := uuid.New(), uuid.New()
	base := func(amount string) []byte {
		return []byte(fmt.Sprintf(`{"wallet_id":%q,"owner_type":"PERSONAL","owner_id":%q,"status":"FROZEN","version":1,"available_microcredits":%s,"extension":true}`, wallet, owner, amount))
	}
	for _, tc := range []struct {
		name, amount string
		valid        bool
	}{
		{"zero", "0", true},
		{"negative-projection", "-1", true},
		{"int64-maximum", "9223372036854775807", true},
		{"int64-minimum", "-9223372036854775808", true},
		{"overflow", "9223372036854775808", false},
		{"fraction", "1.5", false},
		{"string", `"123"`, false},
		{"null", "null", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateWalletFacts(base(tc.amount), wallet, owner, true)
			if (err == nil) != tc.valid {
				t.Fatalf("accepted=%v want=%v: %v", err == nil, tc.valid, err)
			}
		})
	}
	var facts map[string]any
	if err := json.Unmarshal(base("0"), &facts); err != nil {
		t.Fatal(err)
	}
	delete(facts, "available_microcredits")
	missing, _ := json.Marshal(facts)
	if _, err := validateWalletFacts(missing, wallet, owner, true); err == nil {
		t.Fatal("accepted an absent amount as zero")
	}
	if _, err := validateWalletFacts(base("0"), wallet, uuid.New(), true); err == nil {
		t.Fatal("accepted another owner's balance")
	}
}

func TestEnsurePersonalWalletRequiresAcknowledgedOwnerAndState(t *testing.T) {
	wallet, owner := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, field string
		value       any
		remove      bool
	}{
		{"missing-owner", "owner_id", nil, true},
		{"foreign-owner", "owner_id", uuid.New(), false},
		{"organizational-owner", "owner_type", "ORG", false},
		{"missing-version", "version", nil, true},
		{"zero-version", "version", 0, false},
		{"unknown-status", "status", "PROVISIONING", false},
		{"nil-wallet", "wallet_id", uuid.Nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"wallet_id": wallet, "owner_type": "PERSONAL", "owner_id": owner, "status": "ACTIVE", "version": 1}
			if tc.remove {
				delete(body, tc.field)
			} else {
				body[tc.field] = tc.value
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			resolver, err := NewHTTPWalletResolver(server.URL, "fixture-service")
			if err != nil {
				t.Fatal(err)
			}
			if id, err := resolver.Ensure(context.Background(), "PERSONAL", owner); err == nil || id != uuid.Nil {
				t.Fatalf("accepted invalid ensure facts: %s, %v", id, err)
			}
		})
	}
}
