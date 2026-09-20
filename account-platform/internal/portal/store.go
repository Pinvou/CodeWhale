package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/redisconnection"
	"github.com/redis/go-redis/v9"
)

var ErrSessionNotFound = errors.New("portal session not found")
var ErrApprovalWireUnavailable = errors.New("approval wire selection unavailable")

type ApprovalWireLocker interface {
	LockApprovalWireVersion(context.Context, Session, int) (int, error)
	// ApprovalWireVersion reads the immutable per-session wire selection
	// without creating one. A session that never entered the approval centre
	// has no stored selection and is interpreted as v1, exactly like a session
	// persisted before the field existed.
	ApprovalWireVersion(context.Context, Session) (int, error)
}

type Store interface {
	SaveFlow(context.Context, string, LoginFlow) error
	TakeFlow(context.Context, string) (LoginFlow, error)
	SaveSession(context.Context, Session) error
	GetSession(context.Context, string) (Session, error)
	DeleteSession(context.Context, string) error
	SaveApprovalClassification(context.Context, ApprovalClassification) error
	GetApprovalClassification(context.Context, string, string) (ApprovalClassification, error)
	TryRefreshLock(context.Context, string) (bool, error)
	ReleaseRefreshLock(context.Context, string) error
	Ping(context.Context) error
	Close() error
}

type RedisStore struct{ client *redis.Client }

func NewRedisStore(rawURL string) (*RedisStore, error) {
	return NewRedisStoreWithCA(rawURL, "")
}

func NewRedisStoreWithCA(rawURL, caFile string) (*RedisStore, error) {
	options, err := redisconnection.Options(rawURL, caFile)
	if err != nil {
		return nil, err
	}
	return &RedisStore{client: redis.NewClient(options)}, nil
}

func (s *RedisStore) SaveFlow(ctx context.Context, state string, flow LoginFlow) error {
	body, err := json.Marshal(flow)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, "portal:flow:"+state, body, time.Until(flow.ExpiresAt)).Err()
}

func (s *RedisStore) TakeFlow(ctx context.Context, state string) (LoginFlow, error) {
	value, err := s.client.GetDel(ctx, "portal:flow:"+state).Bytes()
	if errors.Is(err, redis.Nil) {
		return LoginFlow{}, ErrSessionNotFound
	}
	var flow LoginFlow
	if err == nil {
		err = json.Unmarshal(value, &flow)
	}
	return flow, err
}

func (s *RedisStore) SaveSession(ctx context.Context, session Session) error {
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	ttl := time.Until(session.RefreshExpiresAt)
	if ttl <= 0 {
		return ErrSessionNotFound
	}
	return s.client.Set(ctx, "portal:session:"+session.ID, body, ttl).Err()
}

func (s *RedisStore) GetSession(ctx context.Context, id string) (Session, error) {
	value, err := s.client.Get(ctx, "portal:session:"+id).Bytes()
	if errors.Is(err, redis.Nil) {
		return Session{}, ErrSessionNotFound
	}
	var session Session
	if err == nil {
		err = json.Unmarshal(value, &session)
	}
	return session, err
}

func (s *RedisStore) DeleteSession(ctx context.Context, id string) error {
	return s.client.Del(ctx, "portal:session:"+id, approvalWireKey(id)).Err()
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
	selected, err := s.client.SetNX(ctx, key, preferred, ttl).Result()
	if err != nil {
		return 0, ErrApprovalWireUnavailable
	}
	if selected {
		return preferred, nil
	}
	locked, err := s.client.Get(ctx, key).Result()
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

func (s *RedisStore) SaveApprovalClassification(ctx context.Context, proof ApprovalClassification) error {
	body, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	ttl := time.Until(proof.ExpiresAt)
	if ttl <= 0 {
		return ErrSessionNotFound
	}
	return s.client.Set(ctx, approvalClassificationKey(proof.SessionID, proof.ApprovalRequestID), body, ttl).Err()
}

func (s *RedisStore) GetApprovalClassification(ctx context.Context, sessionID, approvalRequestID string) (ApprovalClassification, error) {
	value, err := s.client.Get(ctx, approvalClassificationKey(sessionID, approvalRequestID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ApprovalClassification{}, ErrSessionNotFound
	}
	var proof ApprovalClassification
	if err == nil {
		err = json.Unmarshal(value, &proof)
	}
	return proof, err
}

func approvalClassificationKey(sessionID, approvalRequestID string) string {
	digest := sha256.Sum256([]byte(sessionID + "\x00" + approvalRequestID))
	return "portal:approval-classification:" + hex.EncodeToString(digest[:])
}

func (s *RedisStore) TryRefreshLock(ctx context.Context, id string) (bool, error) {
	return s.client.SetNX(ctx, "portal:refresh:"+id, "1", 15*time.Second).Result()
}

func (s *RedisStore) ReleaseRefreshLock(ctx context.Context, id string) error {
	return s.client.Del(ctx, "portal:refresh:"+id).Err()
}

func (s *RedisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }
func (s *RedisStore) Close() error                   { return s.client.Close() }
