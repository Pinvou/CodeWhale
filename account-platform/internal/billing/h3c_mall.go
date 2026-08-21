package billing

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
)

type h3cMallClient interface {
	CreateOrder(context.Context, string, string, string) error
	QueryOrder(context.Context, string) (h3cmall.Order, error)
}

type H3CMallProvider struct {
	client   h3cMallClient
	products map[string]ExternalProduct
}

func NewH3CMallProvider(client h3cMallClient, products map[string]ExternalProduct) (*H3CMallProvider, error) {
	if client == nil || len(products) == 0 {
		return nil, errors.New("H3C Mall payment provider requires a client and product mapping")
	}
	return &H3CMallProvider{client: client, products: products}, nil
}

func (p *H3CMallProvider) Capabilities() PaymentCapabilities {
	return PaymentCapabilities{Provider: "h3c_mall", OrderSyncSupported: true}
}

func (p *H3CMallProvider) Create(ctx context.Context, order PaymentOrder, _ string) (ProviderOrder, error) {
	if order.Channel != "ALIPAY" || strings.TrimSpace(order.Mobile) == "" || strings.TrimSpace(order.ProviderSKU) == "" {
		return ProviderOrder{}, ErrConflict
	}
	if err := p.client.CreateOrder(ctx, order.MerchantOrderNo, order.Mobile, order.ProviderSKU); err != nil {
		// The Mall accepts orders asynchronously. If the acceptance response is
		// lost, retrying the same idempotency key uses the same order key. Query
		// that key before failing so a remotely accepted order is not orphaned.
		existing, queryErr := p.client.QueryOrder(ctx, order.MerchantOrderNo)
		if queryErr != nil || existing.Mobile != order.Mobile || existing.SKU != order.ProviderSKU {
			return ProviderOrder{}, err
		}
		return p.mapOrder(existing)
	}
	// Mall accepts the order asynchronously. Querying immediately can race its
	// processing pipeline, so the normal polling path obtains the actual QR.
	return ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "PENDING", ProviderStatus: "PENDING", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency}, nil
}

func (p *H3CMallProvider) Get(ctx context.Context, merchantOrderNo string) (ProviderOrder, error) {
	order, err := p.client.QueryOrder(ctx, merchantOrderNo)
	if err != nil {
		return ProviderOrder{}, err
	}
	return p.mapOrder(order)
}

func (p *H3CMallProvider) Close(ctx context.Context, merchantOrderNo string) (ProviderOrder, error) {
	// The supplied Mall contract has no close API. Keep querying the source of
	// truth; its PAY_CLOSED state will eventually close the local order.
	return p.Get(ctx, merchantOrderNo)
}

func (p *H3CMallProvider) Refund(context.Context, string, string, int64) (ProviderRefund, error) {
	return ProviderRefund{}, errors.New("H3C Mall refund API is not available in the supplied contract")
}

func (p *H3CMallProvider) Bill(context.Context, string, time.Time) ([]ProviderBillEntry, error) {
	return nil, errors.New("H3C Mall reconciliation bill API is not available in the supplied contract")
}

func (p *H3CMallProvider) mapOrder(order h3cmall.Order) (ProviderOrder, error) {
	var product ExternalProduct
	found := false
	for _, candidate := range p.products {
		if candidate.SKU == order.SKU {
			product, found = candidate, true
			break
		}
	}
	if !found || strings.TrimSpace(order.OrderKey) == "" {
		return ProviderOrder{}, ErrConflict
	}
	result := ProviderOrder{MerchantOrderNo: order.OrderKey, QRData: order.AliQRData, ProviderStatus: order.Status, ProviderTraceID: order.TraceID, Channel: "ALIPAY", AmountFen: product.AmountFen, Currency: "CNY"}
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
			return ProviderOrder{}, ErrConflict
		}
		paidAt, err := parseH3CMallTime(order.PayTime)
		if err != nil {
			return ProviderOrder{}, err
		}
		result.ChannelTransactionID, result.PaidAt = &transactionID, &paidAt
	case "PAY_CLOSED":
		result.Status = "CLOSED"
	case "FAILED":
		result.Status = "FAILED"
	default:
		return ProviderOrder{}, ErrConflict
	}
	return result, nil
}

func parseH3CMallTime(value string) (time.Time, error) {
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
		return time.Time{}, ErrConflict
	}
	return parsed.UTC(), nil
}
