package billing

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
)

type h3cMallStub struct {
	created []string
	order   h3cmall.Order
}

func (s *h3cMallStub) CreateOrder(_ context.Context, orderKey, mobile, sku string) error {
	s.created = []string{orderKey, mobile, sku}
	return nil
}

func (s *h3cMallStub) QueryOrder(context.Context, string) (h3cmall.Order, error) {
	return s.order, nil
}

func TestH3CMallProviderCreatesWithoutInventingQR(t *testing.T) {
	client := &h3cMallStub{}
	provider, err := NewH3CMallProvider(client, map[string]ExternalProduct{"points-100": {SKU: "sku-100", AmountFen: 100}})
	if err != nil {
		t.Fatal(err)
	}
	order, err := provider.Create(context.Background(), PaymentOrder{MerchantOrderNo: "pv-1", Mobile: "+8613800138000", ProviderSKU: "sku-100", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.created) != 3 || order.QRData != "" || order.Status != "PENDING" {
		t.Fatalf("unexpected create result: %#v %#v", client.created, order)
	}
}

func TestH3CMallProviderMapsPaidOrder(t *testing.T) {
	client := &h3cMallStub{order: h3cmall.Order{OrderKey: "pv-1", SKU: "sku-100", Status: "PAID", PayUniqueKey: "trade-1", PayTime: "2026-08-19 12:00:00"}}
	provider, _ := NewH3CMallProvider(client, map[string]ExternalProduct{"points-100": {SKU: "sku-100", AmountFen: 100}})
	order, err := provider.Get(context.Background(), "pv-1")
	if err != nil {
		t.Fatal(err)
	}
	if order.Status != "SUCCESS" || order.ChannelTransactionID == nil || *order.ChannelTransactionID != "trade-1" || order.PaidAt == nil || order.PaidAt.Location() != time.UTC {
		t.Fatalf("unexpected paid mapping: %#v", order)
	}
}
