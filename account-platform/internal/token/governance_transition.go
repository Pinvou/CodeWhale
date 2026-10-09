package token

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GovernanceTransitionRequest is exclusively an offline operations contract.
// It is never accepted by an administrator or service HTTP handler.
type GovernanceTransitionRequest struct {
	ExpectedState        string
	ExpectedEpoch        int64
	TargetState          string
	OperatorID           string
	Reason               string
	RequestID            uuid.UUID
	ApprovedRuntimeRoles []string
	Apply                bool
}

// CheckOrTransitionTokenGovernance keeps the authoritative policy read locked
// until the Token CAS commits. This is coordinated validation, not cross-DB
// atomic business execution. No runtime identity is granted or opened here.
func CheckOrTransitionTokenGovernance(ctx context.Context, tokenPool, accountPool *pgxpool.Pool, input GovernanceTransitionRequest) (TokenGovernanceSnapshot, error) {
	var current TokenGovernanceSnapshot
	if tokenPool == nil {
		return current, errors.New("Token governance database is required")
	}
	handlerDigest, err := TokenCommandHandlerDigest()
	if err != nil {
		return current, err
	}
	policyDigest, err := approvalpolicy.PolicySetDigest()
	if err != nil {
		return current, err
	}
	// Emergency SAFE does not depend on Account availability or writer drain.
	lookupOnly := input.TargetState == "" && !input.Apply && input.RequestID != uuid.Nil
	if input.TargetState != "SAFE_ROLLBACK" && !lookupOnly {
		if accountPool == nil {
			return current, errors.New("authoritative policy database is required")
		}
		accountTx, beginErr := accountPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if beginErr != nil {
			return current, errors.New("authoritative policy database is unavailable")
		}
		defer accountTx.Rollback(ctx)
		if _, err = accountTx.Exec(ctx, `LOCK TABLE approval_command_policies IN SHARE MODE NOWAIT`); err != nil {
			return current, errors.New("authoritative policy facts cannot be locked")
		}
		if _, err = approvalpolicy.VerifyPolicySet(ctx, accountTx); err != nil {
			return current, err
		}
	}
	tx, err := tokenPool.Begin(ctx)
	if err != nil {
		return current, errors.New("Token governance database is unavailable")
	}
	defer tx.Rollback(ctx)
	var trusted bool
	if err = tx.QueryRow(ctx, `SELECT session_user='token_migrator' OR rolsuper FROM pg_catalog.pg_roles WHERE rolname=session_user`).Scan(&trusted); err != nil || !trusted {
		return current, errors.New("dedicated token_migrator identity is required")
	}
	// The trusted migration login inherits this NOLOGIN owner; the runtime
	// cannot SET ROLE to it. Use LOCAL so connection reuse never keeps the role.
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE token_schema_owner`); err != nil {
		return current, errors.New("trusted governance owner is unavailable")
	}
	if lookupOnly {
		if err = tx.QueryRow(ctx, `SELECT state,epoch,policy_set_digest,handler_set_digest,approved_runtime_roles,changed_at FROM public.token_command_governance_transitions WHERE request_id=$1`, input.RequestID).Scan(&current.State, &current.Epoch, &current.PolicySetDigest, &current.HandlerSetDigest, &current.ApprovedRuntimeRoles, &current.UpdatedAt); err != nil {
			return current, errors.New("the original transition result is not available; no transition was submitted")
		}
		return current, nil
	}
	if err = tx.QueryRow(ctx, `SELECT state,epoch,coalesce(policy_set_digest,''),coalesce(handler_set_digest,''),approved_runtime_roles,updated_at FROM public.token_command_governance_activation WHERE singleton=TRUE FOR UPDATE`).Scan(&current.State, &current.Epoch, &current.PolicySetDigest, &current.HandlerSetDigest, &current.ApprovedRuntimeRoles, &current.UpdatedAt); err != nil {
		return current, errors.New("persistent governance state is unavailable")
	}
	if current.UpdatedAt.IsZero() || (current.State == "PRE_ACTIVATION" && (current.Epoch != 0 || current.PolicySetDigest != "" || current.HandlerSetDigest != "" || len(current.ApprovedRuntimeRoles) != 0)) || (current.State != "PRE_ACTIVATION" && current.State != "ACTIVE" && current.State != "SAFE_ROLLBACK") {
		return current, errors.New("persistent governance state is invalid")
	}
	if current.State != "PRE_ACTIVATION" && (current.Epoch < 1 || current.PolicySetDigest != policyDigest || current.HandlerSetDigest != handlerDigest) {
		return current, errors.New("installed release differs from persistent governance evidence")
	}
	if input.TargetState == "" && !input.Apply {
		return current, nil
	}
	if input.ExpectedState != current.State || input.ExpectedEpoch != current.Epoch || input.RequestID == uuid.Nil || strings.TrimSpace(input.OperatorID) != input.OperatorID || utf8.RuneCountInString(input.OperatorID) < 1 || utf8.RuneCountInString(input.OperatorID) > 128 || strings.TrimSpace(input.Reason) != input.Reason || utf8.RuneCountInString(input.Reason) < 1 || utf8.RuneCountInString(input.Reason) > 500 {
		return current, errors.New("governance CAS or operation evidence is invalid")
	}
	allowed := (current.State == "PRE_ACTIVATION" && current.Epoch == 0 && input.TargetState == "ACTIVE") || (current.State == "ACTIVE" && input.TargetState == "SAFE_ROLLBACK") || (current.State == "SAFE_ROLLBACK" && input.TargetState == "ACTIVE")
	if !allowed {
		return current, errors.New("governance transition is not allowed")
	}
	roles := slices.Clone(input.ApprovedRuntimeRoles)
	if input.TargetState == "ACTIVE" {
		if err = checkGovernanceWriterQuiescence(ctx, tx, roles); err != nil {
			return current, err
		}
	} else {
		if len(roles) > 0 && !slices.Equal(roles, current.ApprovedRuntimeRoles) {
			return current, errors.New("SAFE must preserve the active writer identities")
		}
		roles = slices.Clone(current.ApprovedRuntimeRoles)
	}
	if !input.Apply {
		return current, nil
	}
	result, err := tx.Exec(ctx, `UPDATE public.token_command_governance_activation SET state=$1,epoch=epoch+1,policy_set_digest=$2,handler_set_digest=$3,approved_runtime_roles=$4,operator_id=$5,reason=$6,request_id=$7,updated_at=clock_timestamp() WHERE singleton=TRUE AND state=$8 AND epoch=$9`, input.TargetState, policyDigest, handlerDigest, roles, input.OperatorID, input.Reason, input.RequestID, input.ExpectedState, input.ExpectedEpoch)
	if err != nil || result.RowsAffected() != 1 {
		return current, errors.New("governance CAS was rejected")
	}
	if err = tx.QueryRow(ctx, `SELECT state,epoch,policy_set_digest,handler_set_digest,approved_runtime_roles,updated_at FROM public.token_command_governance_activation WHERE singleton=TRUE`).Scan(&current.State, &current.Epoch, &current.PolicySetDigest, &current.HandlerSetDigest, &current.ApprovedRuntimeRoles, &current.UpdatedAt); err != nil {
		return current, errors.New("governance result cannot be read")
	}
	if err = tx.Commit(ctx); err != nil {
		return current, errors.New("governance commit is uncertain; check the original request ID before any transition")
	}
	return current, nil
}

func checkGovernanceWriterQuiescence(ctx context.Context, tx pgx.Tx, candidates []string) error {
	if len(candidates) == 0 || !slices.IsSorted(candidates) {
		return errors.New("candidate writer identities must be nonempty and sorted")
	}
	var canInspect, groupClosed bool
	if err := tx.QueryRow(ctx, `SELECT r.rolsuper OR pg_catalog.pg_has_role(r.oid,'pg_read_all_stats','MEMBER'),NOT g.rolcanlogin AND NOT g.rolsuper AND NOT g.rolcreaterole AND NOT g.rolcreatedb AND NOT g.rolbypassrls FROM pg_catalog.pg_roles r CROSS JOIN pg_catalog.pg_roles g WHERE r.rolname=session_user AND g.rolname='token_gateway'`).Scan(&canInspect, &groupClosed); err != nil || !canInspect || !groupClosed {
		return errors.New("writer inventory requires activity visibility and a NOLOGIN runtime group")
	}
	for index, role := range candidates {
		if role == "" || strings.TrimSpace(role) != role || role == "token_gateway" || (index > 0 && role == candidates[index-1]) {
			return errors.New("candidate writer identities are invalid")
		}
		var safe bool
		if err := tx.QueryRow(ctx, `SELECT NOT r.rolcanlogin AND NOT r.rolsuper AND NOT r.rolcreaterole AND NOT r.rolcreatedb AND NOT r.rolbypassrls AND pg_catalog.pg_has_role(r.oid,'token_gateway','MEMBER') AND NOT pg_catalog.pg_has_role(r.oid,'token_schema_owner','MEMBER') AND NOT pg_catalog.pg_has_role(r.oid,(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER') AND NOT EXISTS(SELECT 1 FROM public.token_command_governance_transitions WHERE state='ACTIVE' AND $1::text=ANY(approved_runtime_roles)) FROM pg_catalog.pg_roles r WHERE r.rolname=$1::text`, role).Scan(&safe); err != nil || !safe {
			return fmt.Errorf("candidate writer %q is unsafe, unavailable, or retired", role)
		}
	}
	// Freeze every Token public-domain table, not a hand-picked subset which
	// could silently miss a newly installed handler's effects. Pending work is
	// retained: this check never deletes or drains Saga/approval history.
	rows, err := tx.Query(ctx, `SELECT c.relname,pg_catalog.pg_get_userbyid(c.relowner) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND c.relname NOT IN ('schema_migrations','token_command_governance_activation','token_command_governance_transitions') ORDER BY c.relname`)
	if err != nil {
		return errors.New("domain writer inventory is unavailable")
	}
	type domainTable struct{ name, owner string }
	var tables []domainTable
	for rows.Next() {
		var table domainTable
		if err = rows.Scan(&table.name, &table.owner); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, table)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(tables) == 0 {
		return errors.New("domain writer inventory is empty or unavailable")
	}
	// Installed tables have distinct protected owners. Only the trusted
	// migrator can assume them; held locks survive subsequent SET ROLE.
	for _, table := range tables {
		if !slices.Contains([]string{"token_gateway", "token_cap_policy_deployer", "token_schema_owner"}, table.owner) {
			return errors.New("unexpected domain owner prevents activation")
		}
		if _, err = tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{table.owner}.Sanitize()); err != nil {
			return errors.New("trusted domain lock identity is unavailable")
		}
		if _, err = tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{"public", table.name}.Sanitize()+" IN ACCESS EXCLUSIVE MODE NOWAIT"); err != nil {
			return errors.New("domain writers have not quiesced")
		}
	}
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE token_schema_owner`); err != nil {
		return errors.New("trusted governance identity is unavailable")
	}
	var unsafe int64
	err = tx.QueryRow(ctx, `WITH writers AS (
 SELECT r.oid,r.rolname,r.rolcanlogin,r.rolsuper FROM pg_catalog.pg_roles r
 WHERE EXISTS (
  SELECT 1 FROM pg_catalog.pg_roles g WHERE (g.oid=r.oid OR pg_catalog.pg_has_role(r.oid,g.oid,'MEMBER')) AND (
   pg_catalog.pg_has_role(g.oid,'token_gateway','MEMBER') OR
   EXISTS (SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND pg_catalog.has_table_privilege(g.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE')) OR
   EXISTS (SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.prosecdef AND pg_catalog.has_function_privilege(g.oid,p.oid,'EXECUTE')))))
 SELECT count(*) FROM writers w WHERE (NOT w.rolsuper AND w.rolname<>session_user AND w.rolcanlogin) OR EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity a WHERE a.usesysid=w.oid AND a.datname=current_database() AND a.pid<>pg_backend_pid())`).Scan(&unsafe)
	if err != nil || unsafe != 0 {
		return errors.New("writer logins, inherited privileges, or old sessions remain")
	}
	return nil
}
