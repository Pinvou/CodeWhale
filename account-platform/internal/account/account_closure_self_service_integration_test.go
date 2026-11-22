package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type closureRecoveryAssessment func(context.Context, uuid.UUID) (AccountClosureAssessment, error)

func (f closureRecoveryAssessment) Assess(ctx context.Context, user uuid.UUID) (AccountClosureAssessment, error) {
	return f(ctx, user)
}

// Real public migrations and the non-superuser account_service actor. External
// assessments are controlled inputs here; this is not provider deletion or an
// end-to-end financial settlement. The existing three-domain fixture runs too.
func TestAccountClosureSelfServiceRecoveryPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	blocker := []AccountClosureBlocker{{Domain: "BILLING", Code: "PERSONAL_BALANCE_NONZERO", ResourceType: "WALLET", UserAction: "CLEAR_PERSONAL_BALANCE"}}
	newRequest := func(t *testing.T) (AccountClosureRequest, identity.Principal, string) {
		t.Helper()
		// Park prior cases in this exclusively owned disposable database. No
		// production request is changed, deleted, or passed through a fake claim.
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=clock_timestamp()+interval '1 day',lease_until=NULL WHERE status<>'COMPLETED'`); err != nil {
			t.Fatal(err)
		}
		user := uuid.New()
		now := time.Now().UTC()
		if _, err := r.pool.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,mobile_e164,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$4,$4 FROM capability`, user, "closure_"+strings.ReplaceAll(user.String(), "-", ""), "+closure:"+user.String(), now); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('closure-recovery',$1,$2,$3,$3)`, user.String(), user, now); err != nil {
			t.Fatal(err)
		}
		principal := identity.Principal{Issuer: "closure-recovery", Subject: user.String(), AuthenticatedAt: now}
		key := uuid.NewString()
		request, err := r.CreateAccountClosure(ctx, principal, key, accountClosureConfirmation)
		if err != nil || request.Stage != "CLOSURE_REQUESTED" {
			t.Fatal("request must remain pre-quiet", request, err)
		}
		return request, principal, key
	}
	claim := func(t *testing.T, request AccountClosureRequest) AccountClosureRequest {
		t.Helper()
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=clock_timestamp()-interval '1 second',lease_until=NULL WHERE id=$1`, request.ID); err != nil {
			t.Fatal(err)
		}
		item, err := r.ClaimAccountClosure(ctx)
		if err != nil || item == nil || item.ID != request.ID {
			t.Fatal("original real claim", item, err)
		}
		return *item
	}
	quiet := func(t *testing.T, request AccountClosureRequest) AccountClosureRequest {
		t.Helper()
		item := claim(t, request)
		if err := r.EnterAccountClosureQuiescing(ctx, item); err != nil {
			t.Fatal(err)
		}
		return claim(t, request)
	}
	snapshot := func(t *testing.T, item AccountClosureRequest) string {
		t.Helper()
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('request',to_jsonb(q),'user',to_jsonb(u),'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM account_closure_audit a WHERE a.closure_request_id=q.id))::text FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, item.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	reject := func(t *testing.T, item AccountClosureRequest) {
		t.Helper()
		before := snapshot(t, item)
		if err := r.SaveAccountClosureBlockers(ctx, item, blocker); !errors.Is(err, ErrConflict) {
			t.Fatalf("unsafe restoration accepted: %v", err)
		}
		if snapshot(t, item) != before {
			t.Fatal("rejected restoration changed user, original request, or immutable audit")
		}
	}
	t.Run("original-request-restores-then-reenters-before-cleanup", func(t *testing.T) {
		request, principal, key := newRequest(t)
		item := quiet(t, request)
		if err := r.SaveAccountClosureBlockers(ctx, item, blocker); err != nil {
			t.Fatal(err)
		}
		var restored bool
		if err := r.pool.QueryRow(ctx, `SELECT u.status='ACTIVE' AND q.stage='FINANCIAL_CLEARANCE' AND q.status='BLOCKED' AND q.lease_until IS NULL AND q.idempotency_key_digest=$2 AND q.request_fingerprint=$3 AND (SELECT count(*) FROM account_closure_audit WHERE closure_request_id=q.id AND event_type='SELF_SERVICE_RESTORED')=1 FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID, closureDigest(key), closureDigest("account-closure-v1\n"+accountClosureConfirmation)).Scan(&restored); err != nil || !restored {
			t.Fatal("original frozen request was not restored safely", restored, err)
		}
		replayed, err := r.CreateAccountClosure(ctx, principal, key, accountClosureConfirmation)
		if err != nil || !replayed.Replayed || replayed.ID != request.ID {
			t.Fatal("original replay lost", replayed, err)
		}
		item = claim(t, request)
		before := snapshot(t, item)
		if err := r.AdvanceAccountClosureToIdentityCleanup(ctx, item); !errors.Is(err, ErrConflict) || snapshot(t, item) != before {
			t.Fatal("ACTIVE user advanced to cleanup", err)
		}
		if err := r.EnterAccountClosureQuiescing(ctx, item); err != nil {
			t.Fatal("reenter", err)
		}
		var reentered bool
		if err := r.pool.QueryRow(ctx, `SELECT u.status='CLOSING' AND q.stage='FINANCIAL_CLEARANCE' AND q.lease_until IS NULL AND q.cleanup_prepared_at IS NULL FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID).Scan(&reentered); err != nil || !reentered {
			t.Fatal("reentry did not release claim in original stage", err)
		}
		if err := r.AdvanceAccountClosureToIdentityCleanup(ctx, item); !errors.Is(err, ErrConflict) {
			t.Fatal("old ACTIVE claim reused after reentry", err)
		}
		item = claim(t, request)
		if err := r.AdvanceAccountClosureToIdentityCleanup(ctx, item); err != nil {
			t.Fatal("new CLOSING claim", err)
		}
		item = claim(t, request)
		reject(t, item)
	})
	t.Run("restored-user-new-ownership-blocks-reentry", func(t *testing.T) {
		request, _, _ := newRequest(t)
		item := quiet(t, request)
		if err := r.SaveAccountClosureBlockers(ctx, item, blocker); err != nil {
			t.Fatal(err)
		}
		item = claim(t, request)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		org, member := uuid.New(), uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'Controlled recovery owner','ACTIVE',$2,transaction_timestamp(),transaction_timestamp())`, org, item.UserID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',transaction_timestamp(),transaction_timestamp())`, member, org, item.UserID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at,grant_source) VALUES($1,$2,'OWNER',$3,transaction_timestamp(),transaction_timestamp(),'ORGANIZATION_CREATION')`, uuid.New(), member, item.UserID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		item = claim(t, request)
		before := snapshot(t, item)
		if err := r.EnterAccountClosureQuiescing(ctx, item); !errors.Is(err, ErrLastOwner) || snapshot(t, item) != before {
			t.Fatal("new owner ignored during reentry", err)
		}
	})
	t.Run("claim-cannot-restore-a-different-user", func(t *testing.T) {
		request, _, _ := newRequest(t)
		item := quiet(t, request)
		item.UserID = uuid.New()
		reject(t, item)
	})
	for _, test := range []struct{ name, sql string }{
		{"expired-lease", `UPDATE account_closure_requests SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`},
		{"replaced-claim-version", `UPDATE account_closure_requests SET version=version+1 WHERE id=$1`},
		{"changed-user-version", `UPDATE users SET version=version+1 WHERE id=(SELECT user_id FROM account_closure_requests WHERE id=$1)`},
		{"disabled-user", `UPDATE users SET status='DISABLED',version=version+1 WHERE id=(SELECT user_id FROM account_closure_requests WHERE id=$1)`},
		{"deleted-user", `UPDATE users SET status='DELETED',version=version+1 WHERE id=(SELECT user_id FROM account_closure_requests WHERE id=$1)`},
		{"cleanup-prepared", `UPDATE account_closure_requests SET cleanup_prepared_at=clock_timestamp() WHERE id=$1`},
		{"identity-deleted", `UPDATE account_closure_requests SET identity_deleted_at=clock_timestamp() WHERE id=$1`},
		{"grants-isolated", `UPDATE account_closure_requests SET grants_isolated_at=clock_timestamp() WHERE id=$1`},
		{"replays-cleared", `UPDATE account_closure_requests SET token_replays_cleared_at=clock_timestamp() WHERE id=$1`},
		{"cleanup-lifecycle", `UPDATE account_closure_requests SET subject_lifecycle_version=2 WHERE id=$1`},
		{"anonymized-user", `UPDATE users SET anonymized_at=clock_timestamp() WHERE id=(SELECT user_id FROM account_closure_requests WHERE id=$1)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _, _ := newRequest(t)
			item := quiet(t, request)
			if _, err := r.pool.Exec(ctx, test.sql, item.ID); err != nil {
				t.Fatal(err)
			}
			if test.name == "disabled-user" || test.name == "deleted-user" {
				// Fresh claim snapshots match the prohibited user state, so this
				// tests the state guard independently from stale-version rejection.
				item = claim(t, request)
			}
			reject(t, item)
		})
	}
	t.Run("only-waiting-does-not-restore-self-service", func(t *testing.T) {
		request, _, _ := newRequest(t)
		item := quiet(t, request)
		if err := r.SaveAccountClosureBlockers(ctx, item, []AccountClosureBlocker{{Domain: "BILLING", Code: "UNSETTLED_HOLD", UserAction: "WAIT_FOR_USAGE_SETTLEMENT"}}); err != nil {
			t.Fatal(err)
		}
		var closing bool
		if err := r.pool.QueryRow(ctx, `SELECT status='CLOSING' FROM users WHERE id=$1`, item.UserID).Scan(&closing); err != nil || !closing {
			t.Fatal("wait-only blocker restored operations", err)
		}
		// This is the persisted shape produced by the old worker: financial
		// clearance is already BLOCKED and the user is still CLOSING.
		item = claim(t, request)
		if item.Stage != "FINANCIAL_CLEARANCE" || item.Status != "BLOCKED" {
			t.Fatal("missing legacy blocked fixture", item)
		}
		if err := r.SaveAccountClosureBlockers(ctx, item, blocker); err != nil {
			t.Fatal("legacy blocked request recovery", err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT status='ACTIVE' FROM users WHERE id=$1`, item.UserID).Scan(&closing); err != nil || !closing {
			t.Fatal("legacy request still trapped", err)
		}
	})
	for _, test := range []struct{ name, sql string }{
		{"expired-lease", `UPDATE account_closure_requests SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`},
		{"request-version", `UPDATE account_closure_requests SET version=version+1 WHERE id=$1`},
		{"user-version", `UPDATE users SET version=version+1 WHERE id=(SELECT user_id FROM account_closure_requests WHERE id=$1)`},
		{"disabled", `UPDATE users SET status='DISABLED',version=version+1 WHERE id=(SELECT user_id FROM account_closure_requests WHERE id=$1)`},
		{"cleanup-marker", `UPDATE account_closure_requests SET cleanup_prepared_at=clock_timestamp() WHERE id=$1`},
	} {
		t.Run("reentry-rejects-"+test.name, func(t *testing.T) {
			request, _, _ := newRequest(t)
			item := quiet(t, request)
			if err := r.SaveAccountClosureBlockers(ctx, item, blocker); err != nil {
				t.Fatal(err)
			}
			item = claim(t, request)
			if _, err := r.pool.Exec(ctx, test.sql, item.ID); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, item)
			if err := r.EnterAccountClosureQuiescing(ctx, item); !errors.Is(err, ErrConflict) || snapshot(t, item) != before {
				t.Fatal("unsafe reentry changed request/user/audit", err)
			}
		})
	}
	for _, target := range []string{"user", "claim"} {
		t.Run(target+"-lock-wait-cannot-outlive-lease", func(t *testing.T) {
			request, _, _ := newRequest(t)
			item := quiet(t, request)
			if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET lease_until=clock_timestamp()+interval '350 milliseconds' WHERE id=$1`, item.ID); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, item)
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var locked uuid.UUID
			sql, id := `SELECT id FROM users WHERE id=$1 FOR UPDATE`, item.UserID
			if target == "claim" {
				sql, id = `SELECT id FROM account_closure_requests WHERE id=$1 FOR UPDATE`, item.ID
			}
			if err := tx.QueryRow(ctx, sql, id).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- r.SaveAccountClosureBlockers(ctx, item, blocker) }()
			deadline := time.Now().Add(3 * time.Second)
			observed := false
			for time.Now().Before(deadline) {
				var blocked, expired bool
				if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid()),lease_until<=clock_timestamp() FROM account_closure_requests WHERE id=$1`, item.ID).Scan(&blocked, &expired); err != nil {
					t.Fatal(err)
				}
				observed = observed || blocked
				if observed && expired {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !observed {
				t.Fatal("did not observe real database lock wait")
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrConflict) {
					t.Fatal("expired waiter restored user", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("waiter did not finish")
			}
			if snapshot(t, item) != before {
				t.Fatal("expired waiter changed evidence")
			}
		})
	}
	t.Run("preexisting-financial-blocker-keeps-user-active-before-quiescing", func(t *testing.T) {
		request, _, _ := newRequest(t)
		var assessed int
		client := closureRecoveryAssessment(func(_ context.Context, user uuid.UUID) (AccountClosureAssessment, error) {
			assessed++
			return AccountClosureAssessment{UserID: user, Ready: false, Blockers: blocker}, nil
		})
		NewAccountClosureWorker(r, time.Second, client).runOnce(ctx)
		var untouched bool
		if err := r.pool.QueryRow(ctx, `SELECT q.stage='CLOSURE_REQUESTED' AND q.status='BLOCKED' AND u.status='ACTIVE' AND q.cleanup_prepared_at IS NULL FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID).Scan(&untouched); err != nil || !untouched || assessed != 1 {
			t.Fatal("pre-check trapped user or skipped assessment", untouched, assessed, err)
		}
	})
	t.Run("worker-rechecks-both-domains-after-late-balance-and-reentry", func(t *testing.T) {
		request, _, _ := newRequest(t)
		lateBalance := true
		var observations []string
		client := func(domain string) AccountClosureClient {
			return closureRecoveryAssessment(func(_ context.Context, user uuid.UUID) (AccountClosureAssessment, error) {
				var status string
				if err := r.pool.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, user).Scan(&status); err != nil {
					return AccountClosureAssessment{}, err
				}
				observations = append(observations, domain+":"+status)
				result := AccountClosureAssessment{UserID: user, Ready: true, Blockers: []AccountClosureBlocker{}}
				if domain == "BILLING" && status == "CLOSING" && lateBalance {
					result.Ready = false
					result.Blockers = blocker
				}
				return result, nil
			})
		}
		worker := NewAccountClosureWorker(r, time.Second, client("BILLING"), client("TOKEN"))
		worker.runOnce(ctx)
		var stage, status string
		if err := r.pool.QueryRow(ctx, `SELECT q.stage,u.status FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID).Scan(&stage, &status); err != nil || stage != "FINANCIAL_CLEARANCE" || status != "ACTIVE" {
			t.Fatal("late balance trapped user", stage, status, err)
		}
		// An ACTIVE precheck can become stale again. The subsequent CLOSING
		// assessment must restore access again instead of starting cleanup.
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ID); err != nil {
			t.Fatal(err)
		}
		worker.runOnce(ctx)
		if err := r.pool.QueryRow(ctx, `SELECT q.stage,u.status FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID).Scan(&stage, &status); err != nil || stage != "FINANCIAL_CLEARANCE" || status != "ACTIVE" {
			t.Fatal("repeated late balance bypassed recovery", stage, status, err)
		}
		lateBalance = false
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ID); err != nil {
			t.Fatal(err)
		}
		worker.runOnce(ctx)
		if err := r.pool.QueryRow(ctx, `SELECT q.stage,u.status FROM account_closure_requests q JOIN users u ON u.id=q.user_id WHERE q.id=$1`, request.ID).Scan(&stage, &status); err != nil || stage != "IDENTITY_CLEANUP" || status != "CLOSING" {
			t.Fatal("original request did not continue", stage, status, err)
		}
		want := "BILLING:ACTIVE,TOKEN:ACTIVE,BILLING:CLOSING,TOKEN:CLOSING,BILLING:ACTIVE,TOKEN:ACTIVE,BILLING:CLOSING,TOKEN:CLOSING,BILLING:ACTIVE,TOKEN:ACTIVE,BILLING:CLOSING,TOKEN:CLOSING"
		if strings.Join(observations, ",") != want {
			t.Fatalf("missing full-domain recheck: %v", observations)
		}
	})
}
