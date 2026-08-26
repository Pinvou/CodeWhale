package h3cpayment

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
)

type mallClient interface {
	CreateOrder(context.Context, string, string, string, string) error
	QueryOrder(context.Context, string) (h3cmall.Order, error)
}

type Product struct {
	SKU       string
	AmountFen int64
}

type ProductResolver func(context.Context, string) (Product, error)

type Provider struct {
	client         mallClient
	resolveProduct ProductResolver
}

func New(client mallClient, resolver ProductResolver) (*Provider, error) {
	if client == nil || resolver == nil {
		return nil, errors.New("H3C Mall payment adapter requires a client and dynamic product resolver")
	}
	return &Provider{client: client, resolveProduct: resolver}, nil
}

func (p *Provider) Capabilities() payment.Capabilities {
	return payment.Capabilities{
		ProviderID: "h3c_mall", Version: "h3c-mall-v1", Channels: []string{"ALIPAY"},
		OrderQuerySupported: true, MinimumQueryIntervalSeconds: 2, NotificationSupported: true,
		AuthoritativeUnpaidTerminal: false, ManualRepairSupported: true,
		CommerceCustomerRequired: true, ReusePendingOrderAfterExpiry: true, ReviewLatePayment: true,
	}
}

func (p *Provider) Create(ctx context.Context, order payment.Order, _ string) (payment.ProviderOrder, error) {
	if order.Channel != "ALIPAY" || strings.TrimSpace(order.MerchantCustomerKey) == "" || strings.TrimSpace(order.Mobile) == "" || strings.TrimSpace(order.ExternalSKU) == "" {
		return payment.ProviderOrder{}, errors.New("invalid H3C Mall payment request")
	}
	if err := p.client.CreateOrder(ctx, order.MerchantOrderNo, order.MerchantCustomerKey, order.Mobile, order.ExternalSKU); err != nil {
		existing, queryErr := p.client.QueryOrder(ctx, order.MerchantOrderNo)
		if queryErr != nil || existing.MerchantCustomerKey != order.MerchantCustomerKey || existing.Mobile != order.Mobile || existing.SKU != order.ExternalSKU {
			return payment.ProviderOrder{}, err
		}
		return p.mapOrder(ctx, existing)
	}
	return payment.ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "PENDING", ProviderStatus: "PENDING", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, AmountSource: "EXPECTED_SNAPSHOT", ExternalCustomerKey: order.MerchantCustomerKey, ExternalSKU: order.ExternalSKU}, nil
}

func (p *Provider) Get(ctx context.Context, merchantOrderNo string) (payment.ProviderOrder, error) {
	order, err := p.client.QueryOrder(ctx, merchantOrderNo)
	if err != nil {
		return payment.ProviderOrder{}, err
	}
	return p.mapOrder(ctx, order)
}

func (p *Provider) Close(ctx context.Context, merchantOrderNo string) (payment.ProviderOrder, error) {
	return p.Get(ctx, merchantOrderNo)
}

func (p *Provider) Refund(context.Context, string, string, int64) (payment.Refund, error) {
	return payment.Refund{}, payment.ErrUnsupported
}

func (p *Provider) Bill(context.Context, string, time.Time) ([]payment.BillEntry, error) {
	return nil, payment.ErrUnsupported
}

func (p *Provider) mapOrder(ctx context.Context, order h3cmall.Order) (payment.ProviderOrder, error) {
	if strings.TrimSpace(order.OrderKey) == "" {
		return payment.ProviderOrder{}, errors.New("H3C Mall returned an unknown product or order")
	}
	product, err := p.resolveProduct(ctx, strings.TrimSpace(order.SKU))
	if err != nil {
		return payment.ProviderOrder{}, errors.New("H3C Mall returned an unknown product or order")
	}
	return MapMallOrder(order, &product)
}

// MapMallOrder is shared by active queries and the H3C callback profile so
// both paths interpret the Mall's native order snapshot identically. A nil
// expected product is used by callbacks and therefore requires provider money
// facts in the payload instead of falling back to a local snapshot.
func MapMallOrder(order h3cmall.Order, expected *Product) (payment.ProviderOrder, error) {
	order.OrderKey = strings.TrimSpace(order.OrderKey)
	order.MerchantCustomerKey = strings.TrimSpace(order.MerchantCustomerKey)
	order.SKU = strings.TrimSpace(order.SKU)
	if order.OrderKey == "" || order.MerchantCustomerKey == "" || order.SKU == "" {
		return payment.ProviderOrder{}, errors.New("H3C Mall order identity is incomplete")
	}
	amountFen, amountSource, err := mallAmountFen(order, expected)
	if err != nil {
		return payment.ProviderOrder{}, err
	}
	providerReference := strings.TrimSpace(order.TradeOrderCode)
	if providerReference == "" {
		providerReference = strings.TrimSpace(order.TradeContractCode)
	}
	result := payment.ProviderOrder{
		MerchantOrderNo: order.OrderKey, ProviderOrderReference: providerReference, QRData: order.AliQRData,
		ProviderStatus: strings.ToUpper(strings.TrimSpace(order.Status)), ProviderTraceID: strings.TrimSpace(order.TraceID),
		Channel: "ALIPAY", AmountFen: amountFen, Currency: "CNY", AmountSource: amountSource,
		ExternalCustomerKey: strings.TrimSpace(order.MerchantCustomerKey), ExternalSKU: strings.TrimSpace(order.SKU),
	}
	if order.UpdatedAt != nil {
		parsed, parseErr := h3cmall.ParseTimeValue(order.UpdatedAt)
		if parseErr != nil {
			return payment.ProviderOrder{}, parseErr
		}
		result.ProviderEventAt = &parsed
	}
	switch strings.ToUpper(order.Status) {
	case "PENDING", "PROCESSING", "WAIT_PAY":
		result.Status = "PENDING"
	case "PAID":
		result.Status = "SUCCESS"
		if status := strings.ToUpper(strings.TrimSpace(order.PaymentStatus)); status != "" && status != "PAY_SUCCESS" {
			return payment.ProviderOrder{}, errors.New("H3C Mall paid order has an inconsistent payment status")
		}
		transactionID := strings.TrimSpace(order.PayUniqueKey)
		if transactionID == "" && order.PaymentOrderID != nil {
			transactionID = strconv.FormatInt(*order.PaymentOrderID, 10)
		}
		if transactionID == "" {
			return payment.ProviderOrder{}, errors.New("H3C Mall paid order has no transaction reference")
		}
		paidAt, err := h3cmall.ParseTimeValue(order.PayTime)
		if err != nil {
			return payment.ProviderOrder{}, err
		}
		result.ChannelTransactionID, result.PaidAt = &transactionID, &paidAt
	case "PAY_CLOSED":
		result.Status = "CLOSED"
	case "FAILED":
		result.Status = "REVIEW_REQUIRED"
	default:
		result.Status = "REVIEW_REQUIRED"
	}
	return result, nil
}

func mallAmountFen(order h3cmall.Order, expected *Product) (int64, string, error) {
	need, err := order.NeedPayAmount.Fen()
	if err != nil {
		return 0, "", err
	}
	sku, err := order.SKUAmount.Fen()
	if err != nil {
		return 0, "", err
	}
	paid, err := order.PaidAmount.Fen()
	if err != nil {
		return 0, "", err
	}
	status := strings.ToUpper(strings.TrimSpace(order.Status))
	amount := need
	if amount == 0 {
		amount = sku
	}
	if status == "PAID" {
		if paid <= 0 {
			return 0, "", errors.New("H3C Mall paid order has no paid amount")
		}
		amount = paid
	}
	for _, candidate := range []int64{need, sku} {
		if candidate > 0 && amount > 0 && candidate != amount {
			return 0, "", errors.New("H3C Mall returned inconsistent order amounts")
		}
	}
	if amount > 0 {
		return amount, "PROVIDER", nil
	}
	if expected == nil || expected.AmountFen <= 0 {
		return 0, "", errors.New("H3C Mall order has no payable amount")
	}
	return expected.AmountFen, "EXPECTED_SNAPSHOT", nil
}
