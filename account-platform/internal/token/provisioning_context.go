package token

import (
	"context"

	"github.com/google/uuid"
)

// Personal route management is delivered before organization ownership. The
// current scope is explicit; no absent organization policy becomes an allow.
func (p *PostgresDefaultRouteProvisioner) ValidateContextModelPolicy(_ context.Context, resolved AuthorizationContext, modelID uuid.UUID) error {
	if resolved.ContextType != "PERSONAL" || resolved.OrganizationID != nil || resolved.UserID == uuid.Nil || modelID == uuid.Nil {
		return ErrModelPolicyDenied
	}
	return nil
}
