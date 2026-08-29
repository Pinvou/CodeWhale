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

func (s *mallStub) CreateOrder(_ context.Context, orderKey, merchantCustomerKey, mobile, sku string) error {
	s.created = []string{orderKey, merchantCustomerKey, mobile, sku}
	return s.createErr
}

func (s *mallStub) QueryOrder(context.Context, string) (h3cmall.Order, error) { return s.order, nil }

func newTestProvider(t *testing.T, client mallClient) *Provider {
	t.Helper()
	provider, err := New(client, func(context.Context, string) (Product, error) {
		return Product{SKU: "sku-100", AmountFen: 100}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestCapabilitiesAreVersionedAndSupplierSpecific(t *testing.T) {
	capabilities := newTestProvider(t, &mallStub{}).Capabilities()
	if !capabilities.Valid() || capabilities.ProviderID != "h3c_mall" || capabilities.Version == "" || !capabilities.CommerceCustomerRequired || !capabilities.SupportsChannel("ALIPAY") || !capabilities.NotificationSupported || capabilities.RefundSupported || capabilities.AuthoritativeUnpaidTerminal || capabilities.ReusePendingOrderAfterExpiry || capabilities.TestOnly {
		t.Fatalf("unexpected capabilities: %#v", capabilities)
	}
}

func TestCreateRecoversAcceptedOrderWithoutInventingQR(t *testing.T) {
	customer := "019c0000-0000-7000-8000-000000000001"
	client := &mallStub{createErr: errors.New("acceptance response lost"), order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: customer, Mobile: "+8613800138000", SKU: "sku-100", NeedPayAmount: "1.00", Status: "WAIT_PAY"}}
	provider := newTestProvider(t, client)
	order, err := provider.Create(context.Background(), payment.Order{MerchantOrderNo: "pv-1", MerchantCustomerKey: customer, Mobile: "+8613800138000", ExternalSKU: "sku-100", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY"}, "")
	if err != nil || order.Status != "PENDING" || order.QRData != "" {
		t.Fatalf("unexpected recovered order: %#v %v", order, err)
	}
	if len(client.created) != 4 || client.created[1] != customer {
		t.Fatalf("merchant customer key was not forwarded: %#v", client.created)
	}
}

func TestMapsPaidOrder(t *testing.T) {
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: "019c0000-0000-7000-8000-000000000001", TradeOrderCode: "mall-1", SKU: "sku-100", Status: "PAID", NeedPayAmount: "1.00", PaidAmount: "1.00", SKUAmount: "1.00", PaymentStatus: "PAY_SUCCESS", PayUniqueKey: "trade-1", PayTime: "2026-08-19 12:00:00", UpdatedAt: "2026-08-19 12:00:01"}}
	order, err := newTestProvider(t, client).Get(context.Background(), "pv-1")
	if err != nil || order.Status != "SUCCESS" || order.ChannelTransactionID == nil || *order.ChannelTransactionID != "trade-1" || order.PaidAt == nil || order.PaidAt.Location() != time.UTC || order.AmountSource != "PROVIDER" || order.ProviderOrderReference != "mall-1" || order.ProviderEventAt == nil || order.ExternalSKU != "sku-100" {
		t.Fatalf("unexpected paid mapping: %#v %v", order, err)
	}
}

func TestMapsMallPaymentDeadline(t *testing.T) {
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: "019c0000-0000-7000-8000-000000000001", SKU: "sku-100", Status: "WAIT_PAY", NeedPayAmount: "1.00", PayEndTime: "2026-08-29 18:15:00"}}
	order, err := newTestProvider(t, client).Get(context.Background(), "pv-1")
	want := time.Date(2026, 8, 29, 10, 15, 0, 0, time.UTC)
	if err != nil || order.ProviderPayableUntil == nil || !order.ProviderPayableUntil.Equal(want) || order.PaymentInstructionExpiresAt == nil || !order.PaymentInstructionExpiresAt.Equal(want) {
		t.Fatalf("unexpected payment deadline mapping: %#v %v", order, err)
	}
}

func TestEmptyMallPaymentDeadlineRemainsOptional(t *testing.T) {
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: "019c0000-0000-7000-8000-000000000001", SKU: "sku-100", Status: "WAIT_PAY", NeedPayAmount: "1.00", PayEndTime: "  "}}
	order, err := newTestProvider(t, client).Get(context.Background(), "pv-1")
	if err != nil || order.ProviderPayableUntil != nil || order.PaymentInstructionExpiresAt != nil {
		t.Fatalf("empty optional payment deadline must be ignored: %#v %v", order, err)
	}
}

func TestProductResolutionIsDynamic(t *testing.T) {
	amount := int64(100)
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: "019c0000-0000-7000-8000-000000000001", SKU: "sku", Status: "FAILED"}}
	provider, err := New(client, func(context.Context, string) (Product, error) { return Product{SKU: "sku", AmountFen: amount}, nil })
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.Get(context.Background(), "pv-1")
	if err != nil || first.AmountFen != 100 {
		t.Fatalf("first dynamic resolution failed: %#v %v", first, err)
	}
	amount = 200
	second, err := provider.Get(context.Background(), "pv-1")
	if err != nil || second.AmountFen != 200 {
		t.Fatalf("updated mapping required a provider restart: %#v %v", second, err)
	}
}

func TestFailedOrderRemainsReviewRequired(t *testing.T) {
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: "019c0000-0000-7000-8000-000000000001", SKU: "sku-100", Status: "FAILED"}}
	order, err := newTestProvider(t, client).Get(context.Background(), "pv-1")
	if err != nil || order.Status != "REVIEW_REQUIRED" || order.AmountSource != "EXPECTED_SNAPSHOT" {
		t.Fatalf("FAILED must not become an authoritative unpaid terminal: %#v %v", order, err)
	}
}

func TestUnknownOrderStatusRemainsReviewRequired(t *testing.T) {
	client := &mallStub{order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: "019c0000-0000-7000-8000-000000000001", SKU: "sku-100", Status: "NEW_MALL_STATE"}}
	order, err := newTestProvider(t, client).Get(context.Background(), "pv-1")
	if err != nil || order.Status != "REVIEW_REQUIRED" || order.AmountSource != "EXPECTED_SNAPSHOT" {
		t.Fatalf("unknown Mall status must fail closed for review: %#v %v", order, err)
	}
}

func TestPaidOrderRejectsInconsistentMallAmounts(t *testing.T) {
	order := h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: "019c0000-0000-7000-8000-000000000001", SKU: "sku-100", Status: "PAID", NeedPayAmount: "1.00", PaidAmount: "2.00", SKUAmount: "1.00", PayUniqueKey: "trade-1", PayTime: "2026-08-19 12:00:00"}
	if _, err := MapMallOrder(order, nil); err == nil {
		t.Fatal("inconsistent Mall amounts were accepted")
	}
}
