package token

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

type SecretProvider interface {
	KeyID() string
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
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

func NewSecretProvider(environment, mode, keyID string, key []byte) (SecretProvider, error) {
	environment = strings.ToLower(strings.TrimSpace(environment))
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "local"
	}
	if mode == "local" {
		if environment == "production" || environment == "production-new" {
			return nil, errors.New("production requires a KMS or Vault secret provider")
		}
		return NewLocalAESProvider(keyID, key)
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
