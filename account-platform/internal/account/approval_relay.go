package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ApprovalAuthorizationEventType = "account.approval-command.authorized.v1"

const approvalRelayClaimFunction = "account_approval_authorization.claim_authorization_events(text,integer,integer)"
const approvalRelayOpenFunction = "account_approval_authorization.open_authorization_delivery(uuid,uuid,bigint,text)"
const approvalRelayFinishFunction = "account_approval_authorization.finish_authorization_delivery(uuid,uuid,bigint,text,text,timestamp with time zone,text)"
const approvalRelayOutcomeFunction = "account_approval_authorization.authorization_relay_outcome(uuid,uuid,bigint,text)"
const approvalRelayReadinessFunction = "account_approval_authorization.authorization_relay_readiness()"

var ErrApprovalRelayUnavailable = errors.New("approval authorization relay is unavailable")
var errApprovalRelayReadinessContract = errors.New("approval authorization relay readiness contract failed")

// Detailed causes stay behind the public readiness error boundary. The stage
// is closed diagnostic metadata; Error never renders a DSN or database error.
type approvalRelayReadinessError struct {
	stage string
	cause error
}

func (failure *approvalRelayReadinessError) Error() string {
	return "approval relay readiness: " + failure.stage
}
func (failure *approvalRelayReadinessError) Unwrap() error { return failure.cause }

type ApprovalRelayStats struct {
	Pending              int64
	ActiveClaims         int64
	ReclaimableClaims    int64
	UnclassifiedDeadRows int64
}

type ApprovalRelayStore struct {
	pool          *pgxpool.Pool
	commit        func(context.Context, pgx.Tx) error
	outcomeLookup func(events.FrozenClaim, string) (string, int, error)
}

func NewApprovalRelayStore(ctx context.Context, dsn string) (*ApprovalRelayStore, error) {
	store, err := newApprovalRelayStore(ctx, dsn)
	if err != nil {
		return nil, ErrApprovalRelayUnavailable
	}
	return store, nil
}

// newApprovalRelayStore preserves the first readiness failure only for
// same-package integration diagnostics. The exported process boundary above
// always maps it to the stable, redacted error.
func newApprovalRelayStore(ctx context.Context, dsn string) (*ApprovalRelayStore, error) {
	pool, err := newApprovalAuthorizationPool(ctx, dsn, "account_approval_relay")
	if err != nil {
		return nil, err
	}
	store := &ApprovalRelayStore{pool: pool}
	if err := store.approvalRelayReadiness(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (store *ApprovalRelayStore) Close() {
	if store != nil && store.pool != nil {
		store.pool.Close()
	}
}

func (store *ApprovalRelayStore) Ready(ctx context.Context) error {
	if err := store.approvalRelayReadiness(ctx); err != nil {
		return ErrApprovalRelayUnavailable
	}
	return nil
}

// approvalRelayReadiness preserves the underlying database classification for
// same-package diagnostics. Public callers receive only the stable error above.
func (store *ApprovalRelayStore) approvalRelayReadiness(ctx context.Context) error {
	if store == nil || store.pool == nil {
		return errApprovalRelayReadinessContract
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	connection, err := store.pool.Acquire(probeContext)
	if err != nil {
		return &approvalRelayReadinessError{stage: "acquire", cause: err}
	}
	defer connection.Release()
	var valid bool
	err = connection.QueryRow(probeContext, `SELECT session_user='account_approval_relay' AND current_user=session_user
        AND rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb
        AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
        AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid OR roleid=r.oid)
        AND NOT pg_catalog.has_database_privilege(current_user,current_database(),'CREATE')
        AND pg_catalog.has_schema_privilege(current_user,'account_approval_authorization','USAGE')
		AND NOT pg_catalog.has_schema_privilege(current_user,'public','CREATE')
		AND NOT pg_catalog.has_schema_privilege(current_user,'account_approval_authorization','CREATE')
		AND NOT pg_catalog.has_schema_privilege(current_user,'account_approval_control','CREATE')
        AND pg_catalog.has_function_privilege(current_user,$1::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$2::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$3::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$4::text,'EXECUTE')
        AND pg_catalog.has_function_privilege(current_user,$5::text,'EXECUTE')
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
            WHERE n.nspname='account_approval_authorization'
              AND p.oid NOT IN ($1::text::regprocedure,$2::text::regprocedure,$3::text::regprocedure,$4::text::regprocedure,$5::text::regprocedure)
              AND pg_catalog.has_function_privilege(current_user,p.oid,'EXECUTE'))
		AND NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_proc p
			JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
			JOIN pg_catalog.pg_roles owner_role ON owner_role.oid=p.proowner
			WHERE p.oid IN ($1::text::regprocedure,$2::text::regprocedure,$3::text::regprocedure,$4::text::regprocedure,$5::text::regprocedure)
			  AND (n.nspname<>'account_approval_authorization'
			       OR owner_role.rolname<>'account_approval_authorization_owner'
			       OR NOT p.prosecdef
			       OR NOT COALESCE(p.proconfig @> ARRAY['search_path=pg_catalog']::text[],false)))
        AND NOT EXISTS (
            SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
            WHERE n.nspname IN ('public','account_approval_authorization','account_approval_control')
              AND c.relkind IN ('r','p','v','m','f')
              AND (pg_catalog.has_any_column_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE,REFERENCES')
                   OR pg_catalog.has_table_privilege(current_user,c.oid,'DELETE,TRUNCATE,TRIGGER')))
        FROM pg_catalog.pg_roles r WHERE rolname=session_user`, approvalRelayClaimFunction, approvalRelayOpenFunction, approvalRelayFinishFunction, approvalRelayOutcomeFunction, approvalRelayReadinessFunction).Scan(&valid)
	if err != nil || !valid {
		if err != nil {
			return &approvalRelayReadinessError{stage: "identity-query", cause: err}
		}
		return &approvalRelayReadinessError{stage: "identity-contract", cause: errApprovalRelayReadinessContract}
	}
	var pending, active, reclaimable, dead int64
	if err := connection.QueryRow(probeContext, `SELECT * FROM account_approval_authorization.authorization_relay_readiness()`).Scan(&pending, &active, &reclaimable, &dead); err != nil {
		return &approvalRelayReadinessError{stage: "sidecar-query", cause: err}
	}
	return nil
}

func (store *ApprovalRelayStore) Stats(ctx context.Context) (ApprovalRelayStats, error) {
	var stats ApprovalRelayStats
	if store == nil || store.pool == nil {
		return stats, ErrApprovalRelayUnavailable
	}
	err := store.pool.QueryRow(ctx, `SELECT * FROM account_approval_authorization.authorization_relay_readiness()`).Scan(
		&stats.Pending, &stats.ActiveClaims, &stats.ReclaimableClaims, &stats.UnclassifiedDeadRows,
	)
	if err != nil {
		return ApprovalRelayStats{}, ErrApprovalRelayUnavailable
	}
	return stats, nil
}

func (store *ApprovalRelayStore) ClaimFrozen(ctx context.Context, workerID string, limit int, lease time.Duration) ([]events.FrozenClaim, error) {
	if store == nil || store.pool == nil || !validRelayWorker(workerID) || limit < 1 || limit > 1000 || lease < 10*time.Second || lease > 5*time.Minute {
		return nil, ErrApprovalRelayUnavailable
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, ErrApprovalRelayUnavailable
	}
	defer boundedApprovalRelayRollback(tx)
	rows, err := tx.Query(ctx, `SELECT * FROM account_approval_authorization.claim_authorization_events($1,$2,$3)`, workerID, limit, int(lease.Seconds()))
	if err != nil {
		return nil, ErrApprovalRelayUnavailable
	}
	claims := make([]events.FrozenClaim, 0, limit)
	for rows.Next() {
		var claim events.FrozenClaim
		if err := rows.Scan(&claim.EventID, &claim.EventType, &claim.TraceID, &claim.ClaimToken, &claim.RelayControlEpoch); err != nil {
			rows.Close()
			return nil, ErrApprovalRelayUnavailable
		}
		claims = append(claims, claim)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return nil, ErrApprovalRelayUnavailable
	}
	if err := store.commitTx(ctx, tx); err != nil {
		if approvalRelayCommitDefinitelyAborted(err) {
			return nil, ErrApprovalRelayUnavailable
		}
		for _, claim := range claims {
			outcome, _, lookupErr := store.outcome(claim, workerID)
			if lookupErr != nil || outcome != "CLAIMED" {
				return nil, ErrApprovalRelayUnavailable
			}
		}
	}
	return claims, nil
}

func (store *ApprovalRelayStore) ProcessFrozen(ctx context.Context, workerID string, claim events.FrozenClaim, maxAttempts int, now time.Time, publisher events.FrozenPublisher) (events.FrozenDisposition, error) {
	if store == nil || store.pool == nil || publisher == nil || !validRelayWorker(workerID) || claim.EventID == uuid.Nil || claim.EventType != ApprovalAuthorizationEventType || claim.ClaimToken == uuid.Nil || claim.RelayControlEpoch < 1 || maxAttempts != 12 {
		return "", ErrApprovalRelayUnavailable
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", ErrApprovalRelayUnavailable
	}
	defer boundedApprovalRelayRollback(tx)
	var eventType, traceID, digest string
	var body []byte
	var attempts int
	var leaseUntil time.Time
	err = tx.QueryRow(ctx, `SELECT * FROM account_approval_authorization.open_authorization_delivery($1,$2,$3,$4)`, claim.EventID, claim.ClaimToken, claim.RelayControlEpoch, workerID).Scan(&eventType, &traceID, &body, &digest, &attempts, &leaseUntil)
	if err != nil || eventType != ApprovalAuthorizationEventType || traceID != claim.TraceID || !validFrozenBody(body, digest) {
		return "", ErrApprovalRelayUnavailable
	}
	// Both the primary and possible DLQ publish share the absolute lease
	// deadline. Keep a final second for the database close; never reuse the
	// primary budget after time has elapsed.
	finishDeadline := leaseUntil.UTC().Add(-time.Second)
	publishBudget := time.Until(finishDeadline)
	if publishBudget > 5*time.Second {
		publishBudget = 5 * time.Second
	}
	if publishBudget <= 0 {
		return "", ErrApprovalRelayUnavailable
	}
	publishContext, cancel := context.WithTimeout(ctx, publishBudget)
	result := publisher(publishContext, events.FrozenMessage{EventID: claim.EventID, Subject: eventType, Body: body, MsgID: claim.EventID.String()})
	cancel()
	switch result.Disposition {
	case events.FrozenPublished, events.FrozenRetry:
	case events.FrozenUnknown:
		// A broker timeout or transport close cannot prove whether PubAck was
		// received. Roll back the close transaction and retain the exact claim;
		// lease recovery may publish the same id and bytes again.
		return "", ErrApprovalRelayUnavailable
	default:
		return "", ErrApprovalRelayUnavailable
	}
	disposition := result.Disposition
	if disposition != events.FrozenPublished {
		disposition = events.FrozenRetry
		if attempts+1 >= maxAttempts {
			deadBudget := time.Until(finishDeadline)
			if deadBudget > 5*time.Second {
				deadBudget = 5 * time.Second
			}
			if deadBudget <= 0 {
				return "", ErrApprovalRelayUnavailable
			}
			deadContext, deadCancel := context.WithTimeout(ctx, deadBudget)
			deadResult := publisher(deadContext, events.FrozenMessage{EventID: claim.EventID, Subject: "account-platform.dead-letter." + eventType, Body: body, MsgID: claim.EventID.String() + ":dead-letter"})
			deadCancel()
			switch deadResult.Disposition {
			case events.FrozenPublished:
				disposition = events.FrozenDeadLettered
			case events.FrozenUnknown:
				return "", ErrApprovalRelayUnavailable
			case events.FrozenRetry:
				if deadResult.ErrorCode != "" {
					result.ErrorCode = deadResult.ErrorCode
				}
			default:
				return "", ErrApprovalRelayUnavailable
			}
		}
	}
	if disposition == events.FrozenRetry && result.ErrorCode == "" {
		result.ErrorCode = "EVENT_DELIVERY_FAILED"
	}
	retryAt := now.UTC().Add(approvalRelayRetryDelay(attempts + 1))
	if disposition != events.FrozenRetry {
		retryAt = time.Time{}
	}
	var finishError any
	if disposition != events.FrozenPublished {
		finishError = safeRelayErrorCode(result.ErrorCode)
	}
	if _, err := tx.Exec(ctx, `SELECT account_approval_authorization.finish_authorization_delivery($1,$2,$3,$4,$5,$6,$7)`, claim.EventID, claim.ClaimToken, claim.RelayControlEpoch, workerID, string(disposition), nullableRelayTime(retryAt), finishError); err != nil {
		return "", ErrApprovalRelayUnavailable
	}
	if err := store.commitTx(ctx, tx); err != nil {
		if approvalRelayCommitDefinitelyAborted(err) {
			return "", ErrApprovalRelayUnavailable
		}
		outcome, outcomeAttempts, lookupErr := store.outcome(claim, workerID)
		if lookupErr != nil || outcome != string(disposition) || outcomeAttempts != attempts+1 {
			return "", ErrApprovalRelayUnavailable
		}
	}
	return disposition, nil
}

func (store *ApprovalRelayStore) outcome(claim events.FrozenClaim, workerID string) (string, int, error) {
	if store.outcomeLookup != nil {
		return store.outcomeLookup(claim, workerID)
	}
	lookupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var outcome string
	var attempts int
	err := store.pool.QueryRow(lookupContext, `SELECT * FROM account_approval_authorization.authorization_relay_outcome($1,$2,$3,$4)`, claim.EventID, claim.ClaimToken, claim.RelayControlEpoch, workerID).Scan(&outcome, &attempts)
	return outcome, attempts, err
}

func (store *ApprovalRelayStore) commitTx(ctx context.Context, tx pgx.Tx) error {
	if store.commit != nil {
		return store.commit(ctx, tx)
	}
	return tx.Commit(ctx)
}

func validRelayWorker(workerID string) bool {
	return workerID == strings.TrimSpace(workerID) && len(workerID) >= 1 && len(workerID) <= 160 && strings.IndexFunc(workerID, unicode.IsControl) < 0
}

func validFrozenBody(body []byte, expected string) bool {
	if len(body) == 0 || len(body) > 64<<10 || len(expected) != 64 {
		return false
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]) == expected
}

func approvalRelayCommitDefinitelyAborted(err error) bool {
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return true
	}
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && (postgresError.Code == "40001" || postgresError.Code == "40P01")
}

func boundedApprovalRelayRollback(tx pgx.Tx) {
	rollbackContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(rollbackContext)
}

func approvalRelayRetryDelay(attempt int) time.Duration {
	if attempt > 9 {
		attempt = 9
	}
	return time.Duration(1<<attempt) * time.Second
}

func safeRelayErrorCode(code string) string {
	switch code {
	case "EVENT_DELIVERY_TIMEOUT", "EVENT_BROKER_UNAVAILABLE", "EVENT_DELIVERY_FAILED":
		return code
	default:
		return "EVENT_DELIVERY_FAILED"
	}
}

func nullableRelayTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}
