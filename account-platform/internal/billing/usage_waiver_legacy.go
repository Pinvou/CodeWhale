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

var ErrLegacyHoldWaiverPending = errors.New("legacy hold waiver is unresolved")

func legacyWaiverHoldPendingTx(ctx context.Context, tx pgx.Tx, holdID uuid.UUID) (bool, error) {
	var table *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('usage_hold_admin_commands')::text`).Scan(&table); err != nil {
		return false, err
	}
	if table == nil {
		return false, nil
	}
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_hold_admin_commands command WHERE command.hold_id=$1 AND NOT EXISTS(SELECT 1 FROM usage_hold_admin_actions action WHERE action.action='SAFE_RELEASE' AND action.idempotency_key=command.idempotency_key AND action.hold_id=command.hold_id AND action.request_fingerprint=command.request_fingerprint))`, holdID).Scan(&pending)
	return pending, err
}

// LegacyUsageWaiverRecord is a read of the original identity. It deliberately
// has no modern command_reference and grants no authority to resume payment.
type LegacyUsageWaiverRecord struct {
	Legacy             bool            `json:"legacy"`
	Completed          bool            `json:"completed"`
	HoldID             uuid.UUID       `json:"hold_id"`
	RequestFingerprint string          `json:"request_fingerprint"`
	Reason             string          `json:"reason"`
	Actor              string          `json:"actor"`
	BeforeStatus       string          `json:"before_status"`
	ActionID           *uuid.UUID      `json:"action_id,omitempty"`
	AfterStatus        *string         `json:"after_status,omitempty"`
	AllocationSnapshot json.RawMessage `json:"allocation_snapshot,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
}

func (r *PostgresRepository) LegacyUsageWaiver(ctx context.Context, holdID uuid.UUID, reason, key string, actor UsageWaiverActor) (LegacyUsageWaiverRecord, error) {
	var record LegacyUsageWaiverRecord
	facts, _ := json.Marshal([]string{holdID.String(), reason, actor.Issuer + "|" + actor.Subject})
	fixedExpected := waiverHash(facts)
	originalReason, originalActor := strings.TrimSpace(reason), strings.TrimSpace(actor.Issuer+"|"+actor.Subject)
	originalExpected := waiverHash([]byte(holdID.String() + "\x00" + originalReason + "\x00" + originalActor))
	matchedKey := key
	err := r.pool.QueryRow(ctx, `SELECT hold_id,request_fingerprint,reason,actor,before_status,id,after_status,allocation_snapshot,created_at FROM usage_hold_admin_actions WHERE action='SAFE_RELEASE' AND idempotency_key=$1`, key).Scan(&record.HoldID, &record.RequestFingerprint, &record.Reason, &record.Actor, &record.BeforeStatus, &record.ActionID, &record.AfterStatus, &record.AllocationSnapshot, &record.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) && key != strings.TrimSpace(key) {
		matchedKey = strings.TrimSpace(key)
		err = r.pool.QueryRow(ctx, `SELECT hold_id,request_fingerprint,reason,actor,before_status,id,after_status,allocation_snapshot,created_at FROM usage_hold_admin_actions WHERE action='SAFE_RELEASE' AND idempotency_key=$1`, strings.TrimSpace(key)).Scan(&record.HoldID, &record.RequestFingerprint, &record.Reason, &record.Actor, &record.BeforeStatus, &record.ActionID, &record.AfterStatus, &record.AllocationSnapshot, &record.CreatedAt)
	}
	if err == nil {
		record.Completed = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return record, err
	} else {
		var table *string
		if err := r.pool.QueryRow(ctx, `SELECT to_regclass('usage_hold_admin_commands')::text`).Scan(&table); err != nil {
			return record, err
		}
		if table == nil {
			return record, ErrNotFound
		}
		if err := r.pool.QueryRow(ctx, `SELECT hold_id,request_fingerprint,reason,actor,before_status,created_at FROM usage_hold_admin_commands WHERE idempotency_key=$1`, key).Scan(&record.HoldID, &record.RequestFingerprint, &record.Reason, &record.Actor, &record.BeforeStatus, &record.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
			return record, ErrNotFound
		} else if err != nil {
			return record, err
		}
	}
	// O completed actions used trimmed NUL-delimited facts; F prepared and
	// completed records used the unchanged JSON facts. Recognize each published
	// writer without changing the stored fingerprint or creating a modern command.
	fixedMatch := matchedKey == key && validIdempotencyKey(key, 120) && record.RequestFingerprint == fixedExpected && record.Reason == reason && record.Actor == actor.Issuer+"|"+actor.Subject
	originalMatch := record.Completed && validOriginalWaiverKey(key) && originalReason != "" && len(originalReason) <= 500 && originalActor != "" && record.RequestFingerprint == originalExpected && record.Reason == originalReason && record.Actor == originalActor
	if record.HoldID != holdID || (!fixedMatch && !originalMatch) {
		return LegacyUsageWaiverRecord{}, ErrConflict
	}
	if record.Completed && (record.AfterStatus == nil || *record.AfterStatus != "RELEASED") {
		return LegacyUsageWaiverRecord{}, ErrConflict
	}
	record.Legacy = true
	return record, nil
}
