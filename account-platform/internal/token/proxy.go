package token

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

const (
	logicalTurnHeader        = "X-Pinvou-Logical-Turn-ID"
	clientCapabilitiesHeader = "X-Pinvou-Client-Capabilities"
	billingNoticeCapability  = "billing-notice-v1"
	billingNoticeEvent       = "BILLING_NOTICE"
)

type billingNotice struct {
	SchemaVersion    int        `json:"schema_version"`
	Code             string     `json:"code"`
	Stage            string     `json:"stage"`
	TurnOutcome      string     `json:"turn_outcome"`
	SettlementStatus string     `json:"settlement_status"`
	Action           string     `json:"action"`
	ContextType      string     `json:"context_type"`
	OrganizationID   *uuid.UUID `json:"organization_id,omitempty"`
	UsageRequestID   uuid.UUID  `json:"usage_request_id"`
}

type billingNoticeContext struct {
	ContextType                         string
	OrganizationID                      *uuid.UUID
	CanPurchaseOrganizationEntitlements bool
}

type chatCompletionRequest struct {
	Model               string `json:"model"`
	Stream              bool   `json:"stream"`
	MaxTokens           int64  `json:"max_tokens"`
	MaxCompletionTokens int64  `json:"max_completion_tokens"`
}

func newWalletInsufficientNotice(stage, outcome, settlement, contextType string, organizationID *uuid.UUID, canPurchaseOrganizationEntitlements bool, usageID uuid.UUID) billingNotice {
	contextType = strings.ToUpper(strings.TrimSpace(contextType))
	action := ""
	switch contextType {
	case "PERSONAL":
		action = "OPEN_RECHARGE"
	case "ORGANIZATION":
		if organizationID != nil && canPurchaseOrganizationEntitlements {
			action = "OPEN_ORGANIZATION_RECHARGE"
		} else {
			action = "CONTACT_ORG_ADMIN"
		}
	}
	return billingNotice{SchemaVersion: 1, Code: "WALLET_INSUFFICIENT", Stage: stage, TurnOutcome: outcome, SettlementStatus: settlement, Action: action, ContextType: contextType, OrganizationID: organizationID, UsageRequestID: usageID}
}

func supportsBillingNotice(r *http.Request) bool {
	for _, capability := range strings.Split(r.Header.Get(clientCapabilitiesHeader), ",") {
		if strings.EqualFold(strings.TrimSpace(capability), billingNoticeCapability) {
			return true
		}
	}
	return false
}

func (s *Service) chatCompletions(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveModelUsagePermission(w, r, "personal.model.invoke", "org.model.invoke")
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		api.WriteError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "the model request is too large", httpserver.RequestID(r.Context()))
		return
	}
	var request chatCompletionRequest
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain between 16 and 128 characters", httpserver.RequestID(r.Context()))
		return
	}
	logicalTurnID, err := normalizeLogicalTurnID(r.Header.Get(logicalTurnHeader))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "LOGICAL_TURN_ID_INVALID", logicalTurnHeader+" must be 16 to 128 URL-safe characters", httpserver.RequestID(r.Context()))
		return
	}
	body, err = normalizeInferenceBody(body, request.Stream)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	// Canonical v1 fingerprints the caller's normalized request. Operational
	// defaults are applied only after the fingerprint is frozen so a rolling
	// gateway/client upgrade cannot turn a retry into IDEMPOTENCY_KEY_REUSED.
	logicalBody := append([]byte(nil), body...)
	body, err = applyDefaultChatMaximumOutput(body, &request, s.defaultMaxOutputTokens)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	keyDigest := digestString([]byte(idempotencyKey))
	requestFingerprint := inferenceFingerprint("CHAT", 1, logicalBody)
	if existing, lookupErr := s.repository.FindUsageByIdempotency(r.Context(), resolved.UserID, keyDigest); lookupErr == nil {
		if existing.CanonicalVersion != 1 || existing.RequestFingerprint != requestFingerprint {
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different model request", httpserver.RequestID(r.Context()))
			return
		}
		s.replayUsage(w, r, existing, logicalBody)
		return
	} else if !errors.Is(lookupErr, ErrNotFound) {
		s.writeError(w, r, lookupErr)
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
	if validator, supported := s.defaultRoutes.(interface {
		ValidateContextModelPolicy(context.Context, AuthorizationContext, uuid.UUID) error
	}); supported {
		if err := validator.ValidateContextModelPolicy(r.Context(), resolved, catalogModel.ModelID); err != nil {
			s.writeError(w, r, err)
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
	var upstreamPayload map[string]any
	if err := json.Unmarshal(body, &upstreamPayload); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	upstreamPayload["model"] = catalogModel.UpstreamModelRef
	upstreamBody, err := json.Marshal(upstreamPayload)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid chat completion request", httpserver.RequestID(r.Context()))
		return
	}
	if s.defaultRoutes == nil {
		s.writeError(w, r, ErrRouteRepairRequired)
		return
	}
	walletID, walletErr := s.activeWallet(r.Context(), resolved)
	if walletErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the active wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	provisioning, provisioningErr := s.defaultRoutes.Ensure(r.Context(), resolved, walletID, catalogModel)
	if provisioningErr != nil {
		s.writeError(w, r, provisioningErr)
		return
	}
	if provisioningErr := provisioningError(provisioning); provisioningErr != nil {
		s.writeError(w, r, provisioningErr)
		return
	}
	candidates, err := s.repository.ListCandidates(r.Context(), catalogModel.ModelID.String(), catalogModel.ProtocolDefinitionID.String())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	route, err := SelectRoute(resolved, RouteRequest{Model: catalogModel.UpstreamModelRef, ProtocolCode: catalogModel.ProtocolCode}, candidates)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	price, err := s.billing.Price(r.Context(), catalogModel.ModelID.String())
	if err != nil {
		s.writePricingError(w, r, err)
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
	usageID := uuid.Must(uuid.NewV7())
	digest := sha256.Sum256(logicalBody)
	modelSnapshot, _ := json.Marshal(catalogModel.Snapshot())
	catalogVersion, routeGeneration := catalogModel.CatalogVersion, route.RouteGeneration
	modelID, capabilityID, protocolID := catalogModel.ModelID, catalogModel.ModelCapabilityVersionID, catalogModel.ProtocolDefinitionID
	usageRequest := UsageRequest{ID: usageID, IdempotencyKeyDigest: keyDigest, IdempotencyNamespace: "model-inference", CanonicalVersion: 1, RequestFingerprint: requestFingerprint, UserID: resolved.UserID, SessionID: resolved.SessionID, OrganizationID: resolved.OrganizationID, TokenAccountID: route.TokenAccountID, CredentialID: route.CredentialID, WalletID: route.WalletID, Model: catalogModel.ModelID.String(), ModelID: &modelID, CatalogVersion: &catalogVersion, CapabilityVersionID: &capabilityID, ProtocolDefinitionID: &protocolID, RouteGeneration: &routeGeneration, ModelSnapshot: modelSnapshot, MaximumMicrocredits: maximumCost}
	pricingSnapshot, _ := json.Marshal(price)
	requestSnapshot := map[string]any{"request_sha256": hex.EncodeToString(digest[:]), "model_id": catalogModel.ModelID, "upstream_model_ref": catalogModel.UpstreamModelRef, "catalog_version": catalogModel.CatalogVersion, "model_capability_version_id": catalogModel.ModelCapabilityVersionID, "protocol_definition_id": catalogModel.ProtocolDefinitionID, "protocol_code": "CHAT", "protocol_version": catalogModel.ProtocolVersion, "route_generation": route.RouteGeneration, "lifecycle_status": catalogModel.LifecycleStatus, "deprecation_effective_at": catalogModel.DeprecationEffectiveAt, "display_name": catalogModel.DisplayName, "context_length": catalogModel.ContextLength, "stream": request.Stream, "estimated_input_tokens": maximumUsage.InputTokens, "maximum_output_tokens": maximumOutput, "estimator": map[string]any{"code": "UTF8_BYTES_DIV4_CEIL", "version": 1}, "context_type": resolved.ContextType, "organization_id": resolved.OrganizationID, "can_purchase_organization_entitlements": slices.Contains(resolved.Permissions, "org.entitlements.purchase"), "context_version": resolved.ContextVersion, "authorization_version": resolved.AuthorizationVersion, "membership_version": resolved.MembershipVersion, "wallet_id": route.WalletID, "token_account_id": route.TokenAccountID, "credential_id": route.CredentialID, "formula_version_id": price.FormulaVersionID, "price_version_id": price.ID, "pricing_snapshot": json.RawMessage(pricingSnapshot)}
	if logicalTurnID != "" {
		requestSnapshot["logical_turn_id"] = logicalTurnID
	}
	usageRequest, created, err := s.repository.BeginUsage(r.Context(), usageRequest, requestSnapshot)
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used for a different model request", httpserver.RequestID(r.Context()))
			return
		}
		s.writeError(w, r, err)
		return
	}
	if !created {
		s.replayUsage(w, r, usageRequest, logicalBody)
		return
	}
	credential, secret, err := s.repository.CredentialSecret(r.Context(), route.TokenAccountID, route.CredentialID)
	if err != nil {
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "credential_unavailable")
		s.writeError(w, r, err)
		return
	}
	defer clear(secret)
	if err := s.providerURLs.Validate(credential.BaseURL); err != nil {
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "provider_url_forbidden")
		s.writeError(w, r, ErrForbidden)
		return
	}
	envelope := DispatchEnvelope{BaseURL: credential.BaseURL, Provider: credential.Provider, Credential: append([]byte(nil), secret...), RequestBody: upstreamBody, Price: price, Stream: request.Stream, MaximumCost: maximumCost, MaximumOutput: maximumUsage.OutputTokens, ContextType: resolved.ContextType, OrganizationID: resolved.OrganizationID, CanPurchaseOrganizationEntitlements: slices.Contains(resolved.Permissions, "org.entitlements.purchase")}
	circuitKey := resilience.Key(envelope.Provider, "MODEL_DISPATCH", envelope.BaseURL)
	if circuitErr := s.breaker.BeforeHighRisk(circuitKey); circuitErr != nil {
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "provider_circuit_open")
		var open *resilience.CircuitOpenError
		if errors.As(circuitErr, &open) {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(open.RetryAfter.Seconds()+0.999))))
		}
		api.WriteError(w, http.StatusServiceUnavailable, "PROVIDER_CIRCUIT_OPEN", "the selected provider route is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.repository.PrepareDispatch(r.Context(), usageRequest.ID, envelope); err != nil {
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "dispatch_preparation_failed")
		s.writeError(w, r, err)
		return
	}
	if err := s.repository.MarkHoldReserving(r.Context(), usageRequest.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	hold, err := s.ensureUsageHold(r.Context(), usageRequest, envelope)
	if err != nil {
		if definitiveBillingFailure(err) {
			s.writeInitialReservationFailure(w, r, usageRequest.ID, envelope, err)
		} else {
			w.Header().Set("Location", "/v1/usage-requests/"+usageRequest.ID.String())
			api.WriteJSON(w, http.StatusAccepted, map[string]any{"code": "HOLD_RESULT_UNKNOWN", "usage_request_id": usageRequest.ID, "execution_status": "HOLD_RESERVING", "status_url": "/v1/usage-requests/" + usageRequest.ID.String()})
		}
		return
	}
	if err := s.repository.CompleteHoldAndDispatch(r.Context(), usageRequest.ID, hold.ID, price.ID); err != nil {
		if errors.Is(err, ErrCancellationRequested) {
			result, cancelErr := s.finishPreDispatchCancellation(context.WithoutCancel(r.Context()), usageRequest.ID)
			if cancelErr != nil {
				s.writeError(w, r, cancelErr)
				return
			}
			api.WriteJSON(w, http.StatusOK, result)
			return
		}
		_ = s.billing.Release(r.Context(), usageRequest.ID, hold.ID, "usage state persistence failed")
		_ = s.repository.MarkReleased(r.Context(), usageRequest.ID, "dispatch_intent_persistence_failed")
		s.writeError(w, r, err)
		return
	}
	usageRequest.HoldID = &hold.ID
	usageRequest.PriceVersionID = &price.ID
	envelope, acquired, err := s.repository.AcquireDispatch(r.Context(), usageRequest.ID, httpserver.RequestID(r.Context()), 10*time.Minute)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !acquired {
		s.replayUsage(w, r, usageRequest, logicalBody)
		return
	}
	s.dispatchUsage(w, r, usageRequest, envelope)
}

func applyDefaultChatMaximumOutput(body []byte, request *chatCompletionRequest, fallback int64) ([]byte, error) {
	if request == nil || request.MaxCompletionTokens != 0 || request.MaxTokens != 0 {
		return body, nil
	}
	var normalized map[string]any
	if err := json.Unmarshal(body, &normalized); err != nil {
		return nil, err
	}
	if _, present := normalized["max_completion_tokens"]; present {
		return body, nil
	}
	if _, present := normalized["max_tokens"]; present {
		return body, nil
	}
	normalized["max_completion_tokens"] = fallback
	request.MaxCompletionTokens = fallback
	return json.Marshal(normalized)
}

func normalizeLogicalTurnID(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) < 16 || len(value) > 128 {
		return "", ErrForbidden
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return "", ErrForbidden
	}
	return value, nil
}

func (s *Service) writePricingError(w http.ResponseWriter, r *http.Request, err error) {
	var billingError *BillingHTTPError
	if errors.As(err, &billingError) {
		switch billingError.Code {
		case "PRICE_MISSING":
			api.WriteError(w, http.StatusUnprocessableEntity, "PRICING_MISSING", "a published model price is required", httpserver.RequestID(r.Context()))
			return
		case "PRICE_AMBIGUOUS":
			api.WriteError(w, http.StatusConflict, "PRICING_AMBIGUOUS", "multiple model prices match the request", httpserver.RequestID(r.Context()))
			return
		case "PRICE_UNAVAILABLE":
			api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "pricing is explicitly unavailable for this model", httpserver.RequestID(r.Context()))
			return
		}
	}
	api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "the model price is unavailable", httpserver.RequestID(r.Context()))
}

func (s *Service) dispatchUsage(w http.ResponseWriter, r *http.Request, usageRequest UsageRequest, envelope DispatchEnvelope) {
	defer clear(envelope.Credential)
	if usageRequest.HoldID == nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "dispatch_hold_missing", map[string]any{})
		api.WriteError(w, http.StatusAccepted, "USAGE_REVIEW", "the model request requires reconciliation", httpserver.RequestID(r.Context()))
		return
	}
	holdID := *usageRequest.HoldID
	if err := s.repository.MarkUpstream(context.WithoutCancel(r.Context()), usageRequest.ID); err != nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "dispatch_state_persistence_failed", map[string]any{})
		s.writeError(w, r, err)
		return
	}
	upstreamContext, cancelUpstream := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Hour)
	defer cancelUpstream()
	defer s.registerActiveUpstream(usageRequest.ID, cancelUpstream)()
	renewalFailed := s.renewHold(upstreamContext, cancelUpstream, usageRequest.ID, holdID)
	response, err := s.upstream.Do(upstreamContext, envelope.BaseURL, envelope.Credential, envelope.RequestBody, usageRequest.ID)
	if err != nil {
		s.breaker.Record(resilience.Key(envelope.Provider, "MODEL_DISPATCH", envelope.BaseURL), resilience.UncertainResult)
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
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		s.breaker.Record(resilience.Key(envelope.Provider, "MODEL_DISPATCH", envelope.BaseURL), resilience.Success)
	} else if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
		s.breaker.Record(resilience.Key(envelope.Provider, "MODEL_DISPATCH", envelope.BaseURL), resilience.InfrastructureFailure)
	} else {
		s.breaker.Record(resilience.Key(envelope.Provider, "MODEL_DISPATCH", envelope.BaseURL), resilience.BusinessRejection)
	}
	if upstreamRequestID := s.upstream.UpstreamRequestID(response); upstreamRequestID != "" {
		_ = s.repository.RecordUpstreamRequest(context.WithoutCancel(r.Context()), usageRequest.ID, UpstreamRequestEvidence{ProviderID: envelope.Provider, AdapterVersion: s.upstream.AdapterVersion(), RequestID: upstreamRequestID})
	}
	w.Header().Set("X-Pinvou-Usage-Request-ID", usageRequest.ID.String())
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.handleUpstreamFailure(w, r, usageRequest.ID, holdID, response)
		return
	}
	s.upstream.CopyResponseHeaders(w.Header(), response.Header)
	if envelope.Stream || strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageRequest.ID, "DELIVERING")
		w.WriteHeader(response.StatusCode)
		s.proxyStream(w, r, usageRequest.ID, holdID, envelope.Price, response.Body, renewalFailed, envelope.MaximumCost, billingNoticeContext{ContextType: envelope.ContextType, OrganizationID: envelope.OrganizationID, CanPurchaseOrganizationEntitlements: envelope.CanPurchaseOrganizationEntitlements})
		return
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if readErr != nil {
		_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "upstream_body_incomplete", map[string]any{"error_class": "body_read"})
		_, _ = w.Write(payload)
		return
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	usage, found := s.upstream.ParseUsage(payload)
	if !found {
		if err := s.repository.SaveReplayResult(context.WithoutCancel(r.Context()), usageRequest.ID, ReplayResult{Kind: "JSON", HTTPStatus: response.StatusCode, ContentType: contentType, Payload: payload, ResponseReceived: true, ResponseDisposition: responseDispositionUsageEvidenceRequired}); err != nil {
			_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageRequest.ID, "result_persistence_failed", map[string]any{})
			api.WriteError(w, http.StatusServiceUnavailable, "RESULT_PERSISTENCE_FAILED", "the model result could not be persisted", httpserver.RequestID(r.Context()))
			return
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(payload)
		return
	}
	background := context.WithoutCancel(r.Context())
	_, insufficient, ok := s.authorizeAdditional(background, usageRequest.ID, holdID, envelope.Price, usage, envelope.MaximumCost)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "SETTLEMENT_PENDING", "the final response is awaiting usage settlement", httpserver.RequestID(r.Context()))
		return
	}
	var notice *billingNotice
	if insufficient {
		value := newWalletInsufficientNotice("ADDITIONAL_RESERVATION", "COMPLETED", "SETTLED", envelope.ContextType, envelope.OrganizationID, envelope.CanPurchaseOrganizationEntitlements, usageRequest.ID)
		notice = &value
	}
	actual, settlementSnapshot, err := settlementRecord(envelope.Price, usage, notice)
	if err != nil {
		_ = s.repository.MarkReview(background, usageRequest.ID, "usage_invalid", map[string]any{"usage": usage})
		api.WriteError(w, http.StatusServiceUnavailable, "SETTLEMENT_REVIEW_REQUIRED", settlementMessage("SETTLEMENT_REVIEW_REQUIRED"), httpserver.RequestID(r.Context()))
		return
	}
	if err := s.repository.SaveReplayResult(background, usageRequest.ID, ReplayResult{Kind: "JSON", HTTPStatus: response.StatusCode, ContentType: contentType, Payload: payload, ResponseReceived: true, ResponseDisposition: responseDispositionTrustedUsage, FinalUsage: &usage, ActualMicrocredits: &actual, SettlementSnapshot: settlementSnapshot}); err != nil {
		_ = s.repository.MarkReview(background, usageRequest.ID, "result_persistence_failed", settlementSnapshot)
		api.WriteError(w, http.StatusServiceUnavailable, "RESULT_PERSISTENCE_FAILED", "the model result could not be persisted", httpserver.RequestID(r.Context()))
		return
	}
	if outcome := s.settlePrepared(background, usageRequest.ID, holdID, actual, usage, settlementSnapshot); outcome != "" {
		api.WriteError(w, http.StatusServiceUnavailable, outcome, settlementMessage(outcome), httpserver.RequestID(r.Context()))
		return
	}
	if notice != nil && supportsBillingNotice(r) {
		setBillingNoticeHeaders(w.Header(), *notice)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
	_ = s.repository.MarkDelivery(background, usageRequest.ID, "DELIVERED")
}

func normalizeInferenceBody(body []byte, stream bool) ([]byte, error) {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if stream {
		streamOptions, _ := payload["stream_options"].(map[string]any)
		if streamOptions == nil {
			streamOptions = map[string]any{}
			payload["stream_options"] = streamOptions
		}
		streamOptions["include_usage"] = true
	}
	return json.Marshal(payload)
}

func digestString(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func inferenceFingerprint(protocol string, version int, body []byte) string {
	prefix := []byte("pinvou-inference-canonical-v1\x00" + strings.ToUpper(protocol) + "\x00" + strconv.Itoa(version) + "\x00")
	return digestString(append(prefix, body...))
}

func (s *Service) replayUsage(w http.ResponseWriter, r *http.Request, usage UsageRequest, logicalBody []byte) {
	if usage.ExecutionStatus == "HOLD_RESERVING" {
		handled, err := s.recoverInitialReservationFailure(w, r, usage.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if handled {
			return
		}
	}
	if usage.ExecutionStatus == "RECEIVED" {
		envelope, err := s.rebuildDispatchPreparation(r.Context(), usage, logicalBody)
		if err != nil {
			_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usage.ID, "dispatch_preparation_unrecoverable", map[string]any{"automatic_redispatch": false})
			api.WriteError(w, http.StatusAccepted, "USAGE_REVIEW", "the model request requires reconciliation", httpserver.RequestID(r.Context()))
			return
		}
		if err := s.repository.PrepareDispatch(context.WithoutCancel(r.Context()), usage.ID, envelope); err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := s.repository.MarkHoldReserving(context.WithoutCancel(r.Context()), usage.ID); err != nil {
			s.writeError(w, r, err)
			return
		}
		usage.ExecutionStatus = "HOLD_RESERVING"
	}
	if usage.ExecutionStatus == "HOLD_RESERVING" {
		envelope, err := s.repository.LoadDispatchPreparation(context.WithoutCancel(r.Context()), usage.ID)
		if err != nil {
			_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usage.ID, "dispatch_preparation_missing", map[string]any{"automatic_redispatch": false})
			api.WriteError(w, http.StatusAccepted, "USAGE_REVIEW", "the model request requires reconciliation", httpserver.RequestID(r.Context()))
			return
		}
		hold, err := s.ensureUsageHold(context.WithoutCancel(r.Context()), usage, envelope)
		if err != nil {
			if definitiveBillingFailure(err) {
				s.writeInitialReservationFailure(w, r, usage.ID, envelope, err)
			} else {
				w.Header().Set("Location", "/v1/usage-requests/"+usage.ID.String())
				api.WriteJSON(w, http.StatusAccepted, map[string]any{"code": "HOLD_RESULT_UNKNOWN", "usage_request_id": usage.ID, "execution_status": "HOLD_RESERVING", "status_url": "/v1/usage-requests/" + usage.ID.String()})
			}
			return
		}
		if err := s.repository.CompleteHoldAndDispatch(context.WithoutCancel(r.Context()), usage.ID, hold.ID, envelope.Price.ID); err != nil {
			if errors.Is(err, ErrCancellationRequested) {
				result, cancelErr := s.finishPreDispatchCancellation(context.WithoutCancel(r.Context()), usage.ID)
				if cancelErr != nil {
					s.writeError(w, r, cancelErr)
					return
				}
				api.WriteJSON(w, http.StatusOK, result)
				return
			}
			s.writeError(w, r, err)
			return
		}
		usage, err = s.repository.GetUsage(context.WithoutCancel(r.Context()), usage.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	if usage.ExecutionStatus == "DISPATCH_INTENT_PERSISTED" {
		envelope, acquired, err := s.repository.AcquireDispatch(context.WithoutCancel(r.Context()), usage.ID, httpserver.RequestID(r.Context()), 10*time.Minute)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if acquired {
			s.dispatchUsage(w, r, usage, envelope)
			return
		}
	}
	after := int64(0)
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			api.WriteError(w, http.StatusBadRequest, "LAST_EVENT_ID_INVALID", "Last-Event-ID must be a non-negative sequence", httpserver.RequestID(r.Context()))
			return
		}
		after = parsed
		deliveryID := strings.TrimSpace(r.Header.Get("X-Pinvou-Delivery-ID"))
		if deliveryID == "" {
			deliveryID = usage.SessionID
		}
		_ = s.repository.RecordDeliveryConfirmation(context.WithoutCancel(r.Context()), usage.ID, deliveryID, parsed)
	}
	w.Header().Set("X-Pinvou-Usage-Request-ID", usage.ID.String())
	w.Header().Set("Cache-Control", "no-store")
	if usage.ExecutionStatus == "STREAMING" {
		s.replayLiveStream(w, r, usage.ID, after)
		return
	}
	snapshot, err := s.repository.LoadReplay(r.Context(), usage.ID, after)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if snapshot.ResultExpired {
		api.WriteError(w, http.StatusGone, "IDEMPOTENCY_RESULT_EXPIRED", "the retained model result has expired; inspect the usage request status", httpserver.RequestID(r.Context()))
		return
	}
	if usage.ExecutionStatus == "SETTLEMENT_PENDING" || usage.ExecutionStatus == "REVIEW" || usage.ExecutionStatus == "UPSTREAM_DISPATCHING" || usage.ExecutionStatus == "HOLD_RESERVED" || usage.ExecutionStatus == "HOLD_RESERVING" || usage.ExecutionStatus == "RECEIVED" {
		w.Header().Set("Location", "/v1/usage-requests/"+usage.ID.String())
		api.WriteJSON(w, http.StatusAccepted, map[string]any{"code": "USAGE_" + usage.ExecutionStatus, "usage_request_id": usage.ID, "execution_status": usage.ExecutionStatus, "status_url": "/v1/usage-requests/" + usage.ID.String()})
		return
	}
	if snapshot.Result != nil {
		if snapshot.Result.Kind == "STREAM_AGGREGATE" && len(snapshot.Events) > 0 {
			writeReplayEvents(w, snapshot.Events)
			return
		}
		w.Header().Set("Content-Type", snapshot.Result.ContentType)
		if notice := billingNoticeFromUsageSnapshot(snapshot.Usage.UsageSnapshot); notice != nil && supportsBillingNotice(r) {
			setBillingNoticeHeaders(w.Header(), *notice)
		}
		w.WriteHeader(snapshot.Result.HTTPStatus)
		_, _ = w.Write(snapshot.Result.Payload)
		return
	}
	if len(snapshot.Events) > 0 {
		writeReplayEvents(w, snapshot.Events)
		return
	}
	w.Header().Set("Location", "/v1/usage-requests/"+usage.ID.String())
	api.WriteJSON(w, http.StatusAccepted, map[string]any{"code": "USAGE_RESULT_PENDING", "usage_request_id": usage.ID, "execution_status": usage.ExecutionStatus, "status_url": "/v1/usage-requests/" + usage.ID.String()})
}

func (s *Service) recoverInitialReservationFailure(w http.ResponseWriter, r *http.Request, usageID uuid.UUID) (bool, error) {
	snapshot, err := s.repository.LoadReplay(context.WithoutCancel(r.Context()), usageID, 0)
	if err != nil {
		return false, err
	}
	if snapshot.Result == nil || snapshot.Result.Kind != "JSON" || snapshot.Result.HTTPStatus != http.StatusPaymentRequired {
		return false, nil
	}
	var payload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(snapshot.Result.Payload, &payload) != nil || (payload.Code != "WALLET_INSUFFICIENT" && payload.Code != "WALLET_RESERVE_FAILED") {
		return false, nil
	}
	if err := s.repository.MarkReleased(context.WithoutCancel(r.Context()), usageID, "reserve_failed"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "RESULT_PERSISTENCE_FAILED", "the billing result could not be finalized", httpserver.RequestID(r.Context()))
		return true, nil
	}
	contentType := snapshot.Result.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(snapshot.Result.HTTPStatus)
	_, _ = w.Write(snapshot.Result.Payload)
	return true, nil
}

func (s *Service) replayLiveStream(w http.ResponseWriter, r *http.Request, usageID uuid.UUID, after int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DELIVERING")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := s.repository.LoadReplay(r.Context(), usageID, after)
		if err != nil {
			_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DISCONNECTED")
			return
		}
		terminalReady := snapshot.Usage.ExecutionStatus == "SUCCEEDED" || snapshot.Usage.Status == "COMMITTED"
		terminalWithheld := false
		for _, event := range snapshot.Events {
			if terminalReplayEvent(event.EventType) && !terminalReady {
				terminalWithheld = true
				break
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, strings.ToLower(event.EventType), event.Payload); err != nil {
				_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DISCONNECTED")
				return
			}
			after = event.Sequence
			if flusher != nil {
				flusher.Flush()
			}
		}
		settlementInFlight := snapshot.Usage.ExecutionStatus == "STREAMING" || snapshot.Usage.ExecutionStatus == "UPSTREAM_DISPATCHING" || snapshot.Usage.ExecutionStatus == "SETTLEMENT_PENDING"
		if terminalWithheld && !settlementInFlight {
			_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DISCONNECTED")
			return
		}
		if terminalWithheld || snapshot.Usage.ExecutionStatus == "SETTLEMENT_PENDING" {
			select {
			case <-r.Context().Done():
				_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DISCONNECTED")
				return
			case <-ticker.C:
				continue
			}
		}
		if snapshot.Usage.ExecutionStatus != "STREAMING" && snapshot.Usage.ExecutionStatus != "UPSTREAM_DISPATCHING" {
			_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DELIVERED")
			return
		}
		select {
		case <-r.Context().Done():
			_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DISCONNECTED")
			return
		case <-ticker.C:
		}
	}
}

func terminalReplayEvent(eventType string) bool {
	eventType = strings.ToUpper(strings.TrimSpace(eventType))
	return eventType == "DONE" || eventType == billingNoticeEvent
}

func writeReplayEvents(w http.ResponseWriter, events []ReplayEvent) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, strings.ToLower(event.EventType), event.Payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

type frozenUsageSnapshot struct {
	UpstreamModelRef                    string          `json:"upstream_model_ref"`
	ContextType                         string          `json:"context_type"`
	OrganizationID                      *uuid.UUID      `json:"organization_id,omitempty"`
	CanPurchaseOrganizationEntitlements bool            `json:"can_purchase_organization_entitlements,omitempty"`
	MembershipVersion                   int64           `json:"membership_version"`
	MaximumOutput                       int64           `json:"maximum_output_tokens"`
	Stream                              bool            `json:"stream"`
	PricingSnapshot                     json.RawMessage `json:"pricing_snapshot"`
}

func (s *Service) rebuildDispatchPreparation(ctx context.Context, usage UsageRequest, logicalBody []byte) (DispatchEnvelope, error) {
	var snapshot frozenUsageSnapshot
	if json.Unmarshal(usage.RequestSnapshot, &snapshot) != nil || strings.TrimSpace(snapshot.UpstreamModelRef) == "" || len(snapshot.PricingSnapshot) == 0 {
		return DispatchEnvelope{}, ErrForbidden
	}
	var price PriceVersion
	if json.Unmarshal(snapshot.PricingSnapshot, &price) != nil || price.ID == uuid.Nil {
		return DispatchEnvelope{}, ErrForbidden
	}
	credential, secret, err := s.repository.CredentialSecret(ctx, usage.TokenAccountID, usage.CredentialID)
	if err != nil {
		return DispatchEnvelope{}, err
	}
	defer clear(secret)
	if err := s.providerURLs.Validate(credential.BaseURL); err != nil {
		return DispatchEnvelope{}, err
	}
	var upstreamPayload map[string]any
	if json.Unmarshal(logicalBody, &upstreamPayload) != nil {
		return DispatchEnvelope{}, ErrForbidden
	}
	if _, hasCompletionLimit := upstreamPayload["max_completion_tokens"]; !hasCompletionLimit {
		if _, hasLegacyLimit := upstreamPayload["max_tokens"]; !hasLegacyLimit {
			upstreamPayload["max_completion_tokens"] = snapshot.MaximumOutput
		}
	}
	upstreamPayload["model"] = snapshot.UpstreamModelRef
	upstreamBody, err := json.Marshal(upstreamPayload)
	if err != nil {
		return DispatchEnvelope{}, err
	}
	return DispatchEnvelope{BaseURL: credential.BaseURL, Provider: credential.Provider, Credential: append([]byte(nil), secret...), RequestBody: upstreamBody, Price: price, Stream: snapshot.Stream, MaximumCost: usage.MaximumMicrocredits, MaximumOutput: snapshot.MaximumOutput, ContextType: snapshot.ContextType, OrganizationID: snapshot.OrganizationID, CanPurchaseOrganizationEntitlements: snapshot.CanPurchaseOrganizationEntitlements}, nil
}

func (s *Service) ensureUsageHold(ctx context.Context, usage UsageRequest, envelope DispatchEnvelope) (BillingHold, error) {
	var snapshot frozenUsageSnapshot
	if json.Unmarshal(usage.RequestSnapshot, &snapshot) != nil {
		return BillingHold{}, ErrForbidden
	}
	pricingSnapshot, err := json.Marshal(envelope.Price)
	if err != nil {
		return BillingHold{}, err
	}
	pricing := HoldPricingContext{ModelID: usage.Model, ProtocolCode: "CHAT", FormulaVersionID: envelope.Price.FormulaVersionID, PricingSnapshot: pricingSnapshot, RequestSnapshot: usage.RequestSnapshot}
	if snapshot.ContextType == "ORGANIZATION" && usage.OrganizationID != nil {
		if client, ok := s.billing.(SnapshotOrganizationBillingClient); ok {
			return client.ReserveMemberQuotaPriced(ctx, usage.ID, usage.WalletID, *usage.OrganizationID, usage.UserID, snapshot.MembershipVersion, usage.MaximumMicrocredits, envelope.Price.ID, pricing)
		}
		client, ok := s.billing.(OrganizationBillingClient)
		if !ok {
			return BillingHold{}, ErrForbidden
		}
		return client.ReserveMemberQuota(ctx, usage.ID, usage.WalletID, *usage.OrganizationID, usage.UserID, snapshot.MembershipVersion, usage.MaximumMicrocredits, envelope.Price.ID)
	}
	if client, ok := s.billing.(SnapshotBillingClient); ok {
		return client.ReservePriced(ctx, usage.ID, usage.WalletID, usage.MaximumMicrocredits, envelope.Price.ID, pricing)
	}
	return s.billing.Reserve(ctx, usage.ID, usage.WalletID, usage.MaximumMicrocredits, envelope.Price.ID)
}

func definitiveBillingFailure(err error) bool {
	var failure *BillingHTTPError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Status {
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func walletInsufficient(err error) bool {
	var failure *BillingHTTPError
	return errors.As(err, &failure) && failure.Status == http.StatusPaymentRequired && strings.EqualFold(strings.TrimSpace(failure.Code), "WALLET_INSUFFICIENT")
}

func (s *Service) writeInitialReservationFailure(w http.ResponseWriter, r *http.Request, usageID uuid.UUID, envelope DispatchEnvelope, failure error) {
	response := map[string]any{
		"code":       "WALLET_RESERVE_FAILED",
		"message":    "the wallet could not reserve enough credits",
		"request_id": httpserver.RequestID(r.Context()),
	}
	if walletInsufficient(failure) {
		response["code"] = "WALLET_INSUFFICIENT"
		response["message"] = "the wallet has insufficient available credits"
		response["billing_notice"] = newWalletInsufficientNotice("INITIAL_RESERVATION", "REJECTED", "NOT_STARTED", envelope.ContextType, envelope.OrganizationID, envelope.CanPurchaseOrganizationEntitlements, usageID)
	}
	payload, err := json.Marshal(response)
	background := context.WithoutCancel(r.Context())
	if err != nil || s.repository.SaveReplayResult(background, usageID, ReplayResult{Kind: "JSON", HTTPStatus: http.StatusPaymentRequired, ContentType: "application/json", Payload: payload}) != nil {
		_ = s.repository.MarkReview(background, usageID, "result_persistence_failed", map[string]any{"billing_stage": "INITIAL_RESERVATION"})
		api.WriteError(w, http.StatusServiceUnavailable, "RESULT_PERSISTENCE_FAILED", "the billing result could not be persisted", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.repository.MarkReleased(background, usageID, "reserve_failed"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "RESULT_PERSISTENCE_FAILED", "the billing result could not be finalized", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = w.Write(payload)
}

func setBillingNoticeHeaders(header http.Header, notice billingNotice) {
	header.Set("X-Pinvou-Billing-Code", notice.Code)
	header.Set("X-Pinvou-Billing-Stage", notice.Stage)
	header.Set("X-Pinvou-Billing-Turn-Outcome", notice.TurnOutcome)
	header.Set("X-Pinvou-Billing-Settlement-Status", notice.SettlementStatus)
	header.Set("X-Pinvou-Billing-Action", notice.Action)
	if notice.ContextType != "" {
		header.Set("X-Pinvou-Billing-Context-Type", notice.ContextType)
	}
	if notice.OrganizationID != nil {
		header.Set("X-Pinvou-Billing-Organization-ID", notice.OrganizationID.String())
	}
	header.Set("X-Pinvou-Billing-Usage-Request-ID", notice.UsageRequestID.String())
}

func billingNoticeFromUsageSnapshot(snapshot json.RawMessage) *billingNotice {
	if len(snapshot) == 0 {
		return nil
	}
	var payload struct {
		BillingNotice *billingNotice `json:"billing_notice"`
	}
	if json.Unmarshal(snapshot, &payload) != nil || payload.BillingNotice == nil || payload.BillingNotice.SchemaVersion != 1 || payload.BillingNotice.Code != "WALLET_INSUFFICIENT" {
		return nil
	}
	return payload.BillingNotice
}

func (s *Service) handleUpstreamFailure(w http.ResponseWriter, r *http.Request, usageID, holdID uuid.UUID, response *http.Response) {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	background := context.WithoutCancel(r.Context())
	if len(payload) == 0 {
		payload = []byte(`{"error":{"code":"UPSTREAM_ERROR","message":"the upstream rejected the model request"}}`)
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	disposition := responseDispositionUsageEvidenceRequired
	if s.upstream.RejectsBeforeGeneration(response.StatusCode) {
		disposition = responseDispositionRejectedBeforeGeneration
	}
	if err := s.repository.SaveReplayResult(background, usageID, ReplayResult{Kind: "JSON", HTTPStatus: response.StatusCode, ContentType: contentType, Payload: payload, ResponseReceived: true, ResponseDisposition: disposition}); err != nil {
		_ = s.repository.MarkReview(background, usageID, "result_persistence_failed", map[string]any{"status_code": response.StatusCode})
		api.WriteError(w, http.StatusServiceUnavailable, "RESULT_PERSISTENCE_FAILED", "the upstream result could not be persisted", httpserver.RequestID(r.Context()))
		return
	}
	if disposition == responseDispositionRejectedBeforeGeneration {
		if err := s.billing.Release(background, usageID, holdID, "upstream rejected before generation"); err == nil {
			_ = s.repository.MarkReleased(background, usageID, "upstream_rejected")
		}
	}
	s.upstream.CopyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(payload)
}

func (s *Service) proxyStream(w http.ResponseWriter, r *http.Request, usageID, holdID uuid.UUID, price PriceVersion, body io.Reader, renewalFailed <-chan struct{}, authorizedMaximum int64, contexts ...billingNoticeContext) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var usage Usage
	usageFound := false
	var lastSequence int64
	aggregate := make([]json.RawMessage, 0)
	deliveryConnected := true
	doneFound := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(payload, []byte("[DONE]")) {
				doneFound = true
				break
			}
			if json.Valid(payload) {
				aggregate = append(aggregate, append(json.RawMessage(nil), payload...))
			}
			if parsed, ok := s.upstream.ParseUsage(payload); ok {
				usage, usageFound = parsed, true
			}
			sequence, err := s.repository.AppendReplayEvent(context.WithoutCancel(r.Context()), usageID, "CHUNK", payload)
			if err != nil {
				_ = s.repository.MarkReview(context.WithoutCancel(r.Context()), usageID, "stream_event_persistence_failed", map[string]any{"last_sequence": lastSequence})
				return
			}
			lastSequence = sequence
			if deliveryConnected {
				current, stateErr := s.repository.GetUsage(context.WithoutCancel(r.Context()), usageID)
				if stateErr == nil && current.DeliveryStatus == "USER_STOPPED" {
					deliveryConnected = false
				}
			}
			if deliveryConnected {
				if _, err := fmt.Fprintf(w, "id: %d\nevent: chunk\ndata: %s\n\n", sequence, payload); err != nil {
					deliveryConnected = false
					_ = s.repository.MarkDelivery(context.WithoutCancel(r.Context()), usageID, "DISCONNECTED")
				} else if flusher != nil {
					flusher.Flush()
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
	streamErr := scanner.Err()
	if streamErr != nil || !usageFound || !doneFound {
		reason := "stream_usage_missing"
		if !doneFound {
			reason = "stream_terminal_missing"
		}
		_ = s.repository.MarkReview(background, usageID, reason, map[string]any{"stream_error": streamErr != nil, "usage_found": usageFound, "done_found": doneFound})
		return
	}
	_, insufficient, authorized := s.authorizeAdditional(background, usageID, holdID, price, usage, authorizedMaximum)
	if !authorized {
		return
	}
	var noticePayload []byte
	var noticeSequence int64
	var persistedNotice *billingNotice
	if insufficient {
		context := billingNoticeContextValue(contexts)
		value := newWalletInsufficientNotice("ADDITIONAL_RESERVATION", "COMPLETED", "SETTLED", context.ContextType, context.OrganizationID, context.CanPurchaseOrganizationEntitlements, usageID)
		persistedNotice = &value
	}
	if persistedNotice != nil && supportsBillingNotice(r) {
		noticePayload, _ = json.Marshal(persistedNotice)
		if len(noticePayload) > 0 {
			var err error
			noticeSequence, err = s.repository.AppendReplayEvent(background, usageID, billingNoticeEvent, noticePayload)
			if err != nil {
				_ = s.repository.MarkReview(background, usageID, "stream_event_persistence_failed", map[string]any{"last_sequence": lastSequence})
				return
			}
			lastSequence = noticeSequence
		}
	}
	doneSequence, err := s.repository.AppendReplayEvent(background, usageID, "DONE", []byte("[DONE]"))
	if err != nil {
		_ = s.repository.MarkReview(background, usageID, "stream_event_persistence_failed", map[string]any{"last_sequence": lastSequence})
		return
	}
	lastSequence = doneSequence
	actual, settlementSnapshot, settlementErr := settlementRecord(price, usage, persistedNotice)
	if settlementErr != nil {
		_ = s.repository.MarkReview(background, usageID, "usage_invalid", map[string]any{"usage": usage})
		return
	}
	aggregatePayload, err := json.Marshal(map[string]any{"events": aggregate, "usage": usage, "last_sequence": lastSequence})
	if err != nil || s.repository.SaveReplayResult(background, usageID, ReplayResult{Kind: "STREAM_AGGREGATE", HTTPStatus: http.StatusOK, ContentType: "application/json", Payload: aggregatePayload, ResponseReceived: true, ResponseDisposition: responseDispositionTrustedUsage, FinalUsage: &usage, ActualMicrocredits: &actual, SettlementSnapshot: settlementSnapshot, LastSequence: lastSequence}) != nil {
		_ = s.repository.MarkReview(background, usageID, "result_persistence_failed", map[string]any{"last_sequence": lastSequence})
		return
	}
	if outcome := s.settlePrepared(background, usageID, holdID, actual, usage, settlementSnapshot); outcome != "" {
		if deliveryConnected {
			_, _ = fmt.Fprintf(w, "event: error\ndata: {\"error\":{\"code\":%q,\"message\":%q}}\n\n", outcome, settlementMessage(outcome))
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}
	if deliveryConnected && len(noticePayload) > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\nevent: billing_notice\ndata: %s\n\n", noticeSequence, noticePayload); err != nil {
			deliveryConnected = false
			_ = s.repository.MarkDelivery(background, usageID, "DISCONNECTED")
		} else if flusher != nil {
			flusher.Flush()
		}
	}
	if deliveryConnected {
		if _, err := fmt.Fprintf(w, "id: %d\nevent: done\ndata: [DONE]\n\n", doneSequence); err != nil {
			deliveryConnected = false
			_ = s.repository.MarkDelivery(background, usageID, "DISCONNECTED")
		} else if flusher != nil {
			flusher.Flush()
		}
	}
	if deliveryConnected {
		_ = s.repository.MarkDelivery(background, usageID, "DELIVERED")
	}
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

func billingNoticeContextValue(values []billingNoticeContext) billingNoticeContext {
	if len(values) == 0 {
		return billingNoticeContext{}
	}
	return values[0]
}

func settlementMessage(outcome string) string {
	if outcome == "SETTLEMENT_REVIEW_REQUIRED" {
		return "the final response requires billing review"
	}
	return "the final response is awaiting usage settlement"
}

func definitiveSettlementConflict(err error) bool {
	var failure *BillingHTTPError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusPaymentRequired, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func (s *Service) settle(ctx context.Context, usageID, holdID uuid.UUID, price PriceVersion, usage Usage, notice *billingNotice) string {
	actual, snapshot, err := settlementRecord(price, usage, notice)
	if err != nil {
		if markErr := s.repository.MarkReview(ctx, usageID, "usage_invalid", map[string]any{"usage": usage}); markErr != nil {
			return "SETTLEMENT_PENDING"
		}
		return "SETTLEMENT_REVIEW_REQUIRED"
	}
	return s.settlePrepared(ctx, usageID, holdID, actual, usage, snapshot)
}

func settlementRecord(price PriceVersion, usage Usage, notice *billingNotice) (int64, map[string]any, error) {
	actual, err := Cost(price, usage)
	if err != nil {
		return 0, nil, err
	}
	snapshot := map[string]any{"usage": usage, "actual_microcredits": actual}
	if notice != nil {
		snapshot["billing_notice"] = notice
	}
	return actual, snapshot, nil
}

func (s *Service) settlePrepared(ctx context.Context, usageID, holdID uuid.UUID, actual int64, usage Usage, snapshot map[string]any) string {
	if err := s.billing.Commit(ctx, usageID, holdID, actual, usage); err != nil {
		if definitiveSettlementConflict(err) {
			if markErr := s.repository.MarkReview(ctx, usageID, "settlement_conflict", snapshot); markErr != nil {
				return "SETTLEMENT_PENDING"
			}
			return "SETTLEMENT_REVIEW_REQUIRED"
		}
		_ = s.repository.MarkReview(ctx, usageID, "settlement_pending", snapshot)
		return "SETTLEMENT_PENDING"
	}
	if err := s.repository.MarkCommitted(ctx, usageID, actual, snapshot); err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "local_commit_record_pending", snapshot)
		return "SETTLEMENT_PENDING"
	}
	return ""
}

func (s *Service) authorizeAdditional(ctx context.Context, usageID, holdID uuid.UUID, price PriceVersion, usage Usage, maximum int64) (int64, bool, bool) {
	actual, err := Cost(price, usage)
	if err != nil {
		_ = s.repository.MarkReview(ctx, usageID, "usage_invalid", map[string]any{"usage": usage})
		return maximum, false, false
	}
	if actual <= maximum {
		return maximum, false, true
	}
	additional := actual - maximum
	client, ok := s.billing.(SnapshotBillingClient)
	if !ok {
		return actual, false, true
	}
	if err := client.Extend(ctx, usageID, holdID, additional); err != nil {
		// Trusted final usage still converges through the idempotent Commit.
		// Only a definite wallet-insufficient response becomes a user notice;
		// uncertain failures become SETTLEMENT_PENDING only if Commit also fails.
		return actual, walletInsufficient(err), true
	}
	if repository, ok := s.repository.(interface {
		IncreaseMaximum(context.Context, uuid.UUID, int64) error
	}); ok {
		if err := repository.IncreaseMaximum(ctx, usageID, additional); err != nil {
			return actual, false, true
		}
	}
	return actual, false, true
}
