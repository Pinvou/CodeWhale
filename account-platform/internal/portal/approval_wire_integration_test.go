package portal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ownedApprovalWireStore(t *testing.T, ids ...string) *RedisStore {
	t.Helper()
	store := realRedisStore(t, ids...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, id := range ids {
			if err := store.client.Del(ctx, approvalWireKey(id)).Err(); err != nil {
				t.Error("owned wire cleanup failed")
			}
		}
	})
	return store
}

func TestRealRedisApprovalWireSelectionIsImmutableAndSessionBound(t *testing.T) {
	ctx := context.Background()
	id := uuid.NewString()
	store := ownedApprovalWireStore(t, id)
	session := validSession(id)
	session.ApprovalV2Eligible = true
	if err := store.SaveSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if version, err := store.ApprovalWireVersion(ctx, session); err != nil || version != 1 {
		t.Fatal("read selected a protocol")
	}
	if count, err := store.client.Exists(ctx, approvalWireKey(id)).Result(); err != nil || count != 0 {
		t.Fatal("read wrote wire selection")
	}
	versions := make(chan int, 16)
	var wg sync.WaitGroup
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func(preferred int) {
			defer wg.Done()
			version, err := store.LockApprovalWireVersion(ctx, session, preferred)
			if err != nil {
				t.Error("selection failed")
				return
			}
			versions <- version
		}(1 + n%2)
	}
	wg.Wait()
	close(versions)
	selected := 0
	for version := range versions {
		if selected == 0 {
			selected = version
		}
		if version != selected {
			t.Fatal("replicas changed selected protocol")
		}
	}
	if selected == 0 {
		t.Fatal("no selection completed")
	}
	if version, err := store.ApprovalWireVersion(ctx, session); err != nil || version != selected {
		t.Fatal("read disagreed with selection")
	}
	ttl, err := store.client.PTTL(ctx, approvalWireKey(id)).Result()
	if err != nil || ttl <= time.Until(session.RefreshExpiresAt) || ttl > time.Until(session.RefreshExpiresAt)+time.Hour+time.Second {
		t.Fatal("wire TTL did not cover the session")
	}
	if err := store.LogoutSession(ctx, id, id, session.RefreshExpiresAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(ctx, id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("logout retained session")
	}
	if _, err := store.LockApprovalWireVersion(ctx, session, 2); !errors.Is(err, ErrApprovalWireUnavailable) {
		t.Fatal("stale authenticated request recreated selection after logout")
	}
	if count, err := store.client.Exists(ctx, approvalWireKey(id)).Result(); err != nil || count != 0 {
		t.Fatal("logout wire key resurrected")
	}
}

func TestRealRedisApprovalWireEligibilityAndCorruptionFailClosed(t *testing.T) {
	ctx := context.Background()
	id := uuid.NewString()
	store := ownedApprovalWireStore(t, id)
	session := validSession(id)
	if err := store.SaveSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if version, err := store.LockApprovalWireVersion(ctx, session, 2); err != nil || version != 1 {
		t.Fatal("ineligible login entered v2")
	}
	if err := store.client.Set(ctx, approvalWireKey(id), "invalid", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApprovalWireVersion(ctx, session); !errors.Is(err, ErrApprovalWireUnavailable) {
		t.Fatal("corrupt selection defaulted to v1")
	}
	if _, err := store.LockApprovalWireVersion(ctx, session, 2); !errors.Is(err, ErrApprovalWireUnavailable) {
		t.Fatal("corrupt selection was overwritten")
	}
}
