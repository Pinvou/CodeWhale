package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// HoldExtension binds one positive reservation increase to an operation ID.
// Zero additional credits remain an ordinary lease renewal without a receipt.
type HoldExtension struct {
	UsageRequestID         uuid.UUID
	HoldID                 uuid.UUID
	OperationID            uuid.UUID
	LeaseSeconds           int
	AdditionalMicrocredits int64
}

var errHoldExtensionReceiptRace = errors.New("hold extension receipt was concurrently committed")

func (c HoldExtension) normalized() (HoldExtension, error) {
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = 900
	}
	if c.UsageRequestID == uuid.Nil || c.HoldID == uuid.Nil || c.AdditionalMicrocredits < 0 || c.LeaseSeconds < 30 || c.LeaseSeconds > 1800 {
		return c, ErrConflict
	}
	if (c.AdditionalMicrocredits > 0) == (c.OperationID == uuid.Nil) {
		return c, ErrConflict
	}
	return c, nil
}

func (c HoldExtension) fingerprint() string {
	// Fixed field order and exact integers; server time is not a command fact.
	facts := struct {
		UsageRequestID         uuid.UUID `json:"usage_request_id"`
		HoldID                 uuid.UUID `json:"hold_id"`
		AdditionalMicrocredits int64     `json:"additional_microcredits"`
		LeaseSeconds           int       `json:"lease_seconds"`
	}{c.UsageRequestID, c.HoldID, c.AdditionalMicrocredits, c.LeaseSeconds}
	b, _ := json.Marshal(facts)
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) ExtendHold(ctx context.Context, command HoldExtension) (Hold, error) {
	command, err := command.normalized()
	if err != nil {
		return Hold{}, err
	}
	// Freeze once at entry, including across confirmed-rollback retries. A wait
	// for the wallet must not refresh an already expired requested deadline.
	deadline := r.now().Add(time.Duration(command.LeaseSeconds) * time.Second)
	for attempt := 0; attempt < 4; attempt++ {
		hold, err, retry := r.extendHoldAttempt(ctx, command, deadline)
		if !retry || command.AdditionalMicrocredits == 0 || attempt == 3 {
			return hold, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Hold{}, ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable hold extension attempt")
}

func (r *PostgresRepository) extendHoldAttempt(ctx context.Context, command HoldExtension, deadline time.Time) (Hold, error, bool) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Hold{}, err, false
	}
	rollback := func() error {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return tx.Rollback(cleanup)
	}
	defer rollback()
	hold, err := func() (Hold, error) {
		if command.AdditionalMicrocredits > 0 {
			saved, found, err := holdExtensionReceipt(ctx, tx, command)
			if err != nil || found {
				return saved, err
			}
		}
		result, err := r.extendHoldWithinTransaction(ctx, tx, command, deadline)
		if err != nil {
			return Hold{}, err
		}
		if command.AdditionalMicrocredits > 0 {
			body, err := json.Marshal(result)
			if err != nil {
				return Hold{}, err
			}
			now := r.now()
			tag, err := tx.Exec(ctx, `INSERT INTO idempotency_records(scope,idempotency_key,request_hash,response_status,response_body,created_at,expires_at) VALUES('usage.hold.extend',$1,$2,200,$3,$4,$4::timestamptz+interval '100 years') ON CONFLICT(scope,idempotency_key) DO NOTHING`, command.OperationID.String(), command.fingerprint(), body, now)
			if err != nil {
				return Hold{}, err
			}
			if tag.RowsAffected() != 1 {
				return Hold{}, errHoldExtensionReceiptRace
			}
		}
		return result, nil
	}()
	if err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			// A cleanup failure is infrastructure uncertainty, even when the
			// original rejection was a definite domain error.
			return Hold{}, fmt.Errorf("rollback hold extension transaction: %w", rollbackErr), false
		}
		var conflict *pgconn.PgError
		retry := errors.Is(err, errHoldExtensionReceiptRace) || (errors.As(err, &conflict) && conflict.Code == "40001")
		return Hold{}, err, retry
	}
	err = tx.Commit(ctx)
	// A server serialization rejection proves that Commit aborted the complete
	// transaction. Every other commit outcome remains uncertain and is never
	// retried here; a caller must discover its receipt with the same operation ID.
	return hold, err, holdExtensionCommitAborted(err)
}

func holdExtensionCommitAborted(err error) bool {
	for err != nil {
		// A mixed error can also describe a connection/cleanup failure. Do not
		// infer a definite server outcome merely because one joined cause is PG.
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			return false
		}
		if rejected, ok := err.(*pgconn.PgError); ok {
			return rejected != nil && rejected.Code == "40001"
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

func holdExtensionReceipt(ctx context.Context, tx pgx.Tx, command HoldExtension) (Hold, bool, error) {
	var hash string
	var status int
	var body []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,response_status,response_body FROM idempotency_records WHERE scope='usage.hold.extend' AND idempotency_key=$1`, command.OperationID.String()).Scan(&hash, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, false, nil
	}
	if err != nil {
		return Hold{}, false, err
	}
	var saved Hold
	if hash != command.fingerprint() || status != 200 || json.Unmarshal(body, &saved) != nil || saved.ID != command.HoldID || saved.UsageRequestID != command.UsageRequestID || saved.WalletID == uuid.Nil || saved.Status != "ACTIVE" || saved.ActualMicrocredits != nil || saved.MaxMicrocredits < command.AdditionalMicrocredits || saved.ExpiresAt.IsZero() {
		return Hold{}, false, ErrConflict
	}
	current, err := getHoldByUsage(ctx, tx, command.UsageRequestID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, false, ErrConflict
	}
	if err != nil {
		return Hold{}, false, err
	}
	if current.ID != saved.ID || current.WalletID != saved.WalletID || !equalUUID(current.PriceVersionID, saved.PriceVersionID) {
		return Hold{}, false, ErrConflict
	}
	// This is the original successful command ACK, not a claim that the hold is
	// still active. Terminal/expiry changes cannot reapply money or renew it.
	return saved, true, nil
}

func (r *PostgresRepository) extendHoldWithinTransaction(ctx context.Context, tx pgx.Tx, command HoldExtension, expiresAt time.Time) (Hold, error) {
	usageRequestID, holdID, additional := command.UsageRequestID, command.HoldID, command.AdditionalMicrocredits
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
	if !expiresAt.After(now) || !hold.ExpiresAt.After(now) {
		return Hold{}, ErrConflict
	}
	previousExpiresAt, previousMaximum := hold.ExpiresAt, hold.MaxMicrocredits
	if _, err := validateMemberQuotaHoldAllocations(ctx, tx, hold.ID, 0); err != nil {
		return Hold{}, err
	}
	if hold.MaxMicrocredits < 0 || additional > math.MaxInt64-hold.MaxMicrocredits {
		return Hold{}, ErrConflict
	}
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
		if _, err := validateMemberQuotaHoldAllocations(ctx, tx, hold.ID, additional); err != nil {
			return Hold{}, err
		}
		hold.MaxMicrocredits += additional
	}
	if expiresAt.After(hold.ExpiresAt) {
		hold.ExpiresAt = expiresAt.UTC()
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_holds SET expires_at=$2,max_microcredits=$3,updated_at=$4 WHERE id=$1 AND status='ACTIVE'`, hold.ID, hold.ExpiresAt, hold.MaxMicrocredits, now); err != nil {
		// The database clock can cross expiry after our locked domain check.
		// Map only this reviewed renewal fence; other trigger failures remain
		// errors and roll back every reservation/lease mutation.
		var fence *pgconn.PgError
		if errors.As(err, &fence) && fence.Code == "55000" && fence.Message == "expired hold cannot be renewed during recovery" {
			return Hold{}, ErrConflict
		}
		return Hold{}, err
	}
	if hold.ExpiresAt.After(previousExpiresAt) || hold.MaxMicrocredits != previousMaximum {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_lease_events(id,hold_id,event_type,previous_expires_at,expires_at,previous_max_microcredits,max_microcredits,additional_microcredits,created_at) VALUES($1,$2,'EXTENDED',$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), hold.ID, previousExpiresAt, hold.ExpiresAt, previousMaximum, hold.MaxMicrocredits, hold.MaxMicrocredits-previousMaximum, now); err != nil {
			return Hold{}, err
		}
	}
	return hold, nil
}
