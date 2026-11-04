package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/evidenceenvelope"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type ProductAdmissionMaterialRepository interface {
	SaveProductAdmissionMaterial(context.Context, ProductAdmissionMaterialSave, ProductAdmissionMaterialGuard) (ProductAdmissionMaterial, error)
	GetProductAdmissionMaterial(context.Context, uuid.UUID, uuid.UUID) (ProductAdmissionMaterial, error)
	GetProductAdmissionMaterialEnvelope(context.Context, uuid.UUID, uuid.UUID) (ProductAdmissionMaterial, evidenceenvelope.Envelope, error)
	ConfirmProductAdmissionMaterialRead(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, ProductAdmissionMaterialGuard) error
	GetProductAdmissionMaterialReceipt(context.Context, uuid.UUID, uuid.UUID, string) (ProductAdmissionMaterial, error)
	SetProductAdmissionMaterialLegalHold(context.Context, ProductAdmissionMaterialHold, ProductAdmissionMaterialGuard) (ProductAdmissionMaterial, error)
	PurgeProductAdmissionMaterials(context.Context, uuid.UUID, int, ProductAdmissionMaterialGuard) (int, error)
}

type productAdmissionOriginalValidator interface {
	Validate(context.Context, string, []byte) error
}

// WithProductAdmissionMaterials enables only this bounded product facility.
// The production constructor supplies the authenticated isolated validator;
// callers cannot silently fall back to in-process parsing or plaintext storage.
func (s *Service) WithProductAdmissionMaterials(repository ProductAdmissionMaterialRepository, wrapper evidenceenvelope.KeyWrapper, validator productAdmissionOriginalValidator) *Service {
	s.admissionMaterials, s.admissionMaterialKeys, s.admissionMaterialValidator = repository, wrapper, validator
	return s
}

func (s *Service) productAdmissionGuard(r *http.Request, actor uuid.UUID) ProductAdmissionMaterialGuard {
	principal, valid := identity.FromContext(r.Context())
	return func(ctx context.Context) error {
		now := s.now()
		if !valid || s.authorizer == nil || actor == uuid.Nil || principal.AuthenticatedAt.IsZero() || principal.AuthenticatedAt.After(now.Add(time.Minute)) || now.Sub(principal.AuthenticatedAt) > rechargeProductRecentAuthentication {
			return authorization.ErrPermissionDenied
		}
		current, err := s.authorizer.AuthorizedUserID(ctx, principal, "platform.products.publish")
		if err != nil {
			return err
		}
		if current != actor {
			return authorization.ErrPermissionDenied
		}
		return nil
	}
}

func (s *Service) productAdmissionActor(r *http.Request) (uuid.UUID, error) {
	principal, valid := identity.FromContext(r.Context())
	if !valid || s.authorizer == nil {
		return uuid.Nil, authorization.ErrPermissionDenied
	}
	actor, err := s.authorizer.AuthorizedUserID(r.Context(), principal, "platform.products.publish")
	if err == nil {
		err = s.productAdmissionGuard(r, actor)(r.Context())
	}
	return actor, err
}

func (s *Service) productAdmissionMaterialError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, authorization.ErrPermissionDenied) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "current product publishing permission and recent authentication are required", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, errImageValidatorInvalid) || errors.Is(err, errEvidencePDFPasswordRequired) {
		api.WriteError(w, http.StatusUnprocessableEntity, "INVALID_ADMISSION_MATERIAL", "upload a viewable static PNG, JPEG or PDF original without validation warnings", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, errImageValidatorUnavailable) || errors.Is(err, evidenceenvelope.ErrInvalid) {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMISSION_MATERIAL_UNAVAILABLE", "product admission material protection is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	s.writeError(w, r, err)
}

func materialPathIDs(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	catalogID, err := uuid.Parse(r.PathValue("catalog_item_id"))
	if err != nil || catalogID == uuid.Nil {
		return uuid.Nil, uuid.Nil, ErrConflict
	}
	materialID, err := uuid.Parse(r.PathValue("material_id"))
	if err != nil || materialID == uuid.Nil {
		return uuid.Nil, uuid.Nil, ErrConflict
	}
	return catalogID, materialID, nil
}

func (s *Service) adminUploadProductAdmissionMaterial(w http.ResponseWriter, r *http.Request) {
	if s.admissionMaterials == nil || s.admissionMaterialKeys == nil || s.admissionMaterialValidator == nil {
		s.productAdmissionMaterialError(w, r, errImageValidatorUnavailable)
		return
	}
	actor, err := s.productAdmissionActor(r)
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	catalogID, err := uuid.Parse(r.PathValue("catalog_item_id"))
	version, versionErr := strconv.ParseInt(r.URL.Query().Get("expected_version"), 10, 64)
	expiry, expiryErr := time.Parse(time.RFC3339Nano, r.URL.Query().Get("intended_expires_at"))
	media, params, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if err != nil || catalogID == uuid.Nil || versionErr != nil || version < 1 || expiryErr != nil || !validMaterialKey(key) || mediaErr != nil || len(params) != 0 || (media != "image/png" && media != "image/jpeg" && media != "application/pdf") {
		s.writeError(w, r, ErrConflict)
		return
	}
	// Read exactly one bounded original. Its digest is computed by Seal, never
	// taken from a client header. Even a replay must match the original facts.
	body, err := io.ReadAll(io.LimitReader(r.Body, evidenceenvelope.MaxPlaintextBytes+1))
	defer clear(body)
	if err != nil || len(body) == 0 || len(body) > evidenceenvelope.MaxPlaintextBytes {
		api.WriteError(w, http.StatusRequestEntityTooLarge, "INVALID_ADMISSION_MATERIAL", "original must be between one byte and 10 MiB", httpserver.RequestID(r.Context()))
		return
	}
	if err = s.admissionMaterialValidator.Validate(r.Context(), media, body); err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	materialID := uuid.New()
	envelope, err := evidenceenvelope.Seal(r.Context(), s.admissionMaterialKeys, evidenceenvelope.Binding{Purpose: "product-admission", OwnerID: catalogID, DocumentID: materialID, Version: 1, MediaType: media}, body)
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	result, err := s.admissionMaterials.SaveProductAdmissionMaterial(r.Context(), ProductAdmissionMaterialSave{MaterialID: materialID, CatalogItemID: catalogID, CatalogVersion: version, Actor: actor, IdempotencyKey: key, IntendedExpiresAt: expiry.UTC(), Envelope: envelope}, s.productAdmissionGuard(r, actor))
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminGetProductAdmissionMaterial(w http.ResponseWriter, r *http.Request) {
	s.readProductAdmissionMaterial(w, r, false)
}

func (s *Service) adminDownloadProductAdmissionMaterial(w http.ResponseWriter, r *http.Request) {
	s.readProductAdmissionMaterial(w, r, true)
}

func (s *Service) readProductAdmissionMaterial(w http.ResponseWriter, r *http.Request, content bool) {
	if s.admissionMaterials == nil || (content && s.admissionMaterialKeys == nil) {
		s.productAdmissionMaterialError(w, r, errImageValidatorUnavailable)
		return
	}
	catalogID, materialID, err := materialPathIDs(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	actor, err := s.productAdmissionActor(r)
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	if !content {
		item, err := s.admissionMaterials.GetProductAdmissionMaterial(r.Context(), catalogID, materialID)
		if err == nil {
			err = s.productAdmissionGuard(r, actor)(r.Context())
		}
		if err != nil {
			s.productAdmissionMaterialError(w, r, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, item)
		return
	}
	item, envelope, err := s.admissionMaterials.GetProductAdmissionMaterialEnvelope(r.Context(), catalogID, materialID)
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	plaintext, err := evidenceenvelope.Open(r.Context(), s.admissionMaterialKeys, evidenceenvelope.Binding{Purpose: "product-admission", OwnerID: catalogID, DocumentID: materialID, Version: 1, MediaType: item.MediaType}, envelope)
	defer clear(plaintext)
	if err == nil {
		err = s.admissionMaterials.ConfirmProductAdmissionMaterialRead(r.Context(), catalogID, materialID, actor, s.productAdmissionGuard(r, actor))
	}
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	extension := map[string]string{"image/png": "png", "image/jpeg": "jpg", "application/pdf": "pdf"}[item.MediaType]
	w.Header().Set("Content-Type", item.MediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="product-admission-%s.%s"`, materialID, extension))
	w.Header().Set("Content-Length", strconv.Itoa(len(plaintext)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(plaintext)
	}
}

func (s *Service) adminProductAdmissionMaterialReceipt(w http.ResponseWriter, r *http.Request) {
	if s.admissionMaterials == nil {
		s.productAdmissionMaterialError(w, r, errImageValidatorUnavailable)
		return
	}
	catalogID, err := uuid.Parse(r.PathValue("catalog_item_id"))
	if err != nil || catalogID == uuid.Nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	actor, err := s.productAdmissionActor(r)
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	result, err := s.admissionMaterials.GetProductAdmissionMaterialReceipt(r.Context(), catalogID, actor, r.PathValue("operation_key"))
	if err == nil {
		err = s.productAdmissionGuard(r, actor)(r.Context())
	}
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminHoldProductAdmissionMaterial(w http.ResponseWriter, r *http.Request) {
	if s.admissionMaterials == nil {
		s.productAdmissionMaterialError(w, r, errImageValidatorUnavailable)
		return
	}
	catalogID, materialID, err := materialPathIDs(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	actor, err := s.productAdmissionActor(r)
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	var body struct {
		ExpectedVersion int64      `json:"expected_version"`
		Hold            bool       `json:"hold"`
		Reason          string     `json:"reason"`
		ReviewAt        *time.Time `json:"review_at"`
	}
	if err = api.DecodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, ErrConflict)
		return
	}
	result, err := s.admissionMaterials.SetProductAdmissionMaterialLegalHold(r.Context(), ProductAdmissionMaterialHold{CatalogItemID: catalogID, MaterialID: materialID, Actor: actor, ExpectedVersion: body.ExpectedVersion, Hold: body.Hold, Reason: body.Reason, ReviewAt: body.ReviewAt, IdempotencyKey: r.Header.Get("Idempotency-Key")}, s.productAdmissionGuard(r, actor))
	if err != nil {
		s.productAdmissionMaterialError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
