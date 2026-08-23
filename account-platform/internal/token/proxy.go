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
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

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
	catalog, ok := s.repository.(CatalogRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_CATALOG_UNAVAILABLE", "the model catalog is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	catalogModel, err := catalog.ResolveCatalogModel(r.Context(), request.Model, r.Header.Get("X-Pinvou-Conversation-Mode"), time.Now().UTC())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if catalogModel.ProtocolCode != "CHAT" || catalogModel.ProtocolVersion != 1 {
		api.WriteError(w, http.StatusUnprocessableEntity, "MODEL_PROTOCOL_UNSUPPORTED", "the catalog model does not support CHAT v1", httpserver.RequestID(r.Context()))
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
	if maximumOutput < 0 || maximumOutput > s.maximumOutputTokens {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "maximum output tokens are outside the allowed range", httpserver.RequestID(r.Context()))
		return
	}
	maximumUsage, err := EstimateChatUsage(body, maximumOutput)
	if err != nil || maximumUsage.InputTokens > s.maximumInputTokens || catalogModel.ValidateRequestLimits(maximumUsage.InputTokens, maximumOutput) != nil {
		api.WriteError(w, http.StatusUnprocessableEntity, "METERING_LIMIT_EXCEEDED", "the request cannot be measured within the configured model limits", httpserver.RequestID(r.Context()))
		return
	}
	logicalBody := append([]byte(nil), body...)
	var upstreamPayload map[string]any
	if err := json.Unmarshal(logicalBody, &upstreamPayload); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	upstreamPayload["model"] = catalogModel.UpstreamModelRef
	upstreamBody, err := json.Marshal(upstreamPayload)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	candidates, err := s.repository.ListCandidates(r.Context(), catalogModel.UpstreamModelRef, "CHAT")
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
	route, err := SelectRoute(resolved, RouteRequest{Model: catalogModel.UpstreamModelRef, ProtocolCode: "CHAT", ExplicitTokenAccountID: explicit}, candidates)
	if errors.Is(err, ErrNoEligibleRoute) && explicit == nil && s.defaultRoutes != nil {
		walletID, provisionErr := s.activeWallet(r.Context(), resolved)
		if provisionErr == nil {
			provisionErr = s.defaultRoutes.Ensure(r.Context(), resolved, walletID, catalogModel.UpstreamModelRef)
		}
		if provisionErr == nil {
			candidates, provisionErr = s.repository.ListCandidates(r.Context(), catalogModel.UpstreamModelRef, "CHAT")
		}
		if provisionErr == nil {
			route, err = SelectRoute(resolved, RouteRequest{Model: catalogModel.UpstreamModelRef, ProtocolCode: "CHAT"}, candidates)
		} else if !errors.Is(provisionErr, ErrNoEligibleRoute) {
			err = provisionErr
		}
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	price, err := s.billing.Price(r.Context(), catalogModel.ModelID.String())
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
	idempotencyKey := requestIdempotencyKey(r.Header.Get("Idempotency-Key"), resolved, logicalBody)
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain between 16 and 128 characters", httpserver.RequestID(r.Context()))
		return
	}
	usageID := uuid.Must(uuid.NewV7())
	digest := sha256.Sum256(logicalBody)
	modelSnapshot, _ := json.Marshal(catalogModel.Snapshot())
	catalogVersion, routeGeneration := catalogModel.CatalogVersion, route.RouteGeneration
	modelID, capabilityID, protocolID := catalogModel.ModelID, catalogModel.ModelCapabilityVersionID, catalogModel.ProtocolDefinitionID
	usageRequest := UsageRequest{ID: usageID, IdempotencyKey: idempotencyKey, UserID: resolved.UserID, SessionID: resolved.SessionID, OrganizationID: resolved.OrganizationID, TokenAccountID: route.TokenAccountID, CredentialID: route.CredentialID, WalletID: route.WalletID, Model: catalogModel.ModelID.String(), ModelID: &modelID, CatalogVersion: &catalogVersion, CapabilityVersionID: &capabilityID, ProtocolDefinitionID: &protocolID, RouteGeneration: &routeGeneration, ModelSnapshot: modelSnapshot, MaximumMicrocredits: maximumCost}
	pricingSnapshot, _ := json.Marshal(price)
	requestSnapshot := map[string]any{"request_sha256": hex.EncodeToString(digest[:]), "model_id": catalogModel.ModelID, "catalog_version": catalogModel.CatalogVersion, "model_capability_version_id": catalogModel.ModelCapabilityVersionID, "protocol_definition_id": catalogModel.ProtocolDefinitionID, "protocol_code": "CHAT", "protocol_version": catalogModel.ProtocolVersion, "route_generation": route.RouteGeneration, "lifecycle_status": catalogModel.LifecycleStatus, "deprecation_effective_at": catalogModel.DeprecationEffectiveAt, "display_name": catalogModel.DisplayName, "context_length": catalogModel.ContextLength, "stream": request.Stream, "estimated_input_tokens": maximumUsage.InputTokens, "maximum_output_tokens": maximumOutput, "estimator": map[string]any{"code": "UTF8_BYTES_DIV4_CEIL", "version": 1}, "context_type": resolved.ContextType, "organization_id": resolved.OrganizationID, "context_version": resolved.ContextVersion, "authorization_version": resolved.AuthorizationVersion, "membership_version": resolved.MembershipVersion, "wallet_id": route.WalletID, "token_account_id": route.TokenAccountID, "credential_id": route.CredentialID, "formula_version_id": price.FormulaVersionID, "price_version_id": price.ID, "pricing_snapshot": json.RawMessage(pricingSnapshot)}
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
	pricingContext := HoldPricingContext{ModelID: catalogModel.ModelID.String(), ProtocolCode: "CHAT", FormulaVersionID: price.FormulaVersionID, PricingSnapshot: pricingSnapshot, RequestSnapshot: requestSnapshotJSON}
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		organizationBilling, supported := s.billing.(OrganizationBillingClient)
		if !supported {
			s.writeError(w, r, ErrForbidden)
			return
		}
		if snapshotBilling, supported := s.billing.(SnapshotOrganizationBillingClient); supported {
			hold, err = snapshotBilling.ReserveMemberQuotaPriced(r.Context(), usageRequest.ID, route.WalletID, *resolved.OrganizationID, resolved.UserID, resolved.MembershipVersion, maximumCost, price.ID, pricingContext)
		} else {
			hold, err = organizationBilling.ReserveMemberQuota(r.Context(), usageRequest.ID, route.WalletID, *resolved.OrganizationID, resolved.UserID, resolved.MembershipVersion, maximumCost, price.ID)
		}
	} else {
		if snapshotBilling, supported := s.billing.(SnapshotBillingClient); supported {
			hold, err = snapshotBilling.ReservePriced(r.Context(), usageRequest.ID, route.WalletID, maximumCost, price.ID, pricingContext)
		} else {
			hold, err = s.billing.Reserve(r.Context(), usageRequest.ID, route.WalletID, maximumCost, price.ID)
		}
	}
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
	upstreamContext, cancelUpstream := context.WithCancel(r.Context())
	defer cancelUpstream()
	renewalFailed := s.renewHold(upstreamContext, cancelUpstream, usageRequest.ID, hold.ID)
	response, err := s.upstream.Do(upstreamContext, credential.BaseURL, secret, upstreamBody, usageRequest.ID)
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
	if upstreamRequestID := s.upstream.UpstreamRequestID(response); upstreamRequestID != "" {
		_ = s.repository.RecordUpstreamRequest(context.WithoutCancel(r.Context()), usageRequest.ID, UpstreamRequestEvidence{ProviderID: credential.Provider, AdapterVersion: s.upstream.AdapterVersion(), RequestID: upstreamRequestID})
	}
	w.Header().Set("X-Pinvou-Usage-Request-ID", usageRequest.ID.String())
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.handleUpstreamFailure(w, r, usageRequest.ID, hold.ID, response)
		return
	}
	s.upstream.CopyResponseHeaders(w.Header(), response.Header)
	if request.Stream || strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		w.WriteHeader(response.StatusCode)
		s.proxyStream(w, r, usageRequest.ID, hold.ID, price, response.Body, renewalFailed, maximumCost, maximumUsage.OutputTokens)
		return
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if readErr != nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "upstream_body_incomplete", map[string]any{"error_class": "body_read"})
		_, _ = w.Write(payload)
		return
	}
	usage, found := s.upstream.ParseUsage(payload)
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
	s.settle(background, usageRequest.ID, hold.ID, price, usage, maximumCost)
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
	if s.upstream.RejectsBeforeGeneration(response.StatusCode) {
		_ = s.billing.Release(context.WithoutCancel(r.Context()), usageID, holdID, "upstream rejected before generation")
		_ = s.repository.MarkReleased(context.WithoutCancel(r.Context()), usageID, "upstream_rejected")
	} else {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageID, "upstream_error_unknown_usage", map[string]any{"status_code": response.StatusCode})
	}
	s.upstream.CopyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}

func (s *Service) proxyStream(w http.ResponseWriter, r *http.Request, usageID, holdID uuid.UUID, price PriceVersion, body io.Reader, renewalFailed <-chan struct{}, authorizedValues ...int64) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var usage Usage
	usageFound := false
	authorizedMaximum, authorizedOutputTokens := int64(0), int64(0)
	if len(authorizedValues) > 0 {
		authorizedMaximum = authorizedValues[0]
	}
	if len(authorizedValues) > 1 {
		authorizedOutputTokens = authorizedValues[1]
	}
	outputAuthorization, _ := Cost(price, Usage{OutputTokens: authorizedOutputTokens})
	var streamedBytes int64
	for scanner.Scan() {
		line := scanner.Bytes()
		streamedBytes += int64(len(line))
		estimatedOutputTokens := streamedBytes / 4
		if streamedBytes%4 != 0 {
			estimatedOutputTokens++
		}
		if estimatedOutputTokens > authorizedOutputTokens {
			required, _ := Cost(price, Usage{OutputTokens: estimatedOutputTokens})
			if required > outputAuthorization {
				additional := required - outputAuthorization
				client, ok := s.billing.(SnapshotBillingClient)
				background := context.WithoutCancel(r.Context())
				if !ok || client.Extend(background, usageID, holdID, additional) != nil {
					_ = s.repository.MarkReview(background, usageID, "additional_preauthorization_failed", map[string]any{"stream_stopped": true, "additional_microcredits": additional})
					return
				}
				if repository, ok := s.repository.(interface {
					IncreaseMaximum(context.Context, uuid.UUID, int64) error
				}); ok {
					if repository.IncreaseMaximum(background, usageID, additional) != nil {
						_ = s.repository.MarkReview(background, usageID, "local_preauthorization_record_pending", map[string]any{"additional_microcredits": additional})
						return
					}
				}
				authorizedMaximum += additional
				outputAuthorization = required
			}
		}
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !bytes.Equal(payload, []byte("[DONE]")) {
				if parsed, ok := s.upstream.ParseUsage(payload); ok {
					usage, usageFound = parsed, true
				}
			}
		}
	}
	background := context.WithoutCancel(r.Context())
	select {
	case <-renewalFailed:
		_ = s.repository.MarkReview(background, usageID, "hold_renewal_failed", map[string]any{"stream_stopped": true})
		return
	default:
	}
	if scanner.Err() != nil || !usageFound {
		_ = s.repository.MarkReview(background, usageID, "stream_usage_missing", map[string]any{"stream_error": scanner.Err() != nil})
		return
	}
	s.settle(background, usageID, holdID, price, usage, authorizedMaximum)
}

func (s *Service) renewHold(ctx context.Context, cancel context.CancelFunc, usageID, holdID uuid.UUID) <-chan struct{} {
	failed := make(chan struct{})
	client, supported := s.billing.(SnapshotBillingClient)
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
				err := client.Extend(renewalContext, usageID, holdID, 0)
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

func (s *Service) settle(ctx context.Context, usageID, holdID uuid.UUID, price PriceVersion, usage Usage, authorizedMaximum ...int64) {
	actual, err := Cost(price, usage)
	if err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "usage_invalid", map[string]any{"usage": usage})
		return
	}
	maximum := int64(0)
	if len(authorizedMaximum) > 0 {
		maximum = authorizedMaximum[0]
	}
	if _, ok := s.authorizeAdditional(ctx, usageID, holdID, price, usage, maximum); !ok {
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
	client, ok := s.billing.(SnapshotBillingClient)
	if !ok || client.Extend(ctx, usageID, holdID, additional) != nil {
		_ = s.repository.MarkReview(ctx, usageID, "additional_preauthorization_failed", map[string]any{"usage": usage, "required_microcredits": additional})
		return maximum, false
	}
	if repository, ok := s.repository.(interface {
		IncreaseMaximum(context.Context, uuid.UUID, int64) error
	}); ok {
		if err := repository.IncreaseMaximum(ctx, usageID, additional); err != nil {
			_ = s.repository.MarkReview(ctx, usageID, "local_preauthorization_record_pending", map[string]any{"additional_microcredits": additional})
			return maximum, false
		}
	}
	return actual, true
}
