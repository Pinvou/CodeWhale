package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
)

func main() {
	configurePortalRedisLogging()
	if len(os.Args) > 1 {
		if os.Args[1] != "capacity-inventory" {
			log.Fatal("TOKEN_PORTAL_COMMAND_INVALID")
		}
		if err := runCapacityInventory(os.Args[2:], os.Stdout); err != nil {
			d := portal.DescribeCapacityInventoryError(err)
			if errors.Is(err, errCapacityInventoryArguments) {
				d.Code, d.Stage = "PORTAL_INVENTORY_ARGUMENTS_INVALID", "arguments"
			}
			log.Fatal(d.Code, " component=", d.Component, " stage=", d.Stage)
		}
		return
	}
	if err := httpserver.ConfigureProcessLogging("token-portal"); err != nil {
		log.Fatal(err)
	}
	cfg, err := config.Load("token-portal")
	if err != nil {
		log.Fatal(err)
	}
	portalConfig, err := loadPortalConfig(cfg.Environment, os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	static := portal.StaticHandler()
	if directory := os.Getenv("PORTAL_SHARED_ASSETS_DIR"); directory != "" {
		handler, root, err := portal.OpenSharedStaticHandler(directory)
		if err != nil {
			log.Fatal("PORTAL_SHARED_ASSETS_NOT_READY: ", err)
		}
		defer root.Close()
		static = handler
	}
	store, err := portal.NewRedisStoreWithAdmissionLimits(os.Getenv("REDIS_URL"), os.Getenv("REDIS_CA_FILE"), portalConfig.AdmissionLimits)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	service, err := portal.NewService(portalConfig, store)
	if err != nil {
		log.Fatal(err)
	}
	if err := runPortal(service, store, static); err != nil {
		_ = store.Close() // log.Fatal bypasses defers.
		log.Fatal("TOKEN_PORTAL_HTTP_SERVER_FAILED")
	}
}

func runPortal(service *portal.Service, store *portal.RedisStore, static http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); store.RunTransientInventory(ctx) }()
	err := httpserver.RunWithApplicationContext(ctx, "token-portal", service.Handler(static), store.Ping)
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return errors.New("TOKEN_PORTAL_INVENTORY_DRAIN_FAILED")
	}
	return err
}

func loadPortalConfig(environment string, getenv func(string) string) (portal.Config, error) {
	cfg := portal.Config{SystemProbeCAFile: strings.TrimSpace(getenv("PORTAL_SYSTEM_PROBE_CA_FILE")), Environment: environment, ClientID: getenv("PORTAL_OIDC_CLIENT_ID"), PrivateCAFile: strings.TrimSpace(getenv("PORTAL_PRIVATE_CA_FILE")), HandoffClientID: getenv("PORTAL_HANDOFF_OIDC_CLIENT_ID"), SessionHandoffTTL: 30 * time.Second}
	cfg.AdmissionLimits = portal.DefaultAdmissionLimits()
	for _, limit := range []struct {
		name   string
		target *int
	}{
		{"PORTAL_OPERATION_RPS", &cfg.AdmissionLimits.OperationsPerSecond},
		{"PORTAL_OPERATION_BURST", &cfg.AdmissionLimits.OperationBurst},
		{"PORTAL_LOGIN_FLOW_CAPACITY", &cfg.AdmissionLimits.FlowCapacity},
		{"PORTAL_SESSION_HANDOFF_CAPACITY", &cfg.AdmissionLimits.HandoffCapacity},
	} {
		if raw := strings.TrimSpace(getenv(limit.name)); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				return portal.Config{}, errors.New(limit.name + " must be a positive integer")
			}
			*limit.target = value
		}
	}
	if !cfg.AdmissionLimits.Valid() {
		return portal.Config{}, portal.ErrAdmissionConfiguration
	}
	trustedProxies, err := portal.ParseTrustedProxyIPs(getenv("PORTAL_TRUSTED_PROXY_IPS"))
	if err != nil {
		return portal.Config{}, err
	}
	cfg.TrustedProxyIPs = trustedProxies
	cfg.SystemProbes, err = loadSystemProbes(getenv("SYSTEM_PROBES_JSON"))
	if err != nil {
		return portal.Config{}, err
	}
	endpoints := []struct {
		name   string
		target **url.URL
	}{
		{"PORTAL_PUBLIC_URL", &cfg.PublicURL}, {"PORTAL_OIDC_AUTHORIZATION_URL", &cfg.AuthorizationURL}, {"PORTAL_OIDC_TOKEN_URL", &cfg.TokenURL}, {"PORTAL_OIDC_ISSUER", &cfg.IssuerURL}, {"PORTAL_OIDC_JWKS_URL", &cfg.JWKSURL}, {"ACCOUNT_HTTP_URL", &cfg.AccountURL}, {"TOKEN_GATEWAY_HTTP_URL", &cfg.TokenGatewayURL}, {"BILLING_HTTP_URL", &cfg.BillingURL},
	}
	for _, item := range endpoints {
		parsed, err := url.Parse(strings.TrimSpace(getenv(item.name)))
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return portal.Config{}, errors.New(item.name + " must be an absolute HTTP(S) URL")
		}
		*item.target = parsed
	}
	for _, flag := range []struct {
		name   string
		target *bool
	}{
		{"PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED", &cfg.AdminModelTokenIAEnabled},
		{"PORTAL_ADMIN_TOKEN_RESOURCE_FORMS_ENABLED", &cfg.AdminTokenResourceFormsEnabled},
		{"PORTAL_ADMIN_MODEL_OPERATIONS_DETAILS_ENABLED", &cfg.AdminModelOperationsDetailsEnabled},
		{"PORTAL_ADMIN_MODEL_DETAILS_ENABLED", &cfg.AdminModelDetailsEnabled},
		{"PORTAL_ADMIN_MODEL_PRICING_EDIT_ENABLED", &cfg.AdminModelPricingEditEnabled},
		{"PORTAL_ADMIN_MODEL_TOKEN_ACTIONS_ENABLED", &cfg.AdminModelTokenActionsEnabled},
		{"PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED", &cfg.AdminModelOperationsOverviewEnabled},
		{"PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED", &cfg.AdminModelTokenPageOverviewsEnabled},
	} {
		if raw := strings.TrimSpace(getenv(flag.name)); raw != "" {
			enabled, err := strconv.ParseBool(raw)
			if err != nil {
				return portal.Config{}, errors.New(flag.name + " must be boolean")
			}
			*flag.target = enabled
		}
	}
	if raw := strings.TrimSpace(getenv("TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64")); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) < 32 {
			return portal.Config{}, errors.New("TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64 must contain at least 32 base64-decoded bytes")
		}
		cfg.TokenAdminProxyHMACKey = key
	}
	if cfg.AdminModelTokenPageOverviewsEnabled && len(cfg.TokenAdminProxyHMACKey) < 32 {
		return portal.Config{}, errors.New("PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED requires TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64")
	}
	if raw := strings.TrimSpace(getenv("PORTAL_ADMIN_TOKEN_DETAILS_ENABLED")); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return portal.Config{}, errors.New("PORTAL_ADMIN_TOKEN_DETAILS_ENABLED must be boolean")
		}
		cfg.AdminTokenDetailsEnabled = enabled
	}
	if raw := strings.TrimSpace(getenv("CREDENTIAL_PORTAL_PUBLIC_URL")); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil {
			return portal.Config{}, errors.New("CREDENTIAL_PORTAL_PUBLIC_URL must be a valid URL")
		}
		cfg.CredentialOrigin = parsed
	}
	cfg.SecureCookies = cfg.PublicURL.Scheme == "https" || environment != "development" && environment != "test"
	if raw := strings.TrimSpace(getenv("PORTAL_SESSION_HANDOFF")); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return portal.Config{}, errors.New("PORTAL_SESSION_HANDOFF must be boolean")
		}
		cfg.HandoffSubjectTokenEnabled = enabled
	}
	secret, err := readHandoffSecret(strings.TrimSpace(getenv("PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE")), cfg.HandoffSubjectTokenEnabled)
	if err != nil {
		return portal.Config{}, err
	}
	cfg.HandoffClientSecret = secret
	return cfg, nil
}

// Mounted secret files may be projected by Kubernetes; their bytes stay in the
// server process and never enter browser cookies or command output.
func readHandoffSecret(filename string, required bool) (string, error) {
	invalid := errors.New("PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE must contain one secret of 32 to 4096 bytes")
	if filename == "" {
		if required {
			return "", errors.New("PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE is required when handoff is enabled")
		}
		return "", nil
	}
	if !filepath.IsAbs(filename) {
		return "", invalid
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", invalid
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", invalid
	}
	body, err := io.ReadAll(io.LimitReader(file, 4097))
	secret := strings.TrimSpace(string(body))
	if err != nil || len(body) > 4096 || len(secret) < 32 || strings.ContainsAny(secret, "\r\n\x00") {
		return "", invalid
	}
	return secret, nil
}
