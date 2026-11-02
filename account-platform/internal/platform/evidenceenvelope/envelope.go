// Package evidenceenvelope encrypts bounded evidence bytes with a per-file DEK.
// It does not authorize access or validate image/PDF content.
package evidenceenvelope

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/google/uuid"
)

const (
	MaxPlaintextBytes  = 10 << 20
	MaxWrappedKeyBytes = 64 << 10
	dekBytes           = 32
	nonceBytes         = 12
	tagBytes           = 16
)

// ErrInvalid deliberately hides provider errors, keys, and evidence contents.
var ErrInvalid = errors.New("evidence envelope is invalid or unavailable")

// KeyWrapper is implemented by the existing Context-aware key providers.
// Implementations must authenticate associatedData, use exactly keyID, retain
// historical keys, honor cancellation, and return caller-owned byte slices.
// Only the 32-byte DEK, never the evidence file, is passed to Encrypt.
type KeyWrapper interface {
	KeyID() string
	EncryptWithKeyIDContext(ctx context.Context, keyID string, plaintext, associatedData []byte) ([]byte, error)
	DecryptWithKeyIDContext(ctx context.Context, keyID string, ciphertext, associatedData []byte) ([]byte, error)
}

// Binding must come from an authorized owner record and immutable document
// version. Client metadata or the envelope itself is not an authority.
type Binding struct {
	Purpose    string
	OwnerID    uuid.UUID
	DocumentID uuid.UUID
	Version    int64
	MediaType  string
}

// Envelope contains the bounded persisted ciphertext and authenticated facts.
// Transport decoding must enforce its own encoded-body limit before allocation.
type Envelope struct {
	Binding         Binding
	PlaintextSize   int64
	PlaintextSHA256 [sha256.Size]byte
	KeyID           string
	WrappedDEK      []byte
	Nonce           []byte
	Ciphertext      []byte
}

// aadV1 is a fixed-order, versioned encoding shared by wrapping and file AEAD.
type aadV1 struct {
	Domain     string `json:"domain"`
	Purpose    string `json:"purpose"`
	OwnerID    string `json:"owner_id"`
	DocumentID string `json:"document_id"`
	Version    int64  `json:"version"`
	MediaType  string `json:"media_type"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	KeyID      string `json:"key_id"`
}

func validToken(s string, purpose bool) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		if i > 0 && (c == '-' || c == '_' || (purpose && c == '.')) {
			continue
		}
		return false
	}
	return true
}

func associatedData(b Binding, size int64, digest [sha256.Size]byte, keyID string) ([]byte, error) {
	if !validToken(b.Purpose, true) || b.OwnerID == uuid.Nil || b.DocumentID == uuid.Nil || b.Version < 1 || size < 1 || size > MaxPlaintextBytes || !validToken(keyID, false) {
		return nil, ErrInvalid
	}
	switch b.MediaType {
	case "image/png", "image/jpeg", "application/pdf":
	default:
		return nil, ErrInvalid
	}
	aad, err := json.Marshal(aadV1{
		Domain:     "pinvou.evidence-envelope.v1",
		Purpose:    b.Purpose,
		OwnerID:    b.OwnerID.String(),
		DocumentID: b.DocumentID.String(),
		Version:    b.Version,
		MediaType:  b.MediaType,
		Size:       size,
		SHA256:     hex.EncodeToString(digest[:]),
		KeyID:      keyID,
	})
	if err != nil {
		return nil, ErrInvalid
	}
	return aad, nil
}

// Seal encrypts one already validated file. Callers must not mutate arguments
// concurrently, and must perform format validation before storing the result.
func Seal(ctx context.Context, wrapper KeyWrapper, binding Binding, plaintext []byte) (Envelope, error) {
	return seal(ctx, wrapper, binding, plaintext, rand.Reader)
}

func seal(ctx context.Context, wrapper KeyWrapper, binding Binding, plaintext []byte, random io.Reader) (Envelope, error) {
	if ctx == nil || ctx.Err() != nil || wrapper == nil || random == nil || len(plaintext) < 1 || len(plaintext) > MaxPlaintextBytes {
		return Envelope{}, ErrInvalid
	}
	keyID := wrapper.KeyID()
	digest := sha256.Sum256(plaintext)
	aad, err := associatedData(binding, int64(len(plaintext)), digest, keyID)
	if err != nil {
		return Envelope{}, ErrInvalid
	}
	dek := make([]byte, dekBytes)
	defer clear(dek)
	if _, err = io.ReadFull(random, dek); err != nil {
		return Envelope{}, ErrInvalid
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return Envelope{}, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, ErrInvalid
	}
	nonce := make([]byte, nonceBytes)
	if _, err = io.ReadFull(random, nonce); err != nil || ctx.Err() != nil {
		return Envelope{}, ErrInvalid
	}
	wrapped, err := wrapper.EncryptWithKeyIDContext(ctx, keyID, dek, aad)
	if err != nil || ctx.Err() != nil || len(wrapped) < 1 || len(wrapped) > MaxWrappedKeyBytes {
		return Envelope{}, ErrInvalid
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	if ctx.Err() != nil {
		return Envelope{}, ErrInvalid
	}
	return Envelope{Binding: binding, PlaintextSize: int64(len(plaintext)), PlaintextSHA256: digest, KeyID: keyID, WrappedDEK: append([]byte(nil), wrapped...), Nonce: nonce, Ciphertext: ciphertext}, nil
}

// Open requires an independently trusted expected binding. Matching authenticated
// metadata proves integrity, not authorization. It reads the persisted historical
// key ID explicitly; it never falls back to the wrapper's current key.
func Open(ctx context.Context, wrapper KeyWrapper, expected Binding, envelope Envelope) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || wrapper == nil || envelope.Binding != expected || len(envelope.WrappedDEK) < 1 || len(envelope.WrappedDEK) > MaxWrappedKeyBytes || len(envelope.Nonce) != nonceBytes || envelope.PlaintextSize < 1 || envelope.PlaintextSize > MaxPlaintextBytes || int64(len(envelope.Ciphertext)) != envelope.PlaintextSize+tagBytes {
		return nil, ErrInvalid
	}
	aad, err := associatedData(expected, envelope.PlaintextSize, envelope.PlaintextSHA256, envelope.KeyID)
	if err != nil {
		return nil, ErrInvalid
	}
	dek, err := wrapper.DecryptWithKeyIDContext(ctx, envelope.KeyID, envelope.WrappedDEK, aad)
	defer clear(dek)
	if err != nil || ctx.Err() != nil || len(dek) != dekBytes {
		return nil, ErrInvalid
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, ErrInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrInvalid
	}
	// Own the output buffer so any failed operation can explicitly erase it.
	buffer := make([]byte, envelope.PlaintextSize)
	plaintext, err := aead.Open(buffer[:0], envelope.Nonce, envelope.Ciphertext, aad)
	if err != nil || ctx.Err() != nil || sha256.Sum256(plaintext) != envelope.PlaintextSHA256 {
		clear(buffer)
		return nil, ErrInvalid
	}
	return plaintext, nil
}
