package main

import (
	"context"
	"sync"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

type accountBackgroundTask struct {
	name string
	run  func(context.Context)
}

// Register the complete set before starting it. No worker can outlive a failed
// configuration phase, and the completion signal includes optional workers.
func startAccountBackground(ctx context.Context, tasks []accountBackgroundTask) <-chan struct{} {
	done := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(len(tasks))
	for _, task := range tasks {
		go func() {
			defer workers.Done()
			observability.RunBackground(ctx, task.name, task.run)
		}()
	}
	go func() {
		workers.Wait()
		close(done)
	}()
	return done
}

// One shared deadline bounds the whole group, not one timeout per worker. The
// caller cancels workers first and keeps their repository open through this wait.
func waitAccountBackground(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
