package main

import (
	"encoding/json"
	"log"
	"net/url"
	"os"
	"strings"

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
	store, err := portal.NewRedisStore(os.Getenv("REDIS_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	publicURL := mustURL("PORTAL_PUBLIC_URL")
	service, err := portal.NewService(portal.Config{
		PublicURL:        publicURL,
		AuthorizationURL: mustURL("PORTAL_OIDC_AUTHORIZATION_URL"),
		TokenURL:         mustURL("PORTAL_OIDC_TOKEN_URL"),
		ClientID:         os.Getenv("PORTAL_OIDC_CLIENT_ID"),
		AccountURL:       mustURL("ACCOUNT_HTTP_URL"),
		BillingURL:       mustURL("BILLING_HTTP_URL"),
		TokenGatewayURL:  mustURL("TOKEN_GATEWAY_HTTP_URL"),
		SystemProbes:     systemProbes(),
		SecureCookies:    publicURL.Scheme == "https" || cfg.Environment == "production" || cfg.Environment == "production-new",
	}, store)
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.RunWithApplication("token-portal", service.Handler(portal.StaticHandler()), store.Ping); err != nil {
		log.Fatal(err)
	}
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
