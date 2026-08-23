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
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || token == "" {
		return nil, errors.New("invalid development payment provider configuration")
	}
	return &Provider{base: u, token: token, client: observability.NewHTTPClient(5 * time.Second)}, nil
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
	body, _ := json.Marshal(map[string]any{"merchant_order_no": order.MerchantOrderNo, "channel": order.Channel, "amount_fen": order.AmountFen, "currency": order.Currency, "callback_url": callback, "scenario": "SUCCESS", "expires_in_seconds": 900})
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/orders", body, &result)
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
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/orders/"+url.PathEscape(merchantOrderNo)+"/refunds", body, &result)
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

func (p *Provider) doJSON(ctx context.Context, method, path string, encoded []byte, output any) error {
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("development payment provider HTTP %d", resp.StatusCode)
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(output)
}
