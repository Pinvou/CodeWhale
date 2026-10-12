package token

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
)

const transitMaximumValue = 1 << 20

var transitKeyName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
var transitCiphertext = regexp.MustCompile(`^vault:v[1-9][0-9]*:[A-Za-z0-9+/]+={0,2}$`)

// TransitProvider delegates cryptography to an OpenBao/Vault Transit engine.
// It never owns the encryption key or follows an authenticated redirect.
type TransitProvider struct {
	keyID     string
	baseURL   string
	token     string
	tokenFile string
	client    *http.Client
}

func NewTransitProvider(config SecretProviderConfig) (*TransitProvider, error) {
	u, err := url.Parse(config.RemoteURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || !transitKeyName.MatchString(config.KeyID) {
		return nil, ErrInvalidSecret
	}
	environment := strings.ToLower(strings.TrimSpace(config.Environment))
	development := environment == "development" || environment == "test"
	if u.Scheme != "https" && !(development && u.Scheme == "http") {
		return nil, errors.New("transit secret provider requires HTTPS")
	}
	// The token file is an Agent sink, not an encryption key. Read it on every
	// request so replacement/revocation never silently falls back to a cached token.
	if (!development && config.RemoteTokenFile == "") || (config.RemoteTokenFile != "" && (config.RemoteToken != "" || !filepath.IsAbs(config.RemoteTokenFile))) {
		return nil, errors.New("transit secret provider requires an exclusive absolute token file")
	}
	if config.TransitCAFile != "" && u.Scheme != "https" {
		return nil, errors.New("dedicated CA configuration requires HTTPS")
	}
	transport, err := newTransitTransport(config.TransitCAFile)
	if err != nil {
		return nil, err
	}
	p := &TransitProvider{keyID: config.KeyID, baseURL: strings.TrimRight(u.String(), "/"), token: config.RemoteToken, tokenFile: config.RemoteTokenFile,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if _, err := p.readToken(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *TransitProvider) KeyID() string { return p.keyID }

func (p *TransitProvider) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	if len(plaintext) == 0 || len(plaintext) > transitMaximumValue || len(associatedData) == 0 || len(associatedData) > transitMaximumValue {
		return nil, ErrInvalidSecret
	}
	return p.call("encrypt", p.keyID, base64.StdEncoding.EncodeToString(plaintext), associatedData)
}

func (p *TransitProvider) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	return p.DecryptWithKeyID(p.keyID, ciphertext, associatedData)
}

func (p *TransitProvider) DecryptWithKeyID(keyID string, ciphertext, associatedData []byte) ([]byte, error) {
	if !transitKeyName.MatchString(keyID) || len(ciphertext) > 2*transitMaximumValue || !transitCiphertext.Match(ciphertext) || len(associatedData) == 0 || len(associatedData) > transitMaximumValue {
		return nil, ErrInvalidSecret
	}
	return p.call("decrypt", keyID, string(ciphertext), associatedData)
}

func (p *TransitProvider) readToken() (string, error) {
	value := p.token
	if p.tokenFile != "" {
		info, err := os.Lstat(p.tokenFile)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 32*1024 || (runtime.GOOS != "windows" && info.Mode().Perm()&0027 != 0) {
			return "", errors.New("transit token file is unavailable or insecure")
		}
		file, err := os.Open(p.tokenFile)
		if err != nil {
			return "", errors.New("transit token file is unavailable")
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || (runtime.GOOS != "windows" && opened.Mode().Perm()&0027 != 0) {
			return "", errors.New("transit token file changed during read")
		}
		body, err := io.ReadAll(io.LimitReader(file, 32*1024+1))
		if err != nil || len(body) > 32*1024 {
			return "", errors.New("transit token file is invalid")
		}
		value = strings.TrimSpace(string(body))
	}
	if len(value) < 16 || len(value) > 32*1024 || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("transit token is invalid")
	}
	return value, nil
}

func (p *TransitProvider) call(operation, keyID, value string, aad []byte) ([]byte, error) {
	credential, err := p.readToken()
	if err != nil {
		return nil, err
	}
	payload := map[string]string{"associated_data": base64.StdEncoding.EncodeToString(aad)}
	if operation == "encrypt" {
		payload["plaintext"] = value
	} else {
		payload["ciphertext"] = value
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrInvalidSecret
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, p.baseURL+"/v1/transit/"+operation+"/"+keyID, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalidSecret
	}
	request.Header.Set("X-Vault-Token", credential)
	request.GetBody = nil // Never replay authenticated encryption/decryption writes.
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		// Transport errors can contain addresses and proxy credentials. Do not
		// surface remote bodies, TLS error details, or request/response payloads.
		return nil, errors.New("transit secret provider unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("transit secret provider rejected operation")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*transitMaximumValue+1))
	if err != nil || len(data) > 4*transitMaximumValue {
		return nil, ErrInvalidSecret
	}
	var result struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
			Plaintext  string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, ErrInvalidSecret
	}
	if operation == "encrypt" {
		if len(result.Data.Ciphertext) > 2*transitMaximumValue || !transitCiphertext.MatchString(result.Data.Ciphertext) {
			return nil, ErrInvalidSecret
		}
		return []byte(result.Data.Ciphertext), nil
	}
	plaintext, err := base64.StdEncoding.Strict().DecodeString(result.Data.Plaintext)
	if err != nil || len(plaintext) == 0 || len(plaintext) > transitMaximumValue {
		return nil, ErrInvalidSecret
	}
	return plaintext, nil
}
