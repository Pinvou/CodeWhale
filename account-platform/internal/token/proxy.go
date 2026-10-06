package token

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	return &HTTPUpstreamClient{client: &http.Client{Transport: observability.NewHTTPTransport(transport), CheckRedirect: rejectProviderRedirect}}
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
	// Model generation and SSE can outlive the server's ordinary response deadline.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		api.WriteError(w, http.StatusServiceUnavailable, "STREAM_UNAVAILABLE", "the model response could not be started", httpserver.RequestID(r.Context()))
		return
	}
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must contain 16 to 128 UTF-8 characters", httpserver.RequestID(r.Context()))
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
	maximumOutput := request.MaxCompletionTokens
	if maximumOutput == 0 {
		maximumOutput = request.MaxTokens
	}
	if maximumOutput < 0 || maximumOutput > s.maximumOutputTokens {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "maximum output tokens are outside the allowed range", httpserver.RequestID(r.Context()))
		return
	}
	maximumUsage, err := EstimateChatUsage(body, maximumOutput)
	if err != nil || maximumUsage.InputTokens > s.maximumInputTokens {
		api.WriteError(w, http.StatusUnprocessableEntity, "METERING_LIMIT_EXCEEDED", "the request cannot be measured within the configured model limits", httpserver.RequestID(r.Context()))
		return
	}
	candidates, err := s.repository.ListCandidates(r.Context(), request.Model)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	authorized := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if Eligible(resolved, candidate, request.Model) && s.unitOwnerActive(r, candidate.OwnerType, candidate.OwnerOrganization, candidate.OwnerUnit) {
			authorized = append(authorized, candidate)
		}
	}
	candidates = authorized
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
	if errors.Is(err, ErrNoEligibleRoute) && explicit == nil && s.defaultRoutes != nil {
		walletID, provisionErr := s.activeWallet(r.Context(), resolved)
		if provisionErr == nil {
			provisionErr = s.defaultRoutes.Ensure(r.Context(), resolved, walletID, request.Model)
		}
		if provisionErr == nil {
			candidates, provisionErr = s.repository.ListCandidates(r.Context(), request.Model)
		}
		if provisionErr == nil {
			route, err = SelectRoute(resolved, RouteRequest{Model: request.Model}, candidates)
		} else if !errors.Is(provisionErr, ErrNoEligibleRoute) {
			err = provisionErr
		}
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body, err = boundedRequestBody(body, request.Stream, false, maximumOutput)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid model request options", httpserver.RequestID(r.Context()))
		return
	}
	if !s.unitOwnerActive(r, route.OwnerType, route.OwnerOrganization, route.OwnerUnit) {
		s.writeError(w, r, ErrForbidden)
		return
	}
	price, err := s.billing.Price(r.Context(), request.Model)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "the model price is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := ValidatePriceFormula(price); err != nil {
		api.WriteError(w, http.StatusUnprocessableEntity, "FORMULA_INVALID", "the published metering formula is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	maximumCost, err := Cost(price, maximumUsage)
	if err != nil {
		api.WriteError(w, http.StatusUnprocessableEntity, "PRICE_INVALID", "the model request cannot be priced", httpserver.RequestID(r.Context()))
		return
	}
	if resolved.ContextType == "ORGANIZATION" {
		if resolved.OrganizationID == nil || resolved.MemberVersion < 1 || s.wallets == nil {
			s.writeError(w, r, ErrForbidden)
			return
		}
		walletOwnerType := ""
		switch route.OwnerType {
		case "ORG":
			if route.OwnerID == *resolved.OrganizationID {
				walletOwnerType = "ORGANIZATION"
			}
		case "ORG_UNIT":
			if route.OwnerUnit != nil && route.OwnerID == *route.OwnerUnit {
				walletOwnerType = "ORG_UNIT"
			}
		}
		if walletOwnerType == "" {
			s.writeError(w, r, ErrForbidden)
			return
		}
		expectedWallet, err := s.wallets.Ensure(r.Context(), walletOwnerType, route.OwnerID)
		if err != nil || expectedWallet != route.WalletID {
			s.writeError(w, r, ErrForbidden)
			return
		}
	}
	usageID := uuid.Must(uuid.NewV7())
	digest := sha256.Sum256(body)
	usageRequest := UsageRequest{ID: usageID, IdempotencyKey: idempotencyKey, UserID: resolved.UserID, SessionID: resolved.SessionID, OrganizationID: resolved.OrganizationID, TokenAccountID: route.TokenAccountID, CredentialID: route.CredentialID, WalletID: route.WalletID, Model: request.Model, MaximumMicrocredits: maximumCost}
	pricingSnapshot, _ := json.Marshal(price)
	requestSnapshot := map[string]any{"request_sha256": hex.EncodeToString(digest[:]), "model": request.Model, "protocol_code": "CHAT", "usage_normalization_version": 2, "stream": request.Stream, "estimated_input_tokens": maximumUsage.InputTokens, "maximum_output_tokens": maximumOutput, "estimator": map[string]any{"code": "UTF8_BYTES_DIV4_CEIL", "version": 1}, "context_type": resolved.ContextType, "organization_id": resolved.OrganizationID, "context_version": resolved.ContextVersion, "authorization_version": resolved.AuthorizationVersion, "membership_version": resolved.MembershipVersion, "member_version": resolved.MemberVersion, "wallet_id": route.WalletID, "token_account_id": route.TokenAccountID, "credential_id": route.CredentialID, "formula_version_id": price.FormulaVersionID, "price_version_id": price.ID, "pricing_snapshot": json.RawMessage(pricingSnapshot)}
	requestSnapshotJSON, _ := json.Marshal(requestSnapshot)
	usageRequest, created, err := s.repository.BeginUsage(r.Context(), usageRequest, requestSnapshot)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !created {
		api.WriteError(w, http.StatusConflict, "USAGE_REPLAY_BLOCKED", "this model request already exists and will not be sent twice", httpserver.RequestID(r.Context()))
		return
	}
	var hold BillingHold
	pricingContext := HoldPricingContext{ModelID: request.Model, ProtocolCode: "CHAT", FormulaVersionID: price.FormulaVersionID, PricingSnapshot: pricingSnapshot, RequestSnapshot: requestSnapshotJSON}
	if route.OwnerType == "ORG" && resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		organizationBilling, supported := s.billing.(OrganizationBillingClient)
		if !supported {
			s.writeError(w, r, ErrForbidden)
			return
		}
		if snapshotBilling, supported := s.billing.(SnapshotOrganizationBillingClient); supported {
			hold, err = snapshotBilling.ReserveMemberQuotaPriced(r.Context(), usageRequest.ID, route.WalletID, *resolved.OrganizationID, resolved.UserID, resolved.MemberVersion, maximumCost, price.ID, pricingContext)
		} else {
			hold, err = organizationBilling.ReserveMemberQuota(r.Context(), usageRequest.ID, route.WalletID, *resolved.OrganizationID, resolved.UserID, resolved.MemberVersion, maximumCost, price.ID)
		}
	} else {
		if snapshotBilling, supported := s.billing.(SnapshotBillingClient); supported {
			hold, err = snapshotBilling.ReservePriced(r.Context(), usageRequest.ID, route.WalletID, maximumCost, price.ID, pricingContext)
		} else {
			hold, err = s.billing.Reserve(r.Context(), usageRequest.ID, route.WalletID, maximumCost, price.ID)
		}
	}
	if err != nil {
		// A timeout can occur after Billing durably reserved the wallet.
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "reserve_result_unknown", map[string]any{"upstream_started": false})
		api.WriteError(w, http.StatusPaymentRequired, "WALLET_RESERVE_FAILED", "the wallet could not reserve enough credits", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.repository.MarkReserved(r.Context(), usageRequest.ID, hold.ID, price.ID); err != nil {
		s.releaseUnused(r.Context(), usageRequest.ID, hold.ID, "usage state persistence failed")
		s.writeError(w, r, err)
		return
	}
	credential, secret, err := s.repository.CredentialSecret(r.Context(), route.TokenAccountID, route.CredentialID)
	if err != nil {
		s.releaseUnused(r.Context(), usageRequest.ID, hold.ID, "credential_unavailable")
		s.writeError(w, r, err)
		return
	}
	defer clear(secret)
	if err := s.providerURLs.Validate(credential.BaseURL); err != nil {
		s.releaseUnused(r.Context(), usageRequest.ID, hold.ID, "provider_url_forbidden")
		s.writeError(w, r, ErrForbidden)
		return
	}
	if err := s.repository.MarkUpstream(r.Context(), usageRequest.ID); err != nil {
		s.releaseUnused(r.Context(), usageRequest.ID, hold.ID, "upstream_not_started")
		s.writeError(w, r, err)
		return
	}
	upstreamContext, cancelUpstream := context.WithCancel(r.Context())
	defer cancelUpstream()
	renewalFailed := s.renewHold(upstreamContext, cancelUpstream, usageRequest.ID, hold.ID)
	response, err := s.upstream.Do(upstreamContext, credential.BaseURL, secret, body, usageRequest.ID)
	if err != nil {
		select {
		case <-renewalFailed:
			_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "hold_renewal_failed", map[string]any{"upstream_cancelled": true})
			api.WriteError(w, http.StatusServiceUnavailable, "HOLD_RENEWAL_FAILED", "the usage authorization could not be renewed", httpserver.RequestID(r.Context()))
			return
		default:
		}
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "upstream_transport_unknown", map[string]any{"error_class": "transport"})
		api.WriteError(w, http.StatusBadGateway, "UPSTREAM_RESULT_UNKNOWN", "the upstream result is unknown and will be reconciled", httpserver.RequestID(r.Context()))
		return
	}
	defer response.Body.Close()
	if newAPIRequestID := response.Header.Get("X-Oneapi-Request-Id"); newAPIRequestID != "" {
		_ = s.repository.RecordUpstreamRequest(context.WithoutCancel(r.Context()), usageRequest.ID, newAPIRequestID)
	}
	w.Header().Set("X-Pinvou-Usage-Request-ID", usageRequest.ID.String())
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.handleUpstreamFailure(w, r, usageRequest.ID, hold.ID, response)
		return
	}
	copySafeHeaders(w.Header(), response.Header)
	if request.Stream || strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		w.WriteHeader(response.StatusCode)
		s.proxyStream(w, r, usageRequest.ID, hold.ID, price, usageRequest.MaximumMicrocredits, response.Body, streamAuthorization{renewalFailed: renewalFailed, maximumOutput: maximumUsage.OutputTokens})
		return
	}
	const maximumResponseBytes = 32 << 20
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if len(payload) > maximumResponseBytes {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "upstream_body_too_large", map[string]any{"response_received": true})
		_, _ = w.Write(payload[:maximumResponseBytes])
		return
	}
	if readErr != nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "upstream_body_incomplete", map[string]any{"error_class": "body_read"})
		_, _ = w.Write(payload)
		return
	}
	usage, found := parseUsageJSON(payload)
	if !found {
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(payload)
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "usage_missing", map[string]any{"response_received": true})
		return
	}
	background := context.WithoutCancel(r.Context())
	maximumCost, ok = s.authorizeAdditional(background, usageRequest.ID, hold.ID, price, usage, maximumCost)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADDITIONAL_PREAUTHORIZATION_FAILED", "the final response could not be authorized", httpserver.RequestID(r.Context()))
		return
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
	s.settle(background, usageRequest.ID, hold.ID, maximumCost, price, usage)
}

func (s *Service) handleUpstreamFailure(w http.ResponseWriter, r *http.Request, usageID, holdID uuid.UUID, response *http.Response) {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if response.StatusCode == 400 || response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 || response.StatusCode == 422 || response.StatusCode == 429 {
		s.releaseUnused(r.Context(), usageID, holdID, "upstream_rejected")
	} else {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageID, "upstream_error_unknown_usage", map[string]any{"status_code": response.StatusCode})
	}
	copySafeHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}

type streamAuthorization struct {
	renewalFailed <-chan struct{}
	maximumOutput int64
}

func (s *Service) proxyStream(w http.ResponseWriter, r *http.Request, usageID, holdID uuid.UUID, price PriceVersion, maximumMicrocredits int64, body io.Reader, authorization ...streamAuthorization) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var usage Usage
	usageFound := false
	doneFound, donePending, streamFailed := false, false, false
	authorizedMaximum, authorizedOutputTokens := maximumMicrocredits, int64(0)
	var renewalFailed <-chan struct{}
	if len(authorization) > 0 {
		authorizedOutputTokens = authorization[0].maximumOutput
		renewalFailed = authorization[0].renewalFailed
	}
	outputAuthorization, costErr := Cost(price, Usage{OutputTokens: authorizedOutputTokens})
	if costErr != nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageID, "usage_invalid", map[string]any{"stream_stopped": true})
		return
	}
	var streamedBytes int64
	for scanner.Scan() {
		line := scanner.Bytes()
		streamedBytes += int64(len(line))
		estimatedOutputTokens := streamedBytes / 4
		if streamedBytes%4 != 0 {
			estimatedOutputTokens++
		}
		if len(authorization) > 0 && estimatedOutputTokens > authorizedOutputTokens {
			required, err := Cost(price, Usage{OutputTokens: estimatedOutputTokens})
			if err != nil {
				_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageID, "usage_invalid", map[string]any{"stream_stopped": true})
				return
			}
			if required > outputAuthorization {
				additional := required - outputAuthorization
				background, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
				if err := s.extendAuthorization(background, usageID, holdID, authorizedMaximum, additional); err != nil {
					reason := "additional_preauthorization_failed"
					if errors.Is(err, errLocalAuthorizationRecord) {
						reason = "local_preauthorization_record_pending"
					}
					_ = s.repository.MarkReview(background, usageID, reason, map[string]any{"stream_stopped": true, "additional_microcredits": additional})
					stop()
					return
				}
				stop()
				authorizedMaximum += additional
				outputAuthorization = required
			}
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				donePending = true
			} else if len(payload) != 0 {
				var event map[string]json.RawMessage
				if donePending || json.Unmarshal(payload, &event) != nil || len(event["error"]) != 0 && string(event["error"]) != "null" {
					streamFailed = true
				}
				parsed, validUsage := parseUsageJSON(payload)
				if len(event["usage"]) != 0 && !bytes.Equal(bytes.TrimSpace(event["usage"]), []byte("null")) && !validUsage {
					streamFailed = true
				}
				if validUsage {
					// Once the Provider reports actual input as well as output,
					// authorize its full cost before forwarding the usage frame.
					// Earlier content cannot be recalled; a failed extension must
					// not expose this completion evidence or the following DONE.
					if len(authorization) > 0 && !streamFailed {
						background, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
						var authorized bool
						authorizedMaximum, authorized = s.authorizeAdditional(background, usageID, holdID, price, parsed, authorizedMaximum)
						stop()
						if !authorized {
							return
						}
					}
					usage, usageFound = parsed, true
				}
			}
		}
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// An SSE event ends at its empty line. A complete DONE event ends the
		// model stream, independently of whether the HTTP transport reaches EOF.
		if len(line) == 0 && donePending {
			doneFound = true
			if closer, ok := body.(io.Closer); ok {
				_ = closer.Close()
			}
			break
		}
	}
	background := context.WithoutCancel(r.Context())
	select {
	case <-renewalFailed:
		_ = s.repository.MarkReview(background, usageID, "hold_renewal_failed", map[string]any{"stream_stopped": true})
		return
	default:
	}
	if scanner.Err() != nil || !usageFound || !doneFound || streamFailed {
		_ = s.repository.MarkReview(background, usageID, "stream_usage_missing", map[string]any{"stream_error": scanner.Err() != nil || streamFailed, "done_received": doneFound})
		return
	}
	s.settle(background, usageID, holdID, authorizedMaximum, price, usage)
}

func (s *Service) renewHold(ctx context.Context, cancel context.CancelFunc, usageID, holdID uuid.UUID) <-chan struct{} {
	failed := make(chan struct{})
	_, supported := s.billing.(SnapshotBillingClient)
	if !supported {
		return failed
	}
	go func() {
		ticker := time.NewTicker(4 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewalContext, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				err := s.renewAuthorization(renewalContext, usageID, holdID)
				stop()
				if err != nil {
					close(failed)
					cancel()
					return
				}
			}
		}
	}()
	return failed
}

func (s *Service) settle(ctx context.Context, usageID, holdID uuid.UUID, maximumMicrocredits int64, price PriceVersion, usage Usage) {
	actual, err := Cost(price, usage)
	if err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "usage_invalid", map[string]any{"usage": usage})
		return
	}
	if actual > maximumMicrocredits {
		var ok bool
		maximumMicrocredits, ok = s.authorizeAdditional(ctx, usageID, holdID, price, usage, maximumMicrocredits)
		if !ok {
			return
		}
	}
	stored, storedErr := s.repository.GetUsage(ctx, usageID)
	if storedErr != nil {
		_ = s.repository.MarkReview(ctx, usageID, "settlement_pending", map[string]any{"usage": usage, "actual_microcredits": actual})
		return
	}
	if err := s.billing.Commit(ctx, usageID, holdID, stored.WalletID, actual, usage); err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "settlement_pending", map[string]any{"usage": usage, "actual_microcredits": actual})
		return
	}
	if err := s.repository.MarkCommitted(ctx, usageID, actual, map[string]any{"usage": usage}); err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "local_commit_record_pending", map[string]any{"usage": usage, "actual_microcredits": actual})
	}
}

func (s *Service) authorizeAdditional(ctx context.Context, usageID, holdID uuid.UUID, price PriceVersion, usage Usage, maximum int64) (int64, bool) {
	actual, err := Cost(price, usage)
	if err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "usage_invalid", map[string]any{"usage": usage})
		return maximum, false
	}
	if actual <= maximum {
		return maximum, true
	}
	additional := actual - maximum
	if err := s.extendAuthorization(ctx, usageID, holdID, maximum, additional); err != nil {
		reason := "additional_preauthorization_failed"
		if errors.Is(err, errLocalAuthorizationRecord) {
			reason = "local_preauthorization_record_pending"
		}
		_ = s.repository.MarkReview(ctx, usageID, reason, map[string]any{"usage": usage, "required_microcredits": additional})
		return maximum, false
	}
	return actual, true
}

func parseUsageJSON(payload []byte) (Usage, bool) {
	var body struct {
		Usage *struct {
			PromptTokens       *int64 `json:"prompt_tokens"`
			CompletionTokens   *int64 `json:"completion_tokens"`
			PromptTokenDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &body) != nil || body.Usage == nil || body.Usage.PromptTokens == nil || body.Usage.CompletionTokens == nil {
		return Usage{}, false
	}
	usage := Usage{InputTokens: *body.Usage.PromptTokens, OutputTokens: *body.Usage.CompletionTokens}
	if body.Usage.PromptTokenDetails != nil {
		usage.CachedInputTokens = body.Usage.PromptTokenDetails.CachedTokens
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens {
		return Usage{}, false
	}
	usage.InputTokens -= usage.CachedInputTokens
	return usage, true
}

func (s *Service) releaseUnused(ctx context.Context, usageID, holdID uuid.UUID, reason string) {
	background := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(background, 15*time.Second)
	defer cancel()
	stored, storedErr := s.repository.GetUsage(ctx, usageID)
	var releaseErr error
	if storedErr != nil {
		releaseErr = storedErr
	} else {
		releaseErr = s.billing.Release(ctx, usageID, holdID, stored.WalletID, reason)
	}
	if releaseErr != nil {
		recordCtx, recordCancel := context.WithTimeout(background, 5*time.Second)
		defer recordCancel()
		_ = s.repository.MarkReview(recordCtx, usageID, "release_pending", map[string]any{"hold_id": holdID, "release_reason": reason, "upstream_started": reason == "upstream_rejected"})
		return
	}
	if err := s.repository.MarkReleased(ctx, usageID, reason); err != nil {
		recordCtx, recordCancel := context.WithTimeout(background, 5*time.Second)
		defer recordCancel()
		_ = s.repository.MarkReview(recordCtx, usageID, "local_release_record_pending", map[string]any{"hold_id": holdID, "billing_released": true, "release_reason": reason})
	}
}

func rejectProviderRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func boundedRequestBody(body []byte, stream, useDefault bool, maximumOutput int64) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if useDefault {
		payload["max_tokens"], _ = json.Marshal(maximumOutput)
	}
	if stream {
		options := map[string]json.RawMessage{}
		if value := payload["stream_options"]; len(value) != 0 && string(value) != "null" {
			if err := json.Unmarshal(value, &options); err != nil {
				return nil, err
			}
		}
		options["include_usage"] = json.RawMessage("true")
		payload["stream_options"], _ = json.Marshal(options)
	}
	return json.Marshal(payload)
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
