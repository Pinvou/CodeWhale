package token

import "errors"

var (
	ErrInvalidSecret         = errors.New("invalid token secret configuration")
	ErrNotFound              = errors.New("token account not found")
	ErrAmbiguousRoute        = errors.New("token route is ambiguous")
	ErrNoEligibleRoute       = errors.New("no eligible token route")
	ErrSecretRotationPending = errors.New("secret key rotation is pending")
	ErrCancellationRequested = errors.New("usage cancellation was requested before dispatch")
	ErrRouteVersionConflict  = errors.New("token route version conflict")
	ErrRouteReferenceInvalid = errors.New("token route model or protocol reference is invalid")
)
