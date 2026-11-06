package billing

import (
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func (s *Service) manualAdmissionActor(w http.ResponseWriter, r *http.Request) (productManualAdmissionRepository, uuid.UUID, bool) {
	store, ok := s.repository.(productManualAdmissionRepository)
	principal, authenticated := identity.FromContext(r.Context())
	if !ok || s.authorizer == nil || !authenticated {
		s.writeError(w, r, ErrConflict)
		return nil, uuid.Nil, false
	}
	actor, err := s.authorizer.AuthorizedUserID(r.Context(), principal, "platform.products.publish")
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return nil, uuid.Nil, false
	}
	if err = s.productAdmissionGuard(r, actor)(r.Context()); err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return nil, uuid.Nil, false
	}
	return store, actor, true
}
func manualAdmissionPath(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	catalogID, err := uuid.Parse(r.PathValue("catalog_item_id"))
	if err != nil || catalogID == uuid.Nil {
		return uuid.Nil, uuid.Nil, ErrConflict
	}
	var verificationID uuid.UUID
	if raw := r.PathValue("verification_id"); raw != "" {
		verificationID, err = uuid.Parse(raw)
		if err != nil || verificationID == uuid.Nil {
			return uuid.Nil, uuid.Nil, ErrConflict
		}
	}
	return catalogID, verificationID, nil
}
func (s *Service) adminCreateManualProductAdmission(w http.ResponseWriter, r *http.Request) {
	store, actor, ok := s.manualAdmissionActor(w, r)
	if !ok {
		return
	}
	catalogID, _, err := manualAdmissionPath(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		ExpectedVersion int64     `json:"expected_version"`
		MaterialID      uuid.UUID `json:"material_id"`
		Reason          string    `json:"reason"`
	}
	if err = api.DecodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	value, err := store.CreateManualProductAdmission(r.Context(), ProductManualAdmissionCreate{CatalogItemID: catalogID, MaterialID: body.MaterialID, Actor: actor, ExpectedVersion: body.ExpectedVersion, Reason: strings.TrimSpace(body.Reason), IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")), Environment: s.productAdmissionEnvironment}, s.productAdmissionGuard(r, actor))
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	api.WriteJSON(w, 201, value)
}
func (s *Service) adminGetManualProductAdmission(w http.ResponseWriter, r *http.Request) {
	store, actor, ok := s.manualAdmissionActor(w, r)
	if !ok {
		return
	}
	catalogID, verificationID, err := manualAdmissionPath(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	value, err := store.GetManualProductAdmission(r.Context(), catalogID, verificationID)
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	if err = s.productAdmissionGuard(r, actor)(r.Context()); err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, 200, value)
}
func (s *Service) adminDecideManualProductAdmission(w http.ResponseWriter, r *http.Request) {
	store, actor, ok := s.manualAdmissionActor(w, r)
	if !ok {
		return
	}
	catalogID, verificationID, err := manualAdmissionPath(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		ExpectedVersion int64  `json:"expected_version"`
		SubmissionSHA   string `json:"submission_sha"`
		Decision        string `json:"decision"`
		Reason          string `json:"reason"`
	}
	if err = api.DecodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	value, err := store.DecideManualProductAdmission(r.Context(), ProductManualAdmissionDecide{CatalogItemID: catalogID, VerificationID: verificationID, Actor: actor, ExpectedVersion: body.ExpectedVersion, SubmissionSHA: body.SubmissionSHA, Decision: body.Decision, Reason: strings.TrimSpace(body.Reason), IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key"))}, s.productAdmissionGuard(r, actor))
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	api.WriteJSON(w, 200, value)
}
