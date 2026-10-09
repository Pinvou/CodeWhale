package h3cmall

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidQRDataOnlyAcceptsPNGData(t *testing.T) {
	png := append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, []byte("test")...)
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if !ValidQRData(valid) {
		t.Fatal("valid PNG data URL was rejected")
	}
	for _, value := range []string{"https://example.com/qr", "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(png), "data:image/png;base64,broken"} {
		if ValidQRData(value) {
			t.Fatalf("invalid QR data accepted: %q", value)
		}
	}
}

func TestDecimalConvertsMallYuanWithoutFloatingPoint(t *testing.T) {
	for value, expected := range map[Decimal]int64{"200": 20000, "200.00": 20000, "0.01": 1} {
		actual, err := value.Fen()
		if err != nil || actual != expected {
			t.Fatalf("Decimal(%q).Fen() = %d, %v; want %d", value, actual, err, expected)
		}
	}
	if _, err := Decimal("200.001").Fen(); err == nil {
		t.Fatal("sub-cent Mall amount was accepted")
	}
}

func TestParseTimeValueAcceptsMallShanghaiLocalTime(t *testing.T) {
	parsed, err := ParseTimeValue("2026-08-27 10:09:43")
	if err != nil {
		t.Fatal(err)
	}
	if expected := "2026-08-27T02:09:43Z"; parsed.Format(time.RFC3339) != expected {
		t.Fatalf("ParseTimeValue() = %s; want %s", parsed.Format(time.RFC3339), expected)
	}
}

func TestCreateAndPageQueryUseMerchantCustomerKey(t *testing.T) {
	const customer = "019c0000-0000-7000-8000-000000000001"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/solution/jwt/login", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "success": true, "failed": false, "content": map[string]string{"tokenValue": "mall-token"}})
	})
	mux.HandleFunc("/api/trade/external/pinwu/order/createOrder", func(w http.ResponseWriter, request *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(request.Body).Decode(&body)
		if request.Header.Get("Authorization") != "mall-token" || body["merchantCustomerKey"] != customer || body["skuBom"] != "0502AJ0Q" {
			t.Errorf("unexpected create request: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "success": true, "failed": false, "content": "accepted"})
	})
	mux.HandleFunc("/api/trade/external/pinwu/order/pageQueryOrder", func(w http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body["merchantCustomerKey"] != customer {
			t.Errorf("unexpected page query: %#v", body)
		}
		order := map[string]any{"orderKey": "pv-1", "merchantCustomerKey": customer, "mobile": "13800138000", "skuBom": "0502AJ0Q", "needPayAmt": 0.10, "skuAmt": 0.10, "status": "WAIT_PAY", "updatedAt": "2026-08-25 16:00:00"}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "success": true, "failed": false, "content": map[string]any{"records": []any{order}, "total": 1, "size": 10, "current": 1, "pages": 1}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, AppKey: "app", AppSecret: "secret", AllowInsecure: true, TokenTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CreateOrder(context.Background(), "pv-1", customer, "+8613800138000", "0502AJ0Q"); err != nil {
		t.Fatal(err)
	}
	page, err := client.PageQueryOrders(context.Background(), PageQueryRequest{MerchantCustomerKey: customer, Current: 1, PageSize: 10})
	if err != nil || len(page.Records) != 1 || page.Records[0].Mobile != "+8613800138000" {
		t.Fatalf("unexpected page: %#v %v", page, err)
	}
}

func TestMallRequestMobileConvertsE164ToMallFormat(t *testing.T) {
	for input, expected := range map[string]string{
		"+8613800138000": "13800138000",
		"13800138000":    "13800138000",
	} {
		actual, err := mallRequestMobile(input)
		if err != nil || actual != expected {
			t.Fatalf("mallRequestMobile(%q) = %q, %v; want %q", input, actual, err, expected)
		}
	}
	for _, input := range []string{"+85213800138000", "1380013800", "1380013800x"} {
		if _, err := mallRequestMobile(input); !errors.Is(err, ErrRejected) {
			t.Fatalf("mallRequestMobile(%q) error = %v; want ErrRejected", input, err)
		}
	}
}

func TestCanonicalMainlandMobileUsesE164(t *testing.T) {
	actual, err := canonicalMainlandMobile("13800138000")
	if err != nil || actual != "+8613800138000" {
		t.Fatalf("canonicalMainlandMobile() = %q, %v", actual, err)
	}
}

func TestMallAuthorityTransportRequiresCompleteBoundedJSON(t *testing.T) {
	for _, response := range []string{
		`{"code":200,"success":true,"content":{"tokenValue":"controlled"}} {}`,
		`{"code":200,"success":true,"content":{"tokenValue":"controlled"}} garbage`,
		`{"code":200,"success":true,"content":{"tokenValue":"controlled"}}` + strings.Repeat(" ", maxResponseBytes),
	} {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) }))
		client, err := New(Config{BaseURL: peer.URL, AppKey: "controlled", AppSecret: "controlled", AllowInsecure: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.accessToken(context.Background(), false)
		peer.Close()
		if !errors.Is(err, ErrResponseInvalid) {
			t.Fatal("partial or oversized Mall authority accepted")
		}
	}
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer target.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer peer.Close()
	client, err := New(Config{BaseURL: peer.URL, AppKey: "controlled", AppSecret: "controlled", AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.accessToken(context.Background(), false); err == nil || calls.Load() != 0 {
		t.Fatal("redirected Mall credentials")
	}
	peer.Close()
	if _, err := client.accessToken(context.Background(), false); err != ErrUnavailable {
		t.Fatal("transport error did not remain a static unavailable category")
	}
}
