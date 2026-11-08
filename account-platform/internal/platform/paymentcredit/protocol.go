// Package paymentcredit defines the durable interlock between payment effects
// and Account identity cleanup. An uncertain operation has no expiry terminal.
package paymentcredit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

var ErrProtocol = errors.New("PAYMENT_CREDIT_PROTOCOL_INVALID")

// Request binds one attempted effect to original immutable financial facts.
// Digests avoid copying provider identifiers or raw evidence into Account.
type Request struct {
	ID                uuid.UUID `json:"id"`
	UserID            uuid.UUID `json:"user_id"`
	OrderID           uuid.UUID `json:"order_id"`
	TransactionDigest string    `json:"transaction_digest"`
	FactsDigest       string    `json:"facts_digest"`
}

func Digest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func (r Request) Valid() bool {
	return r.ID != uuid.Nil && r.UserID != uuid.Nil && r.OrderID != uuid.Nil && validDigest(r.TransactionDigest) && validDigest(r.FactsDigest)
}

func (r Request) Fingerprint() string {
	encoded, _ := json.Marshal(r)
	return Digest(encoded)
}

type Decision struct {
	ID          uuid.UUID `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	State       string    `json:"state"`
}

func (d Decision) Matches(r Request) bool {
	return r.Valid() && d.ID == r.ID && d.Fingerprint == r.Fingerprint()
}

type Admission interface {
	Admit(context.Context, Request) (Decision, error)
}

// Resolve must first obtain the original Billing order lock. ABORTED means a
// retained tombstone fences this attempted effect; terminal audit retention
// never expires an unresolved operation. New attempts always use fresh IDs.
type Resolver interface {
	Resolve(context.Context, Request) (Decision, error)
	Acknowledge(context.Context, Request, string) (Decision, error)
}

type Acknowledgement struct {
	Request Request `json:"request"`
	State   string  `json:"state"`
}
