package securityadmin

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const breakGlassAudience = "pinvou-account-security-break-glass"

var ErrBreakGlassIdentityInvalid = errors.New("break-glass operator credential is invalid")

type BreakGlassIdentity struct {
	CredentialID   string    `json:"credential_id"`
	Subject        string    `json:"subject"`
	TargetUsername string    `json:"target_username"`
	Audience       string    `json:"audience"`
	IssuedAt       time.Time `json:"issued_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type breakGlassCredentialEnvelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type BreakGlassCredentialVerifier struct {
	publicKey ed25519.PublicKey
	now       func() time.Time
}

func NewBreakGlassCredentialVerifier(encodedPublicKey string) (*BreakGlassCredentialVerifier, error) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encodedPublicKey))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, ErrBreakGlassIdentityInvalid
	}
	return &BreakGlassCredentialVerifier{publicKey: ed25519.PublicKey(key), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (v *BreakGlassCredentialVerifier) Verify(raw string) (BreakGlassIdentity, error) {
	if v == nil || len(v.publicKey) != ed25519.PublicKeySize {
		return BreakGlassIdentity{}, ErrBreakGlassIdentityInvalid
	}
	var envelope breakGlassCredentialEnvelope
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &envelope) != nil {
		return BreakGlassIdentity{}, ErrBreakGlassIdentityInvalid
	}
	payload, payloadErr := base64.RawURLEncoding.DecodeString(envelope.Payload)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if payloadErr != nil || signatureErr != nil || !ed25519.Verify(v.publicKey, payload, signature) {
		return BreakGlassIdentity{}, ErrBreakGlassIdentityInvalid
	}
	var identity BreakGlassIdentity
	if json.Unmarshal(payload, &identity) != nil {
		return BreakGlassIdentity{}, ErrBreakGlassIdentityInvalid
	}
	identity.CredentialID = strings.TrimSpace(identity.CredentialID)
	identity.Subject = strings.TrimSpace(identity.Subject)
	identity.TargetUsername = strings.TrimSpace(identity.TargetUsername)
	now := v.now().UTC()
	if identity.Audience != breakGlassAudience || len(identity.CredentialID) < 8 || len(identity.CredentialID) > 200 || len(identity.Subject) < 3 || len(identity.Subject) > 200 || identity.TargetUsername == "" || len(identity.TargetUsername) > 200 || identity.IssuedAt.After(now.Add(time.Minute)) || !identity.ExpiresAt.After(now) || !identity.ExpiresAt.After(identity.IssuedAt) || identity.ExpiresAt.Sub(identity.IssuedAt) > 15*time.Minute {
		return BreakGlassIdentity{}, ErrBreakGlassIdentityInvalid
	}
	return identity, nil
}
