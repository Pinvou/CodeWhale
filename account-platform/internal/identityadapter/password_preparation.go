package identityadapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/passwordpreparation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrPasswordPreparation = errors.New("password preparation requires an admitted challenge")
var ErrPasswordPreparationUnavailable = errors.New("password preparation authority result is unavailable")

type PasswordPreparationVerifier interface {
	Claim(context.Context, passwordpreparation.Claim) (passwordpreparation.Facts, error)
}

type HTTPPasswordPreparationVerifier struct {
	endpoint   string
	credential string
	client     *http.Client
}

func NewHTTPPasswordPreparationVerifier(rawURL, credential, caFile string) (*HTTPPasswordPreparationVerifier, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" || parsed.Path != "" && parsed.Path != "/" || parsed.Scheme != "https" && parsed.Scheme != "http" || strings.TrimSpace(credential) == "" {
		return nil, ErrPasswordPreparation
	}
	client, err := serviceauth.NewWorkloadHTTPClient(caFile)
	if err != nil {
		return nil, err
	}
	return &HTTPPasswordPreparationVerifier{endpoint: strings.TrimRight(parsed.String(), "/") + "/internal/v1/auth/password-preparation-authorizations/claim", credential: credential, client: client}, nil
}

func (v *HTTPPasswordPreparationVerifier) Claim(ctx context.Context, claim passwordpreparation.Claim) (passwordpreparation.Facts, error) {
	var facts passwordpreparation.Facts
	raw, _ := json.Marshal(claim)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, bytes.NewReader(raw))
	if err != nil {
		return facts, err
	}
	request.Header.Set("Content-Type", "application/json")
	if err = serviceauth.SetAuthorization(request, v.credential); err != nil {
		return facts, err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return facts, ErrPasswordPreparationUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Code string `json:"code"`
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
		if response.StatusCode == http.StatusForbidden && decoder.Decode(&failure) == nil && decoder.Decode(new(any)) == io.EOF && failure.Code == "PASSWORD_PREPARATION_INVALID" {
			return facts, ErrPasswordPreparation
		}
		return facts, ErrPasswordPreparationUnavailable
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&facts) != nil || decoder.Decode(new(any)) != io.EOF || facts.ID == uuid.Nil || !strings.HasPrefix(claim.Token, facts.ID.String()+".") || facts.Mobile == "" || facts.Purpose != "REGISTER" && facts.Purpose != "PASSWORD_RESET" || facts.ExpiresAt.IsZero() {
		return passwordpreparation.Facts{}, ErrPasswordPreparationUnavailable
	}
	return facts, nil
}

type passwordPreparationState struct {
	mu          sync.Mutex
	verifier    PasswordPreparationVerifier
	incarnation uuid.UUID
	peers       map[netip.Addr]struct{}
	records     map[uuid.UUID]preparedPassword
	locks       map[uuid.UUID]*passwordPreparationLock
}

// A grant has one local allocation/replay owner. The map mutex only protects
// in-memory bookkeeping, never authority calls, request bodies or responses.
// References include waiters, so removing an idle lock cannot split ownership.
type passwordPreparationLock struct {
	mu    sync.Mutex
	users int
}

func (s *passwordPreparationState) lockGrant(id uuid.UUID) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[uuid.UUID]*passwordPreparationLock)
	}
	lock := s.locks[id]
	if lock == nil {
		lock = &passwordPreparationLock{}
		s.locks[id] = lock
	}
	lock.users++
	s.mu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.mu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(s.locks, id)
		}
		s.mu.Unlock()
	}
}

type preparedPassword struct {
	tokenDigest    [32]byte
	facts          passwordpreparation.Facts
	source, device string
	reference      PasswordReference
}

func (s *Service) WithPasswordPreparation(verifier PasswordPreparationVerifier, rawPeers string) (*Service, error) {
	peers, err := parseTrustedTokenProxyIPs(rawPeers)
	if err != nil || verifier == nil {
		return nil, ErrPasswordPreparation
	}
	s.preparations = &passwordPreparationState{verifier: verifier, incarnation: uuid.New(), peers: peers, records: make(map[uuid.UUID]preparedPassword)}
	return s, nil
}

func (s *passwordPreparationState) source(r *http.Request) (string, string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", "", ErrPasswordPreparation
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || peer.Zone() != "" {
		return "", "", ErrPasswordPreparation
	}
	peer = peer.Unmap()
	source := peer.String()
	if _, ok := s.peers[peer]; ok {
		values := r.Header.Values("X-Pinvou-Client-Source")
		if len(values) == 1 {
			if value, err := netip.ParseAddr(strings.TrimSpace(values[0])); err == nil && value.Zone() == "" {
				source = value.Unmap().String()
			}
		}
	}
	device := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if device == "" || len(device) > 128 {
		device = "device:absent:" + source
	}
	return source, device, nil
}

func (s *Service) preparePassword(w http.ResponseWriter, r *http.Request) {
	state := s.preparations
	// Missing/invalid admission is rejected before decoding or retaining a password.
	tokens := r.Header.Values("X-Pinvou-Password-Preparation")
	if len(tokens) != 1 || len(tokens[0]) > 256 {
		s.writeError(w, r, ErrPasswordPreparation)
		return
	}
	if state == nil {
		s.writeError(w, r, ErrPasswordPreparationUnavailable)
		return
	}
	token := strings.TrimSpace(tokens[0])
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		s.writeError(w, r, ErrPasswordPreparation)
		return
	}
	id, err := uuid.Parse(parts[0])
	if err != nil || id == uuid.Nil {
		s.writeError(w, r, ErrPasswordPreparation)
		return
	}
	source, device, err := state.source(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Serialize only this grant; unrelated admitted requests must not wait on
	// authority I/O or a slow body. Account still serializes replicas.
	unlock := state.lockGrant(id)
	defer unlock()
	state.mu.Lock()
	for key, saved := range state.records {
		if !saved.reference.ExpiresAt.After(s.vault.now()) {
			delete(state.records, key)
		}
	}
	digest := sha256.Sum256([]byte(token))
	saved, exists := state.records[id]
	state.mu.Unlock()
	var facts passwordpreparation.Facts
	if exists {
		if !hmac.Equal(saved.tokenDigest[:], digest[:]) || saved.source != source || saved.device != device {
			s.writeError(w, r, ErrPasswordPreparation)
			return
		}
		facts = saved.facts
	} else {
		facts, err = state.verifier.Claim(r.Context(), passwordpreparation.Claim{Token: token, Owner: state.incarnation, Source: source, Device: device})
		if err != nil {
			if !errors.Is(err, ErrPasswordPreparation) {
				err = ErrPasswordPreparationUnavailable
			}
			s.writeError(w, r, err)
			return
		}
		if facts.ID != id || !facts.ExpiresAt.After(s.vault.now()) {
			s.writeError(w, r, ErrPasswordPreparation)
			return
		}
	}
	var body struct {
		Password string `json:"password"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a complete JSON request is required", "")
		return
	}
	if exists {
		record, err := s.vault.record(saved.reference.Handle, saved.reference.PolicyVersion)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		record.mu.Lock()
		same := hmac.Equal(record.fingerprint[:], s.vault.fingerprintBytes(body.Password))
		record.mu.Unlock()
		if !same {
			s.writeError(w, r, ErrReferenceConflict)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, http.StatusCreated, saved.reference)
		return
	}
	reference, err := s.vault.precheckBefore(body.Password, facts.ExpiresAt)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	record, err := s.vault.record(reference.Handle, reference.PolicyVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	record.mu.Lock()
	record.preparation = passwordpreparation.Reference{ID: facts.ID, Mobile: facts.Mobile, Purpose: facts.Purpose}
	record.mu.Unlock()
	state.mu.Lock()
	state.records[id] = preparedPassword{tokenDigest: digest, facts: facts, source: source, device: device, reference: reference}
	state.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, reference)
}

func (v *PasswordVault) fingerprintBytes(password string) []byte {
	value := v.fingerprint(password)
	return value[:]
}

func (s *Service) passwordReferencePreparation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Handle string `json:"password_handle"`
		Policy string `json:"password_policy_version"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		s.writeError(w, r, ErrReferenceInvalid)
		return
	}
	record, err := s.vault.record(body.Handle, body.Policy)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	record.mu.Lock()
	facts := record.preparation
	facts.Legacy = facts.ID == uuid.Nil
	record.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, facts)
}

func (v *PasswordVault) validatePreparedPurpose(handle, policy, purpose, mobile string) error {
	record, err := v.record(handle, policy)
	if err != nil {
		return err
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	// Existing accepted records without preparation metadata remain usable until their original expiry.
	if record.preparation.ID != uuid.Nil && (record.preparation.Purpose != purpose || record.preparation.Mobile != mobile) {
		return ErrReferenceConflict
	}
	return nil
}
