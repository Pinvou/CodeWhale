package portal

import "time"

type LoginFlow struct {
	Verifier  string    `json:"verifier"`
	Nonce     string    `json:"nonce"`
	ReturnTo  string    `json:"return_to"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Session struct {
	ID               string    `json:"id"`
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	CSRFToken        string    `json:"csrf_token"`
	CreatedAt        time.Time `json:"created_at"`
	Subject          string    `json:"subject"`
	Nonce            string    `json:"nonce"`
}

type TokenResponse struct {
	IDToken          string `json:"id_token"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
}
