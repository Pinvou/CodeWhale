package paymentcallback

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	Scope    = "payment.callback.write"
	Audience = "pinvou-payment-callback"
)

var ErrUnauthorized = errors.New("payment callback client is unauthorized")

type ClientConfig struct {
	ClientID        string
	ClientSecret    string
	ProviderID      string
	ProviderVersion string
	MerchantID      string
}

type AuthConfig struct {
	Clients    []ClientConfig
	SigningKey []byte
	KeyID      string
	TokenTTL   time.Duration
}

type Principal struct {
	ClientID        string
	ProviderID      string
	ProviderVersion string
	MerchantID      string
}

type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

type client struct {
	principal Principal
	secret    []byte
}

type Authenticator struct {
	clients    map[string]client
	signingKey []byte
	keyID      string
	tokenTTL   time.Duration
	now        func() time.Time
}

type tokenClaims struct {
	Issuer          string `json:"iss"`
	Subject         string `json:"sub"`
	Audience        string `json:"aud"`
	Scope           string `json:"scope"`
	ProviderID      string `json:"provider_id"`
	ProviderVersion string `json:"provider_version"`
	MerchantID      string `json:"merchant_id"`
	IssuedAt        int64  `json:"iat"`
	Expires         int64  `json:"exp"`
}

func NewAuthenticator(config AuthConfig) (*Authenticator, error) {
	config.KeyID = strings.TrimSpace(config.KeyID)
	if len(config.Clients) == 0 || len(config.SigningKey) < 32 || config.KeyID == "" {
		return nil, errors.New("payment callback authentication requires clients, a 256-bit signing key, and a key ID")
	}
	clients := make(map[string]client, len(config.Clients))
	for _, configured := range config.Clients {
		principal := Principal{
			ClientID: strings.TrimSpace(configured.ClientID), ProviderID: strings.TrimSpace(configured.ProviderID),
			ProviderVersion: strings.TrimSpace(configured.ProviderVersion), MerchantID: strings.TrimSpace(configured.MerchantID),
		}
		if principal.ClientID == "" || len(configured.ClientSecret) < 32 || principal.ProviderID == "" || principal.ProviderVersion == "" || principal.MerchantID == "" {
			return nil, errors.New("payment callback clients require an ID, 32-character secret, provider identity, version, and merchant ID")
		}
		if _, exists := clients[principal.ClientID]; exists {
			return nil, errors.New("payment callback client IDs must be unique")
		}
		clients[principal.ClientID] = client{principal: principal, secret: []byte(configured.ClientSecret)}
	}
	if config.TokenTTL <= 0 {
		config.TokenTTL = 5 * time.Minute
	}
	if config.TokenTTL < time.Minute || config.TokenTTL > 15*time.Minute {
		return nil, errors.New("payment callback token TTL must be between 1 and 15 minutes")
	}
	return &Authenticator{
		clients: clients, signingKey: append([]byte(nil), config.SigningKey...), keyID: config.KeyID,
		tokenTTL: config.TokenTTL, now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (a *Authenticator) Issue(clientID, clientSecret, requestedScope string) (Token, error) {
	clientID, requestedScope = strings.TrimSpace(clientID), strings.TrimSpace(requestedScope)
	if requestedScope == "" {
		requestedScope = Scope
	}
	configured, ok := a.clients[clientID]
	if !ok || len(clientSecret) != len(configured.secret) || subtle.ConstantTimeCompare([]byte(clientSecret), configured.secret) != 1 || requestedScope != Scope {
		return Token{}, ErrUnauthorized
	}
	now := a.now().UTC()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": a.keyID})
	claims, _ := json.Marshal(tokenClaims{
		Issuer: "pinvou-billing", Subject: configured.principal.ClientID, Audience: Audience, Scope: Scope,
		ProviderID: configured.principal.ProviderID, ProviderVersion: configured.principal.ProviderVersion,
		MerchantID: configured.principal.MerchantID, IssuedAt: now.Unix(), Expires: now.Add(a.tokenTTL).Unix(),
	})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return Token{AccessToken: unsigned + "." + sign(a.signingKey, unsigned), TokenType: "Bearer", ExpiresIn: int64(a.tokenTTL.Seconds()), Scope: Scope}, nil
}

func (a *Authenticator) VerifyBearer(authorization string) (Principal, error) {
	parts := strings.Fields(strings.TrimSpace(authorization))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 4096 {
		return Principal{}, ErrUnauthorized
	}
	segments := strings.Split(parts[1], ".")
	if len(segments) != 3 {
		return Principal{}, ErrUnauthorized
	}
	unsigned := segments[0] + "." + segments[1]
	expected := sign(a.signingKey, unsigned)
	if len(segments[2]) != len(expected) || subtle.ConstantTimeCompare([]byte(segments[2]), []byte(expected)) != 1 {
		return Principal{}, ErrUnauthorized
	}
	headerBytes, headerErr := base64.RawURLEncoding.DecodeString(segments[0])
	claimsBytes, claimsErr := base64.RawURLEncoding.DecodeString(segments[1])
	var header map[string]string
	var claims tokenClaims
	if headerErr != nil || claimsErr != nil || json.Unmarshal(headerBytes, &header) != nil || json.Unmarshal(claimsBytes, &claims) != nil {
		return Principal{}, ErrUnauthorized
	}
	configured, ok := a.clients[claims.Subject]
	now := a.now().UTC().Unix()
	if !ok || header["alg"] != "HS256" || header["typ"] != "JWT" || header["kid"] != a.keyID || claims.Issuer != "pinvou-billing" || claims.Audience != Audience || claims.Scope != Scope || claims.IssuedAt > now+60 || claims.Expires <= now || claims.Expires-claims.IssuedAt > int64((15*time.Minute).Seconds()) || claims.ProviderID != configured.principal.ProviderID || claims.ProviderVersion != configured.principal.ProviderVersion || claims.MerchantID != configured.principal.MerchantID {
		return Principal{}, ErrUnauthorized
	}
	return configured.principal, nil
}

func sign(key []byte, value string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
