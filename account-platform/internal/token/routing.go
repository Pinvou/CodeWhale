package token

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

func Eligible(context AuthorizationContext, candidate Candidate, model string) bool {
	if candidate.Status != "ACTIVE" || candidate.HealthStatus == "UNHEALTHY" || candidate.CredentialID == uuid.Nil || candidate.WalletID == uuid.Nil || !slices.Contains(candidate.AllowedModels, model) {
		return false
	}
	if context.ContextType == "PERSONAL" {
		return candidate.OwnerType == "USER" && candidate.OwnerID == context.UserID
	}
	if context.ContextType != "ORGANIZATION" || context.OrganizationID == nil || candidate.OwnerOrganization == nil || *context.OrganizationID != *candidate.OwnerOrganization || candidate.OwnerType == "USER" {
		return false
	}
	switch candidate.ShareScope {
	case "ORG_ALL":
		return true
	case "EXPLICIT_MEMBERS":
		return slices.Contains(candidate.ExplicitMembers, context.UserID)
	case "OWNER_UNIT_ONLY":
		return candidate.OwnerUnit != nil && slices.Contains(context.UnitIDs, *candidate.OwnerUnit)
	case "OWNER_SUBTREE":
		return candidate.OwnerUnit != nil && slices.Contains(context.AncestorUnitIDs, *candidate.OwnerUnit)
	}
	return false
}

func SelectRoute(context AuthorizationContext, request RouteRequest, candidates []Candidate) (Candidate, error) {
	model := strings.TrimSpace(request.Model)
	protocolCode := strings.ToUpper(strings.TrimSpace(request.ProtocolCode))
	if protocolCode == "" {
		protocolCode = "CHAT"
	}
	if model == "" {
		return Candidate{}, ErrNoEligibleRoute
	}
	eligible := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if Eligible(context, candidate, model) && (candidate.ProtocolCode == "" || candidate.ProtocolCode == protocolCode) {
			eligible = append(eligible, candidate)
		}
	}
	if request.ExplicitTokenAccountID != nil {
		for _, candidate := range eligible {
			if candidate.TokenAccountID == *request.ExplicitTokenAccountID {
				return candidate, nil
			}
		}
		return Candidate{}, ErrForbidden
	}
	unitDefaults := make([]Candidate, 0)
	for _, candidate := range eligible {
		if candidate.DefaultScope == "UNIT" && candidate.OwnerUnit != nil && slices.Contains(context.UnitIDs, *candidate.OwnerUnit) {
			unitDefaults = append(unitDefaults, candidate)
		}
	}
	if len(unitDefaults) > 0 {
		return uniqueHighestPriority(unitDefaults)
	}
	organizationDefaults := make([]Candidate, 0)
	for _, candidate := range eligible {
		if candidate.DefaultScope == "ORG" {
			organizationDefaults = append(organizationDefaults, candidate)
		}
	}
	if len(organizationDefaults) > 0 {
		return uniqueHighestPriority(organizationDefaults)
	}
	return uniqueHighestPriority(eligible)
}

func uniqueHighestPriority(candidates []Candidate) (Candidate, error) {
	if len(candidates) == 0 {
		return Candidate{}, ErrNoEligibleRoute
	}
	best := candidates[0]
	ambiguous := false
	for _, candidate := range candidates[1:] {
		if candidate.Priority > best.Priority {
			best, ambiguous = candidate, false
		} else if candidate.Priority == best.Priority && candidate.TokenAccountID != best.TokenAccountID {
			ambiguous = true
		}
	}
	if ambiguous {
		return Candidate{}, ErrAmbiguousRoute
	}
	return best, nil
}
