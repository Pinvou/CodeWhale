package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
)

// Only persistence is replaced: requests use the installed route, real signed
// workload authentication/permissions, Adapt and handler. No PostgreSQL is used.
type walletProvisioningHTTPRepository struct {
	Repository
	mu           sync.Mutex
	receipt      TokenAccountWalletProvisioningReceipt
	posts, reads int
	err          error
}

func (r *walletProvisioningHTTPRepository) ProvisionTokenAccountWallet(_ context.Context, reference uuid.UUID, ownerType string, ownerID uuid.UUID, digest string) (TokenAccountWalletProvisioningReceipt, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts++
	if r.err != nil {
		return TokenAccountWalletProvisioningReceipt{}, false, r.err
	}
	if r.receipt.CommandReference != uuid.Nil {
		return r.receipt, false, nil
	}
	wallet := uuid.MustParse("01932d30-2c01-7000-8000-000000000003")
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	r.receipt = TokenAccountWalletProvisioningReceipt{CommandReference: reference, OwnerType: ownerType, OwnerID: ownerID, RequestDigest: digest, Status: "SUCCEEDED", WalletID: &wallet, AttemptCount: 1, CreatedAt: now, CompletedAt: &now}
	return r.receipt, true, nil
}
func (r *walletProvisioningHTTPRepository) TokenAccountWalletProvisioningReceipt(_ context.Context, reference uuid.UUID) (TokenAccountWalletProvisioningReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	if r.err != nil {
		return TokenAccountWalletProvisioningReceipt{}, r.err
	}
	if r.receipt.CommandReference != reference {
		return TokenAccountWalletProvisioningReceipt{}, ErrNotFound
	}
	return r.receipt, nil
}
func (r *walletProvisioningHTTPRepository) calls() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.posts, r.reads
}

func TestTokenWalletProvisioningHTTPDecodeContract(t *testing.T) {
	signed := tokenWalletWorkload(t) // Existing loopback TLS JWKS/RS256 fixture.
	reference := uuid.MustParse("01932d30-2c01-7000-8000-000000000001")
	owner := uuid.MustParse("01932d30-2c01-7000-8000-000000000002")
	digest := tokenAccountWalletRequestDigest("PERSONAL", owner)
	raw, err := json.Marshal(map[string]any{"command_reference": reference, "owner_type": "PERSONAL", "owner_id": owner, "request_digest": digest})
	if err != nil {
		t.Fatal(err)
	}
	valid := string(raw)
	path := "/internal/v1/token-account-wallet-provisioning-receipts"
	request := func(t *testing.T, server *httptest.Server, method, suffix, payload, subject string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path+suffix, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if credential := signed(subject); credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, body
	}
	assertError := func(t *testing.T, got, want int, header http.Header, body []byte, code, message string) {
		t.Helper()
		if got != want {
			t.Fatalf("HTTP status=%d want=%d", got, want)
		}
		if header.Get("Content-Type") != "application/json" || header.Get("Cache-Control") != "no-store" {
			t.Fatal("error response must be JSON/no-store")
		}
		var result api.Error
		if json.Unmarshal(body, &result) != nil || result.Code != code || (message != "" && result.Message != message) || result.Details != nil {
			t.Fatal("unexpected stable error envelope")
		}
		if strings.Contains(string(body), "private-input-marker") || len(body) > 512 {
			t.Fatal("request payload or decoder details leaked")
		}
	}
	for _, tc := range []struct{ name, payload string }{
		{"truncated", `{"owner_type":"private-input-marker"`},
		{"unknown-field", strings.TrimSuffix(valid, "}") + `,"private-input-marker":true}`},
		{"trailing-garbage", valid + ` private-input-marker`},
		{"multiple-values", valid + ` {"private-input-marker":true}`},
		{"wrong-field-type", strings.Replace(valid, `"owner_type":"PERSONAL"`, `"owner_type":["private-input-marker"]`, 1)},
		{"invalid-uuid", strings.Replace(valid, reference.String(), "private-input-marker", 1)},
		{"body-over-one-mebibyte", valid + strings.Repeat(" ", 1<<20)},
		{"null", `null`},
		{"nil-reference", strings.Replace(valid, reference.String(), uuid.Nil.String(), 1)},
		{"nil-owner", strings.Replace(valid, owner.String(), uuid.Nil.String(), 1)},
		{"unsupported-owner", strings.Replace(valid, "PERSONAL", "private-input-marker", 1)},
		{"wrong-digest", strings.Replace(valid, digest, "private-input-marker", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &walletProvisioningHTTPRepository{}
			server := httptest.NewServer(NewService(repo).Handler(""))
			defer server.Close()
			status, header, body := request(t, server, http.MethodPost, "", tc.payload, "execute")
			assertError(t, status, http.StatusUnprocessableEntity, header, body, "INVALID_WALLET_PROVISIONING_REQUEST", "wallet provisioning request is invalid")
			if posts, reads := repo.calls(); posts != 0 || reads != 0 {
				t.Fatalf("invalid request reached store: %d/%d", posts, reads)
			}
		})
	}
	t.Run("authentication-and-exact-permission-still-precede-decode", func(t *testing.T) {
		repo := &walletProvisioningHTTPRepository{}
		server := httptest.NewServer(NewService(repo).Handler(""))
		defer server.Close()
		for _, tc := range []struct {
			subject string
			status  int
			code    string
		}{{"", 401, "SERVICE_AUTH_REQUIRED"}, {"wrong", 403, "SERVICE_AUTH_FORBIDDEN"}, {"read", 403, "SERVICE_AUTH_FORBIDDEN"}} {
			status, header, body := request(t, server, http.MethodPost, "", `{"private-input-marker":true}`, tc.subject)
			assertError(t, status, tc.status, header, body, tc.code, "")
		}
		if posts, reads := repo.calls(); posts != 0 || reads != 0 {
			t.Fatal("unauthorized request reached store")
		}
	})
	t.Run("original-one-mebibyte-limit-is-not-reduced", func(t *testing.T) {
		repo := &walletProvisioningHTTPRepository{}
		server := httptest.NewServer(NewService(repo).Handler(""))
		defer server.Close()
		status, _, _ := request(t, server, http.MethodPost, "", valid+strings.Repeat(" ", (1<<20)-len(valid)), "execute")
		if status != http.StatusCreated {
			t.Fatalf("exactly 1MiB request status=%d", status)
		}
		if posts, reads := repo.calls(); posts != 1 || reads != 0 {
			t.Fatal("within-limit request not dispatched once")
		}
	})
	t.Run("actual-token-client-retains-invalid-request-status", func(t *testing.T) {
		repo := &walletProvisioningHTTPRepository{}
		server := httptest.NewServer(NewService(repo).Handler(""))
		defer server.Close()
		client, err := token.NewHTTPWalletResolver(server.URL, signed("token"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ProvisionTokenAccountWallet(context.Background(), uuid.Nil, "PERSONAL", owner, digest)
		var billingErr *token.BillingHTTPError
		if !errors.As(err, &billingErr) || billingErr.Status != http.StatusUnprocessableEntity {
			t.Fatal("actual Token client lost 422 rejection", err)
		}
		if posts, reads := repo.calls(); posts != 0 || reads != 0 {
			t.Fatal("invalid Token request reached store")
		}
	})
	t.Run("created-read-and-replay-actual-token-client", func(t *testing.T) {
		repo := &walletProvisioningHTTPRepository{}
		server := httptest.NewServer(NewService(repo).Handler(""))
		defer server.Close()
		status, header, body := request(t, server, http.MethodPost, "", valid, "execute")
		if status != http.StatusCreated || header.Get("Cache-Control") != "no-store" || header.Get("Content-Type") != "application/json" {
			t.Fatalf("creation HTTP contract: status=%d", status)
		}
		var first TokenAccountWalletProvisioningReceipt
		if json.Unmarshal(body, &first) != nil || first.CommandReference != reference || first.OwnerType != "PERSONAL" || first.OwnerID != owner || first.RequestDigest != digest || first.Status != "SUCCEEDED" || first.WalletID == nil || first.StableErrorCode != nil || first.AttemptCount != 1 || first.CompletedAt == nil {
			t.Fatal("created receipt facts changed")
		}
		status, _, body = request(t, server, http.MethodPost, "", valid, "execute")
		var replay TokenAccountWalletProvisioningReceipt
		if status != http.StatusOK || json.Unmarshal(body, &replay) != nil || !reflect.DeepEqual(first, replay) {
			t.Fatal("replay did not return original HTTP200 receipt")
		}
		client, err := token.NewHTTPWalletResolver(server.URL, signed("token"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.TokenAccountWalletProvisioningReceipt(context.Background(), reference)
		if err != nil || got.CommandReference != reference || got.OwnerID != owner || got.RequestDigest != digest || got.WalletID == nil || *got.WalletID != *first.WalletID || got.Status != first.Status {
			t.Fatal("actual Token client could not consume receipt", err)
		}
		replayed, err := client.ProvisionTokenAccountWallet(context.Background(), reference, "PERSONAL", owner, digest)
		if err != nil || !reflect.DeepEqual(got, replayed) {
			t.Fatal("actual Token client replay changed", err)
		}
		if posts, reads := repo.calls(); posts != 3 || reads != 1 {
			t.Fatalf("unexpected request count %d/%d", posts, reads)
		}
	})
	for _, tc := range []struct {
		name, code string
		storeErr   error
		status     int
	}{
		{"store-unavailable", "WALLET_PROVISIONING_UNAVAILABLE", errors.New("private-input-marker"), 503},
		{"receipt-mismatch", "WALLET_PROVISIONING_RECEIPT_MISMATCH", ErrConflict, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &walletProvisioningHTTPRepository{err: tc.storeErr}
			server := httptest.NewServer(NewService(repo).Handler(""))
			defer server.Close()
			status, header, body := request(t, server, http.MethodPost, "", valid, "execute")
			assertError(t, status, tc.status, header, body, tc.code, "")
			client, err := token.NewHTTPWalletResolver(server.URL, signed("token"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ProvisionTokenAccountWallet(context.Background(), reference, "PERSONAL", owner, digest)
			var billingErr *token.BillingHTTPError
			if !errors.As(err, &billingErr) || billingErr.Status != tc.status {
				t.Fatal("actual Token client lost supplier failure status", err)
			}
			if posts, reads := repo.calls(); posts != 2 || reads != 0 {
				t.Fatal("unexpected store calls")
			}
		})
	}
	t.Run("missing-store-capability-remains-unavailable", func(t *testing.T) {
		server := httptest.NewServer(NewService(nil).Handler(""))
		defer server.Close()
		status, header, body := request(t, server, http.MethodPost, "", valid, "execute")
		assertError(t, status, http.StatusServiceUnavailable, header, body, "WALLET_PROVISIONING_UNAVAILABLE", "")
	})
}
