package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A persisted batch is recoverable. An error is never a completed receipt.
type reconciliationPendingError struct {
	RunID uuid.UUID
	Cause error
}

func (e *reconciliationPendingError) Error() string { return "reconciliation batch requires recovery" }
func (e *reconciliationPendingError) Unwrap() error { return e.Cause }

const reconciliationRunSelect = `SELECT id,provider_id,provider_version,channel,business_date::text,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at,completed_at FROM reconciliation_runs`

func scanReconciliationRun(row billingRowScanner, run *ReconciliationRun) error {
	return row.Scan(&run.ID, &run.ProviderID, &run.ProviderVersion, &run.Channel, &run.BusinessDate, &run.SourceMode, &run.RequestedBy, &run.InputDigest, &run.Status, &run.ProviderRowCount, &run.CaseCount, &run.StartedAt, &run.CompletedAt)
}

func (p *PaymentCoordinator) runReconciliationPipeline(ctx context.Context, channel string, date time.Time, rows []ProviderBillEntry, mode string, actor *uuid.UUID) (ReconciliationRun, error) {
	caps := p.Capabilities()
	if caps.ProviderID == "" || caps.Version == "" || len(rows) > 100000 {
		return ReconciliationRun{}, ErrConflict
	}
	start, end := reconciliationBusinessDateRange(date)
	for _, row := range rows {
		if row.PaidAt.Before(start) || !row.PaidAt.Before(end) {
			return ReconciliationRun{}, ErrConflict
		}
		if len(row.MerchantOrderNo) > 256 || len(row.ProviderReferenceID) > 256 || len(row.ChannelTransactionID) > 256 || len(row.Status) > 64 || len(row.Currency) > 16 || row.AmountFen <= 0 || !strings.EqualFold(row.Channel, channel) || billReference(row) == "" {
			return ReconciliationRun{}, ErrConflict
		}
		kind := normalizedBillEntryType(row)
		if kind != "PAYMENT" && kind != "REFUND" && kind != "CHARGEBACK" {
			return ReconciliationRun{}, ErrConflict
		}
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return ReconciliationRun{}, err
	}
	if len(encoded) > 64<<20 {
		return ReconciliationRun{}, ErrConflict
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return ReconciliationRun{}, err
	}
	defer tx.Rollback(ctx)
	key := caps.ProviderID + ":" + caps.Version + ":" + channel + ":" + date.Format("2006-01-02") + ":" + mode + ":" + digest
	if err := lockTransactionKey(ctx, tx, "payment-reconciliation", key); err != nil {
		return ReconciliationRun{}, err
	}
	var run ReconciliationRun
	if mode != "MANUAL_API" {
		err = scanReconciliationRun(tx.QueryRow(ctx, reconciliationRunSelect+` WHERE provider_id=$1 AND provider_version=$2 AND channel=$3 AND business_date=$4 AND source_mode=$5 AND input_digest=$6 ORDER BY started_at DESC,id DESC LIMIT 1`, caps.ProviderID, caps.Version, channel, date, mode, digest), &run)
		if err == nil {
			if mode == "SCHEDULED_API" && run.Status == "COMPLETED" {
				facts, e := p.auditedReconciliationFacts(ctx, tx, run, rows)
				if e != nil {
					return run, e
				}
				currentDigest, _, e := p.reconciliationObservationDigest(ctx, tx, run, rows, facts)
				if e != nil {
					return run, e
				}
				var savedDigest string
				e = tx.QueryRow(ctx, `SELECT facts_digest FROM reconciliation_observations WHERE id=(SELECT active_observation_id FROM reconciliation_runs WHERE id=$1)`, run.ID).Scan(&savedDigest)
				if e != nil && !errors.Is(e, pgx.ErrNoRows) {
					return run, e
				}
				// Historical completed runs predate immutable observations. Keep
				// their receipt intact and create a new verifiable observation.
				if errors.Is(e, pgx.ErrNoRows) || currentDigest != savedDigest {
					err = pgx.ErrNoRows
				}
			}
		}
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return run, err
			}
			if run.Status == "COMPLETED" {
				return run, nil
			}
			return p.resumeReconciliationRun(ctx, run.ID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return run, err
		}
	}
	now := p.now().UTC().Truncate(time.Microsecond)
	run = ReconciliationRun{ID: uuid.Must(uuid.NewV7()), ProviderID: caps.ProviderID, ProviderVersion: caps.Version, Channel: channel, BusinessDate: date.Format("2006-01-02"), SourceMode: mode, RequestedBy: actor, InputDigest: &digest, Status: "RUNNING", ProviderRowCount: len(rows), StartedAt: now}
	_, err = tx.Exec(ctx, `INSERT INTO reconciliation_runs(id,provider_id,provider_version,channel,business_date,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'RUNNING',$9,0,$10)`, run.ID, caps.ProviderID, caps.Version, channel, date, mode, actor, digest, len(rows), now)
	if err != nil {
		return run, err
	}
	values := make([][]any, 0, len(rows))
	for index, row := range rows {
		raw, _ := json.Marshal(row)
		rowDigest := sha256.Sum256(raw)
		evidenceKey := fmt.Sprintf("%s|%s|%s|%s", normalizedBillEntryType(row), billReference(row), strings.TrimSpace(row.MerchantOrderNo), row.PaidAt.UTC().Format(time.RFC3339Nano))
		values = append(values, []any{run.ID, index + 1, normalizedBillEntryType(row), billReference(row), row.ChannelTransactionID, row.MerchantOrderNo, channel, row.AmountFen, row.Currency, row.Status, row.PaidAt, raw, now, evidenceKey, hex.EncodeToString(rowDigest[:])})
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"reconciliation_provider_rows"}, []string{"reconciliation_run_id", "row_number", "entry_type", "provider_reference_id", "channel_transaction_id", "merchant_order_no", "channel", "amount_fen", "currency", "status", "paid_at", "raw_snapshot", "created_at", "pipeline_evidence_key", "pipeline_payload_digest"}, pgx.CopyFromRows(values)); err != nil {
		return run, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO reconciliation_row_progress(run_id,row_number) SELECT reconciliation_run_id,row_number FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1`, run.ID); err != nil {
		return run, err
	}
	if mode == "FILE_IMPORT" {
		if actor == nil {
			return run, ErrConflict
		}
		importID := uuid.Must(uuid.NewV7())
		if _, err = tx.Exec(ctx, `INSERT INTO provider_bill_imports(id,provider_id,provider_version,channel,business_date,payload_digest,row_count,actor_id,reconciliation_run_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, importID, caps.ProviderID, caps.Version, channel, date, digest, len(rows), *actor, run.ID, now); err != nil {
			return run, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO provider_bill_import_rows(import_id,row_number,merchant_order_no,provider_transaction_id,amount_fen,currency,status,paid_at,entry_type,provider_reference_id) SELECT $2,row_number,merchant_order_no,COALESCE(channel_transaction_id,''),amount_fen,currency,status,paid_at,entry_type,provider_reference_id FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1`, run.ID, importID); err != nil {
			return run, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return run, &reconciliationPendingError{run.ID, err}
	}
	return p.resumeReconciliationRun(ctx, run.ID)
}

func (p *PaymentCoordinator) reconciliationProviderRows(ctx context.Context, q reconciliationQuerier, runID uuid.UUID) ([]ProviderBillEntry, error) {
	stored, err := q.Query(ctx, `SELECT raw_snapshot FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1 ORDER BY row_number`, runID)
	if err != nil {
		return nil, err
	}
	defer stored.Close()
	result := []ProviderBillEntry{}
	for stored.Next() {
		var raw []byte
		if err := stored.Scan(&raw); err != nil {
			return nil, err
		}
		var row ProviderBillEntry
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, stored.Err()
}

func (p *PaymentCoordinator) resumeReconciliationRun(ctx context.Context, id uuid.UUID) (run ReconciliationRun, resultErr error) {
	if err := scanReconciliationRun(p.repository.pool.QueryRow(ctx, reconciliationRunSelect+` WHERE id=$1`, id), &run); err != nil {
		return run, err
	}
	if run.Status == "COMPLETED" {
		return run, nil
	}
	// Session lock serializes only this batch; no financial transaction is open.
	lease, err := p.repository.pool.Acquire(ctx)
	if err != nil {
		return run, &reconciliationPendingError{id, err}
	}
	defer lease.Release()
	leaseKey := "reconciliation-resume:" + id.String()
	var acquired bool
	if err := lease.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, leaseKey).Scan(&acquired); err != nil {
		return run, &reconciliationPendingError{id, err}
	}
	if !acquired {
		return run, &reconciliationPendingError{id, ErrConflict}
	}
	defer func() {
		release, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		if _, err := lease.Exec(release, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, leaseKey); err != nil {
			lease.Conn().Close(release)
		}
	}()
	// The first read precedes admission. A worker may have completed this run
	// while this connection was being acquired; completed receipts are frozen.
	if err := scanReconciliationRun(lease.QueryRow(ctx, reconciliationRunSelect+` WHERE id=$1`, id), &run); err != nil {
		return run, err
	}
	if run.Status == "COMPLETED" {
		return run, nil
	}
	caps := p.Capabilities()
	if run.ProviderID != caps.ProviderID || run.ProviderVersion != caps.Version || !caps.SupportsChannel(run.Channel) {
		return run, &reconciliationPendingError{id, ErrConflict}
	}
	defer func() {
		if resultErr != nil {
			failed, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			_, _ = p.repository.pool.Exec(failed, `UPDATE reconciliation_runs SET status='FAILED',completed_at=NULL,error_message='RECOVERY_REQUIRED',next_attempt_at=clock_timestamp()+interval '1 minute' WHERE id=$1 AND status<>'COMPLETED'`, id)
			resultErr = &reconciliationPendingError{id, resultErr}
		}
	}()
	admitted, err := lease.Exec(ctx, `UPDATE reconciliation_runs SET status='RUNNING',completed_at=NULL,error_message=NULL WHERE id=$1 AND status<>'COMPLETED'`, id)
	if err != nil {
		return run, err
	}
	if admitted.RowsAffected() != 1 {
		return run, ErrConflict
	}
	providerRows, err := p.reconciliationProviderRows(ctx, p.repository.pool, id)
	if err != nil {
		return run, err
	}
	if err := p.persistReadOnlyBillEvidence(ctx, run); err != nil {
		return run, err
	}
	progressRows, err := p.repository.pool.Query(ctx, `SELECT row_number FROM reconciliation_row_progress WHERE run_id=$1 AND processed_at IS NOT NULL`, id)
	if err != nil {
		return run, err
	}
	processedRows := map[int]bool{}
	for progressRows.Next() {
		var index int
		if err := progressRows.Scan(&index); err != nil {
			progressRows.Close()
			return run, err
		}
		processedRows[index] = true
	}
	err = progressRows.Err()
	progressRows.Close()
	if err != nil {
		return run, err
	}
	var deferredErr error
	for index, row := range providerRows {
		if processedRows[index+1] {
			continue
		}
		confirmed, err := p.confirmBillPaymentFacts(ctx, []ProviderBillEntry{row})
		if err != nil {
			if ctx.Err() != nil {
				return run, ctx.Err()
			}
			deferredErr = err
			continue
		}
		err = reconciliationRetry(ctx, func() error {
			tx, err := p.repository.pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			var done bool
			if err := tx.QueryRow(ctx, `SELECT processed_at IS NOT NULL FROM reconciliation_row_progress WHERE run_id=$1 AND row_number=$2 FOR UPDATE`, id, index+1).Scan(&done); err != nil {
				return err
			}
			if done {
				return tx.Commit(ctx)
			}
			if err := p.mergeProviderBillEvidenceWithinTransaction(ctx, tx, row, confirmed[billPaymentKey(row)], p.now()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE reconciliation_row_progress SET processed_at=$3 WHERE run_id=$1 AND row_number=$2`, id, index+1, p.now()); err != nil {
				return err
			}
			return tx.Commit(ctx)
		})
		if err != nil {
			return run, err
		}
	}
	if deferredErr != nil {
		return run, deferredErr
	}
	if err := p.finalizeReconciliationObservation(ctx, run, providerRows); err != nil {
		return run, err
	}
	err = scanReconciliationRun(p.repository.pool.QueryRow(ctx, reconciliationRunSelect+` WHERE id=$1`, id), &run)
	return run, err
}

func reconciliationRetry(ctx context.Context, action func() error) error {
	for attempt := 0; attempt < 4; attempt++ {
		err := action()
		if err == nil {
			return nil
		}
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || (pgerr.Code != "40001" && pgerr.Code != "40P01") || attempt == 3 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ErrConflict
}

func (p *PaymentCoordinator) persistStableReconciliationCase(ctx context.Context, tx pgx.Tx, run ReconciliationRun, difference ReconciliationDifference) error {
	providerJSON := json.RawMessage(`{}`)
	if difference.Provider != nil {
		providerJSON, _ = json.Marshal(difference.Provider)
	}
	internalJSON := difference.InternalSnapshot
	if len(internalJSON) == 0 {
		internalJSON = json.RawMessage(`{}`)
	}
	refundJSON := difference.RefundSnapshot
	if len(refundJSON) == 0 {
		refundJSON = json.RawMessage(`{}`)
	}
	ledgerJSON := difference.LedgerSnapshot
	if len(ledgerJSON) == 0 {
		ledgerJSON = json.RawMessage(`{}`)
	}
	digest := reconciliationDifferenceDigest(run, difference)
	if err := lockTransactionKey(ctx, tx, "reconciliation-case-fact", digest); err != nil {
		return err
	}
	var caseID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM reconciliation_cases WHERE fact_digest=$1`, digest).Scan(&caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		caseID = uuid.Must(uuid.NewV7())
		now := p.now().UTC()
		claim, response, mitigation, resolution := paymentAnomalyDeadlines(now, difference.Severity)
		if _, err = tx.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'BILLING','RECONCILIATION_DIFFERENCE','RECONCILIATION_CASE',$1,$2,'OPEN',$3,$4,$5,$6,$7,$3)`, caseID, difference.Severity, now, claim, response, mitigation, resolution); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,severity,provider_snapshot,internal_snapshot,refund_snapshot,ledger_snapshot,manual_review_case_id,created_at,updated_at,fact_digest) VALUES($1,$2,$3,$4,$5,'OPEN',$6,$7,$8,$9,$10,$1,$11,$11,$12)`, caseID, run.ID, difference.PaymentOrderID, difference.MerchantOrderNo, difference.CaseType, difference.Severity, providerJSON, internalJSON, refundJSON, ledgerJSON, now, digest); err != nil {
			return err
		}
		if err = appendManualReviewEvent(ctx, tx, caseID, "CREATED", run.RequestedBy, map[string]any{"case_type": difference.CaseType, "reconciliation_run_id": run.ID}, now); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM reconciliation_cases WHERE id=$1 FOR UPDATE`, caseID).Scan(&status); err != nil {
		return err
	}
	if status == "RESOLVED" {
		now := p.now().UTC()
		claim, response, mitigation, resolution := paymentAnomalyDeadlines(now, difference.Severity)
		if _, err := tx.Exec(ctx, `UPDATE reconciliation_cases SET status='OPEN',owner_id=NULL,repair_action=NULL,review_result=NULL,resolution=NULL,resolved_at=NULL,version=version+1,updated_at=$2 WHERE id=$1`, caseID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE manual_review_cases SET status='OPEN',owner_id=NULL,claimed_at=NULL,first_response_at=NULL,mitigation_at=NULL,resolved_at=NULL,claim_due_at=$3,first_response_due_at=$4,mitigation_due_at=$5,resolution_due_at=$6,updated_at=$2 WHERE id=$1`, caseID, now, claim, response, mitigation, resolution); err != nil {
			return err
		}
		if err := appendManualReviewEvent(ctx, tx, caseID, "REOPEN", run.RequestedBy, map[string]any{"reason": "VERIFIED_DIFFERENCE_RECURRED", "reconciliation_run_id": run.ID}, now); err != nil {
			return err
		}
	}
	// IGNORED is an explicit acceptance of this exact immutable fact digest. A
	// changed monetary/source fact has another digest and creates a fresh case.
	_, err = tx.Exec(ctx, `INSERT INTO reconciliation_case_runs(run_id,case_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, run.ID, caseID)
	return err
}

// Recovery consumes persisted input and re-queries unfinished payment authority.
func (p *PaymentCoordinator) recoverReconciliationRuns(ctx context.Context) error {
	rows, err := p.repository.pool.Query(ctx, `SELECT id FROM reconciliation_runs WHERE status IN ('RUNNING','FAILED') AND provider_id=$1 AND provider_version=$2 AND (next_attempt_at IS NULL OR next_attempt_at<=clock_timestamp()) ORDER BY next_attempt_at NULLS FIRST,started_at,id LIMIT 10`, p.Capabilities().ProviderID, p.Capabilities().Version)
	if err != nil {
		return err
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var deferred error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slice, cancel := context.WithTimeout(ctx, reconciliationRecoverySlice)
		_, err := p.resumeReconciliationRun(slice, id)
		cancel()
		if err != nil {
			deferred = err
		}
	}
	return deferred
}

// Pure bill observations do not acquire payment or wallet locks. Their immutable
// snapshots and progress are committed together in bounded batches.
func (p *PaymentCoordinator) persistReadOnlyBillEvidence(ctx context.Context, run ReconciliationRun) error {
	for first := 1; first <= run.ProviderRowCount; first += 2000 {
		last := first + 1999
		err := reconciliationRetry(ctx, func() error {
			tx, err := p.repository.pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, `WITH eligible AS MATERIALIZED (
 SELECT row.*,CASE WHEN payment.provider_id=$4 AND payment.provider_version=$5 THEN payment.id END matched_order_id,
 transaction.id matched_transaction_id
 FROM reconciliation_provider_rows row
 JOIN reconciliation_row_progress progress ON progress.run_id=row.reconciliation_run_id AND progress.row_number=row.row_number
 LEFT JOIN payment_orders payment ON payment.merchant_order_no=row.merchant_order_no
 LEFT JOIN payment_transactions transaction ON transaction.payment_order_id=payment.id AND transaction.provider_id=$4 AND transaction.provider_version=$5
 AND transaction.provider_transaction_id=row.channel_transaction_id AND transaction.amount_fen=row.amount_fen AND transaction.currency=row.currency AND transaction.channel=row.channel AND transaction.status IN ('PAID','LATE_PAID')
 WHERE row.reconciliation_run_id=$1 AND row.row_number BETWEEN $2 AND $3 AND progress.processed_at IS NULL
 AND (row.entry_type<>'PAYMENT' OR upper(trim(row.status)) NOT IN ('SUCCESS','PAID','COMPLETED') OR payment.id IS NULL OR payment.provider_id<>$4 OR payment.provider_version<>$5 OR payment.fulfillment_status='CREDITED')
 ), evidence AS (
 INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
 SELECT gen_random_uuid(),matched_order_id,matched_transaction_id,'BILL',$4,$5,pipeline_evidence_key,merchant_order_no,NULLIF(channel_transaction_id,''),amount_fen,currency,status,paid_at,$6,pipeline_payload_digest,raw_snapshot,
 CASE WHEN entry_type='PAYMENT' AND matched_transaction_id IS NOT NULL THEN 'DUPLICATE' ELSE 'REVIEW_REQUIRED' END FROM eligible
 ON CONFLICT(provider_id,provider_version,evidence_type,evidence_key,payload_digest) DO NOTHING RETURNING id
 )
 UPDATE reconciliation_row_progress progress SET processed_at=$6 FROM eligible WHERE progress.run_id=$1 AND progress.row_number=eligible.row_number`, run.ID, first, last, run.ProviderID, run.ProviderVersion, p.now())
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *PaymentCoordinator) prepareReconciliationResolution(ctx context.Context, id uuid.UUID, command *ReconciliationCaseCommand) error {
	var exists bool
	if err := p.repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reconciliation_case_commands WHERE reconciliation_case_id=$1 AND idempotency_key=$2)`, id, command.IdempotencyKey).Scan(&exists); err != nil {
		return err
	}
	// The repository validates actor/hash and returns the immutable saved receipt.
	if exists {
		return nil
	}
	if command.ActorID == uuid.Nil || len(strings.TrimSpace(command.IdempotencyKey)) < 8 || len(command.IdempotencyKey) > 120 || len([]rune(strings.TrimSpace(command.Reason))) < 10 || strings.TrimSpace(command.ReviewResult) == "" {
		return ErrConflict
	}
	var owner *uuid.UUID
	var status string
	if err := p.repository.pool.QueryRow(ctx, `SELECT owner_id,status FROM reconciliation_cases WHERE id=$1`, id).Scan(&owner, &status); err != nil {
		return err
	}
	if owner == nil || *owner != command.ActorID || (status != "OPEN" && status != "IN_REVIEW") {
		return ErrConflict
	}
	var origin ReconciliationRun
	var version int64
	if err := p.repository.pool.QueryRow(ctx, `SELECT run.provider_id,run.provider_version,run.channel,run.business_date::text,c.version FROM reconciliation_cases c JOIN reconciliation_runs run ON run.id=c.reconciliation_run_id WHERE c.id=$1`, id).Scan(&origin.ProviderID, &origin.ProviderVersion, &origin.Channel, &origin.BusinessDate, &version); err != nil {
		return err
	}
	caps := p.Capabilities()
	if caps.ProviderID != origin.ProviderID || caps.Version != origin.ProviderVersion || !caps.BillSupported {
		return ErrConflict
	}
	date, err := time.Parse("2006-01-02", origin.BusinessDate)
	if err != nil {
		return err
	}
	run, err := p.runProviderReconciliation(ctx, origin.Channel, date, "MANUAL_API", &command.ActorID)
	if err != nil {
		return err
	}
	command.verificationRunID = run.ID
	command.verificationCaseVersion = version
	return nil
}

func (r *PostgresRepository) validateReconciliationResolution(ctx context.Context, tx pgx.Tx, item AdminReconciliationCase, command ReconciliationCaseCommand) error {
	if command.verificationRunID == uuid.Nil || command.verificationCaseVersion != item.Version {
		return ErrConflict
	}
	var verification, origin ReconciliationRun
	if err := scanReconciliationRun(tx.QueryRow(ctx, reconciliationRunSelect+` WHERE id=$1`, command.verificationRunID), &verification); err != nil {
		return err
	}
	if err := scanReconciliationRun(tx.QueryRow(ctx, reconciliationRunSelect+` WHERE id=$1`, item.RunID), &origin); err != nil {
		return err
	}
	if verification.Status != "COMPLETED" || verification.ProviderID != origin.ProviderID || verification.ProviderVersion != origin.ProviderVersion || verification.Channel != origin.Channel || verification.BusinessDate != origin.BusinessDate || verification.StartedAt.Before(r.now().Add(-time.Minute)) {
		return ErrConflict
	}
	date, err := time.Parse("2006-01-02", origin.BusinessDate)
	if err != nil {
		return err
	}
	start, end := reconciliationBusinessDateRange(date)
	var providerRows []ProviderBillEntry
	stored, err := tx.Query(ctx, `SELECT raw_snapshot FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1 ORDER BY row_number`, verification.ID)
	if err != nil {
		return err
	}
	for stored.Next() {
		var raw []byte
		if err := stored.Scan(&raw); err != nil {
			stored.Close()
			return err
		}
		var row ProviderBillEntry
		if err := json.Unmarshal(raw, &row); err != nil {
			stored.Close()
			return err
		}
		providerRows = append(providerRows, row)
	}
	err = stored.Err()
	stored.Close()
	if err != nil {
		return err
	}
	facts, err := listFourWayFactsForProvider(ctx, tx, origin.Channel, start, end, origin.ProviderID, origin.ProviderVersion, providerRows)
	if err != nil {
		return err
	}
	if err := auditFourWayReconciliationFacts(ctx, tx, facts); err != nil {
		return err
	}
	for _, difference := range CompareFourWayReconciliation(providerRows, facts) {
		if difference.MerchantOrderNo == item.MerchantOrderNo {
			return ErrConflict
		}
	}
	// Any newer completed provider observation invalidates an earlier proof.
	var newer bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reconciliation_runs WHERE provider_id=$1 AND provider_version=$2 AND channel=$3 AND business_date=$4::date AND status='COMPLETED' AND (started_at,id)>($5,$6))`, origin.ProviderID, origin.ProviderVersion, origin.Channel, origin.BusinessDate, verification.StartedAt, verification.ID).Scan(&newer); err != nil {
		return err
	}
	if newer {
		return ErrConflict
	}
	return nil
}

func reconciliationDifferenceDigest(run ReconciliationRun, difference ReconciliationDifference) string {
	var providerIdentity any
	if difference.Provider != nil {
		row := difference.Provider
		providerIdentity = []any{normalizedBillEntryType(*row), billReference(*row), row.ChannelTransactionID, row.AmountFen, row.Currency, row.Status}
	}
	var internalIdentity any
	if len(difference.InternalSnapshot) > 0 {
		var snapshot struct {
			Transaction struct {
				ID       string `json:"id"`
				Amount   int64  `json:"amount_fen"`
				Currency string `json:"currency"`
			} `json:"payment_transaction"`
		}
		_ = json.Unmarshal(difference.InternalSnapshot, &snapshot)
		internalIdentity = snapshot.Transaction
	}
	payload, _ := json.Marshal([]any{run.ProviderID, run.ProviderVersion, run.Channel, run.BusinessDate, difference.CaseType, difference.MerchantOrderNo, difference.PaymentOrderID, providerIdentity, internalIdentity, difference.InternalEntryType, difference.InternalReference, difference.InternalAmountFen, difference.InternalCurrency, difference.InternalDateDigest, difference.IntegrityDigest})
	sum := sha256.Sum256(payload)
	digest := fmt.Sprintf("%x", sum)
	return digest
}

func reconciliationFactsDigest(facts []ReconciliationInternalFact) string {
	identities := make([]any, 0, len(facts))
	for _, fact := range facts {
		identities = append(identities, []any{fact.EntryType, fact.ProviderReferenceID, fact.PaymentOrderID, fact.MerchantOrderNo, fact.AmountFen, fact.Currency, fact.LedgerPresent, fact.IntegrityCaseType, fact.IntegrityDigest, fact.EventDateVerified, fact.EventDateConflict, fact.EventDateDigest})
	}
	raw, _ := json.Marshal(identities)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum)
}

// Include existing order bindings even before a transaction has been credited.
// Provider-only missing facts must be reobserved when their internal order arrives.
func (p *PaymentCoordinator) reconciliationObservationDigest(ctx context.Context, q reconciliationQuerier, run ReconciliationRun, rows []ProviderBillEntry, facts []ReconciliationInternalFact) (string, []byte, error) {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.MerchantOrderNo)
	}
	var bindings []byte
	result, err := q.Query(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(id,merchant_order_no,wallet_id,provider_id,provider_version,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,primary_provider_transaction_id,requester_id,product_id,product_snapshot->'base_points',product_snapshot->'bonus_points',payer_user_id) ORDER BY merchant_order_no),'[]'::jsonb) FROM payment_orders WHERE merchant_order_no=ANY($1::text[])`, names)
	if err != nil {
		return "", nil, err
	}
	defer result.Close()
	if !result.Next() {
		return "", nil, result.Err()
	}
	if err := result.Scan(&bindings); err != nil {
		return "", nil, err
	}
	raw, _ := json.Marshal([]any{reconciliationFactsDigest(facts), json.RawMessage(bindings)})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum), bindings, nil
}

// Provider-only discrepancies still have an immutable financial order binding.
// Ignore acceptance cannot silently extend to a newly arrived or changed source.
func bindProviderOnlyReconciliationDifferences(differences []ReconciliationDifference, raw []byte, run ReconciliationRun) error {
	var bindings [][]json.RawMessage
	if err := json.Unmarshal(raw, &bindings); err != nil {
		return err
	}
	byMerchant := map[string][]json.RawMessage{}
	for _, binding := range bindings {
		if len(binding) != 17 {
			return ErrConflict
		}
		var merchant, provider, version, channel string
		if json.Unmarshal(binding[1], &merchant) != nil || json.Unmarshal(binding[3], &provider) != nil || json.Unmarshal(binding[4], &version) != nil || json.Unmarshal(binding[5], &channel) != nil {
			return ErrConflict
		}
		if provider == run.ProviderID && version == run.ProviderVersion && channel == run.Channel {
			byMerchant[merchant] = binding
		}
	}
	for index := range differences {
		difference := &differences[index]
		if difference.PaymentOrderID != nil {
			continue
		}
		binding, ok := byMerchant[difference.MerchantOrderNo]
		if !ok {
			continue
		}
		var id uuid.UUID
		if err := json.Unmarshal(binding[0], &id); err != nil {
			return err
		}
		stable := map[string]json.RawMessage{}
		keys := []string{"id", "merchant_order_no", "wallet_id", "provider_id", "provider_version", "channel", "amount_fen", "currency", "credit_microcredits", "", "", "primary_provider_transaction_id", "requester_id", "product_id", "base_points", "bonus_points", "payer_user_id"}
		for index, key := range keys {
			if key != "" {
				stable[key] = binding[index]
			}
		}
		evidence, err := json.Marshal(map[string]any{"order_binding": stable})
		if err != nil {
			return err
		}
		hash := sha256.Sum256(evidence)
		difference.PaymentOrderID = &id
		difference.InternalSnapshot = evidence
		difference.IntegrityDigest = fmt.Sprintf("%x", hash)
	}
	return nil
}

func (p *PaymentCoordinator) auditedReconciliationFacts(ctx context.Context, q reconciliationQuerier, run ReconciliationRun, providerRows []ProviderBillEntry) ([]ReconciliationInternalFact, error) {
	date, err := time.Parse("2006-01-02", run.BusinessDate)
	if err != nil {
		return nil, err
	}
	start, end := reconciliationBusinessDateRange(date)
	facts, err := listFourWayFactsForProvider(ctx, q, run.Channel, start, end, run.ProviderID, run.ProviderVersion, providerRows)
	if err != nil {
		return nil, err
	}
	if err := auditFourWayReconciliationFacts(ctx, q, facts); err != nil {
		return nil, err
	}
	return facts, nil
}
func (p *PaymentCoordinator) finalizeReconciliationObservation(ctx context.Context, run ReconciliationRun, providerRows []ProviderBillEntry) error {
	var observationID uuid.UUID
	var facts []ReconciliationInternalFact
	var factsDigest string
	var sourceBindings []byte
	// This snapshot transaction copies audit evidence, never locks financial rows.
	err := reconciliationRetry(ctx, func() error {
		tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		facts, err = p.auditedReconciliationFacts(ctx, tx, run, providerRows)
		if err != nil {
			return err
		}
		factsDigest, sourceBindings, err = p.reconciliationObservationDigest(ctx, tx, run, providerRows, facts)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT id FROM reconciliation_observations WHERE run_id=$1 AND facts_digest=$2`, run.ID, factsDigest).Scan(&observationID)
		if errors.Is(err, pgx.ErrNoRows) {
			observationID = uuid.Must(uuid.NewV7())
			if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_observations(id,run_id,facts_digest,created_at,source_bindings) VALUES($1,$2,$3,$4,$5)`, observationID, run.ID, factsDigest, p.now(), sourceBindings); err != nil {
				return err
			}
			values := make([][]any, 0, len(facts))
			for index, fact := range facts {
				values = append(values, []any{observationID, index + 1, fact.EntryType, fact.ProviderReferenceID, fact.PaymentOrderID, fact.AmountFen, fact.Currency, nullableReconciliationJSON(fact.InternalSnapshot), nullableReconciliationJSON(fact.RefundSnapshot), nullableReconciliationJSON(fact.LedgerSnapshot), fact.IntegrityCaseType, fact.IntegrityDigest, fact.EventDateVerified, fact.EventDateConflict, fact.EventDateDigest, nullableReconciliationJSON(fact.EventDateSnapshot)})
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"reconciliation_observation_facts"}, []string{"observation_id", "row_number", "entry_type", "provider_reference_id", "payment_order_id", "amount_fen", "currency", "internal_snapshot", "refund_snapshot", "ledger_snapshot", "integrity_case_type", "integrity_digest", "event_date_verified", "event_date_conflict", "event_date_digest", "event_date_snapshot"}, pgx.CopyFromRows(values)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE reconciliation_runs SET active_observation_id=$2 WHERE id=$1`, run.ID, observationID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return err
	}
	differences := CompareFourWayReconciliation(providerRows, facts)
	if err := bindProviderOnlyReconciliationDifferences(differences, sourceBindings, run); err != nil {
		return err
	}
	unique := make([]ReconciliationDifference, 0, len(differences))
	seen := map[string]bool{}
	for _, difference := range differences {
		key := reconciliationDifferenceDigest(run, difference)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, difference)
		}
	}
	observed, err := p.repository.pool.Query(ctx, `SELECT fact_digest FROM reconciliation_observation_progress WHERE observation_id=$1`, observationID)
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for observed.Next() {
		var key string
		if err := observed.Scan(&key); err != nil {
			observed.Close()
			return err
		}
		done[key] = true
	}
	err = observed.Err()
	observed.Close()
	if err != nil {
		return err
	}
	for first := 0; first < len(unique); first += 100 {
		last := first + 100
		if last > len(unique) {
			last = len(unique)
		}
		err := reconciliationRetry(ctx, func() error {
			tx, err := p.repository.pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			for _, difference := range unique[first:last] {
				key := reconciliationDifferenceDigest(run, difference)
				if done[key] {
					continue
				}
				if err := p.persistStableReconciliationCase(ctx, tx, run, difference); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_observation_progress(observation_id,fact_digest,case_id) SELECT $1,$2,id FROM reconciliation_cases WHERE fact_digest=$2 ON CONFLICT DO NOTHING`, observationID, key); err != nil {
					return err
				}
			}
			return tx.Commit(ctx)
		})
		if err != nil {
			return err
		}
	}
	return reconciliationRetry(ctx, func() error {
		tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		current, err := p.auditedReconciliationFacts(ctx, tx, run, providerRows)
		if err != nil {
			return err
		}
		currentDigest, _, err := p.reconciliationObservationDigest(ctx, tx, run, providerRows, current)
		if err != nil {
			return err
		}
		if currentDigest != factsDigest {
			return ErrConflict
		}
		var completedCases int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_observation_progress WHERE observation_id=$1`, observationID).Scan(&completedCases); err != nil {
			return err
		}
		if completedCases != len(unique) {
			return ErrConflict
		}
		result, err := tx.Exec(ctx, `UPDATE reconciliation_runs SET status='COMPLETED',case_count=$2,completed_at=$3,error_message=NULL,next_attempt_at=NULL WHERE id=$1 AND active_observation_id=$4`, run.ID, len(unique), p.now().UTC().Truncate(time.Microsecond), observationID)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrConflict
		}
		return tx.Commit(ctx)
	})
}
func nullableReconciliationJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
