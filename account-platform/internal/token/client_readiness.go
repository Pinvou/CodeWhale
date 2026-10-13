package token

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

const defaultClientReadinessContract = "client-readiness-v1"

var (
	artifactDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type ClientReadinessCheck struct {
	Code    string `json:"code"`
	Status  string `json:"status"`
	Reason  string `json:"reason_code"`
	Version string `json:"version,omitempty"`
}

type ClientReadinessResult struct {
	Ready                  bool                   `json:"ready"`
	Environment            string                 `json:"environment"`
	ServiceVersion         string                 `json:"service_version"`
	ContractVersion        string                 `json:"contract_version"`
	AccountContractVersion string                 `json:"account_contract_version,omitempty"`
	CatalogVersion         int64                  `json:"catalog_version"`
	ProtocolCode           string                 `json:"protocol_code"`
	CheckedAt              time.Time              `json:"checked_at"`
	Checks                 []ClientReadinessCheck `json:"checks"`
}

type ClientCompatibility struct {
	Environment            string                 `json:"environment"`
	ServiceVersion         string                 `json:"service_version"`
	ContractVersion        string                 `json:"contract_version"`
	APIContractVersion     string                 `json:"api_contract_version"`
	CurrentClient          string                 `json:"current_client_version"`
	MinimumClient          string                 `json:"minimum_client_version"`
	MinimumSupportedClient string                 `json:"minimum_supported_client_version"`
	MaximumClient          string                 `json:"maximum_client_version"`
	SunsetAt               string                 `json:"sunset_at,omitempty"`
	UpgradeURL             string                 `json:"upgrade_url"`
	Releases               []clientcompat.Release `json:"releases"`
	AttestationKeyID       string                 `json:"attestation_key_id,omitempty"`
	VerificationKey        string                 `json:"attestation_verification_key_base64url,omitempty"`
	AttestationReady       bool                   `json:"attestation_available"`
	GeneratedAt            string                 `json:"generated_at"`
}

type ClientReadinessAttestationClaims struct {
	Version                int      `json:"version"`
	AttestationID          string   `json:"attestation_id"`
	Environment            string   `json:"environment"`
	ArtifactSHA256         string   `json:"artifact_sha256"`
	ClientVersion          string   `json:"client_version"`
	ServiceVersion         string   `json:"service_version"`
	ContractVersion        string   `json:"contract_version"`
	AccountContractVersion string   `json:"account_contract_version"`
	CatalogVersion         int64    `json:"catalog_version"`
	ProtocolCode           string   `json:"protocol_code"`
	CheckCodes             []string `json:"check_codes"`
	IssuedAt               string   `json:"issued_at"`
	ExpiresAt              string   `json:"expires_at"`
}

type ClientReadinessAttestation struct {
	KeyID     string                           `json:"key_id"`
	Algorithm string                           `json:"algorithm"`
	Claims    ClientReadinessAttestationClaims `json:"claims"`
	Signature string                           `json:"signature_base64url"`
}

type clientCompatibilityResponse struct {
	Service         string `json:"service"`
	ServiceVersion  string `json:"service_version"`
	ContractVersion string `json:"contract_version"`
}

type ClientReadinessConfig struct {
	Environment     string
	ServiceVersion  string
	ContractVersion string
	MinimumClient   string
	MaximumClient   string
	AccountURL      string
	OIDCIssuer      string
	ServiceToken    string
	SigningKeyID    string
	SigningKey      ed25519.PrivateKey
	AttestationTTL  time.Duration
	Compatibility   clientcompat.Manifest
}

type ClientReadinessService struct {
	config  ClientReadinessConfig
	catalog interface {
		ListCatalog(context.Context) (int64, []CatalogModel, error)
	}
	routes interface {
		ValidateModelReady(context.Context, uuid.UUID, string) error
	}
	billing interface {
		Price(context.Context, string) (PriceVersion, error)
	}
	accountURL *url.URL
	oidcURL    *url.URL
	client     *http.Client
	now        func() time.Time
	record     func(context.Context, ClientReadinessAttestation, []byte) error
}

func DecodeClientReadinessSigningKey(encoded string) (ed25519.PrivateKey, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, errors.New("client readiness signing key must be base64")
	}
	if len(raw) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(raw), nil
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("client readiness signing key must contain %d-byte seed or %d-byte private key", ed25519.SeedSize, ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(append([]byte(nil), raw...)), nil
}

func NewClientReadinessService(config ClientReadinessConfig, catalog interface {
	ListCatalog(context.Context) (int64, []CatalogModel, error)
}, routes interface {
	ValidateModelReady(context.Context, uuid.UUID, string) error
}, billing interface {
	Price(context.Context, string) (PriceVersion, error)
}, recorder interface {
	RecordClientReadinessAttestation(context.Context, ClientReadinessAttestation, []byte) error
}) (*ClientReadinessService, error) {
	config.Environment = strings.TrimSpace(config.Environment)
	config.ServiceVersion = strings.TrimSpace(config.ServiceVersion)
	config.ContractVersion = strings.TrimSpace(config.ContractVersion)
	config.MinimumClient = strings.TrimSpace(config.MinimumClient)
	config.MaximumClient = strings.TrimSpace(config.MaximumClient)
	config.ServiceToken = strings.TrimSpace(config.ServiceToken)
	config.SigningKeyID = strings.TrimSpace(config.SigningKeyID)
	config.AccountURL = strings.TrimRight(strings.TrimSpace(config.AccountURL), "/")
	config.OIDCIssuer = strings.TrimRight(strings.TrimSpace(config.OIDCIssuer), "/")
	if config.ContractVersion == "" {
		config.ContractVersion = defaultClientReadinessContract
	}
	if config.AttestationTTL <= 0 || config.AttestationTTL > 15*time.Minute {
		config.AttestationTTL = 10 * time.Minute
	}
	accountURL, accountErr := url.Parse(config.AccountURL)
	oidcURL, oidcErr := url.Parse(config.OIDCIssuer)
	if accountErr != nil || oidcErr != nil || accountURL.Scheme == "" || accountURL.Host == "" || oidcURL.Scheme == "" || oidcURL.Host == "" || config.Environment == "" || config.ServiceToken == "" || catalog == nil || routes == nil || billing == nil {
		return nil, errors.New("client readiness dependencies are incomplete")
	}
	if len(config.SigningKey) != 0 && (len(config.SigningKey) != ed25519.PrivateKeySize || config.SigningKeyID == "") {
		return nil, errors.New("client readiness signing key and key ID must be configured together")
	}
	service := &ClientReadinessService{config: config, catalog: catalog, routes: routes, billing: billing, accountURL: accountURL, oidcURL: oidcURL, client: observability.NewHTTPClient(5 * time.Second), now: func() time.Time { return time.Now().UTC() }}
	if recorder != nil {
		service.record = recorder.RecordClientReadinessAttestation
	}
	return service, nil
}

func (s *ClientReadinessService) Compatibility() ClientCompatibility {
	manifest := s.config.Compatibility
	result := ClientCompatibility{Environment: s.config.Environment, ServiceVersion: s.config.ServiceVersion, ContractVersion: s.config.ContractVersion, APIContractVersion: manifest.APIContractVersion, CurrentClient: manifest.CurrentClientVersion, MinimumClient: manifest.MinimumSupportedClientVersion, MinimumSupportedClient: manifest.MinimumSupportedClientVersion, MaximumClient: manifest.CurrentClientVersion, SunsetAt: manifest.SunsetAt, UpgradeURL: manifest.UpgradeURL, Releases: manifest.Releases, GeneratedAt: s.now().Format(time.RFC3339)}
	if result.MinimumClient == "" {
		result.MinimumClient, result.MinimumSupportedClient, result.MaximumClient = s.config.MinimumClient, s.config.MinimumClient, s.config.MaximumClient
	}
	if len(s.config.SigningKey) == ed25519.PrivateKeySize {
		result.AttestationKeyID = s.config.SigningKeyID
		result.VerificationKey = base64.RawURLEncoding.EncodeToString(s.config.SigningKey.Public().(ed25519.PublicKey))
		result.AttestationReady = true
	}
	return result
}

func (s *ClientReadinessService) Check(ctx context.Context, protocol string) ClientReadinessResult {
	protocol = strings.ToUpper(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "CHAT"
	}
	now := s.now()
	result := ClientReadinessResult{Ready: true, Environment: s.config.Environment, ServiceVersion: s.config.ServiceVersion, ContractVersion: s.config.ContractVersion, ProtocolCode: protocol, CheckedAt: now, Checks: make([]ClientReadinessCheck, 0, 6)}
	add := func(code string, ok bool, reason, version string) {
		status := "PASS"
		if ok {
			reason = "READY"
		} else {
			status, result.Ready = "FAIL", false
		}
		result.Checks = append(result.Checks, ClientReadinessCheck{Code: code, Status: status, Reason: reason, Version: version})
	}
	add("GATEWAY_CONTRACT", s.config.ServiceVersion != "" && s.config.ContractVersion != "", "GATEWAY_CONTRACT_UNAVAILABLE", s.config.ServiceVersion+"/"+s.config.ContractVersion)
	account, err := s.accountCompatibility(ctx)
	if err != nil {
		add("ACCOUNT_CONTRACT", false, "ACCOUNT_CONTRACT_UNAVAILABLE", "")
	} else {
		result.AccountContractVersion = account.ContractVersion
		add("ACCOUNT_CONTRACT", true, "READY", account.ServiceVersion+"/"+account.ContractVersion)
	}
	oidcVersion, err := s.oidcMetadata(ctx)
	add("OIDC_METADATA", err == nil, mapReason(err, "OIDC_METADATA_UNAVAILABLE"), oidcVersion)
	version, models, err := s.catalog.ListCatalog(ctx)
	if err != nil {
		add("MODEL_CATALOG", false, "MODEL_CATALOG_UNAVAILABLE", "")
		add("MANAGED_ROUTE", false, "MANAGED_ROUTE_UNCHECKED", "")
		add("PRICING_FORMULA", false, "PRICING_FORMULA_UNCHECKED", "")
		return result
	}
	result.CatalogVersion = version
	visible := make([]CatalogModel, 0, len(models))
	for _, model := range models {
		if model.SourceAvailable && model.ProtocolCode == protocol && model.LifecycleStatus != "REMOVED" {
			visible = append(visible, model)
		}
	}
	add("MODEL_CATALOG", version > 0 && len(visible) > 0, "MODEL_CATALOG_EMPTY", fmt.Sprint(version))
	readyRoute, priced := false, false
	for _, model := range visible {
		if s.routes.ValidateModelReady(ctx, model.ModelID, protocol) != nil {
			continue
		}
		readyRoute = true
		price, priceErr := s.billing.Price(ctx, model.ModelID.String())
		if priceErr == nil && ValidatePriceFormula(price) == nil {
			priced = true
			break
		}
	}
	add("MANAGED_ROUTE", readyRoute, "MANAGED_ROUTE_NOT_READY", "")
	add("PRICING_FORMULA", priced, "PRICING_FORMULA_INCOMPLETE", "")
	return result
}

func mapReason(err error, failure string) string {
	if err != nil {
		return failure
	}
	return "READY"
}

func (s *ClientReadinessService) accountCompatibility(ctx context.Context) (clientCompatibilityResponse, error) {
	endpoint := *s.accountURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/client-compatibility"
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err := serviceauth.SetAuthorization(request, s.config.ServiceToken); err != nil {
		return clientCompatibilityResponse{}, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return clientCompatibilityResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return clientCompatibilityResponse{}, fmt.Errorf("account compatibility returned HTTP %d", response.StatusCode)
	}
	var result clientCompatibilityResponse
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result) != nil || result.Service != "account" || result.ServiceVersion == "" || result.ContractVersion == "" {
		return clientCompatibilityResponse{}, errors.New("account compatibility response is invalid")
	}
	return result, nil
}

func (s *ClientReadinessService) oidcMetadata(ctx context.Context) (string, error) {
	endpoint := *s.oidcURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/.well-known/openid-configuration"
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	response, err := s.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var metadata struct {
		Issuer  string `json:"issuer"`
		JWKSURL string `json:"jwks_uri"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&metadata) != nil || strings.TrimRight(metadata.Issuer, "/") != strings.TrimRight(s.config.OIDCIssuer, "/") || metadata.JWKSURL == "" {
		return "", errors.New("OIDC metadata response is invalid")
	}
	return metadata.Issuer, nil
}

func (s *ClientReadinessService) Issue(ctx context.Context, artifactSHA256, clientVersion, environment, protocol string) (ClientReadinessAttestation, error) {
	artifactSHA256, environment = strings.ToLower(strings.TrimSpace(artifactSHA256)), strings.TrimSpace(environment)
	if !artifactDigestPattern.MatchString(artifactSHA256) || !clientcompat.ValidVersion(clientVersion) || environment != s.config.Environment || len(s.config.SigningKey) != ed25519.PrivateKeySize || s.record == nil {
		return ClientReadinessAttestation{}, ErrForbidden
	}
	result := s.Check(ctx, protocol)
	if !result.Ready {
		return ClientReadinessAttestation{}, ErrNoEligibleRoute
	}
	issuedAt, expiresAt := s.now(), s.now().Add(s.config.AttestationTTL)
	checkCodes := make([]string, 0, len(result.Checks))
	for _, check := range result.Checks {
		checkCodes = append(checkCodes, check.Code+":"+check.Status)
	}
	claims := ClientReadinessAttestationClaims{Version: 1, AttestationID: uuid.Must(uuid.NewV7()).String(), Environment: environment, ArtifactSHA256: artifactSHA256, ClientVersion: clientVersion, ServiceVersion: result.ServiceVersion, ContractVersion: result.ContractVersion, AccountContractVersion: result.AccountContractVersion, CatalogVersion: result.CatalogVersion, ProtocolCode: result.ProtocolCode, CheckCodes: checkCodes, IssuedAt: issuedAt.Format(time.RFC3339Nano), ExpiresAt: expiresAt.Format(time.RFC3339Nano)}
	payload, _ := json.Marshal(claims)
	attestation := ClientReadinessAttestation{KeyID: s.config.SigningKeyID, Algorithm: "Ed25519", Claims: claims, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.config.SigningKey, payload))}
	checkSnapshot, _ := json.Marshal(result.Checks)
	if err := s.record(ctx, attestation, checkSnapshot); err != nil {
		return ClientReadinessAttestation{}, err
	}
	return attestation, nil
}

func (r *PostgresRepository) RecordClientReadinessAttestation(ctx context.Context, attestation ClientReadinessAttestation, checks []byte) error {
	payload, _ := json.Marshal(attestation.Claims)
	digest := sha256.Sum256(payload)
	issuedAt, issuedErr := time.Parse(time.RFC3339Nano, attestation.Claims.IssuedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, attestation.Claims.ExpiresAt)
	id, idErr := uuid.Parse(attestation.Claims.AttestationID)
	if issuedErr != nil || expiresErr != nil || idErr != nil || !json.Valid(checks) {
		return ErrForbidden
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO client_readiness_attestations(id,environment,artifact_sha256,client_version,service_version,contract_version,account_contract_version,catalog_version,protocol_code,check_snapshot,claims_sha256,key_id,issued_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, id, attestation.Claims.Environment, attestation.Claims.ArtifactSHA256, attestation.Claims.ClientVersion, attestation.Claims.ServiceVersion, attestation.Claims.ContractVersion, attestation.Claims.AccountContractVersion, attestation.Claims.CatalogVersion, attestation.Claims.ProtocolCode, checks, hex.EncodeToString(digest[:]), attestation.KeyID, issuedAt, expiresAt)
	return err
}

func (s *Service) clientReadinessStatus(w http.ResponseWriter, r *http.Request) {
	if s.clientReadiness == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "CLIENT_READINESS_UNAVAILABLE", "client readiness is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result := s.clientReadiness.Check(r.Context(), r.URL.Query().Get("protocol"))
	status := http.StatusOK
	if !result.Ready {
		status = http.StatusServiceUnavailable
	}
	api.WriteJSON(w, status, result)
}

func (s *Service) issueClientReadinessAttestation(w http.ResponseWriter, r *http.Request) {
	if s.clientReadiness == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "CLIENT_READINESS_UNAVAILABLE", "client readiness is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		ArtifactSHA256 string `json:"artifact_sha256"`
		ClientVersion  string `json:"client_version"`
		Environment    string `json:"environment"`
		ProtocolCode   string `json:"protocol_code"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid readiness attestation request", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.clientReadiness.Issue(r.Context(), body.ArtifactSHA256, body.ClientVersion, body.Environment, body.ProtocolCode)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, result)
}

func (s *Service) clientCompatibility(w http.ResponseWriter, r *http.Request) {
	if s.clientReadiness == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "CLIENT_READINESS_UNAVAILABLE", "client readiness is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	api.WriteJSON(w, http.StatusOK, s.clientReadiness.Compatibility())
}
