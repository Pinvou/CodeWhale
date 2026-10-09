package token

import (
	"context"
	"github.com/google/uuid"
)

func (p *PostgresDefaultRouteProvisioner) ValidateContextModelPolicy(ctx context.Context, resolved AuthorizationContext, modelID uuid.UUID) error {
	if resolved.UserID == uuid.Nil || modelID == uuid.Nil {
		return ErrModelPolicyDenied
	}
	if resolved.ContextType == "PERSONAL" && resolved.OrganizationID == nil {
		return nil
	}
	if resolved.ContextType != "ORGANIZATION" || resolved.OrganizationID == nil || !hasPermission(resolved, "org.model.invoke") {
		return ErrModelPolicyDenied
	}
	allowed, err := p.repository.OrganizationModelAllowed(ctx, *resolved.OrganizationID, modelID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrModelPolicyDenied
	}
	return nil
}
