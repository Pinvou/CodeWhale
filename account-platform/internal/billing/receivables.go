package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	receivableAccount         = uuid.MustParse("00000000-0000-7000-8000-000000000005")
	chargebackClearingAccount = uuid.MustParse("00000000-0000-7000-8000-000000000006")
)

type Receivable struct {
	ID                      uuid.UUID  `json:"receivable_id"`
	WalletID                uuid.UUID  `json:"wallet_id"`
	SourceType              string     `json:"source_type"`
	SourceReference         string     `json:"source_reference"`
	PaymentOrderID          *uuid.UUID `json:"payment_order_id,omitempty"`
	PaymentTransactionID    *uuid.UUID `json:"payment_transaction_id,omitempty"`
	EntitlementPackageID    *uuid.UUID `json:"entitlement_package_id,omitempty"`
	UsageRequestID          *uuid.UUID `json:"usage_request_id,omitempty"`
	OriginalMicrocredits    int64      `json:"original_microcredits"`
	SettledMicrocredits     int64      `json:"settled_microcredits"`
	OutstandingMicrocredits int64      `json:"outstanding_microcredits"`
	Status                  string     `json:"status"`
	Version                 int64      `json:"version"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	SettledAt               *time.Time `json:"settled_at,omitempty"`
}

type ReceivableAllocation struct {
	ID                 uuid.UUID `json:"allocation_id"`
	ReceivableID       uuid.UUID `json:"receivable_id"`
	WalletID           uuid.UUID `json:"wallet_id"`
	SourceType         string    `json:"source_type"`
	SourceReference    string    `json:"source_reference"`
	AmountMicrocredits int64     `json:"amount_microcredits"`
	JournalEntryID     uuid.UUID `json:"journal_entry_id"`
	CreatedAt          time.Time `json:"created_at"`
}

func createReceivableWithinTransaction(ctx context.Context, tx pgx.Tx, walletID uuid.UUID, sourceType, sourceReference string, amount int64, paymentOrderID, paymentTransactionID, packageID, usageRequestID *uuid.UUID, now time.Time) (Receivable, error) {
	// The closed source-type set is the same one migration 000025/000060 keeps in
	// receivables.source_type: usage overage, provider chargeback, and the forced
	// refund BASE shortfall created by the reviewed forced-refund path.
	if amount <= 0 || strings.TrimSpace(sourceReference) == "" ||
		(sourceType != "USAGE_OVERAGE" && sourceType != "CHARGEBACK" && sourceType != "FORCED_REFUND") {
		return Receivable{}, ErrConflict
	}
	var existing Receivable
	err := tx.QueryRow(ctx, `SELECT id,wallet_id,source_type,source_reference,payment_order_id,payment_transaction_id,entitlement_package_id,usage_request_id,original_microcredits,settled_microcredits,outstanding_microcredits,status,version,created_at,updated_at,settled_at FROM receivables WHERE source_type=$1 AND source_reference=$2 FOR UPDATE`, sourceType, sourceReference).Scan(&existing.ID, &existing.WalletID, &existing.SourceType, &existing.SourceReference, &existing.PaymentOrderID, &existing.PaymentTransactionID, &existing.EntitlementPackageID, &existing.UsageRequestID, &existing.OriginalMicrocredits, &existing.SettledMicrocredits, &existing.OutstandingMicrocredits, &existing.Status, &existing.Version, &existing.CreatedAt, &existing.UpdatedAt, &existing.SettledAt)
	if err == nil {
		if existing.WalletID != walletID || existing.OriginalMicrocredits != amount {
			return Receivable{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Receivable{}, err
	}
	receivable := Receivable{ID: uuid.Must(uuid.NewV7()), WalletID: walletID, SourceType: sourceType, SourceReference: sourceReference, PaymentOrderID: paymentOrderID, PaymentTransactionID: paymentTransactionID, EntitlementPackageID: packageID, UsageRequestID: usageRequestID, OriginalMicrocredits: amount, OutstandingMicrocredits: amount, Status: "OPEN", Version: 1, CreatedAt: now, UpdatedAt: now}
	_, err = tx.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,payment_order_id,payment_transaction_id,entitlement_package_id,usage_request_id,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,'OPEN',$10,$10)`, receivable.ID, walletID, sourceType, sourceReference, paymentOrderID, paymentTransactionID, packageID, usageRequestID, amount, now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE wallets SET receivable_microcredits=receivable_microcredits+$2,financial_status='RECEIVABLE_OPEN',financial_freeze_reason=$3,version=version+1,updated_at=$4 WHERE id=$1`, walletID, amount, sourceType, now)
	}
	return receivable, err
}

func settleOpenReceivablesWithinTransaction(ctx context.Context, tx pgx.Tx, walletID uuid.UUID, maximum int64, sourceType, sourceReference string, journalID uuid.UUID, now time.Time) (int64, error) {
	if maximum <= 0 {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `SELECT id,wallet_id,source_type,source_reference,payment_order_id,payment_transaction_id,entitlement_package_id,usage_request_id,original_microcredits,settled_microcredits,outstanding_microcredits,status,version,created_at,updated_at,settled_at FROM receivables WHERE wallet_id=$1 AND outstanding_microcredits>0 ORDER BY created_at,id FOR UPDATE`, walletID)
	if err != nil {
		return 0, err
	}
	items := []Receivable{}
	for rows.Next() {
		var item Receivable
		if err := rows.Scan(&item.ID, &item.WalletID, &item.SourceType, &item.SourceReference, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.UsageRequestID, &item.OriginalMicrocredits, &item.SettledMicrocredits, &item.OutstandingMicrocredits, &item.Status, &item.Version, &item.CreatedAt, &item.UpdatedAt, &item.SettledAt); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	rows.Close()
	remaining, applied := maximum, int64(0)
	for _, item := range items {
		if remaining == 0 {
			break
		}
		amount := min(remaining, item.OutstandingMicrocredits)
		status := "PARTIALLY_SETTLED"
		var settledAt *time.Time
		if amount == item.OutstandingMicrocredits {
			status, settledAt = "SETTLED", &now
		}
		if _, err := tx.Exec(ctx, `UPDATE receivables SET settled_microcredits=settled_microcredits+$2,outstanding_microcredits=outstanding_microcredits-$2,status=$3,version=version+1,updated_at=$4,settled_at=$5 WHERE id=$1 AND outstanding_microcredits>=$2`, item.ID, amount, status, now, settledAt); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO receivable_allocations(id,receivable_id,wallet_id,source_type,source_reference,amount_microcredits,journal_entry_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), item.ID, walletID, sourceType, sourceReference, amount, journalID, now); err != nil {
			return 0, err
		}
		remaining, applied = remaining-amount, applied+amount
	}
	if applied > 0 {
		_, err = tx.Exec(ctx, `UPDATE wallets SET receivable_microcredits=receivable_microcredits-$2,financial_status=CASE WHEN receivable_microcredits=$2 THEN 'CLEAR' ELSE 'RECEIVABLE_OPEN' END,financial_freeze_reason=CASE WHEN receivable_microcredits=$2 THEN NULL ELSE financial_freeze_reason END,version=version+1,updated_at=$3 WHERE id=$1 AND receivable_microcredits>=$2`, walletID, applied, now)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE payment_orders payment_order SET attention_summary=CASE WHEN EXISTS(SELECT 1 FROM receivables receivable WHERE receivable.payment_order_id=payment_order.id AND receivable.outstanding_microcredits>0) THEN 'RECEIVABLE_OPEN' WHEN payment_order.attention_summary='RECEIVABLE_OPEN' THEN 'NORMAL' ELSE payment_order.attention_summary END,summary_version=summary_version+1,updated_at=$2 WHERE payment_order.id IN (SELECT DISTINCT receivable.payment_order_id FROM receivables receivable WHERE receivable.wallet_id=$1 AND receivable.payment_order_id IS NOT NULL)`, walletID, now)
		}
	}
	return applied, err
}

func (r *PostgresRepository) ListReceivables(ctx context.Context, walletID *uuid.UUID, status string, limit int) ([]Receivable, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `SELECT id,wallet_id,source_type,source_reference,payment_order_id,payment_transaction_id,entitlement_package_id,usage_request_id,original_microcredits,settled_microcredits,outstanding_microcredits,status,version,created_at,updated_at,settled_at FROM receivables WHERE ($1::uuid IS NULL OR wallet_id=$1) AND ($2='' OR status=$2) ORDER BY created_at DESC,id DESC LIMIT $3`, walletID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Receivable{}
	for rows.Next() {
		var item Receivable
		if err := rows.Scan(&item.ID, &item.WalletID, &item.SourceType, &item.SourceReference, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.UsageRequestID, &item.OriginalMicrocredits, &item.SettledMicrocredits, &item.OutstandingMicrocredits, &item.Status, &item.Version, &item.CreatedAt, &item.UpdatedAt, &item.SettledAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) ReceivableAllocations(ctx context.Context, receivableID uuid.UUID) ([]ReceivableAllocation, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,receivable_id,wallet_id,source_type,source_reference,amount_microcredits,journal_entry_id,created_at FROM receivable_allocations WHERE receivable_id=$1 ORDER BY created_at,id`, receivableID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ReceivableAllocation{}
	for rows.Next() {
		var item ReceivableAllocation
		if err := rows.Scan(&item.ID, &item.ReceivableID, &item.WalletID, &item.SourceType, &item.SourceReference, &item.AmountMicrocredits, &item.JournalEntryID, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
