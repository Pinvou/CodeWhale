package h3cpayment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
)

type mallStub struct {
	created   []string
	order     h3cmall.Order
	createErr error
	queryErr  error
}

func (s *mallStub) CreateOrder(_ context.Context, orderKey, merchantCustomerKey, mobile, sku string) error {
	s.created = []string{orderKey, merchantCustomerKey, mobile, sku}
	return s.createErr
}

func (s *mallStub) QueryOrder(context.Context, string) (h3cmall.Order, error) {
	return s.order, s.queryErr
}

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
	if !capabilities.Valid() || capabilities.ProviderID != "h3c_mall" || capabilities.Version == "" || !capabilities.CommerceCustomerRequired || !capabilities.SupportsChannel("ALIPAY") || len(capabilities.QRScanApps) != 2 || capabilities.QRScanApps[0] != "ALIPAY" || capabilities.QRScanApps[1] != "WECHAT" || !capabilities.NotificationSupported || capabilities.RefundSupported || capabilities.AuthoritativeUnpaidTerminal || capabilities.ReusePendingOrderAfterExpiry || capabilities.TestOnly {
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

func TestCreateRecoveryRejectsUnmatchedIdentityAndKeepsCreateError(t *testing.T) {
	createErr := errors.New("create response lost")
	const customer = "019c0000-0000-7000-8000-000000000001"
	for _, tc := range []struct {
		name, mobile, responseMobile, responseCustomer, responseSKU string
		queryErr                                                    error
	}{
		{name: "different mobile", mobile: "13800138000", responseMobile: "+8613900139000", responseCustomer: customer, responseSKU: "sku-100"},
		{name: "different customer", mobile: "13800138000", responseMobile: "+8613800138000", responseCustomer: "another-customer", responseSKU: "sku-100"},
		{name: "different SKU", mobile: "13800138000", responseMobile: "+8613800138000", responseCustomer: customer, responseSKU: "another-sku"},
		{name: "query failure", mobile: "13800138000", responseMobile: "+8613800138000", responseCustomer: customer, responseSKU: "sku-100", queryErr: errors.New("query unavailable")},
		{name: "invalid input", mobile: "+86 13800138000", responseMobile: "+8613800138000", responseCustomer: customer, responseSKU: "sku-100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mallStub{createErr: createErr, queryErr: tc.queryErr, order: h3cmall.Order{OrderKey: "pv-1", MerchantCustomerKey: tc.responseCustomer, Mobile: tc.responseMobile, SKU: tc.responseSKU, Status: "WAIT_PAY", NeedPayAmount: "1.00"}}
			got, err := newTestProvider(t, client).Create(context.Background(), payment.Order{MerchantOrderNo: "pv-1", MerchantCustomerKey: customer, Mobile: tc.mobile, ExternalSKU: "sku-100", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY"}, "")
			if err != createErr || got.MerchantOrderNo != "" {
				t.Fatalf("identity mismatch must retain original create error and no order: %#v %v", got, err)
			}
		})
	}
}

// This exercises the production SDK against a controlled HTTP peer, not a live Mall.
func TestCreateRecoveryThroughMallSDK(t *testing.T) {
	const customer = "019c0000-0000-7000-8000-000000000001"
	for _, tc := range []struct {
		name, input, responseMobile, responseKey string
		wantRecovery                             bool
	}{
		{"bare", "13800138000", "13800138000", "pv-1", true},
		{"canonical", "+8613800138000", "13800138000", "pv-1", true},
		{"trimmed bare", " \t13800138000\n", "13800138000", "pv-1", true},
		{"trimmed canonical", " \t+8613800138000\n", "13800138000", "pv-1", true},
		{"different mobile", "13800138000", "13900139000", "pv-1", false},
		{"wrong order key", "13800138000", "13800138000", "other-order", false},
		{"invalid response mobile", "13800138000", "+86 13800138000", "pv-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var loginCalls, createCalls, queryCalls int
			mux := http.NewServeMux()
			respond := func(w http.ResponseWriter, content any) {
				t.Helper()
				if err := json.NewEncoder(w).Encode(map[string]any{"code": 200, "success": true, "failed": false, "content": content}); err != nil {
					t.Error(err)
				}
			}
			mux.HandleFunc("/api/solution/jwt/login", func(w http.ResponseWriter, r *http.Request) {
				loginCalls++
				respond(w, map[string]string{"tokenValue": "controlled-token"})
			})
			mux.HandleFunc("/api/trade/external/pinwu/order/createOrder", func(w http.ResponseWriter, r *http.Request) {
				createCalls++
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "controlled-token" || body["orderKey"] != "pv-1" || body["merchantCustomerKey"] != customer || body["mobile"] != "13800138000" || body["skuBom"] != "sku-100" {
					t.Errorf("unexpected SDK create request: %#v", body)
				}
				// Simulate an ambiguous create outcome; query remains the authority for recovery.
				if err := json.NewEncoder(w).Encode(map[string]any{"code": 409, "success": false, "failed": true}); err != nil {
					t.Error(err)
				}
			})
			mux.HandleFunc("/api/trade/external/pinwu/order/queryOrder", func(w http.ResponseWriter, r *http.Request) {
				queryCalls++
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "controlled-token" || body["orderKey"] != "pv-1" {
					t.Errorf("unexpected SDK query request: %#v", body)
				}
				// Raw Mall format is canonicalized by the actual SDK before Provider sees it.
				respond(w, map[string]any{"orderKey": tc.responseKey, "merchantCustomerKey": customer, "mobile": tc.responseMobile, "skuBom": "sku-100", "needPayAmt": "1.00", "status": "WAIT_PAY"})
			})
			peer := httptest.NewServer(mux)
			defer peer.Close()
			client, err := h3cmall.New(h3cmall.Config{BaseURL: peer.URL, AppKey: "controlled", AppSecret: "controlled", AllowInsecure: true})
			if err != nil {
				t.Fatal(err)
			}
			got, err := newTestProvider(t, client).Create(context.Background(), payment.Order{MerchantOrderNo: "pv-1", MerchantCustomerKey: customer, Mobile: tc.input, ExternalSKU: "sku-100", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY"}, "")
			if tc.wantRecovery {
				if err != nil || got.MerchantOrderNo != "pv-1" || got.ExternalCustomerKey != customer || got.ExternalSKU != "sku-100" || got.AmountFen != 100 || got.AmountSource != "PROVIDER" || got.Status != "PENDING" || got.QRData != "" {
					t.Fatalf("SDK recovery: %#v %v", got, err)
				}
			} else if !errors.Is(err, h3cmall.ErrRejected) || err.Error() != "H3C Mall rejected the request: code 409" || got.MerchantOrderNo != "" {
				t.Fatalf("unsafe recovery or replaced original error: %#v %v", got, err)
			}
			if loginCalls != 1 || createCalls != 1 || queryCalls != 1 {
				t.Fatalf("SDK call chain login/create/query = %d/%d/%d", loginCalls, createCalls, queryCalls)
			}
		})
	}
}
