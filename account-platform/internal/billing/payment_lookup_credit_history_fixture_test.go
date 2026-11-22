package billing

// Historical-only F12 writer frozen from main f4e8f785fb76a999adc8f5ce068ffa8d1421504b.
// Its old merge/Apply bodies below keep the original financial and receipt logic.
// This file is not shipped; its entry verifies exactly F12 baseline16 or
// the public38 carrier used for the deployed-boundary upgrade regression. Current
// API tests always use ApplyPaymentFinancialRepair after full public migration.
import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func requireHistoricalF12RepairSchema(ctx context.Context, repository *PostgresRepository, through int64) error {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		return err
	}
	var historical []migrations.Migration
	if through == 900016 {
		historical, err = selectF12PaymentLookupStage(loaded, through)
	} else if through == 900038 {
		// Public carrier representing the observed deployed upper boundary, not
		// a claim that its complete membership/checksums were observed on the server.
		for _, migration := range loaded {
			if migration.Version <= through {
				historical = append(historical, migration)
			}
		}
	} else {
		return errors.New("unregistered historical F12 writer boundary")
	}
	if err != nil {
		return err
	}
	connection, err := repository.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	if _, err := migrations.VerifyApplied(ctx, connection.Conn(), historical); err != nil {
		return err
	}
	var absent bool
	if err := connection.QueryRow(ctx, `SELECT to_regclass('public.payment_credit_attempts') IS NULL`).Scan(&absent); err != nil {
		return err
	}
	if !absent {
		return errors.New("historical F12 writer requires its registered pre-admission schema")
	}
	expected := make(map[int64]string, len(historical))
	for _, migration := range historical {
		expected[migration.Version] = migration.Name
	}
	rows, err := connection.Query(ctx, `SELECT version,name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var version int64
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return err
		}
		if expected[version] != name {
			return errors.New("historical F12 ledger name differs")
		}
	}
	return rows.Err()
}

func (p *PaymentCoordinator) mergeHistoricalF12Payment(ctx context.Context, tx pgx.Tx, order PaymentOrder, evidenceType, evidenceKey string, payload []byte, provider ProviderOrder, repair bool, now time.Time) (PaymentOrder, uuid.UUID, error) {
	if provider.Status != "SUCCESS" || provider.ChannelTransactionID == nil || provider.PaidAt == nil {
		return order, uuid.Nil, ErrConflict
	}
	valid := providerMatchesOrder(provider, order)
	if !valid {
		evidenceID, evidenceErr := p.insertPaymentEvidence(ctx, tx, order, nil, evidenceType, evidenceKey, "REVIEW_REQUIRED", provider, payload, now)
		if evidenceErr != nil {
			return order, uuid.Nil, evidenceErr
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET attention_summary='REVIEW_REQUIRED',status=CASE WHEN status IN ('CREATED','PENDING','EXPIRED') THEN 'MANUAL_REVIEW' ELSE status END,fulfillment_status=CASE WHEN fulfillment_status='PENDING' THEN 'MANUAL_REVIEW' ELSE fulfillment_status END,payment_url=NULL,qr_data=NULL,summary_version=summary_version+1,updated_at=$2 WHERE id=$1`, order.ID, now); err != nil {
			return order, uuid.Nil, err
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "reason": "order channel amount or currency mismatch"}, now); err != nil {
			return order, uuid.Nil, err
		}
		return order, evidenceID, ErrConflict
	}

	var transaction PaymentTransaction
	err := tx.QueryRow(ctx, `SELECT id,payment_order_id,provider_id,provider_version,provider_transaction_id,channel,status,fulfillment_status,amount_fen,currency,COALESCE(provider_status,''),provider_paid_at,verified_at,fulfilled_at
FROM payment_transactions WHERE provider_transaction_id=$1 FOR NO KEY UPDATE`, *provider.ChannelTransactionID).Scan(
		&transaction.ID, &transaction.PaymentOrderID, &transaction.ProviderID, &transaction.ProviderVersion,
		&transaction.ProviderTransactionID, &transaction.Channel, &transaction.Status, &transaction.FulfillmentStatus,
		&transaction.AmountFen, &transaction.Currency, &transaction.ProviderStatus, &transaction.PaidAt,
		&transaction.VerifiedAt, &transaction.FulfilledAt)
	if err == nil {
		mergeResult := "DUPLICATE"
		if transaction.PaymentOrderID != order.ID || transaction.ProviderID != order.ProviderID || transaction.ProviderVersion != order.ProviderVersion || transaction.AmountFen != order.AmountFen || transaction.Currency != order.Currency {
			mergeResult = "REVIEW_REQUIRED"
		}
		var evidenceTransactionID *uuid.UUID
		if transaction.PaymentOrderID == order.ID {
			evidenceTransactionID = &transaction.ID
		}
		evidenceID, evidenceErr := p.insertPaymentEvidence(ctx, tx, order, evidenceTransactionID, evidenceType, evidenceKey, mergeResult, provider, payload, now)
		if evidenceErr != nil {
			return order, uuid.Nil, evidenceErr
		}
		if mergeResult == "REVIEW_REQUIRED" {
			if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "PAYMENT_FACT_CONFLICT", "P0", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "reason": "provider transaction conflicts with an existing owner or immutable snapshot"}, now); err != nil {
				return order, uuid.Nil, err
			}
			return order, evidenceID, ErrConflict
		}
		if repair && transaction.FulfillmentStatus == "REVIEW_REQUIRED" {
			if err := p.ensureCollectionAllowedWithinTransaction(ctx, tx, order); err != nil {
				return order, evidenceID, err
			}
			packageID, err := issuePackageWithinTransaction(ctx, tx, order, transaction.ID, now, true)
			if err != nil {
				return order, evidenceID, err
			}
			resolvedStatus := "PAID"
			if transaction.PaidAt != nil && (!order.ExpiresAt.After(*transaction.PaidAt) || (order.CloseStatus != "" && order.CloseStatus != "OPEN")) {
				resolvedStatus = "LATE_PAID"
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_transactions SET status=$2,fulfillment_status='CREDITED',fulfilled_at=$3,updated_at=$3,evidence_version=evidence_version+1 WHERE id=$1 AND fulfillment_status='REVIEW_REQUIRED'`, transaction.ID, resolvedStatus, now); err != nil {
				return order, evidenceID, err
			}
			if err := events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", transaction.ID, 1, map[string]any{
				"order_id": order.ID, "payment_transaction_id": transaction.ID, "entitlement_package_id": packageID,
				"payer_user_id": order.RequesterID, "wallet_id": order.WalletID,
				"beneficiary_organization_id": order.BeneficiaryOrganizationID, "channel": order.Channel,
				"amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits,
			}); err != nil {
				return order, evidenceID, err
			}
		}
		order, err = projectPrePrincipalFixtureOrder(ctx, tx, order.ID, now)
		return order, evidenceID, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return order, uuid.Nil, err
	}

	if err := p.ensureCollectionAllowedWithinTransaction(ctx, tx, order); err != nil {
		return order, uuid.Nil, err
	}
	var priorTransactions int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1`, order.ID).Scan(&priorTransactions); err != nil {
		return order, uuid.Nil, err
	}
	transaction.ID = uuid.Must(uuid.NewV7())
	transactionStatus, fulfillmentStatus, requiresReview := classifyPaymentTransactionAfterClose(order.ExpiresAt, *provider.PaidAt, order.CloseStatus, p.Policy(), priorTransactions, repair)
	_, err = tx.Exec(ctx, `INSERT INTO payment_transactions(
id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,status,fulfillment_status,
amount_fen,currency,source,provider_status,provider_paid_at,verified_at,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14,$14,$14)`,
		transaction.ID, order.ID, order.ProviderID, order.ProviderVersion, order.Channel, *provider.ChannelTransactionID,
		transactionStatus, fulfillmentStatus, order.AmountFen, order.Currency, evidenceType, provider.ProviderStatus,
		provider.PaidAt, now)
	if err != nil {
		return order, uuid.Nil, err
	}
	mergeResult := "MERGED"
	if requiresReview {
		mergeResult = "REVIEW_REQUIRED"
	}
	evidenceID, err := p.insertPaymentEvidence(ctx, tx, order, &transaction.ID, evidenceType, evidenceKey, mergeResult, provider, payload, now)
	if err != nil {
		return order, uuid.Nil, err
	}
	if requiresReview {
		anomalyType := "LATE_PAYMENT_CONFLICT"
		if priorTransactions > 0 {
			anomalyType = "DUPLICATE_PAYMENT"
		}
		if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, anomalyType, "P1", "payment-evidence:"+evidenceID.String(), paymentAnomalyReference{PaymentOrderID: &order.ID, PaymentTransactionID: &transaction.ID, PaymentEvidenceID: &evidenceID}, map[string]any{"evidence_type": evidenceType, "provider_transaction_id": provider.ChannelTransactionID, "provider_paid_at": provider.PaidAt, "prior_transaction_count": priorTransactions}, now); err != nil {
			return order, uuid.Nil, err
		}
	}
	if !requiresReview {
		packageID, err := issuePackageWithinTransaction(ctx, tx, order, transaction.ID, now, false)
		if err != nil {
			return order, evidenceID, err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_transactions SET fulfillment_status='CREDITED',fulfilled_at=$2,updated_at=$2 WHERE id=$1`, transaction.ID, now); err != nil {
			return order, evidenceID, err
		}
		if err := events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", transaction.ID, 1, map[string]any{
			"order_id": order.ID, "payment_transaction_id": transaction.ID, "entitlement_package_id": packageID,
			"payer_user_id": order.RequesterID, "wallet_id": order.WalletID,
			"beneficiary_organization_id": order.BeneficiaryOrganizationID, "channel": order.Channel,
			"amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits,
		}); err != nil {
			return order, evidenceID, err
		}
	}
	if err := reopenArchivedLatePaymentCase(ctx, tx, order.ID, now); err != nil {
		return order, evidenceID, err
	}
	order, err = projectPrePrincipalFixtureOrder(ctx, tx, order.ID, now)
	return order, evidenceID, err
}

func (p *PaymentCoordinator) applyHistoricalF12PaymentRepair(ctx context.Context, input PaymentFinancialRepairExecutionInput, through int64) (PaymentFinancialRepairResult, error) {
	executionKey := strings.TrimSpace(input.ExecutionKey)
	if p == nil || p.repository == nil || p.repository.pool == nil || input.ApprovalRequestID == uuid.Nil ||
		input.AuthorizationEventID == uuid.Nil || input.ApprovalEpoch < 1 || executionKey == "" || executionKey != input.ExecutionKey ||
		input.ApproverUserID == uuid.Nil || input.RequesterUserID == uuid.Nil || input.RequesterUserID == input.ApproverUserID ||
		input.PolicyVersion < 1 || input.SchemaVersion < 1 || input.ActivationVersion < 1 {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	if err := requireHistoricalF12RepairSchema(ctx, p.repository, through); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	defer tx.Rollback(ctx)
	stored, found, err := paymentFinancialRepairCaseMatches(ctx, tx, input, executionKey)
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if found {
		return stored, tx.Commit(ctx)
	}
	if err := paymentFinancialRepairExecutionActivation(ctx, tx, input, p.now()); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	plan, err := paymentFinancialRepairPlanFrom(ctx, tx, input.Command, input.PolicyVersion, input.SchemaVersion, input.ActivationVersion)
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if mismatch := paymentFinancialRepairFrozenMismatch(plan, input.Frozen); mismatch != "" {
		return PaymentFinancialRepairResult{}, wrapSnapshotMismatch(mismatch)
	}
	order, err := getPaymentOrderByID(ctx, tx, plan.OrderID, true)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PaymentFinancialRepairResult{}, ErrNotFound
		}
		return PaymentFinancialRepairResult{}, err
	}
	if order.Status != "MANUAL_REVIEW" || order.FulfillmentStatus != "MANUAL_REVIEW" {
		return PaymentFinancialRepairResult{}, wrapSnapshotMismatch("order_status_changed")
	}
	now := p.now()
	var balanceBefore int64
	if err := tx.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1 FOR UPDATE`, plan.WalletID).Scan(&balanceBefore); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if balanceBefore != plan.WalletPostedMicrocredits {
		return PaymentFinancialRepairResult{}, wrapSnapshotMismatch("wallet_version_changed")
	}
	snapshot, err := json.Marshal(map[string]any{
		"status": plan.Provider.Status, "provider_status": plan.Provider.ProviderStatus,
		"provider_transaction_id": plan.ProviderTransactionID, "paid_at": plan.PaidAt,
		"external_customer_key": plan.Provider.ExternalCustomerKey, "external_sku": plan.Provider.ExternalSKU,
	})
	if err != nil {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	evidenceKey := "repair:" + plan.OrderID.String() + ":" + plan.ProviderTransactionID
	merged, evidenceID, mergeErr := p.mergeHistoricalF12Payment(ctx, tx, order, "MANUAL_REPAIR", evidenceKey, snapshot, plan.Provider, true, now)
	if mergeErr != nil {
		return PaymentFinancialRepairResult{}, mergeErr
	}
	if evidenceID == uuid.Nil {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	var transactionID, packageID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT transaction.id,package.id FROM payment_transactions transaction
JOIN entitlement_packages package ON package.source_payment_transaction_id=transaction.id
WHERE transaction.payment_order_id=$1 AND transaction.provider_transaction_id=$2`, merged.ID, plan.ProviderTransactionID).
		Scan(&transactionID, &packageID); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	repairID := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO payment_order_repairs(id,payment_order_id,payment_transaction_id,payment_evidence_id,actor_id,reason,
provider_snapshot,entitlement_package_id,points_awarded,wallet_balance_before,wallet_balance_after,created_at,
approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,facts_digest,requester_user_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		// The audit row references the frozen evidence the approval froze (not the
		// writer's own MANUAL_REPAIR provenance row), records the approving
		// identity as its actor and the requesting identity it approved for, per
		// design §4 item 5 and the 2026-09-21 decision that both sides of the
		// independent approval must be attributable.
		repairID, plan.OrderID, transactionID, plan.EvidenceID, input.ApproverUserID, paymentFinancialRepairReason,
		snapshot, packageID, plan.CreditMicrocredits, balanceBefore, balanceBefore+plan.CreditMicrocredits-plan.AppliedToReceivables,
		now, input.ApprovalRequestID, input.AuthorizationEventID, input.ApprovalEpoch,
		legacyFinancialExecutionKeyDigestV1(executionKey), plan.FactsDigest, input.RequesterUserID); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	var journalID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM journal_entries WHERE business_reference=$1`, "repair-payment-transaction:"+transactionID.String()).Scan(&journalID); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	var walletVersionAfter int64
	if err := tx.QueryRow(ctx, `SELECT version FROM wallets WHERE id=$1`, plan.WalletID).Scan(&walletVersionAfter); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	committed, err := p.repository.PaymentFinancialRepairForRequest(ctx, input.ApprovalRequestID)
	if err != nil {
		return PaymentFinancialRepairResult{}, err
	}
	if committed.PaymentTransactionID != transactionID || committed.EntitlementPackageID != packageID || committed.JournalEntryID != journalID ||
		committed.RepairID != repairID || committed.Status != "SUCCEEDED" ||
		walletVersionAfter < 1 || committed.FactsDigest != plan.FactsDigest {
		return PaymentFinancialRepairResult{}, ErrConflict
	}
	return committed, nil
}
