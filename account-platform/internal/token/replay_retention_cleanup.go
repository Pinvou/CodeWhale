package token

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const replayCleanupBatchSize = 100
const replayCleanupTransactionBudget = 2 * time.Second

const replayCleanupCaseGuard = `NOT EXISTS(SELECT 1 FROM usage_replay_retention_cases retention WHERE retention.usage_request_id=content.usage_request_id AND retention.released_at IS NULL AND retention.expires_at>$1)`

type replayCleanupCandidate struct {
	usageID, userID uuid.UUID
	sequence        int64
}

// Each category gets one bounded transaction per cycle. A backlog is continued
// on the next cycle; already committed categories are safe to retry after error.
func (r *PostgresRepository) cleanupReplayContentBatch(ctx context.Context, table string, unsettled bool, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, replayCleanupTransactionBudget)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	sequence, key := "0::bigint", ""
	if table == "usage_stream_events" {
		sequence, key = "content.sequence", " AND content.sequence=@sequence"
	} else if table != "usage_replay_results" {
		return ErrForbidden
	}
	due := `content.expires_at<=@now AND request.status IN ('COMMITTED','RELEASED')`
	if unsettled {
		due = `octet_length(content.encrypted_payload)>0 AND (content.expires_at<=@now OR content.created_at<=@cutoff) AND request.status NOT IN ('COMMITTED','RELEASED')`
	}
	guard := strings.ReplaceAll(replayCleanupCaseGuard, "$1", "@now")
	query := `SELECT content.usage_request_id,request.user_id,` + sequence + ` FROM ` + table + ` content JOIN usage_requests request ON request.id=content.usage_request_id WHERE ` + due + ` AND ` + guard + ` ORDER BY content.expires_at NULLS LAST,content.created_at,content.usage_request_id,` + sequence + ` LIMIT @limit FOR UPDATE OF request SKIP LOCKED`
	rows, err := tx.Query(ctx, query, pgx.NamedArgs{"now": now, "cutoff": now.Add(-720 * time.Hour), "limit": replayCleanupBatchSize})
	if err != nil {
		return err
	}
	var candidates []replayCleanupCandidate
	for rows.Next() {
		var item replayCleanupCandidate
		if err = rows.Scan(&item.usageID, &item.userID, &item.sequence); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, item := range candidates {
		// Anonymization holds the exclusive user lock before touching content.
		// Never wait for it while holding parent rows in the opposite order.
		var admitted bool
		if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(hashtextextended($1,0))`, "token.replay.user:"+item.userID.String()).Scan(&admitted); err != nil {
			return err
		}
		if !admitted {
			continue
		}
		mutation := `DELETE FROM ` + table + ` content USING usage_requests request WHERE `
		if unsettled {
			// Keep original metadata, sequence and first-persistence clocks.
			mutation = `UPDATE ` + table + ` content SET encrypted_payload='\x'::bytea,expires_at=LEAST(COALESCE(content.expires_at,content.created_at+interval '720 hours'),content.created_at+interval '720 hours') FROM usage_requests request WHERE `
		}
		// This new statement observes cases committed before the parent lock.
		// A new case cannot pass the parent lock until this transaction ends.
		mutation += `request.id=content.usage_request_id AND content.usage_request_id=@usage` + key + ` AND ` + due + ` AND ` + guard
		if _, err = tx.Exec(ctx, mutation, pgx.NamedArgs{"now": now, "cutoff": now.Add(-720 * time.Hour), "usage": item.usageID, "sequence": item.sequence}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) cleanupReplayTombstoneBatch(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, replayCleanupTransactionBudget)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Select only unprotected due rows before LIMIT, so protected oldest rows
	// cannot consume the batch. Parent locks serialize key clearing with reuse.
	query := `SELECT content.usage_request_id FROM usage_idempotency_tombstones content JOIN usage_requests request ON request.id=content.usage_request_id WHERE content.expires_at<=$1 AND ` + replayCleanupCaseGuard + ` ORDER BY content.expires_at,content.usage_request_id LIMIT $2 FOR UPDATE OF request,content SKIP LOCKED`
	rows, err := tx.Query(ctx, query, now, replayCleanupBatchSize)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(ids) > 0 {
		// DELETE and key clearing share one statement and transaction. The fresh
		// case check is protected by the already held parent locks.
		_, err = tx.Exec(ctx, `WITH removed AS (DELETE FROM usage_idempotency_tombstones content WHERE content.usage_request_id=ANY($2::uuid[]) AND content.expires_at<=$1 AND `+replayCleanupCaseGuard+` RETURNING usage_request_id) UPDATE usage_requests request SET idempotency_key_digest=NULL,updated_at=$1 FROM removed WHERE request.id=removed.usage_request_id`, now, ids)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) cleanupReplayCaseBatch(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, replayCleanupTransactionBudget)
	defer cancel()
	// One payload row per case cascades atomically with the bounded case rows.
	_, err := r.pool.Exec(ctx, fmt.Sprintf(`WITH due AS (SELECT id FROM usage_replay_retention_cases WHERE (released_at IS NOT NULL AND clear_by<=$1) OR expires_at<=$1 ORDER BY expires_at,id LIMIT %d FOR UPDATE SKIP LOCKED) DELETE FROM usage_replay_retention_cases retention USING due WHERE retention.id=due.id`, replayCleanupBatchSize), now)
	return err
}
