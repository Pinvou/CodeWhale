package identityadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/passwordpreparation"
	"github.com/google/uuid"
)

type preparationClaimFunc func(context.Context, passwordpreparation.Claim) (passwordpreparation.Facts, error)

func (f preparationClaimFunc) Claim(ctx context.Context, c passwordpreparation.Claim) (passwordpreparation.Facts, error) {
	return f(ctx, c)
}

type pausedPreparationBody struct {
	once    sync.Once
	started chan struct{}
	release <-chan struct{}
	reader  io.Reader
}

func (r *pausedPreparationBody) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.reader.Read(p)
}

// Pause real handler I/O at a known barrier, then require an unrelated grant
// to finish before releasing it. A count-only parallel replay test cannot
// distinguish correct grant ownership from a single global queue.
func TestPasswordPreparationUnrelatedGrantsProgress(t *testing.T) {
	for _, stage := range []string{"authority-claim", "request-body"} {
		t.Run(stage, func(t *testing.T) {
			service, fixture, firstToken := preparationFixture(t)
			secondID := uuid.New()
			secondToken := secondID.String() + ".PUBLIC_SYNTHETIC_MAC"
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			firstFacts := fixture.facts
			service.preparations.verifier = preparationClaimFunc(func(_ context.Context, c passwordpreparation.Claim) (passwordpreparation.Facts, error) {
				facts := firstFacts
				switch c.Token {
				case firstToken:
					if stage == "authority-claim" {
						close(started)
						<-release
					}
				case secondToken:
					facts.ID = secondID
				default:
					return passwordpreparation.Facts{}, ErrPasswordPreparation
				}
				return facts, nil
			})
			firstDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				if stage == "authority-claim" {
					firstDone <- preparationHTTP(service, firstToken, "FixturePassword9")
					return
				}
				body := &pausedPreparationBody{started: started, release: release, reader: strings.NewReader(`{"password":"FixturePassword9"}`)}
				r := httptest.NewRequest(http.MethodPost, "/v1/registration/password-prechecks", body)
				r.RemoteAddr = "192.0.2.10:4242"
				r.Header.Set("X-Device-ID", "fixture-device")
				r.Header.Set("X-Pinvou-Password-Preparation", firstToken)
				w := httptest.NewRecorder()
				service.Handler().ServeHTTP(w, r)
				firstDone <- w
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("first request did not reach controlled I/O barrier")
			}
			secondDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { secondDone <- preparationHTTP(service, secondToken, "FixturePassword9") }()
			var second *httptest.ResponseRecorder
			blocked := false
			select {
			case second = <-secondDone:
			case <-time.After(3 * time.Second):
				blocked = true
			}
			unblock()
			var first *httptest.ResponseRecorder
			select {
			case first = <-firstDone:
			case <-time.After(3 * time.Second):
				t.Fatal("first request did not recover after I/O release")
			}
			if blocked {
				select {
				case second = <-secondDone:
				case <-time.After(3 * time.Second):
					t.Fatal("second request did not recover after release")
				}
				t.Error("unrelated admitted grant waited on another grant's I/O")
			}
			if first.Code != 201 || second.Code != 201 {
				t.Fatalf("admitted requests: first=%d second=%d", first.Code, second.Code)
			}
			var firstRef, secondRef PasswordReference
			if json.Unmarshal(first.Body.Bytes(), &firstRef) != nil || json.Unmarshal(second.Body.Bytes(), &secondRef) != nil || firstRef.Handle == "" || firstRef.Handle == secondRef.Handle {
				t.Fatal("different grants must retain distinct successful references")
			}
			// Subsequent same-grant replay still returns the original reference;
			// changed passwords still conflict rather than replacing the secret.
			replay := preparationHTTP(service, secondToken, "FixturePassword9")
			var replayRef PasswordReference
			if replay.Code != 201 || json.Unmarshal(replay.Body.Bytes(), &replayRef) != nil || replayRef != secondRef {
				t.Fatal("same grant replay changed the allocated reference")
			}
			if changed := preparationHTTP(service, secondToken, "DifferentPassword8"); changed.Code != 409 {
				t.Fatal("same grant accepted a changed secret", changed.Code)
			}
			service.preparations.mu.Lock()
			locks := len(service.preparations.locks)
			records := len(service.preparations.records)
			service.preparations.mu.Unlock()
			if locks != 0 || records != 2 {
				t.Fatal("in-flight bookkeeping leaked or lost completed grants", locks, records)
			}
		})
	}
}

func TestPasswordPreparationFailedClaimReleasesGrantOwnership(t *testing.T) {
	service, fixture, token := preparationFixture(t)
	fixture.err = errors.New("controlled authority unavailable")
	if w := preparationHTTP(service, token, "FixturePassword9"); w.Code != 503 {
		t.Fatal("failed claim response", w.Code)
	}
	service.preparations.mu.Lock()
	locks, records := len(service.preparations.locks), len(service.preparations.records)
	service.preparations.mu.Unlock()
	if locks != 0 || records != 0 || len(service.vault.records) != 0 {
		t.Fatal("failed claim retained a lock, reference or password", locks, records)
	}
	fixture.err = nil
	if w := preparationHTTP(service, token, "FixturePassword9"); w.Code != 201 {
		t.Fatal("same grant could not retry after authority recovery", w.Code)
	}
}
