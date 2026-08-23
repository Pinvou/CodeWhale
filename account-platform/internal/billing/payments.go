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
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	QRData               string     `json:"qr_data,omitempty"`
	ChannelTransactionID *string    `json:"channel_transaction_id,omitempty"`
	ExpiresAt            time.Time  `json:"expires_at"`
	PaidAt               *time.Time `json:"paid_at,omitempty"`
	Mobile               string     `json:"-"`
	ProviderSKU          string     `json:"-"`
}

type PaymentOrderHistoryItem struct {
	ID                uuid.UUID  `json:"order_id"`
	PinvouOrderID     uuid.UUID  `json:"pinvou_order_id"`
	MallOrderID       string     `json:"mall_order_id"`
	ProductID         string     `json:"product_id"`
	PlanName          string     `json:"plan_name"`
	SKUCode           string     `json:"sku_code"`
	OrderType         string     `json:"order_type"`
	AmountFen         int64      `json:"amount_fen"`
	DisplayPoints     int64      `json:"display_points"`
	Channel           string     `json:"channel"`
	Status            string     `json:"status"`
	FulfillmentStatus string     `json:"fulfillment_status"`
	ExpiresAt         time.Time  `json:"expires_at"`
	PaidAt            *time.Time `json:"paid_at,omitempty"`
	CreditedAt        *time.Time `json:"credited_at,omitempty"`
	ValidUntil        *time.Time `json:"valid_until,omitempty"`
	InvoiceStatus     string     `json:"invoice_status"`
	CreatedAt         time.Time  `json:"created_at"`
}

type PaymentCoordinator struct {
	repository           *PostgresRepository
	provider             PaymentProvider
	secrets              map[string][]byte
	merchantID           string
	callbackURL          string
	now                  func() time.Time
	productSKUs          map[string]ExternalProduct
	allowTentativePrices bool
}

type ProviderOrder struct {
	MerchantOrderNo      string     `json:"merchant_order_no"`
	PaymentURL           string     `json:"payment_url"`
	QRData               string     `json:"qr_data,omitempty"`
	Status               string     `json:"status"`
	Channel              string     `json:"channel"`
	AmountFen            int64      `json:"amount_fen"`
	Currency             string     `json:"currency"`
	ChannelTransactionID *string    `json:"channel_transaction_id,omitempty"`
	PaidAt               *time.Time `json:"paid_at,omitempty"`
	ProviderStatus       string     `json:"provider_status,omitempty"`
	ProviderTraceID      string     `json:"provider_trace_id,omitempty"`
}

type ExternalProduct struct {
	SKU       string `json:"sku"`
	AmountFen int64  `json:"amount_fen"`
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

type PaymentCapabilities struct {
	Provider                 string `json:"provider"`
	OrderSyncSupported       bool   `json:"order_sync_supported"`
	RefundSupported          bool   `json:"refund_supported"`
	CloseSupported           bool   `json:"close_supported"`
	NotificationSupported    bool   `json:"notification_supported"`
	ReconciliationSupported  bool   `json:"reconciliation_supported"`
	ManualRepairSupported    bool   `json:"manual_repair_supported"`
	CommerceCustomerRequired bool   `json:"commerce_customer_required"`
}

// PaymentPolicy describes provider-specific behavior that the generic billing
// workflow must honor. Providers opt in explicitly; future providers keep the
// conservative defaults without introducing provider-name branches here.
type PaymentPolicy struct {
	ReusePendingOrderAfterExpiry bool
	ReviewLatePayment            bool
}

func (p PaymentPolicy) RequiresReview(expiresAt, paidAt time.Time) bool {
	return p.ReviewLatePayment && !expiresAt.After(paidAt)
}

type paymentCapabilityProvider interface {
	Capabilities() PaymentCapabilities
}

type paymentPolicyProvider interface {
	Policy() PaymentPolicy
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
	return &PaymentCoordinator{repository: repository, provider: provider, secrets: parsedSecrets, merchantID: merchantID, callbackURL: parsedCallback.String(), now: func() time.Time { return time.Now().UTC() }, allowTentativePrices: true}, nil
}

func NewPaymentCoordinatorWithProvider(repository *PostgresRepository, provider PaymentProvider, products map[string]ExternalProduct) (*PaymentCoordinator, error) {
	if repository == nil || provider == nil || len(products) == 0 {
		return nil, errors.New("external payment provider and product mapping are required")
	}
	validated := make(map[string]ExternalProduct, len(products))
	for productID, product := range products {
		productID, product.SKU = strings.TrimSpace(productID), strings.TrimSpace(product.SKU)
		if productID == "" || product.SKU == "" || product.AmountFen <= 0 {
			return nil, errors.New("external payment product mapping is invalid")
		}
		validated[productID] = product
	}
	return &PaymentCoordinator{repository: repository, provider: provider, productSKUs: validated, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (p *PaymentCoordinator) AllowTentativePrices(allowed bool) *PaymentCoordinator {
	p.allowTentativePrices = allowed
	return p
}

func (p *PaymentCoordinator) FilterProducts(products []Product) []Product {
	if len(p.productSKUs) == 0 {
		return products
	}
	filtered := make([]Product, 0, len(products))
	for _, product := range products {
		if product.PriceStatus != "FROZEN" && !p.allowTentativePrices {
			continue
		}
		if external, ok := p.productSKUs[product.ProductID]; ok && external.AmountFen == product.AmountFen {
			product.Channels = []string{"ALIPAY"}
			filtered = append(filtered, product)
		}
	}
	return filtered
}

func (p *PaymentCoordinator) Capabilities() PaymentCapabilities {
	if provider, ok := p.provider.(paymentCapabilityProvider); ok {
		return provider.Capabilities()
	}
	return PaymentCapabilities{Provider: "unknown", OrderSyncSupported: true}
}

func (p *PaymentCoordinator) Policy() PaymentPolicy {
	if provider, ok := p.provider.(paymentPolicyProvider); ok {
		return provider.Policy()
	}
	return PaymentPolicy{}
}

func (p *PaymentCoordinator) CreateOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, idempotencyKey string) (PaymentOrder, bool, error) {
	return p.createOrder(ctx, requesterID, walletID, productID, channel, "", idempotencyKey)
}

func (p *PaymentCoordinator) CreateMobileOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, mobile, idempotencyKey string) (PaymentOrder, bool, error) {
	return p.createOrder(ctx, requesterID, walletID, productID, channel, mobile, idempotencyKey)
}

func (p *PaymentCoordinator) createOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, mobile, idempotencyKey string) (PaymentOrder, bool, error) {
	channel = strings.ToUpper(channel)
	if len(idempotencyKey) < 16 || !validPaymentChannel(channel) {
		return PaymentOrder{}, false, ErrConflict
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return PaymentOrder{}, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "payment.create", idempotencyKey); err != nil {
		return PaymentOrder{}, false, err
	}
	if err := lockTransactionKey(ctx, tx, "payment.pending", requesterID.String()+"|"+walletID.String()+"|"+productID); err != nil {
		return PaymentOrder{}, false, err
	}
	requestDigest := sha256.Sum256([]byte(requesterID.String() + "|" + walletID.String() + "|" + productID + "|" + channel + "|" + mobile))
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
		return order, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, false, err
	}
	var product Product
	err = tx.QueryRow(ctx, `SELECT product_id,plan_name,sku_code,amount_fen,credit_microcredits,display_points,base_points,bonus_points,validity_days,price_status,invoice_product_name,currency,channels,catalog_version FROM recharge_products WHERE product_id=$1 AND enabled AND $2=ANY(channels)`, productID, channel).Scan(&product.ProductID, &product.PlanName, &product.SKUCode, &product.AmountFen, &product.CreditMicrocredits, &product.DisplayPoints, &product.BasePoints, &product.BonusPoints, &product.ValidityDays, &product.PriceStatus, &product.InvoiceProductName, &product.Currency, &product.Channels, &product.CatalogVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, false, ErrConflict
	}
	if err != nil {
		return PaymentOrder{}, false, err
	}
	if product.PriceStatus != "FROZEN" && !p.allowTentativePrices {
		return PaymentOrder{}, false, ErrConflict
	}
	now := p.now()
	policy := p.Policy()
	existing, err := getPendingPlanOrderForUpdate(ctx, tx, requesterID, walletID, productID, channel, now, policy.ReusePendingOrderAfterExpiry)
	if err == nil {
		if policy.ReusePendingOrderAfterExpiry && !existing.ExpiresAt.After(now) {
			existing.ExpiresAt = now.Add(15 * time.Minute)
			if _, err := tx.Exec(ctx, `UPDATE payment_orders SET expires_at=$2,reconciliation_next_at=$3,reconciliation_attempts=0,reconciliation_last_error=NULL,updated_at=$3 WHERE id=$1`, existing.ID, existing.ExpiresAt, now); err != nil {
				return PaymentOrder{}, false, err
			}
		}
		responseBody, _ := json.Marshal(existing)
		if _, err := tx.Exec(ctx, `INSERT INTO idempotency_records(scope,idempotency_key,request_hash,response_status,response_body,created_at,expires_at) VALUES('payment.create',$1,$2,200,$3,$4,$5)`, idempotencyKey, requestHash, responseBody, now, now.Add(24*time.Hour)); err != nil {
			return PaymentOrder{}, false, err
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PaymentOrder{}, false, err
	}
	external := p.productSKUs[productID]
	if len(p.productSKUs) > 0 && (channel != "ALIPAY" || mobile == "" || external.SKU == "" || external.AmountFen != product.AmountFen) {
		return PaymentOrder{}, false, ErrConflict
	}
	if _, err := getWallet(ctx, tx, walletID, false); err != nil {
		return PaymentOrder{}, false, ErrNotFound
	}
	merchantDigest := sha256.Sum256([]byte(requesterID.String() + "|" + idempotencyKey))
	order := PaymentOrder{ID: uuid.Must(uuid.NewV7()), MerchantOrderNo: "pv-" + hex.EncodeToString(merchantDigest[:16]), RequesterID: requesterID, WalletID: walletID, ProductID: productID, Channel: channel, AmountFen: product.AmountFen, Currency: product.Currency, CreditMicrocredits: product.CreditMicrocredits, Status: "CREATED", FulfillmentStatus: "PENDING", ExpiresAt: now.Add(15 * time.Minute), Mobile: mobile, ProviderSKU: external.SKU}
	snapshot, _ := json.Marshal(product)
	_, err = tx.Exec(ctx, `INSERT INTO payment_orders(id, merchant_order_no, requester_id, wallet_id, product_id, product_snapshot, channel, amount_fen, currency, credit_microcredits, status, fulfillment_status, expires_at, created_at, updated_at, mobile_e164, provider_sku) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'CREATED','PENDING',$11,$12,$12,NULLIF($13,''),NULLIF($14,''))`, order.ID, order.MerchantOrderNo, requesterID, walletID, productID, snapshot, channel, order.AmountFen, order.Currency, order.CreditMicrocredits, order.ExpiresAt, now, mobile, external.SKU)
	if err != nil {
		return PaymentOrder{}, false, err
	}
	providerOrder, err := p.provider.Create(ctx, order, p.callbackURL)
	if err != nil {
		return PaymentOrder{}, false, err
	}
	order.Status = "PENDING"
	order.PaymentURL = providerOrder.PaymentURL
	order.QRData = providerOrder.QRData
	_, err = tx.Exec(ctx, `UPDATE payment_orders SET status='PENDING',payment_url=NULLIF($2,''),qr_data=NULLIF($3,''),updated_at=$4 WHERE id=$1`, order.ID, order.PaymentURL, order.QRData, p.now())
	if err != nil {
		return PaymentOrder{}, false, err
	}
	responseBody, _ := json.Marshal(order)
	_, err = tx.Exec(ctx, `INSERT INTO idempotency_records(scope,idempotency_key,request_hash,response_status,response_body,created_at,expires_at) VALUES ('payment.create',$1,$2,201,$3,$4,$5)`, idempotencyKey, requestHash, responseBody, now, now.Add(24*time.Hour))
	if err != nil {
		return PaymentOrder{}, false, err
	}
	return order, true, tx.Commit(ctx)
}

func getPendingPlanOrderForUpdate(ctx context.Context, tx pgx.Tx, requesterID, walletID uuid.UUID, productID, channel string, now time.Time, includeExpired bool) (PaymentOrder, error) {
	var order PaymentOrder
	err := tx.QueryRow(ctx, `SELECT id,merchant_order_no,requester_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,COALESCE(payment_url,''),COALESCE(qr_data,''),channel_transaction_id,expires_at,paid_at FROM payment_orders WHERE requester_id=$1 AND wallet_id=$2 AND product_id=$3 AND channel=$4 AND status IN ('CREATED','PENDING') AND ($6 OR expires_at>$5) ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE`, requesterID, walletID, productID, channel, now, includeExpired).Scan(&order.ID, &order.MerchantOrderNo, &order.RequesterID, &order.WalletID, &order.ProductID, &order.Channel, &order.AmountFen, &order.Currency, &order.CreditMicrocredits, &order.Status, &order.FulfillmentStatus, &order.PaymentURL, &order.QRData, &order.ChannelTransactionID, &order.ExpiresAt, &order.PaidAt)
	return order, err
}

func (p *PaymentCoordinator) ProcessNotification(ctx context.Context, channel, signature string, body []byte) error {
	channel = strings.ToUpper(channel)
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
	if err := json.Unmarshal(body, &notification); err != nil || notification.NotificationID == uuid.Nil || time.Unix(notification.Timestamp, 0).Before(p.now().Add(-5*time.Minute)) || time.Unix(notification.Timestamp, 0).After(p.now().Add(5*time.Minute)) {
		return ErrForbiddenPayment
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	digest := sha256.Sum256(body)
	command, err := tx.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result) VALUES ($1,$2,$3,$4,$5,'RECEIVED') ON CONFLICT DO NOTHING`, notification.NotificationID, channel, hex.EncodeToString(digest[:]), body, p.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM payment_notifications WHERE notification_id=$1`, notification.NotificationID).Scan(&existingHash); err != nil || existingHash != hex.EncodeToString(digest[:]) {
			return ErrForbiddenPayment
		}
		return tx.Commit(ctx)
	}
	order, err := getPaymentOrderForUpdate(ctx, tx, notification.MerchantOrderNo)
	if err != nil {
		return err
	}
	if notification.MerchantID != p.merchantID || notification.Channel != order.Channel || channel != order.Channel || notification.AmountFen != order.AmountFen || notification.Currency != order.Currency || notification.Status != "SUCCESS" {
		return ErrForbiddenPayment
	}
	if order.FulfillmentStatus == "CREDITED" {
		if order.ChannelTransactionID == nil || *order.ChannelTransactionID != notification.ChannelTransaction {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	status := "PAID"
	if !order.ExpiresAt.After(notification.PaidAt) {
		status = "PAID_LATE"
	}
	transactionID := uuid.Must(uuid.NewV7())
	command, err = tx.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,channel,channel_transaction_id,status,amount_fen,currency,source,verified_at,created_at,updated_at) VALUES($1,$2,$3,$4,'SUCCEEDED',$5,$6,'NOTIFICATION',$7,$7,$7) ON CONFLICT (channel_transaction_id) DO NOTHING`, transactionID, order.ID, order.Channel, notification.ChannelTransaction, order.AmountFen, order.Currency, p.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var existingOrderID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT payment_order_id FROM payment_transactions WHERE channel_transaction_id=$1`, notification.ChannelTransaction).Scan(&existingOrderID); err != nil || existingOrderID != order.ID {
			return ErrConflict
		}
	}
	if p.Policy().RequiresReview(order.ExpiresAt, notification.PaidAt) {
		_, err = tx.Exec(ctx, `UPDATE payment_orders SET status='MANUAL_REVIEW',fulfillment_status='MANUAL_REVIEW',channel_transaction_id=$2,paid_at=$3,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,updated_at=$4 WHERE id=$1`, order.ID, notification.ChannelTransaction, notification.PaidAt, p.now())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_notifications SET result='MANUAL_REVIEW',processed_at=$2 WHERE notification_id=$1`, notification.NotificationID, p.now())
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `UPDATE payment_orders SET status=$2, fulfillment_status='CREDITED', channel_transaction_id=$3, paid_at=$4, updated_at=$5 WHERE id=$1`, order.ID, status, notification.ChannelTransaction, notification.PaidAt, p.now())
	if err != nil {
		return err
	}
	if _, err := issuePackageWithinTransaction(ctx, tx, order, p.now(), false); err != nil {
		return err
	}
	if err := events.AppendOutbox(ctx, tx, "billing.recharge.succeeded.v1", order.ID, 1, map[string]any{
		"order_id": order.ID, "wallet_id": order.WalletID, "channel": order.Channel, "amount_fen": order.AmountFen, "credit_microcredits": order.CreditMicrocredits,
	}); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE payment_notifications SET result='PROCESSED', processed_at=$2 WHERE notification_id=$1`, notification.NotificationID, p.now())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var ErrForbiddenPayment = errors.New("payment notification rejected")

func issuePackageWithinTransaction(ctx context.Context, tx pgx.Tx, order PaymentOrder, now time.Time, repair bool) (uuid.UUID, error) {
	wallet, err := getWallet(ctx, tx, order.WalletID, true)
	if err != nil || wallet.Status != "ACTIVE" {
		return uuid.Nil, ErrWalletUnavailable
	}
	var snapshot []byte
	if err := tx.QueryRow(ctx, `SELECT product_snapshot FROM payment_orders WHERE id=$1`, order.ID).Scan(&snapshot); err != nil {
		return uuid.Nil, err
	}
	var product Product
	if err := json.Unmarshal(snapshot, &product); err != nil {
		return uuid.Nil, ErrConflict
	}
	if product.ValidityDays <= 0 {
		product.ValidityDays = 30
	}
	if product.PlanName == "" {
		product.PlanName = order.ProductID
	}
	if product.SKUCode == "" {
		product.SKUCode = order.ProviderSKU
	}
	journalType, reference, description := "RECHARGE", "recharge:"+order.ID.String(), "payment recharge"
	if repair {
		journalType, reference, description = "REPAIR_CREDIT", "repair:"+order.ID.String(), "verified payment repair credit"
	}
	components, err := splitProductComponents(order.CreditMicrocredits, product.BasePoints, product.BonusPoints)
	if err != nil {
		return uuid.Nil, err
	}
	amount := order.CreditMicrocredits
	entryID := uuid.Must(uuid.NewV7())
	if err := insertJournal(ctx, tx, entryID, journalType, reference, description, now); err != nil {
		return uuid.Nil, err
	}
	accountID, err := walletLedgerAccount(ctx, tx, order.WalletID)
	if err != nil {
		return uuid.Nil, err
	}
	validFrom, validUntil := now, now.AddDate(0, 0, product.ValidityDays)
	packageID := uuid.Must(uuid.NewV7())
	primaryLotID := components[0].lotID
	for _, component := range components {
		lotReference := reference
		if component.kind == "BONUS" && len(components) > 1 {
			lotReference += ":bonus"
		}
		refundable := component.kind == "BASE"
		if _, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES ($1,$2,'RECHARGE',$3,$4,$4,$5,$6,$7)`, component.lotID, order.WalletID, lotReference, component.amount, refundable, validUntil, now); err != nil {
			return uuid.Nil, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,$11,$12,$13,'ACTIVE',$11,$11,'PAYMENT',$14)`, packageID, order.RequesterID, order.WalletID, order.ID, primaryLotID, order.ProductID, product.PlanName, product.SKUCode, order.AmountFen, amount, now, validFrom, validUntil, reference); err != nil {
		return uuid.Nil, err
	}
	for _, component := range components {
		if _, err = tx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,$3,$1,$4,$4,$5,$6,'ACTIVE',$5,$5)`, component.lotID, packageID, component.kind, component.amount, now, validUntil); err != nil {
			return uuid.Nil, err
		}
		if err = insertPosting(ctx, tx, entryID, accountID, component.amount, &component.lotID, now); err != nil {
			return uuid.Nil, err
		}
	}
	if err = insertPosting(ctx, tx, entryID, paymentClearingAccount, -amount, nil, now); err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1`, order.WalletID, amount, now)
	return packageID, err
}

type productComponentCredit struct {
	kind   string
	amount int64
	lotID  uuid.UUID
}

func splitProductComponents(total, basePoints, bonusPoints int64) ([]productComponentCredit, error) {
	if total <= 0 || basePoints < 0 || bonusPoints < 0 || basePoints > math.MaxInt64-bonusPoints || basePoints+bonusPoints <= 0 {
		return nil, ErrConflict
	}
	weight := basePoints + bonusPoints
	fraction := new(big.Int).Mul(big.NewInt(total%weight), big.NewInt(basePoints))
	fraction.Quo(fraction, big.NewInt(weight))
	base := total/weight*basePoints + fraction.Int64()
	bonus := total - base
	components := make([]productComponentCredit, 0, 2)
	if base > 0 {
		components = append(components, productComponentCredit{kind: "BASE", amount: base, lotID: uuid.Must(uuid.NewV7())})
	}
	if bonus > 0 {
		components = append(components, productComponentCredit{kind: "BONUS", amount: bonus, lotID: uuid.Must(uuid.NewV7())})
	}
	if len(components) == 0 {
		return nil, ErrConflict
	}
	return components, nil
}

func getPaymentOrderForUpdate(ctx context.Context, tx pgx.Tx, merchant string) (PaymentOrder, error) {
	var o PaymentOrder
	err := tx.QueryRow(ctx, `SELECT id,merchant_order_no,requester_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,COALESCE(payment_url,''),COALESCE(qr_data,''),channel_transaction_id,expires_at,paid_at FROM payment_orders WHERE merchant_order_no=$1 FOR UPDATE`, merchant).Scan(&o.ID, &o.MerchantOrderNo, &o.RequesterID, &o.WalletID, &o.ProductID, &o.Channel, &o.AmountFen, &o.Currency, &o.CreditMicrocredits, &o.Status, &o.FulfillmentStatus, &o.PaymentURL, &o.QRData, &o.ChannelTransactionID, &o.ExpiresAt, &o.PaidAt)
	return o, err
}

type SimulatorProvider struct {
	base   *url.URL
	token  string
	client *http.Client
}

func (p *SimulatorProvider) Capabilities() PaymentCapabilities {
	return PaymentCapabilities{
		Provider: "simulator", OrderSyncSupported: true, RefundSupported: true,
		CloseSupported: true, NotificationSupported: true, ReconciliationSupported: true,
	}
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
