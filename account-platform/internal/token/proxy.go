package token

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type UpstreamClient interface {
	Do(context.Context, string, []byte, []byte, uuid.UUID) (*http.Response, error)
}

type HTTPUpstreamClient struct{ client *http.Client }

func NewHTTPUpstreamClient() *HTTPUpstreamClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &HTTPUpstreamClient{client: &http.Client{Transport: observability.NewHTTPTransport(transport)}}
}

func (c *HTTPUpstreamClient) Do(ctx context.Context, baseURL string, secret, body []byte, usageID uuid.UUID) (*http.Response, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, ErrNoEligibleRoute
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("X-Request-ID", usageID.String())
	return c.client.Do(request)
}

func (s *Service) chatCompletions(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		api.WriteError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "the model request is too large", httpserver.RequestID(r.Context()))
		return
	}
	var request struct {
		Model               string `json:"model"`
		Stream              bool   `json:"stream"`
		MaxTokens           int64  `json:"max_tokens"`
		MaxCompletionTokens int64  `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	if request.Stream {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid streaming request", httpserver.RequestID(r.Context()))
			return
		}
		streamOptions, _ := payload["stream_options"].(map[string]any)
		if streamOptions == nil {
			streamOptions = map[string]any{}
			payload["stream_options"] = streamOptions
		}
		streamOptions["include_usage"] = true
		body, err = json.Marshal(payload)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid streaming request", httpserver.RequestID(r.Context()))
			return
		}
	}
	maximumOutput := request.MaxCompletionTokens
	if maximumOutput == 0 {
		maximumOutput = request.MaxTokens
	}
	if maximumOutput == 0 {
		maximumOutput = s.defaultMaxOutputTokens
	}
	if maximumOutput < 1 || maximumOutput > s.maximumOutputTokens {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "maximum output tokens are outside the allowed range", httpserver.RequestID(r.Context()))
		return
	}
	candidates, err := s.repository.ListCandidates(r.Context(), request.Model)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var explicit *uuid.UUID
	if raw := strings.TrimSpace(r.Header.Get("X-Pinvou-Token-Account-ID")); raw != "" {
		parsed, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid explicit token account", httpserver.RequestID(r.Context()))
			return
		}
		explicit = &parsed
	}
	route, err := SelectRoute(resolved, RouteRequest{Model: request.Model, ExplicitTokenAccountID: explicit}, candidates)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	price, err := s.billing.Price(r.Context(), request.Model)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "the model price is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	maximumCost, err := Cost(price, MaximumUsage(s.maximumInputTokens, maximumOutput))
	if err != nil || maximumCost <= 0 {
		api.WriteError(w, http.StatusUnprocessableEntity, "PRICE_INVALID", "the model request cannot be priced", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := requestIdempotencyKey(r.Header.Get("Idempotency-Key"), resolved, body)
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain between 16 and 128 characters", httpserver.RequestID(r.Context()))
		return
	}
	usageID := uuid.Must(uuid.NewV7())
	digest := sha256.Sum256(body)
	usageRequest := UsageRequest{ID: usageID, IdempotencyKey: idempotencyKey, UserID: resolved.UserID, SessionID: resolved.SessionID, OrganizationID: resolved.OrganizationID, TokenAccountID: route.TokenAccountID, CredentialID: route.CredentialID, WalletID: route.WalletID, Model: request.Model, MaximumMicrocredits: maximumCost}
	usageRequest, created, err := s.repository.BeginUsage(r.Context(), usageRequest, map[string]any{"request_sha256": hex.EncodeToString(digest[:]), "model": request.Model, "stream": request.Stream, "maximum_output_tokens": maximumOutput})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !created {
		api.WriteError(w, http.StatusConflict, "USAGE_REPLAY_BLOCKED", "this model request already exists and will not be sent twice", httpserver.RequestID(r.Context()))
		return
	}
	hold, err := s.billing.Reserve(r.Context(), usageRequest.ID, route.WalletID, maximumCost, price.ID)
	if err != nil {
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "reserve_failed")
		api.WriteError(w, http.StatusPaymentRequired, "WALLET_RESERVE_FAILED", "the wallet could not reserve enough credits", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.repository.MarkReserved(r.Context(), usageRequest.ID, hold.ID, price.ID); err != nil {
		_ = s.billing.Release(r.Context(), usageRequest.ID, hold.ID, "usage state persistence failed")
		s.writeError(w, r, err)
		return
	}
	credential, secret, err := s.repository.CredentialSecret(r.Context(), route.TokenAccountID, route.CredentialID)
	if err != nil {
		_ = s.billing.Release(r.Context(), usageRequest.ID, hold.ID, "credential unavailable")
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "credential_unavailable")
		s.writeError(w, r, err)
		return
	}
	defer clear(secret)
	if err := s.providerURLs.Validate(credential.BaseURL); err != nil {
		_ = s.billing.Release(r.Context(), usageRequest.ID, hold.ID, "provider URL is not allowed")
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "provider_url_forbidden")
		s.writeError(w, r, ErrForbidden)
		return
	}
	if err := s.repository.MarkUpstream(r.Context(), usageRequest.ID); err != nil {
		_ = s.billing.Release(r.Context(), usageRequest.ID, hold.ID, "upstream not started")
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "upstream_not_started")
		s.writeError(w, r, err)
		return
	}
	response, err := s.upstream.Do(r.Context(), credential.BaseURL, secret, body, usageRequest.ID)
	if err != nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "upstream_transport_unknown", map[string]any{"error_class": "transport"})
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_RESULT_UNKNOWN", "the upstream result is unknown and will be reconciled", httpserver.RequestID(r.Context()))
		return
	}
	defer response.Body.Close()
	w.Header().Set("X-Pinvou-Usage-Request-ID", usageRequest.ID.String())
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.handleUpstreamFailure(w, r, usageRequest.ID, hold.ID, response)
		return
	}
	copySafeHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if request.Stream || strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		s.proxyStream(w, r, usageRequest.ID, hold.ID, price, response.Body)
		return
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if readErr != nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "upstream_body_incomplete", map[string]any{"error_class": "body_read"})
		_, _ = w.Write(payload)
		return
	}
	_, _ = w.Write(payload)
	usage, found := parseUsageJSON(payload)
	if !found {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "usage_missing", map[string]any{"response_received": true})
		return
	}
	s.settle(context.WithoutCancel(r.Context()), usageRequest.ID, hold.ID, price, usage)
}

func requestIdempotencyKey(provided string, resolved AuthorizationContext, body []byte) string {
	if provided = strings.TrimSpace(provided); provided != "" {
		return provided
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("pinvou-chat-v1\x00"))
	_, _ = digest.Write([]byte(resolved.UserID.String()))
	_, _ = digest.Write([]byte{'\x00'})
	_, _ = digest.Write([]byte(resolved.SessionID))
	_, _ = digest.Write([]byte{'\x00'})
	_, _ = digest.Write(body)
	return "desktop-" + hex.EncodeToString(digest.Sum(nil))
}

func (s *Service) handleUpstreamFailure(w http.ResponseWriter, r *http.Request, usageID, holdID uuid.UUID, response *http.Response) {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if response.StatusCode == 400 || response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 || response.StatusCode == 422 || response.StatusCode == 429 {
		_ = s.billing.Release(context.WithoutCancel(r.Context()), usageID, holdID, "upstream rejected before generation")
		_ = s.repository.MarkReleased(context.WithoutCancel(r.Context()), usageID, "upstream_rejected")
	} else {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageID, "upstream_error_unknown_usage", map[string]any{"status_code": response.StatusCode})
	}
	copySafeHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}

func (s *Service) proxyStream(w http.ResponseWriter, r *http.Request, usageID, holdID uuid.UUID, price PriceVersion, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var usage Usage
	usageFound := false
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !bytes.Equal(payload, []byte("[DONE]")) {
				if parsed, ok := parseUsageJSON(payload); ok {
					usage, usageFound = parsed, true
				}
			}
		}
	}
	background := context.WithoutCancel(r.Context())
	if scanner.Err() != nil || !usageFound {
		_ = s.repository.MarkReview(background, usageID, "stream_usage_missing", map[string]any{"stream_error": scanner.Err() != nil})
		return
	}
	s.settle(background, usageID, holdID, price, usage)
}

func (s *Service) settle(ctx context.Context, usageID, holdID uuid.UUID, price PriceVersion, usage Usage) {
	actual, err := Cost(price, usage)
	if err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "usage_invalid", map[string]any{"usage": usage})
		return
	}
	if err := s.billing.Commit(ctx, usageID, holdID, actual, usage); err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "settlement_pending", map[string]any{"usage": usage, "actual_microcredits": actual})
		return
	}
	if err := s.repository.MarkCommitted(ctx, usageID, actual, map[string]any{"usage": usage}); err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "local_commit_record_pending", map[string]any{"usage": usage, "actual_microcredits": actual})
	}
}

func parseUsageJSON(payload []byte) (Usage, bool) {
	var body struct {
		Usage *struct {
			PromptTokens       int64 `json:"prompt_tokens"`
			CompletionTokens   int64 `json:"completion_tokens"`
			PromptTokenDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &body) != nil || body.Usage == nil {
		return Usage{}, false
	}
	usage := Usage{InputTokens: body.Usage.PromptTokens, OutputTokens: body.Usage.CompletionTokens}
	if body.Usage.PromptTokenDetails != nil {
		usage.CachedInputTokens = body.Usage.PromptTokenDetails.CachedTokens
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens {
		return Usage{}, false
	}
	return usage, true
}

func copySafeHeaders(destination, source http.Header) {
	for key, values := range source {
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "set-cookie", "content-length":
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}
