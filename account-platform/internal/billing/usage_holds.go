package billing

import (
	"context"
	"encoding/json"
	"errors"

	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) Commit(ctx context.Context, usageRequestID, holdID uuid.UUID, actual int64, usage map[string]any) (Hold, error) {
	if actual < 0 {
		return Hold{}, ErrConflict
	}
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		return Hold{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Hold{}, err
	}
	defer tx.Rollback(ctx)
	hold, err := getHoldByUsage(ctx, tx, usageRequestID, false)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hold.ID != holdID) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, err
	}
	if _, err := getWallet(ctx, tx, hold.WalletID, true); err != nil {
		return Hold{}, err
	}
	hold, err = getHoldByUsage(ctx, tx, usageRequestID, true)
	if err != nil || hold.ID != holdID {
		return Hold{}, ErrNotFound
	}
	if hold.Status == "COMMITTED" && hold.ActualMicrocredits != nil && *hold.ActualMicrocredits == actual {
		return hold, tx.Commit(ctx)
	}
	if hold.Status == "RELEASED" && actual > 0 {
		var releaseReason string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(usage_snapshot->>'release_reason','') FROM usage_holds WHERE id=$1`, hold.ID).Scan(&releaseReason); err != nil {
			return Hold{}, err
		}
		if releaseReason == "ENTITLEMENT_EXPIRED_UNCERTAIN" {
			now := r.now()
			_, err := tx.Exec(ctx, `INSERT INTO usage_loss_reviews(id,usage_request_id,hold_id,wallet_id,reason_code,claimed_actual_microcredits,usage_snapshot,status,created_at,updated_at) VALUES($1,$2,$3,$4,'ENTITLEMENT_EXPIRED_UNCERTAIN',$5,$6,'REVIEW_REQUIRED',$7,$7) ON CONFLICT(hold_id) DO UPDATE SET claimed_actual_microcredits=EXCLUDED.claimed_actual_microcredits,usage_snapshot=EXCLUDED.usage_snapshot,updated_at=EXCLUDED.updated_at`, uuid.Must(uuid.NewV7()), usageRequestID, hold.ID, hold.WalletID, actual, usageJSON, now)
			if err != nil {
				return Hold{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return Hold{}, err
			}
			return hold, ErrUsagePlatformLossReview
		}
	}
	if hold.Status != "ACTIVE" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	backedActual := min(actual, hold.MaxMicrocredits)
	overage := actual - backedActual
	entryID := uuid.Nil
	var walletAccount uuid.UUID
	if actual > 0 {
		entryID = uuid.Must(uuid.NewV7())
		if err := insertJournal(ctx, tx, entryID, "USAGE", "usage:"+usageRequestID.String(), "settle model usage", now); err != nil {
			return Hold{}, err
		}
		walletAccount, err = walletLedgerAccount(ctx, tx, hold.WalletID)
		if err != nil {
			return Hold{}, err
		}
	}
	type allocation struct {
		packageID   uuid.UUID
		componentID uuid.UUID
		lotID       uuid.UUID
		reserved    int64
	}
	rows, err := tx.Query(ctx, `SELECT allocation.package_id,allocation.component_id,component.credit_lot_id,allocation.reserved_microcredits FROM usage_hold_allocations allocation JOIN entitlement_components component ON component.id=allocation.component_id WHERE allocation.hold_id=$1 ORDER BY component.expires_at,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF allocation,component`, hold.ID)
	if err != nil {
		return Hold{}, err
	}
	allocations := make([]allocation, 0)
	for rows.Next() {
		var item allocation
		if err := rows.Scan(&item.packageID, &item.componentID, &item.lotID, &item.reserved); err != nil {
			rows.Close()
			return Hold{}, err
		}
		allocations = append(allocations, item)
	}
	rows.Close()
	remaining := backedActual
	for _, item := range allocations {
		settled := min(remaining, item.reserved)
		released := item.reserved - settled
		if settled > 0 {
			command, err := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, item.lotID, settled)
			if err != nil {
				return Hold{}, err
			}
			if command.RowsAffected() != 1 {
				return Hold{}, ErrConflict
			}
			if err := insertPosting(ctx, tx, entryID, walletAccount, -settled, &item.lotID, now); err != nil {
				return Hold{}, err
			}
		}
		memberBacked, memberErr := hasMemberQuotaAllocations(ctx, tx, hold.ID, item.componentID)
		if memberErr != nil {
			return Hold{}, memberErr
		}
		if memberBacked {
			if err := settleMemberQuotaComponent(ctx, tx, hold.ID, item.componentID, settled, now); err != nil {
				return Hold{}, err
			}
		} else {
			command, err := tx.Exec(ctx, `UPDATE entitlement_components SET reserved_microcredits=reserved_microcredits-$2,used_microcredits=used_microcredits+$3,available_microcredits=available_microcredits+$4,status=CASE WHEN status IN ('FROZEN','REVOKED','EXPIRED') THEN status WHEN reserved_microcredits-$2=0 AND available_microcredits+$4+active_grant_microcredits+suspended_grant_microcredits=0 THEN 'CONSUMED' ELSE 'ACTIVE' END,updated_at=$5 WHERE id=$1 AND reserved_microcredits>=$2`, item.componentID, item.reserved, settled, released, now)
			if err != nil {
				return Hold{}, err
			}
			if command.RowsAffected() != 1 {
				return Hold{}, ErrConflict
			}
		}
		command, err := tx.Exec(ctx, `UPDATE usage_hold_allocations SET settled_microcredits=$2,released_microcredits=$3,updated_at=$4 WHERE hold_id=$1 AND component_id=$5`, hold.ID, settled, released, now, item.componentID)
		if err != nil {
			return Hold{}, err
		}
		if command.RowsAffected() != 1 {
			return Hold{}, ErrConflict
		}
		remaining -= settled
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages package SET status=CASE WHEN package.status IN ('FROZEN','REVOKED','EXPIRED') THEN package.status WHEN EXISTS(SELECT 1 FROM entitlement_components component WHERE component.package_id=package.id AND (component.available_microcredits>0 OR component.reserved_microcredits>0 OR component.active_grant_microcredits>0 OR component.suspended_grant_microcredits>0)) THEN 'ACTIVE' ELSE 'CONSUMED' END,updated_at=$2 WHERE package.id IN (SELECT package_id FROM usage_hold_allocations WHERE hold_id=$1)`, hold.ID, now); err != nil {
		return Hold{}, err
	}
	if remaining != 0 {
		return Hold{}, errors.New("usage hold is not backed by entitlement packages")
	}
	if overage > 0 {
		if err := insertPosting(ctx, tx, entryID, receivableAccount, -overage, nil, now); err != nil {
			return Hold{}, err
		}
		if _, err := createReceivableWithinTransaction(ctx, tx, hold.WalletID, "USAGE_OVERAGE", usageRequestID.String(), overage, nil, nil, nil, &usageRequestID, now); err != nil {
			return Hold{}, err
		}
	}
	if actual > 0 {
		if err := insertPosting(ctx, tx, entryID, usageRevenueAccount, actual, nil, now); err != nil {
			return Hold{}, err
		}
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits = posted_microcredits - $2, reserved_microcredits = reserved_microcredits - $3, version = version + 1, updated_at = $4 WHERE id = $1 AND reserved_microcredits >= $3 AND posted_microcredits >= $2`, hold.WalletID, backedActual, hold.MaxMicrocredits, now)
	if err == nil && command.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE usage_holds SET status = 'COMMITTED', actual_microcredits = $2, usage_snapshot = $3, terminal_at = $4, updated_at = $4 WHERE id = $1`, hold.ID, actual, usageJSON, now)
	}
	if err != nil {
		return Hold{}, err
	}
	hold.Status = "COMMITTED"
	hold.ActualMicrocredits = &actual
	return hold, tx.Commit(ctx)
}

func (r *PostgresRepository) ExtendHold(ctx context.Context, usageRequestID, holdID uuid.UUID, expiresAt time.Time, additional int64) (Hold, error) {
	if usageRequestID == uuid.Nil || holdID == uuid.Nil || additional < 0 || !expiresAt.After(r.now()) {
		return Hold{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Hold{}, err
	}
	defer tx.Rollback(ctx)
	hold, err := getHoldByUsage(ctx, tx, usageRequestID, false)
	if err == nil {
		// Match Commit/Release: acquire the wallet before the mutable hold.
		if _, err = getWallet(ctx, tx, hold.WalletID, true); err == nil {
			hold, err = getHoldByUsage(ctx, tx, usageRequestID, true)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hold.ID != holdID) {
		return Hold{}, ErrNotFound
	}
	if err != nil {
		return Hold{}, err
	}
	if hold.Status != "ACTIVE" {
		return Hold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	if !expiresAt.After(now) {
		return Hold{}, ErrConflict
	}
	previousExpiresAt, previousMaximum := hold.ExpiresAt, hold.MaxMicrocredits
	if additional > 0 {
		var organizationID, memberUserID *uuid.UUID
		var membershipVersion *int64
		if err := tx.QueryRow(ctx, `SELECT organization_id,member_user_id,membership_version FROM usage_holds WHERE id=$1`, hold.ID).Scan(&organizationID, &memberUserID, &membershipVersion); err != nil {
			return Hold{}, err
		}
		if organizationID != nil || memberUserID != nil || membershipVersion != nil {
			if organizationID == nil || memberUserID == nil || membershipVersion == nil {
				return Hold{}, ErrConflict
			}
			if err := r.extendMemberQuotaHold(ctx, tx, hold, *organizationID, *memberUserID, *membershipVersion, additional, now); err != nil {
				return Hold{}, err
			}
		} else if err := extendPersonalHold(ctx, tx, hold, additional, now); err != nil {
			return Hold{}, err
		}
		hold.MaxMicrocredits += additional
	}
	if expiresAt.After(hold.ExpiresAt) {
		hold.ExpiresAt = expiresAt.UTC()
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_holds SET expires_at=$2,max_microcredits=$3,updated_at=$4 WHERE id=$1 AND status='ACTIVE'`, hold.ID, hold.ExpiresAt, hold.MaxMicrocredits, now); err != nil {
		return Hold{}, err
	}
	if hold.ExpiresAt.After(previousExpiresAt) || hold.MaxMicrocredits != previousMaximum {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_lease_events(id,hold_id,event_type,previous_expires_at,expires_at,previous_max_microcredits,max_microcredits,additional_microcredits,created_at) VALUES($1,$2,'EXTENDED',$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), hold.ID, previousExpiresAt, hold.ExpiresAt, previousMaximum, hold.MaxMicrocredits, hold.MaxMicrocredits-previousMaximum, now); err != nil {
			return Hold{}, err
		}
	}
	return hold, tx.Commit(ctx)
}

func extendPersonalHold(ctx context.Context, tx pgx.Tx, hold Hold, additional int64, now time.Time) error {
	wallet, err := getWallet(ctx, tx, hold.WalletID, true)
	if err != nil {
		return err
	}
	if wallet.Status != "ACTIVE" || wallet.AvailableMicrocredits < additional {
		return ErrInsufficientBalance
	}
	type allocation struct {
		packageID, componentID uuid.UUID
		amount                 int64
	}
	rows, err := tx.Query(ctx, `SELECT component.package_id,component.id,component.available_microcredits FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$1 AND package.status='ACTIVE' AND package.valid_from<=$2 AND component.expires_at>$2 AND component.status='ACTIVE' AND component.available_microcredits>0 ORDER BY component.expires_at,CASE component.component_type WHEN 'BONUS' THEN 0 ELSE 1 END,component.credited_at,component.id FOR UPDATE OF component`, hold.WalletID, now)
	if err != nil {
		return err
	}
	remaining := additional
	items := make([]allocation, 0)
	for rows.Next() {
		var item allocation
		var available int64
		if err := rows.Scan(&item.packageID, &item.componentID, &available); err != nil {
			rows.Close()
			return err
		}
		item.amount = min(remaining, available)
		if item.amount > 0 {
			items = append(items, item)
			remaining -= item.amount
		}
		if remaining == 0 {
			break
		}
	}
	rows.Close()
	if remaining != 0 {
		return ErrInsufficientBalance
	}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1 AND status='ACTIVE' AND receivable_microcredits=0 AND posted_microcredits-reserved_microcredits>=$2`, hold.WalletID, additional, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInsufficientBalance
	}
	for _, item := range items {
		command, err = tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,reserved_microcredits=reserved_microcredits+$2,updated_at=$3 WHERE id=$1 AND available_microcredits>=$2`, item.componentID, item.amount, now)
		if err != nil || command.RowsAffected() != 1 {
			if err != nil {
				return err
			}
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(hold_id,component_id) DO UPDATE SET reserved_microcredits=usage_hold_allocations.reserved_microcredits+EXCLUDED.reserved_microcredits,updated_at=EXCLUDED.updated_at`, hold.ID, item.packageID, item.componentID, item.amount, now)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresRepository) ExpireHolds(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT usage_request_id, id FROM usage_holds WHERE status = 'ACTIVE' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, r.now(), limit)
	if err != nil {
		return 0, err
	}
	type candidate struct{ usageID, holdID uuid.UUID }
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.usageID, &item.holdID); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	count := 0
	for _, candidate := range candidates {
		if hold, err := r.releaseHold(ctx, candidate.usageID, candidate.holdID, "hold lease expired", true); err == nil {
			if hold.Status == "RELEASED" {
				count++
			}
		} else if !errors.Is(err, ErrUsageAlreadyTerminal) {
			return count, err
		}
	}
	return count, nil
}

func hasMemberQuotaAllocations(ctx context.Context, tx pgx.Tx, holdID, componentID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM member_quota_hold_allocations WHERE hold_id=$1 AND component_id=$2)`, holdID, componentID).Scan(&exists)
	return exists, err
}
