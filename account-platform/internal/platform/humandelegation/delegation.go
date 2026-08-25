package humandelegation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	Version         = "PINVOU_HUMAN_DELEGATION_V1"
	MaximumLifetime = 5 * time.Minute
)

var (
	codePattern   = regexp.MustCompile(`^[a-z][a-z0-9._:-]{1,127}$`)
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

var ErrInvalid = errors.New("human delegation is invalid")

type Claims struct {
	Version              string     `json:"version"`
	DelegationID         uuid.UUID  `json:"delegation_id"`
	Issuer               string     `json:"issuer"`
	Audience             string     `json:"audience"`
	HumanUserID          uuid.UUID  `json:"human_user_id"`
	Permission           string     `json:"permission"`
	AuthorizationVersion int64      `json:"authorization_version"`
	OrganizationID       *uuid.UUID `json:"organization_id,omitempty"`
	UnitID               *uuid.UUID `json:"unit_id,omitempty"`
	ResourceType         string     `json:"resource_type"`
	ResourceID           string     `json:"resource_id"`
	HTTPMethod           string     `json:"http_method"`
	HTTPPath             string     `json:"http_path"`
	RequestSHA256        string     `json:"request_sha256"`
	IssuedAt             time.Time  `json:"issued_at"`
	ExpiresAt            time.Time  `json:"expires_at"`
}

type Envelope struct {
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type Signer struct {
	keyID      string
	privateKey ed25519.PrivateKey
	now        func() time.Time
}

type Verifier struct {
	keys map[string]ed25519.PublicKey
	now  func() time.Time
}

type Expectation struct {
	Audience       string
	Permission     string
	OrganizationID *uuid.UUID
	UnitID         *uuid.UUID
	ResourceType   string
	ResourceID     string
	HTTPMethod     string
	HTTPPath       string
	RequestSHA256  string
}

func DecodeSigningKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, errors.New("human delegation signing key must be unpadded base64url")
	}
	if len(raw) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(raw), nil
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("human delegation signing key must contain %d-byte seed or %d-byte private key", ed25519.SeedSize, ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(append([]byte(nil), raw...)), nil
}

func DecodeVerificationKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("human delegation verification key must be a 32-byte unpadded base64url value")
	}
	return ed25519.PublicKey(append([]byte(nil), raw...)), nil
}

func NewSigner(keyID string, privateKey ed25519.PrivateKey) (*Signer, error) {
	keyID = strings.TrimSpace(keyID)
	if !codePattern.MatchString(keyID) || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("human delegation signer requires a valid key ID and Ed25519 private key")
	}
	return &Signer{keyID: keyID, privateKey: append(ed25519.PrivateKey(nil), privateKey...), now: func() time.Time { return time.Now().UTC() }}, nil
}

func NewVerifier(keys map[string]ed25519.PublicKey) (*Verifier, error) {
	copyKeys := make(map[string]ed25519.PublicKey, len(keys))
	for keyID, key := range keys {
		keyID = strings.TrimSpace(keyID)
		if !codePattern.MatchString(keyID) || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("human delegation verifier contains an invalid key")
		}
		copyKeys[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	if len(copyKeys) == 0 {
		return nil, errors.New("human delegation verifier requires at least one key")
	}
	return &Verifier{keys: copyKeys, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Signer) Sign(claims Claims) (string, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return "", ErrInvalid
	}
	now := s.now().UTC()
	claims.Version = Version
	if claims.DelegationID == uuid.Nil {
		claims.DelegationID = uuid.Must(uuid.NewV7())
	}
	claims.Issuer = "account-service"
	claims.IssuedAt = now
	if claims.ExpiresAt.IsZero() {
		claims.ExpiresAt = now.Add(MaximumLifetime)
	}
	claims = normalize(claims)
	if validateClaims(claims, now) != nil {
		return "", ErrInvalid
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	envelope := Envelope{
		KeyID:     s.keyID,
		Payload:   base64.RawURLEncoding.EncodeToString(payload),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.privateKey, payload)),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func (v *Verifier) Verify(token string, expected Expectation) (Claims, error) {
	if v == nil || len(token) > 16*1024 {
		return Claims{}, ErrInvalid
	}
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var envelope Envelope
	if json.Unmarshal(encoded, &envelope) != nil {
		return Claims{}, ErrInvalid
	}
	key, ok := v.keys[strings.TrimSpace(envelope.KeyID)]
	payload, payloadErr := base64.RawURLEncoding.DecodeString(envelope.Payload)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if !ok || payloadErr != nil || signatureErr != nil || !ed25519.Verify(key, payload, signature) {
		return Claims{}, ErrInvalid
	}
	var claims Claims
	if json.Unmarshal(payload, &claims) != nil {
		return Claims{}, ErrInvalid
	}
	claims = normalize(claims)
	if validateClaims(claims, v.now().UTC()) != nil || !matches(claims, expected) {
		return Claims{}, ErrInvalid
	}
	return claims, nil
}

func RequestDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func normalize(claims Claims) Claims {
	claims.Audience = strings.TrimSpace(claims.Audience)
	claims.Permission = strings.TrimSpace(claims.Permission)
	claims.ResourceType = strings.TrimSpace(claims.ResourceType)
	claims.ResourceID = strings.TrimSpace(claims.ResourceID)
	claims.HTTPMethod = strings.ToUpper(strings.TrimSpace(claims.HTTPMethod))
	claims.HTTPPath = strings.TrimSpace(claims.HTTPPath)
	claims.RequestSHA256 = strings.ToLower(strings.TrimSpace(claims.RequestSHA256))
	claims.IssuedAt = claims.IssuedAt.UTC()
	claims.ExpiresAt = claims.ExpiresAt.UTC()
	return claims
}

func validateClaims(claims Claims, now time.Time) error {
	if claims.Version != Version || claims.DelegationID == uuid.Nil || claims.Issuer != "account-service" || claims.HumanUserID == uuid.Nil ||
		!codePattern.MatchString(claims.Audience) || !codePattern.MatchString(claims.Permission) || claims.AuthorizationVersion < 1 ||
		!codePattern.MatchString(claims.ResourceType) || claims.ResourceID == "" || len(claims.ResourceID) > 256 ||
		claims.HTTPMethod == "" || len(claims.HTTPMethod) > 16 || !strings.HasPrefix(claims.HTTPPath, "/internal/v1/") || len(claims.HTTPPath) > 1024 ||
		!digestPattern.MatchString(claims.RequestSHA256) || (claims.UnitID != nil && claims.OrganizationID == nil) ||
		claims.IssuedAt.After(now.Add(30*time.Second)) || !claims.ExpiresAt.After(claims.IssuedAt) || claims.ExpiresAt.Sub(claims.IssuedAt) > MaximumLifetime || !claims.ExpiresAt.After(now) {
		return ErrInvalid
	}
	return nil
}

func matches(claims Claims, expected Expectation) bool {
	expectedClaims := normalize(Claims{Audience: expected.Audience, Permission: expected.Permission, OrganizationID: expected.OrganizationID, UnitID: expected.UnitID, ResourceType: expected.ResourceType, ResourceID: expected.ResourceID, HTTPMethod: expected.HTTPMethod, HTTPPath: expected.HTTPPath, RequestSHA256: expected.RequestSHA256})
	return claims.Audience == expectedClaims.Audience && claims.Permission == expectedClaims.Permission && equalUUID(claims.OrganizationID, expectedClaims.OrganizationID) && equalUUID(claims.UnitID, expectedClaims.UnitID) && claims.ResourceType == expectedClaims.ResourceType && claims.ResourceID == expectedClaims.ResourceID && claims.HTTPMethod == expectedClaims.HTTPMethod && claims.HTTPPath == expectedClaims.HTTPPath && claims.RequestSHA256 == expectedClaims.RequestSHA256
}

func equalUUID(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
