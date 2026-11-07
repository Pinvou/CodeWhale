package token

import (
	"context"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
)

// A process-local breaker cannot rely on winning a shared database health
// lease. An already rejected, authenticated request may provide one bounded
// read-only observation for its own selected key. It remains rejected even if
// this observation closes the circuit; no paid call or hold is retried here.
func (s *Service) probeDispatchCircuit(ctx context.Context, credential Credential, model CatalogModel) {
	if ctx.Err() != nil {
		return
	}
	adapter, ok := s.executionUpstream.(RouteHealthProbeAdapter)
	if !ok || adapter.AdapterVersion() != model.AdapterVersion || !strings.EqualFold(credential.Provider, model.ProviderID) || model.UpstreamModelRef == "" || s.providerURLs.Validate(credential.BaseURL) != nil {
		return
	}
	permit, err := s.breaker.AdmitReadOnlyRecovery(resilience.Key(credential.Provider, "MODEL_DISPATCH", credential.BaseURL))
	if err != nil || permit == nil {
		return
	}
	defer permit.Cancel()
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	guard, ok := s.repository.(interface {
		dispatchCircuitCredentialCurrent(context.Context, Credential, CatalogModel) (bool, error)
	})
	if !ok {
		return
	}
	current, err := guard.dispatchCircuitCredentialCurrent(probeCtx, credential, model)
	if err != nil || !current {
		return
	}
	fresh, secret, err := s.repository.CredentialSecret(probeCtx, credential.TokenAccountID, credential.ID)
	defer clear(secret)
	if err != nil || len(secret) == 0 || fresh.ID != credential.ID || fresh.TokenAccountID != credential.TokenAccountID || fresh.Version != credential.Version || fresh.Provider != credential.Provider || fresh.BaseURL != credential.BaseURL || fresh.Fingerprint != credential.Fingerprint {
		return
	}
	models, err := adapter.DiscoverModels(probeCtx, credential.BaseURL, secret)
	if ctx.Err() != nil {
		return
	}
	if probeCtx.Err() == context.DeadlineExceeded {
		// The admitted read exhausted its own budget without a trustworthy
		// result. It cannot recover; retain unknown-outcome backoff instead of
		// attempting an authority query on an already exhausted context.
		permit.Record(resilience.UncertainResult)
		return
	}
	current, guardErr := guard.dispatchCircuitCredentialCurrent(probeCtx, credential, model)
	if guardErr != nil || !current {
		return
	}
	if err == nil && containsDiscoveredModel(models, model.UpstreamModelRef) {
		permit.Record(resilience.Success)
	} else {
		permit.Record(resilience.InfrastructureFailure)
	}
}

// Observe current credential/route authority before and after the health read.
// This uses ordinary runtime SELECT rights and never clears a financial hold.
func (r *PostgresRepository) dispatchCircuitCredentialCurrent(ctx context.Context, credential Credential, model CatalogModel) (bool, error) {
	var current bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_credentials credential
 JOIN token_accounts account ON account.id=credential.token_account_id
 JOIN routing_policies route ON route.token_account_id=account.id
 WHERE credential.id=$1 AND credential.token_account_id=$2 AND credential.version=$3
 AND credential.provider=$4 AND credential.base_url=$5 AND credential.fingerprint=$6
 AND credential.status='ACTIVE' AND credential.validation_status='VALID' AND $7=ANY(credential.allowed_models)
 AND account.status='ACTIVE' AND route.model_id=$8 AND route.reference_status='RESOLVED'
 AND route.health_status='HEALTHY' AND route.health_expires_at>clock_timestamp() AND NOT `+tokenRouteFinancialHoldSQL+`)`, credential.ID, credential.TokenAccountID, credential.Version, credential.Provider, credential.BaseURL, credential.Fingerprint, model.UpstreamModelRef, model.ModelID).Scan(&current)
	return current, err
}
