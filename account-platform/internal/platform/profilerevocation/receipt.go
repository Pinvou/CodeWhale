package profilerevocation

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var ErrUnconfirmed = errors.New("profile revocation has no confirmed receipt")
var ErrConflict = errors.New("profile revocation belongs to another subject")

// A dispatch token identifies one adapter attempt. Reclaiming even the same
// token never grants dispatch again after an acknowledgement is lost.
type Command struct {
	OperationID   uuid.UUID `json:"operation_id"`
	Subject       string    `json:"subject"`
	DispatchToken uuid.UUID `json:"dispatch_token"`
	ReplayOnly    bool      `json:"replay_only,omitempty"`
}

type Result struct {
	State string `json:"state"`
}

type Authority interface {
	ClaimProfileRevocation(context.Context, Command) (Result, error)
	ConfirmProfileRevocation(context.Context, Command) error
}
