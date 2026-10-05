package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PaymentOrder struct {
	ID                   uuid.UUID  `json:"order_id"`
	MerchantOrderNo      string     `json:"merchant_order_no"`
	RequesterID          uuid.UUID  `json:"requester_id"`
	WalletID             uuid.UUID  `json:"wallet_id"`
	ProductID            string     `json:"product_id"`
	Channel              string     `json:"channel"`
	AmountFen            int64      `json:"amount_fen"`
	Currency             string     `json:"currency"`
	CreditMicrocredits   int64      `json:"credit_microcredits"`
	Status               string     `json:"status"`
	FulfillmentStatus    string     `json:"fulfillment_status"`
	PaymentURL           string     `json:"payment_url,omitempty"`
	ChannelTransactionID *string    `json:"channel_transaction_id,omitempty"`
	ExpiresAt            time.Time  `json:"expires_at"`
	PaidAt               *time.Time `json:"paid_at,omitempty"`
}

type PaymentCoordinator struct {
	repository  *PostgresRepository
	provider    PaymentProvider
	secrets     map[string][]byte
	merchantID  string
	callbackURL string
	now         func() time.Time
}

type ProviderOrder struct {
	MerchantOrderNo      string     `json:"merchant_order_no"`
	PaymentURL           string     `json:"payment_url"`
	Status               string     `json:"status"`
	Channel              string     `json:"channel"`
	AmountFen            int64      `json:"amount_fen"`
	Currency             string     `json:"currency"`
	ChannelTransactionID *string    `json:"channel_transaction_id,omitempty"`
	PaidAt               *time.Time `json:"paid_at,omitempty"`
}

type ProviderRefund struct {
	MerchantRefundNo string `json:"merchant_refund_no"`
	ChannelRefundID  string `json:"channel_refund_id,omitempty"`
	AmountFen        int64  `json:"amount_fen"`
	Status           string `json:"status"`
}

type ProviderBillEntry struct {
	MerchantOrderNo      string    `json:"merchant_order_no"`
	ChannelTransactionID string    `json:"channel_transaction_id"`
	Channel              string    `json:"channel"`
	AmountFen            int64     `json:"amount_fen"`
	Currency             string    `json:"currency"`
	Status               string    `json:"status"`
	PaidAt               time.Time `json:"paid_at"`
}

type PaymentProvider interface {
	Create(context.Context, PaymentOrder, string) (ProviderOrder, error)
	Get(context.Context, string) (ProviderOrder, error)
	Close(context.Context, string) (ProviderOrder, error)
	Refund(context.Context, string, string, int64) (ProviderRefund, error)
	Bill(context.Context, string, time.Time) ([]ProviderBillEntry, error)
}

func NewPaymentCoordinator(repository *PostgresRepository, providerURL, serviceToken, merchantID, callbackURL string, secrets map[string]string) (*PaymentCoordinator, error) {
	provider, err := NewSimulatorProvider(providerURL, serviceToken)
	if err != nil {
		return nil, err
	}
	parsedSecrets := make(map[string][]byte)
	for _, channel := range []string{"UNIONPAY", "ALIPAY", "WECHAT"} {
		if len(secrets[channel]) < 32 {
			return nil, fmt.Errorf("%s notification secret must contain at least 32 characters", channel)
		}
		parsedSecrets[channel] = []byte(secrets[channel])
	}
	parsedCallback, err := url.Parse(strings.TrimSpace(callbackURL))
	if err != nil || parsedCallback.Scheme == "" || parsedCallback.Host == "" {
		return nil, errors.New("invalid Billing payment callback URL")
	}
	return &PaymentCoordinator{repository: repository, provider: provider, secrets: parsedSecrets, merchantID: merchantID, callbackURL: parsedCallback.String(), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (p *PaymentCoordinator) CreateOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, idempotencyKey string) (PaymentOrder, bool, error) {
	channel = strings.ToUpper(channel)
	if !validIdempotencyKey(idempotencyKey, 128) || !validPaymentChannel(channel) {
		return PaymentOrder{}, false, ErrConflict
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "payment.create:"+idempotencyKey); err != nil {
		return PaymentOrder{}, false, err
	}
	requestDigest := sha256.Sum256([]byte(requesterID.String() + "|" + walletID.String() + "|" + productID + "|" + channel))
	requestHash := hex.EncodeToString(requestDigest[:])
	var existingHash string
	var existingBody []byte
	err = tx.QueryRow(ctx, `SELECT request_hash, response_body FROM idempotency_records WHERE scope = 'payment.create' AND idempotency_key = $1 FOR UPDATE`, idempotencyKey).Scan(&existingHash, &existingBody)
	if err == nil {
		if existingHash != requestHash {
			return PaymentOrder{}, false, ErrConflict
		}
		var order PaymentOrder
		if err := json.Unmarshal(existingBody, &order); err != nil {
			return PaymentOrder{}, false, err
		}
		order, err = getPaymentOrderByID(ctx, tx, order.ID, false)
		if err != nil {
			return PaymentOrder{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return PaymentOrder{}, false, err
		}
		return p.activatePreparedOrder(ctx, order, false)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, false, err
	}
	var product Product
	err = tx.QueryRow(ctx, `SELECT product_id, amount_fen, credit_microcredits, display_points, currency, channels, catalog_version FROM recharge_products WHERE product_id = $1 AND enabled AND $2 = ANY(channels)`, productID, channel).Scan(&product.ProductID, &product.AmountFen, &product.CreditMicrocredits, &product.DisplayPoints, &product.Currency, &product.Channels, &product.CatalogVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, false, ErrConflict
	}
	if err != nil {
		return PaymentOrder{}, false, err
	}
	wallet, err := getWallet(ctx, tx, walletID, false)
	if err != nil {
		return PaymentOrder{}, false, ErrNotFound
	}
	if wallet.Status != "ACTIVE" {
		return PaymentOrder{}, false, ErrWalletUnavailable
	}
	now := p.now()
	order := PaymentOrder{ID: uuid.Must(uuid.NewV7()), MerchantOrderNo: "pv-" + uuid.Must(uuid.NewV7()).String(), RequesterID: requesterID, WalletID: walletID, ProductID: productID, Channel: channel, AmountFen: product.AmountFen, Currency: product.Currency, CreditMicrocredits: product.CreditMicrocredits, Status: "CREATED", FulfillmentStatus: "PENDING", ExpiresAt: now.Add(15 * time.Minute)}
	snapshot, _ := json.Marshal(product)
	_, err = tx.Exec(ctx, `INSERT INTO payment_orders(id, merchant_order_no, requester_id, wallet_id, product_id, product_snapshot, channel, amount_fen, currency, credit_microcredits, status, fulfillment_status, expires_at, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'CREATED','PENDING',$11,$12,$12)`, order.ID, order.MerchantOrderNo, requesterID, walletID, productID, snapshot, channel, order.AmountFen, order.Currency, order.CreditMicrocredits, order.ExpiresAt, now)
	if err != nil {
		return PaymentOrder{}, false, err
	}
	responseBody, _ := json.Marshal(order)
	_, err = tx.Exec(ctx, `INSERT INTO idempotency_records(scope,idempotency_key,request_hash,response_status,response_body,created_at,expires_at) VALUES ('payment.create',$1,$2,201,$3,$4,$5)`, idempotencyKey, requestHash, responseBody, now, now.Add(24*time.Hour))
	if err != nil {
		return PaymentOrder{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentOrder{}, false, err
	}
	return p.activatePreparedOrder(ctx, order, true)
}

// Persist the order and its stable merchant number before the provider side
// effect. An interrupted activation is resumed by the same idempotency key.
func (p *PaymentCoordinator) activatePreparedOrder(ctx context.Context, prepared PaymentOrder, created bool) (PaymentOrder, bool, error) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return prepared, created, err
	}
	defer tx.Rollback(ctx)
	order, err := getPaymentOrderByID(ctx, tx, prepared.ID, true)
	if err != nil {
		return prepared, created, err
	}
	if order.Status != "CREATED" {
		return order, created, tx.Commit(ctx)
	}
	wallet, err := getWallet(ctx, tx, order.WalletID, true)
	if err != nil {
		return order, created, err
	}
	if wallet.Status != "ACTIVE" {
		return order, created, ErrWalletUnavailable
	}
	provider, err := p.provider.Create(ctx, order, p.callbackURL)
	if err != nil {
		return order, created, errors.Join(ErrPaymentProvider, err)
	}
	refunded := provider.Status == "PARTIALLY_REFUNDED" || provider.Status == "REFUNDED"
	if provider.MerchantOrderNo != order.MerchantOrderNo || provider.Channel != order.Channel || provider.AmountFen != order.AmountFen || provider.Currency != order.Currency || (provider.PaymentURL == "" && !refunded) {
		return order, created, ErrConflict
	}
	order.Status, order.PaymentURL = "PENDING", provider.PaymentURL
	switch provider.Status {
	case "CLOSED", "FAILED":
		order.Status = provider.Status
	case "PARTIALLY_REFUNDED", "REFUNDED":
		// Record the first refunded fact before releasing the existing order
		// lock; a delayed SUCCESS notification cannot win a gap between txs.
		order.Status, order.FulfillmentStatus = "MANUAL_REVIEW", "MANUAL_REVIEW"
	case "PENDING", "SUCCESS":
	default:
		return order, created, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders
SET status=$2,payment_url=$3,updated_at=$4,fulfillment_status=$5,
    reconciliation_next_at=CASE WHEN $2='MANUAL_REVIEW' THEN NULL ELSE reconciliation_next_at END,
    reconciliation_lease_until=CASE WHEN $2='MANUAL_REVIEW' THEN NULL ELSE reconciliation_lease_until END,
    reconciliation_last_error=CASE WHEN $2='MANUAL_REVIEW' THEN NULL ELSE reconciliation_last_error END
WHERE id=$1`, order.ID, order.Status, order.PaymentURL, p.now(), order.FulfillmentStatus); err != nil {
		return order, created, err
	}
	if err := tx.Commit(ctx); err != nil {
		return order, created, err
	}
	if provider.Status == "SUCCESS" {
		order, err = p.applyQueriedPayment(ctx, order.ID, provider)
	}
	return order, created, err
}

func (p *PaymentCoordinator) ProcessNotification(ctx context.Context, channel, signature string, body []byte) error {
	channel = strings.ToUpper(channel)
	if !validPaymentChannel(channel) {
		return ErrForbiddenPayment
	}
	secret := p.secrets[channel]
	expected := paymentSignature(secret, body)
	if len(signature) != len(expected) || subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return ErrForbiddenPayment
	}
	var notification struct {
		NotificationID     uuid.UUID `json:"notification_id"`
		MerchantID         string    `json:"merchant_id"`
		MerchantOrderNo    string    `json:"merchant_order_no"`
		ChannelTransaction string    `json:"channel_transaction_id"`
		Channel            string    `json:"channel"`
		Status             string    `json:"status"`
		AmountFen          int64     `json:"amount_fen"`
		Currency           string    `json:"currency"`
		PaidAt             time.Time `json:"paid_at"`
		Timestamp          int64     `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &notification); err != nil || notification.NotificationID == uuid.Nil || strings.TrimSpace(notification.ChannelTransaction) == "" || notification.PaidAt.IsZero() || time.Unix(notification.Timestamp, 0).Before(p.now().Add(-5*time.Minute)) || time.Unix(notification.Timestamp, 0).After(p.now().Add(5*time.Minute)) {
		return ErrForbiddenPayment
	}
	if notification.Channel != channel || notification.MerchantID != p.merchantID || notification.MerchantOrderNo == "" || notification.AmountFen <= 0 || notification.Currency != "CNY" || notification.Status != "SUCCESS" {
		return ErrForbiddenPayment
	}
	// Receipt survives a failed fulfillment transaction so a retry or review
	// retains the verified channel fact (ADR-006).
	digest := sha256.Sum256(body)
	_, err := p.repository.pool.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result) VALUES ($1,$2,$3,$4,$5,'RECEIVED') ON CONFLICT DO NOTHING`, notification.NotificationID, channel, hex.EncodeToString(digest[:]), body, p.now())
	if err != nil {
		return err
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Timestamp authenticates this delivery, not the immutable payment fact.
	// JSONB compares numbers exactly and every other (even unknown) field.
	var sameFacts bool
	var result string
	if err := tx.QueryRow(ctx, `SELECT (payload - 'timestamp') = ($2::jsonb - 'timestamp'),result FROM payment_notifications WHERE notification_id=$1 FOR UPDATE`, notification.NotificationID, body).Scan(&sameFacts, &result); err != nil {
		return err
	}
	if !sameFacts {
		return ErrForbiddenPayment
	}
	if result == "PROCESSED" {
		return tx.Commit(ctx)
	}
	order, err := getPaymentOrderForUpdate(ctx, tx, notification.MerchantOrderNo)
	if err != nil {
		return err
	}
	if notification.MerchantID != p.merchantID || notification.Channel != order.Channel || channel != order.Channel || notification.AmountFen != order.AmountFen || notification.Currency != order.Currency || notification.Status != "SUCCESS" {
		return ErrForbiddenPayment
	}
	if order.FulfillmentStatus == "MANUAL_REVIEW" {
		// Keep the authenticated receipt, but do not turn a delayed SUCCESS
		// delivery into gross credit after a refunded first observation.
		if _, err := tx.Exec(ctx, `UPDATE payment_notifications SET result='PROCESSED',processed_at=$2 WHERE notification_id=$1`, notification.NotificationID, p.now()); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if order.FulfillmentStatus == "CREDITED" || order.FulfillmentStatus == "REFUNDED" {
		if order.ChannelTransactionID == nil || *order.ChannelTransactionID != notification.ChannelTransaction || order.PaidAt == nil || !samePaymentTime(*order.PaidAt, notification.PaidAt) {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_notifications SET result='PROCESSED',processed_at=$2 WHERE notification_id=$1`, notification.NotificationID, p.now()); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	status := "PAID"
	if !order.ExpiresAt.After(notification.PaidAt) {
		status = "PAID_LATE"
	}
	transactionID := uuid.Must(uuid.NewV7())
	command, err := tx.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,channel,channel_transaction_id,status,amount_fen,currency,source,verified_at,created_at,updated_at) VALUES($1,$2,$3,$4,'SUCCEEDED',$5,$6,'NOTIFICATION',$7,$7,$7) ON CONFLICT (channel_transaction_id) DO NOTHING`, transactionID, order.ID, order.Channel, notification.ChannelTransaction, order.AmountFen, order.Currency, p.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var existingOrderID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT payment_order_id FROM payment_transactions WHERE channel_transaction_id=$1`, notification.ChannelTransaction).Scan(&existingOrderID); err != nil || existingOrderID != order.ID {
			return ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `UPDATE payment_orders SET status=$2, fulfillment_status='CREDITED', channel_transaction_id=$3, paid_at=$4, updated_at=$5 WHERE id=$1`, order.ID, status, notification.ChannelTransaction, notification.PaidAt, p.now())
	if err != nil {
		return err
	}
	if err := creditWithinTransaction(ctx, tx, order.WalletID, "recharge:"+order.ID.String(), order.CreditMicrocredits, p.now()); err != nil {
		return err
	}
	if err := enqueueRechargeEvent(ctx, tx, order, p.now()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE payment_notifications SET result='PROCESSED', processed_at=$2 WHERE notification_id=$1`, notification.NotificationID, p.now())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func providerOrderMatches(order PaymentOrder, provider ProviderOrder) bool {
	return provider.MerchantOrderNo == order.MerchantOrderNo && provider.Channel == order.Channel && provider.AmountFen == order.AmountFen && provider.Currency == order.Currency
}

func samePaymentTime(left, right time.Time) bool {
	// PostgreSQL TIMESTAMPTZ persists microseconds, while provider JSON may
	// contain nanoseconds. Compare facts at the ledger's persisted precision.
	return left.Truncate(time.Microsecond).Equal(right.Truncate(time.Microsecond))
}

func enqueueRechargeEvent(ctx context.Context, tx pgx.Tx, order PaymentOrder, now time.Time) error {
	return events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", order.ID, 1, map[string]any{"order_id": order.ID, "wallet_id": order.WalletID, "channel": order.Channel, "amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits})
}

var ErrForbiddenPayment = errors.New("payment notification rejected")

func creditWithinTransaction(ctx context.Context, tx pgx.Tx, walletID uuid.UUID, reference string, amount int64, now time.Time) error {
	wallet, err := getWallet(ctx, tx, walletID, true)
	if err != nil || wallet.Status != "ACTIVE" {
		return ErrWalletUnavailable
	}
	entryID, lotID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := insertJournal(ctx, tx, entryID, "RECHARGE", reference, "payment recharge", now); err != nil {
		return err
	}
	accountID, err := walletLedgerAccount(ctx, tx, walletID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,created_at) VALUES ($1,$2,'RECHARGE',$3,$4,$4,true,$5)`, lotID, walletID, reference, amount, now); err != nil {
		return err
	}
	if err = insertPosting(ctx, tx, entryID, accountID, amount, &lotID, now); err != nil {
		return err
	}
	if err = insertPosting(ctx, tx, entryID, paymentClearingAccount, -amount, nil, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1`, walletID, amount, now)
	return err
}

func getPaymentOrderForUpdate(ctx context.Context, tx pgx.Tx, merchant string) (PaymentOrder, error) {
	var o PaymentOrder
	err := tx.QueryRow(ctx, `SELECT id,merchant_order_no,requester_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,COALESCE(payment_url,''),channel_transaction_id,expires_at,paid_at FROM payment_orders WHERE merchant_order_no=$1 FOR UPDATE`, merchant).Scan(&o.ID, &o.MerchantOrderNo, &o.RequesterID, &o.WalletID, &o.ProductID, &o.Channel, &o.AmountFen, &o.Currency, &o.CreditMicrocredits, &o.Status, &o.FulfillmentStatus, &o.PaymentURL, &o.ChannelTransactionID, &o.ExpiresAt, &o.PaidAt)
	return o, err
}

type SimulatorProvider struct {
	base   *url.URL
	token  string
	client *http.Client
}

func NewSimulatorProvider(raw, token string) (*SimulatorProvider, error) {
	u, e := url.Parse(strings.TrimRight(raw, "/"))
	if e != nil || u.Host == "" || token == "" {
		return nil, errors.New("invalid payment simulator configuration")
	}
	return &SimulatorProvider{u, token, observability.NewHTTPClient(5 * time.Second)}, nil
}
func (p *SimulatorProvider) Create(ctx context.Context, o PaymentOrder, callback string) (ProviderOrder, error) {
	var result ProviderOrder
	body, _ := json.Marshal(map[string]any{"merchant_order_no": o.MerchantOrderNo, "channel": o.Channel, "amount_fen": o.AmountFen, "currency": o.Currency, "callback_url": callback, "scenario": "SUCCESS", "expires_in_seconds": 900})
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, p.base.String()+"/internal/v1/orders", bytes.NewReader(body))
	if e != nil {
		return result, e
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := p.client.Do(req)
	if e != nil {
		return result, e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return result, fmt.Errorf("simulator HTTP %d: %s", resp.StatusCode, data)
	}
	return result, json.NewDecoder(resp.Body).Decode(&result)
}

func (p *SimulatorProvider) Get(ctx context.Context, merchantOrderNo string) (ProviderOrder, error) {
	var result ProviderOrder
	err := p.doJSON(ctx, http.MethodGet, "/internal/v1/orders/"+url.PathEscape(merchantOrderNo), nil, &result)
	return result, err
}

func (p *SimulatorProvider) Close(ctx context.Context, merchantOrderNo string) (ProviderOrder, error) {
	var result ProviderOrder
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/orders/"+url.PathEscape(merchantOrderNo)+"/close", map[string]any{}, &result)
	return result, err
}

func (p *SimulatorProvider) Refund(ctx context.Context, merchantOrderNo, merchantRefundNo string, amountFen int64) (ProviderRefund, error) {
	var result ProviderRefund
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/orders/"+url.PathEscape(merchantOrderNo)+"/refunds", map[string]any{"merchant_refund_no": merchantRefundNo, "amount_fen": amountFen}, &result)
	return result, err
}

func (p *SimulatorProvider) Bill(ctx context.Context, channel string, businessDate time.Time) ([]ProviderBillEntry, error) {
	var result struct {
		Items []ProviderBillEntry `json:"items"`
	}
	path := "/internal/v1/bills/" + businessDate.UTC().Format("2006-01-02") + "?channel=" + url.QueryEscape(strings.ToUpper(channel))
	err := p.doJSON(ctx, http.MethodGet, path, nil, &result)
	return result.Items, err
}

func (p *SimulatorProvider) doJSON(ctx context.Context, method, path string, input any, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base.String()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("simulator HTTP %d: %s", resp.StatusCode, data)
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(output)
}
func paymentSignature(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}
func validPaymentChannel(v string) bool { return v == "UNIONPAY" || v == "ALIPAY" || v == "WECHAT" }

func validIdempotencyKey(key string, maximum int) bool {
	if !utf8.ValidString(key) {
		return false
	}
	characters := utf8.RuneCountInString(key)
	return characters >= 16 && (maximum == 0 || characters <= maximum)
}
