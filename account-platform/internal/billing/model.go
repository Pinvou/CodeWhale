package billing

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type Wallet struct {
	ID                    uuid.UUID `json:"wallet_id"`
	OwnerType             string    `json:"owner_type"`
	OwnerID               uuid.UUID `json:"owner_id"`
	Status                string    `json:"status"`
	PostedMicrocredits    int64     `json:"posted_microcredits"`
	ReservedMicrocredits  int64     `json:"reserved_microcredits"`
	AvailableMicrocredits int64     `json:"available_microcredits"`
	Version               int64     `json:"version"`
}

var (
	ErrNotFound = errors.New("billing resource not found")
	ErrConflict = errors.New("billing resource conflict")
)

type Repository interface {
	EnsureWallet(context.Context, string, uuid.UUID) (Wallet, bool, error)
	GetWallet(context.Context, uuid.UUID) (Wallet, error)
}
