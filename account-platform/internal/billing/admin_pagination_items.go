package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type adminPaginationItemsRepository interface {
	adminPaginationRepository
	AdminWalletsByIDs(context.Context, []uuid.UUID) ([]Wallet, error)
	AdminWalletLedgerByIDs(context.Context, uuid.UUID, []uuid.UUID) ([]AdminLedgerEntry, error)
	AdminReceivablesByIDs(context.Context, []uuid.UUID) ([]Receivable, error)
	AdminPaymentOrdersByIDs(context.Context, []uuid.UUID) ([]AdminPaymentOrder, error)
	AdminRefundsByIDs(context.Context, []string) ([]AdminRefundOperation, error)
	AdminAdjustmentsByIDs(context.Context, []uuid.UUID) ([]ManualAdjustment, error)
	AdminUsageHoldsByIDs(context.Context, []uuid.UUID) ([]AdminHold, error)
	AdminLatePaymentReviewsByIDs(context.Context, []uuid.UUID) ([]LatePaymentReviewCase, error)
	AdminPaymentAnomaliesByIDs(context.Context, []uuid.UUID) ([]PaymentAnomalyCase, error)
	AdminReconciliationRunsByIDs(context.Context, []uuid.UUID) ([]ReconciliationRun, error)
	AdminReconciliationCasesByIDs(context.Context, []uuid.UUID) ([]AdminReconciliationCase, error)
}

func (r *PostgresRepository) AdminWalletsByIDs(ctx context.Context, ids []uuid.UUID) ([]Wallet, error) {
	items := []Wallet{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,receivable_microcredits,posted_microcredits-reserved_microcredits-receivable_microcredits,financial_status,financial_freeze_reason,version FROM wallets WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Wallet
		if err := rows.Scan(&item.ID, &item.OwnerType, &item.OwnerID, &item.Status, &item.PostedMicrocredits, &item.ReservedMicrocredits, &item.ReceivableMicrocredits, &item.AvailableMicrocredits, &item.FinancialStatus, &item.FinancialFreezeReason, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminWalletLedgerByIDs(ctx context.Context, walletID uuid.UUID, ids []uuid.UUID) ([]AdminLedgerEntry, error) {
	items := []AdminLedgerEntry{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT je.id,lp.id,la.id,la.code,la.account_type,lp.credit_lot_id,component.package_id,component.id,component.component_type,je.entry_type,je.business_reference,je.description,lp.delta_microcredits,(SELECT COALESCE(sum(peer.delta_microcredits),0) FROM ledger_postings peer WHERE peer.journal_entry_id=je.id),lp.created_at FROM ledger_postings lp JOIN journal_entries je ON je.id=lp.journal_entry_id JOIN ledger_accounts la ON la.id=lp.ledger_account_id LEFT JOIN entitlement_components component ON component.credit_lot_id=lp.credit_lot_id WHERE la.wallet_id=$2 AND lp.id=ANY($1) ORDER BY array_position($1::uuid[],lp.id)`, ids, walletID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AdminLedgerEntry
		if err := rows.Scan(&item.JournalEntryID, &item.PostingID, &item.LedgerAccountID, &item.LedgerAccountCode, &item.LedgerAccountType, &item.CreditLotID, &item.EntitlementPackageID, &item.EntitlementComponentID, &item.ComponentType, &item.EntryType, &item.BusinessRef, &item.Description, &item.DeltaMicrocredit, &item.JournalBalance, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminReceivablesByIDs(ctx context.Context, ids []uuid.UUID) ([]Receivable, error) {
	items := []Receivable{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,wallet_id,source_type,source_reference,payment_order_id,payment_transaction_id,entitlement_package_id,usage_request_id,original_microcredits,settled_microcredits,outstanding_microcredits,status,version,created_at,updated_at,settled_at FROM receivables WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Receivable
		if err := rows.Scan(&item.ID, &item.WalletID, &item.SourceType, &item.SourceReference, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.UsageRequestID, &item.OriginalMicrocredits, &item.SettledMicrocredits, &item.OutstandingMicrocredits, &item.Status, &item.Version, &item.CreatedAt, &item.UpdatedAt, &item.SettledAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminPaymentOrdersByIDs(ctx context.Context, ids []uuid.UUID) ([]AdminPaymentOrder, error) {
	items := []AdminPaymentOrder{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,merchant_order_no,provider_id,provider_version,product_id,COALESCE(provider_sku,''),COALESCE(primary_provider_transaction_id,''),COALESCE(mobile_e164,''),requester_id,payer_user_id,beneficiary_organization_id,wallet_id,channel,amount_fen,credit_microcredits,status,fulfillment_status,close_status,paid_at,payment_summary,refund_summary,chargeback_summary,attention_summary,summary_version,created_at,updated_at FROM payment_orders WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AdminPaymentOrder
		var mobile string
		if err := rows.Scan(&item.ID, &item.MerchantOrderNo, &item.ProviderID, &item.ProviderVersion, &item.ProductID, &item.ProviderSKU, &item.PrimaryProviderTransaction, &mobile, &item.RequesterID, &item.PayerUserID, &item.BeneficiaryOrganizationID, &item.WalletID, &item.Channel, &item.AmountFen, &item.CreditMicrocredits, &item.Status, &item.FulfillmentStatus, &item.CloseStatus, &item.PaidAt, &item.PaymentSummary, &item.RefundSummary, &item.ChargebackSummary, &item.AttentionSummary, &item.SummaryVersion, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.MobileMasked = maskAdminMobile(mobile)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminRefundsByIDs(ctx context.Context, ids []string) ([]AdminRefundOperation, error) {
	items := []AdminRefundOperation{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT operation_id,operation_type,payment_order_id,payment_transaction_id,entitlement_package_id,provider_refund_id,amount_fen,base_microcredits,bonus_microcredits,status,reason,created_at,updated_at FROM (SELECT 'NORMAL:'||refund.id::text resource_id,refund.id operation_id,'NORMAL' operation_type,refund.payment_order_id,refund.payment_transaction_id,refund.entitlement_package_id,refund.channel_refund_id provider_refund_id,refund.amount_fen,refund.base_microcredits,refund.bonus_microcredits,refund.status,refund.reason,refund.created_at,refund.updated_at FROM refunds refund UNION ALL SELECT 'FORCED:'||forced.id::text,forced.id,'FORCED',forced.payment_order_id,forced.payment_transaction_id,forced.entitlement_package_id,forced.provider_refund_id,forced.amount_fen,0,0,forced.status,'PROVIDER_INITIATED',forced.created_at,COALESCE(forced.resolved_at,forced.created_at) FROM forced_refund_cases forced) operation WHERE resource_id=ANY($1) ORDER BY array_position($1::text[],resource_id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AdminRefundOperation
		if err := rows.Scan(&item.ID, &item.OperationType, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.ProviderRefundID, &item.AmountFen, &item.BaseMicrocredits, &item.BonusMicrocredits, &item.Status, &item.Reason, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminAdjustmentsByIDs(ctx context.Context, ids []uuid.UUID) ([]ManualAdjustment, error) {
	items := []ManualAdjustment{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,wallet_id,amount_microcredits,reason,requested_by,approved_by,rejected_by,status,version,request_idempotency_key,approval_request_id,execution_idempotency_key,journal_entry_id,decision_reason,created_at,updated_at,decided_at FROM manual_adjustments WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ManualAdjustment
		if err := rows.Scan(&item.ID, &item.WalletID, &item.AmountMicrocredits, &item.Reason, &item.RequestedBy, &item.ApprovedBy, &item.RejectedBy, &item.Status, &item.Version, &item.RequestIdempotencyKey, &item.ApprovalRequestID, &item.ExecutionIdempotencyKey, &item.JournalEntryID, &item.DecisionReason, &item.CreatedAt, &item.UpdatedAt, &item.DecidedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminUsageHoldsByIDs(ctx context.Context, ids []uuid.UUID) ([]AdminHold, error) {
	items := []AdminHold{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT hold.id,hold.usage_request_id,hold.wallet_id,hold.price_version_id,hold.status,CASE WHEN hold.status='REVIEW' THEN 'REVIEW_REQUIRED' WHEN hold.status='ACTIVE' AND hold.expires_at<=now() THEN 'LEASE_EXPIRED' WHEN hold.status<>'ACTIVE' AND COALESCE(allocation.outstanding,0)<>0 THEN 'TERMINAL_ALLOCATION_REMAINS' WHEN hold.status='ACTIVE' AND COALESCE(allocation.outstanding,0)<>hold.max_microcredits THEN 'ALLOCATION_MISMATCH' ELSE 'NONE' END,hold.max_microcredits,hold.actual_microcredits,hold.expires_at,hold.terminal_at,hold.model_id,hold.protocol_code,hold.formula_version_id,hold.pricing_snapshot,hold.request_snapshot,hold.usage_snapshot,hold.organization_id,hold.member_user_id,hold.membership_version,hold.created_at,hold.updated_at FROM usage_holds hold LEFT JOIN (SELECT hold_id,sum(reserved_microcredits-settled_microcredits-released_microcredits) outstanding FROM usage_hold_allocations GROUP BY hold_id) allocation ON allocation.hold_id=hold.id WHERE hold.id=ANY($1) ORDER BY array_position($1::uuid[],hold.id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AdminHold
		if err := scanAdminHold(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminLatePaymentReviewsByIDs(ctx context.Context, ids []uuid.UUID) ([]LatePaymentReviewCase, error) {
	items := []LatePaymentReviewCase{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, latePaymentReviewSelect+` WHERE late.id=ANY($1) ORDER BY array_position($1::uuid[],late.id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item, _, err := scanLatePaymentReviewCase(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminPaymentAnomaliesByIDs(ctx context.Context, ids []uuid.UUID) ([]PaymentAnomalyCase, error) {
	items := []PaymentAnomalyCase{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, paymentAnomalySelect+` WHERE anomaly.id=ANY($1) ORDER BY array_position($1::uuid[],anomaly.id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item, _, err := scanPaymentAnomaly(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminReconciliationRunsByIDs(ctx context.Context, ids []uuid.UUID) ([]ReconciliationRun, error) {
	items := []ReconciliationRun{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,provider_id,provider_version,channel,business_date,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at,COALESCE(completed_at,started_at) FROM reconciliation_runs WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ReconciliationRun
		var businessDate time.Time
		if err := rows.Scan(&item.ID, &item.ProviderID, &item.ProviderVersion, &item.Channel, &businessDate, &item.SourceMode, &item.RequestedBy, &item.InputDigest, &item.Status, &item.ProviderRowCount, &item.CaseCount, &item.StartedAt, &item.CompletedAt); err != nil {
			return nil, err
		}
		item.BusinessDate = businessDate.Format("2006-01-02")
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminReconciliationCasesByIDs(ctx context.Context, ids []uuid.UUID) ([]AdminReconciliationCase, error) {
	items := []AdminReconciliationCase{}
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.pool.Query(ctx, reconciliationCaseSelect+` WHERE id=ANY($1) ORDER BY array_position($1::uuid[],id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AdminReconciliationCase
		if err := scanReconciliationCase(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func ensureAdminPaginationItems(repository any) (adminPaginationItemsRepository, error) {
	paged, ok := repository.(adminPaginationItemsRepository)
	if !ok {
		return nil, errors.New("admin pagination repository is unavailable")
	}
	return paged, nil
}
