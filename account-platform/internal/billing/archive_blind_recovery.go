package billing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const automaticArchiveBlindReason = "archive deadline reached without a successful authoritative unpaid query"

type ArchiveBlindRecoveryCandidate struct {
	CaseID                uuid.UUID
	PaymentOrderID        uuid.UUID
	ProviderID            string
	ProviderVersion       string
	LastSuccessfulQueryAt *time.Time
}

type ArchiveBlindRecoveryStore interface {
	ListAutomaticallyMisclassifiedArchiveBlindCases(context.Context, int) ([]ArchiveBlindRecoveryCandidate, error)
	RetractAutomaticallyMisclassifiedArchiveBlindCase(context.Context, ArchiveBlindRecoveryCandidate) (bool, error)
}

// ArchiveBlindRecoveryWorker repairs the historical state produced before
// non-authoritative providers were excluded from archive-blind escalation. A
// fresh provider query is mandatory before the erroneous safety pause can be
// retracted, so deployment never re-enables an unreachable provider.
type ArchiveBlindRecoveryWorker struct {
	store        ArchiveBlindRecoveryStore
	synchronizer PaymentOrderSynchronizer
	interval     time.Duration
	batchSize    int
	logger       *slog.Logger
}

func NewArchiveBlindRecoveryWorker(store ArchiveBlindRecoveryStore, synchronizer PaymentOrderSynchronizer, interval time.Duration) *ArchiveBlindRecoveryWorker {
	if interval < time.Second {
		interval = time.Minute
	}
	return &ArchiveBlindRecoveryWorker{
		store:        store,
		synchronizer: synchronizer,
		interval:     interval,
		// Recovery performs a real provider query before retracting each stale
		// case. Keep the default batch at one so a deployment cannot turn a
		// backlog into a provider-query burst; the periodic worker will converge
		// the backlog one verified case at a time.
		batchSize: 1,
		logger:    slog.Default().With("component", "archive-blind-recovery"),
	}
}

func (worker *ArchiveBlindRecoveryWorker) Run(ctx context.Context) {
	worker.runOnce(ctx)
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			worker.runOnce(ctx)
		}
	}
}

func (worker *ArchiveBlindRecoveryWorker) runOnce(ctx context.Context) {
	items, err := worker.store.ListAutomaticallyMisclassifiedArchiveBlindCases(ctx, worker.batchSize)
	if err != nil {
		worker.logger.Error("list automatically misclassified archive blind cases failed", "error", err)
		return
	}
	for _, item := range items {
		if _, err := worker.synchronizer.SyncOrder(ctx, item.PaymentOrderID); err != nil {
			worker.logger.Warn("archive blind recovery probe failed", "case_id", item.CaseID, "order_id", item.PaymentOrderID, "error", safePaymentReconciliationError(err))
			continue
		}
		retracted, err := worker.store.RetractAutomaticallyMisclassifiedArchiveBlindCase(ctx, item)
		if err != nil {
			worker.logger.Error("retract automatically misclassified archive blind case failed", "case_id", item.CaseID, "order_id", item.PaymentOrderID, "error", err)
			continue
		}
		if retracted {
			worker.logger.Info("automatically misclassified archive blind case retracted", "case_id", item.CaseID, "order_id", item.PaymentOrderID, "provider_id", item.ProviderID, "provider_version", item.ProviderVersion)
		}
	}
}

func (r *PostgresRepository) ListAutomaticallyMisclassifiedArchiveBlindCases(ctx context.Context, limit int) ([]ArchiveBlindRecoveryCandidate, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
SELECT late.id,late.payment_order_id,late.provider_id,late.provider_version,payment.last_successful_query_at
  FROM late_payment_review_cases late
  JOIN payment_orders payment ON payment.id=late.payment_order_id
 WHERE late.review_status='PROVIDER_BLIND'
   AND late.blind_reason=$1
   AND late.provider_capability_snapshot @> '{"authoritative_unpaid_terminal":false}'::jsonb
   AND EXISTS(
       SELECT 1 FROM late_payment_review_evidence evidence
        WHERE evidence.case_id=late.id AND evidence.evidence_type='PROVIDER_BLIND'
          AND evidence.actor_id IS NULL AND evidence.payload_snapshot->>'reason'=$1)
   AND NOT EXISTS(
       SELECT 1 FROM payment_observation_gaps gap
        WHERE gap.provider_id=late.provider_id AND gap.provider_version=late.provider_version)
   AND NOT EXISTS(
       SELECT 1 FROM payment_stop_loss_evaluations evaluation
        WHERE evaluation.scope_type='PROVIDER'
          AND evaluation.provider_id=late.provider_id AND evaluation.provider_version=late.provider_version
          AND evaluation.action_applied
          AND NOT EXISTS(
              SELECT 1 FROM payment_provider_resume_executions resume
               WHERE resume.provider_id=late.provider_id AND resume.provider_version=late.provider_version
                 AND resume.executed_at>evaluation.evaluated_at))
 ORDER BY late.updated_at,late.id
 LIMIT $2`, automaticArchiveBlindReason, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ArchiveBlindRecoveryCandidate, 0)
	for rows.Next() {
		var item ArchiveBlindRecoveryCandidate
		if err := rows.Scan(&item.CaseID, &item.PaymentOrderID, &item.ProviderID, &item.ProviderVersion, &item.LastSuccessfulQueryAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RetractAutomaticallyMisclassifiedArchiveBlindCase(ctx context.Context, expected ArchiveBlindRecoveryCandidate) (bool, error) {
	if expected.CaseID == uuid.Nil || expected.PaymentOrderID == uuid.Nil || expected.ProviderID == "" || expected.ProviderVersion == "" {
		return false, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment-provider.archive-blind-recovery", expected.ProviderID+":"+expected.ProviderVersion); err != nil {
		return false, err
	}

	var lastSuccessfulQueryAt *time.Time
	err = tx.QueryRow(ctx, `
SELECT payment.last_successful_query_at
  FROM late_payment_review_cases late
  JOIN payment_orders payment ON payment.id=late.payment_order_id
 WHERE late.id=$1 AND late.payment_order_id=$2
   AND late.provider_id=$3 AND late.provider_version=$4
   AND late.review_status='PROVIDER_BLIND' AND late.blind_reason=$5
   AND late.provider_capability_snapshot @> '{"authoritative_unpaid_terminal":false}'::jsonb
   AND EXISTS(
       SELECT 1 FROM late_payment_review_evidence evidence
        WHERE evidence.case_id=late.id AND evidence.evidence_type='PROVIDER_BLIND'
          AND evidence.actor_id IS NULL AND evidence.payload_snapshot->>'reason'=$5)
   AND NOT EXISTS(
       SELECT 1 FROM payment_observation_gaps gap
        WHERE gap.provider_id=late.provider_id AND gap.provider_version=late.provider_version)
   AND NOT EXISTS(
       SELECT 1 FROM payment_stop_loss_evaluations evaluation
        WHERE evaluation.scope_type='PROVIDER'
          AND evaluation.provider_id=late.provider_id AND evaluation.provider_version=late.provider_version
          AND evaluation.action_applied
          AND NOT EXISTS(
              SELECT 1 FROM payment_provider_resume_executions resume
               WHERE resume.provider_id=late.provider_id AND resume.provider_version=late.provider_version
                 AND resume.executed_at>evaluation.evaluated_at))
 FOR UPDATE OF late,payment`, expected.CaseID, expected.PaymentOrderID, expected.ProviderID, expected.ProviderVersion, automaticArchiveBlindReason).Scan(&lastSuccessfulQueryAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if lastSuccessfulQueryAt == nil || expected.LastSuccessfulQueryAt != nil && !lastSuccessfulQueryAt.After(*expected.LastSuccessfulQueryAt) {
		return false, tx.Commit(ctx)
	}

	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE late_payment_review_cases SET review_status='REVIEW_REQUIRED_LATE_PAYMENT',blind_reason=NULL,last_query_at=$2,last_query_result='QUERY_SUCCEEDED',next_review_at=$3,updated_at=$2 WHERE id=$1 AND review_status='PROVIDER_BLIND' AND blind_reason=$4`, expected.CaseID, now, now.Add(24*time.Hour), automaticArchiveBlindReason)
	if err != nil || command.RowsAffected() == 0 {
		return false, err
	}
	control, err := tx.Exec(ctx, `
UPDATE payment_provider_controls control
   SET new_orders_enabled=TRUE,reason_code='AVAILABLE',reason='',version=version+1,updated_at=$3
 WHERE control.provider_id=$1 AND control.provider_version=$2
   AND NOT control.new_orders_enabled
   AND control.reason_code='PROVIDER_BLIND' AND control.reason=$4
   AND NOT EXISTS(
       SELECT 1 FROM late_payment_review_cases remaining
        WHERE remaining.provider_id=$1 AND remaining.provider_version=$2
          AND remaining.review_status='PROVIDER_BLIND')
   AND NOT EXISTS(
       SELECT 1 FROM payment_observation_gaps gap
        WHERE gap.provider_id=$1 AND gap.provider_version=$2)
   AND NOT EXISTS(
       SELECT 1 FROM payment_stop_loss_evaluations evaluation
        WHERE evaluation.scope_type='PROVIDER'
          AND evaluation.provider_id=$1 AND evaluation.provider_version=$2
          AND evaluation.action_applied
          AND NOT EXISTS(
              SELECT 1 FROM payment_provider_resume_executions resume
               WHERE resume.provider_id=$1 AND resume.provider_version=$2
                 AND resume.executed_at>evaluation.evaluated_at))`, expected.ProviderID, expected.ProviderVersion, now, automaticArchiveBlindReason)
	if err != nil {
		return false, err
	}
	providerRestored := control.RowsAffected() == 1
	if err := appendManualReviewEvent(ctx, tx, expected.CaseID, "PROVIDER_BLIND_RETRACTED", nil, map[string]any{
		"reason": automaticArchiveBlindReason, "provider_restored": providerRestored,
		"authoritative_unpaid_terminal": false, "fresh_query_at": lastSuccessfulQueryAt,
	}, now); err != nil {
		return false, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.provider-blind.retracted.v1", expected.CaseID, 1, map[string]any{
		"case_id": expected.CaseID, "payment_order_id": expected.PaymentOrderID,
		"provider_id": expected.ProviderID, "provider_version": expected.ProviderVersion,
		"provider_restored": providerRestored,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
