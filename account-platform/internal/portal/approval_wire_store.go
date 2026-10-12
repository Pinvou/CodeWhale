package portal

import (
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"time"
)

var ErrApprovalWireUnavailable = errors.New("approval wire unavailable")

type ApprovalWireLocker interface {
	LockApprovalWireVersion(context.Context, Session, int) (int, error)
	ApprovalWireVersion(context.Context, Session) (int, error)
}

func approvalWireKey(id string) string { return "portal:approval-wire:" + id }

// SETNX selects exactly once across Portal replicas and browser tabs. A lock
// survives slightly longer than the Session so TTL rounding or refresh cannot
// change protocol mid-session; deletion removes both keys together.
func (s *RedisStore) LockApprovalWireVersion(ctx context.Context, session Session, preferred int) (int, error) {
	if s == nil || s.client == nil || session.ID == "" || !session.RefreshExpiresAt.After(time.Now()) ||
		(preferred != 1 && preferred != 2) {
		return 0, ErrApprovalWireUnavailable
	}
	if !session.ApprovalV2Eligible {
		preferred = 1
	}
	key := approvalWireKey(session.ID)
	ttl := time.Until(session.RefreshExpiresAt) + time.Hour
	// The selection and live-session check share one Redis operation. A request
	// authenticated before logout cannot recreate metadata after logout deleted it.
	locked, err := s.client.Eval(ctx, `
if redis.call('EXISTS', KEYS[1]) ~= 1 then return '' end
redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2], 'NX')
return redis.call('GET', KEYS[2])
`, []string{sessionKey(session.ID), key}, preferred, ttl.Milliseconds()).Text()
	if err != nil {
		return 0, ErrApprovalWireUnavailable
	}
	switch locked {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, ErrApprovalWireUnavailable
	}
}

// ApprovalWireVersion is the read-only companion of LockApprovalWireVersion.
// It never selects a version: an absent key means the session has not entered
// the approval centre yet and therefore stays on v1. A key that holds anything
// other than "1"/"2", or a store failure, fails closed so a corrupted or
// unreadable selection can never be read as v1.
func (s *RedisStore) ApprovalWireVersion(ctx context.Context, session Session) (int, error) {
	if s == nil || s.client == nil || session.ID == "" {
		return 0, ErrApprovalWireUnavailable
	}
	locked, err := s.client.Get(ctx, approvalWireKey(session.ID)).Result()
	if errors.Is(err, redis.Nil) {
		return 1, nil
	}
	if err != nil {
		return 0, ErrApprovalWireUnavailable
	}
	switch locked {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, ErrApprovalWireUnavailable
	}
}
