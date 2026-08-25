package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type PaymentOrder struct {
	ID                           uuid.UUID  `json:"order_id"`
	MerchantOrderNo              string     `json:"merchant_order_no"`
	RequesterID                  uuid.UUID  `json:"requester_id"`
	BeneficiaryOrganizationID    *uuid.UUID `json:"beneficiary_organization_id,omitempty"`
	WalletID                     uuid.UUID  `json:"wallet_id"`
	ProductID                    string     `json:"product_id"`
	Channel                      string     `json:"channel"`
	AmountFen                    int64      `json:"amount_fen"`
	Currency                     string     `json:"currency"`
	CreditMicrocredits           int64      `json:"credit_microcredits"`
	Status                       string     `json:"status"`
	FulfillmentStatus            string     `json:"fulfillment_status"`
	CloseStatus                  string     `json:"close_status"`
	PaymentURL                   string     `json:"payment_url,omitempty"`
	QRData                       string     `json:"qr_data,omitempty"`
	PrimaryProviderTransactionID *string    `json:"primary_provider_transaction_id,omitempty"`
	ExpiresAt                    time.Time  `json:"expires_at"`
	PaidAt                       *time.Time `json:"paid_at,omitempty"`
	Mobile                       string     `json:"-"`
	ProviderSKU                  string     `json:"-"`
	ProviderID                   string     `json:"-"`
	ProviderVersion              string     `json:"-"`
	ProviderPayableUntil         time.Time  `json:"provider_payable_until"`
	LateReviewBoundaryAt         time.Time  `json:"late_review_boundary_at"`
	FinalArchiveEligibleAt       time.Time  `json:"final_archive_eligible_at"`
	ArchiveStatus                string     `json:"archive_status"`
}

type PaymentOrderHistoryItem struct {
	ID                     uuid.UUID  `json:"order_id"`
	PinvouOrderID          uuid.UUID  `json:"pinvou_order_id"`
	ProviderOrderReference string     `json:"provider_order_reference"`
	ProductID              string     `json:"product_id"`
	PlanName               string     `json:"plan_name"`
	SKUCode                string     `json:"sku_code"`
	OrderType              string     `json:"order_type"`
	AmountFen              int64      `json:"amount_fen"`
	DisplayPoints          int64      `json:"display_points"`
	Channel                string     `json:"channel"`
	Status                 string     `json:"status"`
	FulfillmentStatus      string     `json:"fulfillment_status"`
	CloseStatus            string     `json:"close_status"`
	ExpiresAt              time.Time  `json:"expires_at"`
	PaidAt                 *time.Time `json:"paid_at,omitempty"`
	CreditedAt             *time.Time `json:"credited_at,omitempty"`
	ValidUntil             *time.Time `json:"valid_until,omitempty"`
	InvoiceStatus          string     `json:"invoice_status"`
	CreatedAt              time.Time  `json:"created_at"`
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
	breaker              *resilience.Breaker
}

type ProviderOrder = payment.ProviderOrder

type ExternalProduct struct {
	MappingID       uuid.UUID `json:"mapping_id"`
	MappingVersion  int64     `json:"mapping_version"`
	ProviderID      string    `json:"provider_id"`
	ProviderVersion string    `json:"provider_version"`
	SKU             string    `json:"external_sku"`
	AmountFen       int64     `json:"amount_fen"`
	Currency        string    `json:"currency"`
	Channels        []string  `json:"channels"`
	Status          string    `json:"status"`
}

type ProviderRefund = payment.Refund
type ProviderBillEntry = payment.BillEntry
type PaymentCapabilities = payment.Capabilities

// PaymentPolicy describes provider-specific behavior that the generic billing
// workflow must honor. Providers opt in explicitly; future providers keep the
// conservative defaults without introducing provider-name branches here.
type PaymentPolicy struct {
	ReusePendingOrderAfterExpiry bool
	ReviewLatePayment            bool
	MaximumLateConfirmation      time.Duration
}

func (p PaymentPolicy) RequiresReview(expiresAt, paidAt time.Time) bool {
	if expiresAt.After(paidAt) {
		return false
	}
	return p.ReviewLatePayment || p.MaximumLateConfirmation <= 0 || paidAt.After(expiresAt.Add(p.MaximumLateConfirmation))
}

type PaymentProvider = payment.Provider

func NewPaymentCoordinatorWithProvider(repository *PostgresRepository, provider PaymentProvider, products map[string]ExternalProduct) (*PaymentCoordinator, error) {
	if repository == nil || provider == nil || len(products) == 0 {
		return nil, errors.New("external payment provider and product mapping are required")
	}
	if !provider.Capabilities().Valid() {
		return nil, errors.New("payment provider capabilities are invalid")
	}
	validated := make(map[string]ExternalProduct, len(products))
	for productID, product := range products {
		productID, product.SKU = strings.TrimSpace(productID), strings.TrimSpace(product.SKU)
		if productID == "" || product.MappingID == uuid.Nil || product.MappingVersion < 1 || product.SKU == "" || product.AmountFen <= 0 || product.ProviderID != provider.Capabilities().ProviderID || product.ProviderVersion != provider.Capabilities().Version || len(product.Channels) == 0 || product.Status != "FROZEN" && product.Status != "TEST_ONLY" {
			return nil, errors.New("external payment product mapping is invalid")
		}
		validated[productID] = product
	}
	return &PaymentCoordinator{repository: repository, provider: provider, productSKUs: validated, now: func() time.Time { return time.Now().UTC() }, breaker: resilience.NewBreaker()}, nil
}

func (p *PaymentCoordinator) ConfigureNotifications(merchantID, callbackURL string, secrets map[string]string) error {
	if !p.Capabilities().NotificationSupported {
		return payment.ErrUnsupported
	}
	parsedSecrets := make(map[string][]byte)
	for _, channel := range p.Capabilities().Channels {
		if len(secrets[channel]) < 32 {
			return fmt.Errorf("%s notification secret must contain at least 32 characters", channel)
		}
		parsedSecrets[channel] = []byte(secrets[channel])
	}
	parsedCallback, err := url.Parse(strings.TrimSpace(callbackURL))
	if strings.TrimSpace(merchantID) == "" || err != nil || parsedCallback.Host == "" || parsedCallback.Scheme != "https" && parsedCallback.Scheme != "http" {
		return errors.New("invalid payment notification configuration")
	}
	p.merchantID, p.callbackURL, p.secrets = merchantID, strings.TrimRight(callbackURL, "/"), parsedSecrets
	return nil
}

func (p *PaymentCoordinator) ConfigureTrustedNotifications(merchantID string) error {
	if !p.Capabilities().NotificationSupported {
		return payment.ErrUnsupported
	}
	merchantID = strings.TrimSpace(merchantID)
	if merchantID == "" {
		return errors.New("payment merchant ID is required for trusted provider notifications")
	}
	p.merchantID = merchantID
	return nil
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
		if external, ok := p.productSKUs[product.ProductID]; ok && external.AmountFen == product.AmountFen && external.Currency == product.Currency {
			product.Channels = intersectChannels(product.Channels, external.Channels, p.Capabilities().Channels)
			if len(product.Channels) == 0 {
				continue
			}
			filtered = append(filtered, product)
		}
	}
	return filtered
}

func (p *PaymentCoordinator) ApplyProductAvailability(ctx context.Context, products []Product) ([]Product, error) {
	if p.repository == nil {
		return products, nil
	}
	return p.repository.ApplyProductAvailability(ctx, p.Capabilities().ProviderID, p.Capabilities().Version, products)
}

func intersectChannels(groups ...[]string) []string {
	if len(groups) == 0 {
		return nil
	}
	result := make([]string, 0, len(groups[0]))
	for _, channel := range groups[0] {
		channel = strings.ToUpper(strings.TrimSpace(channel))
		if channel == "" {
			continue
		}
		present := true
		for _, group := range groups[1:] {
			found := false
			for _, candidate := range group {
				found = found || strings.EqualFold(candidate, channel)
			}
			present = present && found
		}
		if present {
			result = append(result, channel)
		}
	}
	return result
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func (p *PaymentCoordinator) Capabilities() PaymentCapabilities {
	capabilities := p.provider.Capabilities()
	switch {
	case capabilities.BillSupported:
		capabilities.ReconciliationMode = "API"
	case capabilities.BillFileImportSupported:
		capabilities.ReconciliationMode = "FILE_IMPORT"
	default:
		capabilities.ReconciliationMode = "QUERY_DEGRADED"
	}
	capabilities.FormalCollectionAdmitted = capabilities.FormalCollectionEligible(int64(latePaymentReviewGrace.Seconds()))
	return capabilities
}

func (p *PaymentCoordinator) Policy() PaymentPolicy {
	capabilities := p.Capabilities()
	return PaymentPolicy{ReusePendingOrderAfterExpiry: capabilities.ReusePendingOrderAfterExpiry, ReviewLatePayment: capabilities.ReviewLatePayment, MaximumLateConfirmation: time.Duration(capabilities.MaximumLateConfirmationSeconds) * time.Second}
}

func (p *PaymentCoordinator) payableWindow() time.Duration {
	window := time.Duration(p.Capabilities().MaximumPayableWindowSeconds) * time.Second
	if window <= 0 {
		return 15 * time.Minute
	}
	return window
}

func (p *PaymentCoordinator) CreateOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, idempotencyKey string) (PaymentOrder, bool, error) {
	return p.createOrder(ctx, requesterID, walletID, nil, productID, channel, "", idempotencyKey)
}

func (p *PaymentCoordinator) CreateMobileOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, mobile, idempotencyKey string) (PaymentOrder, bool, error) {
	return p.createOrder(ctx, requesterID, walletID, nil, productID, channel, mobile, idempotencyKey)
}

func (p *PaymentCoordinator) CreateOrganizationMobileOrder(ctx context.Context, requesterID, walletID, organizationID uuid.UUID, productID, channel, mobile, idempotencyKey string) (PaymentOrder, bool, error) {
	if organizationID == uuid.Nil {
		return PaymentOrder{}, false, ErrConflict
	}
	return p.createOrder(ctx, requesterID, walletID, &organizationID, productID, channel, mobile, idempotencyKey)
}

func (p *PaymentCoordinator) createOrder(ctx context.Context, requesterID, walletID uuid.UUID, beneficiaryOrganizationID *uuid.UUID, productID, channel, mobile, idempotencyKey string) (PaymentOrder, bool, error) {
	channel = strings.ToUpper(channel)
	if len(idempotencyKey) < 16 || !validPaymentChannel(channel) || !p.Capabilities().SupportsChannel(channel) {
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
	beneficiary := "PERSONAL"
	if beneficiaryOrganizationID != nil {
		beneficiary = beneficiaryOrganizationID.String()
	}
	requestDigest := sha256.Sum256([]byte(requesterID.String() + "|" + walletID.String() + "|" + beneficiary + "|" + productID + "|" + channel + "|" + mobile))
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
			existing.ExpiresAt = now.Add(p.payableWindow())
			providerPayableUntil := now.Add(p.payableWindow())
			lateReviewBoundaryAt := now.Add(72 * time.Hour)
			finalArchiveEligibleAt := providerPayableUntil.Add(time.Duration(p.Capabilities().MaximumLateConfirmationSeconds)*time.Second + 7*24*time.Hour)
			if _, err := tx.Exec(ctx, `UPDATE payment_orders SET expires_at=$2,provider_payable_until=$3,late_review_boundary_at=$4,final_archive_eligible_at=$5,reconciliation_next_at=$6,reconciliation_attempts=0,reconciliation_last_error=NULL,updated_at=$6 WHERE id=$1`, existing.ID, existing.ExpiresAt, providerPayableUntil, lateReviewBoundaryAt, finalArchiveEligibleAt, now); err != nil {
				return PaymentOrder{}, false, err
			}
			existing.ProviderPayableUntil, existing.LateReviewBoundaryAt, existing.FinalArchiveEligibleAt = providerPayableUntil, lateReviewBoundaryAt, finalArchiveEligibleAt
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
	if external.MappingID == uuid.Nil || external.SKU == "" || external.AmountFen != product.AmountFen || external.Currency != product.Currency || !containsFold(external.Channels, channel) || !p.Capabilities().SupportsChannel(channel) || p.Capabilities().CommerceCustomerRequired && mobile == "" {
		return PaymentOrder{}, false, ErrConflict
	}
	if external.Status == "FROZEN" && !p.Capabilities().FormalCollectionEligible(7*24*60*60) {
		return PaymentOrder{}, false, ErrConflict
	}
	var providerOrdersEnabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT new_orders_enabled FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2),TRUE)`, p.Capabilities().ProviderID, p.Capabilities().Version).Scan(&providerOrdersEnabled); err != nil {
		return PaymentOrder{}, false, err
	}
	if !providerOrdersEnabled {
		return PaymentOrder{}, false, ErrConflict
	}
	var productOrdersEnabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT new_orders_enabled FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3),TRUE)`, p.Capabilities().ProviderID, p.Capabilities().Version, productID).Scan(&productOrdersEnabled); err != nil {
		return PaymentOrder{}, false, err
	}
	if !productOrdersEnabled {
		return PaymentOrder{}, false, ErrConflict
	}
	wallet, walletErr := getWallet(ctx, tx, walletID, false)
	if walletErr != nil {
		return PaymentOrder{}, false, ErrNotFound
	}
	if beneficiaryOrganizationID == nil && (wallet.OwnerType != "PERSONAL" || wallet.OwnerID != requesterID) {
		return PaymentOrder{}, false, ErrConflict
	}
	if beneficiaryOrganizationID != nil && (wallet.OwnerType != "ORGANIZATION" || wallet.OwnerID != *beneficiaryOrganizationID) {
		return PaymentOrder{}, false, ErrConflict
	}
	merchantDigest := sha256.Sum256([]byte(requesterID.String() + "|" + idempotencyKey))
	providerPayableUntil := now.Add(p.payableWindow())
	lateReviewBoundaryAt := now.Add(72 * time.Hour)
	finalArchiveEligibleAt := providerPayableUntil.Add(time.Duration(p.Capabilities().MaximumLateConfirmationSeconds)*time.Second + 7*24*time.Hour)
	order := PaymentOrder{ID: uuid.Must(uuid.NewV7()), MerchantOrderNo: "pv-" + hex.EncodeToString(merchantDigest[:16]), RequesterID: requesterID, BeneficiaryOrganizationID: beneficiaryOrganizationID, WalletID: walletID, ProductID: productID, Channel: channel, AmountFen: product.AmountFen, Currency: product.Currency, CreditMicrocredits: product.CreditMicrocredits, Status: "CREATED", FulfillmentStatus: "PENDING", CloseStatus: "OPEN", ExpiresAt: now.Add(p.payableWindow()), Mobile: mobile, ProviderSKU: external.SKU, ProviderID: p.Capabilities().ProviderID, ProviderVersion: p.Capabilities().Version, ProviderPayableUntil: providerPayableUntil, LateReviewBoundaryAt: lateReviewBoundaryAt, FinalArchiveEligibleAt: finalArchiveEligibleAt, ArchiveStatus: "ACTIVE"}
	snapshot, _ := json.Marshal(product)
	mappingSnapshot, _ := json.Marshal(external)
	capabilitySnapshot, _ := json.Marshal(p.Capabilities())
	payerMappingSnapshot, _ := json.Marshal(map[string]any{"payer_user_id": requesterID, "provider_id": p.Capabilities().ProviderID, "provider_version": p.Capabilities().Version, "commerce_customer_required": p.Capabilities().CommerceCustomerRequired, "status": map[bool]string{true: "READY", false: "NOT_REQUIRED"}[p.Capabilities().CommerceCustomerRequired]})
	_, err = tx.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,payer_commerce_mapping_snapshot,beneficiary_organization_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,mobile_e164,provider_sku,provider_id,provider_version,product_mapping_id,product_mapping_snapshot,provider_capability_snapshot) VALUES ($1,$2,$3,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'CREATED','PENDING',$13,$14,$15,$16,$17,$17,NULLIF($18,''),NULLIF($19,''),$20,$21,$22,$23,$24)`, order.ID, order.MerchantOrderNo, requesterID, payerMappingSnapshot, beneficiaryOrganizationID, walletID, productID, snapshot, channel, order.AmountFen, order.Currency, order.CreditMicrocredits, order.ExpiresAt, providerPayableUntil, lateReviewBoundaryAt, finalArchiveEligibleAt, now, mobile, external.SKU, p.Capabilities().ProviderID, p.Capabilities().Version, external.MappingID, mappingSnapshot, capabilitySnapshot)
	if err != nil {
		return PaymentOrder{}, false, err
	}
	providerOrder, err := providerCall(p, "ORDER_CREATE", order.Channel, true, func() (payment.ProviderOrder, error) {
		return p.provider.Create(ctx, payment.Order{MerchantOrderNo: order.MerchantOrderNo, MerchantCustomerKey: requesterID.String(), Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Mobile: order.Mobile, ExternalSKU: order.ProviderSKU}, p.callbackURL)
	})
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
	err := tx.QueryRow(ctx, `SELECT id,merchant_order_no,requester_id,beneficiary_organization_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,close_status,COALESCE(payment_url,''),COALESCE(qr_data,''),primary_provider_transaction_id,expires_at,paid_at,provider_id,provider_version FROM payment_orders WHERE requester_id=$1 AND wallet_id=$2 AND product_id=$3 AND channel=$4 AND status IN ('CREATED','PENDING') AND close_status='OPEN' AND ($6 OR expires_at>$5) ORDER BY created_at DESC,id DESC LIMIT 1 FOR UPDATE`, requesterID, walletID, productID, channel, now, includeExpired).Scan(&order.ID, &order.MerchantOrderNo, &order.RequesterID, &order.BeneficiaryOrganizationID, &order.WalletID, &order.ProductID, &order.Channel, &order.AmountFen, &order.Currency, &order.CreditMicrocredits, &order.Status, &order.FulfillmentStatus, &order.CloseStatus, &order.PaymentURL, &order.QRData, &order.PrimaryProviderTransactionID, &order.ExpiresAt, &order.PaidAt, &order.ProviderID, &order.ProviderVersion)
	return order, err
}

type paymentNotification struct {
	NotificationID              uuid.UUID       `json:"notification_id"`
	MerchantID                  string          `json:"merchant_id"`
	MerchantOrderNo             string          `json:"merchant_order_no"`
	ChannelTransaction          string          `json:"channel_transaction_id"`
	Channel                     string          `json:"channel"`
	Status                      string          `json:"status"`
	AmountFen                   int64           `json:"amount_fen"`
	Currency                    string          `json:"currency"`
	PaidAt                      time.Time       `json:"paid_at"`
	Timestamp                   int64           `json:"timestamp"`
	ProviderOrderReference      string          `json:"provider_order_reference,omitempty"`
	ProviderStatus              string          `json:"provider_status,omitempty"`
	ProviderTraceID             string          `json:"provider_trace_id,omitempty"`
	ExternalCustomerKey         string          `json:"external_customer_key,omitempty"`
	ExternalSKU                 string          `json:"external_sku,omitempty"`
	ProviderEventAt             *time.Time      `json:"provider_event_at,omitempty"`
	ProviderPayableUntil        *time.Time      `json:"provider_payable_until,omitempty"`
	PaymentInstructionExpiresAt *time.Time      `json:"payment_instruction_expires_at,omitempty"`
	ProviderPayload             json.RawMessage `json:"provider_payload,omitempty"`
}

func (p *PaymentCoordinator) PersistNotification(ctx context.Context, channel, signature string, body []byte, source string) (uuid.UUID, bool, error) {
	if !p.Capabilities().NotificationSupported {
		return uuid.Nil, false, payment.ErrUnsupported
	}
	channel = strings.ToUpper(channel)
	secret := p.secrets[channel]
	expected := paymentSignature(secret, body)
	if len(signature) != len(expected) || subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return uuid.Nil, false, ErrForbiddenPayment
	}
	var notification paymentNotification
	if err := json.Unmarshal(body, &notification); err != nil || notification.NotificationID == uuid.Nil || time.Unix(notification.Timestamp, 0).Before(p.now().Add(-5*time.Minute)) || time.Unix(notification.Timestamp, 0).After(p.now().Add(5*time.Minute)) {
		return uuid.Nil, false, ErrForbiddenPayment
	}
	return p.persistNotification(ctx, channel, notification, body, source)
}

func (p *PaymentCoordinator) PersistProviderNotification(ctx context.Context, providerID, merchantID string, notificationID uuid.UUID, sentAt time.Time, provider ProviderOrder, payload []byte, source string) (uuid.UUID, bool, error) {
	if !p.Capabilities().NotificationSupported || !strings.EqualFold(strings.TrimSpace(providerID), p.Capabilities().ProviderID) || strings.TrimSpace(merchantID) != p.merchantID || notificationID == uuid.Nil || sentAt.Before(p.now().Add(-5*time.Minute)) || sentAt.After(p.now().Add(5*time.Minute)) || provider.MerchantOrderNo == "" || provider.Channel == "" || provider.AmountFen <= 0 || provider.Currency == "" {
		return uuid.Nil, false, ErrForbiddenPayment
	}
	notification := paymentNotification{
		NotificationID: notificationID, MerchantID: p.merchantID, MerchantOrderNo: provider.MerchantOrderNo,
		Channel: provider.Channel, Status: provider.Status, AmountFen: provider.AmountFen, Currency: provider.Currency,
		Timestamp: sentAt.Unix(), ProviderOrderReference: provider.ProviderOrderReference, ProviderStatus: provider.ProviderStatus,
		ProviderTraceID: provider.ProviderTraceID, ProviderEventAt: provider.ProviderEventAt,
		ExternalCustomerKey: provider.ExternalCustomerKey, ExternalSKU: provider.ExternalSKU,
		ProviderPayableUntil: provider.ProviderPayableUntil, PaymentInstructionExpiresAt: provider.PaymentInstructionExpiresAt,
		ProviderPayload: append(json.RawMessage(nil), payload...),
	}
	if provider.ChannelTransactionID != nil {
		notification.ChannelTransaction = *provider.ChannelTransactionID
	}
	if provider.PaidAt != nil {
		notification.PaidAt = *provider.PaidAt
	}
	body, err := json.Marshal(notification)
	if err != nil {
		return uuid.Nil, false, err
	}
	return p.persistNotification(ctx, provider.Channel, notification, body, source)
}

func (p *PaymentCoordinator) persistNotification(ctx context.Context, channel string, notification paymentNotification, body []byte, source string) (uuid.UUID, bool, error) {
	channel = strings.ToUpper(strings.TrimSpace(channel))
	if notification.NotificationID == uuid.Nil || notification.MerchantID != p.merchantID || notification.Channel != channel || notification.AmountFen <= 0 || notification.Currency == "" || notification.Status == "" {
		return uuid.Nil, false, ErrForbiddenPayment
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	defer tx.Rollback(ctx)
	digest := sha256.Sum256(body)
	command, err := tx.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result) VALUES ($1,$2,$3,$4,$5,'RECEIVED') ON CONFLICT DO NOTHING`, notification.NotificationID, channel, hex.EncodeToString(digest[:]), body, p.now())
	if err != nil {
		return uuid.Nil, false, err
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM payment_notifications WHERE notification_id=$1`, notification.NotificationID).Scan(&existingHash); err != nil || existingHash != hex.EncodeToString(digest[:]) {
			return uuid.Nil, false, ErrForbiddenPayment
		}
		return notification.NotificationID, false, tx.Commit(ctx)
	}
	sourceHash := sha256.Sum256([]byte(strings.TrimSpace(source)))
	var recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_risk_attempts WHERE action='CALLBACK' AND provider_id=$1 AND source_hash=$2 AND occurred_at>$3::timestamptz-interval '1 second'`, p.Capabilities().ProviderID, sourceHash[:], p.now()).Scan(&recent); err != nil {
		return uuid.Nil, false, err
	}
	result := "ACCEPTED"
	if recent >= 200 {
		result = "OVER_CAPACITY"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payment_risk_attempts(id,action,source_hash,provider_id,result,provider_dispatched,occurred_at) VALUES($1,'CALLBACK',$2,$3,$4,false,$5)`, uuid.Must(uuid.NewV7()), sourceHash[:], p.Capabilities().ProviderID, result, p.now()); err != nil {
		return uuid.Nil, false, err
	}
	return notification.NotificationID, true, tx.Commit(ctx)
}

func (p *PaymentCoordinator) ProcessNotification(ctx context.Context, channel, signature string, body []byte) error {
	id, _, err := p.PersistNotification(ctx, channel, signature, body, "source:direct")
	if err != nil {
		return err
	}
	return p.ProcessPersistedNotification(ctx, id)
}

func (p *PaymentCoordinator) ProcessPersistedNotification(ctx context.Context, notificationID uuid.UUID) error {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var channel, result string
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT channel,payload,result FROM payment_notifications WHERE notification_id=$1 FOR UPDATE`, notificationID).Scan(&channel, &body, &result); err != nil {
		return err
	}
	if result != "RECEIVED" {
		return tx.Commit(ctx)
	}
	var notification paymentNotification
	if json.Unmarshal(body, &notification) != nil {
		return ErrForbiddenPayment
	}
	order, err := getPaymentOrderForUpdate(ctx, tx, notification.MerchantOrderNo)
	if err != nil {
		return err
	}
	if notification.MerchantID != p.merchantID || notification.Channel != channel {
		return ErrForbiddenPayment
	}
	provider := ProviderOrder{MerchantOrderNo: notification.MerchantOrderNo, ProviderOrderReference: notification.ProviderOrderReference, Channel: notification.Channel, AmountFen: notification.AmountFen, Currency: notification.Currency, Status: notification.Status, ProviderStatus: notification.ProviderStatus, ProviderTraceID: notification.ProviderTraceID, ExternalCustomerKey: notification.ExternalCustomerKey, ExternalSKU: notification.ExternalSKU, ProviderEventAt: notification.ProviderEventAt, ProviderPayableUntil: notification.ProviderPayableUntil, PaymentInstructionExpiresAt: notification.PaymentInstructionExpiresAt, AmountSource: "PROVIDER"}
	if provider.ProviderStatus == "" {
		provider.ProviderStatus = notification.Status
	}
	if notification.ChannelTransaction != "" {
		provider.ChannelTransactionID = &notification.ChannelTransaction
	}
	if !notification.PaidAt.IsZero() {
		provider.PaidAt = &notification.PaidAt
	}
	var evidenceID uuid.UUID
	var mergeErr error
	merged := order
	if provider.Status == "SUCCESS" {
		if provider.ChannelTransactionID == nil || provider.PaidAt == nil {
			return ErrForbiddenPayment
		}
		merged, evidenceID, mergeErr = p.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "CALLBACK", notification.NotificationID.String(), body, provider, false, p.now())
	} else {
		mergeErr = p.mergeProviderObservationWithinTransaction(ctx, tx, order, "CALLBACK", notification.NotificationID.String(), body, provider, p.now())
		if lookupErr := tx.QueryRow(ctx, `SELECT id FROM payment_evidence WHERE provider_id=$1 AND provider_version=$2 AND evidence_type='CALLBACK' AND evidence_key=$3 ORDER BY received_at DESC LIMIT 1`, order.ProviderID, order.ProviderVersion, notification.NotificationID.String()).Scan(&evidenceID); lookupErr != nil {
			return lookupErr
		}
	}
	if mergeErr != nil && !errors.Is(mergeErr, ErrConflict) {
		return mergeErr
	}
	result = "PROCESSED"
	if merged.Status == "MANUAL_REVIEW" || errors.Is(mergeErr, ErrConflict) {
		result = "MANUAL_REVIEW"
	}
	_, err = tx.Exec(ctx, `UPDATE payment_notifications SET result=$2,processed_at=$3,payment_evidence_id=$4 WHERE notification_id=$1`, notification.NotificationID, result, p.now(), evidenceID)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return mergeErr
}

var ErrForbiddenPayment = errors.New("payment notification rejected")

func issuePackageWithinTransaction(ctx context.Context, tx pgx.Tx, order PaymentOrder, paymentTransactionID uuid.UUID, now time.Time, repair bool) (uuid.UUID, error) {
	if paymentTransactionID == uuid.Nil {
		return uuid.Nil, ErrConflict
	}
	wallet, err := getWallet(ctx, tx, order.WalletID, true)
	if err != nil || wallet.Status != "ACTIVE" {
		return uuid.Nil, ErrWalletUnavailable
	}
	var snapshot []byte
	var beneficiaryOrganizationID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT product_snapshot,beneficiary_organization_id FROM payment_orders WHERE id=$1`, order.ID).Scan(&snapshot, &beneficiaryOrganizationID); err != nil {
		return uuid.Nil, err
	}
	if beneficiaryOrganizationID == nil {
		if wallet.OwnerType != "PERSONAL" || wallet.OwnerID != order.RequesterID {
			return uuid.Nil, ErrConflict
		}
	} else if wallet.OwnerType != "ORGANIZATION" || wallet.OwnerID != *beneficiaryOrganizationID {
		return uuid.Nil, ErrConflict
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
	journalType, reference, description := "RECHARGE", "payment-transaction:"+paymentTransactionID.String(), "payment recharge"
	if repair {
		journalType, reference, description = "REPAIR_CREDIT", "repair-payment-transaction:"+paymentTransactionID.String(), "verified payment repair credit"
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
	baseAmount := int64(0)
	for _, component := range components {
		if component.kind == "BASE" {
			baseAmount = component.amount
			break
		}
	}
	appliedToReceivables, err := settleOpenReceivablesWithinTransaction(ctx, tx, order.WalletID, baseAmount, "CASH_RECHARGE_BASE", reference, entryID, now)
	if err != nil {
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
		remaining := component.amount
		if component.kind == "BASE" {
			remaining -= appliedToReceivables
		}
		if _, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES ($1,$2,'RECHARGE',$3,$4,$5,$6,$7,$8)`, component.lotID, order.WalletID, lotReference, component.amount, remaining, refundable, validUntil, now); err != nil {
			return uuid.Nil, err
		}
	}
	var packageUserID *uuid.UUID
	if beneficiaryOrganizationID == nil {
		packageUserID = &order.RequesterID
	}
	packageStatus := "ACTIVE"
	if amount-appliedToReceivables == 0 {
		packageStatus = "CONSUMED"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_used,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$14,$14,'PAYMENT',$18)`, packageID, packageUserID, order.WalletID, order.ID, paymentTransactionID, primaryLotID, order.ProductID, product.PlanName, order.ProviderSKU, order.AmountFen, amount, appliedToReceivables, amount-appliedToReceivables, now, validFrom, validUntil, packageStatus, reference); err != nil {
		return uuid.Nil, err
	}
	for _, component := range components {
		available, used := component.amount, int64(0)
		if component.kind == "BASE" {
			available, used = component.amount-appliedToReceivables, appliedToReceivables
		}
		status := "ACTIVE"
		if available == 0 {
			status = "CONSUMED"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,used_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,$3,$1,$4,$5,$6,$7,$8,$9,$7,$7)`, component.lotID, packageID, component.kind, component.amount, available, used, now, validUntil, status); err != nil {
			return uuid.Nil, err
		}
		if available > 0 {
			if err = insertPosting(ctx, tx, entryID, accountID, available, &component.lotID, now); err != nil {
				return uuid.Nil, err
			}
		}
	}
	if appliedToReceivables > 0 {
		if err = insertPosting(ctx, tx, entryID, receivableAccount, appliedToReceivables, nil, now); err != nil {
			return uuid.Nil, err
		}
	}
	if err = insertPosting(ctx, tx, entryID, paymentClearingAccount, -amount, nil, now); err != nil {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1`, order.WalletID, amount-appliedToReceivables, now)
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
	err := tx.QueryRow(ctx, `SELECT id,merchant_order_no,requester_id,beneficiary_organization_id,wallet_id,product_id,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,close_status,COALESCE(payment_url,''),COALESCE(qr_data,''),primary_provider_transaction_id,expires_at,paid_at,COALESCE(provider_sku,''),provider_id,provider_version FROM payment_orders WHERE merchant_order_no=$1 FOR UPDATE`, merchant).Scan(&o.ID, &o.MerchantOrderNo, &o.RequesterID, &o.BeneficiaryOrganizationID, &o.WalletID, &o.ProductID, &o.Channel, &o.AmountFen, &o.Currency, &o.CreditMicrocredits, &o.Status, &o.FulfillmentStatus, &o.CloseStatus, &o.PaymentURL, &o.QRData, &o.PrimaryProviderTransactionID, &o.ExpiresAt, &o.PaidAt, &o.ProviderSKU, &o.ProviderID, &o.ProviderVersion)
	return o, err
}

func paymentSignature(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	_, _ = m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}
func validPaymentChannel(v string) bool {
	if v == "" || len(v) > 32 {
		return false
	}
	for _, char := range v {
		if char != '_' && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}
