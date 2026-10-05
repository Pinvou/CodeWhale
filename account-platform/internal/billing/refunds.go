package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrPaymentProvider = errors.New("payment provider request failed")

type Refund struct {
	ID                 uuid.UUID `json:"refund_id"`
	PaymentOrderID     uuid.UUID `json:"payment_order_id"`
	RequesterID        uuid.UUID `json:"requester_id"`
	MerchantRefundNo   string    `json:"merchant_refund_no"`
	ChannelRefundID    *string   `json:"channel_refund_id,omitempty"`
	AmountFen          int64     `json:"amount_fen"`
	CreditMicrocredits int64     `json:"credit_microcredits"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	FailureReason      *string   `json:"failure_reason,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (p *PaymentCoordinator) GetOrder(ctx context.Context, orderID, requesterID uuid.UUID) (PaymentOrder, error) {
	order, err := getPaymentOrderByID(ctx, p.repository.pool, orderID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	if order.RequesterID != requesterID {
		return PaymentOrder{}, ErrNotFound
	}
	return order, nil
}

func (p *PaymentCoordinator) RecentOrder(ctx context.Context, requesterID, walletID uuid.UUID) (*PaymentOrder, error) {
	query := `SELECT id,merchant_order_no,requester_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,COALESCE(payment_url,''),COALESCE(qr_data,''),channel_transaction_id,expires_at,paid_at,COALESCE(mobile_e164,''),COALESCE(provider_sku,'')
		FROM payment_orders WHERE requester_id=$1 AND wallet_id=$2 ORDER BY created_at DESC LIMIT 1`
	var order PaymentOrder
	err := p.repository.pool.QueryRow(ctx, query, requesterID, walletID).Scan(&order.ID, &order.MerchantOrderNo, &order.RequesterID, &order.WalletID, &order.ProductID, &order.Channel, &order.AmountFen, &order.Currency, &order.CreditMicrocredits, &order.Status, &order.FulfillmentStatus, &order.PaymentURL, &order.QRData, &order.ChannelTransactionID, &order.ExpiresAt, &order.PaidAt, &order.Mobile, &order.ProviderSKU)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &order, nil
}

func (p *PaymentCoordinator) ListOrders(ctx context.Context, requesterID, walletID uuid.UUID, limit int) ([]PaymentOrderHistoryItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := p.repository.pool.Query(ctx, `SELECT po.id,po.merchant_order_no,po.product_id,COALESCE(po.product_snapshot->>'plan_name',po.product_id),COALESCE(po.provider_sku,po.product_snapshot->>'sku_code',''),po.amount_fen,COALESCE((po.product_snapshot->>'display_points')::bigint,po.credit_microcredits/10000),po.channel,po.status,po.fulfillment_status,po.expires_at,po.paid_at,ep.credited_at,ep.valid_until,po.created_at,CASE WHEN COALESCE(po.product_snapshot->>'price_status','LEGACY')='LEGACY' THEN 'LEGACY' ELSE 'PLAN' END
		FROM payment_orders po LEFT JOIN entitlement_packages ep ON ep.payment_order_id=po.id WHERE po.requester_id=$1 AND po.wallet_id=$2 ORDER BY po.created_at DESC,po.id DESC LIMIT $3`, requesterID, walletID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PaymentOrderHistoryItem, 0)
	for rows.Next() {
		var item PaymentOrderHistoryItem
		if err := rows.Scan(&item.ID, &item.MallOrderID, &item.ProductID, &item.PlanName, &item.SKUCode, &item.AmountFen, &item.DisplayPoints, &item.Channel, &item.Status, &item.FulfillmentStatus, &item.ExpiresAt, &item.PaidAt, &item.CreditedAt, &item.ValidUntil, &item.CreatedAt, &item.OrderType); err != nil {
			return nil, err
		}
		item.PinvouOrderID, item.InvoiceStatus = item.ID, "NOT_SUPPORTED"
		items = append(items, item)
	}
	return items, rows.Err()
}

func (p *PaymentCoordinator) CloseOrder(ctx context.Context, orderID, requesterID uuid.UUID) (PaymentOrder, error) {
	order, err := p.GetOrder(ctx, orderID, requesterID)
	if err != nil {
		return PaymentOrder{}, err
	}
	if order.Status == "CLOSED" {
		return order, nil
	}
	if order.Status != "PENDING" && order.Status != "CREATED" {
		return PaymentOrder{}, ErrConflict
	}
	providerOrder, err := p.provider.Close(ctx, order.MerchantOrderNo)
	if err != nil {
		return PaymentOrder{}, fmt.Errorf("%w: %v", ErrPaymentProvider, err)
	}
	if !providerOrderMatches(order, providerOrder) {
		return PaymentOrder{}, ErrConflict
	}
	if providerOrder.Status == "SUCCESS" || providerOrder.Status == "PARTIALLY_REFUNDED" || providerOrder.Status == "REFUNDED" {
		return p.applyQueriedPayment(ctx, order.ID, providerOrder)
	}
	if providerOrder.Status != "CLOSED" {
		return PaymentOrder{}, ErrConflict
	}
	now := p.now()
	command, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET status='CLOSED',updated_at=$3 WHERE id=$1 AND requester_id=$2 AND status IN ('CREATED','PENDING')`, orderID, requesterID, now)
	if err != nil {
		return PaymentOrder{}, err
	}
	if command.RowsAffected() == 0 {
		return p.GetOrder(ctx, orderID, requesterID)
	}
	order.Status = "CLOSED"
	return order, nil
}

func (p *PaymentCoordinator) CreateRefund(ctx context.Context, orderID, requesterID uuid.UUID, amountFen int64, idempotencyKey, reason string) (Refund, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	reason = strings.TrimSpace(reason)
	if orderID == uuid.Nil || requesterID == uuid.Nil || amountFen <= 0 || !validIdempotencyKey(idempotencyKey, 0) || reason == "" {
		return Refund{}, false, ErrConflict
	}
	digest := sha256.Sum256([]byte(orderID.String() + "|" + requesterID.String() + "|" + fmt.Sprint(amountFen) + "|" + reason))
	requestHash := hex.EncodeToString(digest[:])
	refund, created, err := p.reserveRefund(ctx, orderID, requesterID, amountFen, idempotencyKey, requestHash, reason)
	if err != nil || refund.Status == "SUCCEEDED" || refund.Status == "FAILED" || refund.Status == "MANUAL_REVIEW" {
		return refund, created, err
	}
	merchantOrderNo, err := refundMerchantOrder(ctx, p.repository.pool, orderID)
	if err != nil {
		return refund, created, err
	}
	providerRefund, providerErr := p.provider.Refund(ctx, merchantOrderNo, refund.MerchantRefundNo, amountFen)
	if providerErr != nil {
		return refund, created, fmt.Errorf("%w: %v", ErrPaymentProvider, providerErr)
	}
	return p.finishRefund(ctx, refund, providerRefund, created)
}

func (p *PaymentCoordinator) reserveRefund(ctx context.Context, orderID, requesterID uuid.UUID, amountFen int64, idempotencyKey, requestHash, reason string) (Refund, bool, error) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return Refund{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "refund.create:"+idempotencyKey); err != nil {
		return Refund{}, false, err
	}
	refund, err := getRefundByIdempotency(ctx, tx, idempotencyKey, true)
	if err == nil {
		if refund.PaymentOrderID != orderID || refund.RequesterID != requesterID || refund.AmountFen != amountFen {
			return Refund{}, false, ErrConflict
		}
		var storedHash string
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM refunds WHERE id=$1`, refund.ID).Scan(&storedHash); err != nil || storedHash != requestHash {
			return Refund{}, false, ErrConflict
		}
		return refund, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, false, err
	}
	order, err := getPaymentOrderByID(ctx, tx, orderID, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && order.RequesterID != requesterID) {
		return Refund{}, false, ErrNotFound
	}
	if err != nil || order.FulfillmentStatus != "CREDITED" || (order.Status != "PAID" && order.Status != "PAID_LATE" && order.Status != "PARTIALLY_REFUNDED") {
		return Refund{}, false, ErrConflict
	}
	credits, ok := proportionalCredits(order.AmountFen, order.CreditMicrocredits, amountFen)
	if !ok {
		return Refund{}, false, ErrConflict
	}
	wallet, err := getWallet(ctx, tx, order.WalletID, true)
	if err != nil || wallet.Status != "ACTIVE" {
		return Refund{}, false, ErrWalletUnavailable
	}
	var lotRemaining int64
	err = tx.QueryRow(ctx, `SELECT remaining_microcredits FROM credit_lots WHERE wallet_id=$1 AND source_reference=$2 AND refundable FOR UPDATE`, order.WalletID, "recharge:"+order.ID.String()).Scan(&lotRemaining)
	if errors.Is(err, pgx.ErrNoRows) || credits > lotRemaining || credits > wallet.AvailableMicrocredits {
		return Refund{}, false, ErrInsufficientBalance
	}
	if err != nil {
		return Refund{}, false, err
	}
	var pendingCredits int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(credit_microcredits),0) FROM refunds WHERE payment_order_id=$1 AND status IN ('REQUESTED','MANUAL_REVIEW')`, orderID).Scan(&pendingCredits); err != nil {
		return Refund{}, false, err
	}
	if credits > lotRemaining-pendingCredits {
		return Refund{}, false, ErrInsufficientBalance
	}
	now := p.now()
	refund = Refund{ID: uuid.Must(uuid.NewV7()), PaymentOrderID: orderID, RequesterID: requesterID, MerchantRefundNo: "pvr-" + uuid.Must(uuid.NewV7()).String(), AmountFen: amountFen, CreditMicrocredits: credits, Status: "REQUESTED", Reason: reason, CreatedAt: now, UpdatedAt: now}
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1 AND posted_microcredits-reserved_microcredits >= $2`, order.WalletID, credits, now)
	if err != nil || command.RowsAffected() != 1 {
		return Refund{}, false, ErrInsufficientBalance
	}
	_, err = tx.Exec(ctx, `INSERT INTO refunds(id,payment_order_id,requester_id,merchant_refund_no,amount_fen,credit_microcredits,status,idempotency_key,request_hash,reason,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'REQUESTED',$7,$8,$9,$10,$10)`, refund.ID, orderID, requesterID, refund.MerchantRefundNo, amountFen, credits, idempotencyKey, requestHash, reason, now)
	if err != nil {
		return Refund{}, false, err
	}
	return refund, true, tx.Commit(ctx)
}

func (p *PaymentCoordinator) finishRefund(ctx context.Context, requested Refund, provider ProviderRefund, created bool) (Refund, bool, error) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return Refund{}, created, err
	}
	defer tx.Rollback(ctx)
	refund, err := getRefundByID(ctx, tx, requested.ID, true)
	if err != nil {
		return Refund{}, created, err
	}
	if refund.Status != "REQUESTED" {
		return refund, created, tx.Commit(ctx)
	}
	order, err := getPaymentOrderByID(ctx, tx, refund.PaymentOrderID, true)
	if err != nil {
		return Refund{}, created, err
	}
	wallet, err := getWallet(ctx, tx, order.WalletID, true)
	if err != nil {
		return Refund{}, created, err
	}
	now := p.now()
	if provider.Status != "SUCCESS" || provider.MerchantRefundNo != refund.MerchantRefundNo || provider.AmountFen != refund.AmountFen || strings.TrimSpace(provider.ChannelRefundID) == "" {
		if provider.Status != "FAILED" || provider.MerchantRefundNo != refund.MerchantRefundNo || provider.AmountFen != refund.AmountFen {
			// An unexpected reply does not prove that no money left the channel.
			return refund, created, ErrPaymentProvider
		}
		failure := "payment provider rejected the refund"
		command, updateErr := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND reserved_microcredits >= $2`, order.WalletID, refund.CreditMicrocredits, now)
		err = updateErr
		if err == nil && command.RowsAffected() != 1 {
			return refund, created, ErrConflict
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE refunds SET status='FAILED',failure_reason=$2,updated_at=$3 WHERE id=$1`, refund.ID, failure, now)
		}
		if err != nil {
			return Refund{}, created, err
		}
		refund.Status, refund.FailureReason, refund.UpdatedAt = "FAILED", &failure, now
		return refund, created, tx.Commit(ctx)
	}
	var lotID uuid.UUID
	var lotRemaining int64
	err = tx.QueryRow(ctx, `SELECT id,remaining_microcredits FROM credit_lots WHERE wallet_id=$1 AND source_reference=$2 AND refundable FOR UPDATE`, order.WalletID, "recharge:"+order.ID.String()).Scan(&lotID, &lotRemaining)
	if err != nil {
		return Refund{}, created, err
	}
	if lotRemaining < refund.CreditMicrocredits || wallet.ReservedMicrocredits < refund.CreditMicrocredits || wallet.PostedMicrocredits < refund.CreditMicrocredits {
		_, updateErr := tx.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW',failure_reason='local refundable balance changed after provider refund',channel_refund_id=$2,updated_at=$3 WHERE id=$1`, refund.ID, provider.ChannelRefundID, now)
		if updateErr != nil {
			return Refund{}, created, updateErr
		}
		refund.Status = "MANUAL_REVIEW"
		refund.ChannelRefundID = &provider.ChannelRefundID
		return refund, created, tx.Commit(ctx)
	}
	entryID := uuid.Must(uuid.NewV7())
	if err := insertJournal(ctx, tx, entryID, "REFUND", "refund:"+refund.ID.String(), "refund original payment", now); err != nil {
		return Refund{}, created, err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, order.WalletID)
	if err == nil {
		err = insertPosting(ctx, tx, entryID, walletAccount, -refund.CreditMicrocredits, &lotID, now)
	}
	if err == nil {
		err = insertPosting(ctx, tx, entryID, refundClearingAccount, refund.CreditMicrocredits, nil, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits >= $2`, lotID, refund.CreditMicrocredits)
	}
	if err == nil {
		command, updateErr := tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-$2,revoked_microcredits=revoked_microcredits+$2,status=CASE WHEN available_microcredits=$2 AND reserved_microcredits=0 THEN 'REVOKED' ELSE status END,updated_at=$3 WHERE credit_lot_id=$1 AND available_microcredits>=$2`, lotID, refund.CreditMicrocredits, now)
		err = updateErr
		if err == nil && command.RowsAffected() != 1 {
			err = ErrConflict
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-$2,reserved_microcredits=reserved_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1`, order.WalletID, refund.CreditMicrocredits, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE refunds SET status='SUCCEEDED',channel_refund_id=$2,updated_at=$3 WHERE id=$1`, refund.ID, provider.ChannelRefundID, now)
	}
	if err != nil {
		return Refund{}, created, err
	}
	var refundedFen int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_fen),0) FROM refunds WHERE payment_order_id=$1 AND status='SUCCEEDED'`, order.ID).Scan(&refundedFen); err != nil {
		return Refund{}, created, err
	}
	orderStatus := "PARTIALLY_REFUNDED"
	fulfillment := "CREDITED"
	if refundedFen == order.AmountFen {
		orderStatus, fulfillment = "REFUNDED", "REFUNDED"
	}
	_, err = tx.Exec(ctx, `UPDATE payment_orders SET status=$2,fulfillment_status=$3,updated_at=$4 WHERE id=$1`, order.ID, orderStatus, fulfillment, now)
	if err == nil && order.ChannelTransactionID != nil {
		_, err = tx.Exec(ctx, `UPDATE payment_transactions SET status=$2,updated_at=$3 WHERE channel_transaction_id=$1`, *order.ChannelTransactionID, map[bool]string{true: "REFUNDED", false: "PARTIALLY_REFUNDED"}[refundedFen == order.AmountFen], now)
	}
	if err != nil {
		return Refund{}, created, err
	}
	refund.Status, refund.ChannelRefundID, refund.UpdatedAt = "SUCCEEDED", &provider.ChannelRefundID, now
	return refund, created, tx.Commit(ctx)
}

func proportionalCredits(orderAmountFen, orderCredits, refundAmountFen int64) (int64, bool) {
	if orderAmountFen <= 0 || orderCredits <= 0 || refundAmountFen <= 0 || refundAmountFen > orderAmountFen || refundAmountFen > math.MaxInt64/orderCredits {
		return 0, false
	}
	product := refundAmountFen * orderCredits
	if product%orderAmountFen != 0 {
		return 0, false
	}
	credits := product / orderAmountFen
	return credits, credits > 0
}

func refundMerchantOrder(ctx context.Context, database queryable, orderID uuid.UUID) (string, error) {
	var merchant string
	err := database.QueryRow(ctx, `SELECT merchant_order_no FROM payment_orders WHERE id=$1`, orderID).Scan(&merchant)
	return merchant, err
}

func getPaymentOrderByID(ctx context.Context, database queryable, orderID uuid.UUID, lock bool) (PaymentOrder, error) {
	query := `SELECT id,merchant_order_no,requester_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,COALESCE(payment_url,''),COALESCE(qr_data,''),channel_transaction_id,expires_at,paid_at,COALESCE(mobile_e164,''),COALESCE(provider_sku,'') FROM payment_orders WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var order PaymentOrder
	err := database.QueryRow(ctx, query, orderID).Scan(&order.ID, &order.MerchantOrderNo, &order.RequesterID, &order.WalletID, &order.ProductID, &order.Channel, &order.AmountFen, &order.Currency, &order.CreditMicrocredits, &order.Status, &order.FulfillmentStatus, &order.PaymentURL, &order.QRData, &order.ChannelTransactionID, &order.ExpiresAt, &order.PaidAt, &order.Mobile, &order.ProviderSKU)
	return order, err
}

func getRefundByIdempotency(ctx context.Context, database queryable, key string, lock bool) (Refund, error) {
	query := `SELECT id,payment_order_id,requester_id,merchant_refund_no,channel_refund_id,amount_fen,credit_microcredits,status,reason,failure_reason,created_at,updated_at FROM refunds WHERE idempotency_key=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanRefund(database.QueryRow(ctx, query, key))
}

func getRefundByID(ctx context.Context, database queryable, id uuid.UUID, lock bool) (Refund, error) {
	query := `SELECT id,payment_order_id,requester_id,merchant_refund_no,channel_refund_id,amount_fen,credit_microcredits,status,reason,failure_reason,created_at,updated_at FROM refunds WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanRefund(database.QueryRow(ctx, query, id))
}

func scanRefund(row pgx.Row) (Refund, error) {
	var refund Refund
	err := row.Scan(&refund.ID, &refund.PaymentOrderID, &refund.RequesterID, &refund.MerchantRefundNo, &refund.ChannelRefundID, &refund.AmountFen, &refund.CreditMicrocredits, &refund.Status, &refund.Reason, &refund.FailureReason, &refund.CreatedAt, &refund.UpdatedAt)
	return refund, err
}
