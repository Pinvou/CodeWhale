package account

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Public migrations and the existing exclusively owned, non-superuser fixture.
// Financial assessments below are inputs, not a real Billing settlement.
func TestAccountClosureBlockedSLAAlertsPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	clock := time.Now().UTC().Truncate(time.Microsecond)
	r.now = func() time.Time { return clock }
	financial := []AccountClosureBlocker{{Domain: "BILLING", Code: "PERSONAL_BALANCE_NONZERO", ResourceType: "WALLET", UserAction: "CLEAR_PERSONAL_BALANCE"}}
	newRequest := func(t *testing.T, role string) AccountClosureRequest {
		t.Helper()
		clock = time.Now().UTC().Truncate(time.Microsecond)
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=clock_timestamp()+interval '1 day',lease_until=NULL WHERE status<>'COMPLETED'`); err != nil {
			t.Fatal(err)
		}
		user := uuid.New()
		if _, err := r.pool.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,mobile_e164,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$4,$4 FROM capability`, user, "alert_"+strings.ReplaceAll(user.String(), "-", ""), "+alert:"+user.String(), clock); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('closure-alert',$1,$2,$3,$3)`, user.String(), user, clock); err != nil {
			t.Fatal(err)
		}
		if role == "OWNER" {
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			org, member := uuid.New(), uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'Controlled closure alert owner','ACTIVE',$2,transaction_timestamp(),transaction_timestamp())`, org, user); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',transaction_timestamp(),transaction_timestamp())`, member, org, user); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at,grant_source) VALUES($1,$2,'OWNER',$3,transaction_timestamp(),transaction_timestamp(),'ORGANIZATION_CREATION')`, uuid.New(), member, user); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		} else if role == "PLATFORM_SUPER_ADMIN" {
			if _, err := r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,created_at,starts_at) VALUES($1,$2,'PLATFORM_SUPER_ADMIN',$2,transaction_timestamp(),transaction_timestamp())`, uuid.New(), user); err != nil {
				t.Fatal(err)
			}
		}
		request, err := r.CreateAccountClosure(ctx, identity.Principal{Issuer: "closure-alert", Subject: user.String(), AuthenticatedAt: clock}, uuid.NewString(), accountClosureConfirmation)
		if err != nil || request.Stage != "CLOSURE_REQUESTED" || (role != "" && (request.Status != "BLOCKED" || len(request.Blockers) != 1)) {
			t.Fatal("real initial role gate", request, err)
		}
		return request
	}
	due := func(t *testing.T, id uuid.UUID) {
		t.Helper()
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=$2,lease_until=NULL WHERE id=$1`, id, clock.Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T, request AccountClosureRequest) AccountClosureRequest {
		t.Helper()
		due(t, request.ID)
		item, err := r.ClaimAccountClosure(ctx)
		if err != nil || item == nil || item.ID != request.ID {
			t.Fatal("real original claim", item, err)
		}
		return *item
	}
	alerts := func(t *testing.T, request AccountClosureRequest, want int, from, to string, blockers []AccountClosureBlocker) {
		t.Helper()
		encoded, err := json.Marshal(blockers)
		if err != nil {
			t.Fatal(err)
		}
		var count, matched int
		if err := r.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE from_stage=$2 AND to_stage=$3 AND redacted_details=jsonb_build_object('blocker_digest',$4::text,'alert_after',$5::timestamptz)) FROM account_closure_audit WHERE closure_request_id=$1 AND event_type='BLOCKED_SLA_ALERT'`, request.ID, from, to, closureDigest(string(encoded)), request.SLAExpiresAt).Scan(&count, &matched); err != nil || count != want || matched != want {
			t.Fatalf("alert count/payload got=%d/%d want=%d: %v", count, matched, want, err)
		}
	}
	snapshot := func(t *testing.T, request AccountClosureRequest) string {
		t.Helper()
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('request',to_jsonb(q),'user',to_jsonb(u),'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM account_closure_audit a WHERE a.closure_request_id=q.id))::text FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, role := range []string{"OWNER", "PLATFORM_SUPER_ADMIN"} {
		t.Run("requested-"+role+"-worker", func(t *testing.T) {
			request := newRequest(t, role)
			for _, offset := range []time.Duration{-time.Microsecond, 0, time.Microsecond, time.Minute} {
				clock = request.SLAExpiresAt.Add(offset)
				due(t, request.ID)
				NewAccountClosureWorker(r, time.Second).runOnce(ctx)
				want := 0
				if offset > 0 {
					want = 1
				}
				alerts(t, request, want, "CLOSURE_REQUESTED", "CLOSURE_REQUESTED", request.Blockers)
			}
			var safe bool
			if err := r.pool.QueryRow(ctx, `SELECT q.status='BLOCKED' AND q.stage='CLOSURE_REQUESTED' AND q.lease_until IS NULL AND q.cleanup_prepared_at IS NULL AND u.status='ACTIVE' FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID).Scan(&safe); err != nil || !safe {
				t.Fatal("alert changed initial role blocker lifecycle", err)
			}
		})
	}
	for _, stage := range []string{"CLOSURE_REQUESTED", "QUIESCING", "FINANCIAL_CLEARANCE"} {
		t.Run(stage+"-atomic-failure-retry-and-dedup", func(t *testing.T) {
			request := newRequest(t, "")
			if stage != "CLOSURE_REQUESTED" {
				if err := r.EnterAccountClosureQuiescing(ctx, claim(t, request)); err != nil {
					t.Fatal(err)
				}
			}
			save := r.SaveAccountClosureBlockers
			to := "FINANCIAL_CLEARANCE"
			if stage == "CLOSURE_REQUESTED" {
				save, to = r.SaveAccountClosureRequestedBlockers, "CLOSURE_REQUESTED"
			}
			if stage == "FINANCIAL_CLEARANCE" {
				if err := save(ctx, claim(t, request), financial); err != nil {
					t.Fatal(err)
				}
			}
			alerts(t, request, 0, stage, to, financial)
			clock = request.SLAExpiresAt.Add(time.Microsecond)
			item := claim(t, request)
			if item.Stage != stage {
				t.Fatal("incorrect real stage", item.Stage)
			}
			if _, err := r.pool.Exec(ctx, `CREATE FUNCTION closure_alert_test_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='P9001', MESSAGE='controlled alert failure'; END $$; CREATE TRIGGER closure_alert_test_reject BEFORE INSERT ON account_closure_audit FOR EACH ROW WHEN (NEW.event_type='BLOCKED_SLA_ALERT') EXECUTE FUNCTION closure_alert_test_reject()`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := r.pool.Exec(ctx, `DROP TRIGGER IF EXISTS closure_alert_test_reject ON account_closure_audit; DROP FUNCTION IF EXISTS closure_alert_test_reject()`); err != nil {
					t.Error("owned fault injector cleanup", err)
				}
			})
			before := snapshot(t, request)
			var injected *pgconn.PgError
			if err := save(ctx, item, financial); !errors.As(err, &injected) || injected.Code != "P9001" || snapshot(t, request) != before {
				t.Fatal("alert failure did not atomically roll back request/user/audit", err)
			}
			if _, err := r.pool.Exec(ctx, `DROP TRIGGER closure_alert_test_reject ON account_closure_audit; DROP FUNCTION closure_alert_test_reject()`); err != nil {
				t.Fatal(err)
			}
			if err := save(ctx, item, financial); err != nil {
				t.Fatal("same claim could not recover after rolled-back alert failure", err)
			}
			alerts(t, request, 1, stage, to, financial)
			before = snapshot(t, request)
			if err := save(ctx, item, financial); !errors.Is(err, ErrConflict) || snapshot(t, request) != before {
				t.Fatal("stale claim changed persisted state", err)
			}
			if err := save(ctx, claim(t, request), financial); err != nil {
				t.Fatal("fresh claim replay", err)
			}
			alerts(t, request, 1, stage, to, financial)
			if stage != "CLOSURE_REQUESTED" {
				if err := save(ctx, claim(t, request), []AccountClosureBlocker{}); err != nil {
					t.Fatal("empty assessment", err)
				}
				alerts(t, request, 1, stage, to, financial)
			}
		})
	}
}
