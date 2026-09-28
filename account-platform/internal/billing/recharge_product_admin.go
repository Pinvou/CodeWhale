package billing

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const rechargeProductRecentAuthentication = 15 * time.Minute

func (s *Service) requireRecentProductAuthentication() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := identity.FromContext(c.Request.Context())
		now := s.now()
		if !ok || principal.AuthenticatedAt.IsZero() || principal.AuthenticatedAt.After(now.Add(time.Minute)) || now.Sub(principal.AuthenticatedAt) > rechargeProductRecentAuthentication {
			api.WriteError(c.Writer, http.StatusUnauthorized, "RECENT_AUTHENTICATION_REQUIRED", "recent authentication is required", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		c.Next()
	}
}

func catalogActor(r *http.Request) (identity.Principal, uuid.UUID, bool) {
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		return principal, uuid.Nil, false
	}
	userID, err := uuid.Parse(principal.Subject)
	return principal, userID, err == nil
}

func catalogRequestHash(command string, body any) string {
	raw, _ := json.Marshal(body)
	digest := sha256.Sum256(append([]byte(command+"\n"), raw...))
	return hex.EncodeToString(digest[:])
}

func catalogResourceRequestHash(command string, catalogID uuid.UUID, body any) string {
	raw, _ := json.Marshal(body)
	digest := sha256.Sum256(append([]byte(command+"\n"+catalogID.String()+"\n"), raw...))
	return hex.EncodeToString(digest[:])
}

func (s *Service) replayCatalogCommand(w http.ResponseWriter, r *http.Request, hash string) (bool, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 16 || len(key) > 128 {
		return false, ErrConflict
	}
	status, body, found, err := s.catalog.RechargeProductCommand(r.Context(), key, hash)
	if err != nil || !found {
		return found, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return true, err
}

func (s *Service) saveCatalogCommand(r *http.Request, command, hash, actor, reason string, catalogID *uuid.UUID, expected int64, before, after *int64, status int, response any) error {
	body, _ := json.Marshal(response)
	return s.catalog.RecordRechargeProductCommand(r.Context(), strings.TrimSpace(r.Header.Get("Idempotency-Key")), command, hash, catalogID, actor, reason, expected, before, after, status, body)
}

func parseCatalogCursor(value string) (*time.Time, *uuid.UUID, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return nil, nil, ErrConflict
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, nil, err
	}
	id, err := uuid.Parse(parts[1])
	return &at, &id, err
}

func catalogCursor(item RechargeProductCatalogItem) string {
	return base64.RawURLEncoding.EncodeToString([]byte(item.UpdatedAt.Format(time.RFC3339Nano) + "|" + item.CatalogItemID.String()))
}

func (s *Service) adminRechargeProducts(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			s.writeError(w, r, ErrConflict)
			return
		}
		limit = value
	}
	at, id, err := parseCatalogCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	items, err := s.catalog.ListRechargeProducts(r.Context(), r.URL.Query().Get("lifecycle_status"), strings.EqualFold(r.URL.Query().Get("include_deleted"), "true"), limit, at, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	next := ""
	if len(items) == limit {
		next = catalogCursor(items[len(items)-1])
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

type rechargeProductAdapterOption struct {
	ProviderID      string   `json:"provider_id"`
	ProviderVersion string   `json:"provider_version"`
	Channels        []string `json:"channels"`
}

type rechargeProductCreateOptions struct {
	Adapters            []rechargeProductAdapterOption `json:"adapters"`
	InvoiceProductNames []string                       `json:"invoice_product_names"`
}

func (s *Service) rechargeProductCreateOptions(r *http.Request) (rechargeProductCreateOptions, error) {
	if s.payments == nil || s.catalogCreateOptions == nil {
		return rechargeProductCreateOptions{}, payment.ErrUnsupported
	}
	capabilities := s.payments.Capabilities()
	names, err := s.catalogCreateOptions.ListRechargeProductInvoiceNames(r.Context())
	if err != nil {
		return rechargeProductCreateOptions{}, err
	}
	return rechargeProductCreateOptions{
		Adapters: []rechargeProductAdapterOption{{
			ProviderID:      capabilities.ProviderID,
			ProviderVersion: capabilities.Version,
			Channels:        append([]string(nil), capabilities.Channels...),
		}},
		InvoiceProductNames: names,
	}, nil
}

func (s *Service) adminRechargeProductCreateOptions(w http.ResponseWriter, r *http.Request) {
	options, err := s.rechargeProductCreateOptions(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, options)
}

func validateRechargeProductCreateSelection(input CreateRechargeProduct, options rechargeProductCreateOptions) error {
	invoiceName := strings.TrimSpace(input.InvoiceProductName)
	invoiceAllowed := false
	for _, candidate := range options.InvoiceProductNames {
		invoiceAllowed = invoiceAllowed || invoiceName == candidate
	}
	if !invoiceAllowed {
		return ErrConflict
	}
	for _, adapter := range options.Adapters {
		if strings.TrimSpace(input.Mapping.ProviderID) != adapter.ProviderID || strings.TrimSpace(input.Mapping.ProviderVersion) != adapter.ProviderVersion {
			continue
		}
		if len(input.Mapping.Channels) == 0 {
			return ErrConflict
		}
		seen := make(map[string]bool, len(input.Mapping.Channels))
		for _, channel := range input.Mapping.Channels {
			channel = strings.ToUpper(strings.TrimSpace(channel))
			if channel == "" || seen[channel] {
				return ErrConflict
			}
			seen[channel] = true
			allowed := false
			for _, candidate := range adapter.Channels {
				allowed = allowed || strings.EqualFold(channel, candidate)
			}
			if !allowed {
				return ErrConflict
			}
		}
		return nil
	}
	return ErrConflict
}

type createRechargeProductRequest struct {
	CreateRechargeProduct
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}

func (s *Service) adminCreateRechargeProduct(w http.ResponseWriter, r *http.Request) {
	var body createRechargeProductRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	principal, _, ok := catalogActor(r)
	if !ok {
		s.writeError(w, r, ErrConflict)
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.ExpectedVersion != 0 || body.Reason == "" {
		s.writeError(w, r, ErrConflict)
		return
	}
	if _, err := normalizeRechargeProductMicrocredits(&body.CreateRechargeProduct); err != nil {
		s.writeError(w, r, err)
		return
	}
	hash := catalogRequestHash("CREATE", body)
	if replay, err := s.replayCatalogCommand(w, r, hash); replay || err != nil {
		if err != nil {
			s.writeError(w, r, err)
		}
		return
	}
	options, err := s.rechargeProductCreateOptions(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err = validateRechargeProductCreateSelection(body.CreateRechargeProduct, options); err != nil {
		s.writeError(w, r, err)
		return
	}
	item, err := s.catalog.CreateRechargeProduct(r.Context(), body.CreateRechargeProduct)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	after := item.Version
	if err = s.saveCatalogCommand(r, "BILLING_RECHARGE_PRODUCT_CREATE", hash, principal.Subject, body.Reason, &item.CatalogItemID, 0, nil, &after, http.StatusCreated, item); err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, item)
}

type catalogTransitionRequest struct {
	ExpectedVersion int64      `json:"expected_version"`
	Reason          string     `json:"reason"`
	EvidenceID      *uuid.UUID `json:"evidence_id,omitempty"`
	RetryOf         *uuid.UUID `json:"retry_of,omitempty"`
}

func catalogPathIDs(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	catalogID, err := uuid.Parse(r.PathValue("catalog_item_id"))
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	verificationID := uuid.Nil
	if raw := r.PathValue("verification_id"); raw != "" {
		verificationID, err = uuid.Parse(raw)
	}
	return catalogID, verificationID, err
}

func (s *Service) runCatalogTransition(w http.ResponseWriter, r *http.Request, target, command string) {
	var body catalogTransitionRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	catalogID, _, err := catalogPathIDs(r)
	if err != nil || body.ExpectedVersion < 1 || body.Reason == "" {
		s.writeError(w, r, ErrConflict)
		return
	}
	principal, _, ok := catalogActor(r)
	if !ok {
		s.writeError(w, r, ErrConflict)
		return
	}
	hash := catalogRequestHash(command, body)
	if replay, replayErr := s.replayCatalogCommand(w, r, hash); replay || replayErr != nil {
		if replayErr != nil {
			s.writeError(w, r, replayErr)
		}
		return
	}
	before := body.ExpectedVersion
	item, err := s.catalog.TransitionRechargeProduct(r.Context(), catalogID, body.ExpectedVersion, target, body.EvidenceID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	after := item.Version
	if err = s.saveCatalogCommand(r, command, hash, principal.Subject, body.Reason, &catalogID, body.ExpectedVersion, &before, &after, http.StatusOK, item); err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}
func (s *Service) adminPublishRechargeProduct(w http.ResponseWriter, r *http.Request) {
	s.runCatalogTransition(w, r, "ON_SALE", "BILLING_RECHARGE_PRODUCT_PUBLISH")
}
func (s *Service) adminUnpublishRechargeProduct(w http.ResponseWriter, r *http.Request) {
	s.runCatalogTransition(w, r, "OFF_SALE", "BILLING_RECHARGE_PRODUCT_UNPUBLISH")
}
func (s *Service) adminDeleteRechargeProduct(w http.ResponseWriter, r *http.Request) {
	s.runCatalogTransition(w, r, "DELETED", "BILLING_RECHARGE_PRODUCT_DELETE")
}

type catalogDisplayNamesRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	DisplayNameZhCN string `json:"display_name_zh_cn"`
	DisplayNameEN   string `json:"display_name_en"`
	DisplayNameJA   string `json:"display_name_ja"`
	Reason          string `json:"reason"`
}

func (s *Service) adminUpdateRechargeProductDisplayNames(w http.ResponseWriter, r *http.Request) {
	catalogID, _, pathErr := catalogPathIDs(r)
	if pathErr != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		s.writeError(w, r, ErrConflict)
		return
	}
	var body catalogDisplayNamesRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.DisplayNameZhCN = strings.TrimSpace(body.DisplayNameZhCN)
	body.DisplayNameEN = strings.TrimSpace(body.DisplayNameEN)
	body.DisplayNameJA = strings.TrimSpace(body.DisplayNameJA)
	body.Reason = strings.TrimSpace(body.Reason)
	principal, _, actorOK := catalogActor(r)
	if !actorOK || body.ExpectedVersion < 1 || body.DisplayNameZhCN == "" || body.DisplayNameEN == "" || body.DisplayNameJA == "" || body.Reason == "" {
		s.writeError(w, r, ErrConflict)
		return
	}
	hash := catalogResourceRequestHash("BILLING_RECHARGE_PRODUCT_DISPLAY_NAMES_UPDATE", catalogID, body)
	item, err := s.catalog.ExecuteRechargeProductDisplayNamesUpdate(r.Context(), RechargeProductDisplayNamesCommand{
		CatalogItemID: catalogID, ExpectedVersion: body.ExpectedVersion,
		DisplayNameZhCN: body.DisplayNameZhCN, DisplayNameEN: body.DisplayNameEN, DisplayNameJA: body.DisplayNameJA,
		IdempotencyKey: idempotencyKey, RequestHash: hash, Actor: principal.Subject, Reason: body.Reason,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminProductAdmissionVerifications(w http.ResponseWriter, r *http.Request) {
	catalogID, _, err := catalogPathIDs(r)
	if err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	items, err := s.catalog.ListProductAdmissionVerifications(r.Context(), catalogID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) startProductAdmissionVerification(w http.ResponseWriter, r *http.Request, retryPath bool) {
	var body catalogTransitionRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	catalogID, verificationID, err := catalogPathIDs(r)
	if err != nil || body.ExpectedVersion < 1 || body.Reason == "" {
		s.writeError(w, r, ErrConflict)
		return
	}
	principal, userID, ok := catalogActor(r)
	if !ok {
		s.writeError(w, r, ErrConflict)
		return
	}
	item, err := s.catalog.RechargeProductCatalogItem(r.Context(), catalogID)
	if err != nil || item.Version != body.ExpectedVersion {
		s.writeError(w, r, ErrConflict)
		return
	}
	if retryPath {
		body.RetryOf = &verificationID
	}
	hash := catalogRequestHash("BILLING_RECHARGE_PRODUCT_ADMISSION_VERIFY", body)
	if replay, replayErr := s.replayCatalogCommand(w, r, hash); replay || replayErr != nil {
		if replayErr != nil {
			s.writeError(w, r, replayErr)
		}
		return
	}
	verification, _, err := s.catalog.StartProductAdmissionVerification(r.Context(), catalogID, userID, body.RetryOf)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	before, after := item.Version, item.Version
	if err = s.saveCatalogCommand(r, "BILLING_RECHARGE_PRODUCT_ADMISSION_VERIFY", hash, principal.Subject, body.Reason, &catalogID, body.ExpectedVersion, &before, &after, http.StatusAccepted, verification); err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusAccepted, verification)
}
func (s *Service) adminStartProductAdmissionVerification(w http.ResponseWriter, r *http.Request) {
	s.startProductAdmissionVerification(w, r, false)
}
func (s *Service) adminRetryProductAdmissionVerification(w http.ResponseWriter, r *http.Request) {
	s.startProductAdmissionVerification(w, r, true)
}

func (s *Service) adminRevokeProductAdmission(w http.ResponseWriter, r *http.Request) {
	var body catalogTransitionRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	catalogID, verificationID, err := catalogPathIDs(r)
	if err != nil || body.ExpectedVersion < 1 || body.Reason == "" {
		s.writeError(w, r, ErrConflict)
		return
	}
	principal, _, ok := catalogActor(r)
	if !ok {
		s.writeError(w, r, ErrConflict)
		return
	}
	item, err := s.catalog.RechargeProductCatalogItem(r.Context(), catalogID)
	if err != nil || item.Version != body.ExpectedVersion {
		s.writeError(w, r, ErrConflict)
		return
	}
	evidenceID, err := s.catalog.EvidenceIDForVerification(r.Context(), catalogID, verificationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	hash := catalogRequestHash("BILLING_RECHARGE_PRODUCT_ADMISSION_REVOKE", body)
	if replay, replayErr := s.replayCatalogCommand(w, r, hash); replay || replayErr != nil {
		if replayErr != nil {
			s.writeError(w, r, replayErr)
		}
		return
	}
	if err = s.catalog.RevokeProductAdmission(r.Context(), evidenceID, "HUMAN", principal.Subject, body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	before, after := item.Version, item.Version
	response := map[string]any{"status": "REVOKED", "evidence_id": evidenceID}
	if err = s.saveCatalogCommand(r, "BILLING_RECHARGE_PRODUCT_ADMISSION_REVOKE", hash, principal.Subject, body.Reason, &catalogID, body.ExpectedVersion, &before, &after, http.StatusOK, response); err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, response)
}

func (s *Service) completeProductAdmissionVerification(w http.ResponseWriter, r *http.Request) {
	verificationID, err := uuid.Parse(r.PathValue("verification_id"))
	if err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	var body struct {
		Method    string    `json:"method"`
		Source    string    `json:"source_reference"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || strings.TrimSpace(workload.Subject) == "" {
		s.writeError(w, r, ErrConflict)
		return
	}
	evidenceID, err := s.catalog.CompleteProductAdmissionVerification(r.Context(), verificationID, workload.Subject, body.Method, body.Source, body.ExpiresAt)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"evidence_id": evidenceID})
}

func (s *Service) revokeProductAdmissionInternal(w http.ResponseWriter, r *http.Request) {
	evidenceID, err := uuid.Parse(r.PathValue("evidence_id"))
	if err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || strings.TrimSpace(workload.Subject) == "" {
		s.writeError(w, r, ErrConflict)
		return
	}
	if err = s.catalog.RevokeProductAdmission(r.Context(), evidenceID, "PROVIDER", workload.Subject, body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "REVOKED"})
}
