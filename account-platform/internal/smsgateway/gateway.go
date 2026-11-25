package smsgateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	challengeLifetime = 5 * time.Minute
	clockAllowance    = 30 * time.Second
)

var (
	mobileNumber     = regexp.MustCompile("^[+]861[3-9][0-9]{9}$")
	verificationCode = regexp.MustCompile("^[0-9]{6}$")
	templateCode     = regexp.MustCompile("^SMS_[0-9]+$")
	receiptID        = regexp.MustCompile("^[a-zA-Z0-9_.^-]{1,128}$")
)

type Templates struct {
	Register      string
	Login         string
	ProfileMobile string
	PasswordReset string
}

func (t Templates) forPurpose(purpose string) string {
	switch purpose {
	case "REGISTER":
		return t.Register
	case "LOGIN":
		return t.Login
	case "PROFILE_MOBILE":
		return t.ProfileMobile
	case "PASSWORD_RESET":
		return t.PasswordReset
	default:
		return ""
	}
}

type request struct {
	Mobile      string `json:"mobile"`
	Purpose     string `json:"purpose"`
	Code        string `json:"code"`
	ChallengeID string `json:"challenge_id"`
}

type sendResult struct {
	MessageID string
	Rejected  bool
}

type sender interface {
	send(context.Context, request, string) sendResult
}

type Handler struct {
	tokenHash      [32]byte
	fingerprintKey []byte
	templates      Templates
	store          *store
	sender         sender
	now            func() time.Time
}

func NewHandler(token string, templates Templates, stateDirectory string, aliyun *Aliyun) (*Handler, error) {
	if aliyun == nil {
		return nil, errors.New("missing SMS sender")
	}
	return newHandler(token, templates, stateDirectory, aliyun, time.Now)
}

func newHandler(token string, templates Templates, stateDirectory string, sender sender, now func() time.Time) (*Handler, error) {
	if len(token) < 32 || len(token) > 512 || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("invalid SMS gateway bearer token")
	}
	for _, code := range []string{templates.Register, templates.Login, templates.ProfileMobile, templates.PasswordReset} {
		if !templateCode.MatchString(code) {
			return nil, errors.New("configure all four Aliyun SMS template codes")
		}
	}
	store, err := openStore(stateDirectory)
	if err != nil {
		return nil, err
	}
	return &Handler{tokenHash: sha256.Sum256([]byte("Bearer " + token)), fingerprintKey: []byte(token), templates: templates, store: store, sender: sender, now: now}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/health/ready" || r.URL.Path == "/health/live" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/health/ready" && h.store.probe() != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != "/v1/sms/send" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	receivedHash := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(h.tokenHash[:], receivedHash[:]) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var input request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(input.ChallengeID)
	template := h.templates.forPurpose(input.Purpose)
	if err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 || id.String() != input.ChallengeID ||
		len(r.Header.Values("Idempotency-Key")) != 1 || r.Header.Get("Idempotency-Key") != input.ChallengeID ||
		!mobileNumber.MatchString(input.Mobile) || !verificationCode.MatchString(input.Code) || template == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	seconds, nanos := id.Time().UnixTime()
	createdAt := time.Unix(seconds, nanos)
	now := h.now()
	if createdAt.After(now.Add(clockAllowance)) || !createdAt.Add(challengeLifetime+clockAllowance).After(now) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	fingerprint := h.fingerprint(input)
	record, reserved, err := h.store.reserve(id, fingerprint, now)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !hmac.Equal([]byte(record.Fingerprint), []byte(fingerprint)) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if reserved {
		result := h.sender.send(r.Context(), input, template)
		switch {
		case result.Rejected:
			record.State = rejected
		case receiptID.MatchString(result.MessageID):
			record.State = accepted
			record.MessageID = result.MessageID
		}
		if record.State != uncertain && h.store.complete(id, record) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	switch record.State {
	case accepted:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			MessageID string `json:"message_id"`
		}{record.MessageID})
	case rejected:
		w.WriteHeader(http.StatusUnprocessableEntity)
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (h *Handler) fingerprint(input request) string {
	payload, _ := json.Marshal(input)
	mac := hmac.New(sha256.New, h.fingerprintKey)
	_, _ = mac.Write([]byte("pinvou-enterprise-sms-v1\x00"))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
