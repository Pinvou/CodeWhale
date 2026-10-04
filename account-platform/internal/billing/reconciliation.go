package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ReconciliationDifference struct {
	MerchantOrderNo string
	PaymentOrderID  *uuid.UUID
	CaseType        string
	Provider        *ProviderBillEntry
	Internal        *PaymentOrder
}

type ReconciliationRun struct {
	ID               uuid.UUID `json:"reconciliation_run_id"`
	Channel          string    `json:"channel"`
	BusinessDate     string    `json:"business_date"`
	Status           string    `json:"status"`
	ProviderRowCount int       `json:"provider_row_count"`
	CaseCount        int       `json:"case_count"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      time.Time `json:"completed_at"`
}

func (p *PaymentCoordinator) SyncOrder(ctx context.Context, orderID uuid.UUID) (PaymentOrder, error) {
	order, err := getPaymentOrderByID(ctx, p.repository.pool, orderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	providerOrder, err := p.provider.Get(ctx, order.MerchantOrderNo)
	if err != nil {
		return PaymentOrder{}, errors.Join(ErrPaymentProvider, err)
	}
	if providerOrder.MerchantOrderNo != order.MerchantOrderNo || providerOrder.Channel != order.Channel || providerOrder.AmountFen != order.AmountFen || providerOrder.Currency != order.Currency {
		return PaymentOrder{}, ErrConflict
	}
	if providerOrder.Status == "SUCCESS" || providerOrder.Status == "PARTIALLY_REFUNDED" || providerOrder.Status == "REFUNDED" {
		return p.applyQueriedPayment(ctx, order.ID, providerOrder)
	}
	if providerOrder.Status == "CLOSED" || providerOrder.Status == "FAILED" {
		if _, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET status=$2,updated_at=$3 WHERE id=$1 AND status IN ('CREATED','PENDING')`, order.ID, providerOrder.Status, p.now()); err != nil {
			return PaymentOrder{}, err
		}
		return getPaymentOrderByID(ctx, p.repository.pool, order.ID, false)
	}
	if (order.Status == "CREATED" || order.Status == "PENDING") && !order.ExpiresAt.After(p.now()) && providerOrder.Status == "PENDING" {
		closed, closeErr := p.provider.Close(ctx, order.MerchantOrderNo)
		if closeErr != nil {
			return PaymentOrder{}, errors.Join(ErrPaymentProvider, closeErr)
		}
		if !providerOrderMatches(order, closed) {
			return PaymentOrder{}, ErrConflict
		}
		if closed.Status == "CLOSED" {
			_, err = p.repository.pool.Exec(ctx, `UPDATE payment_orders SET status='CLOSED',updated_at=$2 WHERE id=$1 AND status IN ('CREATED','PENDING')`, order.ID, p.now())
			if err != nil {
				return PaymentOrder{}, err
			}
			return getPaymentOrderByID(ctx, p.repository.pool, order.ID, false)
		}
	}
	return order, nil
}

func (p *PaymentCoordinator) applyQueriedPayment(ctx context.Context, orderID uuid.UUID, provider ProviderOrder) (PaymentOrder, error) {
	if provider.ChannelTransactionID == nil || strings.TrimSpace(*provider.ChannelTransactionID) == "" || provider.PaidAt == nil || provider.PaidAt.IsZero() {
		return PaymentOrder{}, ErrConflict
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err := getPaymentOrderByID(ctx, tx, orderID, true)
	if err != nil {
		return PaymentOrder{}, err
	}
	if provider.MerchantOrderNo != order.MerchantOrderNo || provider.Channel != order.Channel || provider.AmountFen != order.AmountFen || provider.Currency != order.Currency {
		return PaymentOrder{}, ErrConflict
	}
	if order.FulfillmentStatus == "CREDITED" || order.FulfillmentStatus == "REFUNDED" {
		if order.ChannelTransactionID == nil || *order.ChannelTransactionID != *provider.ChannelTransactionID || order.PaidAt == nil || !samePaymentTime(*order.PaidAt, *provider.PaidAt) {
			return PaymentOrder{}, ErrConflict
		}
		return order, tx.Commit(ctx)
	}
	now := p.now()
	command, err := tx.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,channel,channel_transaction_id,status,amount_fen,currency,source,verified_at,created_at,updated_at) VALUES($1,$2,$3,$4,'SUCCEEDED',$5,$6,'QUERY',$7,$7,$7) ON CONFLICT (channel_transaction_id) DO NOTHING`, uuid.Must(uuid.NewV7()), order.ID, order.Channel, *provider.ChannelTransactionID, order.AmountFen, order.Currency, now)
	if err != nil {
		return PaymentOrder{}, err
	}
	if command.RowsAffected() == 0 {
		var existingOrderID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT payment_order_id FROM payment_transactions WHERE channel_transaction_id=$1`, *provider.ChannelTransactionID).Scan(&existingOrderID); err != nil || existingOrderID != order.ID {
			return PaymentOrder{}, ErrConflict
		}
	}
	status := "PAID"
	if !order.ExpiresAt.After(*provider.PaidAt) {
		status = "PAID_LATE"
	}
	_, err = tx.Exec(ctx, `UPDATE payment_orders SET status=$2,fulfillment_status='CREDITED',channel_transaction_id=$3,paid_at=$4,updated_at=$5 WHERE id=$1`, order.ID, status, *provider.ChannelTransactionID, *provider.PaidAt, now)
	if err == nil {
		err = creditWithinTransaction(ctx, tx, order.WalletID, "recharge:"+order.ID.String(), order.CreditMicrocredits, now)
	}
	if err == nil {
		err = enqueueRechargeEvent(ctx, tx, order, now)
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	order.Status, order.FulfillmentStatus, order.ChannelTransactionID, order.PaidAt = status, "CREDITED", provider.ChannelTransactionID, provider.PaidAt
	return order, tx.Commit(ctx)
}

func CompareReconciliation(provider []ProviderBillEntry, internal []PaymentOrder) []ReconciliationDifference {
	providerByMerchant := make(map[string]ProviderBillEntry, len(provider))
	transactionOwners := make(map[string]string, len(provider))
	differences := make([]ReconciliationDifference, 0)
	for i := range provider {
		item := provider[i]
		if owner, exists := transactionOwners[item.ChannelTransactionID]; exists && owner != item.MerchantOrderNo {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, CaseType: "DUPLICATE_TRANSACTION", Provider: &copy})
		} else {
			transactionOwners[item.ChannelTransactionID] = item.MerchantOrderNo
		}
		providerByMerchant[item.MerchantOrderNo] = item
	}
	internalByMerchant := make(map[string]PaymentOrder, len(internal))
	for i := range internal {
		item := internal[i]
		internalByMerchant[item.MerchantOrderNo] = item
		providerItem, exists := providerByMerchant[item.MerchantOrderNo]
		if !exists {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, PaymentOrderID: &copy.ID, CaseType: "INTERNAL_PAID_PROVIDER_MISSING", Internal: &copy})
			continue
		}
		if providerItem.AmountFen != item.AmountFen || providerItem.Currency != item.Currency {
			providerCopy, internalCopy := providerItem, item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: item.MerchantOrderNo, PaymentOrderID: &internalCopy.ID, CaseType: "AMOUNT_MISMATCH", Provider: &providerCopy, Internal: &internalCopy})
		}
	}
	for merchant, item := range providerByMerchant {
		if _, exists := internalByMerchant[merchant]; !exists {
			copy := item
			differences = append(differences, ReconciliationDifference{MerchantOrderNo: merchant, CaseType: "PROVIDER_PAID_INTERNAL_MISSING", Provider: &copy})
		}
	}
	return differences
}

func (p *PaymentCoordinator) RunReconciliation(ctx context.Context, channel string, businessDate time.Time) (ReconciliationRun, error) {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if !validPaymentChannel(channel) {
		return ReconciliationRun{}, ErrConflict
	}
	date := businessDate.UTC()
	providerRows, err := p.provider.Bill(ctx, channel, date)
	if err != nil {
		return ReconciliationRun{}, errors.Join(ErrPaymentProvider, err)
	}
	start, end := date, date.AddDate(0, 0, 1)
	internalRows, err := p.listPaidOrders(ctx, channel, start, end)
	if err != nil {
		return ReconciliationRun{}, err
	}
	differences := CompareReconciliation(providerRows, internalRows)
	now := p.now()
	run := ReconciliationRun{ID: uuid.Must(uuid.NewV7()), Channel: channel, BusinessDate: date.Format("2006-01-02"), Status: "COMPLETED", ProviderRowCount: len(providerRows), CaseCount: len(differences), StartedAt: now, CompletedAt: now}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return ReconciliationRun{}, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `INSERT INTO reconciliation_runs(id,channel,business_date,status,provider_row_count,case_count,started_at,completed_at) VALUES($1,$2,$3,'COMPLETED',$4,$5,$6,$6) ON CONFLICT(channel,business_date) DO UPDATE SET status='COMPLETED',provider_row_count=EXCLUDED.provider_row_count,case_count=EXCLUDED.case_count,started_at=EXCLUDED.started_at,completed_at=EXCLUDED.completed_at,error_message=NULL RETURNING id`, run.ID, channel, date, len(providerRows), len(differences), now).Scan(&run.ID)
	if err != nil {
		return ReconciliationRun{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM reconciliation_cases WHERE reconciliation_run_id=$1`, run.ID); err != nil {
		return ReconciliationRun{}, err
	}
	for _, difference := range differences {
		providerJSON, _ := json.Marshal(difference.Provider)
		internalJSON, _ := json.Marshal(difference.Internal)
		_, err = tx.Exec(ctx, `INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,provider_snapshot,internal_snapshot,created_at) VALUES($1,$2,$3,$4,$5,'OPEN',$6,$7,$8)`, uuid.Must(uuid.NewV7()), run.ID, difference.PaymentOrderID, difference.MerchantOrderNo, difference.CaseType, providerJSON, internalJSON, now)
		if err != nil {
			return ReconciliationRun{}, err
		}
	}
	return run, tx.Commit(ctx)
}

func (p *PaymentCoordinator) listPaidOrders(ctx context.Context, channel string, start, end time.Time) ([]PaymentOrder, error) {
	rows, err := p.repository.pool.Query(ctx, `SELECT id,merchant_order_no,requester_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,COALESCE(payment_url,''),channel_transaction_id,expires_at,paid_at FROM payment_orders WHERE channel=$1 AND paid_at >= $2 AND paid_at < $3 AND fulfillment_status IN ('CREDITED','REFUNDED') ORDER BY paid_at,merchant_order_no`, channel, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PaymentOrder, 0)
	for rows.Next() {
		var item PaymentOrder
		if err := rows.Scan(&item.ID, &item.MerchantOrderNo, &item.RequesterID, &item.WalletID, &item.ProductID, &item.Channel, &item.AmountFen, &item.Currency, &item.CreditMicrocredits, &item.Status, &item.FulfillmentStatus, &item.PaymentURL, &item.ChannelTransactionID, &item.ExpiresAt, &item.PaidAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
