package token

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGovernanceEpochOwnsExactlyOneGeneration(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newTokenGovernanceEpoch(parent)
	first.close()
	first.close()
	if first.context.Err() == nil || first.cancel != nil || parent.Err() != nil {
		t.Fatal("closing one generation leaked cancellation or cancelled its parent")
	}
	next := newTokenGovernanceEpoch(parent)
	defer next.close()
	if next.context.Err() != nil {
		t.Fatal("new generation inherited the retired generation's cancellation")
	}
	cancel()
	if next.context.Err() == nil {
		t.Fatal("parent shutdown did not cancel the active generation")
	}
}

type supervisorActivation struct {
	sync.Mutex
	value TokenCommandGovernanceActivation
	err   error
}

func (r *supervisorActivation) CurrentTokenCommandGovernanceActivation(context.Context) (TokenCommandGovernanceActivation, error) {
	r.Lock()
	defer r.Unlock()
	return r.value, r.err
}
func (r *supervisorActivation) set(state string, epoch int64, err error) {
	r.Lock()
	defer r.Unlock()
	r.value = TokenCommandGovernanceActivation{State: state, Epoch: epoch}
	r.err = err
}

func TestGovernanceSupervisorEpochReadyAndCancellation(t *testing.T) {
	reader := &supervisorActivation{}
	reader.set("PRE_ACTIVATION", 0, nil)
	defaultRoutes := &PostgresDefaultRouteProvisioner{}
	service := &Service{defaultRoutes: defaultRoutes}
	type loop struct {
		epoch   int64
		ready   func()
		stopped chan struct{}
	}
	started := make(chan loop, 12)
	supervisor := &TokenGovernanceSupervisor{service: service, reader: reader, interval: time.Millisecond}
	supervisor.factory = func(activation TokenCommandGovernanceActivation) ([]governedWorkerRun, error) {
		worker := func(ctx context.Context, ready func()) error {
			stopped := make(chan struct{})
			started <- loop{activation.Epoch, ready, stopped}
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		}
		return []governedWorkerRun{worker, worker}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- supervisor.Run(ctx) }()
	select {
	case <-started:
		t.Fatal("PRE started workers")
	case <-time.After(15 * time.Millisecond):
	}
	if service.tokenAccountCreateReady.Load() || defaultRoutes.accountCreateReady.Load() {
		t.Fatal("PRE create ready")
	}
	next := func() loop {
		t.Helper()
		select {
		case item := <-started:
			return item
		case <-time.After(time.Second):
			t.Fatal("worker did not start")
			return loop{}
		}
	}
	waitReady := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for service.tokenAccountCreateReady.Load() != want || defaultRoutes.accountCreateReady.Load() != want {
			if time.Now().After(deadline) {
				t.Fatalf("API and default-route create readiness did not both become %v", want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitStopped := func(item loop) {
		t.Helper()
		select {
		case <-item.stopped:
		case <-time.After(time.Second):
			t.Fatal("old epoch loop not cancelled")
		}
	}
	reader.set("ACTIVE", 1, nil)
	first, second := next(), next()
	if first.epoch != 1 || second.epoch != 1 {
		t.Fatal("workers did not freeze selected epoch")
	}
	first.ready()
	time.Sleep(5 * time.Millisecond)
	if service.tokenAccountCreateReady.Load() {
		t.Fatal("one loop cannot open create")
	}
	second.ready()
	waitReady(true)
	reader.set("SAFE_ROLLBACK", 2, nil)
	waitReady(false)
	waitStopped(first)
	waitStopped(second)
	reader.set("ACTIVE", 3, nil)
	third, fourth := next(), next()
	if third.epoch != 3 || fourth.epoch != 3 || service.tokenAccountCreateReady.Load() {
		t.Fatal("new epoch reused readiness")
	}
	third.ready()
	fourth.ready()
	waitReady(true)
	reader.set("ACTIVE", 3, errors.New("unavailable"))
	waitReady(false)
	waitStopped(third)
	waitStopped(fourth)
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestGovernanceSupervisorWorkerExitClosesReady(t *testing.T) {
	reader := &supervisorActivation{}
	reader.set("ACTIVE", 1, nil)
	service := &Service{}
	exit := make(chan struct{})
	var mu sync.Mutex
	generations := 0
	supervisor := &TokenGovernanceSupervisor{service: service, reader: reader, interval: time.Millisecond}
	supervisor.factory = func(TokenCommandGovernanceActivation) ([]governedWorkerRun, error) {
		mu.Lock()
		generations++
		generation := generations
		mu.Unlock()
		if generation > 1 {
			return nil, errors.New("dependency unavailable")
		}
		return []governedWorkerRun{
			func(ctx context.Context, ready func()) error {
				ready()
				select {
				case <-exit:
					return errors.New("worker failed")
				case <-ctx.Done():
					return ctx.Err()
				}
			},
			func(ctx context.Context, ready func()) error { ready(); <-ctx.Done(); return ctx.Err() },
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for !service.tokenAccountCreateReady.Load() {
		if time.Now().After(deadline) {
			t.Fatal("not ready")
		}
		time.Sleep(time.Millisecond)
	}
	close(exit)
	deadline = time.Now().Add(time.Second)
	for service.tokenAccountCreateReady.Load() {
		if time.Now().After(deadline) {
			t.Fatal("worker exit left create ready")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not exit")
	}
}
