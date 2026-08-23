package token

import (
	"context"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type OrganizationLifecycleBlocker struct {
	Code         string `json:"code"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

type OrganizationLifecycleAssessment struct {
	OrganizationID uuid.UUID                      `json:"organization_id"`
	Status         string                         `json:"status"`
	AccountVersion int64                          `json:"account_version"`
	Ready          bool                           `json:"ready"`
	Blockers       []OrganizationLifecycleBlocker `json:"blockers"`
}

func (r *PostgresRepository) ApplyOrganizationLifecycle(ctx context.Context, organizationID uuid.UUID, status string, accountVersion int64, preflightOnly bool) (OrganizationLifecycleAssessment, error) {
	status = strings.ToUpper(strings.TrimSpace(status))
	if organizationID == uuid.Nil || accountVersion < 1 || (status != "ACTIVE" && status != "DISABLED" && status != "DELETED") {
		return OrganizationLifecycleAssessment{}, ErrOrganizationLifecycleConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-lifecycle:"+organizationID.String()); err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	var currentStatus string
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT status,account_version FROM organization_lifecycle_states WHERE organization_id=$1 FOR UPDATE`, organizationID).Scan(&currentStatus, &currentVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return OrganizationLifecycleAssessment{}, err
	}
	if err == nil {
		if currentStatus == "DELETED" && status != "DELETED" {
			return OrganizationLifecycleAssessment{}, ErrOrganizationLifecycleConflict
		}
		if accountVersion < currentVersion || (accountVersion == currentVersion && status != currentStatus) {
			return OrganizationLifecycleAssessment{}, ErrOrganizationLifecycleConflict
		}
		if accountVersion == currentVersion && status == currentStatus {
			return OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: accountVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}, tx.Commit(ctx)
		}
	}
	assessment := OrganizationLifecycleAssessment{OrganizationID: organizationID, Status: status, AccountVersion: accountVersion, Ready: true, Blockers: []OrganizationLifecycleBlocker{}}
	if status == "DELETED" {
		if currentStatus != "DISABLED" {
			assessment.Ready = false
			assessment.Blockers = append(assessment.Blockers, OrganizationLifecycleBlocker{Code: "ORGANIZATION_NOT_DISABLED", ResourceType: "organization", ResourceID: organizationID.String()})
			return assessment, tx.Commit(ctx)
		}
		rows, err := tx.Query(ctx, `SELECT id::text FROM usage_requests WHERE organization_id=$1 AND status IN ('CREATED','RESERVED','UPSTREAM_ACTIVE','SETTLEMENT_PENDING','REVIEW') ORDER BY started_at,id`, organizationID)
		if err != nil {
			return OrganizationLifecycleAssessment{}, err
		}
		for rows.Next() {
			var resourceID string
			if err := rows.Scan(&resourceID); err != nil {
				rows.Close()
				return OrganizationLifecycleAssessment{}, err
			}
			assessment.Blockers = append(assessment.Blockers, OrganizationLifecycleBlocker{Code: "UNSETTLED_USAGE_REQUEST", ResourceType: "usage_request", ResourceID: resourceID})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return OrganizationLifecycleAssessment{}, err
		}
		rows.Close()
		assessment.Ready = len(assessment.Blockers) == 0
		if !assessment.Ready {
			return assessment, tx.Commit(ctx)
		}
	}
	if preflightOnly {
		return assessment, tx.Commit(ctx)
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO organization_lifecycle_states(organization_id,status,account_version,updated_at) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id) DO UPDATE SET status=EXCLUDED.status,account_version=EXCLUDED.account_version,updated_at=EXCLUDED.updated_at`, organizationID, status, accountVersion, now); err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	if err := events.AppendOutbox(ctx, tx, "token.organization.lifecycle-applied.v1", organizationID, accountVersion, map[string]any{"organization_id": organizationID, "status": status, "account_version": accountVersion}); err != nil {
		return OrganizationLifecycleAssessment{}, err
	}
	return assessment, tx.Commit(ctx)
}
