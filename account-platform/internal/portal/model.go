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
	ApprovalV2Eligible bool      `json:"approval_v2_eligible,omitempty"`
	ID                 string    `json:"id"`
	LineageRoot        string    `json:"lineage_root,omitempty"`
	AccessToken        string    `json:"access_token"`
	RefreshToken       string    `json:"refresh_token"`
	AccessExpiresAt    time.Time `json:"access_expires_at"`
	RefreshExpiresAt   time.Time `json:"refresh_expires_at"`
	CSRFToken          string    `json:"csrf_token"`
	CreatedAt          time.Time `json:"created_at"`
	OIDCClientID       string    `json:"oidc_client_id,omitempty"`
	Subject            string    `json:"subject"`
	Nonce              string    `json:"nonce"`
}

type TokenResponse struct {
	IDToken          string `json:"id_token"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
}
