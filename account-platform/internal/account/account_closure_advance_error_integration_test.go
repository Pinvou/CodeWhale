package account

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestF08ClosureAdvanceFailureIsScheduledPostgres(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "retry-before-sla"
		if expired {
			name = "repair-required-after-sla"
		}
		t.Run(name, func(t *testing.T) {
			f := retainedWalletFenceActors(t)
			r := f.repository(t)
			ctx, now, user := f.ctx, time.Now().UTC(), uuid.New()
			if _, err := r.pool.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,mobile_e164,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$4,$4 FROM capability`, user, "advance_"+strings.ReplaceAll(user.String(), "-", ""), "+advance:"+user.String(), now); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('closure-advance',$1,$2,$3,$3)`, user.String(), user, now); err != nil {
				t.Fatal(err)
			}
			request, err := r.CreateAccountClosure(ctx, identity.Principal{Issuer: "closure-advance", Subject: user.String(), AuthenticatedAt: now}, uuid.NewString(), accountClosureConfirmation)
			if err != nil {
				t.Fatal(err)
			}
			item, err := r.ClaimAccountClosure(ctx)
			if err != nil || item == nil || item.ID != request.ID {
				t.Fatal("initial owned claim missing", item, err)
			}
			if err := r.EnterAccountClosureQuiescing(ctx, *item); err != nil {
				t.Fatal(err)
			}
			// Controlled completed administrative disable: do not weaken the real
			// pre-cleanup user gate or replace Advance with a fake return value.
			if _, err := r.pool.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1 WHERE id=$1`, user); err != nil {
				t.Fatal(err)
			}
			if expired {
				r.now = func() time.Time { return request.SLAExpiresAt.Add(time.Minute) }
			}
			ready := closureRecoveryAssessment(func(context.Context, uuid.UUID) (AccountClosureAssessment, error) {
				return AccountClosureAssessment{Ready: true, Blockers: []AccountClosureBlocker{}}, nil
			})
			NewAccountClosureWorker(r, time.Second, ready).runOnce(ctx)
			var status, stage, code, userStatus string
			var next *time.Time
			var lease *time.Time
			var untouchedMilestones bool
			if err := r.pool.QueryRow(ctx, `SELECT request.status,request.stage,COALESCE(request.last_error_code,''),request.next_attempt_at,request.lease_until,users.status,request.cleanup_prepared_at IS NULL AND request.identity_deleted_at IS NULL AND request.grants_isolated_at IS NULL AND request.token_replays_cleared_at IS NULL AND request.completed_at IS NULL FROM account_closure_requests request JOIN users ON users.id=request.user_id WHERE request.id=$1`, request.ID).Scan(&status, &stage, &code, &next, &lease, &userStatus, &untouchedMilestones); err != nil {
				t.Fatal(err)
			}
			if stage != "QUIESCING" || code != "IDENTITY_CLEANUP_ADVANCE_FAILED" || lease != nil || userStatus != "DISABLED" || !untouchedMilestones {
				t.Fatal("failed advance changed the stage/user or did not record retry", status, stage, code, lease, userStatus, untouchedMilestones)
			}
			if expired {
				if status != "REPAIR_REQUIRED" || next != nil {
					t.Fatal("past-SLA failure remained automatically claimable", status, next)
				}
			} else if status != "RETRY" || next == nil || !next.After(r.now()) {
				t.Fatal("advance failure has no bounded retry", status, next)
			}
			if nextItem, err := r.ClaimAccountClosure(ctx); err != nil || nextItem != nil {
				t.Fatal("scheduled failure was immediately reclaimed", nextItem, err)
			}
		})
	}
}
