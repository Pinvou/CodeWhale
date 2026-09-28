package portal

import "time"

type LoginFlow struct {
	Verifier        string    `json:"verifier"`
	Nonce           string    `json:"nonce"`
	ReturnTo        string    `json:"return_to"`
	PreviousSession string    `json:"previous_session,omitempty"`
	LineageRoot     string    `json:"lineage_root,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type SessionHandoff struct {
	Session   Session   `json:"session"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Session struct {
	ID               string    `json:"id"`
	LineageRoot      string    `json:"lineage_root,omitempty"`
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	CSRFToken        string    `json:"csrf_token"`
	CreatedAt        time.Time `json:"created_at"`
	OIDCClientID     string    `json:"oidc_client_id,omitempty"`
	// Absent on pre-upgrade sessions: those always remain on approval wire v1.
	ApprovalV2Eligible bool `json:"approval_v2_eligible,omitempty"`
}

// ApprovalClassification is a short-lived, server-authenticated proof used by
// the Portal to classify an approval action without trusting browser input or
// contacting Account after the model/Token action gate has been disabled.
type ApprovalClassification struct {
	SessionID             string    `json:"session_id"`
	ActorTokenFingerprint string    `json:"actor_token_fingerprint"`
	ApprovalRequestID     string    `json:"approval_request_id"`
	CommandType           string    `json:"command_type"`
	ExpiresAt             time.Time `json:"expires_at"`
}

type TokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
}
