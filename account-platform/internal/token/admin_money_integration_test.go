package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestPostgresNewUsageReadsKeepExactMoneyAndNullWithoutChangingExistingStatus(t *testing.T) {
	ctx := context.Background()
	repo, usage := settlementFixture(t)
	const amount int64 = 9007199254740993
	if _, err := repo.pool.Exec(ctx, `UPDATE usage_requests SET maximum_microcredits=$2 WHERE id=$1`, usage.ID, amount); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkCommitted(ctx, usage.ID, amount, map[string]any{"usage": Usage{InputTokens: 3}}); err != nil {
		t.Fatal(err)
	}
	pending := usage
	pending.ID, pending.IdempotencyKey, pending.MaximumMicrocredits = uuid.New(), uuid.NewString(), amount
	if _, created, err := repo.BeginUsage(ctx, pending, map[string]any{}); err != nil || !created {
		t.Fatal("missing unknown-amount usage fixture")
	}
	service := NewService(repo, fixtureContexts{AuthorizationContext{UserID: usage.UserID, SessionID: usage.SessionID, ContextType: "PERSONAL"}}, nil, nil, ProviderURLPolicy{}, nil, nil, 100)
	server := httptest.NewServer(service.Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{RealmRoles: []string{"platform-admin"}}, nil
	}))
	defer server.Close()
	read := func(path string) map[string]any {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer fixture-access-token")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		decoder := json.NewDecoder(response.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil || response.StatusCode != http.StatusOK {
			t.Fatal("usage HTTP fixture failed")
		}
		return body
	}
	for _, test := range []struct{ path, id, maximum, actual string }{{"/admin/v1/usage-requests", "usage_request_id", "maximum_microcredits", "actual_microcredits"}, {"/v1/me/usage-requests", "usage_request_id", "", "actual_microcredits"}} {
		t.Run(test.path, func(t *testing.T) {
			items, ok := read(test.path)["items"].([]any)
			if !ok || len(items) != 2 {
				t.Fatal("missing usage records")
			}
			for _, value := range items {
				item := value.(map[string]any)
				if test.maximum != "" {
					for _, field := range []string{"usage_request_id", "model", "status", "maximum_microcredits", "actual_microcredits", "error_class", "started_at", "updated_at"} {
						if _, present := item[field]; !present {
							t.Fatal("admin usage omitted a field consumed by the operations UI")
						}
					}
					if _, present := item["IdempotencyKey"]; present {
						t.Fatal("admin usage leaked an untagged internal field")
					}
				}
				if test.maximum != "" && item[test.maximum] != "9007199254740993" {
					t.Fatal("usage maximum was not exact")
				}
				if item[test.id] == usage.ID.String() {
					if item[test.actual] != "9007199254740993" {
						t.Fatal("usage actual was not exact")
					}
				} else if item[test.id] == pending.ID.String() {
					if item[test.actual] != nil {
						t.Fatal("unknown amount did not remain null")
					}
				} else {
					t.Fatal("unexpected usage")
				}
			}
		})
	}
	body := read("/v1/usage-requests/" + usage.ID.String())
	if body["actual_microcredits"] != json.Number("9007199254740993") {
		t.Fatal("existing usage status contract changed")
	}
}
