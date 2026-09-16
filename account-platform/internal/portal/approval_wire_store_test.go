package portal

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestApprovalWireSelectionIsImmutableAcrossConcurrentTabs(t *testing.T) {
	store := newMemoryStore()
	ctx := context.Background()
	session := Session{ID: "new-session", ApprovalV2Eligible: true, RefreshExpiresAt: time.Now().Add(time.Hour)}
	if err := store.SaveSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	const tabs = 40
	results := make(chan int, tabs)
	var group sync.WaitGroup
	for index := 0; index < tabs; index++ {
		group.Add(1)
		go func(preferred int) {
			defer group.Done()
			wire, err := store.LockApprovalWireVersion(ctx, session, preferred)
			if err != nil {
				results <- 0
				return
			}
			results <- wire
		}(1 + index%2)
	}
	group.Wait()
	close(results)
	first := 0
	for result := range results {
		if result == 0 || (first != 0 && result != first) {
			t.Fatalf("tabs selected inconsistent approval wire: first=%d actual=%d", first, result)
		}
		first = result
	}
	old := Session{ID: "old-session", RefreshExpiresAt: time.Now().Add(time.Hour)}
	if wire, err := store.LockApprovalWireVersion(ctx, old, 2); err != nil || wire != 1 {
		t.Fatalf("old session did not default to v1: wire=%d err=%v", wire, err)
	}
	if err := store.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.approvalWires[session.ID]; ok {
		t.Fatal("approval wire survived session deletion")
	}
}
