package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func usageWaiverCommandFacts(holdID uuid.UUID, reason, actor, key string) (string, error) {
	if holdID == uuid.Nil || strings.TrimSpace(reason) == "" || !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > 500 || strings.TrimSpace(actor) == "" || !validIdempotencyKey(key, 120) {
		return "", ErrConflict
	}
	facts, err := json.Marshal([]string{holdID.String(), reason, actor})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(facts)
	return hex.EncodeToString(digest[:]), nil
}

// PrepareAdminUsageWaiver binds one logical command before the Token authority
// is called. A lost reply cannot reuse the key for a different hold or decision.
func (r *PostgresRepository) PrepareAdminUsageWaiver(ctx context.Context, holdID uuid.UUID, reason, actor, key string) (AdminHold, error) {
	fingerprint, err := usageWaiverCommandFacts(holdID, reason, actor, key)
	if err != nil {
		return AdminHold{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return AdminHold{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "usage-hold.admin.safe-release", key); err != nil {
		return AdminHold{}, err
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint FROM usage_hold_admin_commands WHERE idempotency_key=$1`, key).Scan(&previous)
	if err == nil && previous != fingerprint {
		return AdminHold{}, ErrIdempotencyConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, err
	}
	hold, err := adminHoldByID(ctx, tx, holdID)
	if err != nil {
		return AdminHold{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_admin_commands(idempotency_key,hold_id,request_fingerprint,reason,actor,before_status,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(idempotency_key) DO NOTHING`, key, holdID, fingerprint, reason, actor, hold.Status, r.now()); err != nil {
		return AdminHold{}, err
	}
	return hold, tx.Commit(ctx)
}

// RecordAdminUsageWaiver only records the existing financial winner. Neither
// REVIEW nor an expired lease is permission to release a reservation here.
func (r *PostgresRepository) RecordAdminUsageWaiver(ctx context.Context, holdID uuid.UUID, reason, actor, key string) (AdminHold, error) {
	fingerprint, err := usageWaiverCommandFacts(holdID, reason, actor, key)
	if err != nil {
		return AdminHold{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return AdminHold{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "usage-hold.admin.safe-release", key); err != nil {
		return AdminHold{}, err
	}
	var stored, beforeStatus string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,before_status FROM usage_hold_admin_commands WHERE idempotency_key=$1`, key).Scan(&stored, &beforeStatus)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && stored != fingerprint {
		return AdminHold{}, ErrIdempotencyConflict
	}
	if err != nil {
		return AdminHold{}, err
	}
	hold, err := adminHoldByID(ctx, tx, holdID)
	if err != nil {
		return AdminHold{}, err
	}
	if _, err := getWallet(ctx, tx, hold.WalletID, true); err != nil {
		return AdminHold{}, err
	}
	if _, err := getHoldByUsage(ctx, tx, hold.UsageRequestID, true); err != nil {
		return AdminHold{}, err
	}
	hold, err = adminHoldByID(ctx, tx, holdID)
	if err != nil {
		return AdminHold{}, err
	}
	var proof struct {
		Waiver bool   `json:"admin_waiver"`
		Actor  string `json:"admin_actor"`
		Reason string `json:"admin_release_reason"`
	}
	if hold.Status != "RELEASED" || hold.ActualMicrocredits == nil || *hold.ActualMicrocredits != 0 || json.Unmarshal(hold.UsageSnapshot, &proof) != nil || !proof.Waiver || proof.Actor != actor || proof.Reason != reason {
		return AdminHold{}, ErrUsageAlreadyTerminal
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint FROM usage_hold_admin_actions WHERE action='SAFE_RELEASE' AND idempotency_key=$1`, key).Scan(&existing)
	if err == nil {
		if existing != fingerprint {
			return AdminHold{}, ErrIdempotencyConflict
		}
		return hold, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, err
	}
	var snapshot json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
'component_allocations',COALESCE((SELECT jsonb_agg(jsonb_build_object('package_id',package_id,'component_id',component_id,'reserved_microcredits',reserved_microcredits::text,'settled_microcredits',settled_microcredits::text,'released_microcredits',released_microcredits::text) ORDER BY component_id) FROM usage_hold_allocations WHERE hold_id=$1),'[]'::jsonb),
'member_allocations',COALESCE((SELECT jsonb_agg(jsonb_build_object('grant_id',grant_id,'component_id',component_id,'reserved_microcredits',reserved_microcredits::text,'settled_microcredits',settled_microcredits::text,'released_microcredits',released_microcredits::text) ORDER BY grant_id) FROM member_quota_hold_allocations WHERE hold_id=$1),'[]'::jsonb))`, holdID).Scan(&snapshot); err != nil {
		return AdminHold{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_hold_admin_actions(id,hold_id,action,idempotency_key,request_fingerprint,reason,actor,before_status,after_status,allocation_snapshot,created_at) VALUES($1,$2,'SAFE_RELEASE',$3,$4,$5,$6,$7,'RELEASED',$8,$9)`, uuid.Must(uuid.NewV7()), holdID, key, fingerprint, reason, actor, beforeStatus, snapshot, r.now())
	if err != nil {
		return AdminHold{}, err
	}
	return hold, tx.Commit(ctx)
}
