package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrTokenAccountCapReached           = errors.New("token account owner cap reached")
	ErrTokenAccountPolicyUnavailable    = errors.New("token account owner cap policy unavailable")
	ErrTokenAccountPolicyConfigMismatch = errors.New("token account owner cap policy config mismatch")
	ErrTokenAccountCreateNotActive      = errors.New("token account creation is not active")
)

type tokenAccountOwnerCapPolicy struct {
	PolicyVersion int64
	MaxPerOwner   int64
	ConfigDigest  string
}

type tokenAccountOwnerCapacity struct {
	Policy tokenAccountOwnerCapPolicy
	Count  int64
}

func tokenAccountOwnerCapConfigDigest(maxPerOwner, version int64) string {
	payload := []byte(`{"max_per_owner":` + strconv.FormatInt(maxPerOwner, 10) + `,"policy_version":` + strconv.FormatInt(version, 10) + `}`)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func loadTokenAccountOwnerCapacity(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ownerType string, ownerID uuid.UUID) (tokenAccountOwnerCapacity, error) {
	var result tokenAccountOwnerCapacity
	err := query.QueryRow(ctx, `SELECT revision.policy_version,revision.max_per_owner,revision.config_digest,
 ((SELECT count(*) FROM token_accounts account
   WHERE account.management_mode='PLATFORM' AND account.owner_type=$1 AND account.owner_id=$2
     AND account.status IN ('ACTIVE','DISABLED','FROZEN')) +
  (SELECT count(*) FROM token_account_provisioning_reservations reservation
   WHERE reservation.owner_type=$1 AND reservation.owner_id=$2
     AND reservation.status IN ('PENDING','MANUAL_REVIEW')))
FROM token_account_owner_cap_policy_active pointer
JOIN token_account_owner_cap_policy_revisions revision ON revision.policy_version=pointer.active_policy_version
WHERE pointer.singleton_key`, ownerType, ownerID).Scan(&result.Policy.PolicyVersion, &result.Policy.MaxPerOwner, &result.Policy.ConfigDigest, &result.Count)
	if errors.Is(err, pgx.ErrNoRows) || isUndefinedOwnerCapPolicy(err) {
		return tokenAccountOwnerCapacity{}, ErrTokenAccountPolicyUnavailable
	}
	if err != nil {
		return tokenAccountOwnerCapacity{}, err
	}
	if result.Policy.PolicyVersion < 1 || result.Policy.MaxPerOwner < 1 || result.Policy.MaxPerOwner > 1000 || result.Count < 0 || result.Policy.ConfigDigest != tokenAccountOwnerCapConfigDigest(result.Policy.MaxPerOwner, result.Policy.PolicyVersion) {
		return tokenAccountOwnerCapacity{}, ErrTokenAccountPolicyConfigMismatch
	}
	return result, nil
}

// lockTokenAccountOwnerCapPolicy serializes every PLATFORM-account creator
// with the privileged active-policy CAS. It is deliberately separate from the
// read-only precheck loader because PostgreSQL read-only snapshots may not take
// row locks.
func lockTokenAccountOwnerCapPolicy(ctx context.Context, tx pgx.Tx) error {
	var activeVersion *int64
	if err := tx.QueryRow(ctx, `SELECT active_policy_version FROM token_account_owner_cap_policy_active WHERE singleton_key FOR SHARE`).Scan(&activeVersion); errors.Is(err, pgx.ErrNoRows) || isUndefinedOwnerCapPolicy(err) {
		return ErrTokenAccountPolicyUnavailable
	} else if err != nil {
		return err
	}
	if activeVersion == nil || *activeVersion < 1 {
		return ErrTokenAccountPolicyUnavailable
	}
	return nil
}

func isUndefinedOwnerCapPolicy(err error) bool {
	if err == nil {
		return false
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "42P01"
}

func (capacity tokenAccountOwnerCapacity) validateExpected(expected governedAccountCreateExpected) error {
	if expected.OwnerAccountCount != capacity.Count || expected.OwnerAccountCap != capacity.Policy.MaxPerOwner || expected.OwnerAccountCapPolicyVersion != capacity.Policy.PolicyVersion {
		return ErrCatalogVersionConflict
	}
	if capacity.Count >= capacity.Policy.MaxPerOwner {
		return ErrTokenAccountCapReached
	}
	return nil
}

func lockTokenAccountOwner(ctx context.Context, tx pgx.Tx, ownerType string, ownerID uuid.UUID) error {
	if ownerID == uuid.Nil || ownerType == "" {
		return ErrTokenCommandInvalid
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fmt.Sprintf("token-account-owner:%s:%s", ownerType, ownerID))
	return err
}

// A SERIALIZABLE snapshot taken before waiting for the owner mutex can miss a
// committed READ COMMITTED creator. Acquire the same mutex at session scope
// first, then start the transaction and its first snapshot. This also covers
// default-route/background creators that use the transaction-scoped variant.
func (r *PostgresRepository) beginOwnerLockedSerializableTransaction(ctx context.Context, ownerType string, ownerID uuid.UUID) (pgx.Tx, func(), error) {
	return r.beginTokenSerializedTransaction(ctx, fmt.Sprintf("token-account-owner:%s:%s", ownerType, ownerID))
}

func (r *PostgresRepository) beginTokenSerializedTransaction(ctx context.Context, key string) (pgx.Tx, func(), error) {
	connection, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err = connection.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); err != nil {
		// A transport error may conceal a granted session lock. Closing the
		// connection guarantees it never returns to the pool with a leaked lock.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = connection.Hijack().Close(cleanup)
		return nil, nil, err
	}
	var tx pgx.Tx
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if tx != nil {
			_ = tx.Rollback(cleanup)
		}
		var unlocked bool
		if unlockErr := connection.QueryRow(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key).Scan(&unlocked); unlockErr != nil || !unlocked {
			_ = connection.Hijack().Close(cleanup)
			return
		}
		connection.Release()
	}
	tx, err = connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		release()
		return nil, nil, err
	}
	return tx, release, nil
}
