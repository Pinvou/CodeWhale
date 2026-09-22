package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("token-portal"); err != nil {
		log.Fatal(err)
	}
	cfg, err := config.Load("token-portal")
	if err != nil {
		log.Fatal(err)
	}
	store, err := portal.NewRedisStoreWithCA(os.Getenv("REDIS_URL"), os.Getenv("REDIS_CA_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	publicURL := mustURL("PORTAL_PUBLIC_URL")
	compatibility, err := clientcompat.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	proxyHMACKey := []byte(nil)
	if raw := strings.TrimSpace(os.Getenv("TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64")); raw != "" {
		proxyHMACKey, err = base64.StdEncoding.DecodeString(raw)
		if err != nil || len(proxyHMACKey) < 32 {
			log.Fatal("TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64 must contain at least 32 base64-decoded bytes")
		}
	}
	if configured := strings.TrimSpace(os.Getenv("TOKEN_ADMIN_INGRESS_MAX_TIMEOUT_SECONDS")); configured != "" && configured != "14" {
		log.Fatal("TOKEN_ADMIN_INGRESS_MAX_TIMEOUT_SECONDS must be 14 to preserve the frozen Token admin deadline chains")
	}
	adminModelTokenIAEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED")); raw != "" {
		adminModelTokenIAEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED must be a boolean")
		}
	}
	adminTokenResourceFormsEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_TOKEN_RESOURCE_FORMS_ENABLED")); raw != "" {
		adminTokenResourceFormsEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_TOKEN_RESOURCE_FORMS_ENABLED must be a boolean")
		}
	}
	adminModelOperationsDetailsEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_MODEL_OPERATIONS_DETAILS_ENABLED")); raw != "" {
		adminModelOperationsDetailsEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_MODEL_OPERATIONS_DETAILS_ENABLED must be a boolean")
		}
	}
	adminModelDetailsEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_MODEL_DETAILS_ENABLED")); raw != "" {
		adminModelDetailsEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_MODEL_DETAILS_ENABLED must be a boolean")
		}
	}
	adminModelPricingEditEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_MODEL_PRICING_EDIT_ENABLED")); raw != "" {
		adminModelPricingEditEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_MODEL_PRICING_EDIT_ENABLED must be a boolean")
		}
	}
	adminModelOperationsOverviewEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED")); raw != "" {
		adminModelOperationsOverviewEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED must be a boolean")
		}
	}
	adminModelTokenPageOverviewsEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED")); raw != "" {
		adminModelTokenPageOverviewsEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED must be a boolean")
		}
	}
	adminTokenDetailsEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_TOKEN_DETAILS_ENABLED")); raw != "" {
		adminTokenDetailsEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_TOKEN_DETAILS_ENABLED must be a boolean")
		}
	}
	var credentialOrigin *url.URL
	if raw := strings.TrimSpace(os.Getenv("CREDENTIAL_PORTAL_PUBLIC_URL")); raw != "" {
		credentialOrigin = parseAbsoluteURL(raw, "CREDENTIAL_PORTAL_PUBLIC_URL")
	}
	adminModelTokenActionsEnabled := false
	if raw := strings.TrimSpace(os.Getenv("PORTAL_ADMIN_MODEL_TOKEN_ACTIONS_ENABLED")); raw != "" {
		adminModelTokenActionsEnabled, err = strconv.ParseBool(raw)
		if err != nil {
			log.Fatal("PORTAL_ADMIN_MODEL_TOKEN_ACTIONS_ENABLED must be a boolean")
		}
	}
	service, err := portal.NewService(portal.Config{
		PublicURL:                           publicURL,
		AuthorizationURL:                    mustURL("PORTAL_OIDC_AUTHORIZATION_URL"),
		TokenURL:                            mustURL("PORTAL_OIDC_TOKEN_URL"),
		PrivateCAFile:                       strings.TrimSpace(os.Getenv("PORTAL_PRIVATE_CA_FILE")),
		ClientID:                            os.Getenv("PORTAL_OIDC_CLIENT_ID"),
		HandoffClientID:                     os.Getenv("PORTAL_HANDOFF_OIDC_CLIENT_ID"),
		HandoffClientSecret:                 mustSecretFile("PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE", os.Getenv("PORTAL_SESSION_HANDOFF") == "true"),
		AccountURL:                          mustURL("ACCOUNT_HTTP_URL"),
		BillingURL:                          mustURL("BILLING_HTTP_URL"),
		TokenGatewayURL:                     mustURL("TOKEN_GATEWAY_HTTP_URL"),
		ClientVersion:                       compatibility.Manifest().CurrentClientVersion,
		HandoffSubjectTokenEnabled:          os.Getenv("PORTAL_SESSION_HANDOFF") == "true",
		SessionHandoffTTL:                   30 * time.Second,
		TokenAdminProxyHMACKey:              proxyHMACKey,
		AdminModelTokenIAEnabled:            adminModelTokenIAEnabled,
		AdminTokenResourceFormsEnabled:      adminTokenResourceFormsEnabled,
		AdminModelOperationsDetailsEnabled:  adminModelOperationsDetailsEnabled,
		AdminModelDetailsEnabled:            adminModelDetailsEnabled,
		AdminModelPricingEditEnabled:        adminModelPricingEditEnabled,
		AdminModelOperationsOverviewEnabled: adminModelOperationsOverviewEnabled,
		AdminModelTokenPageOverviewsEnabled: adminModelTokenPageOverviewsEnabled,
		AdminTokenDetailsEnabled:            adminTokenDetailsEnabled,
		AdminModelTokenActionsEnabled:       adminModelTokenActionsEnabled,
		CredentialOrigin:                    credentialOrigin,
		SystemProbes:                        systemProbes(),
		SecureCookies:                       publicURL.Scheme == "https" || cfg.Environment == "production" || cfg.Environment == "production-new",
	}, store)
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.RunWithApplication("token-portal", compatibility.Wrap(service.Handler(portal.StaticHandler())), store.Ping); err != nil {
		log.Fatal(err)
	}
}

func mustSecretFile(name string, required bool) string {
	path := strings.TrimSpace(os.Getenv(name))
	if path == "" {
		if required {
			log.Fatalf("%s is required when desktop session handoff is enabled", name)
		}
		return ""
	}
	if !filepath.IsAbs(path) {
		log.Fatalf("%s must be an absolute path", name)
	}
	file, err := os.Open(path)
	if err != nil {
		log.Fatalf("%s could not be opened", name)
	}
	defer file.Close()
	limited, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		log.Fatalf("%s could not be read", name)
	}
	secret := strings.TrimSpace(string(limited))
	if len(limited) > 4096 || len(secret) < 32 || strings.ContainsAny(secret, "\r\n\x00") {
		log.Fatalf("%s must contain one secret of 32 to 4096 bytes", name)
	}
	return secret
}

func systemProbes() []portal.SystemProbe {
	var configured []struct {
		Key              string `json:"key"`
		Kind             string `json:"kind"`
		URL              string `json:"url"`
		JSONBooleanField string `json:"json_boolean_field"`
	}
	if err := json.Unmarshal([]byte(os.Getenv("SYSTEM_PROBES_JSON")), &configured); err != nil {
		log.Fatalf("SYSTEM_PROBES_JSON must be a JSON array: %v", err)
	}
	seen := map[string]struct{}{}
	result := make([]portal.SystemProbe, 0, len(configured))
	for _, item := range configured {
		item.Key = strings.TrimSpace(item.Key)
		item.Kind = strings.ToLower(strings.TrimSpace(item.Kind))
		if item.Key == "" || (item.Kind != "http" && item.Kind != "relay" && item.Kind != "local") {
			log.Fatalf("SYSTEM_PROBES_JSON contains an invalid key or kind")
		}
		if _, duplicate := seen[item.Key]; duplicate {
			log.Fatalf("SYSTEM_PROBES_JSON contains duplicate key %q", item.Key)
		}
		seen[item.Key] = struct{}{}
		var target *url.URL
		if item.Kind != "local" {
			target = parseAbsoluteURL(item.URL, "SYSTEM_PROBES_JSON url")
		}
		result = append(result, portal.SystemProbe{Key: item.Key, Kind: item.Kind, Target: target, JSONBooleanField: strings.TrimSpace(item.JSONBooleanField)})
	}
	return result
}

func parseAbsoluteURL(value, label string) *url.URL {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(value), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		log.Fatalf("%s must be an absolute URL without credentials or fragment", label)
	}
	return parsed
}

func mustURL(name string) *url.URL {
	return parseAbsoluteURL(os.Getenv(name), name)
}
