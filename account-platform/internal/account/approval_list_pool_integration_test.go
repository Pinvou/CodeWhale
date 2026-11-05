package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The ordinary public Up/Verify fixture owns its UUID database and genuine
// runtime/writer roles. The repository under test is constructed by the real
// production constructor, including its fixed 20-connection runtime pool.
func approvalListPoolFixture(t *testing.T) (*PostgresRepository, *retainedWalletFenceFixture, *pgx.Conn) {
	t.Helper()
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	installed := f.repository(t)
	u, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + installed.pool.Config().ConnConfig.Database
	u.User = url.User("account_service")
	q := u.Query()
	q.Set("application_name", "approval-list-pool-"+uuid.NewString())
	u.RawQuery = q.Encode()
	r, err := NewPostgresRepository(f.ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	if r.pool.Config().MaxConns != 20 {
		t.Fatal("test did not exercise the production 20-connection configuration")
	}
	u.User = url.User("account_approval_writer")
	if err := r.ConfigureApprovalWriter(f.ctx, u.String()); err != nil {
		t.Fatal("genuine approval writer readiness", err)
	}
	r.WithTokenGovernanceActivation(&tokenRecentAuthActivation{active: true})
	c := f.config.Copy()
	c.Database = installed.pool.Config().ConnConfig.Database
	observer, err := pgx.ConnectConfig(f.ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Close(context.Background()) })
	return r, f, observer
}

func TestApprovalListPoolRuntimePostgres(t *testing.T) {
	r, f, observer := approvalListPoolFixture(t)
	ctx := f.ctx
	owner := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "list-pool-owner")
	p := entitlementApprovalPrincipal(owner)
	submitted, _ := wallClockSubmitted(t, r, ctx, p)

	t.Run("twenty-concurrent-production-lists-release-rows-before-authorization", func(t *testing.T) {
		gate, err := observer.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(context.Background())
		if _, err := gate.Exec(ctx, `LOCK TABLE approval_requests IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		type result struct {
			items []ApprovalRequest
			err   error
		}
		results := make(chan result, 20)
		for i := 0; i < 20; i++ {
			go func() {
				items, err := r.ListApprovalRequests(requestCtx, p, ApprovalFilter{Limit: 1})
				results <- result{items, err}
			}()
		}
		app := r.pool.Config().ConnConfig.RuntimeParams["application_name"]
		// The independent admin connection observes every real list SQL waiting
		// on the table gate. No fake pool, reduced MaxConns, or timing-only race.
		wallClockWait(t, ctx, f.admin, `SELECT count(*)=20 FROM pg_stat_activity WHERE application_name=$1 AND datname=$2 AND wait_event_type='Lock' AND query LIKE '%FROM approval_requests ar%'`, app, r.pool.Config().ConnConfig.Database)
		if r.pool.Stat().AcquiredConns() != 20 {
			t.Fatal("table gate did not occupy all twenty runtime connections")
		}
		if err := gate.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		completed := 0
		observation := time.NewTimer(500 * time.Millisecond)
		defer observation.Stop()
		for completed < 20 {
			select {
			case got := <-results:
				completed++
				if got.err != nil || len(got.items) != 1 || got.items[0].ID != submitted.ID {
					t.Errorf("real concurrent list failed: count=%d err=%v", len(got.items), got.err)
				}
			case <-observation.C:
				var snapshot string
				if err := f.admin.QueryRow(ctx, `SELECT COALESCE(json_agg(json_build_object('pid',pid,'state',state,'wait_type',wait_event_type,'wait',wait_event,'last_sql_is_list',query LIKE '%FROM approval_requests ar%')),'[]')::text FROM pg_stat_activity WHERE application_name=$1 AND datname=$2 AND usename='account_service'`, app, r.pool.Config().ConnConfig.Database).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				t.Logf("actual runtime pool: acquired=%d max=%d completed=%d sessions=%s", r.pool.Stat().AcquiredConns(), r.pool.Config().MaxConns, completed, snapshot)
			}
		}
		if r.pool.Stat().AcquiredConns() != 0 {
			t.Fatal("list left checked-out connections")
		}
	})

	t.Run("v1-http-filter-order-limits-and-public-projection", func(t *testing.T) {
		for i := 0; i < 204; i++ {
			draft := tokenRecentAuthDraft("TOKEN_CREDENTIAL_REVOKE", time.Now().UTC())
			r.WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft})
			if _, err := r.CreateApprovalDraft(ctx, p, draft); err != nil {
				t.Fatal("create genuine draft", i, err)
			}
		}
		h := userCodeHandler(r, map[string]identity.Principal{"list-owner": p})
		for _, check := range []struct {
			query string
			count int
		}{{"?limit=200", 200}, {"", 50}, {"?limit=201", 200}, {"?status=SUBMITTED&limit=200", 1}} {
			request := httptest.NewRequest(http.MethodGet, "/admin/v1/approval-requests"+check.query, nil)
			request.Header.Set("Authorization", "Bearer list-owner")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			var body struct {
				Items []ApprovalRequest `json:"items"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Items) != check.count {
				t.Fatalf("v1 list wire count/status: query=%s code=%d count=%d", check.query, response.Code, len(body.Items))
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("public list cache boundary changed")
			}
			var projection struct {
				Items []map[string]json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
				t.Fatal(err)
			}
			for _, item := range projection.Items {
				for _, private := range []string{"requester_permission", "approver_permission", "snapshot_schema_version"} {
					if _, present := item[private]; present {
						t.Fatal("private top-level projection field became public", private)
					}
				}
			}
			for i := 1; i < len(body.Items); i++ {
				a, b := body.Items[i-1], body.Items[i]
				if a.UpdatedAt.Before(b.UpdatedAt) || a.UpdatedAt.Equal(b.UpdatedAt) && a.ID.String() > b.ID.String() {
					t.Fatal("original updated_at descending/id ascending order changed")
				}
			}
			if check.count == 1 && (body.Items[0].ID != submitted.ID || !containsListAction(body.Items[0].AllowedActions, "WITHDRAW")) {
				t.Fatal("submitted owner identity/action changed")
			}
		}
	})

	t.Run("revoke-after-visibility-cannot-return-foreign-rows", func(t *testing.T) {
		reader := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "list-revoked-reader")
		gate, err := observer.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(context.Background())
		if _, err := gate.Exec(ctx, `SELECT id FROM role_assignments WHERE user_id=$1 FOR UPDATE`, reader.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			items, err := r.ListApprovalRequests(ctx, entitlementApprovalPrincipal(reader), ApprovalFilter{Limit: 1})
			if len(items) != 0 {
				err = fmt.Errorf("revoked reader received %d foreign rows", len(items))
			}
			done <- err
		}()
		app := r.pool.Config().ConnConfig.RuntimeParams["application_name"]
		wallClockWait(t, ctx, f.admin, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%array_agg(locked.id%')`, app)
		if _, err := gate.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, reader.ID); err != nil {
			t.Fatal(err)
		}
		if err := gate.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrApprovalAuthorizationChanged) {
			t.Fatalf("post-visibility revocation did not reject original authority version: %v", err)
		}
	})

	t.Run("expired-authority-hides-foreign-rows-without-erasing-own-history", func(t *testing.T) {
		actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "list-expired-owner")
		own, _ := wallClockSubmitted(t, r, ctx, entitlementApprovalPrincipal(actor))
		if _, err := observer.Exec(ctx, `UPDATE role_assignments SET expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, actor.ID); err != nil {
			t.Fatal(err)
		}
		items, err := r.ListApprovalRequests(ctx, entitlementApprovalPrincipal(actor), ApprovalFilter{Limit: 200})
		if err != nil || len(items) != 1 || items[0].ID != own.ID || len(items[0].AllowedActions) != 0 {
			t.Fatalf("expired actor lost owned history or gained authority: count=%d err=%v", len(items), err)
		}
	})

	t.Run("cancelled-list-returns-no-partial-items-or-held-connection", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		items, err := r.ListApprovalRequests(cancelled, p, ApprovalFilter{Limit: 200})
		if err == nil || len(items) != 0 || r.pool.Stat().AcquiredConns() != 0 {
			t.Fatalf("cancelled list partial output/connection: count=%d err=%v", len(items), err)
		}
	})
}

func containsListAction(actions []string, expected string) bool {
	for _, action := range actions {
		if action == expected {
			return true
		}
	}
	return false
}
