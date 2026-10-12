package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type closedGrantInputClient struct {
	BillingClient
	OrganizationBillingClient
	ids     []uuid.UUID
	keys    []string
	reasons []string
}

func (c *closedGrantInputClient) ReclaimAccountClosedGrant(_ context.Context, org, id, _ uuid.UUID, reason, key string) (json.RawMessage, error) {
	c.ids = append(c.ids, id)
	c.keys = append(c.keys, key)
	c.reasons = append(c.reasons, reason)
	if id == uuid.Nil {
		return nil, ErrNotFound
	}
	return json.RawMessage(fmt.Sprintf(`{"grant":{"grant_id":%q,"organization_id":%q,"status":"RECLAIMED_ACCOUNT_CLOSED"},"amount_microcredits":1,"subject_lifecycle_version":2,"replayed":false}`, id, org)), nil
}
func TestClosedGrantInputsThroughInstalledTokenRoutes(t *testing.T) {
	org, user, id, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tests := []struct {
		name        string
		batch       bool
		ids         []uuid.UUID
		reason      string
		want, calls int
	}{
		{"single167", false, nil, strings.Repeat("理", 167), 200, 1},
		{"single500", false, nil, strings.Repeat("理", 500), 200, 1},
		{"single501", false, nil, strings.Repeat("理", 501), 400, 0},
		{"batch500", true, []uuid.UUID{id, other}, strings.Repeat("理", 500), 207, 2},
		{"batch501", true, []uuid.UUID{id, other}, strings.Repeat("理", 501), 400, 0},
		{"duplicate", true, []uuid.UUID{id, id}, "回收", 400, 0},
		{"nilgrant-per-item", true, []uuid.UUID{id, uuid.Nil}, "回收", 207, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service, _, close := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: user, ContextType: "ORGANIZATION", OrganizationID: &org})
			defer close()
			client := &closedGrantInputClient{}
			service.billing = client
			handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "owned", Subject: "owner"}, nil
			})
			path := "/v1/organizations/" + org.String() + "/account-closed-grants/" + id.String() + "/reclaim"
			body := map[string]any{"reason": tc.reason}
			if tc.batch {
				path = "/v1/organizations/" + org.String() + "/account-closed-grants:reclaim-batch"
				body["grant_ids"] = tc.ids
			}
			raw, _ := json.Marshal(body)
			for attempt := 0; attempt < 2; attempt++ {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
				req.Header.Set("Authorization", "Bearer owned")
				req.Header.Set("Idempotency-Key", "closed-grant-owned-key")
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, req)
				if out.Code != tc.want {
					t.Fatalf("HTTP=%d want=%d body=%s", out.Code, tc.want, out.Body.String())
				}
				if len(client.ids) != (attempt+1)*tc.calls {
					t.Fatalf("Billing calls=%d want=%d", len(client.ids), (attempt+1)*tc.calls)
				}
				if tc.calls > 0 && client.reasons[len(client.reasons)-1] != tc.reason {
					t.Fatal("reason mutated")
				}
			}
			if tc.batch && tc.calls > 0 {
				for i, grant := range tc.ids {
					digest := sha256.Sum256([]byte(fmt.Sprintf("account-closed-reclaim-batch-v1\n%s\n%d\n%s", "closed-grant-owned-key", i, grant)))
					want := hex.EncodeToString(digest[:])
					if client.keys[i] != want || client.keys[i+tc.calls] != want {
						t.Fatal("stable original subkey algorithm lost")
					}
				}
			}
		})
	}
}

func TestClosedGrantKeysUsePublishedCharacterBounds(t *testing.T) {
	org, user, id := uuid.New(), uuid.New(), uuid.New()
	for _, keyCase := range []struct {
		name, key string
		allowed   bool
	}{
		{"ascii16", strings.Repeat("a", 16), true}, {"ascii128", strings.Repeat("a", 128), true},
		{"unicode8", strings.Repeat("键", 8), false}, {"unicode16", strings.Repeat("键", 16), true},
		{"unicode128", strings.Repeat("键", 128), true}, {"unicode129", strings.Repeat("键", 129), false},
	} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%v", keyCase.name, batch), func(t *testing.T) {
				service, _, close := newModelUsageAuthorizationService(t, AuthorizationContext{UserID: user, ContextType: "ORGANIZATION", OrganizationID: &org})
				defer close()
				client := &closedGrantInputClient{}
				service.billing = client
				handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
					return identity.Principal{Issuer: "owned", Subject: "owner"}, nil
				})
				path := "/v1/organizations/" + org.String() + "/account-closed-grants/" + id.String() + "/reclaim"
				body := map[string]any{"reason": "回收"}
				want, calls := 400, 0
				if keyCase.allowed {
					want, calls = 200, 1
				}
				if batch {
					path = "/v1/organizations/" + org.String() + "/account-closed-grants:reclaim-batch"
					body["grant_ids"] = []uuid.UUID{id}
					if keyCase.allowed {
						want = 207
					}
				}
				raw, _ := json.Marshal(body)
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
				r.Header.Set("Authorization", "Bearer owned")
				r.Header.Set("Idempotency-Key", keyCase.key)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != want || len(client.ids) != calls {
					t.Fatalf("status=%d calls=%d expected %d/%d", w.Code, len(client.ids), want, calls)
				}
				if keyCase.allowed && !batch && client.keys[0] != keyCase.key {
					t.Fatal("original Unicode key altered")
				}
				if keyCase.allowed && batch {
					digest := sha256.Sum256([]byte(fmt.Sprintf("account-closed-reclaim-batch-v1\n%s\n%d\n%s", keyCase.key, 0, id)))
					if client.keys[0] != hex.EncodeToString(digest[:]) {
						t.Fatal("Unicode source key changed original SHA subkey")
					}
				}
			})
		}
	}
}
