package token

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	platformconfig "github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

type SecretProvider interface {
	KeyID() string
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
	DecryptWithKeyID(keyID string, ciphertext, associatedData []byte) ([]byte, error)
}

type LocalAESProvider struct {
	keyID string
	aead  cipher.AEAD
}

func NewLocalAESProvider(keyID string, key []byte) (*LocalAESProvider, error) {
	if strings.TrimSpace(keyID) == "" || len(key) != 32 {
		return nil, ErrInvalidSecret
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalidSecret
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrInvalidSecret
	}
	return &LocalAESProvider{keyID: keyID, aead: aead}, nil
}

type SecretProviderConfig struct {
	Environment     string
	Mode            string
	KeyID           string
	LocalKey        []byte
	RemoteURL       string
	RemoteToken     string
	RemoteTokenFile string
	// TransitCAFile scopes private trust to the native KMS client only.
	TransitCAFile string
}

func NewSecretProvider(config SecretProviderConfig) (SecretProvider, error) {
	environment := strings.ToLower(strings.TrimSpace(config.Environment))
	mode := strings.ToLower(strings.TrimSpace(config.Mode))
	if mode == "" {
		mode = "local"
	}
	if config.TransitCAFile != "" && mode != "vault-transit" {
		return nil, errors.New("dedicated CA configuration requires vault-transit mode")
	}
	if mode == "local" {
		if platformconfig.IsProduction(environment) {
			return nil, errors.New("production requires a KMS or Vault secret provider")
		}
		return NewLocalAESProvider(config.KeyID, config.LocalKey)
	}
	if mode == "remote" {
		return NewRemoteEnvelopeProvider(environment, config.KeyID, config.RemoteURL, config.RemoteToken)
	}
	if mode == "vault-transit" {
		return NewTransitProvider(config)
	}
	return nil, fmt.Errorf("secret provider %q is not configured", mode)
}

func (p *LocalAESProvider) KeyID() string { return p.keyID }

func (p *LocalAESProvider) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, ErrInvalidSecret
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return p.aead.Seal(nonce, nonce, plaintext, associatedData), nil
}

func (p *LocalAESProvider) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	return p.DecryptWithKeyID(p.keyID, ciphertext, associatedData)
}

func (p *LocalAESProvider) DecryptWithKeyID(keyID string, ciphertext, associatedData []byte) ([]byte, error) {
	if keyID != p.keyID {
		return nil, ErrInvalidSecret
	}
	if len(ciphertext) <= p.aead.NonceSize() {
		return nil, ErrInvalidSecret
	}
	nonce, payload := ciphertext[:p.aead.NonceSize()], ciphertext[p.aead.NonceSize():]
	plaintext, err := p.aead.Open(nil, nonce, payload, associatedData)
	if err != nil {
		return nil, ErrInvalidSecret
	}
	return plaintext, nil
}

type RemoteEnvelopeProvider struct {
	keyID   string
	baseURL *url.URL
	token   string
	client  *http.Client
}

const (
	// Remote also protects dispatch and replay data. Keep room for Base64 of a
	// 32 MiB replay result and its envelope instead of importing Transit limits.
	remoteEnvelopeMaximumRequestBytes  = 64 << 20
	remoteEnvelopeMaximumResponseBytes = 1 << 20 // The original Remote read budget.
	remoteEnvelopeMaximumKeyIDBytes    = 1 << 20 // Bound JSON escaping of opaque key metadata.
)

func validRemoteEnvelopeKeyID(keyID string) bool {
	return len(keyID) <= remoteEnvelopeMaximumKeyIDBytes && strings.TrimSpace(keyID) != ""
}

func NewRemoteEnvelopeProvider(environment, keyID, rawURL, token string) (*RemoteEnvelopeProvider, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !validRemoteEnvelopeKeyID(keyID) || len(strings.TrimSpace(token)) < 32 {
		return nil, ErrInvalidSecret
	}
	production := platformconfig.IsProduction(environment)
	if (production && parsed.Scheme != "https") || (!production && parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("remote secret provider requires HTTPS in production")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &RemoteEnvelopeProvider{keyID: keyID, baseURL: parsed, token: token, client: client}, nil
}

func (p *RemoteEnvelopeProvider) KeyID() string { return p.keyID }

func (p *RemoteEnvelopeProvider) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	return p.call("encrypt", p.keyID, plaintext, associatedData)
}

func (p *RemoteEnvelopeProvider) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	return p.DecryptWithKeyID(p.keyID, ciphertext, associatedData)
}

func (p *RemoteEnvelopeProvider) DecryptWithKeyID(keyID string, ciphertext, associatedData []byte) ([]byte, error) {
	return p.call("decrypt", keyID, ciphertext, associatedData)
}

func encodeRemoteEnvelopeRequest(keyID string, value, associatedData []byte, maximum int) ([]byte, error) {
	if !validRemoteEnvelopeKeyID(keyID) || len(value) == 0 || len(associatedData) == 0 || len(value) > maximum || len(associatedData) > maximum {
		return nil, ErrInvalidSecret
	}
	encodedBytes := base64.StdEncoding.EncodedLen(len(value)) + base64.StdEncoding.EncodedLen(len(associatedData))
	if encodedBytes > maximum {
		return nil, ErrInvalidSecret
	}
	fields := map[string]string{"key_id": keyID, "value_base64": "", "associated_data_b64": ""}
	// Base64 adds no JSON escapes. Measure the same encoder's fixed fields and
	// opaque key ID before allocating the potentially large encoded payloads.
	metadata, err := json.Marshal(fields)
	if err != nil || len(metadata) > maximum-encodedBytes {
		return nil, ErrInvalidSecret
	}
	fields["value_base64"] = base64.StdEncoding.EncodeToString(value)
	fields["associated_data_b64"] = base64.StdEncoding.EncodeToString(associatedData)
	return json.Marshal(fields)
}

func (p *RemoteEnvelopeProvider) call(operation, keyID string, value, associatedData []byte) ([]byte, error) {
	payload, err := encodeRemoteEnvelopeRequest(keyID, value, associatedData, remoteEnvelopeMaximumRequestBytes)
	if err != nil {
		return nil, ErrInvalidSecret
	}
	target := *p.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/" + operation
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, ErrInvalidSecret
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("remote secret provider unavailable: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("remote secret provider returned HTTP %d", response.StatusCode)
	}
	var body struct {
		Value string `json:"value_base64"`
	}
	raw, err := readBoundedUpstreamBody(response.Body, remoteEnvelopeMaximumResponseBytes)
	if err != nil || json.Unmarshal(raw, &body) != nil {
		return nil, ErrInvalidSecret
	}
	decoded, err := base64.StdEncoding.DecodeString(body.Value)
	if err != nil || len(decoded) == 0 {
		return nil, ErrInvalidSecret
	}
	return decoded, nil
}

func Fingerprint(secret []byte) string {
	digest := sha256.Sum256(secret)
	return hex.EncodeToString(digest[:])
}

func EqualFingerprint(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
