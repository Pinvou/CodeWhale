package paymentsimulator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
)

func TestProviderRejectsAmbiguousURLs(t *testing.T) {
	for _, raw := range []string{"file://example", "ftp://example", "http://user:secret@example", "http://example?token=x", "http://example?", "http://example#fragment", "http:///path"} {
		if _, err := New(raw, "controlled-token"); err == nil {
			t.Fatal("accepted an ambiguous provider URL")
		}
	}
}

func TestProviderRequiresExactSuccessAndFullBoundedJSON(t *testing.T) {
	methods := []struct {
		name   string
		status int
		call   func(*Provider) error
	}{
		{"create", 201, func(p *Provider) error {
			_, err := p.Create(context.Background(), payment.Order{MerchantOrderNo: "owned-order", Channel: "ALIPAY", AmountFen: 1, Currency: "CNY"}, "")
			return err
		}},
		{"get", 200, func(p *Provider) error { _, err := p.Get(context.Background(), "owned-order"); return err }},
		{"close", 200, func(p *Provider) error { _, err := p.Close(context.Background(), "owned-order"); return err }},
		{"refund", 201, func(p *Provider) error {
			_, err := p.Refund(context.Background(), "owned-order", "owned-refund", 1)
			return err
		}},
		{"bill", 200, func(p *Provider) error { _, err := p.Bill(context.Background(), "ALIPAY", time.Now()); return err }},
	}
	for _, method := range methods {
		for _, test := range []struct {
			name, body string
			status     int
			valid      bool
		}{
			{"complete", `{"merchant_order_no":"owned-order","items":[]}`, method.status, true},
			{"unexpected-status", `{}`, 202, false}, {"no-content", "", 204, false},
			{"null", "null", method.status, false}, {"array", "[]", method.status, false},
			{"html", "<html></html>", method.status, false}, {"truncated", `{"items":`, method.status, false},
			{"trailing", `{} {}`, method.status, false}, {"oversized", `{}` + strings.Repeat(" ", 2<<20), method.status, false},
		} {
			t.Run(method.name+"/"+test.name, func(t *testing.T) {
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer controlled-token" {
						t.Error("missing service authentication")
					}
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.body))
				}))
				defer peer.Close()
				provider, err := New(peer.URL, "controlled-token")
				if err != nil {
					t.Fatal(err)
				}
				if err := method.call(provider); (err == nil) != test.valid {
					t.Fatalf("accepted=%v want=%v", err == nil, test.valid)
				}
			})
		}
		t.Run(method.name+"/redirect", func(t *testing.T) {
			var dispatched atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { dispatched.Add(1); _, _ = w.Write([]byte(`{}`)) }))
			defer target.Close()
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			}))
			defer peer.Close()
			provider, err := New(peer.URL, "controlled-token")
			if err != nil {
				t.Fatal(err)
			}
			if method.call(provider) == nil || dispatched.Load() != 0 {
				t.Fatal("followed provider redirect")
			}
		})
	}
}

func TestProviderReloadsProjectedWorkloadCredential(t *testing.T) {
	file := filepath.Join(t.TempDir(), "projected-token")
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		want := "Bearer controlled-first"
		if attempt == 2 {
			want = "Bearer controlled-renewed"
		}
		if r.Header.Get("Authorization") != want {
			t.Error("provider did not read the current projected credential")
		}
		_, _ = w.Write([]byte(`{"merchant_order_no":"controlled"}`))
	}))
	defer peer.Close()
	provider, err := New(peer.URL, "file:"+file)
	if err != nil {
		t.Fatal("provider initialization failed")
	}
	for _, value := range []string{"controlled-first", "controlled-renewed"} {
		if err := os.WriteFile(file, []byte(value), 0600); err != nil {
			t.Fatal("fixture credential write failed")
		}
		if _, err := provider.Get(context.Background(), "controlled"); err != nil {
			t.Fatal("projected credential request failed")
		}
	}
	if err := os.WriteFile(file, []byte("controlled\ninvalid"), 0600); err != nil {
		t.Fatal("fixture credential write failed")
	}
	_, err = provider.Get(context.Background(), "controlled")
	if err == nil || err.Error() != "development payment provider credential is unavailable" || requests.Load() != 2 {
		t.Fatal("invalid credential dispatched or returned unsafe error detail")
	}
}
