package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type OrganizationUnitBlocker struct {
	Code         string `json:"code"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

type OrganizationUnitDeletionAssessment struct {
	OrganizationID   uuid.UUID                 `json:"organization_id"`
	UnitID           uuid.UUID                 `json:"unit_id"`
	Status           string                    `json:"status"`
	StructureVersion int64                     `json:"structure_version"`
	Ready            bool                      `json:"ready"`
	Blockers         []OrganizationUnitBlocker `json:"blockers"`
}

func (r *PostgresRepository) ApplyOrganizationUnitLifecycle(ctx context.Context, organizationID, unitID uuid.UUID, status string, structureVersion int64) (OrganizationUnitDeletionAssessment, error) {
	status = strings.ToUpper(strings.TrimSpace(status))
	if organizationID == uuid.Nil || unitID == uuid.Nil || structureVersion < 1 || (status != "ACTIVE" && status != "DELETION_PENDING" && status != "DELETED") {
		return OrganizationUnitDeletionAssessment{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-unit:"+organizationID.String()+":"+unitID.String()); err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	var currentStatus string
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT status,structure_version FROM organization_unit_lifecycle_states WHERE organization_id=$1 AND unit_id=$2 FOR UPDATE`, organizationID, unitID).Scan(&currentStatus, &currentVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return OrganizationUnitDeletionAssessment{}, err
	}
	if errors.Is(err, pgx.ErrNoRows) && status == "DELETED" {
		return OrganizationUnitDeletionAssessment{}, ErrConflict
	}
	if err == nil {
		if currentStatus == "DELETED" && status != "DELETED" {
			return OrganizationUnitDeletionAssessment{}, ErrConflict
		}
		if (currentStatus == "DELETION_PENDING" && status == "ACTIVE") || (currentStatus == "ACTIVE" && status == "DELETED") {
			return OrganizationUnitDeletionAssessment{}, ErrConflict
		}
		if structureVersion < currentVersion || (structureVersion == currentVersion && status != currentStatus && !(currentStatus == "ACTIVE" && status == "DELETION_PENDING")) {
			return OrganizationUnitDeletionAssessment{}, ErrConflict
		}
	}
	assessment, err := organizationUnitDeletionBlockers(ctx, tx, organizationID, unitID)
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	assessment.Status = status
	assessment.StructureVersion = structureVersion
	if status == "ACTIVE" {
		assessment.Ready = true
		assessment.Blockers = []OrganizationUnitBlocker{}
	}
	if status == "DELETED" && !assessment.Ready {
		return assessment, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_lifecycle_states(organization_id,unit_id,status,structure_version,updated_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,unit_id) DO UPDATE SET status=EXCLUDED.status,structure_version=EXCLUDED.structure_version,updated_at=EXCLUDED.updated_at`, organizationID, unitID, status, structureVersion, time.Now().UTC()); err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	return assessment, tx.Commit(ctx)
}

type organizationUnitQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func organizationUnitDeletionBlockers(ctx context.Context, queryer organizationUnitQueryer, organizationID, unitID uuid.UUID) (OrganizationUnitDeletionAssessment, error) {
	assessment := OrganizationUnitDeletionAssessment{OrganizationID: organizationID, UnitID: unitID, Ready: true, Blockers: []OrganizationUnitBlocker{}}
	rows, err := queryer.Query(ctx, `SELECT blocker_code,resource_type,resource_id FROM organization_unit_governance_blockers WHERE organization_id=$1 AND unit_id=$2 AND active ORDER BY blocker_code,resource_type,resource_id`, organizationID, unitID)
	if err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var blocker OrganizationUnitBlocker
		if err := rows.Scan(&blocker.Code, &blocker.ResourceType, &blocker.ResourceID); err != nil {
			return OrganizationUnitDeletionAssessment{}, err
		}
		assessment.Blockers = append(assessment.Blockers, blocker)
	}
	if err := rows.Err(); err != nil {
		return OrganizationUnitDeletionAssessment{}, err
	}
	assessment.Ready = len(assessment.Blockers) == 0
	return assessment, nil
}

func (r *PostgresRepository) SetOrganizationUnitGovernanceBlocker(ctx context.Context, organizationID, unitID uuid.UUID, code, resourceType, resourceID string, active bool, details json.RawMessage, now time.Time) error {
	if organizationID == uuid.Nil || unitID == uuid.Nil || strings.TrimSpace(code) == "" || strings.TrimSpace(resourceType) == "" || strings.TrimSpace(resourceID) == "" {
		return ErrConflict
	}
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-unit:"+organizationID.String()+":"+unitID.String()); err != nil {
		return err
	}
	if active {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM organization_unit_lifecycle_states WHERE organization_id=$1 AND unit_id=$2`, organizationID, unitID).Scan(&status)
		if err == nil && status != "ACTIVE" {
			return ErrConflict
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_governance_blockers(organization_id,unit_id,blocker_code,resource_type,resource_id,active,details,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(organization_id,unit_id,blocker_code,resource_type,resource_id) DO UPDATE SET active=EXCLUDED.active,details=EXCLUDED.details,updated_at=EXCLUDED.updated_at`, organizationID, unitID, strings.TrimSpace(code), strings.TrimSpace(resourceType), strings.TrimSpace(resourceID), active, details, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
