package serviceauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const kubernetesJWKSURL = "https://kubernetes.default.svc/openid/v1/jwks"

var errKubernetesJWKS = errors.New("Kubernetes workload JWKS configuration or credential is invalid")

// API credentials are a separate opt-in from ordinary workload transports.
// Neither Compose nor external public JWKS inherits Kubernetes credentials.
func workloadJWKSHTTPClient(caFile, issuer, jwksURL string) (*http.Client, error) {
	mode := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_JWKS_AUTH_MODE"))
	tokenFile := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE"))
	subject := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_JWKS_API_SUBJECT"))
	if mode == "" {
		if tokenFile != "" || subject != "" {
			return nil, errKubernetesJWKS
		}
		return workloadHTTPClient(caFile)
	}
	parts := strings.Split(subject, ":")
	if mode != "kubernetes" || jwksURL != kubernetesJWKSURL || caFile == "" ||
		!canonicalWorkloadHTTPS(issuer) || len(parts) != 4 || parts[0] != "system" ||
		parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" ||
		tokenFile == "" || tokenFile == strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_TOKEN_FILE")) {
		return nil, errKubernetesJWKS
	}
	if !filepath.IsAbs(tokenFile) || filepath.Clean(tokenFile) != tokenFile {
		return nil, errKubernetesJWKS
	}
	// The projection is a rotating credential, not immutable configuration.
	// Validate its contents at every actual fetch, so a missing/expired token
	// during startup can recover after kubelet replaces it. Freezing that
	// transient error in newAuthenticator would reject requests until restart.
	client, err := workloadHTTPClient(caFile)
	if err != nil {
		return nil, err
	}
	client.Transport = &kubernetesJWKSAuthorizer{base: client.Transport, tokenFile: tokenFile, issuer: issuer, subject: subject}
	return client, nil
}

type kubernetesJWKSAuthorizer struct {
	base                       http.RoundTripper
	tokenFile, issuer, subject string
}

func (t *kubernetesJWKSAuthorizer) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.Method != http.MethodGet ||
		request.URL.String() != kubernetesJWKSURL ||
		(request.Host != "" && request.Host != "kubernetes.default.svc") ||
		(request.Body != nil && request.Body != http.NoBody) {
		return nil, errKubernetesJWKS
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	token, err := readKubernetesAPIToken(t.tokenFile, t.issuer, t.subject, time.Now())
	if err != nil {
		return nil, errKubernetesJWKS
	}
	copy := request.Clone(request.Context())
	if copy.Header == nil {
		copy.Header = make(http.Header)
	}
	copy.Header.Del("Cookie")
	copy.Header.Del("Proxy-Authorization")
	copy.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(copy)
}

// Kubelet atomically replaces ..data. Resolve the current token on every fetch,
// keeping the resolved regular file inside this exact read-only projection.
// Never pin the old inode with subPath or cache an API credential across fetches.
func readKubernetesAPIToken(filename, issuer, subject string, now time.Time) (string, error) {
	const maximum = 32 * 1024
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename {
		return "", errKubernetesJWKS
	}
	root := filepath.Dir(filename)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || resolvedRoot != root {
		return "", errKubernetesJWKS
	}
	for attempt := 0; attempt < 2; attempt++ {
		resolved, err := filepath.EvalSymlinks(filename)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", errKubernetesJWKS
		}
		info, err := os.Lstat(resolved)
		valid := func(info os.FileInfo) bool {
			return info != nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= maximum &&
				(runtime.GOOS == "windows" || info.Mode().Perm()&0022 == 0)
		}
		if err != nil || !valid(info) {
			continue
		}
		f, err := os.Open(filename)
		if err != nil {
			continue
		}
		opened, statErr := f.Stat()
		body, readErr := io.ReadAll(io.LimitReader(f, maximum+1))
		closeErr := f.Close()
		if statErr != nil || !os.SameFile(info, opened) || !valid(opened) || readErr != nil || closeErr != nil || len(body) > maximum {
			continue
		}
		token := strings.TrimSpace(string(body))
		if strings.ContainsAny(token, " \r\n\t") {
			return "", errKubernetesJWKS
		}
		segments := strings.Split(token, ".")
		if len(segments) != 3 || segments[0] == "" || segments[1] == "" || segments[2] == "" {
			return "", errKubernetesJWKS
		}
		header, headerErr := base64.RawURLEncoding.DecodeString(segments[0])
		payload, payloadErr := base64.RawURLEncoding.DecodeString(segments[1])
		signature, signatureErr := base64.RawURLEncoding.DecodeString(segments[2])
		var metadata struct {
			Algorithm string `json:"alg"`
		}
		var claims struct {
			Issuer    string          `json:"iss"`
			Subject   string          `json:"sub"`
			Issued    int64           `json:"iat"`
			Expiry    int64           `json:"exp"`
			NotBefore int64           `json:"nbf"`
			Audience  json.RawMessage `json:"aud"`
		}
		if headerErr != nil || payloadErr != nil || signatureErr != nil || len(signature) == 0 ||
			json.Unmarshal(header, &metadata) != nil || metadata.Algorithm == "" || metadata.Algorithm == "none" ||
			json.Unmarshal(payload, &claims) != nil || claims.Issuer != issuer || claims.Subject != subject ||
			!validTokenTimes(time.Unix(claims.Issued, 0), time.Unix(claims.Expiry, 0), claims.NotBefore, now) || claims.Issued <= 0 {
			return "", errKubernetesJWKS
		}
		var audiences []string
		if json.Unmarshal(claims.Audience, &audiences) != nil {
			var audience string
			if json.Unmarshal(claims.Audience, &audience) != nil {
				return "", errKubernetesJWKS
			}
			audiences = []string{audience}
		}
		if len(audiences) == 0 {
			return "", errKubernetesJWKS
		}
		for _, audience := range audiences {
			if strings.TrimSpace(audience) == "" {
				return "", errKubernetesJWKS
			}
		}
		// This is only a local shape/lifetime check. The Kubernetes API server
		// authenticates the credential's signature, bound identity and API audience.
		return token, nil
	}
	return "", errKubernetesJWKS
}
