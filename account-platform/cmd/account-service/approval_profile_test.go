package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/go-jose/go-jose/v4"
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
			if clients.billingActivation == nil || clients.prechecks == nil || clients.proofs == nil || clients.results == nil || clients.effects == nil {
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

type f09PrecheckAccountRepository struct {
	account.Repository
	userID  uuid.UUID
	allowed bool
}

func (r *f09PrecheckAccountRepository) GetAuthorizationVersions(context.Context, identity.Principal) (account.AuthorizationVersions, error) {
	return account.AuthorizationVersions{ContextVersion: 1, AuthorizationVersion: 1, PersonalAuthorizationVersion: 1, PlatformAuthorizationVersion: 1}, nil
}

func (r *f09PrecheckAccountRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _, _ *uuid.UUID) (account.AuthorizationDecision, error) {
	return account.AuthorizationDecision{UserID: r.userID, UserStatus: "ACTIVE", Allowed: r.allowed && permission == "platform.billing.repairs.request", Permission: permission, AuthorizationVersion: 1}, nil
}

type f09PrecheckBillingRepository struct {
	billing.Repository
	input    approvalencoding.LegacyProofInput
	orderID  uuid.UUID
	calls    atomic.Int64
	rejected bool
}

func (r *f09PrecheckBillingRepository) EntitlementCorrectionProofInput(_ context.Context, packageID, orderID uuid.UUID, reason string) (approvalencoding.LegacyProofInput, error) {
	r.calls.Add(1)
	if r.rejected || packageID.String() != r.input.TargetResourceID || orderID != r.orderID || reason != "PRODUCT_SPLIT_CORRECTION" {
		return approvalencoding.LegacyProofInput{}, billing.ErrConflict
	}
	return r.input, nil
}

// Controlled repository facts and a real TLS/JWKS workload verifier exercise
// the production constructor, installation helper and both registered Handlers.
// This is not process startup, a live issuer or financial database execution.
func TestF09ApprovalRuntimeRegisteredPrechecks(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "f09-controlled", Algorithm: "RS256", Use: "sig"}}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(jwks.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/issuer"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL+"/jwks")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "f09-precheck")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"account","service":"account-service","permissions":["internal.billing.approval-proof.read"]},{"subject":"wrong-permission","service":"account-service","permissions":["internal.billing.wallet.read"]},{"subject":"wrong-service","service":"token-gateway","permissions":["internal.billing.approval-proof.read"]}]`)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "f09-controlled"))
	if err != nil {
		t.Fatal(err)
	}
	token := func(subject string) string {
		t.Helper()
		now := time.Now()
		raw, err := json.Marshal(map[string]any{"iss": issuer, "aud": "f09-precheck", "sub": subject, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		signed, err := signer.Sign(raw)
		if err != nil {
			t.Fatal(err)
		}
		value, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	packageID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	orderID := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	normalized := json.RawMessage(`{"package_id":"00000000-0000-4000-8000-000000000001","payment_order_id":"00000000-0000-4000-8000-000000000002","reason_code":"PRODUCT_SPLIT_CORRECTION"}`)
	critical := json.RawMessage(`{"package_id":"00000000-0000-4000-8000-000000000001","wallet_id":"00000000-0000-4000-8000-000000000003","wallet_version":9007199254740993,"payment_order_id":"00000000-0000-4000-8000-000000000002","payment_transaction_id":"00000000-0000-4000-8000-000000000004","source_component_id":"00000000-0000-4000-8000-000000000005","source_lot_id":"00000000-0000-4000-8000-000000000006","target_component_id":null,"target_lot_id":null,"source_type":"BASE","target_type":"BONUS","amount_microcredits":9007199254740993,"source_evidence_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","facts_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","policy_version":2}`)
	financial := json.RawMessage(`{"package_total_microcredits":9223372036854775807,"source_original_microcredits":9223372036854775807,"target_original_microcredits":0,"effective_total_microcredits":9223372036854775807}`)
	body := `{"command_type":"ENTITLEMENT_COMPONENT_ADJUSTMENT","target_resource_id":"` + packageID.String() + `","payment_order_id":"` + orderID.String() + `","reason_code":"PRODUCT_SPLIT_CORRECTION"}`
	for _, tc := range []struct {
		name, subject                                      string
		profile                                            config.AccountPlatformProfile
		allowed, authenticated, stale, malformed, rejected bool
		status                                             int
		ownerCalls                                         int64
	}{
		{"personal-owner-projection", "account", config.AccountPlatformProfilePersonal, true, true, false, false, false, 200, 1},
		{"models-owner-projection", "account", config.AccountPlatformProfileModels, true, true, false, false, false, 200, 1},
		{"wrong-workload-permission", "wrong-permission", config.AccountPlatformProfilePersonal, true, true, false, false, false, 503, 0},
		{"wrong-workload-service", "wrong-service", config.AccountPlatformProfilePersonal, true, true, false, false, false, 503, 0},
		{"no-browser-session", "account", config.AccountPlatformProfilePersonal, true, false, false, false, false, 401, 0},
		{"stale-browser-session", "account", config.AccountPlatformProfilePersonal, true, true, true, false, false, 401, 0},
		{"permission-denied", "account", config.AccountPlatformProfilePersonal, false, true, false, false, false, 403, 0},
		{"closed-input-rejection", "account", config.AccountPlatformProfilePersonal, true, true, false, true, false, 400, 0},
		{"owner-rejected-target", "account", config.AccountPlatformProfilePersonal, true, true, false, false, true, 422, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := &f09PrecheckBillingRepository{orderID: orderID, rejected: tc.rejected, input: approvalencoding.LegacyProofInput{CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", PolicyVersion: 2, SnapshotSchemaVersion: 2, TargetResourceType: "ENTITLEMENT_PACKAGE", TargetResourceID: packageID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}}
			downstream := httptest.NewServer(billing.NewService(owner).Handler(""))
			defer downstream.Close()
			tokenURL, policy, handlers := "", "", ""
			if tc.profile == config.AccountPlatformProfileModels {
				tokenURL = "https://token.invalid"
				policy = strings.Repeat("a", 64)
				handlers = strings.Repeat("b", 64)
			}
			clients, err := newApprovalRuntimeClients(tc.profile, downstream.URL, tokenURL, token(tc.subject), policy, handlers)
			if err != nil {
				t.Fatal(err)
			}
			service := account.NewService(&f09PrecheckAccountRepository{userID: uuid.New(), allowed: tc.allowed}, nil)
			installApprovalRuntimeClients(tc.profile, &account.PostgresRepository{}, service, clients)
			authTime := time.Now()
			if tc.stale {
				authTime = authTime.Add(-16 * time.Minute)
			}
			handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "controlled-browser", Subject: "operator", SessionID: "session", AuthenticatedAt: authTime}, nil
			})
			requestBody := body
			if tc.malformed {
				requestBody = strings.TrimSuffix(body, "}") + `,"amount_microcredits":1}`
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/v2/approval-command-prechecks", strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			if tc.authenticated {
				request.Header.Set("Authorization", "Bearer browser-controlled")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status || owner.calls.Load() != tc.ownerCalls {
				t.Fatalf("status=%d ownerReads=%d body=%s", response.Code, owner.calls.Load(), response.Body.String())
			}
			if tc.status == 200 {
				var got struct{ Normalized, Critical, Financial json.RawMessage }
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				got.Normalized = wire["normalized_command"]
				got.Critical = wire["critical_resource_versions"]
				got.Financial = wire["financial_snapshot"]
				if string(got.Normalized) != string(normalized) || string(got.Critical) != string(critical) || string(got.Financial) != string(financial) || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("owner facts/large integers or no-store changed")
				}
			}
			if strings.Contains(response.Body.String(), "wrong-permission") || strings.Contains(response.Body.String(), "private database") {
				t.Fatal("internal detail escaped")
			}
		})
	}
	// The original missing-authority contract remains a closed failure.
	if _, err := newApprovalRuntimeClients(config.AccountPlatformProfilePersonal, "", "", "controlled", "", ""); err == nil || errors.Is(err, context.Canceled) {
		t.Fatal("missing Billing authority accepted")
	}
}
