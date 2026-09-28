package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/redisconnection"
	"github.com/redis/go-redis/v9"
)

var ErrSessionNotFound = errors.New("portal session not found")
var ErrLoginFlowInvalidated = errors.New("portal login flow invalidated by logout")
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
	SaveSessionHandoff(context.Context, string, SessionHandoff) error
	TakeSessionHandoff(context.Context, string) (SessionHandoff, error)
	SaveSession(context.Context, Session) error
	CommitLoginSession(context.Context, string, Session) error
	ReplaceSessionFromHandoff(context.Context, string, Session, time.Time) error
	GetSession(context.Context, string) (Session, error)
	ResolveSessionLineageRoot(context.Context, string) (string, error)
	DeleteSession(context.Context, string) error
	LogoutSession(context.Context, string, string, time.Time) error
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

func (s *RedisStore) SaveSessionHandoff(ctx context.Context, hash string, value SessionHandoff) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, "portal:session-handoff:"+hash, body, time.Until(value.ExpiresAt)).Err()
}

func (s *RedisStore) TakeSessionHandoff(ctx context.Context, hash string) (SessionHandoff, error) {
	value, err := s.client.GetDel(ctx, "portal:session-handoff:"+hash).Bytes()
	if errors.Is(err, redis.Nil) {
		return SessionHandoff{}, ErrSessionNotFound
	}
	var handoff SessionHandoff
	if err == nil {
		err = json.Unmarshal(value, &handoff)
	}
	return handoff, err
}

func (s *RedisStore) SaveSession(ctx context.Context, session Session) error {
	root := sessionLineageRoot(session)
	session.LineageRoot = root
	body, ttl, err := marshalSession(session)
	if err != nil {
		return err
	}
	result, err := s.client.Eval(ctx, `
local root = redis.call('GET', KEYS[2]) or ARGV[3]
if root ~= ARGV[3] or redis.call('EXISTS', 'portal:logout:' .. root) == 1 then
  return 0
end
local lineage_key = 'portal:lineage-members:' .. root
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('SET', KEYS[2], root, 'PX', ARGV[2])
redis.call('SADD', lineage_key, ARGV[4])
redis.call('PEXPIRE', lineage_key, ARGV[2])
local members = redis.call('SMEMBERS', lineage_key)
for _, member in ipairs(members) do
  redis.call('SET', 'portal:session-root:' .. member, root, 'PX', ARGV[2])
end
return 1
`, []string{sessionKey(session.ID), sessionRootKey(session.ID)}, body, ttl.Milliseconds(), root, session.ID).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return ErrLoginFlowInvalidated
	}
	return nil
}

func sessionKey(id string) string          { return "portal:session:" + id }
func sessionRootKey(id string) string      { return "portal:session-root:" + id }
func logoutMarkerKey(root string) string   { return "portal:logout:" + root }
func lineageMembersKey(root string) string { return "portal:lineage-members:" + root }

func sessionLineageRoot(session Session) string {
	if root := strings.TrimSpace(session.LineageRoot); root != "" {
		return root
	}
	return session.ID
}

func marshalSession(session Session) ([]byte, time.Duration, error) {
	body, err := json.Marshal(session)
	if err != nil {
		return nil, 0, err
	}
	ttl := time.Until(session.RefreshExpiresAt)
	if ttl <= 0 {
		return nil, 0, ErrSessionNotFound
	}
	return body, ttl, nil
}

// CommitLoginSession atomically rejects an OIDC callback whose predecessor was
// explicitly logged out. This closes the callback-after-logout race without
// reviving the old browser identity.
func (s *RedisStore) CommitLoginSession(ctx context.Context, previousID string, session Session) error {
	root := sessionLineageRoot(session)
	session.LineageRoot = root
	body, ttl, err := marshalSession(session)
	if err != nil {
		return err
	}
	result, err := s.client.Eval(ctx, `
local root = ARGV[3]
if ARGV[5] ~= '' then
  local mapped = redis.call('GET', KEYS[1])
  if mapped then
    if root ~= '' and root ~= mapped then
      return 0
    end
    root = mapped
  end
end
if root == '' then root = ARGV[6] end
if redis.call('EXISTS', 'portal:logout:' .. root) == 1 then
  return 0
end
redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2])
redis.call('SET', KEYS[5], root, 'PX', ARGV[2])
local lineage_key = 'portal:lineage-members:' .. root
redis.call('SADD', lineage_key, ARGV[4])
redis.call('PEXPIRE', lineage_key, ARGV[2])
local members = redis.call('SMEMBERS', lineage_key)
for _, member in ipairs(members) do
  redis.call('SET', 'portal:session-root:' .. member, root, 'PX', ARGV[2])
end
if ARGV[5] ~= '' then
  redis.call('DEL', KEYS[3], KEYS[4])
end
return 1
`, []string{sessionRootKey(previousID), sessionKey(session.ID), sessionKey(previousID), approvalWireKey(previousID), sessionRootKey(session.ID)}, body, ttl.Milliseconds(), root, session.ID, previousID, session.ID).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return ErrLoginFlowInvalidated
	}
	return nil
}

// ReplaceSessionFromHandoff intentionally supersedes the browser identity with
// the desktop-authenticated identity, even after an explicit browser logout.
func (s *RedisStore) ReplaceSessionFromHandoff(ctx context.Context, previousID string, session Session, markerExpiresAt time.Time) error {
	root := sessionLineageRoot(session)
	session.LineageRoot = root
	body, ttl, err := marshalSession(session)
	if err != nil {
		return err
	}
	markerTTL := time.Until(markerExpiresAt)
	if markerTTL <= 0 {
		return ErrSessionNotFound
	}
	return s.client.Eval(ctx, `
if ARGV[1] ~= '' then
  local old_root = redis.call('GET', KEYS[1]) or ARGV[1]
  redis.call('SET', 'portal:logout:' .. old_root, '1', 'PX', ARGV[5])
  local members = redis.call('SMEMBERS', 'portal:lineage-members:' .. old_root)
  for _, member in ipairs(members) do
    redis.call('DEL', 'portal:session:' .. member, 'portal:approval-wire:' .. member)
  end
  redis.call('DEL', 'portal:lineage-members:' .. old_root)
end
redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3])
redis.call('SET', KEYS[3], ARGV[4], 'PX', ARGV[3])
redis.call('SADD', KEYS[4], ARGV[6])
redis.call('PEXPIRE', KEYS[4], ARGV[3])
return 1
`, []string{sessionRootKey(previousID), sessionKey(session.ID), sessionRootKey(session.ID), lineageMembersKey(root)}, previousID, body, ttl.Milliseconds(), root, markerTTL.Milliseconds(), session.ID).Err()
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

func (s *RedisStore) ResolveSessionLineageRoot(ctx context.Context, id string) (string, error) {
	root, err := s.client.Get(ctx, sessionRootKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", err
	}
	if root = strings.TrimSpace(root); root == "" {
		return "", ErrSessionNotFound
	}
	return root, nil
}

func (s *RedisStore) DeleteSession(ctx context.Context, id string) error {
	return s.client.Del(ctx, "portal:session:"+id, approvalWireKey(id)).Err()
}

// LogoutSession is idempotent: it leaves a tombstone even when the session was
// already deleted, so a pre-logout OIDC callback cannot recreate it.
func (s *RedisStore) LogoutSession(ctx context.Context, id, rootHint string, markerExpiresAt time.Time) error {
	ttl := time.Until(markerExpiresAt)
	if ttl <= 0 {
		return ErrSessionNotFound
	}
	return s.client.Eval(ctx, `
local root = ARGV[2]
local mapped = redis.call('GET', KEYS[1])
if mapped then root = mapped end
if root == '' then root = ARGV[3] end
redis.call('SET', 'portal:logout:' .. root, '1', 'PX', ARGV[1])
local members = redis.call('SMEMBERS', 'portal:lineage-members:' .. root)
for _, member in ipairs(members) do
  redis.call('DEL', 'portal:session:' .. member, 'portal:approval-wire:' .. member)
end
redis.call('DEL', 'portal:lineage-members:' .. root, KEYS[2], KEYS[3])
return 1
`, []string{sessionRootKey(id), sessionKey(id), approvalWireKey(id)}, ttl.Milliseconds(), rootHint, id).Err()
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
