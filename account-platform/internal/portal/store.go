package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrSessionNotFound = errors.New("portal session not found")

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
	options, err := redis.ParseURL(rawURL)
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
	return s.client.Del(ctx, "portal:session:"+id).Err()
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
