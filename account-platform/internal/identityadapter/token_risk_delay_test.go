package identityadapter

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIdentifierRiskProgressiveDelaySequence(t *testing.T) {
	for _, c := range []struct {
		failures int64
		seconds  int
	}{{0, 0}, {4, 0}, {5, 1}, {6, 2}, {7, 4}, {8, 8}, {9, 16}, {10, 30}, {100, 30}} {
		if actual := identifierVerificationDelay(c.failures); actual != time.Duration(c.seconds)*time.Second {
			t.Fatal(c, actual)
		}
	}
}

func TestIdentifierRiskKnownAndUnknownShareDelay(t *testing.T) {
	a, store, _, _ := identifierRiskHTTP(t, 5, 200, 0)
	d := tokenRiskDimensions{action: "password", identifier: "publicvictim"}
	unknown, err := a.Admit(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.values["pinvou:auth-alias:"+a.digest("publicvictim")] = "stable"
	store.values["pinvou:auth-failure:user:stable"] = "9"
	store.mu.Unlock()
	known, err := a.Admit(context.Background(), d)
	if err != nil || known != unknown || known.VerificationDelay != time.Second {
		t.Fatal("known/unknown identifier response diverged", known, unknown, err)
	}
}

type identifierDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *identifierDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestIdentifierRiskDeadlinePreservesEarlierCallerBound(t *testing.T) {
	deadline := time.Now().Add(100 * time.Millisecond)
	parent, stop := context.WithDeadline(context.Background(), deadline)
	defer stop()
	r := httptest.NewRequest("POST", "/", nil).WithContext(parent)
	w := &identifierDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	ctx, cancel, err := tokenVerificationContext(w, r, time.Now(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	actual, ok := ctx.Deadline()
	if !ok || !actual.Equal(deadline) || !w.deadline.Equal(deadline) {
		t.Fatal("caller cancellation/deadline was enlarged", actual, w.deadline)
	}
	if err := waitTokenVerification(ctx, time.Second); err != context.DeadlineExceeded {
		t.Fatal("real wait ignored caller deadline", err)
	}
}

func TestIdentifierRiskUnsupportedDeadlineFailsBeforeVerification(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	ctx, cancel, err := tokenVerificationContext(httptest.NewRecorder(), r, time.Now(), time.Second)
	if err == nil || ctx != nil || cancel != nil {
		t.Fatal("unsupported response transport admitted extended work", ctx, err)
	}
}
func TestIdentifierRiskHardQuotaAndKnownUserRemainHard(t *testing.T) {
	a, store, _, _ := identifierRiskHTTP(t, 5, 200, 0)
	d := tokenRiskDimensions{action: "password", identifier: "publicvictim", source: "public-source", device: "public-device"}
	store.mu.Lock()
	store.scriptRetry = 45
	store.mu.Unlock()
	decision, err := a.Admit(context.Background(), d)
	if err != nil || decision.RetryAfter != 45*time.Second || decision.VerificationDelay != 0 {
		t.Fatal("hard quota changed", decision, err)
	}
	store.mu.Lock()
	store.scriptRetry = 0
	store.values["pinvou:auth-alias:"+a.digest("publicvictim")] = "stable"
	store.values["pinvou:auth-failure:user:stable"] = "10"
	store.mu.Unlock()
	decision, err = a.Admit(context.Background(), d)
	if err != nil || decision.RetryAfter != 15*time.Minute || decision.VerificationDelay != 0 {
		t.Fatal("stable lock changed", decision, err)
	}
	decision, err = a.Admit(context.Background(), tokenRiskDimensions{action: "refresh", identifier: "publicvictim", session: "public-refresh"})
	if err != nil || decision.RetryAfter != 0 || decision.VerificationDelay != 0 {
		t.Fatal("refresh inherited password wait", decision, err)
	}
}
