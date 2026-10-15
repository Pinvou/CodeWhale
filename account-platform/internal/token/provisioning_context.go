package token

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var _ provisioningOperations = (*PostgresDefaultRouteProvisioner)(nil)

// Personal route management is delivered before organization ownership. The
// current scope is explicit; no absent organization policy becomes an allow.
func (p *PostgresDefaultRouteProvisioner) ValidateContextModelPolicy(_ context.Context, resolved AuthorizationContext, modelID uuid.UUID) error {
	if resolved.ContextType != "PERSONAL" || resolved.OrganizationID != nil || resolved.UserID == uuid.Nil || modelID == uuid.Nil {
		return ErrModelPolicyDenied
	}
	return nil
}

// ContextEligible is the discovery projection of the existing invocation route
// contract. It never creates wallets, provisions resources or reserves funds.
func (p *PostgresDefaultRouteProvisioner) ContextEligible(ctx context.Context, resolved AuthorizationContext, modelID uuid.UUID, protocol string) (bool, string, error) {
	scopeType, scopeID, err := provisioningScope(resolved)
	if err != nil {
		return false, "CONTEXT_INVALID", nil
	}
	if err := p.ValidateContextModelPolicy(ctx, resolved, modelID); err != nil {
		if errors.Is(err, ErrModelPolicyDenied) {
			return false, "MODEL_POLICY_DENIED", nil
		}
		return false, "POLICY_UNAVAILABLE", err
	}
	protocol = strings.ToUpper(strings.TrimSpace(protocol))
	model, err := scanCatalogModel(p.repository.pool.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=(SELECT max(version) FROM model_catalog_versions) AND e.model_id=$1 AND p.protocol_code=$2 AND e.lifecycle_status<>'REMOVED'`, modelID, protocol))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "ROUTE_PROVISIONING_DRIFTED", nil
	}
	if err != nil {
		return false, "ROUTE_PROVISIONING_UNAVAILABLE", err
	}
	if !model.SourceAvailable || model.ProviderID != p.config.Provider || model.AdapterVersion != p.config.AdapterVersion {
		return false, "ROUTE_PROVISIONING_DRIFTED", nil
	}
	item, err := scanProvisioning(p.repository.pool.QueryRow(ctx, provisioningSelect+` WHERE scope_type=$1 AND scope_id=$2 AND model_id=$3 AND protocol_code=$4 ORDER BY desired_generation DESC LIMIT 1`, scopeType, scopeID, modelID, protocol))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "ROUTE_PROVISIONING_REQUIRED", nil
	}
	if err != nil {
		return false, "ROUTE_PROVISIONING_UNAVAILABLE", err
	}
	if item.Status != "READY" {
		return false, "ROUTE_PROVISIONING_" + item.Status, nil
	}
	if item.ProtocolVersionID == nil || *item.ProtocolVersionID != model.ProtocolDefinitionID || item.ActualGeneration == nil || *item.ActualGeneration != item.DesiredGeneration || item.ActiveTokenAccountID == nil || item.ActiveCredentialID == nil {
		return false, "ROUTE_PROVISIONING_DRIFTED", nil
	}
	var walletID uuid.UUID
	var signature string
	if err := p.repository.pool.QueryRow(ctx, `SELECT wallet_id,desired_signature FROM route_provisionings WHERE id=$1`, item.ID).Scan(&walletID, &signature); err != nil {
		return false, "ROUTE_PROVISIONING_UNAVAILABLE", err
	}
	if walletID == uuid.Nil || signature != p.desiredSignature(scopeType, scopeID, walletID, model) {
		return false, "ROUTE_PROVISIONING_DRIFTED", nil
	}
	candidates, err := p.repository.ListCandidates(ctx, modelID.String(), model.ProtocolDefinitionID.String())
	if err != nil {
		return false, "ROUTE_PROVISIONING_UNAVAILABLE", err
	}
	route, err := SelectRoute(resolved, RouteRequest{Model: model.UpstreamModelRef, ProtocolCode: model.ProtocolCode}, candidates)
	if errors.Is(err, ErrNoEligibleRoute) || errors.Is(err, ErrAmbiguousRoute) {
		return false, "ROUTE_PROVISIONING_DRIFTED", nil
	}
	if err != nil {
		return false, "ROUTE_PROVISIONING_UNAVAILABLE", err
	}
	if route.TokenAccountID != *item.ActiveTokenAccountID || route.CredentialID != *item.ActiveCredentialID || route.WalletID != walletID || route.RouteGeneration != *item.ActualGeneration {
		return false, "ROUTE_PROVISIONING_DRIFTED", nil
	}
	return true, "READY", nil
}
