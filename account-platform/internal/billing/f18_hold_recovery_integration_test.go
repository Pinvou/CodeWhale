package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A guarded disposable coordinator, not an operator database or a real Token
// dispatch. The trusted positive is explicitly a controlled authenticated RPC.
func TestF18HoldRecoveryUsesTrustedFence(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	admin, e := pgx.Connect(ctx, raw)
	if e != nil {
		t.Fatal("owned coordinator unavailable")
	}
	var user, dir string
	var port, actors, dbs, sessions int
	e = admin.QueryRow(ctx, `SELECT session_user,current_setting('data_directory'),current_setting('port')::int,(SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>'pinvou_wave_test'),(SELECT count(*) FROM pg_database WHERE NOT datistemplate AND datname<>'postgres'),(SELECT count(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND backend_type='client backend')`).Scan(&user, &dir, &port, &actors, &dbs, &sessions)
	if e != nil || user != "pinvou_wave_test" || port != 55504 || strings.ReplaceAll(dir, "\\", "/") != "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data" || actors != 0 || dbs != 0 || sessions != 0 {
		admin.Close(ctx)
		t.Fatal("owned identity/absence guard")
	}
	roles := []string{"billing_schema_owner", "billing_service", "billing_migrator"}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, role := range []string{"billing_migrator", "billing_service", "billing_schema_owner"} {
			if _, e := admin.Exec(cleanup, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); e != nil {
				t.Error("owned actor cleanup failed")
			}
		}
		admin.Close(cleanup)
	})
	for _, role := range roles {
		login, inherit := "LOGIN", "NOINHERIT"
		if role == "billing_schema_owner" {
			login = "NOLOGIN"
		}
		if role == "billing_service" {
			inherit = "INHERIT"
		}
		if _, e = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" "+inherit+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); e != nil {
			t.Fatal("owned actor creation failed")
		}
	}
	if _, e = admin.Exec(ctx, "GRANT billing_schema_owner,billing_service TO billing_migrator; GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE"); e != nil {
		t.Fatal(e)
	}
	r, _ := newAdminPaginationIntegrationRepository(t, ctx, raw)
	all, e := migrations.Load("../../migrations/billing")
	if e != nil {
		t.Fatal(e)
	}
	ledgerConnection, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, e = migrations.VerifyApplied(ctx, ledgerConnection.Conn(), all)
	ledgerConnection.Release()
	if e != nil {
		t.Fatal("public migration ledger verification", e)
	}
	for _, scenario := range []string{"nil-proof", "unproven", "timeout", "controlled-trusted"} {
		t.Run(scenario, func(t *testing.T) {
			_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
			usage := uuid.New()
			hold, e := r.Reserve(ctx, usage, order.WalletID, 100, nil, time.Now().UTC().Add(time.Minute))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); e != nil {
				t.Fatal(e)
			}
			var proof ExpiredHoldProofClient
			if scenario != "nil-proof" {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
					if q.Method != "POST" || q.URL.Path != "/internal/v1/holds/expiry-proof" || q.Header.Get("Authorization") != "Bearer controlled-workload" {
						t.Error("wrong authenticated proof route")
						w.WriteHeader(400)
						return
					}
					var i ExpiredHoldIdentity
					if json.NewDecoder(q.Body).Decode(&i) != nil {
						w.WriteHeader(400)
						return
					}
					if scenario == "unproven" {
						w.WriteHeader(503)
						return
					}
					if scenario == "timeout" {
						<-q.Context().Done()
						return
					}
					if _, e := r.Release(q.Context(), i.UsageRequestID, i.HoldID, "controlled pre-dispatch fence"); e != nil {
						t.Error(e)
						w.WriteHeader(503)
						return
					}
					json.NewEncoder(w).Encode(ExpiredHoldProof{Identity: i, ProofID: uuid.New(), Status: "LOCAL_CONFIRMED"})
				}))
				defer server.Close()
				client := server.Client()
				if scenario == "timeout" {
					client.Timeout = 25 * time.Millisecond
				}
				proof, e = NewHTTPExpiredHoldProofClient(server.URL, "controlled-workload", client)
				if e != nil {
					t.Fatal(e)
				}
			}
			worker := NewExpiredHoldRecoveryWorker(r, proof, time.Minute)
			if os.Getenv("F18_PRIVATE_DIRECT_EXPIRY_DIAGNOSTIC") == "1" {
				_, e = r.ExpireHolds(ctx, 1)
			} else {
				e = worker.RunOnceLimit(ctx, 1)
			}
			if e != nil {
				t.Fatal(e)
			}
			var state, status string
			var reserved int64
			e = r.pool.QueryRow(ctx, `SELECT h.status,w.reserved_microcredits,COALESCE(x.state,'MISSING_INTENT') FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id LEFT JOIN usage_hold_expiry_recovery x ON x.hold_id=h.id WHERE h.id=$1`, hold.ID).Scan(&status, &reserved, &state)
			wantStatus, wantState, wantReserved := "ACTIVE", "MANUAL_REVIEW", int64(100)
			if scenario == "controlled-trusted" {
				wantStatus, wantState, wantReserved = "RELEASED", "COMPLETED", 0
			}
			if e != nil || status != wantStatus || state != wantState || reserved != wantReserved {
				t.Fatalf("financial/intent boundary status=%s reserved=%d state=%s: %v", status, reserved, state, e)
			}
			// Prevent a previous MANUAL_REVIEW candidate from consuming this pass's limit.
			if scenario != "controlled-trusted" {
				if _, e = r.pool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE hold_id=$1`, hold.ID); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
