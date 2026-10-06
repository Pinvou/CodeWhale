package paymentsimulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
)

type Provider struct {
	base   *url.URL
	token  string
	client *http.Client
}

func New(raw, token string) (*Provider, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("invalid development payment provider configuration")
	}
	client := observability.NewHTTPClient(5 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Provider{base: u, token: token, client: client}, nil
}

func (p *Provider) Capabilities() payment.Capabilities {
	return payment.Capabilities{
		ProviderID: "development_simulator", Version: "development-simulator-v1", Channels: []string{"UNIONPAY", "ALIPAY", "WECHAT"},
		OrderQuerySupported: true, OrderHistoryRetentionSeconds: 90 * 24 * 60 * 60, MinimumQueryIntervalSeconds: 1,
		MaximumPayableWindowSeconds: 15 * 60, MaximumLateConfirmationSeconds: 60,
		AuthoritativeUnpaidTerminal: true, RefundSupported: true, RefundIdempotencySupported: true, CloseSupported: true,
		NotificationSupported: true, BillSupported: true, ManualRepairSupported: true, TestOnly: true,
	}
}

func (p *Provider) Create(ctx context.Context, order payment.Order, callback string) (payment.ProviderOrder, error) {
	var result payment.ProviderOrder
	body, _ := json.Marshal(map[string]any{"merchant_order_no": order.MerchantOrderNo, "channel": order.Channel, "amount_fen": order.AmountFen, "currency": order.Currency, "mobile": order.Mobile, "provider_sku": order.ExternalSKU, "callback_url": callback, "scenario": "SUCCESS", "expires_in_seconds": 900})
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/orders", body, &result, http.StatusCreated, http.StatusOK)
	return result, err
}

func (p *Provider) Get(ctx context.Context, merchantOrderNo string) (payment.ProviderOrder, error) {
	var result payment.ProviderOrder
	err := p.doJSON(ctx, http.MethodGet, "/internal/v1/orders/"+url.PathEscape(merchantOrderNo), nil, &result)
	return result, err
}

func (p *Provider) Close(ctx context.Context, merchantOrderNo string) (payment.ProviderOrder, error) {
	var result payment.ProviderOrder
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/orders/"+url.PathEscape(merchantOrderNo)+"/close", []byte(`{}`), &result)
	return result, err
}

func (p *Provider) Refund(ctx context.Context, merchantOrderNo, merchantRefundNo string, amountFen int64) (payment.Refund, error) {
	var result payment.Refund
	body, _ := json.Marshal(map[string]any{"merchant_refund_no": merchantRefundNo, "amount_fen": amountFen})
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/orders/"+url.PathEscape(merchantOrderNo)+"/refunds", body, &result, http.StatusCreated, http.StatusOK)
	return result, err
}

func (p *Provider) Bill(ctx context.Context, channel string, businessDate time.Time) ([]payment.BillEntry, error) {
	var result struct {
		Items []payment.BillEntry `json:"items"`
	}
	path := "/internal/v1/bills/" + businessDate.UTC().Format("2006-01-02") + "?channel=" + url.QueryEscape(strings.ToUpper(channel))
	err := p.doJSON(ctx, http.MethodGet, path, nil, &result)
	return result.Items, err
}

func (p *Provider) doJSON(ctx context.Context, method, path string, encoded []byte, output any, statuses ...int) error {
	var body io.Reader
	if encoded != nil {
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base.String()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	accepted := resp.StatusCode == http.StatusOK
	if len(statuses) > 0 {
		accepted = false
		for _, status := range statuses {
			accepted = accepted || resp.StatusCode == status
		}
	}
	if !accepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("development payment provider HTTP %d", resp.StatusCode)
	}
	if output == nil {
		return errors.New("development payment provider response destination is required")
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(payload) > 2<<20 {
		return errors.New("development payment provider response exceeds the allowed size")
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return errors.New("development payment provider response must be a JSON object")
	}
	return json.Unmarshal(payload, output)
}
