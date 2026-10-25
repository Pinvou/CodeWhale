// Package passwordpreparation defines the password-free Account/Identity grant protocol.
package passwordpreparation

import (
	"github.com/google/uuid"
	"time"
)

type Authorization struct {
	ID        uuid.UUID `json:"authorization_id"`
	Token     string    `json:"authorization_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Claim struct {
	Token  string    `json:"authorization_token"`
	Owner  uuid.UUID `json:"identity_incarnation"`
	Source string    `json:"source"`
	Device string    `json:"device"`
}

// Facts contain no password, password digest, or Identity credential.
type Facts struct {
	ID        uuid.UUID `json:"authorization_id"`
	Mobile    string    `json:"mobile"`
	Purpose   string    `json:"purpose"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Reference struct {
	Legacy  bool      `json:"legacy"`
	ID      uuid.UUID `json:"authorization_id"`
	Mobile  string    `json:"mobile"`
	Purpose string    `json:"purpose"`
}
