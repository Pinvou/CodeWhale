package h3cmall

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

var (
	ErrUnavailable     = errors.New("H3C Mall is unavailable")
	ErrRejected        = errors.New("H3C Mall rejected the request")
	ErrResponseInvalid = errors.New("H3C Mall returned an invalid response")
)

const maxResponseBytes = 2 << 20

type Config struct {
	BaseURL       string
	AppKey        string
	AppSecret     string
	AllowInsecure bool
	TokenTTL      time.Duration
}

type Client struct {
	baseURL   *url.URL
	appKey    string
	appSecret string
	tokenTTL  time.Duration
	http      *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

type envelope[T any] struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Success   bool   `json:"success"`
	Failed    bool   `json:"failed"`
	Content   T      `json:"content"`
	TraceID   string `json:"traceId"`
	Timestamp any    `json:"timeStamp"`
}

type userSyncResult struct {
	SuccessList []userSyncItem `json:"successList"`
	FailedList  []userSyncItem `json:"failedList"`
}

type userSyncItem struct {
	Mobile  string `json:"mobile"`
	Message string `json:"message"`
}

func New(config Config) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("H3C_MALL_BASE_URL must be an absolute URL without credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && !(config.AllowInsecure && parsed.Scheme == "http") {
		return nil, errors.New("H3C_MALL_BASE_URL must use HTTPS")
	}
	if strings.TrimSpace(config.AppKey) == "" || strings.TrimSpace(config.AppSecret) == "" {
		return nil, errors.New("H3C Mall application credentials are required")
	}
	ttl := config.TokenTTL
	if ttl <= 0 || ttl > time.Hour {
		ttl = time.Hour
	}
	client := observability.NewHTTPClient(15 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		baseURL: parsed,
		appKey:  strings.TrimSpace(config.AppKey), appSecret: strings.TrimSpace(config.AppSecret),
		tokenTTL: ttl, http: client,
	}, nil
}

func (c *Client) CreateUser(ctx context.Context, mobile string) error {
	requestMobile, err := mallRequestMobile(mobile)
	if err != nil {
		return err
	}
	var result userSyncResult
	if err := c.authorizedJSON(ctx, "/api/middle/external/pinwu/user/batch-create", map[string]any{"mobiles": []string{requestMobile}}, &result); err != nil {
		return err
	}
	for _, item := range result.SuccessList {
		if normalized, normalizeErr := mallRequestMobile(item.Mobile); normalizeErr == nil && normalized == requestMobile {
			return nil
		}
	}
	for _, item := range result.FailedList {
		normalized, normalizeErr := mallRequestMobile(item.Mobile)
		if normalizeErr == nil && normalized == requestMobile && strings.Contains(item.Message, "已存在") {
			// The current Mall contract has no stable ALREADY_EXISTS code. Keep this
			// compatibility branch isolated here so it can be removed once supplied.
			return nil
		}
	}
	return fmt.Errorf("%w: user synchronization did not confirm the requested mobile", ErrRejected)
}

func (c *Client) EnsureCustomer(ctx context.Context, request commerce.CustomerProvisionRequest) (commerce.CustomerProvisionResult, error) {
	if request.SubjectType != "USER" {
		return commerce.CustomerProvisionResult{}, fmt.Errorf("%w: unsupported customer subject", ErrRejected)
	}
	if err := c.CreateUser(ctx, request.ContactMobile); err != nil {
		return commerce.CustomerProvisionResult{}, err
	}
	mobile, err := mallRequestMobile(request.ContactMobile)
	if err != nil {
		return commerce.CustomerProvisionResult{}, err
	}
	return commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***" + mobile[len(mobile)-4:]}, nil
}

func (c *Client) CreateOrder(ctx context.Context, orderKey, merchantCustomerKey, mobile, sku string) error {
	requestMobile, err := mallRequestMobile(mobile)
	if err != nil {
		return err
	}
	merchantCustomerKey = strings.TrimSpace(merchantCustomerKey)
	if merchantCustomerKey == "" {
		return fmt.Errorf("%w: merchant customer key is required", ErrRejected)
	}
	var accepted string
	return c.authorizedJSON(ctx, "/api/trade/external/pinwu/order/createOrder", map[string]string{
		"orderKey": orderKey, "merchantCustomerKey": merchantCustomerKey, "mobile": requestMobile, "skuBom": sku,
	}, &accepted)
}

func (c *Client) QueryOrder(ctx context.Context, orderKey string) (Order, error) {
	var order Order
	err := c.authorizedJSON(ctx, "/api/trade/external/pinwu/order/queryOrder", map[string]string{"orderKey": orderKey}, &order)
	if err != nil {
		return Order{}, err
	}
	if order.OrderKey != orderKey || strings.TrimSpace(order.Status) == "" {
		return Order{}, ErrResponseInvalid
	}
	if err := normalizeOrderResponse(&order); err != nil {
		return Order{}, ErrResponseInvalid
	}
	return order, nil
}

func (c *Client) PageQueryOrders(ctx context.Context, request PageQueryRequest) (OrderPage, error) {
	request.MerchantCustomerKey = strings.TrimSpace(request.MerchantCustomerKey)
	if request.MerchantCustomerKey == "" || request.Current < 0 || request.PageSize < 0 || request.PageSize > 100 {
		return OrderPage{}, fmt.Errorf("%w: invalid order page query", ErrRejected)
	}
	input := map[string]any{"merchantCustomerKey": request.MerchantCustomerKey}
	if mobile := strings.TrimSpace(request.Mobile); mobile != "" {
		requestMobile, err := mallRequestMobile(mobile)
		if err != nil {
			return OrderPage{}, err
		}
		input["mobile"] = requestMobile
	}
	if value := strings.TrimSpace(request.OrderKey); value != "" {
		input["orderKey"] = value
	}
	if value := strings.TrimSpace(request.SKU); value != "" {
		input["skuBom"] = value
	}
	if value := strings.ToUpper(strings.TrimSpace(request.Status)); value != "" {
		input["status"] = value
	}
	if request.Current > 0 {
		input["current"] = request.Current
	}
	if request.PageSize > 0 {
		input["pageSize"] = request.PageSize
	}
	var page OrderPage
	if err := c.authorizedJSON(ctx, "/api/trade/external/pinwu/order/pageQueryOrder", input, &page); err != nil {
		return OrderPage{}, err
	}
	for index := range page.Records {
		if strings.TrimSpace(page.Records[index].MerchantCustomerKey) != request.MerchantCustomerKey || normalizeOrderResponse(&page.Records[index]) != nil {
			return OrderPage{}, ErrResponseInvalid
		}
	}
	return page, nil
}

func normalizeOrderResponse(order *Order) error {
	order.OrderKey = strings.TrimSpace(order.OrderKey)
	order.MerchantCustomerKey = strings.TrimSpace(order.MerchantCustomerKey)
	order.SKU = strings.TrimSpace(order.SKU)
	order.Status = strings.ToUpper(strings.TrimSpace(order.Status))
	if strings.TrimSpace(order.Mobile) != "" {
		mobile, err := canonicalMainlandMobile(order.Mobile)
		if err != nil {
			return err
		}
		order.Mobile = mobile
	}
	if order.AliQRData != "" && !ValidQRData(order.AliQRData) {
		return ErrResponseInvalid
	}
	return nil
}

func mallRequestMobile(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "+86") {
		value = strings.TrimPrefix(value, "+86")
	}
	if len(value) != 11 || value[0] != '1' {
		return "", fmt.Errorf("%w: invalid mainland mobile", ErrRejected)
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return "", fmt.Errorf("%w: invalid mainland mobile", ErrRejected)
		}
	}
	return value, nil
}

func canonicalMainlandMobile(value string) (string, error) {
	mobile, err := mallRequestMobile(value)
	if err != nil {
		return "", err
	}
	return "+86" + mobile, nil
}

func ValidQRData(value string) bool {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(value, prefix) || len(value) > 1024*1024 {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) >= 8 && len(decoded) <= 768*1024 && bytes.Equal(decoded[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10})
}

func (c *Client) authorizedJSON(ctx context.Context, path string, input, output any) error {
	token, err := c.accessToken(ctx, false)
	if err != nil {
		return err
	}
	status, err := c.postJSON(ctx, path, token, input, output)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		token, tokenErr := c.accessToken(ctx, true)
		if tokenErr != nil {
			return tokenErr
		}
		status, err = c.postJSON(ctx, path, token, input, output)
	}
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("%w: HTTP %d", ErrUnavailable, status)
	}
	return nil
}

func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Since(c.tokenAt) < c.tokenTTL {
		return c.token, nil
	}
	var content struct {
		TokenValue string `json:"tokenValue"`
	}
	status, err := c.postJSON(ctx, "/api/solution/jwt/login", "", map[string]string{"appKey": c.appKey, "appSecret": c.appSecret}, &content)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 || strings.TrimSpace(content.TokenValue) == "" {
		return "", ErrUnavailable
	}
	c.token, c.tokenAt = content.TokenValue, time.Now()
	return c.token, nil
}

func (c *Client) postJSON(ctx context.Context, path, token string, input, output any) (int, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	if token != "" {
		request.Header.Set("Authorization", token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return response.StatusCode, nil
	}
	var body envelope[json.RawMessage]
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil || len(data) > maxResponseBytes || json.Unmarshal(data, &body) != nil {
		return response.StatusCode, ErrResponseInvalid
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, nil
	}
	if !body.Success || body.Failed || body.Code != http.StatusOK {
		return response.StatusCode, fmt.Errorf("%w: code %d", ErrRejected, body.Code)
	}
	if output != nil && len(body.Content) > 0 && string(body.Content) != "null" {
		if err := json.Unmarshal(body.Content, output); err != nil {
			return response.StatusCode, ErrResponseInvalid
		}
	}
	return response.StatusCode, nil
}
