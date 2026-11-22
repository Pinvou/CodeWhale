package account

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const registrationQueryTokenVersion = "v1"

type RegistrationTokenSigner struct {
	key []byte
	now func() time.Time
	ttl time.Duration
}

func NewRegistrationTokenSigner(key []byte) (*RegistrationTokenSigner, error) {
	if len(key) < 32 {
		return nil, errors.New("registration query HMAC key must contain at least 32 bytes")
	}
	return &RegistrationTokenSigner{key: append([]byte(nil), key...), now: func() time.Time { return time.Now().UTC() }, ttl: 48 * time.Hour}, nil
}

func (s *RegistrationTokenSigner) Issue(operationID uuid.UUID) (string, time.Time, error) {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	expiresAt := s.now().Add(s.ttl)
	payload := strings.Join([]string{registrationQueryTokenVersion, strconv.FormatInt(expiresAt.Unix(), 10), base64.RawURLEncoding.EncodeToString(nonce)}, ".")
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.signature(operationID, payload)), expiresAt, nil
}

func (s *RegistrationTokenSigner) Validate(operationID uuid.UUID, token string) error {
	parts := strings.Split(token, ".")
	if operationID == uuid.Nil || len(parts) != 4 || parts[0] != registrationQueryTokenVersion {
		return ErrRegistrationQueryUnauthorized
	}
	expiresUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !time.Unix(expiresUnix, 0).After(s.now()) {
		return ErrRegistrationQueryUnauthorized
	}
	payload := strings.Join(parts[:3], ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || !hmac.Equal(signature, s.signature(operationID, payload)) {
		return ErrRegistrationQueryUnauthorized
	}
	return nil
}

func (s *RegistrationTokenSigner) signature(operationID uuid.UUID, payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(operationID.String()))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func registrationTokenHash(token string) [32]byte { return sha256.Sum256([]byte(token)) }
