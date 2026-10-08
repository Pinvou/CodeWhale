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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrPaymentProvider              = errors.New("payment provider request failed")
	ErrNormalRefundCreationDisabled = errors.New("normal refund creation is disabled")
)

type Refund struct {
	ID                   uuid.UUID `json:"refund_id"`
	PaymentOrderID       uuid.UUID `json:"payment_order_id"`
	PaymentTransactionID uuid.UUID `json:"payment_transaction_id"`
	EntitlementPackageID uuid.UUID `json:"entitlement_package_id"`
	RequesterID          uuid.UUID `json:"requester_id"`
	MerchantRefundNo     string    `json:"merchant_refund_no"`
	ChannelRefundID      *string   `json:"channel_refund_id,omitempty"`
	AmountFen            int64     `json:"amount_fen,string"`
	CreditMicrocredits   int64     `json:"credit_microcredits,string"`
	BaseMicrocredits     int64     `json:"base_microcredits,string"`
	BonusMicrocredits    int64     `json:"bonus_microcredits,string"`
	Status               string    `json:"status"`
	Reason               string    `json:"reason"`
	FailureReason        *string   `json:"failure_reason,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
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
	maskClosedPaymentInstructions(&order)
	return order, nil
}

func (p *PaymentCoordinator) RecentOrder(ctx context.Context, requesterID, walletID uuid.UUID) (*PaymentOrder, error) {
	query := `SELECT id,merchant_order_no,requester_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,close_status,COALESCE(payment_url,''),COALESCE(qr_data,''),primary_provider_transaction_id,expires_at,paid_at,provider_id,provider_version,COALESCE(provider_capability_snapshot,'{}'::jsonb)
		FROM payment_orders WHERE requester_id=$1 AND wallet_id=$2 ORDER BY created_at DESC LIMIT 1`
	var order PaymentOrder
	var capabilitySnapshot []byte
	err := p.repository.pool.QueryRow(ctx, query, requesterID, walletID).Scan(&order.ID, &order.MerchantOrderNo, &order.RequesterID, &order.WalletID, &order.ProductID, &order.Channel, &order.AmountFen, &order.Currency, &order.CreditMicrocredits, &order.Status, &order.FulfillmentStatus, &order.CloseStatus, &order.PaymentURL, &order.QRData, &order.PrimaryProviderTransactionID, &order.ExpiresAt, &order.PaidAt, &order.ProviderID, &order.ProviderVersion, &capabilitySnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := applyPaymentCapabilitySnapshot(&order, capabilitySnapshot); err != nil {
		return nil, err
	}
	maskClosedPaymentInstructions(&order)
	return &order, nil
}

func (p *PaymentCoordinator) ListOrders(ctx context.Context, requesterID, walletID uuid.UUID, limit int) ([]PaymentOrderHistoryItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := p.repository.pool.Query(ctx, `SELECT po.id,po.merchant_order_no,po.product_id,COALESCE(po.product_snapshot->>'plan_name',po.product_id),COALESCE(catalog.display_name_zh_cn,''),COALESCE(catalog.display_name_en,''),COALESCE(catalog.display_name_ja,''),COALESCE(po.provider_sku,po.product_snapshot->>'sku_code',''),po.amount_fen,COALESCE((po.product_snapshot->>'display_points')::bigint,po.credit_microcredits/10000),po.channel,po.status,po.fulfillment_status,po.close_status,po.expires_at,po.paid_at,packages.credited_at,packages.valid_until,po.created_at
		FROM payment_orders po LEFT JOIN recharge_product_catalog_items catalog ON catalog.product_id=po.product_id LEFT JOIN LATERAL (SELECT min(credited_at) credited_at,max(valid_until) valid_until FROM entitlement_packages WHERE payment_order_id=po.id) packages ON true WHERE po.requester_id=$1 AND po.wallet_id=$2 ORDER BY po.created_at DESC,po.id DESC LIMIT $3`, requesterID, walletID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PaymentOrderHistoryItem, 0)
	for rows.Next() {
		var item PaymentOrderHistoryItem
		if err := rows.Scan(&item.ID, &item.ProviderOrderReference, &item.ProductID, &item.PlanName, &item.CurrentPlanNameZhCN, &item.CurrentPlanNameEN, &item.CurrentPlanNameJA, &item.SKUCode, &item.AmountFen, &item.DisplayPoints, &item.Channel, &item.Status, &item.FulfillmentStatus, &item.CloseStatus, &item.ExpiresAt, &item.PaidAt, &item.CreditedAt, &item.ValidUntil, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.PinvouOrderID, item.OrderType, item.InvoiceStatus = item.ID, "PLAN", "NOT_SUPPORTED"
		items = append(items, item)
	}
	return items, rows.Err()
}

func (p *PaymentCoordinator) CloseOrder(ctx context.Context, orderID, requesterID uuid.UUID) (PaymentOrder, error) {
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err := getPaymentOrderByID(ctx, tx, orderID, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && order.RequesterID != requesterID) {
		return PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	if p.Capabilities().ProviderID != order.ProviderID || p.Capabilities().Version != order.ProviderVersion {
		return PaymentOrder{}, ErrConflict
	}
	if order.CloseStatus == "PROVIDER_CLOSED" || order.CloseStatus == "EXPIRED" || order.CloseStatus == "LOCALLY_CLOSED_EXTERNAL_PENDING" {
		_ = tx.Commit(ctx)
		maskClosedPaymentInstructions(&order)
		return order, nil
	}
	if order.Status != "PENDING" && order.Status != "CREATED" {
		return PaymentOrder{}, ErrConflict
	}
	now := p.now()
	if _, err := tx.Exec(ctx, `UPDATE payment_orders SET close_status='CLOSE_REQUESTED',close_requested_at=COALESCE(close_requested_at,$3),payment_url=NULL,qr_data=NULL,attention_summary='PROCESSING',summary_version=summary_version+1,updated_at=$3 WHERE id=$1 AND requester_id=$2 AND close_status IN ('OPEN','CLOSE_REQUESTED')`, orderID, requesterID, now); err != nil {
		return PaymentOrder{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentOrder{}, err
	}
	if !p.Capabilities().CloseSupported {
		return p.markLocallyClosed(ctx, orderID, requesterID)
	}
	providerOrder, err := providerCall(ctx, p, "ORDER_CLOSE", order.Channel, true, providerCallScope(order), func() (ProviderOrder, error) {
		return p.provider.Close(ctx, order.MerchantOrderNo)
	})
	if err != nil {
		return PaymentOrder{}, ErrPaymentProvider
	}
	if err := p.recordProviderObservation(ctx, order.ID, "QUERY", providerOrder); err != nil {
		return PaymentOrder{}, err
	}
	if providerOrder.Status != "CLOSED" {
		return PaymentOrder{}, ErrConflict
	}
	return p.markProviderClosed(ctx, orderID, requesterID)
}

func (p *PaymentCoordinator) markLocallyClosed(ctx context.Context, orderID, requesterID uuid.UUID) (PaymentOrder, error) {
	now := p.now()
	_, err := p.repository.pool.Exec(ctx, `UPDATE payment_orders SET close_status='LOCALLY_CLOSED_EXTERNAL_PENDING',locally_closed_at=COALESCE(locally_closed_at,$3),attention_summary='PROCESSING',summary_version=summary_version+1,updated_at=$3 WHERE id=$1 AND requester_id=$2 AND close_status='CLOSE_REQUESTED'`, orderID, requesterID, now)
	if err != nil {
		return PaymentOrder{}, err
	}
	order, err := p.GetOrder(ctx, orderID, requesterID)
	return order, err
}

func (p *PaymentCoordinator) markProviderClosed(ctx context.Context, orderID, requesterID uuid.UUID) (PaymentOrder, error) {
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PaymentOrder{}, err
	}
	defer tx.Rollback(ctx)
	order, err := getPaymentOrderByID(ctx, tx, orderID, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && order.RequesterID != requesterID) {
		return PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return PaymentOrder{}, err
	}
	now := p.now()
	if order.CloseStatus == "CLOSE_REQUESTED" {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET close_status='LOCALLY_CLOSED_EXTERNAL_PENDING',locally_closed_at=COALESCE(locally_closed_at,$2),updated_at=$2 WHERE id=$1 AND close_status='CLOSE_REQUESTED'`, orderID, now); err != nil {
			return PaymentOrder{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders SET close_status='PROVIDER_CLOSED',provider_closed_at=COALESCE(provider_closed_at,$2),status='CLOSED',payment_url=NULL,qr_data=NULL,attention_summary='NORMAL',summary_version=summary_version+1,updated_at=$2 WHERE id=$1 AND status IN ('CREATED','PENDING') AND close_status='LOCALLY_CLOSED_EXTERNAL_PENDING'`, orderID, now); err != nil {
		return PaymentOrder{}, err
	}
	order, err = getPaymentOrderByID(ctx, tx, orderID, false)
	if err != nil {
		return PaymentOrder{}, err
	}
	maskClosedPaymentInstructions(&order)
	return order, tx.Commit(ctx)
}

func maskClosedPaymentInstructions(order *PaymentOrder) {
	if order != nil && (order.Status == "EXPIRED" || (order.CloseStatus != "" && order.CloseStatus != "OPEN")) {
		order.PaymentURL, order.QRData = "", ""
	}
}

func (p *PaymentCoordinator) CreateRefund(ctx context.Context, orderID, requesterID, transactionID, packageID uuid.UUID, idempotencyKey, reason string) (Refund, bool, error) {
	if !p.NormalRefundCreationEnabled() {
		return Refund{}, false, ErrNormalRefundCreationDisabled
	}
	if !p.Capabilities().RefundSupported {
		return Refund{}, false, payment.ErrUnsupported
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	reason = strings.TrimSpace(reason)
	if orderID == uuid.Nil || requesterID == uuid.Nil || transactionID == uuid.Nil || packageID == uuid.Nil || len(idempotencyKey) < 16 || reason == "" || len(reason) > 500 {
		return Refund{}, false, ErrConflict
	}
	digest := sha256.Sum256([]byte(orderID.String() + "|" + requesterID.String() + "|" + transactionID.String() + "|" + packageID.String() + "|" + reason))
	requestHash := hex.EncodeToString(digest[:])
	refund, created, err := p.reserveRefund(ctx, orderID, requesterID, transactionID, packageID, idempotencyKey, requestHash, reason)
	return refund, created, err
}

func (p *PaymentCoordinator) ProcessRefund(ctx context.Context, refundID uuid.UUID) error {
	_, err := p.processRefund(ctx, refundID, false)
	return err
}

func (p *PaymentCoordinator) processRefund(ctx context.Context, refundID uuid.UUID, reconcile bool) (Refund, error) {
	if refundID == uuid.Nil {
		return Refund{}, ErrConflict
	}
	refund, err := getRefundByID(ctx, p.repository.pool, refundID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, ErrNotFound
	}
	if err != nil || refund.Status != "REQUESTED" && !(reconcile && refund.Status == "MANUAL_REVIEW") {
		return refund, err
	}
	if !p.Capabilities().RefundSupported || !p.Capabilities().RefundIdempotencySupported {
		return refund, payment.ErrUnsupported
	}
	// Persist the possible-dispatch boundary before taking the financial gate.
	// The stable merchant refund number is the provider's idempotency key.
	claimed, err := p.repository.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='IN_PROGRESS',dispatch_started_at=COALESCE(dispatch_started_at,$2),dispatch_lease_until=$2+interval '2 minutes' WHERE id=$1 AND status IN ('REQUESTED','MANUAL_REVIEW') AND (dispatch_lease_until IS NULL OR dispatch_lease_until<$2)`, refundID, p.now())
	if err != nil {
		return refund, err
	}
	if claimed.RowsAffected() != 1 {
		return refund, ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return refund, err
	}
	defer tx.Rollback(context.Background())
	order, err := getPaymentOrderByID(ctx, tx, refund.PaymentOrderID, true)
	if err != nil {
		return refund, err
	}
	if p.Capabilities().ProviderID != order.ProviderID || p.Capabilities().Version != order.ProviderVersion {
		return refund, ErrConflict
	}
	refund, err = getRefundByID(ctx, tx, refundID, true)
	if err != nil {
		return refund, err
	}
	if refund.Status != "REQUESTED" && !(reconcile && refund.Status == "MANUAL_REVIEW") {
		return refund, ErrConflict
	}
	var blocked bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chargeback_cases WHERE entitlement_package_id=$1 AND status<>'DISMISSED') OR EXISTS(SELECT 1 FROM forced_refund_cases WHERE entitlement_package_id=$1 AND resolution_code IS DISTINCT FROM 'DISMISS') OR EXISTS(SELECT 1 FROM receivables WHERE wallet_id=$2 AND outstanding_microcredits>0)`, refund.EntitlementPackageID, order.WalletID).Scan(&blocked)
	if err != nil {
		return refund, err
	}
	if blocked {
		if _, err := tx.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=NULL WHERE id=$1`, refundID); err != nil {
			return refund, err
		}
		if err := tx.Commit(ctx); err != nil {
			return refund, err
		}
		return refund, ErrConflict
	}
	merchantOrderNo, err := refundMerchantOrder(ctx, tx, refund.PaymentOrderID)
	if err != nil {
		return refund, err
	}
	providerRefund, providerErr := providerCall(ctx, p, "REFUND", "default", true, ProviderCallScope{OrderID: refund.PaymentOrderID, SubjectID: refund.RequesterID}, func() (ProviderRefund, error) {
		return p.provider.Refund(ctx, merchantOrderNo, refund.MerchantRefundNo, refund.AmountFen)
	})
	if providerErr != nil {
		// JetStream may redeliver this event. The stable merchant refund number
		// makes that safe only because the selected Provider explicitly declared
		// refund idempotency support.
		if _, err := tx.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=NULL WHERE id=$1`, refundID); err != nil {
			return refund, err
		}
		if err := tx.Commit(ctx); err != nil {
			return refund, err
		}
		return refund, ErrPaymentProvider
	}
	if reconcile {
		// The trusted idempotent provider result, not the human's declaration,
		// must prove the terminal outcome before re-entering local settlement.
		if providerRefund.MerchantRefundNo != refund.MerchantRefundNo || providerRefund.AmountFen != refund.AmountFen || providerRefund.Status != "SUCCESS" && providerRefund.Status != "FAILED" || providerRefund.Status == "SUCCESS" && strings.TrimSpace(providerRefund.ChannelRefundID) == "" {
			if _, err := tx.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=NULL WHERE id=$1`, refundID); err != nil {
				return refund, err
			}
			if err := tx.Commit(ctx); err != nil {
				return refund, err
			}
			return refund, ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE refunds SET status='REQUESTED' WHERE id=$1 AND status='MANUAL_REVIEW'`, refundID); err != nil {
			return refund, err
		}
	}
	result, _, err := p.finishRefundTransaction(ctx, tx, refund, providerRefund, false)
	return result, err
}

func (p *PaymentCoordinator) reserveRefund(ctx context.Context, orderID, requesterID, transactionID, packageID uuid.UUID, idempotencyKey, requestHash, reason string) (Refund, bool, error) {
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Refund{}, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment.refund", idempotencyKey); err != nil {
		return Refund{}, false, err
	}
	refund, err := getRefundByIdempotency(ctx, tx, idempotencyKey, true)
	if err == nil {
		if refund.PaymentOrderID != orderID || refund.RequesterID != requesterID || refund.PaymentTransactionID != transactionID || refund.EntitlementPackageID != packageID {
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
	if err != nil || order.FulfillmentStatus != "CREDITED" || (order.Status != "PAID" && order.Status != "PAID_LATE") {
		return Refund{}, false, ErrConflict
	}
	wallet, err := getWallet(ctx, tx, order.WalletID, true)
	if err != nil || wallet.Status != "ACTIVE" || wallet.ReceivableMicrocredits != 0 {
		return Refund{}, false, ErrWalletUnavailable
	}
	var amountFen int64
	err = tx.QueryRow(ctx, `SELECT transaction.amount_fen
		FROM payment_transactions transaction
		JOIN entitlement_packages package
		  ON package.source_payment_transaction_id=transaction.id
		 AND package.payment_order_id=transaction.payment_order_id
		WHERE transaction.id=$1 AND transaction.payment_order_id=$2 AND package.id=$3
		  AND transaction.status IN ('PAID','LATE_PAID')
		  AND transaction.fulfillment_status='CREDITED'
		  AND package.wallet_id=$4 AND package.status='ACTIVE'
		FOR NO KEY UPDATE OF transaction FOR UPDATE OF package`, transactionID, orderID, packageID, order.WalletID).Scan(&amountFen)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, false, ErrConflict
	}
	if err != nil {
		return Refund{}, false, err
	}
	var abnormal bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM chargeback_cases WHERE payment_transaction_id=$1 AND status<>'DISMISSED')
		OR EXISTS(SELECT 1 FROM forced_refund_cases WHERE payment_transaction_id=$1 AND status='REVIEW_REQUIRED')
		OR EXISTS(SELECT 1 FROM refunds WHERE payment_transaction_id=$1 AND entitlement_package_id=$3 AND status IN ('REQUESTED','SUCCEEDED','MANUAL_REVIEW'))
		OR EXISTS(SELECT 1 FROM receivables WHERE payment_order_id=$2 AND outstanding_microcredits>0)
		OR EXISTS(SELECT 1 FROM payment_orders WHERE id=$2 AND attention_summary<>'NORMAL')
		OR EXISTS(
			SELECT 1 FROM usage_hold_allocations allocation
			JOIN usage_holds hold ON hold.id=allocation.hold_id
			WHERE allocation.package_id=$3 AND hold.status IN ('ACTIVE','REVIEW')
			  AND allocation.reserved_microcredits>allocation.settled_microcredits+allocation.released_microcredits
		)`, transactionID, orderID, packageID).Scan(&abnormal); err != nil {
		return Refund{}, false, err
	}
	if abnormal {
		return Refund{}, false, ErrConflict
	}
	type componentReservation struct {
		id, lotID                    uuid.UUID
		kind, status                 string
		available, active, suspended int64
	}
	rows, err := tx.Query(ctx, `SELECT id,credit_lot_id,component_type,status,available_microcredits,active_grant_microcredits,suspended_grant_microcredits FROM entitlement_components WHERE package_id=$1 AND used_microcredits=0 AND reserved_microcredits=0 AND expired_microcredits=0 AND revoked_microcredits=0 ORDER BY id FOR UPDATE`, packageID)
	if err != nil {
		return Refund{}, false, err
	}
	components := []componentReservation{}
	var base, bonus int64
	for rows.Next() {
		var item componentReservation
		if err := rows.Scan(&item.id, &item.lotID, &item.kind, &item.status, &item.available, &item.active, &item.suspended); err != nil {
			rows.Close()
			return Refund{}, false, err
		}
		total := item.available + item.active + item.suspended
		if total <= 0 || (item.kind != "BASE" && item.kind != "BONUS") {
			rows.Close()
			return Refund{}, false, ErrConflict
		}
		if item.kind == "BASE" {
			base += total
		} else {
			bonus += total
		}
		components = append(components, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Refund{}, false, err
	}
	if len(components) == 0 || base == 0 {
		return Refund{}, false, ErrConflict
	}
	var componentCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM entitlement_components WHERE package_id=$1`, packageID).Scan(&componentCount); err != nil {
		return Refund{}, false, err
	}
	if componentCount != len(components) {
		return Refund{}, false, ErrConflict
	}
	var invalidGrant bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM member_quota_grants WHERE package_id=$1 AND (used_microcredits>0 OR reserved_microcredits>0 OR expired_microcredits>0 OR revoked_microcredits>0))`, packageID).Scan(&invalidGrant); err != nil {
		return Refund{}, false, err
	}
	if invalidGrant {
		return Refund{}, false, ErrConflict
	}
	now := p.now()
	refund = Refund{ID: uuid.Must(uuid.NewV7()), PaymentOrderID: orderID, PaymentTransactionID: transactionID, EntitlementPackageID: packageID, RequesterID: requesterID, MerchantRefundNo: "pvr-" + uuid.Must(uuid.NewV7()).String(), AmountFen: amountFen, CreditMicrocredits: base + bonus, BaseMicrocredits: base, BonusMicrocredits: bonus, Status: "REQUESTED", Reason: reason, CreatedAt: now, UpdatedAt: now}
	snapshot, err := json.Marshal(map[string]any{"payment_transaction_id": transactionID, "entitlement_package_id": packageID, "base_microcredits": base, "bonus_microcredits": bonus, "amount_fen": amountFen})
	if err != nil {
		return Refund{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO refunds(id,payment_order_id,payment_transaction_id,entitlement_package_id,requester_id,merchant_refund_no,amount_fen,credit_microcredits,base_microcredits,bonus_microcredits,status,idempotency_key,request_hash,reason,entitlement_snapshot,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'REQUESTED',$11,$12,$13,$14,$15,$15)`, refund.ID, orderID, transactionID, packageID, requesterID, refund.MerchantRefundNo, amountFen, base+bonus, base, bonus, idempotencyKey, requestHash, reason, snapshot, now)
	if err != nil {
		return Refund{}, false, err
	}
	for _, component := range components {
		if _, err := tx.Exec(ctx, `INSERT INTO refund_component_reservations(refund_id,component_id,package_id,component_type,credit_lot_id,available_microcredits,active_grant_microcredits,suspended_grant_microcredits,prior_status,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, refund.ID, component.id, packageID, component.kind, component.lotID, component.available, component.active, component.suspended, component.status, now); err != nil {
			return Refund{}, false, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO refund_grant_reservations(refund_id,grant_id,component_id,available_microcredits,prior_status,prior_suspension_reason,created_at) SELECT $1,id,component_id,available_microcredits,status,suspension_reason,$2 FROM member_quota_grants WHERE package_id=$3`, refund.ID, now, packageID); err != nil {
		return Refund{}, false, err
	}
	command, err := tx.Exec(ctx, `UPDATE entitlement_packages SET status='FROZEN',updated_at=$2 WHERE id=$1 AND status='ACTIVE'`, packageID, now)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return Refund{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE entitlement_components SET status='FROZEN',updated_at=$2 WHERE package_id=$1`, packageID, now); err != nil {
		return Refund{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE member_quota_grants SET status='SUSPENDED',suspension_reason='REFUND_PENDING',allocation_version=allocation_version+1,updated_at=$2 WHERE package_id=$1 AND status='ACTIVE'`, packageID, now); err != nil {
		return Refund{}, false, err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.refund.requested.v1", refund.ID, 1, map[string]any{"refund_id": refund.ID, "payment_order_id": orderID, "payment_transaction_id": transactionID, "entitlement_package_id": packageID, "amount_fen": amountFen}); err != nil {
		return Refund{}, false, err
	}
	return refund, true, tx.Commit(ctx)
}

func (p *PaymentCoordinator) finishRefund(ctx context.Context, requested Refund, provider ProviderRefund, created bool) (Refund, bool, error) {
	tx, err := p.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Refund{}, created, err
	}
	defer tx.Rollback(ctx)
	return p.finishRefundTransaction(ctx, tx, requested, provider, created)
}

func (p *PaymentCoordinator) finishRefundTransaction(ctx context.Context, tx pgx.Tx, requested Refund, provider ProviderRefund, created bool) (Refund, bool, error) {
	initial, err := getRefundByID(ctx, tx, requested.ID, false)
	if err != nil {
		return Refund{}, created, err
	}
	// Financial writers acquire the shared wallet/order gate before a refund
	// row, because forced review can update that row while holding the wallet.
	order, err := getPaymentOrderByID(ctx, tx, initial.PaymentOrderID, true)
	if err != nil {
		return Refund{}, created, err
	}
	wallet, err := getWallet(ctx, tx, order.WalletID, false)
	if err != nil {
		return Refund{}, created, err
	}
	refund, err := getRefundByID(ctx, tx, requested.ID, true)
	if err != nil || refund.PaymentOrderID != order.ID || refund.PaymentTransactionID != initial.PaymentTransactionID || refund.EntitlementPackageID != initial.EntitlementPackageID {
		return Refund{}, created, ErrConflict
	}
	if refund.Status != "REQUESTED" {
		return refund, created, tx.Commit(ctx)
	}
	now := p.now()
	if provider.MerchantRefundNo != refund.MerchantRefundNo || provider.AmountFen != refund.AmountFen || (provider.Status == "SUCCESS" && strings.TrimSpace(provider.ChannelRefundID) == "") {
		failure := "payment provider returned conflicting refund evidence"
		if _, err = tx.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=NULL,status='MANUAL_REVIEW',failure_reason=$2,channel_refund_id=NULLIF($3,''),intent_version=intent_version+1,updated_at=$4 WHERE id=$1`, refund.ID, failure, provider.ChannelRefundID, now); err != nil {
			return Refund{}, created, err
		}
		if err = events.AppendOutbox(ctx, tx, "billing.refund.review-required.v1", refund.ID, 2, map[string]any{"refund_id": refund.ID, "payment_transaction_id": refund.PaymentTransactionID, "entitlement_package_id": refund.EntitlementPackageID, "failure_reason": failure}); err != nil {
			return Refund{}, created, err
		}
		if _, err = createPaymentAnomalyWithinTransaction(ctx, tx, "REFUND_FAILURE", "P1", "refund:"+refund.ID.String()+":provider-conflict", paymentAnomalyReference{PaymentOrderID: &refund.PaymentOrderID, PaymentTransactionID: &refund.PaymentTransactionID, RefundID: &refund.ID}, map[string]any{"failure_reason": failure, "provider_status": provider.Status, "provider_refund_id": provider.ChannelRefundID}, now); err != nil {
			return Refund{}, created, err
		}
		refund.Status, refund.FailureReason, refund.UpdatedAt = "MANUAL_REVIEW", &failure, now
		if strings.TrimSpace(provider.ChannelRefundID) != "" {
			refund.ChannelRefundID = &provider.ChannelRefundID
		}
		return refund, created, tx.Commit(ctx)
	}
	if provider.Status != "SUCCESS" && provider.Status != "FAILED" {
		return refund, created, ErrPaymentProvider
	}
	if provider.Status == "FAILED" {
		failure := "payment provider rejected the refund"
		err = restoreRefundEntitlements(ctx, tx, refund.ID, refund.EntitlementPackageID, now)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE refunds SET dispatch_state='COMPLETED',dispatch_lease_until=NULL,status='FAILED',failure_reason=$2,intent_version=intent_version+1,updated_at=$3 WHERE id=$1`, refund.ID, failure, now)
		}
		if err == nil {
			err = events.AppendOutbox(ctx, tx, "billing.refund.failed.v1", refund.ID, 2, map[string]any{"refund_id": refund.ID, "payment_transaction_id": refund.PaymentTransactionID, "entitlement_package_id": refund.EntitlementPackageID, "failure_reason": failure})
		}
		if err == nil {
			_, err = createPaymentAnomalyWithinTransaction(ctx, tx, "REFUND_FAILURE", "P2", "refund:"+refund.ID.String()+":provider-rejected", paymentAnomalyReference{PaymentOrderID: &refund.PaymentOrderID, PaymentTransactionID: &refund.PaymentTransactionID, RefundID: &refund.ID}, map[string]any{"failure_reason": failure, "provider_status": provider.Status}, now)
		}
		if err != nil {
			return Refund{}, created, err
		}
		refund.Status, refund.FailureReason, refund.UpdatedAt = "FAILED", &failure, now
		return refund, created, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT component_id,credit_lot_id,component_type,available_microcredits,active_grant_microcredits,suspended_grant_microcredits FROM refund_component_reservations WHERE refund_id=$1 ORDER BY component_id FOR UPDATE`, refund.ID)
	if err != nil {
		return Refund{}, created, err
	}
	type reservedComponent struct {
		id, lotID                    uuid.UUID
		kind                         string
		available, active, suspended int64
	}
	components := []reservedComponent{}
	var total int64
	for rows.Next() {
		var item reservedComponent
		if err := rows.Scan(&item.id, &item.lotID, &item.kind, &item.available, &item.active, &item.suspended); err != nil {
			rows.Close()
			return Refund{}, created, err
		}
		total += item.available + item.active + item.suspended
		components = append(components, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Refund{}, created, err
	}
	if len(components) == 0 || total != refund.CreditMicrocredits || wallet.PostedMicrocredits < total {
		_, updateErr := tx.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=NULL,status='MANUAL_REVIEW',failure_reason='local refundable balance changed after provider refund',channel_refund_id=$2,intent_version=intent_version+1,updated_at=$3 WHERE id=$1`, refund.ID, provider.ChannelRefundID, now)
		if updateErr != nil {
			return Refund{}, created, updateErr
		}
		if updateErr = events.AppendOutbox(ctx, tx, "billing.refund.review-required.v1", refund.ID, 2, map[string]any{"refund_id": refund.ID, "payment_transaction_id": refund.PaymentTransactionID, "entitlement_package_id": refund.EntitlementPackageID}); updateErr != nil {
			return Refund{}, created, updateErr
		}
		if _, updateErr = createPaymentAnomalyWithinTransaction(ctx, tx, "REFUND_FAILURE", "P0", "refund:"+refund.ID.String()+":local-conflict", paymentAnomalyReference{PaymentOrderID: &refund.PaymentOrderID, PaymentTransactionID: &refund.PaymentTransactionID, RefundID: &refund.ID}, map[string]any{"failure_reason": "local refundable balance changed after provider refund", "provider_refund_id": provider.ChannelRefundID}, now); updateErr != nil {
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
	for _, component := range components {
		amount := component.available + component.active + component.suspended
		if err == nil {
			err = insertPosting(ctx, tx, entryID, walletAccount, -amount, &component.lotID, now)
		}
		if err == nil {
			command, updateErr := tx.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=remaining_microcredits-$2 WHERE id=$1 AND remaining_microcredits>=$2`, component.lotID, amount)
			err = updateErr
			if err == nil && command.RowsAffected() != 1 {
				err = ErrConflict
			}
		}
		if err == nil {
			command, updateErr := tx.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=0,active_grant_microcredits=0,suspended_grant_microcredits=0,revoked_microcredits=revoked_microcredits+$2,status='REVOKED',updated_at=$3 WHERE id=$1 AND status='FROZEN' AND available_microcredits=$4 AND active_grant_microcredits=$5 AND suspended_grant_microcredits=$6 AND used_microcredits=0 AND reserved_microcredits=0 AND expired_microcredits=0`, component.id, amount, now, component.available, component.active, component.suspended)
			err = updateErr
			if err == nil && command.RowsAffected() != 1 {
				err = ErrConflict
			}
		}
	}
	if err == nil {
		var expectedGrants int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM refund_grant_reservations WHERE refund_id=$1`, refund.ID).Scan(&expectedGrants); err == nil {
			command, updateErr := tx.Exec(ctx, `UPDATE member_quota_grants member_grant SET revoked_microcredits=revoked_microcredits+reservation.available_microcredits,available_microcredits=0,status='REVOKED',suspension_reason=NULL,allocation_version=allocation_version+1,updated_at=$2 FROM refund_grant_reservations reservation WHERE reservation.refund_id=$1 AND reservation.grant_id=member_grant.id AND member_grant.available_microcredits=reservation.available_microcredits AND member_grant.used_microcredits=0 AND member_grant.reserved_microcredits=0 AND member_grant.expired_microcredits=0 AND member_grant.revoked_microcredits=0 AND member_grant.status='SUSPENDED' AND member_grant.suspension_reason='REFUND_PENDING'`, refund.ID, now)
			err = updateErr
			if err == nil && command.RowsAffected() != expectedGrants {
				err = ErrConflict
			}
		}
	}

	if err == nil {
		err = insertPosting(ctx, tx, entryID, refundClearingAccount, total, nil, now)
	}
	if err == nil {
		command, updateErr := tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND posted_microcredits>=$2`, order.WalletID, total, now)
		err = updateErr
		if err == nil && command.RowsAffected() != 1 {
			err = ErrConflict
		}
	}
	if err == nil {
		command, updateErr := tx.Exec(ctx, `UPDATE entitlement_packages SET status='REVOKED',updated_at=$3 WHERE id=$1 AND status='FROZEN' AND points_remaining=0 AND points_revoked=$2 AND points_used=0 AND points_reserved=0 AND points_expired=0`, refund.EntitlementPackageID, total, now)
		err = updateErr
		if err == nil && command.RowsAffected() != 1 {
			err = ErrConflict
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE refunds SET dispatch_state='COMPLETED',dispatch_lease_until=NULL,status='SUCCEEDED',channel_refund_id=$2,intent_version=intent_version+1,updated_at=$3 WHERE id=$1`, refund.ID, provider.ChannelRefundID, now)
	}
	if err == nil {
		err = events.AppendOutbox(ctx, tx, "billing.refund.succeeded.v1", refund.ID, 2, map[string]any{"refund_id": refund.ID, "payment_order_id": refund.PaymentOrderID, "payment_transaction_id": refund.PaymentTransactionID, "entitlement_package_id": refund.EntitlementPackageID, "amount_fen": refund.AmountFen, "base_microcredits": refund.BaseMicrocredits, "bonus_microcredits": refund.BonusMicrocredits})
	}
	if err != nil {
		return Refund{}, created, err
	}
	if _, err = projectPaymentOrderWithinTransaction(ctx, tx, order.ID, now); err != nil {
		return Refund{}, created, err
	}
	refund.Status, refund.ChannelRefundID, refund.UpdatedAt = "SUCCEEDED", &provider.ChannelRefundID, now
	return refund, created, tx.Commit(ctx)
}

func restoreRefundEntitlements(ctx context.Context, tx pgx.Tx, refundID, packageID uuid.UUID, now time.Time) error {
	// finishRefund already holds the wallet/order gate shared by evidence writers.
	// Ending this refund must not release another still-live financial fence.
	var canRestore bool
	if err := tx.QueryRow(ctx, `SELECT package.status='FROZEN' AND wallet.status='ACTIVE'
		AND NOT EXISTS(SELECT 1 FROM chargeback_cases WHERE entitlement_package_id=package.id AND status='REVIEW_REQUIRED')
		AND NOT EXISTS(SELECT 1 FROM forced_refund_cases WHERE entitlement_package_id=package.id AND status='REVIEW_REQUIRED')
		AND NOT EXISTS(SELECT 1 FROM refunds WHERE entitlement_package_id=package.id AND id<>$2 AND status IN ('REQUESTED','MANUAL_REVIEW'))
		FROM entitlement_packages package JOIN wallets wallet ON wallet.id=package.wallet_id WHERE package.id=$1`, packageID, refundID).Scan(&canRestore); err != nil {
		return err
	}
	if !canRestore {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE member_quota_grants member_grant SET status=reservation.prior_status,suspension_reason=reservation.prior_suspension_reason,allocation_version=allocation_version+1,updated_at=$3 FROM refund_grant_reservations reservation WHERE reservation.refund_id=$1 AND reservation.grant_id=member_grant.id AND member_grant.package_id=$2 AND member_grant.status='SUSPENDED' AND (member_grant.suspension_reason='REFUND_PENDING' OR reservation.prior_status='SUSPENDED')`, refundID, packageID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_components component SET status=reservation.prior_status,updated_at=$3 FROM refund_component_reservations reservation WHERE reservation.refund_id=$1 AND reservation.component_id=component.id AND component.package_id=$2 AND component.status='FROZEN'`, refundID, packageID, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE entitlement_packages SET status='ACTIVE',updated_at=$2 WHERE id=$1 AND status='FROZEN'`, packageID, now)
	return err
}

func refundMerchantOrder(ctx context.Context, database queryable, orderID uuid.UUID) (string, error) {
	var merchant string
	err := database.QueryRow(ctx, `SELECT merchant_order_no FROM payment_orders WHERE id=$1`, orderID).Scan(&merchant)
	return merchant, err
}

func getPaymentOrderByID(ctx context.Context, database queryable, orderID uuid.UUID, lock bool) (PaymentOrder, error) {
	query := `SELECT id,merchant_order_no,requester_id,beneficiary_organization_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,close_status,COALESCE(payment_url,''),COALESCE(qr_data,''),primary_provider_transaction_id,expires_at,paid_at,COALESCE(provider_sku,''),provider_id,provider_version,COALESCE(provider_capability_snapshot,'{}'::jsonb) FROM payment_orders WHERE id=$1`
	args := []any{orderID}
	if lock {
		initial, err := getPaymentOrderByID(ctx, database, orderID, false)
		if err != nil {
			return PaymentOrder{}, err
		}
		if _, err := getWallet(ctx, database, initial.WalletID, true); err != nil {
			return PaymentOrder{}, err
		}
		query += ` AND wallet_id=$2 FOR NO KEY UPDATE`
		args = append(args, initial.WalletID)
	}
	var order PaymentOrder
	var capabilitySnapshot []byte
	err := database.QueryRow(ctx, query, args...).Scan(&order.ID, &order.MerchantOrderNo, &order.RequesterID, &order.BeneficiaryOrganizationID, &order.WalletID, &order.ProductID, &order.Channel, &order.AmountFen, &order.Currency, &order.CreditMicrocredits, &order.Status, &order.FulfillmentStatus, &order.CloseStatus, &order.PaymentURL, &order.QRData, &order.PrimaryProviderTransactionID, &order.ExpiresAt, &order.PaidAt, &order.ProviderSKU, &order.ProviderID, &order.ProviderVersion, &capabilitySnapshot)
	if err == nil {
		err = applyPaymentCapabilitySnapshot(&order, capabilitySnapshot)
	}
	return order, err
}

func applyPaymentCapabilitySnapshot(order *PaymentOrder, snapshot []byte) error {
	var capabilities struct {
		QRScanApps []string `json:"qr_scan_apps"`
	}
	if len(snapshot) == 0 {
		order.QRScanApps = nil
		return nil
	}
	if err := json.Unmarshal(snapshot, &capabilities); err != nil {
		return fmt.Errorf("decode payment capability snapshot: %w", err)
	}
	normalized := make([]string, 0, len(capabilities.QRScanApps))
	seen := make(map[string]bool, len(capabilities.QRScanApps))
	for _, app := range capabilities.QRScanApps {
		app = strings.ToUpper(strings.TrimSpace(app))
		if !validPaymentChannel(app) {
			order.QRScanApps = nil
			return nil
		}
		if !seen[app] {
			normalized = append(normalized, app)
			seen[app] = true
		}
	}
	order.QRScanApps = normalized
	return nil
}

func getRefundByIdempotency(ctx context.Context, database queryable, key string, lock bool) (Refund, error) {
	query := `SELECT id,payment_order_id,payment_transaction_id,entitlement_package_id,requester_id,merchant_refund_no,channel_refund_id,amount_fen,credit_microcredits,base_microcredits,bonus_microcredits,status,reason,failure_reason,created_at,updated_at FROM refunds WHERE idempotency_key=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanRefund(database.QueryRow(ctx, query, key))
}

func getRefundByID(ctx context.Context, database queryable, id uuid.UUID, lock bool) (Refund, error) {
	query := `SELECT id,payment_order_id,payment_transaction_id,entitlement_package_id,requester_id,merchant_refund_no,channel_refund_id,amount_fen,credit_microcredits,base_microcredits,bonus_microcredits,status,reason,failure_reason,created_at,updated_at FROM refunds WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanRefund(database.QueryRow(ctx, query, id))
}

func scanRefund(row pgx.Row) (Refund, error) {
	var refund Refund
	err := row.Scan(&refund.ID, &refund.PaymentOrderID, &refund.PaymentTransactionID, &refund.EntitlementPackageID, &refund.RequesterID, &refund.MerchantRefundNo, &refund.ChannelRefundID, &refund.AmountFen, &refund.CreditMicrocredits, &refund.BaseMicrocredits, &refund.BonusMicrocredits, &refund.Status, &refund.Reason, &refund.FailureReason, &refund.CreatedAt, &refund.UpdatedAt)
	return refund, err
}
