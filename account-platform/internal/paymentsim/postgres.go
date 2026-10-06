package paymentsim

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct {
	pool        *pgxpool.Pool
	publicURL   string
	tokenSecret []byte
	hmacSecrets map[string][]byte
	now         func() time.Time
}

func NewPostgresRepository(ctx context.Context, databaseURL, publicURL, tokenSecret string, hmacSecrets map[string]string) (*PostgresRepository, error) {
	if len(tokenSecret) < 32 {
		return nil, errors.New("PAYMENT_SIM_PAY_TOKEN_SECRET must contain at least 32 characters")
	}
	secrets := make(map[string][]byte, len(hmacSecrets))
	for _, channel := range []string{"UNIONPAY", "ALIPAY", "WECHAT"} {
		if len(hmacSecrets[channel]) < 32 {
			return nil, fmt.Errorf("%s HMAC secret must contain at least 32 characters", channel)
		}
		secrets[channel] = []byte(hmacSecrets[channel])
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	repository := &PostgresRepository{pool: pool, publicURL: strings.TrimRight(publicURL, "/"), tokenSecret: []byte(tokenSecret), hmacSecrets: secrets, now: func() time.Time { return time.Now().UTC() }}
	if err := repository.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }
func (r *PostgresRepository) Close()                         { r.pool.Close() }

func (r *PostgresRepository) Create(ctx context.Context, request CreateOrder) (Order, bool, error) {
	request.Channel = strings.ToUpper(request.Channel)
	request.Currency = strings.ToUpper(request.Currency)
	request.Scenario = strings.ToUpper(request.Scenario)
	if request.Scenario == "" {
		request.Scenario = "SUCCESS"
	}
	if !validChannel(request.Channel) || !validScenario(request.Scenario) || request.Currency != "CNY" || request.AmountFen <= 0 || request.MerchantOrderNo == "" || request.CallbackURL == "" || !request.ExpiresAt.After(r.now()) {
		return Order{}, false, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Order{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, request.MerchantOrderNo); err != nil {
		return Order{}, false, err
	}
	order, err := getOrderByMerchant(ctx, tx, request.MerchantOrderNo)
	if err == nil {
		if order.Channel != request.Channel || order.AmountFen != request.AmountFen || order.Currency != request.Currency || order.Scenario != request.Scenario || order.Mobile != request.Mobile || order.ProviderSKU != request.ProviderSKU {
			return Order{}, false, ErrConflict
		}
		var callbackURL string
		if err := tx.QueryRow(ctx, `SELECT callback_url FROM simulator_orders WHERE id=$1`, order.ID).Scan(&callbackURL); err != nil {
			return Order{}, false, err
		}
		if callbackURL != request.CallbackURL {
			return Order{}, false, ErrConflict
		}
		order.PaymentURL = r.paymentURL(request.MerchantOrderNo)
		return order, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, err
	}
	token := r.paymentToken(request.MerchantOrderNo)
	hash := sha256.Sum256([]byte(token))
	now := r.now()
	order = Order{ID: uuid.Must(uuid.NewV7()), MerchantOrderNo: request.MerchantOrderNo, Channel: request.Channel, AmountFen: request.AmountFen, Currency: request.Currency, Mobile: request.Mobile, ProviderSKU: request.ProviderSKU, Status: "PENDING", Scenario: request.Scenario, PaymentURL: r.publicURL + "/pay/" + token, ExpiresAt: request.ExpiresAt.UTC()}
	_, err = tx.Exec(ctx, `
INSERT INTO simulator_orders(id, merchant_order_no, channel, amount_fen, currency, status, scenario, pay_token_hash, callback_url, expires_at, created_at, updated_at,mobile,provider_sku)
VALUES ($1, $2, $3, $4, $5, 'PENDING', $6, $7, $8, $9, $10, $10,$11,$12)`, order.ID, order.MerchantOrderNo, order.Channel, order.AmountFen, order.Currency, order.Scenario, hash[:], request.CallbackURL, order.ExpiresAt, now, order.Mobile, order.ProviderSKU)
	if err != nil {
		return Order{}, false, err
	}
	return order, true, tx.Commit(ctx)
}

func (r *PostgresRepository) GetByMerchantOrder(ctx context.Context, merchantOrderNo string) (Order, error) {
	order, err := getOrderByMerchant(ctx, r.pool, merchantOrderNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err == nil {
		order.PaymentURL = r.paymentURL(merchantOrderNo)
	}
	return order, err
}

func (r *PostgresRepository) GetByPaymentToken(ctx context.Context, token string) (Order, error) {
	hash := sha256.Sum256([]byte(token))
	order, err := getOrderByTokenHash(ctx, r.pool, hash[:], false)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return order, err
}

func (r *PostgresRepository) Complete(ctx context.Context, token string) (Order, error) {
	hash := sha256.Sum256([]byte(token))
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback(ctx)
	order, err := getOrderByTokenHash(ctx, tx, hash[:], true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if order.Status == "SUCCESS" {
		return order, tx.Commit(ctx)
	}
	if order.Status != "PENDING" {
		return Order{}, ErrConflict
	}
	now := r.now()
	if !order.ExpiresAt.After(now) && order.Scenario != "LATE_SUCCESS" {
		_, err := tx.Exec(ctx, `UPDATE simulator_orders SET status = 'CLOSED', updated_at = $2 WHERE id = $1`, order.ID, now)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			return Order{}, err
		}
		return Order{}, ErrExpired
	}
	switch order.Scenario {
	case "FAIL":
		order.Status = "FAILED"
	case "CANCEL":
		order.Status = "CLOSED"
	case "TIMEOUT":
		return order, tx.Commit(ctx)
	default:
		order.Status = "SUCCESS"
	}
	if order.Status != "SUCCESS" {
		_, err := tx.Exec(ctx, `UPDATE simulator_orders SET status = $2, updated_at = $3 WHERE id = $1`, order.ID, order.Status, now)
		if err != nil {
			return Order{}, err
		}
		return order, tx.Commit(ctx)
	}
	transactionID := strings.ToLower(order.Channel) + "-" + uuid.Must(uuid.NewV7()).String()
	order.ChannelTransactionID = &transactionID
	order.PaidAt = &now
	_, err = tx.Exec(ctx, `UPDATE simulator_orders SET status = 'SUCCESS', channel_transaction_id = $2, paid_at = $3, updated_at = $3 WHERE id = $1`, order.ID, transactionID, now)
	if err != nil {
		return Order{}, err
	}
	if order.Scenario != "DROP_NOTIFY" {
		if err := r.enqueueNotification(ctx, tx, order, now); err != nil {
			return Order{}, err
		}
	}
	return order, tx.Commit(ctx)
}

func (r *PostgresRepository) CloseOrder(ctx context.Context, merchantOrderNo string) (Order, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback(ctx)
	order, err := getOrderByMerchantForUpdate(ctx, tx, merchantOrderNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if order.Status == "CLOSED" {
		return order, tx.Commit(ctx)
	}
	if order.Status != "PENDING" {
		return Order{}, ErrConflict
	}
	order.Status = "CLOSED"
	_, err = tx.Exec(ctx, `UPDATE simulator_orders SET status='CLOSED', updated_at=$2 WHERE id=$1`, order.ID, r.now())
	if err != nil {
		return Order{}, err
	}
	return order, tx.Commit(ctx)
}

func (r *PostgresRepository) Refund(ctx context.Context, merchantOrderNo, merchantRefundNo string, amountFen int64) (Refund, bool, error) {
	if strings.TrimSpace(merchantRefundNo) == "" || amountFen <= 0 {
		return Refund{}, false, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Refund{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "simulator.refund:"+merchantRefundNo); err != nil {
		return Refund{}, false, err
	}
	var existing Refund
	var existingMerchantOrderNo string
	err = tx.QueryRow(ctx, `SELECT r.merchant_refund_no,COALESCE(r.channel_refund_id,''),r.amount_fen,r.status,r.updated_at,o.merchant_order_no FROM simulator_refunds r JOIN simulator_orders o ON o.id=r.order_id WHERE r.merchant_refund_no=$1 FOR UPDATE OF r`, merchantRefundNo).Scan(&existing.MerchantRefundNo, &existing.ChannelRefundID, &existing.AmountFen, &existing.Status, &existing.UpdatedAt, &existingMerchantOrderNo)
	if err == nil {
		if existing.AmountFen != amountFen || existingMerchantOrderNo != merchantOrderNo {
			return Refund{}, false, ErrConflict
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, false, err
	}
	order, err := getOrderByMerchantForUpdate(ctx, tx, merchantOrderNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, false, ErrNotFound
	}
	if err != nil || (order.Status != "SUCCESS" && order.Status != "PARTIALLY_REFUNDED") || amountFen > order.AmountFen-order.RefundedAmountFen {
		return Refund{}, false, ErrConflict
	}
	now := r.now()
	refund := Refund{MerchantRefundNo: merchantRefundNo, AmountFen: amountFen, UpdatedAt: now}
	if order.Scenario == "REFUND_FAIL" {
		refund.Status = "FAILED"
		_, err = tx.Exec(ctx, `INSERT INTO simulator_refunds(id,order_id,merchant_refund_no,amount_fen,status,created_at,updated_at) VALUES($1,$2,$3,$4,'FAILED',$5,$5)`, uuid.Must(uuid.NewV7()), order.ID, merchantRefundNo, amountFen, now)
		if err != nil {
			return Refund{}, false, err
		}
		return refund, true, tx.Commit(ctx)
	}
	refund.Status = "SUCCESS"
	refund.ChannelRefundID = strings.ToLower(order.Channel) + "-refund-" + uuid.Must(uuid.NewV7()).String()
	newRefunded := order.RefundedAmountFen + amountFen
	newStatus := "PARTIALLY_REFUNDED"
	if newRefunded == order.AmountFen {
		newStatus = "REFUNDED"
	}
	_, err = tx.Exec(ctx, `INSERT INTO simulator_refunds(id,order_id,merchant_refund_no,amount_fen,status,channel_refund_id,created_at,updated_at) VALUES($1,$2,$3,$4,'SUCCESS',$5,$6,$6)`, uuid.Must(uuid.NewV7()), order.ID, merchantRefundNo, amountFen, refund.ChannelRefundID, now)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE simulator_orders SET refunded_amount_fen=$2,status=$3,updated_at=$4 WHERE id=$1`, order.ID, newRefunded, newStatus, now)
	}
	if err != nil {
		return Refund{}, false, err
	}
	return refund, true, tx.Commit(ctx)
}

func (r *PostgresRepository) ListBill(ctx context.Context, channel string, businessDate time.Time) ([]BillEntry, error) {
	channel = strings.ToUpper(channel)
	if !validChannel(channel) {
		return nil, ErrConflict
	}
	start := businessDate.UTC()
	end := start.AddDate(0, 0, 1)
	rows, err := r.pool.Query(ctx, `SELECT merchant_order_no,channel_transaction_id,channel,amount_fen,currency,status,paid_at FROM simulator_orders WHERE channel=$1 AND paid_at >= $2 AND paid_at < $3 AND channel_transaction_id IS NOT NULL ORDER BY paid_at,merchant_order_no`, channel, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]BillEntry, 0)
	for rows.Next() {
		var item BillEntry
		if err := rows.Scan(&item.MerchantOrderNo, &item.ChannelTransactionID, &item.Channel, &item.AmountFen, &item.Currency, &item.Status, &item.PaidAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) enqueueNotification(ctx context.Context, tx pgx.Tx, order Order, now time.Time) error {
	notificationID := uuid.Must(uuid.NewV7())
	payload := map[string]any{
		"notification_id":        notificationID,
		"merchant_id":            "pinvou-development",
		"merchant_order_no":      order.MerchantOrderNo,
		"channel_transaction_id": *order.ChannelTransactionID,
		"channel":                order.Channel,
		"status":                 "SUCCESS",
		"amount_fen":             order.AmountFen,
		"currency":               order.Currency,
		"paid_at":                order.PaidAt,
		"timestamp":              now.Unix(),
	}
	switch order.Scenario {
	case "BAD_AMOUNT":
		payload["amount_fen"] = order.AmountFen + 1
	case "BAD_CURRENCY":
		payload["currency"] = "USD"
	case "BAD_MERCHANT":
		payload["merchant_id"] = "unexpected-merchant"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	signature := sign(r.hmacSecrets[order.Channel], body)
	if order.Scenario == "BAD_SIGNATURE" {
		signature = "invalid-" + signature
	}
	deliveries := 1
	if order.Scenario == "DUPLICATE_NOTIFY" {
		deliveries = 2
	}
	_, err = tx.Exec(ctx, `INSERT INTO simulator_notifications(id, order_id, notification_id, payload, signature, status, attempts, remaining_deliveries, next_attempt_at, created_at) VALUES ($1, $2, $3, $4, $5, 'PENDING', 0, $6, $7, $7)`, uuid.Must(uuid.NewV7()), order.ID, notificationID, body, signature, deliveries, now)
	return err
}

func (r *PostgresRepository) ClaimNotifications(ctx context.Context, limit int) ([]Notification, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
WITH claimed AS (
  SELECT n.id FROM simulator_notifications n
  WHERE n.status = 'PENDING' AND n.next_attempt_at <= $1
  ORDER BY n.next_attempt_at FOR UPDATE SKIP LOCKED LIMIT $2
)
UPDATE simulator_notifications n
SET attempts = attempts + 1, next_attempt_at = $1 + interval '30 seconds'
FROM claimed c, simulator_orders o
WHERE n.id = c.id AND o.id = n.order_id
RETURNING n.id, n.order_id, o.callback_url, o.channel, n.payload, n.signature, n.attempts, n.remaining_deliveries, o.scenario`, r.now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Notification, 0)
	for rows.Next() {
		var item Notification
		var scenario string
		if err := rows.Scan(&item.ID, &item.OrderID, &item.CallbackURL, &item.Channel, &item.Payload, &item.Signature, &item.Attempts, &item.RemainingDeliveries, &scenario); err != nil {
			return nil, err
		}
		// Timestamp belongs to each signed delivery. Preserve the event ID and
		// every payment fact while refreshing it after a delayed queue/retry.
		// RawMessage preserves exact integer amounts from the persisted JSON bytes.
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			return nil, err
		}
		payload["timestamp"], _ = json.Marshal(r.now().Unix())
		item.Payload, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		// Refreshing a delivery timestamp changes bytes. Sign the actual bytes sent,
		// preserving intentionally invalid signatures in the failure scenario.
		item.Signature = sign(r.hmacSecrets[item.Channel], item.Payload)
		if scenario == "BAD_SIGNATURE" {
			item.Signature = "invalid-" + item.Signature
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) MarkNotification(ctx context.Context, notification Notification, deliveryError error) error {
	if deliveryError == nil {
		if notification.RemainingDeliveries > 1 {
			_, err := r.pool.Exec(ctx, `UPDATE simulator_notifications SET remaining_deliveries = remaining_deliveries - 1, next_attempt_at = $2, last_error = NULL WHERE id = $1 AND status='PENDING' AND attempts=$3`, notification.ID, r.now().Add(time.Second), notification.Attempts)
			return err
		}
		_, err := r.pool.Exec(ctx, `UPDATE simulator_notifications SET status = 'DELIVERED', delivered_at = $2, last_error = NULL WHERE id = $1 AND status='PENDING' AND attempts=$3`, notification.ID, r.now(), notification.Attempts)
		return err
	}
	status := "PENDING"
	if notification.Attempts >= 10 {
		status = "ABANDONED"
	}
	delay := time.Duration(min(notification.Attempts*notification.Attempts, 300)) * time.Second
	_, err := r.pool.Exec(ctx, `UPDATE simulator_notifications SET status = $2, next_attempt_at = $3, last_error = $4 WHERE id = $1 AND status='PENDING' AND attempts=$5`, notification.ID, status, r.now().Add(delay), truncate(deliveryError.Error(), 500), notification.Attempts)
	return err
}

type queryable interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getOrderByMerchant(ctx context.Context, database queryable, merchantOrderNo string) (Order, error) {
	var order Order
	err := database.QueryRow(ctx, `SELECT id, merchant_order_no, channel, amount_fen, currency, status, scenario, channel_transaction_id, expires_at, paid_at, refunded_amount_fen,mobile,provider_sku FROM simulator_orders WHERE merchant_order_no = $1`, merchantOrderNo).Scan(&order.ID, &order.MerchantOrderNo, &order.Channel, &order.AmountFen, &order.Currency, &order.Status, &order.Scenario, &order.ChannelTransactionID, &order.ExpiresAt, &order.PaidAt, &order.RefundedAmountFen, &order.Mobile, &order.ProviderSKU)
	return order, err
}

func getOrderByMerchantForUpdate(ctx context.Context, database queryable, merchantOrderNo string) (Order, error) {
	var order Order
	err := database.QueryRow(ctx, `SELECT id, merchant_order_no, channel, amount_fen, currency, status, scenario, channel_transaction_id, expires_at, paid_at, refunded_amount_fen,mobile,provider_sku FROM simulator_orders WHERE merchant_order_no = $1 FOR UPDATE`, merchantOrderNo).Scan(&order.ID, &order.MerchantOrderNo, &order.Channel, &order.AmountFen, &order.Currency, &order.Status, &order.Scenario, &order.ChannelTransactionID, &order.ExpiresAt, &order.PaidAt, &order.RefundedAmountFen, &order.Mobile, &order.ProviderSKU)
	return order, err
}

func getOrderByTokenHash(ctx context.Context, database queryable, hash []byte, lock bool) (Order, error) {
	query := `SELECT id, merchant_order_no, channel, amount_fen, currency, status, scenario, channel_transaction_id, expires_at, paid_at, refunded_amount_fen,mobile,provider_sku FROM simulator_orders WHERE pay_token_hash = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var order Order
	err := database.QueryRow(ctx, query, hash).Scan(&order.ID, &order.MerchantOrderNo, &order.Channel, &order.AmountFen, &order.Currency, &order.Status, &order.Scenario, &order.ChannelTransactionID, &order.ExpiresAt, &order.PaidAt, &order.RefundedAmountFen, &order.Mobile, &order.ProviderSKU)
	return order, err
}

func (r *PostgresRepository) paymentToken(merchantOrderNo string) string {
	digest := hmac.New(sha256.New, r.tokenSecret)
	_, _ = digest.Write([]byte(merchantOrderNo))
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

func (r *PostgresRepository) paymentURL(merchantOrderNo string) string {
	return r.publicURL + "/pay/" + r.paymentToken(merchantOrderNo)
}

func sign(secret, body []byte) string {
	digest := hmac.New(sha256.New, secret)
	_, _ = digest.Write(body)
	return base64.StdEncoding.EncodeToString(digest.Sum(nil))
}

func validChannel(channel string) bool {
	return channel == "UNIONPAY" || channel == "ALIPAY" || channel == "WECHAT"
}

func validScenario(scenario string) bool {
	switch scenario {
	case "SUCCESS", "FAIL", "CANCEL", "TIMEOUT", "LATE_SUCCESS", "DROP_NOTIFY", "DUPLICATE_NOTIFY", "BAD_AMOUNT", "BAD_CURRENCY", "BAD_MERCHANT", "BAD_SIGNATURE", "REFUND_FAIL":
		return true
	default:
		return false
	}
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}
