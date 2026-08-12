package token

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound        = errors.New("token account not found")
	ErrForbidden       = errors.New("token account is not authorized")
	ErrAmbiguousRoute  = errors.New("token route is ambiguous")
	ErrNoEligibleRoute = errors.New("no eligible token route")
	ErrInvalidSecret   = errors.New("invalid token secret configuration")
)

type AuthorizationContext struct {
	UserID            uuid.UUID
	SessionID         string
	ContextType       string
	OrganizationID    *uuid.UUID
	MembershipVersion int64
	UnitIDs           []uuid.UUID
	AncestorUnitIDs   []uuid.UUID
	Roles             []string
}

type Candidate struct {
	TokenAccountID    uuid.UUID
	Name              string
	OwnerType         string
	OwnerID           uuid.UUID
	OwnerOrganization *uuid.UUID
	OwnerUnit         *uuid.UUID
	WalletID          uuid.UUID
	Status            string
	ShareScope        string
	AllowedModels     []string
	ExplicitMembers   []uuid.UUID
	DefaultScope      string
	Priority          int
	HealthStatus      string
	CredentialID      uuid.UUID
}

type RouteRequest struct {
	Model                  string
	ExplicitTokenAccountID *uuid.UUID
}

type UsageRequest struct {
	ID                  uuid.UUID
	IdempotencyKey      string
	UserID              uuid.UUID
	SessionID           string
	OrganizationID      *uuid.UUID
	TokenAccountID      uuid.UUID
	CredentialID        uuid.UUID
	WalletID            uuid.UUID
	PriceVersionID      *uuid.UUID
	HoldID              *uuid.UUID
	Model               string
	Status              string
	MaximumMicrocredits int64
	ActualMicrocredits  *int64
	StartedAt           time.Time
}

type UsageStore interface {
	BeginUsage(context.Context, UsageRequest, map[string]any) (UsageRequest, bool, error)
	MarkReserved(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	MarkUpstream(context.Context, uuid.UUID) error
	MarkCommitted(context.Context, uuid.UUID, int64, map[string]any) error
	MarkReleased(context.Context, uuid.UUID, string) error
	MarkReview(context.Context, uuid.UUID, string, map[string]any) error
}
