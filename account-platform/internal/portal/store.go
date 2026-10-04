package portal

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrSessionNotFound = errors.New("portal session not found")
var ErrRefreshConflict = errors.New("portal refresh owner or session changed")

type Store interface {
	SaveFlow(context.Context, string, LoginFlow) error
	TakeFlow(context.Context, string) (LoginFlow, error)
	SaveSession(context.Context, Session) error
	SaveRefreshedSession(context.Context, Session, Session, string) error
	GetSession(context.Context, string) (Session, error)
	DeleteSession(context.Context, string) error
	DeleteRefreshedSession(context.Context, Session, string) error
	TryRefreshLock(context.Context, string, string) (bool, error)
	ReleaseRefreshLock(context.Context, string, string) error
	Ping(context.Context) error
	Close() error
}

type RedisStore struct{ client *redis.Client }

func NewRedisStore(rawURL string) (*RedisStore, error) {
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	options.ContextTimeoutEnabled = true
	return &RedisStore{client: redis.NewClient(options)}, nil
}

func (s *RedisStore) SaveFlow(ctx context.Context, state string, flow LoginFlow) error {
	body, err := json.Marshal(flow)
	if err != nil {
		return err
	}
	ttl := time.Until(flow.ExpiresAt)
	if ttl <= 0 {
		return ErrSessionNotFound
	}
	return s.client.Set(ctx, "portal:flow:"+state, body, ttl).Err()
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

func (s *RedisStore) DeleteRefreshedSession(ctx context.Context, previous Session, owner string) error {
	if previous.ID == "" || owner == "" {
		return ErrSessionNotFound
	}
	body, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	changed, err := s.client.Eval(ctx, `
local current = redis.call('GET', KEYS[1])
if not current then return -1 end
if redis.call('GET', KEYS[2]) ~= ARGV[1] or current ~= ARGV[2] then return 0 end
return redis.call('DEL', KEYS[1])`, []string{"portal:session:" + previous.ID, "portal:refresh:" + previous.ID}, owner, body).Int()
	if err != nil {
		return err
	}
	if changed == -1 {
		return ErrSessionNotFound
	}
	if changed != 1 {
		return ErrRefreshConflict
	}
	return nil
}

func (s *RedisStore) TryRefreshLock(ctx context.Context, id, owner string) (bool, error) {
	if owner == "" {
		return false, errors.New("refresh lock owner is required")
	}
	return s.client.SetNX(ctx, "portal:refresh:"+id, owner, 30*time.Second).Result()
}

func (s *RedisStore) ReleaseRefreshLock(ctx context.Context, id, owner string) error {
	return s.client.Eval(ctx, `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`, []string{"portal:refresh:" + id}, owner).Err()
}

func (s *RedisStore) SaveRefreshedSession(ctx context.Context, previous, updated Session, owner string) error {
	if previous.ID == "" || updated.ID != previous.ID || owner == "" {
		return ErrSessionNotFound
	}
	before, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	after, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	ttl := time.Until(updated.RefreshExpiresAt).Milliseconds()
	if ttl <= 0 {
		return ErrSessionNotFound
	}
	// Fence both lease handoff and logout. A refresh may update an existing
	// unchanged session; it may never recreate a deleted session.
	changed, err := s.client.Eval(ctx, `
local current = redis.call('GET', KEYS[1])
if not current then return -1 end
if redis.call('GET', KEYS[2]) ~= ARGV[1] or current ~= ARGV[2] then return 0 end
redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
return 1`, []string{"portal:session:" + previous.ID, "portal:refresh:" + previous.ID}, owner, before, after, ttl).Int()
	if err != nil {
		return err
	}
	if changed == -1 {
		return ErrSessionNotFound
	}
	if changed != 1 {
		return ErrRefreshConflict
	}
	return nil
}

func (s *RedisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }
func (s *RedisStore) Close() error                   { return s.client.Close() }
