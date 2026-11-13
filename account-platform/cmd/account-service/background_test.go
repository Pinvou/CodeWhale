package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAccountBackgroundDrainIncludesEveryWorker(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	const count = 3
	started, canceled, returned := make(chan int, count), make(chan int, count), make(chan int, count)
	releases := make([]chan struct{}, count)
	once := make([]sync.Once, count)
	var tasks []accountBackgroundTask
	for i := range count {
		releases[i] = make(chan struct{})
		tasks = append(tasks, accountBackgroundTask{"fixture", func(ctx context.Context) {
			started <- i
			<-ctx.Done()
			canceled <- i
			<-releases[i]
			returned <- i
		}})
	}
	done := startAccountBackground(ctx, tasks)
	release := func(i int) { once[i].Do(func() { close(releases[i]) }) }
	t.Cleanup(func() {
		stop()
		for i := range count {
			release(i)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned workers did not retire")
		}
	})
	receive := func(ch <-chan int) {
		t.Helper()
		for range count {
			select {
			case <-ch:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not reach the lifecycle barrier")
			}
		}
	}
	receive(started)
	stop()
	receive(canceled)
	for i := range count - 1 {
		release(i)
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			t.Fatal("released worker did not return")
		}
	}
	select {
	case <-done:
		t.Fatal("drain finished while another worker still owned its resource")
	default:
	}
	release(count - 1)
	waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitAccountBackground(waitCtx, done); err != nil {
		t.Fatal("all workers returned but the next drain remained blocked", err)
	}
}

func TestAccountBackgroundDrainDeadlineDoesNotPretendCompletion(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	done := startAccountBackground(ctx, []accountBackgroundTask{{"fixture", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		<-release
	}}})
	t.Cleanup(func() {
		stop()
		unblock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("held worker did not retire after release")
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	stop()
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := waitAccountBackground(waitCtx, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unfinished group must report bounded drain expiry", err)
	}
	select {
	case <-done:
		t.Fatal("drain expiry falsely acknowledged worker completion")
	default:
	}
	unblock()
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer retryCancel()
	if err := waitAccountBackground(retryCtx, done); err != nil {
		t.Fatal("actual worker completion was lost after drain expiry", err)
	}
}

func TestAccountBackgroundEmptyGroupCompletes(t *testing.T) {
	done := startAccountBackground(context.Background(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := waitAccountBackground(ctx, done); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := waitAccountBackground(ctx, done); err != nil {
		t.Fatal("completed group was reported as timed out", err)
	}
}
