package token

import (
	"errors"
	"time"
)

var ErrRateLimited = errors.New("model request rate or concurrency limit reached")

// The stored context error remains a finite denial class; this wave does not
// install organization invocation or quota producers.
var ErrOrganizationMembershipContext = errors.New("organization membership context is missing or invalid")

type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }
