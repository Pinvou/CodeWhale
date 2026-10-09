package token

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type organizationModelPolicyRepository interface {
	GetOrganizationModelPolicy(context.Context, uuid.UUID) (OrganizationModelPolicy, error)
	SaveOrganizationModelPolicy(context.Context, uuid.UUID, uuid.UUID, int64, int64, string, []uuid.UUID, string, string) (OrganizationModelPolicy, error)
}

func (r *PostgresRepository) OrganizationModelAllowed(ctx context.Context, organizationID, modelID uuid.UUID) (bool, error) {
	var mode string
	var models []uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT mode,model_ids FROM organization_model_policies WHERE organization_id=$1`, organizationID).Scan(&mode, &models)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	contains := false
	for _, candidate := range models {
		if candidate == modelID {
			contains = true
			break
		}
	}
	return mode == "ALLOW_ALL" || mode == "ALLOW_LIST" && contains || mode == "DENY_LIST" && !contains, nil
}

func (r *PostgresRepository) GetOrganizationModelPolicy(ctx context.Context, organizationID uuid.UUID) (OrganizationModelPolicy, error) {
	var item OrganizationModelPolicy
	var actor uuid.UUID
	var updatedAt time.Time
	err := r.pool.QueryRow(ctx, `SELECT organization_id,version,mode,model_ids,authorization_version,updated_by,updated_at FROM organization_model_policies WHERE organization_id=$1`, organizationID).Scan(&item.OrganizationID, &item.Version, &item.Mode, &item.ModelIDs, &item.AuthorizationVersion, &actor, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrganizationModelPolicy{OrganizationID: organizationID, Version: 0, Mode: "ALLOW_ALL", ModelIDs: []uuid.UUID{}, AuthorizationVersion: 1}, nil
	}
	if err == nil {
		item.UpdatedBy, item.UpdatedAt = &actor, &updatedAt
	}
	return item, err
}

func (r *PostgresRepository) SaveOrganizationModelPolicy(ctx context.Context, organizationID, actor uuid.UUID, authorizationVersion, expectedVersion int64, mode string, modelIDs []uuid.UUID, key, reason string) (OrganizationModelPolicy, error) {
	mode, reason, key = strings.ToUpper(strings.TrimSpace(mode)), strings.TrimSpace(reason), strings.TrimSpace(key)
	if organizationID == uuid.Nil || actor == uuid.Nil || authorizationVersion < 1 || expectedVersion < 0 || reason == "" || len(reason) > 500 || len(key) < 16 || (mode != "ALLOW_ALL" && mode != "ALLOW_LIST" && mode != "DENY_LIST") {
		return OrganizationModelPolicy{}, ErrForbidden
	}
	unique := map[uuid.UUID]bool{}
	for _, id := range modelIDs {
		if id == uuid.Nil {
			return OrganizationModelPolicy{}, ErrForbidden
		}
		unique[id] = true
	}
	// Empty selections are valid for every mode. Normalize nil and [] to the
	// same non-null stored array and immutable request facts.
	modelIDs = make([]uuid.UUID, 0, len(unique))
	for id := range unique {
		modelIDs = append(modelIDs, id)
	}
	sort.Slice(modelIDs, func(i, j int) bool { return modelIDs[i].String() < modelIDs[j].String() })
	if mode == "ALLOW_ALL" && len(modelIDs) != 0 {
		return OrganizationModelPolicy{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return OrganizationModelPolicy{}, err
	}
	defer tx.Rollback(ctx)
	// Serialize the first policy as well as later row updates. An absent row
	// cannot provide the CAS fence used by concurrent initial requests.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "organization-model-policy:"+organizationID.String()); err != nil {
		return OrganizationModelPolicy{}, err
	}
	digest := provisioningDigest(map[string]any{"organization_id": organizationID, "actor_id": actor, "expected_version": expectedVersion, "mode": mode, "model_ids": modelIDs, "reason": reason, "authorization_version": authorizationVersion})
	var storedDigest string
	var resultVersion int64
	err = tx.QueryRow(ctx, `SELECT request_digest,result_version FROM organization_model_policy_commands WHERE organization_id=$1 AND idempotency_key=$2`, organizationID, key).Scan(&storedDigest, &resultVersion)
	if err == nil {
		if storedDigest != digest {
			return OrganizationModelPolicy{}, ErrIdempotencyConflict
		}
		item, getErr := getOrganizationPolicyVersionTx(ctx, tx, organizationID, resultVersion)
		if getErr != nil {
			return OrganizationModelPolicy{}, getErr
		}
		return item, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return OrganizationModelPolicy{}, err
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT version FROM organization_model_policies WHERE organization_id=$1 FOR UPDATE`, organizationID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		current, err = 0, nil
	}
	if err != nil || current != expectedVersion {
		return OrganizationModelPolicy{}, ErrModelPolicyVersionConflict
	}
	if len(modelIDs) > 0 {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM model_catalog_entries WHERE catalog_version=(SELECT max(version) FROM model_catalog_versions) AND lifecycle_status<>'REMOVED' AND model_id=ANY($1)`, modelIDs).Scan(&count); err != nil || count != len(modelIDs) {
			return OrganizationModelPolicy{}, ErrModelUnavailable
		}
	}
	now, version := r.now(), current+1
	_, err = tx.Exec(ctx, `INSERT INTO organization_model_policies(organization_id,version,mode,model_ids,authorization_version,updated_by,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id) DO UPDATE SET version=EXCLUDED.version,mode=EXCLUDED.mode,model_ids=EXCLUDED.model_ids,authorization_version=EXCLUDED.authorization_version,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`, organizationID, version, mode, modelIDs, authorizationVersion, actor, now)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO organization_model_policy_commands(organization_id,idempotency_key,request_digest,result_version,actor_id,created_at) VALUES($1,$2,$3,$4,$5,$6)`, organizationID, key, digest, version, actor, now)
	}
	if err == nil {
		snapshot, _ := json.Marshal(map[string]any{"mode": mode, "model_ids": modelIDs})
		_, err = tx.Exec(ctx, `INSERT INTO organization_model_policy_audit(id,organization_id,policy_version,actor_id,authorization_version,reason,snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), organizationID, version, actor, authorizationVersion, reason, snapshot, now)
	}
	if err != nil {
		return OrganizationModelPolicy{}, err
	}
	item, err := getOrganizationPolicyTx(ctx, tx, organizationID)
	if err != nil {
		return OrganizationModelPolicy{}, err
	}
	return item, tx.Commit(ctx)
}

func getOrganizationPolicyTx(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) (OrganizationModelPolicy, error) {
	var item OrganizationModelPolicy
	var actor uuid.UUID
	var updatedAt time.Time
	err := tx.QueryRow(ctx, `SELECT organization_id,version,mode,model_ids,authorization_version,updated_by,updated_at FROM organization_model_policies WHERE organization_id=$1`, organizationID).Scan(&item.OrganizationID, &item.Version, &item.Mode, &item.ModelIDs, &item.AuthorizationVersion, &actor, &updatedAt)
	if err == nil {
		item.UpdatedBy, item.UpdatedAt = &actor, &updatedAt
	}
	return item, err
}

func (s *Service) organizationModelPolicy(w http.ResponseWriter, r *http.Request) {
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	principal, authenticated := identity.FromContext(r.Context())
	permission := "org.models.read"
	if r.Method == http.MethodPut {
		permission = "org.models.manage"
	}
	if err != nil || !authenticated || s.authorizer == nil {
		s.writeError(w, r, ErrForbidden)
		return
	}
	repository, ok := s.repository.(organizationModelPolicyRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_POLICY_UNAVAILABLE", "organization model policy is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	actor, authorizationVersion, err := s.authorizer.AuthorizedOrganizationUserID(r.Context(), principal, permission, organizationID)
	if errors.Is(err, authorization.ErrPermissionDenied) {
		s.writeError(w, r, ErrForbidden)
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "organization authorization is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("X-Authorization-Version", strconv.FormatInt(authorizationVersion, 10))
	if r.Method == http.MethodGet {
		item, err := repository.GetOrganizationModelPolicy(r.Context(), organizationID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, item)
		return
	}
	var body struct {
		ExpectedVersion int64       `json:"expected_version"`
		Mode            string      `json:"mode"`
		ModelIDs        []uuid.UUID `json:"model_ids"`
		Reason          string      `json:"reason"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization model policy", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.SaveOrganizationModelPolicy(r.Context(), organizationID, actor, authorizationVersion, body.ExpectedVersion, body.Mode, body.ModelIDs, r.Header.Get("Idempotency-Key"), body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}
