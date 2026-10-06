package paymentsim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

type Service struct {
	repository Repository
	client     *http.Client
	now        func() time.Time
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository, client: observability.NewHTTPClient(5 * time.Second), now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Handler(serviceToken string) http.Handler {
	router := httpserver.NewRouter()
	management := router.Group("/internal")
	management.Use(serviceauth.GinMiddleware(serviceToken))
	management.POST("/v1/orders", httpserver.AdaptFunc(s.createOrder))
	management.GET("/v1/orders/:merchant_order_no", httpserver.AdaptFunc(s.getOrder))
	management.POST("/v1/orders/:merchant_order_no/close", httpserver.AdaptFunc(s.closeOrder))
	management.POST("/v1/orders/:merchant_order_no/refunds", httpserver.AdaptFunc(s.refundOrder))
	management.GET("/v1/bills/:business_date", httpserver.AdaptFunc(s.getBill))
	router.GET("/pay/:token", httpserver.AdaptFunc(s.paymentPage))
	router.POST("/pay/:token/complete", httpserver.AdaptFunc(s.completePayment))
	return router
}

func (s *Service) createOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MerchantOrderNo  string `json:"merchant_order_no"`
		Channel          string `json:"channel"`
		AmountFen        int64  `json:"amount_fen"`
		Currency         string `json:"currency"`
		Mobile           string `json:"mobile"`
		ProviderSKU      string `json:"provider_sku"`
		CallbackURL      string `json:"callback_url"`
		Scenario         string `json:"scenario"`
		ExpiresInSeconds int    `json:"expires_in_seconds"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid simulated order", httpserver.RequestID(r.Context()))
		return
	}
	if body.ExpiresInSeconds == 0 {
		body.ExpiresInSeconds = 900
	}
	order, created, err := s.repository.Create(r.Context(), CreateOrder{MerchantOrderNo: body.MerchantOrderNo, Channel: body.Channel, AmountFen: body.AmountFen, Currency: body.Currency, Mobile: body.Mobile, ProviderSKU: body.ProviderSKU, CallbackURL: body.CallbackURL, Scenario: body.Scenario, ExpiresAt: s.now().Add(time.Duration(body.ExpiresInSeconds) * time.Second)})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, order)
}

func (s *Service) getOrder(w http.ResponseWriter, r *http.Request) {
	order, err := s.repository.GetByMerchantOrder(r.Context(), r.PathValue("merchant_order_no"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, order)
}

func (s *Service) closeOrder(w http.ResponseWriter, r *http.Request) {
	order, err := s.repository.CloseOrder(r.Context(), r.PathValue("merchant_order_no"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, order)
}

func (s *Service) refundOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MerchantRefundNo string `json:"merchant_refund_no"`
		AmountFen        int64  `json:"amount_fen"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || strings.TrimSpace(body.MerchantRefundNo) == "" || body.AmountFen <= 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid simulated refund", httpserver.RequestID(r.Context()))
		return
	}
	refund, created, err := s.repository.Refund(r.Context(), r.PathValue("merchant_order_no"), body.MerchantRefundNo, body.AmountFen)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, refund)
}

func (s *Service) getBill(w http.ResponseWriter, r *http.Request) {
	businessDate, err := time.Parse("2006-01-02", r.PathValue("business_date"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid business date", httpserver.RequestID(r.Context()))
		return
	}
	channel := strings.ToUpper(r.URL.Query().Get("channel"))
	items, err := s.repository.ListBill(r.Context(), channel, businessDate)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"business_date": businessDate.Format("2006-01-02"), "channel": channel, "items": items})
}

var paymentTemplate = template.Must(template.New("payment").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Pinvou 模拟支付</title>
<style>body{font-family:system-ui;margin:0;background:#f5f7fb;color:#172033}.card{max-width:420px;margin:8vh auto;background:#fff;padding:28px;border-radius:18px;box-shadow:0 12px 40px #1d29451a}button{width:100%;padding:14px;border:0;border-radius:10px;background:#2457f5;color:#fff;font-weight:700}.meta{color:#667085}.amount{font-size:36px;font-weight:800}</style></head>
<body><main class="card"><h1>{{.Channel}} 模拟支付</h1><p class="amount">¥{{.Amount}}</p><p class="meta">订单 {{.MerchantOrderNo}}</p><p class="meta">场景 {{.Scenario}} · 状态 {{.Status}}</p>{{if .CanPay}}<form method="post" action="/pay/{{.Token}}/complete"><button type="submit">确认模拟付款</button></form>{{else}}<p>此订单当前不可付款。</p>{{end}}</main></body></html>`))

func (s *Service) paymentPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	order, err := s.repository.GetByPaymentToken(r.Context(), token)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = paymentTemplate.Execute(w, map[string]any{"Channel": order.Channel, "Amount": fmt.Sprintf("%d.%02d", order.AmountFen/100, order.AmountFen%100), "MerchantOrderNo": order.MerchantOrderNo, "Scenario": order.Scenario, "Status": order.Status, "Token": token, "CanPay": order.Status == "PENDING"})
}

func (s *Service) completePayment(w http.ResponseWriter, r *http.Request) {
	order, err := s.repository.Complete(r.Context(), r.PathValue("token"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>模拟支付结果</title><p>订单状态：%s</p>", template.HTMLEscapeString(order.Status))
}

func (s *Service) DeliverPending(ctx context.Context) error {
	notifications, err := s.repository.ClaimNotifications(ctx, 20)
	if err != nil {
		return err
	}
	for _, notification := range notifications {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, notification.CallbackURL, bytes.NewReader(notification.Payload))
		if requestErr == nil {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Pinvou-Payment-Channel", notification.Channel)
			request.Header.Set("X-Pinvou-Payment-Signature", notification.Signature)
			response, callErr := s.client.Do(request)
			if callErr != nil {
				requestErr = callErr
			} else {
				response.Body.Close()
				if response.StatusCode < 200 || response.StatusCode >= 300 {
					requestErr = fmt.Errorf("callback returned HTTP %d", response.StatusCode)
				}
			}
		}
		if err := s.repository.MarkNotification(ctx, notification, requestErr); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RunDeliveryWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.DeliverPending(ctx)
		}
	}
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the simulated payment request could not be completed"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "the simulated payment was not found"
	case errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "PAYMENT_CONFLICT", "the simulated payment conflicts with existing state"
	case errors.Is(err, ErrExpired):
		status, code, message = http.StatusGone, "ORDER_EXPIRED", "the simulated payment has expired"
	}
	api.WriteError(w, status, code, strings.TrimSpace(message), httpserver.RequestID(r.Context()))
}
