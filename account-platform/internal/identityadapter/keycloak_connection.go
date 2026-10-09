package identityadapter

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

var ErrKeycloakConfiguration = errors.New("Keycloak connection configuration is invalid")

// KeycloakConnectionConfig belongs to the identity adapter, not global HTTP
// configuration. SecretFile is read once; rotation requires a controlled restart.
type KeycloakConnectionConfig struct {
	AdminURL, TokenURL, Realm, ClientID string
	ClientSecret, SecretFile, CAFile    string
	Production                          bool
}

func NewKeycloakDirectoryWithConfig(c KeycloakConnectionConfig) (*KeycloakDirectory, error) {
	if (c.SecretFile != "" && c.ClientSecret != "") || (c.Production && (c.SecretFile == "" || c.ClientSecret != "" || c.CAFile == "")) {
		return nil, ErrKeycloakConfiguration
	}
	if c.Production {
		admin, err := keycloakHTTPSURL(c.AdminURL)
		token, tokenErr := keycloakHTTPSURL(c.TokenURL)
		if err != nil || tokenErr != nil || !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(c.Realm) ||
			admin.Host != token.Host || (admin.Path != "" && admin.Path != "/") ||
			token.Path != "/realms/"+c.Realm+"/protocol/openid-connect/token" {
			return nil, ErrKeycloakConfiguration
		}
	}
	if c.SecretFile != "" {
		body, err := readKeycloakFile(c.SecretFile, 16<<10, true)
		if err != nil {
			return nil, err
		}
		c.ClientSecret = strings.TrimSpace(string(body))
		clear(body)
		if c.ClientSecret == "" || strings.ContainsAny(c.ClientSecret, "\r\n\x00") {
			return nil, ErrKeycloakConfiguration
		}
	}
	directory, err := NewKeycloakDirectory(c.AdminURL, c.TokenURL, c.Realm, c.ClientID, c.ClientSecret)
	if err != nil {
		return nil, ErrKeycloakConfiguration
	}
	if c.CAFile != "" || c.Production {
		// Both endpoints must use the same explicit trust, including optional
		// private-CA use outside production. Never change process-wide roots.
		if _, err := keycloakHTTPSURL(c.AdminURL); err != nil {
			return nil, err
		}
		directory.client, err = keycloakHTTPClient(c.TokenURL, c.CAFile, c.Production, 10*time.Second)
		if err != nil {
			return nil, err
		}
	}
	return directory, nil
}

func keycloakHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return nil, ErrKeycloakConfiguration
	}
	return u, nil
}

func keycloakHTTPClient(endpoint, caFile string, production bool, timeout time.Duration) (*http.Client, error) {
	if caFile == "" && !production {
		return &http.Client{Timeout: timeout}, nil
	}
	if caFile == "" {
		return nil, ErrKeycloakConfiguration
	}
	if _, err := keycloakHTTPSURL(endpoint); err != nil {
		return nil, err
	}
	pool, err := keycloakRoots(caFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: timeout, Transport: observability.NewHTTPTransport(keycloakTransport(pool)),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func keycloakTransport(pool *x509.CertPool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return transport
}

func readKeycloakFile(filename string, maximum int64, secret bool) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(filename)
	info, statErr := os.Lstat(filename)
	valid := func(info os.FileInfo) bool {
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
			return false
		}
		if runtime.GOOS != "windows" {
			if secret {
				return info.Mode().Perm() == 0400 || info.Mode().Perm() == 0600
			}
			return info.Mode().Perm()&0022 == 0
		}
		return true // Production Linux permissions are covered separately.
	}
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || err != nil || resolved != filename || statErr != nil || !valid(info) {
		return nil, ErrKeycloakConfiguration
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, ErrKeycloakConfiguration
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !valid(opened) {
		return nil, ErrKeycloakConfiguration
	}
	body, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maximum {
		return nil, ErrKeycloakConfiguration
	}
	return body, nil
}

func keycloakRoots(filename string) (*x509.CertPool, error) {
	body, err := readKeycloakFile(filename, 1<<20, false)
	if err != nil {
		return nil, err
	}
	pool, count := x509.NewCertPool(), 0
	for len(bytes.TrimSpace(body)) > 0 {
		body = bytes.TrimSpace(body)
		if !bytes.HasPrefix(body, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, ErrKeycloakConfiguration
		}
		// Bound the first candidate so pem.Decode cannot skip a damaged block.
		marker := []byte("-----END CERTIFICATE-----")
		end := bytes.Index(body, marker)
		if end < 0 {
			return nil, ErrKeycloakConfiguration
		}
		end += len(marker)
		candidate := body[:end]
		block, rest := pem.Decode(candidate)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 ||
			len(bytes.TrimSpace(rest)) != 0 || bytes.Count(candidate, []byte("-----BEGIN ")) != 1 {
			return nil, ErrKeycloakConfiguration
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
			return nil, ErrKeycloakConfiguration
		}
		pool.AddCert(cert)
		count++
		body = body[end:]
	}
	if count == 0 {
		return nil, ErrKeycloakConfiguration
	}
	return pool, nil
}
