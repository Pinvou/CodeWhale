package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

const (
	maximumCatalogCandidates = 16_384
	maximumCatalogModels     = 4_096
	maximumCatalogBytes      = 2 << 20
)

// Each row describes only its actual route model. A credential's other allowed
// models must not inherit this route's health or authorization projection.
func (r *PostgresRepository) ListModelCandidates(ctx context.Context, resolved AuthorizationContext) ([]Candidate, error) {
	rows, err := r.pool.Query(ctx, `
SELECT ta.id,ta.name,ta.owner_type,ta.owner_id,ta.owner_organization_id,ta.owner_unit_id,ta.wallet_id,ta.status,
       sp.scope,ARRAY[rp.model],
       ARRAY(SELECT em.user_id FROM token_explicit_members em WHERE em.token_account_id=ta.id ORDER BY em.user_id),
       rp.default_scope,rp.priority,rp.health_status,pc.id
FROM token_accounts ta
JOIN token_share_policies sp ON sp.token_account_id=ta.id
JOIN provider_credentials pc ON pc.token_account_id=ta.id AND pc.status='ACTIVE' AND pc.validation_status='VALID'
JOIN routing_policies rp ON rp.token_account_id=ta.id AND rp.model=ANY(pc.allowed_models)
WHERE ta.status='ACTIVE' AND
      (($1='PERSONAL' AND ta.owner_type='USER' AND ta.owner_id=$2) OR
       ($1='ORGANIZATION' AND ta.owner_type<>'USER' AND ta.owner_organization_id=$3))
ORDER BY rp.model,ta.id,pc.id
LIMIT 16385`, resolved.ContextType, resolved.UserID, resolved.OrganizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Candidate, 0)
	for rows.Next() {
		var item Candidate
		if err := rows.Scan(&item.TokenAccountID, &item.Name, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.OwnerUnit, &item.WalletID, &item.Status, &item.ShareScope, &item.AllowedModels, &item.ExplicitMembers, &item.DefaultScope, &item.Priority, &item.HealthStatus, &item.CredentialID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func defaultRouteOwner(resolved AuthorizationContext) (string, uuid.UUID, error) {
	if resolved.UserID == uuid.Nil {
		return "", uuid.Nil, ErrNoEligibleRoute
	}
	if resolved.ContextType == "PERSONAL" {
		return "USER", resolved.UserID, nil
	}
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		return "ORG", *resolved.OrganizationID, nil
	}
	return "", uuid.Nil, ErrNoEligibleRoute
}

// Before first use, Ensure may provision the configured platform account. Once
// ANY platform account exists, Ensure leaves it to its administrator: disabled
// accounts, revoked credentials and deleted routes must never be resurrected by
// a model-list request. This read-only projection uses that same owner identity.
func (p *PostgresDefaultRouteProvisioner) DiscoverableModels(ctx context.Context, resolved AuthorizationContext) ([]string, error) {
	ownerType, ownerID, err := defaultRouteOwner(resolved)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := p.repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM token_accounts WHERE owner_type=$1 AND owner_id=$2 AND management_mode='PLATFORM')`, ownerType, ownerID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return []string{}, nil
	}
	return slices.Clone(p.config.Models), nil
}

type defaultModelDiscovery interface {
	DiscoverableModels(context.Context, AuthorizationContext) ([]string, error)
}

// Model discovery is a permission projection, not billing or route selection.
// Execution still checks the current context, route and credit independently.
func (s *Service) listModels(w http.ResponseWriter, r *http.Request) {
	unavailable := func() {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_CATALOG_UNAVAILABLE", "the authorized model catalog is unavailable", httpserver.RequestID(r.Context()))
	}
	resolved, err := s.contexts.Resolve(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			s.writeError(w, r, err)
		} else {
			unavailable()
		}
		return
	}
	candidates, err := s.repository.ListModelCandidates(r.Context(), resolved)
	if err != nil || len(candidates) > maximumCatalogCandidates {
		unavailable()
		return
	}
	models := make(map[string]struct{})
	type unitKey struct{ organization, unit uuid.UUID }
	unitStatus := make(map[unitKey]bool)
	for _, candidate := range candidates {
		eligibleModels := make([]string, 0, len(candidate.AllowedModels))
		for _, model := range candidate.AllowedModels {
			if model != "" && Eligible(resolved, candidate, model) {
				eligibleModels = append(eligibleModels, model)
			}
		}
		if len(eligibleModels) == 0 {
			continue
		}
		if candidate.OwnerType == "ORG_UNIT" {
			if candidate.OwnerOrganization == nil || candidate.OwnerUnit == nil {
				continue
			}
			key := unitKey{*candidate.OwnerOrganization, *candidate.OwnerUnit}
			active, checked := unitStatus[key]
			if !checked {
				resolver, ok := s.contexts.(unitOwnershipResolver)
				if !ok {
					unavailable()
					return
				}
				active, err = resolver.UnitActive(r.Context(), r.Header.Get("Authorization"), key.organization, key.unit)
				if err != nil {
					unavailable()
					return
				}
				unitStatus[key] = active
			}
			if !active {
				continue
			}
		}
		for _, model := range eligibleModels {
			models[model] = struct{}{}
		}
	}
	if discovery, ok := s.defaultRoutes.(defaultModelDiscovery); ok {
		defaults, err := discovery.DiscoverableModels(r.Context(), resolved)
		if err != nil {
			unavailable()
			return
		}
		for _, model := range defaults {
			models[model] = struct{}{}
		}
	}
	if len(models) > maximumCatalogModels {
		unavailable()
		return
	}
	ids := make([]string, 0, len(models))
	for model := range models {
		ids = append(ids, model)
	}
	slices.Sort(ids)
	type entry struct {
		ID string `json:"id"`
	}
	data := make([]entry, 0, len(ids))
	for _, model := range ids {
		data = append(data, entry{ID: model})
	}
	payload, err := json.Marshal(struct {
		Data []entry `json:"data"`
	}{Data: data})
	if err != nil || len(payload) > maximumCatalogBytes {
		unavailable()
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}
