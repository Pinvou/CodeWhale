package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/google/uuid"
)

func TestF04ApprovalProfileClients(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name                                        string
		profile                                     config.AccountPlatformProfile
		billing, token, credential, policy, handler string
		valid                                       bool
	}{
		{"personal-no-models", config.AccountPlatformProfilePersonal, "https://billing.invalid", "", "controlled", "", "", true},
		{"personal-explicit-ignores-model-config", config.AccountPlatformProfilePersonal, "https://billing.invalid", "bad token URL", "controlled", "bad", "bad", true},
		{"personal-missing-billing", config.AccountPlatformProfilePersonal, "", "", "controlled", "", "", false},
		{"personal-missing-credential", config.AccountPlatformProfilePersonal, "https://billing.invalid", "", "", "", "", false},
		{"models-full", config.AccountPlatformProfileModels, "https://billing.invalid", "https://token.invalid", "controlled", validDigest, validDigest, true},
		{"models-missing-token", config.AccountPlatformProfileModels, "https://billing.invalid", "", "controlled", validDigest, validDigest, false},
		{"models-missing-policy-digest", config.AccountPlatformProfileModels, "https://billing.invalid", "https://token.invalid", "controlled", "", validDigest, false},
		{"models-missing-handler-digest", config.AccountPlatformProfileModels, "https://billing.invalid", "https://token.invalid", "controlled", validDigest, "", false},
		{"unknown-never-falls-back", config.AccountPlatformProfile("unknown"), "https://billing.invalid", "", "controlled", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clients, err := newApprovalRuntimeClients(tc.profile, tc.billing, tc.token, tc.credential, tc.policy, tc.handler)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !tc.valid {
				return
			}
			if clients.billingActivation == nil || clients.proofs == nil || clients.results == nil || clients.effects == nil {
				t.Fatal("Billing runtime incomplete")
			}
			if tc.profile == config.AccountPlatformProfilePersonal {
				if clients.tokenActivation != nil || clients.snapshots != nil {
					t.Fatal("personal installed Token activation/snapshots")
				}
			} else if clients.tokenActivation == nil || clients.snapshots == nil {
				t.Fatal("models governance incomplete")
			}
		})
	}
	// Blank and unknown selectors cannot silently choose Billing-only clients.
	for _, value := range []string{"", "models"} {
		t.Run("strict-selector-"+value, func(t *testing.T) {
			profile, err := config.ParseAccountPlatformProfile(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = newApprovalRuntimeClients(profile, "https://billing.invalid", "", "controlled", "", ""); err == nil {
				t.Fatal("missing model dependencies downgraded")
			}
		})
	}
}

func TestF04PersonalApprovalAuthorityRouting(t *testing.T) {
	paths := make(chan string, 4)
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controlled" {
			t.Error("workload credential missing")
		}
		paths <- r.URL.Path
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer billing.Close()
	clients, err := newApprovalRuntimeClients(config.AccountPlatformProfilePersonal, billing.URL, "", "controlled", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = clients.billingActivation.Snapshot(context.Background()); err == nil {
		t.Fatal("503 activation accepted")
	}
	if _, err = clients.effects.Observe(context.Background(), uuid.New(), "WALLET_ADJUSTMENT", strings.Repeat("b", 64)); err == nil {
		t.Fatal("503 effect accepted")
	}
	if len(paths) != 2 {
		t.Fatalf("Billing route count=%d", len(paths))
	}
	if first, second := <-paths, <-paths; first != "/internal/v1/approval-command-activations" || !strings.HasPrefix(second, "/internal/v1/approval-effects/") {
		t.Fatalf("Billing routes=%s,%s", first, second)
	}
	// The Billing-only constructor must not route an absent Token authority to Billing.
	if _, err = clients.effects.Observe(context.Background(), uuid.New(), "TOKEN_CREDENTIAL_REVOKE", strings.Repeat("b", 64)); err == nil || len(paths) != 0 {
		t.Fatal("personal substituted Token authority")
	}
}
