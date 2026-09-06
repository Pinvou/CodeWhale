package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestRequiredGovernanceSupervisorExitClosesProcessContext(t *testing.T) {
	for _, failure := range []error{nil, errors.New("fixture registration failure")} {
		ctx, cancel := context.WithCancel(context.Background())
		var stopped atomic.Bool
		runRequiredGovernanceSupervisor(ctx, func() {
			if !stopped.Load() {
				t.Error("HTTP cancellation preceded not-ready publication")
			}
			cancel()
		}, &stopped, func(context.Context) error { return failure })
		if ctx.Err() == nil || !stopped.Load() {
			t.Fatal("unexpected supervisor return left the process live")
		}
		cancel()
	}
}

func TestRequiredGovernanceSupervisorNormalShutdownIsNotFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stopped atomic.Bool
	runRequiredGovernanceSupervisor(ctx, cancel, &stopped, func(context.Context) error { return context.Canceled })
	if stopped.Load() {
		t.Fatal("normal cancellation marked the process as failed")
	}
}
