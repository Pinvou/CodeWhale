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
		if environment == "production" || environment == "production-new" {
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

func NewRemoteEnvelopeProvider(environment, keyID, rawURL, token string) (*RemoteEnvelopeProvider, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimSpace(keyID) == "" || len(strings.TrimSpace(token)) < 32 {
		return nil, ErrInvalidSecret
	}
	production := environment == "production" || environment == "production-new"
	if (production && parsed.Scheme != "https") || (!production && parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("remote secret provider requires HTTPS in production")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &RemoteEnvelopeProvider{keyID: keyID, baseURL: parsed, token: token, client: client}, nil
}

func (p *RemoteEnvelopeProvider) KeyID() string { return p.keyID }

func (p *RemoteEnvelopeProvider) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	if len(plaintext) == 0 || len(associatedData) == 0 {
		return nil, ErrInvalidSecret
	}
	return p.call("encrypt", p.keyID, plaintext, associatedData)
}

func (p *RemoteEnvelopeProvider) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	return p.DecryptWithKeyID(p.keyID, ciphertext, associatedData)
}

func (p *RemoteEnvelopeProvider) DecryptWithKeyID(keyID string, ciphertext, associatedData []byte) ([]byte, error) {
	if strings.TrimSpace(keyID) == "" || len(ciphertext) == 0 || len(associatedData) == 0 {
		return nil, ErrInvalidSecret
	}
	return p.call("decrypt", keyID, ciphertext, associatedData)
}

func (p *RemoteEnvelopeProvider) call(operation, keyID string, value, associatedData []byte) ([]byte, error) {
	payload, err := json.Marshal(map[string]string{
		"key_id":              keyID,
		"value_base64":        base64.StdEncoding.EncodeToString(value),
		"associated_data_b64": base64.StdEncoding.EncodeToString(associatedData),
	})
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
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
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
