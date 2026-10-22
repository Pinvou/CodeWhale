package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrLegacyWaiverRequiresReview = errors.New("legacy waiver requires review")

type UsageWaiverActor struct {
	ID                   uuid.UUID
	Issuer, Subject      string
	AuthorizationVersion int64
}

func waiverHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func waiverHumanFingerprint(hold uuid.UUID, reason string, actor UsageWaiverActor) string {
	raw, _ := json.Marshal([]string{hold.String(), reason, actor.ID.String(), actor.Issuer, actor.Subject})
	return waiverHash(raw)
}

// legacyWaiverKeyTx reads the original namespace first. It never fabricates a
// modern reference for old commands whose financial result is still unknown.
func legacyWaiverKeyTx(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_hold_admin_actions WHERE action='SAFE_RELEASE' AND idempotency_key=$1)`, key).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}
	var table *string
	if err := tx.QueryRow(ctx, `SELECT to_regclass('usage_hold_admin_commands')::text`).Scan(&table); err != nil {
		return false, err
	}
	if table == nil {
		return false, nil
	}
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_hold_admin_commands WHERE idempotency_key=$1)`, key).Scan(&exists)
	return exists, err
}

// waiverAllocationSnapshot binds the source identities and reservation amounts,
// not the lifecycle of a grant: release must restore the current suspended
// bucket without reactivating its owner or membership.
func waiverAllocationSnapshot(ctx context.Context, tx pgx.Tx, holdID uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
 'components',COALESCE((SELECT jsonb_agg(jsonb_build_object('package_id',package_id,'component_id',component_id,'reserved',reserved_microcredits,'settled',settled_microcredits,'released',released_microcredits) ORDER BY component_id) FROM usage_hold_allocations WHERE hold_id=$1),'[]'::jsonb),
 'grants',COALESCE((SELECT jsonb_agg(jsonb_build_object('grant_id',grant_id,'component_id',component_id,'reserved',reserved_microcredits,'settled',settled_microcredits,'released',released_microcredits) ORDER BY grant_id) FROM member_quota_hold_allocations WHERE hold_id=$1),'[]'::jsonb))::text`, holdID).Scan(&raw)
	return raw, err
}

func validateWaiverAllocations(ctx context.Context, tx pgx.Tx, hold Hold) error {
	if _, err := validateMemberQuotaHoldAllocations(ctx, tx, hold.ID, 0); err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(allocation.reserved_microcredits),0)=$2::bigint AND COALESCE(bool_and(
 allocation.package_id=component.package_id AND package.wallet_id=$3 AND lot.wallet_id=$3
 AND allocation.reserved_microcredits>0 AND allocation.settled_microcredits=0 AND allocation.released_microcredits=0),true)
 FROM usage_hold_allocations allocation JOIN entitlement_components component ON component.id=allocation.component_id
 JOIN entitlement_packages package ON package.id=allocation.package_id JOIN credit_lots lot ON lot.id=component.credit_lot_id
 WHERE allocation.hold_id=$1`, hold.ID, hold.MaxMicrocredits, hold.WalletID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}

func readWaiverCommand(ctx context.Context, q queryable, reference uuid.UUID) (usagewaiver.Command, error) {
	var raw []byte
	var c usagewaiver.Command
	err := q.QueryRow(ctx, `SELECT command_snapshot FROM usage_waiver_commands WHERE command_reference=$1`, reference).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if json.Unmarshal(raw, &c) != nil || !c.Valid() {
		return c, ErrConflict
	}
	return c, nil
}

func readWaiverResult(ctx context.Context, q queryable, c usagewaiver.Command) (usagewaiver.Result, error) {
	var raw []byte
	var v usagewaiver.Result
	err := q.QueryRow(ctx, `SELECT result_snapshot FROM usage_waiver_results WHERE command_reference=$1`, c.Reference).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if json.Unmarshal(raw, &v) != nil || !v.Matches(c) {
		return v, ErrConflict
	}
	return v, nil
}

func (r *PostgresRepository) PrepareUsageWaiver(ctx context.Context, holdID uuid.UUID, reason, key string, actor UsageWaiverActor) (usagewaiver.Command, error) {
	var zero usagewaiver.Command
	if holdID == uuid.Nil || !validIdempotencyKey(key, 120) || !utf8.ValidString(reason) || strings.TrimSpace(reason) == "" || utf8.RuneCountInString(reason) > 500 || actor.ID == uuid.Nil || actor.AuthorizationVersion <= 0 || strings.TrimSpace(actor.Issuer) == "" || strings.TrimSpace(actor.Subject) == "" {
		return zero, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	keyDigest := waiverHash([]byte(key))
	fingerprint := waiverHumanFingerprint(holdID, reason, actor)
	if err := lockTransactionKey(ctx, tx, "billing.usage-waiver", keyDigest); err != nil {
		return zero, err
	}
	var reference uuid.UUID
	var previous string
	err = tx.QueryRow(ctx, `SELECT command_reference,request_fingerprint FROM usage_waiver_commands WHERE idempotency_key_digest=$1`, keyDigest).Scan(&reference, &previous)
	if err == nil {
		if previous != fingerprint {
			return zero, ErrConflict
		}
		c, err := readWaiverCommand(ctx, tx, reference)
		if err != nil {
			return zero, err
		}
		return c, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, err
	}
	legacy, err := legacyWaiverKeyTx(ctx, tx, key)
	if err != nil {
		return zero, err
	}
	if legacy {
		return zero, ErrLegacyWaiverRequiresReview
	}
	var usageID, walletID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT usage_request_id,wallet_id FROM usage_holds WHERE id=$1`, holdID).Scan(&usageID, &walletID); errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrNotFound
	} else if err != nil {
		return zero, err
	}
	if _, err := getWallet(ctx, tx, walletID, true); err != nil {
		return zero, err
	}
	h, err := getHoldByUsage(ctx, tx, usageID, true)
	if err != nil {
		return zero, err
	}
	if h.ID != holdID || (h.Status != "ACTIVE" && h.Status != "REVIEW") {
		return zero, ErrUsageAlreadyTerminal
	}
	if pending, err := legacyWaiverHoldPendingTx(ctx, tx, h.ID); err != nil {
		return zero, err
	} else if pending {
		return zero, ErrLegacyHoldWaiverPending
	}
	var other bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_waiver_hold_claims WHERE hold_id=$1)`, h.ID).Scan(&other); err != nil {
		return zero, err
	}
	if other {
		return zero, ErrConflict
	}
	if err := validateWaiverAllocations(ctx, tx, h); err != nil {
		return zero, err
	}
	allocations, err := waiverAllocationSnapshot(ctx, tx, h.ID)
	if err != nil {
		return zero, err
	}
	c := usagewaiver.Command{Version: usagewaiver.Version, Reference: uuid.Must(uuid.NewV7()), HoldID: h.ID, UsageID: h.UsageRequestID, WalletID: h.WalletID, Maximum: h.MaxMicrocredits, AllocationDigest: waiverHash(allocations), ActorID: actor.ID, Issuer: actor.Issuer, Subject: actor.Subject, AuthorizationVersion: actor.AuthorizationVersion, Reason: reason, CreatedAt: r.now().UTC().Truncate(time.Microsecond)}
	c.Digest = c.Fingerprint()
	raw, err := json.Marshal(c)
	if err != nil || !c.Valid() {
		return zero, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO usage_waiver_commands(command_reference,hold_id,usage_request_id,wallet_id,maximum_microcredits,actor_user_id,actor_issuer,actor_subject,authorization_version,idempotency_key_digest,request_fingerprint,command_snapshot,before_status,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, c.Reference, c.HoldID, c.UsageID, c.WalletID, c.Maximum, c.ActorID, c.Issuer, c.Subject, c.AuthorizationVersion, keyDigest, fingerprint, raw, h.Status, c.CreatedAt)
	if err != nil {
		return zero, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_waiver_hold_claims(hold_id,command_reference,created_at) VALUES($1,$2,$3)`, c.HoldID, c.Reference, c.CreatedAt); err != nil {
		return zero, err
	}
	return c, tx.Commit(ctx)
}

// ExecuteUsageWaiver is the financial owner. The original wallet -> hold lock
// order is shared with Commit; there can be only one actual money winner.
func (r *PostgresRepository) ExecuteUsageWaiver(ctx context.Context, request usagewaiver.ExecuteRequest) (usagewaiver.Result, error) {
	var zero usagewaiver.Result
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	c, err := readWaiverCommand(ctx, tx, request.Reference)
	if err != nil {
		return zero, err
	}
	if c.Digest != request.Digest {
		return zero, ErrConflict
	}
	if err := lockTransactionKey(ctx, tx, "billing.usage-waiver-result", c.Reference.String()); err != nil {
		return zero, err
	}
	if result, err := readWaiverResult(ctx, tx, c); err == nil {
		return result, tx.Commit(ctx)
	} else if !errors.Is(err, ErrNotFound) {
		return zero, err
	}
	if _, err := getWallet(ctx, tx, c.WalletID, true); err != nil {
		return zero, err
	}
	h, err := getHoldByUsage(ctx, tx, c.UsageID, true)
	if err != nil {
		return zero, err
	}
	if h.ID != c.HoldID || h.WalletID != c.WalletID || h.MaxMicrocredits != c.Maximum {
		return zero, ErrConflict
	}
	var claim uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT command_reference FROM usage_waiver_hold_claims WHERE hold_id=$1 FOR UPDATE`, c.HoldID).Scan(&claim); errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrConflict
	} else if err != nil {
		return zero, err
	}
	if claim != c.Reference {
		return zero, ErrConflict
	}
	before, err := waiverAllocationSnapshot(ctx, tx, h.ID)
	if err != nil {
		return zero, err
	}
	now := r.now().UTC().Truncate(time.Microsecond)
	result := usagewaiver.Result{CommandReference: c.Reference, RequestDigest: c.Digest, HoldID: c.HoldID, UsageID: c.UsageID, WalletID: c.WalletID, Maximum: c.Maximum, HoldStatus: h.Status, Actual: h.ActualMicrocredits, CompletedAt: now}
	switch h.Status {
	case "COMMITTED":
		result.Outcome = usagewaiver.Committed
	case "RELEASED":
		result.Outcome = usagewaiver.ReleasedOther
	case "ACTIVE", "REVIEW":
		if waiverHash(before) != c.AllocationDigest {
			return zero, ErrConflict
		}
		if err := validateWaiverAllocations(ctx, tx, h); err != nil {
			return zero, err
		}
		if h.MaxMicrocredits == 0 {
			if _, err := validateMemberQuotaHoldAllocations(ctx, tx, h.ID, 0); err != nil {
				return zero, err
			}
			var allocationCount int
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_hold_allocations WHERE hold_id=$1)+(SELECT count(*) FROM member_quota_hold_allocations WHERE hold_id=$1)`, h.ID).Scan(&allocationCount); err != nil {
				return zero, err
			}
			if allocationCount != 0 {
				return zero, ErrConflict
			}
		} else if err := releasePackageAllocations(ctx, tx, h.ID, now); err != nil {
			return zero, err
		}
		command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND reserved_microcredits>=$2`, h.WalletID, h.MaxMicrocredits, now)
		if err != nil {
			return zero, err
		}
		if command.RowsAffected() != 1 {
			return zero, ErrConflict
		}
		proof, _ := json.Marshal(map[string]any{"admin_waiver": true, "waiver_command_reference": c.Reference, "waiver_request_digest": c.Digest, "actor_user_id": c.ActorID, "actor_issuer": c.Issuer, "actor_subject": c.Subject, "reason": c.Reason, "release_reason": "admin_waiver"})
		if _, err := tx.Exec(ctx, `UPDATE usage_holds SET status='RELEASED',actual_microcredits=0,usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb)||$2::jsonb,terminal_at=$3,updated_at=$3 WHERE id=$1`, h.ID, proof, now); err != nil {
			return zero, err
		}
		zeroAmount := int64(0)
		result.Outcome = usagewaiver.Waived
		result.HoldStatus = "RELEASED"
		result.Actual = &zeroAmount
	default:
		return zero, ErrUsageAlreadyTerminal
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(usage_snapshot,'{}'::jsonb) FROM usage_holds WHERE id=$1`, h.ID).Scan(&result.UsageSnapshot); err != nil {
		return zero, err
	}
	if !result.Matches(c) {
		return zero, ErrConflict
	}
	after, err := waiverAllocationSnapshot(ctx, tx, h.ID)
	if err != nil {
		return zero, err
	}
	allocationAudit, _ := json.Marshal(map[string]any{"before": json.RawMessage(before), "after": json.RawMessage(after)})
	raw, _ := json.Marshal(result)
	if _, err := tx.Exec(ctx, `INSERT INTO usage_waiver_results(command_reference,outcome,result_snapshot,allocation_snapshot,created_at) VALUES($1,$2,$3,$4,$5)`, c.Reference, result.Outcome, raw, allocationAudit, now); err != nil {
		return zero, err
	}
	if result.Outcome == usagewaiver.Waived {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_admin_actions(id,hold_id,action,idempotency_key,request_fingerprint,reason,actor,before_status,after_status,allocation_snapshot,created_at) SELECT $1,hold_id,'SAFE_RELEASE',$2,$3,$4,$5,before_status,'RELEASED',$6,$7 FROM usage_waiver_commands WHERE command_reference=$8`, uuid.Must(uuid.NewV7()), "waiver:"+c.Reference.String(), c.Digest, c.Reason, c.Issuer+"|"+c.Subject+"|"+c.ActorID.String(), allocationAudit, now, c.Reference); err != nil {
			return zero, err
		}
	}
	return result, tx.Commit(ctx)
}
