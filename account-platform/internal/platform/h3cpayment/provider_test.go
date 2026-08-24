package h3cpayment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
)

type mallStub struct {
	created   []string
	order     h3cmall.Order
	createErr error
}

func (s *mallStub) CreateOrder(_ context.Context, orderKey, mobile, sku string) error {
	s.created = []string{orderKey, mobile, sku}
	return s.createErr
}

func (s *mallStub) QueryOrder(context.Context, string) (h3cmall.Order, error) { return s.order, nil }

func newTestProvider(t *testing.T, client mallClient) *Provider {
	t.Helper()
	provider, err := New(client, map[string]Product{"points-100": {SKU: "sku-100", AmountFen: 100}})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestCapabilitiesAreVersionedAndSupplierSpecific(t *testing.T) {
	capabilities := newTestProvider(t, &mallStub{}).Capabilities()
	if !capabilities.Valid() || capabilities.ProviderID != "h3c_mall" || capabilities.Version == "" || !capabilities.CommerceCustomerRequired || !capabilities.SupportsChannel("ALIPAY") || !capabilities.NotificationSupported || capabilities.RefundSupported || capabilities.AuthoritativeUnpaidTerminal || capabilities.TestOnly {
		t.Fatalf("unexpected capabilities: %#v", capabilities)
	}
}

func TestCreateRecoversAcceptedOrderWithoutInventingQR(t *testing.T) {
	client := &mallStub{createErr: errors.New("acceptance response lost"), order: h3cmall.Order{OrderKey: "pv-1", Mobile: "+8613800138000", SKU: "sku-100", Status: "WAIT_PAY"}}
	provider := newTestProvider(t, client)
	order, err := provider.Create(context.Background(), payment.Order{MerchantOrderNo: "pv-1", Mobile: "+8613800138000", ExternalSKU: "sku-100", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY"}, "")
	if err != nil || order.Status != "PENDING" || order.QRData != "" {
		t.Fatalf("unexpected recovered order: %#v %v", order, err)
	}
}

func TestMapsPaidOrder(t *testing.T) {
	amount := int64(100)
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", ProviderOrderNo: "mall-1", SKU: "sku-100", Status: "PAID", AmountFen: &amount, Currency: "CNY", PayUniqueKey: "trade-1", PayTime: "2026-08-19 12:00:00", StatusOccurredAt: "2026-08-19T04:00:00Z"}}
	order, err := newTestProvider(t, client).Get(context.Background(), "pv-1")
	if err != nil || order.Status != "SUCCESS" || order.ChannelTransactionID == nil || *order.ChannelTransactionID != "trade-1" || order.PaidAt == nil || order.PaidAt.Location() != time.UTC || order.AmountSource != "PROVIDER" || order.ProviderOrderReference != "mall-1" || order.ProviderEventAt == nil {
		t.Fatalf("unexpected paid mapping: %#v %v", order, err)
	}
}

func TestFailedOrderRemainsReviewRequired(t *testing.T) {
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", SKU: "sku-100", Status: "FAILED"}}
	order, err := newTestProvider(t, client).Get(context.Background(), "pv-1")
	if err != nil || order.Status != "REVIEW_REQUIRED" || order.AmountSource != "EXPECTED_SNAPSHOT" {
		t.Fatalf("FAILED must not become an authoritative unpaid terminal: %#v %v", order, err)
	}
}
