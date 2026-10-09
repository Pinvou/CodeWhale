package token

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"

	"encoding/json"
	"errors"

	"io"
	"net/http"
	"net/url"

	"strings"

	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"

	"github.com/gin-gonic/gin"
)

type CredentialValidator interface {
	Validate(context.Context, string, []byte, []string) error
}

// ManagementCatalogAdapter describes source discovery and supported protocols;
// it provides no invocation or personal usage entry point.
type ManagementCatalogAdapter interface {
	CatalogSourceAdapter
	Protocols() []GatewayProtocolCapability
}

func (s *Service) WithCatalogAdapter(adapter ManagementCatalogAdapter) *Service {
	s.upstream = adapter
	s.cancellationAdapter, _ = adapter.(interface {
		CancelCapability() ProviderCancelCapability
	})
	return s
}

// WithTokenAccountCreateCapability is an APT-138 assembly seam. Production
// constructors intentionally never call it while governance is PRE_ACTIVATION.
func (s *Service) WithTokenAccountCreateCapability(ready bool) *Service {
	s.tokenAccountCreateReady.Store(ready)
	if provisioner, ok := s.defaultRoutes.(*PostgresDefaultRouteProvisioner); ok {
		provisioner.WithTokenAccountCreateCapability(ready)
	}
	return s
}

func (s *Service) WithTokenAccountProvisioning(wallets WalletProvisioningClient, authority OwnerAuthorityClient) *Service {
	s.walletProvisioning = wallets
	s.ownerAuthority = authority
	return s
}

func (s *Service) requireTokenAccountCreateCapability() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.tokenAccountCreateReady.Load() {
			api.WriteError(c.Writer, http.StatusServiceUnavailable, "TOKEN_ACCOUNT_CREATE_NOT_ACTIVE", "Token Account creation is temporarily unavailable", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Service) WithDefaultRoutes(provisioner DefaultRouteProvisioner) *Service {
	s.defaultRoutes = provisioner
	if managed, ok := provisioner.(*PostgresDefaultRouteProvisioner); ok {
		managed.WithTokenAccountCreateCapability(s.tokenAccountCreateReady.Load())
	}
	return s
}

func (s *Service) WithCatalogSynchronizer(synchronizer *CatalogSynchronizer) *Service {
	s.catalogSync = synchronizer
	return s
}

func (s *Service) WithOwnerCandidateDirectory(directory OwnerCandidateDirectory) *Service {
	s.ownerCandidates = directory
	return s
}

func (s *Service) WithAdminProxyHMACKey(key []byte) *Service {
	s.adminProxyHMACKey = append([]byte(nil), key...)
	return s
}

func (s *Service) WithApprovedTokenCommandExecutor(executor ApprovedTokenCommandExecutor) *Service {
	s.approvedCommandExecutor = executor
	return s
}

func (s *Service) WithAdminOperationsOverview(enabled bool) *Service {
	s.operationsOverviewEnabled = enabled
	return s
}

func (s *Service) WithAdminPageOverviews(enabled bool) *Service {
	s.pageOverviewsEnabled = enabled
	return s
}

func (s *Service) WithCredentialIsolation(enabled bool) *Service {
	s.credentialIsolationEnabled = enabled
	return s
}

func (s *Service) requireCredentialIsolationEnabled() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if !s.credentialIsolationEnabled {
			api.WriteError(ctx.Writer, http.StatusServiceUnavailable, "CREDENTIAL_ISOLATION_UNAVAILABLE", "credential isolation is not enabled", httpserver.RequestID(ctx.Request.Context()))
			ctx.Abort()
			return
		}
		ctx.Next()
	}
}

type ProviderURLPolicy struct {
	environment  string
	allowedHosts map[string]bool
}

func NewProviderURLPolicy(environment string, allowedHosts []string) (ProviderURLPolicy, error) {
	policy := ProviderURLPolicy{environment: strings.ToLower(strings.TrimSpace(environment)), allowedHosts: map[string]bool{}}
	for _, host := range allowedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			policy.allowedHosts[host] = true
		}
	}
	if len(policy.allowedHosts) == 0 {
		return ProviderURLPolicy{}, errors.New("TOKEN_PROVIDER_ALLOWED_HOSTS must not be empty")
	}
	return policy, nil
}

func (p ProviderURLPolicy) Validate(rawURL string) error {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !p.allowedHosts[strings.ToLower(parsed.Hostname())] {
		return ErrInvalidSecret
	}
	if (p.environment == "production" || p.environment == "production-new") && parsed.Scheme != "https" {
		return ErrInvalidSecret
	}
	return nil
}

type HTTPModelValidator struct {
	client *http.Client
	policy ProviderURLPolicy
}

func NewHTTPModelValidator(policy ProviderURLPolicy) *HTTPModelValidator {
	return &HTTPModelValidator{client: observability.NewHTTPClient(10 * time.Second), policy: policy}
}

func (v *HTTPModelValidator) Validate(ctx context.Context, rawURL string, secret []byte, models []string) error {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || v.policy.Validate(rawURL) != nil || len(secret) == 0 || len(normalizeModels(models)) == 0 {
		return ErrInvalidSecret
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String()+"/v1/models", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	response, err := v.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrInvalidSecret
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return ErrInvalidSecret
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	available := map[string]bool{}
	for _, item := range payload.Data {
		available[item.ID] = true
	}
	for _, model := range normalizeModels(models) {
		if !available[model] {
			return ErrInvalidSecret
		}
	}
	return nil
}

// WithManagement attaches the complete management repository and explicit
// internal service authority. Personal finance continues to use NewService.
func (s *Service) WithManagement(repository Repository, validator CredentialValidator, policy ProviderURLPolicy, serviceToken string, verify identity.VerifyFunc) *Service {
	s.managementEnabled = true
	s.repository, s.validator, s.providerURLs = repository, validator, policy
	s.managementServiceToken, s.managementVerify = serviceToken, verify
	s.cancellationStore, _ = repository.(managementCancellationStore)
	s.cancellationBilling, _ = s.billing.(cancellationBillingClient)
	return s
}

// NewManagementService composes the current control plane with the retained
// personal finance facade. It does not configure an execution client.
func NewManagementService(repository Repository, contexts ContextResolver, wallets WalletResolver, validator CredentialValidator, policy ProviderURLPolicy, billing BillingClient, adapter ManagementCatalogAdapter) *Service {
	return NewService(contexts, wallets, billing).
		WithManagement(repository, validator, policy, "", nil).
		WithCatalogAdapter(adapter)
}

// ManagementHandler freezes server-owned authority before route construction.
func (s *Service) ManagementHandler(serviceToken string, verify identity.VerifyFunc) http.Handler {
	s.managementEnabled, s.managementServiceToken, s.managementVerify = true, serviceToken, verify
	return s.Handler(verify)
}
