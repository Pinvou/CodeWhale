package token

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Controlled shared persistence models two Service instances. The actual
// owner renewal loop, registry and HTTP adapter execute; this is not the public
// chat entry point, a real worker transaction, PostgreSQL or real money proof.
// dispatchUsage's production four-minute ticker has no injection seam. We use
// the loop's existing tick channel, never a second concurrent dispatch loop.
type remoteOwnerRepository struct {
	Repository
	mu                sync.Mutex
	requested         bool
	origin            string
	active            bool
	readErr, leaseErr error
	reads, renews     int
	renewed, read     chan struct{}
	budget            time.Time
	badBudget         bool
}

func (r *remoteOwnerRepository) RenewDispatchLease(ctx context.Context, _ uuid.UUID, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renews++
	r.budget, _ = ctx.Deadline()
	if r.budget.IsZero() || time.Until(r.budget) > dispatchLeaseWriteTimeout+time.Second {
		r.badBudget = true
	}
	r.renewed <- struct{}{}
	return r.active, r.leaseErr
}
func (r *remoteOwnerRepository) GetUsage(ctx context.Context, id uuid.UUID) (UsageRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	deadline, _ := ctx.Deadline()
	if !deadline.Equal(r.budget) {
		r.badBudget = true
	}
	r.read <- struct{}{}
	return UsageRequest{ID: id, EmergencyCancellationRequested: r.requested, CancellationOrigin: r.origin, CancelStatus: "UNSUPPORTED", DeliveryStatus: "USER_STOPPED"}, r.readErr
}

func remoteOwnerWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("required owner-loop/HTTP transport observation missing")
	}
}

func TestRemoteModelCancellationOwnerLoopTransport(t *testing.T) {
	for _, mode := range []string{"remote_emergency", "remote_emergency_then_user", "user_only", "read_failure", "lease_failure", "natural_terminal"} {
		t.Run(mode, func(t *testing.T) {
			repo := &remoteOwnerRepository{active: true, renewed: make(chan struct{}, 8), read: make(chan struct{}, 8)}
			funds := &streamBillingFake{}
			a, b := &Service{repository: repo, executionBilling: funds}, &Service{repository: repo, executionBilling: funds}
			id := uuid.New()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var cancellations atomic.Int32
			abort := func() { cancellations.Add(1); cancel() }
			unregister := a.registerActiveUpstream(id, abort)
			defer unregister()
			ticks := make(chan time.Time, 2)
			failed, stop, err := a.renewDispatchLease(t.Context(), abort, id, "original-owner", ticks)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			remoteOwnerWait(t, repo.renewed)
			// Initial renewal is synchronous; v6 has observed false before Do.
			// Do not gate transport start on that new read: the old-loop negative
			// must reach an actual provider before proving it misses cancellation.
			select {
			case <-repo.read:
			default:
			}
			started, providerDone, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var finishOnce sync.Once
			releaseProvider := func() { finishOnce.Do(func() { close(finish) }) }
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
				close(started)
				defer close(providerDone)
				select {
				case <-req.Context().Done():
				case <-finish:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"choices":[]}`))
				}
			}))
			defer provider.Close()
			defer releaseProvider()
			upstreamDone := make(chan error, 1)
			go func() {
				response, err := NewHTTPUpstreamClient().Do(ctx, provider.URL, []byte("controlled"), []byte(`{"model":"controlled"}`), id)
				if response != nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
				upstreamDone <- err
			}()
			remoteOwnerWait(t, started)
			// Only B observes/commits the remote event. Its local registry must
			// miss A's transport; a persistent observation is therefore required.
			repo.mu.Lock()
			switch mode {
			case "remote_emergency":
				repo.requested = true
				repo.origin = "MODEL_EMERGENCY"
			case "remote_emergency_then_user":
				repo.requested = true
				repo.origin = "USER"
			case "user_only":
				repo.origin = "USER"
			case "read_failure":
				repo.readErr = errors.New("controlled persistent read failure")
			case "lease_failure":
				repo.leaseErr = ErrForbidden
			case "natural_terminal":
				repo.active = false
			}
			repo.mu.Unlock()
			if b.abortActiveUpstream(id) {
				t.Fatal("other Service unexpectedly owned local transport")
			}
			ticks <- time.Now()
			remoteOwnerWait(t, repo.renewed)
			cancelExpected := mode != "user_only" && mode != "natural_terminal"
			if cancelExpected {
				remoteOwnerWait(t, ctx.Done())
				remoteOwnerWait(t, providerDone)
				if err := <-upstreamDone; err == nil {
					t.Fatal("cancelled owner adapter reported success")
				}
				if cancellations.Load() != 1 {
					t.Fatal("owner context cancellation was not exactly once")
				}
				select {
				case <-failed:
					if mode != "read_failure" && mode != "lease_failure" {
						t.Fatal("requested cancellation misclassified as lease failure")
					}
				default:
					if mode == "read_failure" || mode == "lease_failure" {
						t.Fatal("lease/read failure contract lost")
					}
				}
			} else {
				if mode == "user_only" {
					remoteOwnerWait(t, repo.read)
				}
				if ctx.Err() != nil || cancellations.Load() != 0 {
					t.Fatal("USER-only or terminal observation aborted provider")
				}
				select {
				case <-failed:
					t.Fatal("healthy/terminal owner reported failure")
				default:
				}
				releaseProvider()
				remoteOwnerWait(t, providerDone)
				if err := <-upstreamDone; err != nil {
					t.Fatal(err)
				}
			}
			stop()
			repo.mu.Lock()
			reads, renews, bad := repo.reads, repo.renews, repo.badBudget
			repo.mu.Unlock()
			if bad {
				t.Fatal("renew and authority read did not share original bounded budget")
			}
			if mode != "natural_terminal" && mode != "lease_failure" && reads != 2 {
				t.Fatal("initial and tick authority observations missing")
			}
			if (mode == "natural_terminal" || mode == "lease_failure") && reads != 1 {
				t.Fatal("inactive/failed renewal must not read cancellation authority")
			}
			ticks <- time.Now()
			repo.mu.Lock()
			unchanged := repo.reads == reads && repo.renews == renews
			repo.mu.Unlock()
			if !unchanged || funds.commitCalls != 0 || funds.releaseCalls != 0 {
				t.Fatal("finished loop repeated state or financial work")
			}
		})
	}
	for _, mode := range []string{"initial_requested", "initial_read_failure", "initial_lease_failure"} {
		t.Run(mode, func(t *testing.T) {
			repo := &remoteOwnerRepository{active: true, renewed: make(chan struct{}, 4), read: make(chan struct{}, 4)}
			if mode == "initial_requested" {
				repo.requested = true
			}
			if mode == "initial_read_failure" {
				repo.readErr = ErrForbidden
			}
			if mode == "initial_lease_failure" {
				repo.leaseErr = ErrForbidden
			}
			var cancelled atomic.Int32
			s := &Service{repository: repo}
			failed, stop, err := s.renewDispatchLease(t.Context(), func() { cancelled.Add(1) }, uuid.New(), "owner", make(chan time.Time))
			defer stop()
			if mode == "initial_requested" {
				if err != nil || cancelled.Load() != 1 {
					t.Fatal("initial REQUESTED did not stop local owner", err)
				}
				select {
				case <-failed:
					t.Fatal("initial REQUESTED became lease failure")
				default:
				}
			} else if !errors.Is(err, ErrForbidden) || cancelled.Load() != 0 {
				t.Fatal("initial error contract changed", err)
			}
			if mode == "initial_lease_failure" && repo.reads != 0 {
				t.Fatal("failed initial renewal must not read authority")
			}
		})
	}
}
