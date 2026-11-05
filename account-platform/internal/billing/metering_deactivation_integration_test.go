package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestMeteringDeactivationReceiptAuditAndLegacyHTTP(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_AUTH_ISSUER", "")
	repo, ctx := retainedFinanceRepository(t)
	actor := uuid.New()
	create := func() MeteringUnitDefinition {
		t.Helper()
		item, err := repo.CreateMeteringUnitVersion(ctx, MeteringUnitDefinition{UnitCode: "TEST_" + uuid.NewString(), MinimumPrecision: 1, PriceDenominator: 100, RoundingRule: "CEILING"}, actor, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	t.Run("concurrent-same-key-replays-one-mutation-and-audit", func(t *testing.T) {
		item, key := create(), uuid.NewString()
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 8)
		results := make(chan MeteringUnitDefinition, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				result, err := repo.DeactivateMeteringUnit(ctx, item.ID, actor, key)
				errs <- err
				results <- result
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		close(results)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var first MeteringUnitDefinition
		for result := range results {
			if first.ID == uuid.Nil {
				first = result
			}
			if result.Status != "INACTIVE" || result.DeactivatedAt == nil || first.DeactivatedAt == nil || !result.DeactivatedAt.Equal(*first.DeactivatedAt) {
				t.Fatal("unstable deactivation receipt")
			}
		}
		other := create()
		if _, err := repo.DeactivateMeteringUnit(ctx, other.ID, actor, key); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("changed target reused key: %v", err)
		}
		repeat, err := repo.DeactivateMeteringUnit(ctx, item.ID, actor, uuid.NewString())
		if err != nil || !repeat.DeactivatedAt.Equal(*first.DeactivatedAt) {
			t.Fatal("no-op changed timestamp", err)
		}
		var audits, receipts int
		var auditActor uuid.UUID
		var before, after MeteringUnitDefinition
		var beforeJSON, afterJSON []byte
		err = repo.pool.QueryRow(ctx, `SELECT actor_user_id,before_snapshot,after_snapshot FROM pricing_audits WHERE action='UNIT_DEACTIVATED' AND after_snapshot->>'unit_definition_id'=$1`, item.ID.String()).Scan(&auditActor, &beforeJSON, &afterJSON)
		if err != nil || json.Unmarshal(beforeJSON, &before) != nil || json.Unmarshal(afterJSON, &after) != nil || auditActor != actor || before.Status != "ACTIVE" || after.Status != "INACTIVE" || before.ID != item.ID || after.ID != item.ID {
			t.Fatal("audit lost verified actor or unit facts", err)
		}
		err = repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pricing_audits WHERE action='UNIT_DEACTIVATED' AND after_snapshot->>'unit_definition_id'=$1),(SELECT count(*) FROM pricing_operation_commands WHERE operation='UNIT_DEACTIVATE' AND response_snapshot->>'unit_definition_id'=$1)`, item.ID.String()).Scan(&audits, &receipts)
		if err != nil || audits != 1 || receipts != 2 {
			t.Fatalf("effects/receipts %d/%d: %v", audits, receipts, err)
		}
	})
	t.Run("different-actors-serialize-and-missing-target-rolls-back", func(t *testing.T) {
		unit := create()
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := repo.DeactivateMeteringUnit(ctx, unit.ID, uuid.New(), uuid.NewString())
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var audits, receipts int
		if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pricing_audits WHERE action='UNIT_DEACTIVATED' AND after_snapshot->>'unit_definition_id'=$1),(SELECT count(*) FROM pricing_operation_commands WHERE operation='UNIT_DEACTIVATE' AND response_snapshot->>'unit_definition_id'=$1)`, unit.ID.String()).Scan(&audits, &receipts); err != nil || audits != 1 || receipts != 2 {
			t.Fatalf("different actor effects %d/%d: %v", audits, receipts, err)
		}
		key := uuid.NewString()
		if _, err := repo.DeactivateMeteringUnit(ctx, uuid.New(), actor, key); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing unit did not fail closed", err)
		}
		if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM pricing_operation_commands WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, key).Scan(&receipts); err != nil || receipts != 0 {
			t.Fatal("failed transaction left receipt", err)
		}
	})
	t.Run("registered-empty-body-header-and-no-header", func(t *testing.T) {
		account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Subject, Permission string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Subject != actor.String() || body.Permission != "platform.pricing.manage" {
				t.Error("unverified authority request")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor.String()})
		}))
		defer account.Close()
		authorizer, err := authorization.NewClient(account.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		handler := NewService(repo).WithAuthorizer(authorizer).Handler("controlled-service", func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled", Subject: actor.String()}, nil
		})
		unit := create()
		path := "/admin/v1/metering-units/" + unit.ID.String() + "/deactivate"
		call := func(auth, key string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, path, nil)
			if auth != "" {
				request.Header.Set("Authorization", "Bearer "+auth)
			}
			if key != "" {
				request.Header.Set("Idempotency-Key", key)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		}
		if got := call("", ""); got.Code != 401 {
			t.Fatalf("unauthenticated status %d", got.Code)
		}
		key := uuid.NewString()
		first := call("controlled", key)
		replay := call("controlled", key)
		if first.Code != 200 || replay.Code != 200 || first.Body.String() != replay.Body.String() {
			t.Fatalf("HTTP receipt mismatch %d/%d", first.Code, replay.Code)
		}
		if got := call("controlled", ""); got.Code != 200 || got.Body.String() != first.Body.String() {
			t.Fatalf("legacy no-key status %d", got.Code)
		}
		if got := call("controlled", "short"); got.Code != 409 {
			t.Fatalf("malformed key status %d", got.Code)
		}
	})
}
