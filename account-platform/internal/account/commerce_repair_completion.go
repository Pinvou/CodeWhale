package account

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func (r *PostgresRepository) ObserveCommerceRepair(ctx context.Context, principal identity.Principal, caseID uuid.UUID, in CommerceRepairObservationInput) (out CommerceRepairObservation, err error) {
	if caseID == uuid.Nil || !in.valid() {
		return out, ErrInvalidCommerceRepair
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	seedCase, err := scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id=$1`, caseID))
	if err != nil {
		return out, err
	}
	seed, _, _, err := loadCommerceRepairJob(ctx, tx, seedCase.JobID, "")
	if err != nil {
		return out, err
	}
	actor, err := commerceRepairActor(ctx, tx, principal)
	if err != nil {
		return out, err
	}
	locks, err := lockCommerceRepairAuthorization(ctx, tx, seed, platformAuthorizationRequirement{UserID: actor, Permission: PermissionCommerceRepairsRequest})
	if err != nil {
		return out, err
	}
	j, status, _, subjectStatus, lifecycle, err := lockCommerceRepairJob(ctx, tx, seed)
	if err != nil {
		return out, err
	}
	c, err := scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id=$1 FOR UPDATE`, caseID))
	if err != nil {
		return out, err
	}
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return out, err
	}
	binding, authVersion, _, err := platformAuthorizationAt(ctx, tx, locks[0], now)
	if err != nil {
		return out, err
	}
	// A committed observation is an immutable receipt even if newer evidence
	// or a completion was recorded before this identical retry reached us.
	previous, e := scanCommerceRepairObservation(tx.QueryRow(ctx, `SELECT `+commerceRepairObservationColumns+` FROM commerce_customer_repair_observations WHERE id=$1`, in.ObservationID))
	if e == nil {
		if !previous.matches(caseID, actor, in) {
			return out, ErrConflict
		}
		return previous, tx.Commit(ctx)
	}
	if !errors.Is(e, ErrNotFound) {
		return out, e
	}
	if c.Status != "OPEN" || c.Version != in.ExpectedVersion || !commerceRepairBindingMatches(c, j) || !commerceRepairInspectableJobStatus(status) {
		return out, ErrConflict
	}
	if in.ExternalReferenceRedacted != "" && (subjectStatus != "ACTIVE" && subjectStatus != "DISABLED" || lifecycle != j.SubjectLifecycleVersion) {
		return out, ErrConflict
	}
	out, err = scanCommerceRepairObservation(tx.QueryRow(ctx, `INSERT INTO commerce_customer_repair_observations
(id,case_id,case_version,outcome,evidence_reference,evidence_sha256,all_attempts_ended,external_reference_redacted,proposer_user_id,proposer_binding_id,proposer_authorization_version,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+commerceRepairObservationColumns,
		in.ObservationID, caseID, c.Version+1, in.Outcome, in.EvidenceReference, in.EvidenceSHA256, in.AllAttemptsEnded, in.ExternalReferenceRedacted, actor, binding, authVersion, now))
	if err != nil {
		var conflict *pgconn.PgError
		if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == "commerce_customer_repair_observations_pkey" {
			return out, ErrConflict
		}
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE commerce_customer_repair_cases SET version=version+1 WHERE id=$1`, caseID); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE manual_review_cases SET first_response_at=COALESCE(first_response_at,$2),updated_at=$2 WHERE id=$1 AND status='OPEN'`, caseID, now); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,actor_id,details,created_at)
VALUES($1,$2,'COMMERCE_EVIDENCE_OBSERVED',$3,jsonb_build_object('observation_id',$4::text,'outcome',$5::text,'case_version',$6::bigint),$7)`, uuid.Must(uuid.NewV7()), caseID, actor, out.ID.String(), out.Outcome, out.CaseVersion, now); err != nil {
		return out, err
	}
	if err = appendAudit(ctx, tx, actor, principal.SessionID, nil, "commerce.repair.observe", "commerce_repair_case", caseID.String(), "SUCCEEDED", map[string]any{"observation_id": out.ID, "outcome": out.Outcome, "case_version": out.CaseVersion}); err != nil {
		return out, err
	}
	if err = recheckCommerceRepairAuthorities(ctx, tx, commerceRepairAuthority{Locked: locks[0], BindingID: binding, Version: authVersion}); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (r *PostgresRepository) ConfirmCommerceRepair(ctx context.Context, principal identity.Principal, caseID uuid.UUID, in CommerceRepairConfirmation) (out CommerceRepairCase, err error) {
	if caseID == uuid.Nil || in.ObservationID == uuid.Nil || in.ExpectedVersion < 2 {
		return out, ErrInvalidCommerceRepair
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	seedCase, err := scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id=$1`, caseID))
	if err != nil {
		return out, err
	}
	proposal, err := scanCommerceRepairObservation(tx.QueryRow(ctx, `SELECT `+commerceRepairObservationColumns+` FROM commerce_customer_repair_observations WHERE id=$1 AND case_id=$2`, in.ObservationID, caseID))
	if err != nil {
		return out, err
	}
	actor, err := commerceRepairActor(ctx, tx, principal)
	if err != nil {
		return out, err
	}
	if actor == proposal.ProposerUserID {
		return out, ErrForbidden
	}
	seed, _, _, err := loadCommerceRepairJob(ctx, tx, seedCase.JobID, "")
	if err != nil {
		return out, err
	}
	requirements := []platformAuthorizationRequirement{{UserID: actor, Permission: PermissionCommerceRepairsApprove}}
	// A receipt retry does not perform a second completion. The proposer's
	// authority was checked at the original commit, not at every later read.
	if seedCase.Status != "RESOLVED" {
		requirements = append(requirements, platformAuthorizationRequirement{UserID: proposal.ProposerUserID, Permission: PermissionCommerceRepairsRequest})
	}
	locks, err := lockCommerceRepairAuthorization(ctx, tx, seed, requirements...)
	if err != nil {
		return out, err
	}
	j, status, _, subjectStatus, lifecycle, err := lockCommerceRepairJob(ctx, tx, seed)
	if err != nil {
		return out, err
	}
	c, err := scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id=$1 FOR UPDATE`, caseID))
	if err != nil {
		return out, err
	}
	now, err := approvalDatabaseTime(ctx, tx)
	if err != nil {
		return out, err
	}
	reviewerBinding, reviewerVersion, _, err := platformAuthorizationAt(ctx, tx, locks[0], now)
	if err != nil {
		return out, err
	}
	if c.Status == "RESOLVED" {
		if c.ApprovedObservationID == nil || *c.ApprovedObservationID != in.ObservationID || c.ReviewerUserID == nil || *c.ReviewerUserID != actor || proposal.CaseVersion != in.ExpectedVersion {
			return out, ErrConflict
		}
		return c, tx.Commit(ctx)
	}
	if len(locks) != 2 {
		return out, ErrConflict
	}
	binding, version, _, err := platformAuthorizationAt(ctx, tx, locks[1], now)
	if err != nil {
		return out, err
	}
	if binding != proposal.ProposerBindingID || version != proposal.ProposerAuthorizationVersion {
		return out, ErrForbidden
	}
	if c.Version != in.ExpectedVersion || proposal.CaseVersion != c.Version || !commerceRepairBindingMatches(c, j) ||
		!proposal.AllAttemptsEnded || proposal.Outcome == "UNCONFIRMED" || !commerceRepairInspectableJobStatus(status) {
		return out, ErrConflict
	}
	var currentMapping bool
	err = tx.QueryRow(ctx, `SELECT subject_type=$2 AND subject_id=$3 AND provider_id=$4 AND adapter_version=$5 AND version=$6 AND status<>'DETACHED'
FROM commerce_customer_mappings WHERE id=$1`, j.MappingID, j.SubjectType, j.SubjectID, j.ProviderID, j.AdapterVersion, j.MappingVersion).Scan(&currentMapping)
	if err != nil {
		return out, err
	}
	current := currentMapping && subjectStatus == "ACTIVE" && lifecycle == j.SubjectLifecycleVersion && status == "REPAIR_REQUIRED"
	resolution := "ENDED_NO_EFFECT"
	if proposal.Outcome == "SUCCEEDED" {
		resolution = "SUCCEEDED_EVIDENCE_ONLY"
		if current {
			resolution = "SUCCEEDED_CURRENT"
			if _, err = tx.Exec(ctx, `UPDATE commerce_customer_mappings SET status='READY',external_reference_redacted=NULLIF($2,''),last_error_code=NULL,provisioned_at=$3,version=version+1,updated_at=$3 WHERE id=$1`, j.MappingID, proposal.ExternalReferenceRedacted, now); err != nil {
				return out, err
			}
		}
		outcome := "STALE"
		reference := ""
		if current {
			outcome = "SUCCEEDED"
			reference = proposal.ExternalReferenceRedacted
		}
		if _, err = tx.Exec(ctx, `INSERT INTO commerce_customer_sync_evidence(id,job_id,mapping_id,provider_id,adapter_version,outcome,external_reference_redacted,recorded_at) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`, uuid.Must(uuid.NewV7()), j.ID, j.MappingID, j.ProviderID, j.AdapterVersion, outcome, reference, now); err != nil {
			return out, err
		}
	}
	// Retired/closed generations retain their original job and mapping facts.
	// Only the case receipt and its evidence may change for those generations.
	if current {
		jobStatus := "MANUAL_NO_EFFECT"
		if proposal.Outcome == "SUCCEEDED" {
			jobStatus = "COMPLETED"
		}
		if _, err = tx.Exec(ctx, `UPDATE commerce_customer_sync_jobs SET status=$2,lease_until=NULL,completed_at=$3,updated_at=$3 WHERE id=$1`, j.ID, jobStatus, now); err != nil {
			return out, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE commerce_customer_repair_cases SET status='RESOLVED',resolution=$2,approved_observation_id=$3,reviewer_user_id=$4,resolved_at=$5,reviewer_binding_id=$6,reviewer_authorization_version=$7,version=version+1 WHERE id=$1`, caseID, resolution, proposal.ID, actor, now, reviewerBinding, reviewerVersion); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=$2,updated_at=$2 WHERE id=$1 AND status='OPEN'`, caseID, now); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO manual_review_case_events(id,case_id,event_type,actor_id,details,created_at) VALUES($1,$2,'COMMERCE_COMPLETION_VERIFIED',$3,jsonb_build_object('observation_id',$4::text,'resolution',$5::text,'proposer_user_id',$6::text),$7)`, uuid.Must(uuid.NewV7()), caseID, actor, proposal.ID.String(), resolution, proposal.ProposerUserID.String(), now); err != nil {
		return out, err
	}
	if err = appendAudit(ctx, tx, actor, principal.SessionID, nil, "commerce.repair.confirm", "commerce_repair_case", caseID.String(), "SUCCEEDED", map[string]any{"observation_id": proposal.ID, "resolution": resolution, "proposer_user_id": proposal.ProposerUserID, "proposer_binding_id": proposal.ProposerBindingID, "proposer_authorization_version": proposal.ProposerAuthorizationVersion, "reviewer_binding_id": reviewerBinding, "reviewer_authorization_version": reviewerVersion}); err != nil {
		return out, err
	}
	out, err = scanCommerceRepairCase(tx.QueryRow(ctx, `SELECT `+commerceRepairCaseColumns+` FROM commerce_customer_repair_cases WHERE id=$1`, caseID))
	if err != nil {
		return out, err
	}
	if err = recheckCommerceRepairAuthorities(ctx, tx,
		commerceRepairAuthority{Locked: locks[0], BindingID: reviewerBinding, Version: reviewerVersion},
		commerceRepairAuthority{Locked: locks[1], BindingID: proposal.ProposerBindingID, Version: proposal.ProposerAuthorizationVersion}); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
