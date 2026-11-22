package account

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	tokenOwnerCandidateCursorResource = "token-owner-candidates"
	tokenOwnerCandidateCursorTTL      = 15 * time.Minute
)

func encodeTokenOwnerCandidateCursor(cursor tokenOwnerCandidateCursor, key []byte) (string, error) {
	if len(key) < 32 {
		return "", errors.New("owner candidate cursor key must contain at least 32 bytes")
	}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeTokenOwnerCandidateCursor(raw string, key []byte, actor TokenOwnerCandidateActor, filter TokenOwnerCandidateFilter, now time.Time) (*tokenOwnerCandidateCursor, error) {
	if len(key) < 32 {
		return nil, ErrInvalidTokenOwnerCandidateCursor
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return nil, ErrInvalidTokenOwnerCandidateCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return nil, ErrInvalidTokenOwnerCandidateCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalidTokenOwnerCandidateCursor
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, ErrInvalidTokenOwnerCandidateCursor
	}
	var cursor tokenOwnerCandidateCursor
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Version != 1 || cursor.Resource != tokenOwnerCandidateCursorResource || cursor.ScanID == [16]byte{} || cursor.OwnerID == [16]byte{} || cursor.AuthorizationVersion < 1 || cursor.HighWater.IsZero() || cursor.IssuedAt.IsZero() || cursor.ExpiresAt.IsZero() {
		return nil, ErrInvalidTokenOwnerCandidateCursor
	}
	if cursor.CallerService != actor.CallerService || cursor.CallerSubject != actor.CallerSubject || cursor.ActorIssuer != actor.Issuer || cursor.ActorSubject != actor.Subject || cursor.FilterFingerprint != tokenOwnerCandidateFilterFingerprint(filter) {
		return nil, ErrInvalidTokenOwnerCandidateCursor
	}
	now = now.UTC()
	if !cursor.ExpiresAt.After(now) || cursor.ExpiresAt.Sub(cursor.IssuedAt) != tokenOwnerCandidateCursorTTL || cursor.IssuedAt.After(now.Add(time.Minute)) {
		return nil, ErrTokenOwnerCandidateCursorExpired
	}
	cursor.HighWater = cursor.HighWater.UTC()
	cursor.IssuedAt = cursor.IssuedAt.UTC()
	cursor.ExpiresAt = cursor.ExpiresAt.UTC()
	return &cursor, nil
}
