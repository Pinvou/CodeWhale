package portal

import (
	_ "embed"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
)

//go:embed credential-handoff.v1.js
var credentialHandoffScript string

func (s *Service) credentialHandoff(w http.ResponseWriter, r *http.Request) {
	if !s.config.AdminTokenDetailsEnabled || s.config.CredentialOrigin == nil || r.URL.Path != "/credential-handoff" || r.URL.RawQuery != "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	origin := credentialOriginProjection(s.config)
	if strings.TrimSpace(origin) == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'sha256-"+credentialHandoffScriptSHA256+"'; form-action "+origin+"; worker-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "origin")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), clipboard-read=(), clipboard-write=()")
	if s.config.SecureCookies {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"referrer\" content=\"origin\"><meta name=\"pinvou-credential-origin\" content=\"" + html.EscapeString(origin) + "\"><title>Credential handoff</title></head><body><script>" + credentialHandoffScript + "</script></body></html>"))
}

func validateCredentialOrigin(origin *url.URL, secure bool) error {
	if origin == nil || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") {
		return errors.New("credential origin must be an absolute origin without path, query, fragment, or credentials")
	}
	host := strings.ToLower(origin.Hostname())
	if secure {
		if origin.Scheme != "https" || (host != "credentials.pinvou.com" && host != "credentials-uat.pinvou.com") || origin.Port() != "" {
			return errors.New("credential origin is not in the fixed production or UAT allowlist")
		}
		return nil
	}
	if origin.Scheme != "http" || host != "127.0.0.1" && host != "localhost" {
		return errors.New("development credential origin must use loopback HTTP")
	}
	return nil
}

func credentialOriginProjection(config Config) string {
	if !config.AdminTokenDetailsEnabled || config.CredentialOrigin == nil {
		return ""
	}
	return config.CredentialOrigin.Scheme + "://" + strings.ToLower(config.CredentialOrigin.Host)
}
