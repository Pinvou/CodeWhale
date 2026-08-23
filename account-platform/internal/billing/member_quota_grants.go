package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func normalizeMemberQuotaAllocation(request MemberQuotaAllocation) (MemberQuotaAllocation, string, error) {
	request.ComponentType = strings.ToUpper(strings.TrimSpace(request.ComponentType))
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.OrganizationID == uuid.Nil || request.MemberUserID == uuid.Nil || request.PackageID == uuid.Nil || request.ComponentID == uuid.Nil || request.CreatedBy == uuid.Nil || request.AmountMicrocredits <= 0 || request.MembershipVersion < 1 || (request.ComponentType != "BASE" && request.ComponentType != "BONUS") || len(request.IdempotencyKey) < 16 || len(request.IdempotencyKey) > 128 {
		return MemberQuotaAllocation{}, "", ErrConflict
	}
	fingerprintInput := struct {
		MemberUserID       uuid.UUID `json:"member_user_id"`
		PackageID          uuid.UUID `json:"package_id"`
		ComponentID        uuid.UUID `json:"component_id"`
		ComponentType      string    `json:"component_type"`
		AmountMicrocredits int64     `json:"amount_microcredits"`
		MembershipVersion  int64     `json:"membership_version"`
		CreatedBy          uuid.UUID `json:"created_by"`
	}{request.MemberUserID, request.PackageID, request.ComponentID, request.ComponentType, request.AmountMicrocredits, request.MembershipVersion, request.CreatedBy}
	encoded, _ := json.Marshal(fingerprintInput)
	digest := sha256.Sum256(encoded)
	return request, hex.EncodeToString(digest[:]), nil
}

func (r *PostgresRepository) AllocateMemberQuota(ctx context.Context, request MemberQuotaAllocation) (MemberQuotaGrant, bool, error) {
	request, fingerprint, err := normalizeMemberQuotaAllocation(request)
	if err != nil {
		return MemberQuotaGrant{}, false, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MemberQuotaGrant{}, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "member-quota", request.OrganizationID.String()+":"+request.IdempotencyKey); err != nil {
		return MemberQuotaGrant{}, false, err
	}
	existing, existingFingerprint, err := loadMemberQuotaGrantByCommand(ctx, tx, request.OrganizationID, request.IdempotencyKey)
	if err == nil {
		if existingFingerprint != fingerprint {
			return MemberQuotaGrant{}, false, ErrIdempotencyConflict
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MemberQuotaGrant{}, false, err
	}
	var lifecycleStatus string
	var lifecycleVersion int64
	if err := tx.QueryRow(ctx, `SELECT status,membership_version FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, request.OrganizationID, request.MemberUserID).Scan(&lifecycleStatus, &lifecycleVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MemberQuotaGrant{}, false, ErrConflict
		}
		return MemberQuotaGrant{}, false, err
	}
	if lifecycleStatus != "ACTIVE" || lifecycleVersion != request.MembershipVersion {
		return MemberQuotaGrant{}, false, ErrConflict
	}
	var componentType, componentStatus, walletOwnerType, packageStatus string
	var walletOwnerID uuid.UUID
	var available int64
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
SELECT component.component_type,component.available_microcredits,component.status,component.expires_at,
       package.status,wallet.owner_type,wallet.owner_id
FROM entitlement_components component
JOIN entitlement_packages package ON package.id=component.package_id
JOIN wallets wallet ON wallet.id=package.wallet_id
WHERE component.id=$1 AND package.id=$2
FOR UPDATE OF component`, request.ComponentID, request.PackageID).Scan(&componentType, &available, &componentStatus, &expiresAt, &packageStatus, &walletOwnerType, &walletOwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemberQuotaGrant{}, false, ErrNotFound
	}
	if err != nil {
		return MemberQuotaGrant{}, false, err
	}
	now := r.now()
	if walletOwnerType != "ORGANIZATION" || walletOwnerID != request.OrganizationID || componentType != request.ComponentType || componentStatus != "ACTIVE" || packageStatus != "ACTIVE" || !expiresAt.After(now) {
		return MemberQuotaGrant{}, false, ErrConflict
	}
	if available < request.AmountMicrocredits {
		return MemberQuotaGrant{}, false, ErrInsufficientComponentCapacity
	}
	grant := MemberQuotaGrant{ID: uuid.Must(uuid.NewV7()), OrganizationID: request.OrganizationID, MemberUserID: request.MemberUserID, PackageID: request.PackageID, ComponentID: request.ComponentID, ComponentType: request.ComponentType, OriginalMicrocredits: request.AmountMicrocredits, AvailableMicrocredits: request.AmountMicrocredits, Status: "ACTIVE", MembershipVersion: lifecycleVersion, AllocationVersion: 1, CreatedBy: request.CreatedBy, ExpiresAt: expiresAt, CreatedAt: now, UpdatedAt: now}
	command, err := tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,active_grant_microcredits=active_grant_microcredits+$2,updated_at=$3 WHERE id=$1 AND available_microcredits>=$2`, request.ComponentID, request.AmountMicrocredits, now)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrInsufficientComponentCapacity
		}
		return MemberQuotaGrant{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,status,membership_version,allocation_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7,'ACTIVE',$8,1,$9,$10,$11,$12,$13,$13)`, grant.ID, request.OrganizationID, request.MemberUserID, request.PackageID, request.ComponentID, request.ComponentType, request.AmountMicrocredits, lifecycleVersion, request.CreatedBy, request.IdempotencyKey, fingerprint, expiresAt, now)
	if err != nil {
		return MemberQuotaGrant{}, false, err
	}
	if err := appendMemberQuotaAudit(ctx, tx, grant, "ALLOCATED", &request.CreatedBy, request.AmountMicrocredits, now); err != nil {
		return MemberQuotaGrant{}, false, err
	}
	if err := appendMemberQuotaOutbox(ctx, tx, grant, "billing.member-quota.allocated.v1", now); err != nil {
		return MemberQuotaGrant{}, false, err
	}
	return grant, true, tx.Commit(ctx)
}

func (r *PostgresRepository) ListMemberQuotaGrants(ctx context.Context, organizationID uuid.UUID, memberUserID *uuid.UUID, limit int) ([]MemberQuotaGrant, error) {
	if organizationID == uuid.Nil {
		return nil, ErrConflict
	}
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,reserved_microcredits,used_microcredits,expired_microcredits,revoked_microcredits,status,suspension_reason,membership_version,allocation_version,created_by,expires_at,created_at,updated_at FROM member_quota_grants WHERE organization_id=$1 AND ($2::uuid IS NULL OR member_user_id=$2) ORDER BY expires_at,id LIMIT $3`, organizationID, memberUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]MemberQuotaGrant, 0)
	for rows.Next() {
		var item MemberQuotaGrant
		if err := scanMemberQuotaGrant(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RevokeMemberQuota(ctx context.Context, organizationID, grantID, actorUserID uuid.UUID, reason, idempotencyKey string) (MemberQuotaGrant, error) {
	reason, idempotencyKey = strings.TrimSpace(reason), strings.TrimSpace(idempotencyKey)
	if organizationID == uuid.Nil || grantID == uuid.Nil || actorUserID == uuid.Nil || reason == "" || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		return MemberQuotaGrant{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MemberQuotaGrant{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "member-quota-revoke", organizationID.String()+":"+idempotencyKey); err != nil {
		return MemberQuotaGrant{}, err
	}
	fingerprintDigest := sha256.Sum256([]byte("v1|" + grantID.String() + "|" + actorUserID.String() + "|" + reason))
	fingerprint := hex.EncodeToString(fingerprintDigest[:])
	var existingFingerprint string
	var existingSnapshot []byte
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,response_snapshot FROM member_quota_revoke_commands WHERE organization_id=$1 AND idempotency_key=$2 FOR UPDATE`, organizationID, idempotencyKey).Scan(&existingFingerprint, &existingSnapshot)
	if err == nil {
		if existingFingerprint != fingerprint {
			return MemberQuotaGrant{}, ErrIdempotencyConflict
		}
		var existing MemberQuotaGrant
		if err := json.Unmarshal(existingSnapshot, &existing); err != nil {
			return MemberQuotaGrant{}, err
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MemberQuotaGrant{}, err
	}
	grant, _, err := loadMemberQuotaGrant(ctx, tx, grantID, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && grant.OrganizationID != organizationID) {
		return MemberQuotaGrant{}, ErrNotFound
	}
	if err != nil {
		return MemberQuotaGrant{}, err
	}
	if grant.Status == "REVOKED" {
		snapshot, marshalErr := json.Marshal(grant)
		if marshalErr != nil {
			return MemberQuotaGrant{}, marshalErr
		}
		if _, err := tx.Exec(ctx, `INSERT INTO member_quota_revoke_commands(organization_id,idempotency_key,request_fingerprint,grant_id,response_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6)`, organizationID, idempotencyKey, fingerprint, grant.ID, snapshot, r.now()); err != nil {
			return MemberQuotaGrant{}, err
		}
		return grant, tx.Commit(ctx)
	}
	accountClosed := grant.SuspensionReason != nil && *grant.SuspensionReason == "ACCOUNT_CLOSED"
	if accountClosed || grant.Status == "RECLAIMED_ACCOUNT_CLOSED" || grant.ReservedMicrocredits > 0 || grant.AvailableMicrocredits == 0 || (grant.Status != "ACTIVE" && grant.Status != "SUSPENDED") {
		return MemberQuotaGrant{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM entitlement_components WHERE id=$1 FOR UPDATE`, grant.ComponentID); err != nil {
		return MemberQuotaGrant{}, err
	}
	bucket := "active_grant_microcredits"
	if grant.Status == "SUSPENDED" {
		bucket = "suspended_grant_microcredits"
	}
	now := r.now()
	query := `UPDATE entitlement_components SET ` + bucket + `=` + bucket + `-$2,available_microcredits=available_microcredits+$2,updated_at=$3 WHERE id=$1 AND ` + bucket + `>=$2`
	command, err := tx.Exec(ctx, query, grant.ComponentID, grant.AvailableMicrocredits, now)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return MemberQuotaGrant{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE member_quota_grants SET revoked_microcredits=revoked_microcredits+available_microcredits,available_microcredits=0,status='REVOKED',suspension_reason=NULL,allocation_version=allocation_version+1,updated_at=$2 WHERE id=$1`, grant.ID, now)
	if err != nil {
		return MemberQuotaGrant{}, err
	}
	amount := grant.AvailableMicrocredits
	grant.AvailableMicrocredits = 0
	grant.RevokedMicrocredits += amount
	grant.Status, grant.SuspensionReason, grant.AllocationVersion, grant.UpdatedAt = "REVOKED", nil, grant.AllocationVersion+1, now
	if err := appendMemberQuotaAudit(ctx, tx, grant, "REVOKED", &actorUserID, amount, now); err != nil {
		return MemberQuotaGrant{}, err
	}
	if err := appendMemberQuotaOutbox(ctx, tx, grant, "billing.member-quota.revoked.v1", now); err != nil {
		return MemberQuotaGrant{}, err
	}
	snapshot, _ := json.Marshal(grant)
	if _, err := tx.Exec(ctx, `INSERT INTO member_quota_revoke_commands(organization_id,idempotency_key,request_fingerprint,grant_id,response_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6)`, organizationID, idempotencyKey, fingerprint, grant.ID, snapshot, now); err != nil {
		return MemberQuotaGrant{}, err
	}
	return grant, tx.Commit(ctx)
}

func loadMemberQuotaGrantByCommand(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, idempotencyKey string) (MemberQuotaGrant, string, error) {
	var grant MemberQuotaGrant
	var fingerprint string
	err := tx.QueryRow(ctx, `SELECT id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,reserved_microcredits,used_microcredits,expired_microcredits,revoked_microcredits,status,suspension_reason,membership_version,allocation_version,created_by,expires_at,created_at,updated_at,request_fingerprint FROM member_quota_grants WHERE organization_id=$1 AND idempotency_key=$2`, organizationID, idempotencyKey).Scan(&grant.ID, &grant.OrganizationID, &grant.MemberUserID, &grant.PackageID, &grant.ComponentID, &grant.ComponentType, &grant.OriginalMicrocredits, &grant.AvailableMicrocredits, &grant.ReservedMicrocredits, &grant.UsedMicrocredits, &grant.ExpiredMicrocredits, &grant.RevokedMicrocredits, &grant.Status, &grant.SuspensionReason, &grant.MembershipVersion, &grant.AllocationVersion, &grant.CreatedBy, &grant.ExpiresAt, &grant.CreatedAt, &grant.UpdatedAt, &fingerprint)
	return grant, fingerprint, err
}

func loadMemberQuotaGrant(ctx context.Context, tx pgx.Tx, grantID uuid.UUID, lock bool) (MemberQuotaGrant, string, error) {
	query := `SELECT id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,reserved_microcredits,used_microcredits,expired_microcredits,revoked_microcredits,status,suspension_reason,membership_version,allocation_version,created_by,expires_at,created_at,updated_at,request_fingerprint FROM member_quota_grants WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var grant MemberQuotaGrant
	var fingerprint string
	err := tx.QueryRow(ctx, query, grantID).Scan(&grant.ID, &grant.OrganizationID, &grant.MemberUserID, &grant.PackageID, &grant.ComponentID, &grant.ComponentType, &grant.OriginalMicrocredits, &grant.AvailableMicrocredits, &grant.ReservedMicrocredits, &grant.UsedMicrocredits, &grant.ExpiredMicrocredits, &grant.RevokedMicrocredits, &grant.Status, &grant.SuspensionReason, &grant.MembershipVersion, &grant.AllocationVersion, &grant.CreatedBy, &grant.ExpiresAt, &grant.CreatedAt, &grant.UpdatedAt, &fingerprint)
	return grant, fingerprint, err
}

type grantScanner interface{ Scan(...any) error }

func scanMemberQuotaGrant(scanner grantScanner, grant *MemberQuotaGrant) error {
	return scanner.Scan(&grant.ID, &grant.OrganizationID, &grant.MemberUserID, &grant.PackageID, &grant.ComponentID, &grant.ComponentType, &grant.OriginalMicrocredits, &grant.AvailableMicrocredits, &grant.ReservedMicrocredits, &grant.UsedMicrocredits, &grant.ExpiredMicrocredits, &grant.RevokedMicrocredits, &grant.Status, &grant.SuspensionReason, &grant.MembershipVersion, &grant.AllocationVersion, &grant.CreatedBy, &grant.ExpiresAt, &grant.CreatedAt, &grant.UpdatedAt)
}

func appendMemberQuotaAudit(ctx context.Context, tx pgx.Tx, grant MemberQuotaGrant, action string, actor *uuid.UUID, amount int64, now time.Time) error {
	snapshot, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO member_quota_grant_audits(id,grant_id,organization_id,member_user_id,action,actor_user_id,amount_microcredits,snapshot,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), grant.ID, grant.OrganizationID, grant.MemberUserID, action, actor, amount, snapshot, now)
	return err
}

func appendMemberQuotaOutbox(ctx context.Context, tx pgx.Tx, grant MemberQuotaGrant, eventType string, now time.Time) error {
	payload, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.Must(uuid.NewV7()), eventType, grant.ID, grant.AllocationVersion, payload, "member-quota:"+grant.ID.String(), now)
	return err
}

func expireMemberQuotaGrantsForComponent(ctx context.Context, tx pgx.Tx, componentID uuid.UUID, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id FROM member_quota_grants WHERE component_id=$1 AND status IN ('ACTIVE','SUSPENDED') ORDER BY id FOR UPDATE`, componentID)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		grant, _, err := loadMemberQuotaGrant(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if grant.ReservedMicrocredits != 0 {
			return ErrConflict
		}
		amount := grant.AvailableMicrocredits
		if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET available_microcredits=0,expired_microcredits=expired_microcredits+available_microcredits,status='EXPIRED',suspension_reason=NULL,allocation_version=allocation_version+1,updated_at=$2 WHERE id=$1`, grant.ID, now); err != nil {
			return err
		}
		grant.AvailableMicrocredits = 0
		grant.ExpiredMicrocredits += amount
		grant.Status, grant.SuspensionReason, grant.AllocationVersion, grant.UpdatedAt = "EXPIRED", nil, grant.AllocationVersion+1, now
		if err := appendMemberQuotaAudit(ctx, tx, grant, "EXPIRED", nil, amount, now); err != nil {
			return err
		}
		if err := appendMemberQuotaOutbox(ctx, tx, grant, "billing.member-quota.expired.v1", now); err != nil {
			return err
		}
	}
	return nil
}
