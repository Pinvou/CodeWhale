package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type admissionFaultStore struct {
	*memoryStore
	fail       PortalOperation
	err        error
	operations []PortalOperation
	reserveErr error
}

func (s *admissionFaultStore) AdmitOperation(_ context.Context, op PortalOperation) error {
	s.operations = append(s.operations, op)
	if op == s.fail {
		return s.err
	}
	return nil
}
func (s *admissionFaultStore) ReserveSessionHandoff(context.Context, string, time.Time) error {
	return s.reserveErr
}

func TestPortalAdmissionCoversExplicitRootAndReauthentication(t *testing.T) {
	for _, path := range []string{"/auth/login", "/auth/login?reauthenticate=1", "/auth/login?force_login=1", "/"} {
		t.Run(path, func(t *testing.T) {
			store := &admissionFaultStore{memoryStore: newMemoryStore(), fail: LoginStartOperation, err: &PortalQuotaError{RetryAfter: 1500 * time.Millisecond}}
			service := testService(t, store)
			if err := store.SaveSession(context.Background(), validSession("prior")); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if path != "/" {
				r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: "prior"})
			}
			w := httptest.NewRecorder()
			service.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(w, r)
			if w.Code != 429 || w.Header().Get("Retry-After") != "2" || len(w.Result().Cookies()) != 0 || len(store.flows) != 0 || len(store.operations) != 1 || store.operations[0] != LoginStartOperation {
				t.Fatal("login alias bypassed shared admission or changed cookies/flows", path, w.Code)
			}
			if _, err := store.GetSession(context.Background(), "prior"); err != nil {
				t.Fatal("quota rejection lost prior session", err)
			}
		})
	}
}

func TestPortalCallbackAdmissionPreservesUnconsumedOriginalFlow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{{"quota", &PortalQuotaError{RetryAfter: time.Second}, 429}, {"dependency", errors.New("controlled-private-backend-detail"), 503}} {
		t.Run(tc.name, func(t *testing.T) {
			store := &admissionFaultStore{memoryStore: newMemoryStore(), fail: LoginCallbackOperation, err: tc.err}
			service := testService(t, store)
			original := LoginFlow{Verifier: "original-verifier", Nonce: "original-nonce", ExpiresAt: time.Now().Add(time.Minute)}
			if err := store.SaveFlow(context.Background(), "original-state", original); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=original-state&code=original-code", nil)
			r.AddCookie(&http.Cookie{Name: service.flowCookieName(), Value: "original-state"})
			w := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(w, r)
			if w.Code != tc.status || len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "controlled-private") || len(store.operations) != 1 || store.operations[0] != LoginCallbackOperation {
				t.Fatal("callback rejection changed semantics", w.Code)
			}
			got, err := store.TakeFlow(context.Background(), "original-state")
			if err != nil || got.Verifier != original.Verifier || got.Nonce != original.Nonce || !got.ExpiresAt.Equal(original.ExpiresAt) {
				t.Fatal("rejected callback consumed or rewrote original", err)
			}
			if _, err = store.TakeFlow(context.Background(), "original-state"); !errors.Is(err, ErrSessionNotFound) {
				t.Fatal("original flow lost single-use behavior", err)
			}
		})
	}
}

type admissionRoundTripper func(*http.Request) (*http.Response, error)

func (f admissionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPortalHandoffAdmissionAndReservationPrecedeProviderCalls(t *testing.T) {
	for _, stage := range []string{"operation", "capacity"} {
		t.Run(stage, func(t *testing.T) {
			service, original := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
			store := &admissionFaultStore{memoryStore: original}
			if stage == "operation" {
				store.fail = HandoffCreateOperation
				store.err = &PortalQuotaError{RetryAfter: time.Second}
			} else {
				store.reserveErr = &PortalQuotaError{RetryAfter: time.Second}
			}
			service.store = store
			calls := 0
			service.client.Transport = admissionRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("controlled failure"))}, nil
			})
			r := httptest.NewRequest(http.MethodPost, "/portal-session-handoff", nil)
			r.Header.Set("Authorization", "Bearer access")
			w := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(w, r)
			if w.Code != 429 || calls != 0 || len(store.handoffs) != 0 || len(w.Result().Cookies()) != 0 || len(store.operations) != 1 || store.operations[0] != HandoffCreateOperation {
				t.Fatal("handoff rejected too late or minted a ticket", stage, w.Code, calls)
			}
		})
	}
}

func TestPortalHandoffConsumeQuotaPreservesTicketAndNextAttempt(t *testing.T) {
	service, original := newSessionHandoffTestService(t, "11111111-1111-4111-8111-111111111111")
	store := &admissionFaultStore{memoryStore: original, fail: HandoffConsumeOperation, err: &PortalQuotaError{RetryAfter: time.Second}}
	service.store = store
	ticket := "controlled-original-ticket-0001"
	digest := sha256.Sum256([]byte(ticket))
	id := hex.EncodeToString(digest[:])
	handoff := SessionHandoff{Session: validSession("new-session"), ExpiresAt: time.Now().Add(30 * time.Second)}
	if err := store.SaveSessionHandoff(context.Background(), id, handoff); err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/portal-session-handoff/consume", strings.NewReader(`{"ticket":"`+ticket+`"}`))
		r.Header.Set("Origin", service.config.PublicURL.String())
		r.Header.Set("Content-Type", "application/json")
		return r
	}
	w := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(w, request())
	if w.Code != 429 || len(store.handoffs) != 1 || len(store.sessions) != 0 || len(w.Result().Cookies()) != 0 {
		t.Fatal("quota consumed ticket or replaced browser identity", w.Code)
	}
	store.fail = ""
	w = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(w, request())
	if w.Code != 200 || len(store.handoffs) != 0 {
		t.Fatal("admitted retry could not use untouched original ticket", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(w, request())
	if w.Code != 401 {
		t.Fatal("successful handoff became replayable", w.Code)
	}
}
