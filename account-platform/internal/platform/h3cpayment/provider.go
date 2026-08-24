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
	CreateOrder(context.Context, string, string, string) error
	QueryOrder(context.Context, string) (h3cmall.Order, error)
}

type Product struct {
	SKU       string
	AmountFen int64
}

type Provider struct {
	client   mallClient
	products map[string]Product
}

func New(client mallClient, products map[string]Product) (*Provider, error) {
	if client == nil || len(products) == 0 {
		return nil, errors.New("H3C Mall payment adapter requires a client and product mapping")
	}
	return &Provider{client: client, products: products}, nil
}

func (p *Provider) Capabilities() payment.Capabilities {
	return payment.Capabilities{
		ProviderID: "h3c_mall", Version: "h3c-mall-v1", Channels: []string{"ALIPAY"},
		OrderQuerySupported: true, MinimumQueryIntervalSeconds: 15, NotificationSupported: true,
		AuthoritativeUnpaidTerminal: false, ManualRepairSupported: true,
		CommerceCustomerRequired: true, ReusePendingOrderAfterExpiry: true, ReviewLatePayment: true,
	}
}

func (p *Provider) Create(ctx context.Context, order payment.Order, _ string) (payment.ProviderOrder, error) {
	if order.Channel != "ALIPAY" || strings.TrimSpace(order.Mobile) == "" || strings.TrimSpace(order.ExternalSKU) == "" {
		return payment.ProviderOrder{}, errors.New("invalid H3C Mall payment request")
	}
	if err := p.client.CreateOrder(ctx, order.MerchantOrderNo, order.Mobile, order.ExternalSKU); err != nil {
		existing, queryErr := p.client.QueryOrder(ctx, order.MerchantOrderNo)
		if queryErr != nil || existing.Mobile != order.Mobile || existing.SKU != order.ExternalSKU {
			return payment.ProviderOrder{}, err
		}
		return p.mapOrder(existing)
	}
	return payment.ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "PENDING", ProviderStatus: "PENDING", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, AmountSource: "EXPECTED_SNAPSHOT"}, nil
}

func (p *Provider) Get(ctx context.Context, merchantOrderNo string) (payment.ProviderOrder, error) {
	order, err := p.client.QueryOrder(ctx, merchantOrderNo)
	if err != nil {
		return payment.ProviderOrder{}, err
	}
	return p.mapOrder(order)
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

func (p *Provider) mapOrder(order h3cmall.Order) (payment.ProviderOrder, error) {
	var product Product
	found := false
	for _, candidate := range p.products {
		if candidate.SKU == order.SKU {
			product, found = candidate, true
			break
		}
	}
	if !found || strings.TrimSpace(order.OrderKey) == "" {
		return payment.ProviderOrder{}, errors.New("H3C Mall returned an unknown product or order")
	}
	amountFen, amountSource := product.AmountFen, "EXPECTED_SNAPSHOT"
	if order.AmountFen != nil && *order.AmountFen > 0 {
		amountFen, amountSource = *order.AmountFen, "PROVIDER"
	}
	currency := strings.ToUpper(strings.TrimSpace(order.Currency))
	if currency == "" {
		currency = "CNY"
	}
	result := payment.ProviderOrder{MerchantOrderNo: order.OrderKey, ProviderOrderReference: strings.TrimSpace(order.ProviderOrderNo), QRData: order.AliQRData, ProviderStatus: order.Status, ProviderTraceID: order.TraceID, Channel: "ALIPAY", AmountFen: amountFen, Currency: currency, AmountSource: amountSource}
	if parsed, err := parseOptionalTime(order.StatusOccurredAt); err != nil {
		return payment.ProviderOrder{}, err
	} else {
		result.ProviderEventAt = parsed
	}
	if parsed, err := parseOptionalTime(order.ProviderPayableUntil); err != nil {
		return payment.ProviderOrder{}, err
	} else {
		result.ProviderPayableUntil = parsed
	}
	if parsed, err := parseOptionalTime(order.PaymentInstructionExpiresAt); err != nil {
		return payment.ProviderOrder{}, err
	} else {
		result.PaymentInstructionExpiresAt = parsed
	}
	switch strings.ToUpper(order.Status) {
	case "PENDING", "PROCESSING", "WAIT_PAY":
		result.Status = "PENDING"
	case "PAID":
		result.Status = "SUCCESS"
		transactionID := strings.TrimSpace(order.PayUniqueKey)
		if transactionID == "" && order.PaymentOrderID != nil {
			transactionID = strconv.FormatInt(*order.PaymentOrderID, 10)
		}
		if transactionID == "" {
			return payment.ProviderOrder{}, errors.New("H3C Mall paid order has no transaction reference")
		}
		paidAt, err := parseTime(order.PayTime)
		if err != nil {
			return payment.ProviderOrder{}, err
		}
		result.ChannelTransactionID, result.PaidAt = &transactionID, &paidAt
	case "PAY_CLOSED":
		result.Status = "CLOSED"
	case "FAILED":
		result.Status = "REVIEW_REQUIRED"
	default:
		return payment.ProviderOrder{}, errors.New("H3C Mall returned an unknown payment status")
	}
	return result, nil
}

func parseOptionalTime(value string) (*time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := parseTime(value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, location)
	if err != nil {
		return time.Time{}, errors.New("H3C Mall returned an invalid payment time")
	}
	return parsed.UTC(), nil
}
