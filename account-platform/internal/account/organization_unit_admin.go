package account

import (
	"context"
	"errors"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) AdminUnitState(ctx context.Context, organizationID, unitID uuid.UUID) (UnitMutationResult, error) {
	var result UnitMutationResult
	err := r.pool.QueryRow(ctx, `SELECT unit.id,unit.organization_id,unit.parent_id,unit.name,unit.status,unit.version,organization.structure_version FROM organization_units unit JOIN organizations organization ON organization.id=unit.organization_id WHERE unit.organization_id=$1 AND unit.id=$2`, organizationID, unitID).Scan(&result.Unit.ID, &result.Unit.OrganizationID, &result.Unit.ParentID, &result.Unit.Name, &result.Unit.Status, &result.Unit.Version, &result.StructureVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return UnitMutationResult{}, ErrNotFound
	}
	return result, err
}

func (r *PostgresRepository) AdminCreateUnit(ctx context.Context, principal identity.Principal, organizationID uuid.UUID, parentID *uuid.UUID, name, idempotencyKey string) (UnitMutationResult, error) {
	name = strings.TrimSpace(name)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if name == "" || len([]rune(name)) > 120 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return UnitMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UnitMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if actorID == nil {
		return UnitMutationResult{}, ErrForbidden
	}
	requestHash := organizationUnitRequestHash("CREATE", uuid.Nil, parentID, name, 0)
	replayed, err := lockOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "CREATE", idempotencyKey, requestHash)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	var organizationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return UnitMutationResult{}, ErrNotFound
	} else if err != nil {
		return UnitMutationResult{}, err
	}
	if organizationStatus != "ACTIVE" {
		return UnitMutationResult{}, ErrConflict
	}
	if parentID != nil {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id=$1 AND id=$2 AND status='ACTIVE')`, organizationID, *parentID).Scan(&active); err != nil {
			return UnitMutationResult{}, err
		}
		if !active {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
	}
	now := r.now()
	unit := Unit{ID: uuid.Must(uuid.NewV7()), OrganizationID: organizationID, ParentID: parentID, Name: name, Status: "ACTIVE", Version: 1}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_units(id,organization_id,parent_id,name,status,created_at,updated_at) VALUES($1,$2,$3,$4,'ACTIVE',$5,$5)`, unit.ID, organizationID, parentID, name, now); err != nil {
		return UnitMutationResult{}, mapConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id,ancestor_id,descendant_id,depth) VALUES($1,$2,$2,0)`, organizationID, unit.ID); err != nil {
		return UnitMutationResult{}, err
	}
	if parentID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO organization_unit_closure(organization_id,ancestor_id,descendant_id,depth) SELECT organization_id,ancestor_id,$3,depth+1 FROM organization_unit_closure WHERE organization_id=$1 AND descendant_id=$2`, organizationID, *parentID, unit.ID); err != nil {
			return UnitMutationResult{}, err
		}
	}
	structureVersion, err := nextOrganizationStructureVersion(ctx, tx, organizationID, now)
	if err != nil {
		return UnitMutationResult{}, err
	}
	result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
	if err := saveOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "CREATE", idempotencyKey, requestHash, result, now); err != nil {
		return UnitMutationResult{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.create", "organization_unit", unit.ID.String(), map[string]any{"name": name, "parent_id": parentID, "idempotency_key": idempotencyKey, "structure_version": structureVersion}); err != nil {
		return UnitMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminRenameUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID, name, idempotencyKey string) (UnitMutationResult, error) {
	name = strings.TrimSpace(name)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if name == "" || len([]rune(name)) > 120 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return UnitMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UnitMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if actorID == nil {
		return UnitMutationResult{}, ErrForbidden
	}
	requestHash := organizationUnitRequestHash("RENAME", unitID, nil, name, 0)
	replayed, err := lockOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "RENAME", idempotencyKey, requestHash)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	var organizationStatus string
	var structureVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,structure_version FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus, &structureVersion); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return UnitMutationResult{}, ErrNotFound
	} else if err != nil {
		return UnitMutationResult{}, err
	}
	var unit Unit
	err = tx.QueryRow(ctx, `SELECT id,organization_id,parent_id,name,status,version FROM organization_units WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, unitID).Scan(&unit.ID, &unit.OrganizationID, &unit.ParentID, &unit.Name, &unit.Status, &unit.Version)
	if errors.Is(err, pgx.ErrNoRows) || unit.Status == "DELETED" {
		return UnitMutationResult{}, ErrNotFound
	}
	if err != nil {
		return UnitMutationResult{}, err
	}
	if unit.Status != "ACTIVE" {
		return UnitMutationResult{}, ErrConflict
	}
	result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
	if unit.Name != name {
		oldName := unit.Name
		now := r.now()
		unit.Name = name
		unit.Version++
		if _, err := tx.Exec(ctx, `UPDATE organization_units SET name=$3,version=version+1,updated_at=$4 WHERE organization_id=$1 AND id=$2`, organizationID, unitID, name, now); err != nil {
			return UnitMutationResult{}, mapConstraintError(err)
		}
		structureVersion, err = nextOrganizationStructureVersion(ctx, tx, organizationID, now)
		if err != nil {
			return UnitMutationResult{}, err
		}
		result = UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
		if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.rename", "organization_unit", unitID.String(), map[string]any{"from": oldName, "to": name, "idempotency_key": idempotencyKey, "structure_version": structureVersion}); err != nil {
			return UnitMutationResult{}, err
		}
	}
	if err := saveOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "RENAME", idempotencyKey, requestHash, result, r.now()); err != nil {
		return UnitMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminMoveUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID, newParentID *uuid.UUID, expectedStructureVersion int64, idempotencyKey string) (UnitMutationResult, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if expectedStructureVersion < 1 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return UnitMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UnitMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return UnitMutationResult{}, err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if actorID == nil {
		return UnitMutationResult{}, ErrForbidden
	}
	requestHash := organizationUnitRequestHash("MOVE", unitID, newParentID, "", expectedStructureVersion)
	replayed, err := lockOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "MOVE", idempotencyKey, requestHash)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	var organizationStatus string
	var structureVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,structure_version FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus, &structureVersion); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return UnitMutationResult{}, ErrNotFound
	} else if err != nil {
		return UnitMutationResult{}, err
	}
	if organizationStatus != "ACTIVE" || structureVersion != expectedStructureVersion {
		return UnitMutationResult{}, ErrConflict
	}
	var unit Unit
	err = tx.QueryRow(ctx, `SELECT id,organization_id,parent_id,name,status,version FROM organization_units WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, unitID).Scan(&unit.ID, &unit.OrganizationID, &unit.ParentID, &unit.Name, &unit.Status, &unit.Version)
	if errors.Is(err, pgx.ErrNoRows) || unit.Status == "DELETED" {
		return UnitMutationResult{}, ErrNotFound
	}
	if err != nil {
		return UnitMutationResult{}, err
	}
	if unit.Status != "ACTIVE" {
		return UnitMutationResult{}, ErrConflict
	}
	if newParentID != nil {
		if *newParentID == unitID {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
		var invalidParent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_unit_closure WHERE organization_id=$1 AND ancestor_id=$2 AND descendant_id=$3)`, organizationID, unitID, *newParentID).Scan(&invalidParent); err != nil {
			return UnitMutationResult{}, err
		}
		if invalidParent {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
		var parentActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_units WHERE organization_id=$1 AND id=$2 AND status='ACTIVE')`, organizationID, *newParentID).Scan(&parentActive); err != nil {
			return UnitMutationResult{}, err
		}
		if !parentActive {
			return UnitMutationResult{}, ErrInvalidHierarchy
		}
	}
	if (unit.ParentID == nil && newParentID == nil) || (unit.ParentID != nil && newParentID != nil && *unit.ParentID == *newParentID) {
		result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
		if err := saveOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "MOVE", idempotencyKey, requestHash, result, r.now()); err != nil {
			return UnitMutationResult{}, err
		}
		return result, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM organization_unit_closure closure
WHERE closure.organization_id=$1
  AND closure.descendant_id IN (SELECT descendant_id FROM organization_unit_closure WHERE organization_id=$1 AND ancestor_id=$2)
  AND closure.ancestor_id NOT IN (SELECT descendant_id FROM organization_unit_closure WHERE organization_id=$1 AND ancestor_id=$2)`, organizationID, unitID); err != nil {
		return UnitMutationResult{}, err
	}
	if newParentID != nil {
		if _, err := tx.Exec(ctx, `
INSERT INTO organization_unit_closure(organization_id,ancestor_id,descendant_id,depth)
SELECT $1,parent_path.ancestor_id,subtree.descendant_id,parent_path.depth+subtree.depth+1
FROM organization_unit_closure parent_path
CROSS JOIN organization_unit_closure subtree
WHERE parent_path.organization_id=$1 AND parent_path.descendant_id=$3
  AND subtree.organization_id=$1 AND subtree.ancestor_id=$2`, organizationID, unitID, *newParentID); err != nil {
			return UnitMutationResult{}, err
		}
	}
	now := r.now()
	unit.ParentID = newParentID
	unit.Version++
	if _, err := tx.Exec(ctx, `UPDATE organization_units SET parent_id=$3,version=version+1,updated_at=$4 WHERE organization_id=$1 AND id=$2`, organizationID, unitID, newParentID, now); err != nil {
		return UnitMutationResult{}, mapConstraintError(err)
	}
	structureVersion, err = nextOrganizationStructureVersion(ctx, tx, organizationID, now)
	if err != nil {
		return UnitMutationResult{}, err
	}
	result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
	if err := saveOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "MOVE", idempotencyKey, requestHash, result, now); err != nil {
		return UnitMutationResult{}, err
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.move", "organization_unit", unitID.String(), map[string]any{"new_parent_id": newParentID, "idempotency_key": idempotencyKey, "structure_version": structureVersion}); err != nil {
		return UnitMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

type unitBlockerQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func organizationUnitDeletionBlockers(ctx context.Context, queryer unitBlockerQueryer, organizationID, unitID uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	rows, err := queryer.Query(ctx, `
SELECT code,resource_type,resource_id FROM (
  SELECT 'PROTECTED_CHILD_UNIT' code,'organization_unit' resource_type,id::text resource_id
  FROM organization_units WHERE organization_id=$1 AND parent_id=$2 AND status<>'DELETED'
  UNION ALL
  SELECT 'MEMBER_ATTACHED','membership',unit_membership.membership_id::text
  FROM membership_units unit_membership
  JOIN memberships membership ON membership.id=unit_membership.membership_id
  WHERE unit_membership.unit_id=$2 AND membership.status<>'REVOKED'
  UNION ALL
  SELECT 'ROLE_BINDING_ATTACHED','role_binding',binding.id::text
  FROM role_bindings binding
  JOIN memberships membership ON membership.id=binding.membership_id
  WHERE binding.unit_id=$2 AND membership.status<>'REVOKED'
    AND binding.revoked_at IS NULL AND (binding.expires_at IS NULL OR binding.expires_at>now())
  UNION ALL
  SELECT 'PENDING_APPROVAL_REQUEST','approval_request',id::text
  FROM approval_requests WHERE target_resource_type='organization_unit' AND target_resource_id=$2::text
    AND status IN ('DRAFT','SUBMITTED','APPROVED','EXECUTING','EXECUTION_FAILED')
  UNION ALL
  SELECT blocker_code,resource_type,resource_id
  FROM organization_unit_governance_blockers WHERE organization_id=$1 AND unit_id=$2 AND active
) blockers ORDER BY code,resource_type,resource_id`, organizationID, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blockers := make([]OrganizationLifecycleBlocker, 0)
	for rows.Next() {
		var blocker OrganizationLifecycleBlocker
		blocker.Domain = "ACCOUNT"
		if err := rows.Scan(&blocker.Code, &blocker.ResourceType, &blocker.ResourceID); err != nil {
			return nil, err
		}
		blockers = append(blockers, blocker)
	}
	return blockers, rows.Err()
}

func (r *PostgresRepository) AdminUnitDeletionBlockers(ctx context.Context, organizationID, unitID uuid.UUID) ([]OrganizationLifecycleBlocker, error) {
	return organizationUnitDeletionBlockers(ctx, r.pool, organizationID, unitID)
}

func (r *PostgresRepository) AdminDeleteUnit(ctx context.Context, principal identity.Principal, organizationID, unitID uuid.UUID, idempotencyKey string) (UnitMutationResult, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		return UnitMutationResult{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UnitMutationResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, organizationID.String()); err != nil {
		return UnitMutationResult{}, err
	}
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if actorID == nil {
		return UnitMutationResult{}, ErrForbidden
	}
	requestHash := organizationUnitRequestHash("DELETE", unitID, nil, "", 0)
	replayed, err := lockOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "DELETE", idempotencyKey, requestHash)
	if err != nil {
		return UnitMutationResult{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	var organizationStatus string
	var structureVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,structure_version FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&organizationStatus, &structureVersion); errors.Is(err, pgx.ErrNoRows) || organizationStatus == "DELETED" {
		return UnitMutationResult{}, ErrNotFound
	} else if err != nil {
		return UnitMutationResult{}, err
	}
	var unit Unit
	err = tx.QueryRow(ctx, `SELECT id,organization_id,parent_id,name,status,version FROM organization_units WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, unitID).Scan(&unit.ID, &unit.OrganizationID, &unit.ParentID, &unit.Name, &unit.Status, &unit.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return UnitMutationResult{}, ErrNotFound
	}
	if err != nil {
		return UnitMutationResult{}, err
	}
	result := UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
	if unit.Status != "DELETED" {
		blockers, err := organizationUnitDeletionBlockers(ctx, tx, organizationID, unitID)
		if err != nil {
			return UnitMutationResult{}, err
		}
		if len(blockers) > 0 {
			return UnitMutationResult{}, &OrganizationUnitBlockedError{OrganizationID: organizationID, UnitID: unitID, Blockers: blockers}
		}
		now := r.now()
		unit.Status = "DELETED"
		unit.Version++
		if _, err := tx.Exec(ctx, `UPDATE organization_units SET status='DELETED',deleted_at=$3,version=version+1,updated_at=$3 WHERE organization_id=$1 AND id=$2`, organizationID, unitID, now); err != nil {
			return UnitMutationResult{}, err
		}
		structureVersion, err = nextOrganizationStructureVersion(ctx, tx, organizationID, now)
		if err != nil {
			return UnitMutationResult{}, err
		}
		result = UnitMutationResult{Unit: unit, StructureVersion: structureVersion}
		if err := appendAdminAudit(ctx, tx, actorID, principal, &organizationID, "admin.organization_unit.delete", "organization_unit", unitID.String(), map[string]any{"name": unit.Name, "idempotency_key": idempotencyKey, "structure_version": structureVersion}); err != nil {
			return UnitMutationResult{}, err
		}
	}
	if err := saveOrganizationUnitCommand(ctx, tx, *actorID, organizationID, "DELETE", idempotencyKey, requestHash, result, r.now()); err != nil {
		return UnitMutationResult{}, err
	}
	return result, tx.Commit(ctx)
}

type OrganizationUnitBlockedError struct {
	OrganizationID uuid.UUID
	UnitID         uuid.UUID
	Blockers       []OrganizationLifecycleBlocker
}

func (e *OrganizationUnitBlockedError) Error() string {
	return "organization unit deletion is blocked"
}
