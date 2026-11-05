package account

import (
	"context"
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/jackc/pgx/v5"
)

// VerifyApprovalRecoveryActor checks the actual authenticated session, not the
// username claimed in configuration. No role switching is part of this runtime.
func VerifyApprovalRecoveryActor(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var session, current string
	var login, inherit, super, createDB, createRole, replication, bypass, members, claim, release, owns, tableAccess bool
	err := db.QueryRow(ctx, `SELECT session_user,current_user,r.rolcanlogin,r.rolinherit,r.rolsuper,r.rolcreatedb,r.rolcreaterole,r.rolreplication,r.rolbypassrls,
 EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid OR roleid=r.oid),
 has_function_privilege(session_user,'public.claim_approval_recovery_query(uuid,uuid,uuid,text,bigint,bigint)','EXECUTE'),
 has_function_privilege(session_user,'public.release_approval_recovery_query(uuid,uuid,bigint)','EXECUTE'),
 EXISTS(SELECT 1 FROM pg_catalog.pg_class WHERE relowner=r.oid) OR EXISTS(SELECT 1 FROM pg_catalog.pg_proc WHERE proowner=r.oid),
 EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname IN ('public','account_approval_authorization') AND c.relkind IN ('r','p','v','m') AND has_table_privilege(r.oid,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'))
 FROM pg_catalog.pg_roles r WHERE r.rolname=session_user`).Scan(&session, &current, &login, &inherit, &super, &createDB, &createRole, &replication, &bypass, &members, &claim, &release, &owns, &tableAccess)
	if err != nil || session != "account_approval_recovery" || current != session || !login || inherit || super || createDB || createRole || replication || bypass || members || !claim || !release || owns || tableAccess {
		return errors.New("RECOVERY_DATABASE_ACTOR_INVALID")
	}
	return nil
}

// ApprovalRecoveryRuntime owns only recovery readers and terminal confirmation
// clients. It cannot approve a request, begin execution or call a financial command.
type ApprovalRecoveryRuntime struct {
	planner    *ApprovalRecoveryPlanClient
	leaser     RecoveryQueryLeaser
	effects    approvaleffect.Reader
	results    ApprovalRecoveryResultReader
	confirmer  RecoverySuccessConfirmer
	zeroEffect RecoveryZeroEffectConfirmer
}

func NewApprovalRecoveryRuntime(accountURL, billingURL, credential string, leaser RecoveryQueryLeaser, client *http.Client) (*ApprovalRecoveryRuntime, error) {
	if leaser == nil || client == nil {
		return nil, ErrApprovalRecoveryPlanInvalid
	}
	planner, err := NewApprovalRecoveryPlanClient(accountURL, credential)
	if err != nil {
		return nil, err
	}
	results, err := NewApprovalRecoveryResultClient(billingURL, credential)
	if err != nil {
		return nil, err
	}
	confirmer, err := NewApprovalRecoveryConfirmationClient(accountURL, credential)
	if err != nil {
		return nil, err
	}
	zero, err := NewApprovalRecoveryZeroEffectClient(accountURL, credential)
	if err != nil {
		return nil, err
	}
	effects, err := approvaleffect.NewClientWithHTTPClient(billingURL, credential, client)
	if err != nil {
		return nil, err
	}
	planner.httpClient, results.httpClient = client, client
	confirmer.httpClient, zero.httpClient = client, client
	return &ApprovalRecoveryRuntime{planner: planner, leaser: leaser, effects: effects, results: results, confirmer: confirmer, zeroEffect: zero}, nil
}

type ApprovalRecoveryPass struct {
	Candidates     int
	Confirmed      int
	Invalidated    int
	Pending        int
	Unavailable    int
	MoreCandidates bool
}

// RunOnce processes the bounded snapshot independently so a failed authority
// query does not prevent investigating the other candidates already selected.
// Unknown observations are counted for operator review, never retried as effects.
func (r *ApprovalRecoveryRuntime) RunOnce(ctx context.Context, limit int) (ApprovalRecoveryPass, error) {
	if r == nil || r.planner == nil {
		return ApprovalRecoveryPass{}, ErrApprovalRecoveryPlanInvalid
	}
	plan, err := r.planner.PlanProofApprovalRecovery(ctx, limit)
	if err != nil {
		return ApprovalRecoveryPass{}, err
	}
	summary := ApprovalRecoveryPass{Candidates: len(plan.Candidates), MoreCandidates: plan.MoreCandidates}
	var failures []error
	for _, candidate := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return summary, errors.Join(append(failures, err)...)
		}
		batch, err := InspectProofRecoveryLeasedWithRecovery(ctx, oneRecoveryCandidate{candidate}, r.leaser, r.effects, r.results, r.confirmer, r.zeroEffect, 1)
		if err != nil {
			summary.Unavailable++
			failures = append(failures, err)
			continue
		}
		if len(batch.Inspections) != 1 {
			summary.Unavailable++
			failures = append(failures, ErrApprovalRecoveryPlanInvalid)
			continue
		}
		switch batch.Inspections[0].Disposition {
		case "ORIGINAL_RESULT_CONFIRMED":
			summary.Confirmed++
		case "ZERO_EFFECT_INVALIDATED":
			summary.Invalidated++
		default:
			summary.Pending++
		}
	}
	return summary, errors.Join(failures...)
}
