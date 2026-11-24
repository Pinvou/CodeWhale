package smsgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const fixtureToken = "public-test-only-token-with-32-characters"

type fakeSender func(context.Context, request, string) sendResult

func (f fakeSender) send(ctx context.Context, input request, template string) sendResult {
	return f(ctx, input, template)
}

func stateDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("durable SMS state requires the Linux container filesystem")
	}
	dir := filepath.Join(t.TempDir(), "private-dispatch")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fixture(t *testing.T, dir string, opts Templates, send fakeSender) *Handler {
	t.Helper()
	h, err := newHandler(fixtureToken, opts, dir, send, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func challenge(purpose string) request {
	return request{Mobile: "+8613800000000", Purpose: purpose, Code: "654321", ChallengeID: uuid.Must(uuid.NewV7()).String()}
}

func dispatch(h *Handler, input request, token string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(input)
	r := httptest.NewRequest(http.MethodPost, "/v1/sms/send", bytes.NewReader(body))
	r.Header.Set("Idempotency-Key", input.ChallengeID)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPurposeRoutesAllFourTemplateCodes(t *testing.T) {
	opts := Templates{Register: "SMS_101", Login: "SMS_102", ProfileMobile: "SMS_103", PasswordReset: "SMS_104"}
	expected := map[string]string{"REGISTER": "SMS_101", "LOGIN": "SMS_102", "PROFILE_MOBILE": "SMS_103", "PASSWORD_RESET": "SMS_104"}
	for _, common := range []bool{false, true} {
		if common {
			opts = Templates{"SMS_200", "SMS_200", "SMS_200", "SMS_200"}
		}
		for purpose, want := range expected {
			if common {
				want = "SMS_200"
			}
			input := challenge(purpose)
			count := 0
			h := fixture(t, stateDir(t), opts, func(_ context.Context, req request, template string) sendResult {
				count++
				if req != input || template != want {
					t.Errorf("wrong message mapping for %s", purpose)
				}
				return sendResult{MessageID: "test-receipt"}
			})
			if got := dispatch(h, input, fixtureToken); got.Code != 200 || !strings.Contains(got.Body.String(), "test-receipt") || count != 1 {
				t.Fatalf("expected accepted dispatch for %s", purpose)
			}
		}
	}
}

func TestReplayAfterRestartPreservesAcceptedReceiptAndNeverResends(t *testing.T) {
	dir := stateDir(t)
	opts := Templates{"SMS_1", "SMS_2", "SMS_3", "SMS_4"}
	input := challenge("REGISTER")
	count := 0
	send := func(_ context.Context, _ request, _ string) sendResult {
		count++
		return sendResult{MessageID: "cloud-receipt"}
	}
	h := fixture(t, dir, opts, send)
	w := dispatch(h, input, fixtureToken)
	if w.Code != http.StatusOK || count != 1 {
		t.Fatalf("initial request returned %d", w.Code)
	}
	restarted := fixture(t, dir, Templates{"SMS_5", "SMS_6", "SMS_7", "SMS_8"}, send)
	replay := dispatch(restarted, input, fixtureToken)
	if replay.Code != http.StatusOK || replay.Body.String() != w.Body.String() || count != 1 || !restarted.store.nextPrune.IsZero() {
		t.Fatal("restart lost the accepted receipt")
	}
	input.Code = "123456"
	if w := dispatch(restarted, input, fixtureToken); w.Code != http.StatusConflict || count != 1 {
		t.Fatal("different request reused an accepted challenge")
	}
	record := filepath.Join(dir, input.ChallengeID+".json")
	data, err := os.ReadFile(record)
	if err != nil || bytes.Contains(data, []byte(input.Mobile)) || bytes.Contains(data, []byte("654321")) {
		t.Fatal("dispatch record contained the phone number or the verification code")
	}
}

func TestUnknownOutcomeRemainsUnconfirmedAcrossConcurrentCallsAndRestart(t *testing.T) {
	dir := stateDir(t)
	opts := Templates{"SMS_1", "SMS_2", "SMS_3", "SMS_4"}
	input := challenge("LOGIN")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := fixture(t, dir, opts, func(_ context.Context, _ request, _ string) sendResult {
		once.Do(func() { close(started) })
		<-release
		return sendResult{}
	})
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- dispatch(h, input, fixtureToken) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first dispatch did not reach the sender")
	}
	replay := fixture(t, dir, opts, func(_ context.Context, _ request, _ string) sendResult {
		t.Error("duplicate or uncertain request attempted to send")
		return sendResult{}
	})
	if w := dispatch(replay, input, fixtureToken); w.Code != http.StatusServiceUnavailable {
		t.Errorf("concurrent retry returned %d", w.Code)
	}
	close(release)
	if w := <-first; w.Code != http.StatusServiceUnavailable {
		t.Errorf("unknown result returned %d", w.Code)
	}
	if w := dispatch(replay, input, fixtureToken); w.Code != http.StatusServiceUnavailable {
		t.Errorf("replayed unknown result returned %d", w.Code)
	}
}

func TestAuthorizationExpiryAndExplicitRejection(t *testing.T) {
	dir := stateDir(t)
	opts := Templates{"SMS_1", "SMS_2", "SMS_3", "SMS_4"}
	input := challenge("PASSWORD_RESET")
	calls := 0
	h := fixture(t, dir, opts, func(_ context.Context, _ request, _ string) sendResult {
		calls++
		return sendResult{Rejected: true}
	})
	if w := dispatch(h, input, "invalid"); w.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatal("unauthorized request reached sender")
	}
	h.now = func() time.Time { return time.Now().Add(15 * time.Minute) }
	if w := dispatch(h, input, fixtureToken); w.Code != http.StatusBadRequest || calls != 0 {
		t.Fatal("expired challenge reached sender")
	}
	h.now = time.Now
	if a, b := dispatch(h, input, fixtureToken), dispatch(h, input, fixtureToken); a.Code != 422 || b.Code != 422 || calls != 1 {
		t.Fatal("expected one explicit rejection and a stable replay")
	}
}

func TestReadinessRequiresUsableStateAndLivenessDoesNot(t *testing.T) {
	dir := stateDir(t)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dir); err == nil {
		t.Fatal("a read-only directory passed the startup check")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	h := fixture(t, dir, Templates{"SMS_1", "SMS_2", "SMS_3", "SMS_4"}, func(_ context.Context, _ request, _ string) sendResult {
		t.Fatal("health checks must never send an SMS")
		return sendResult{}
	})
	check := func(path string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Fatalf("%s returned %d, want %d", path, w.Code, want)
		}
	}
	check("/health/ready", http.StatusOK)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("readiness left state files behind")
	}
	if err := os.Rename(dir, dir+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	check("/health/ready", http.StatusServiceUnavailable)
	check("/health/live", http.StatusOK)
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAliyunSendRequestUsesProductionAPIAndExpectedWireFields(t *testing.T) {
	a, err := NewAliyun("fixture-id", "fixture-secret", "测试签名")
	if err != nil {
		t.Fatal(err)
	}
	input := challenge("REGISTER")
	a.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if r.URL.Scheme != "https" || r.URL.Host != smsHost || r.Method != http.MethodPost ||
			r.Header.Get("X-Acs-Action") != "SendSms" || r.Header.Get("X-Acs-Version") != "2017-05-25" ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "ACS3-HMAC-SHA256 Credential=fixture-id,") ||
			q.Get("TemplateCode") != "SMS_100" || q.Get("SignName") != "测试签名" ||
			q.Get("PhoneNumbers") != "13800000000" || q.Get("TemplateParam") != `{"code":"654321"}` || q.Has("OutId") ||
			r.Header.Get("Idempotency-Key") != "" {
			t.Error("unexpected request contract")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"Code":"OK","BizId":"9999^0"}`))}, nil
	})
	if result := a.send(context.Background(), input, "SMS_100"); result.Rejected || result.MessageID != "9999^0" {
		t.Fatal("provider receipt changed")
	}
	a.client.Transport = transportFunc(func(_ *http.Request) (*http.Response, error) { return nil, errors.New("transport closed") })
	if result := a.send(context.Background(), input, "SMS_100"); result.Rejected || result.MessageID != "" {
		t.Fatal("transport error is not a definite rejection")
	}
}

func TestAliyunResponseMustFitWithinCompleteBodyLimit(t *testing.T) {
	a, err := NewAliyun("fixture-id", "fixture-secret", "测试签名")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		body string
		want sendResult
	}{
		{`{"Code":"OK","BizId":"test-receipt"}`, sendResult{MessageID: "test-receipt"}},
		{`{"Code":"isv.MOBILE_NUMBER_ILLEGAL"}`, sendResult{Rejected: true}},
	} {
		atLimit := fixture.body + strings.Repeat(" ", maxResponseBytes-len(fixture.body))
		for _, test := range []struct {
			body string
			want sendResult
		}{
			{atLimit, fixture.want},
			{atLimit + " ", sendResult{}},
			{atLimit + "{}", sendResult{}},
		} {
			a.client.Transport = transportFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			if got := a.send(context.Background(), challenge("REGISTER"), "SMS_100"); got != test.want {
				t.Fatalf("response of %d bytes returned %+v, want %+v", len(test.body), got, test.want)
			}
		}
	}
}
