package portal

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu       sync.Mutex
	flows    map[string]LoginFlow
	sessions map[string]Session
	locks    map[string]string
	roots    map[string]string
	markers  map[string]time.Time
	handoffs map[string]SessionHandoff
}

func newMemoryStore() *memoryStore {
	return &memoryStore{flows: map[string]LoginFlow{}, sessions: map[string]Session{}, locks: map[string]string{}, roots: map[string]string{}, markers: map[string]time.Time{}, handoffs: map[string]SessionHandoff{}}
}
func (s *memoryStore) SaveFlow(_ context.Context, key string, value LoginFlow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flows[key] = value
	return nil
}
func (s *memoryStore) TakeFlow(_ context.Context, key string) (LoginFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.flows[key]
	delete(s.flows, key)
	if !ok {
		return LoginFlow{}, ErrSessionNotFound
	}
	return value, nil
}
func (s *memoryStore) SaveSession(_ context.Context, value Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	root := sessionLineageRoot(value)
	if _, exists := s.markers[root]; exists {
		return ErrLoginFlowInvalidated
	}
	value.LineageRoot = root
	s.sessions[value.ID] = value
	s.roots[value.ID] = root
	return nil
}
func (s *memoryStore) GetSession(_ context.Context, key string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[key]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return value, nil
}
func (s *memoryStore) DeleteSession(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
	return nil
}
func (s *memoryStore) TryRefreshLock(_ context.Context, id, owner string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.locks[id]; exists {
		return false, nil
	}
	s.locks[id] = owner
	return true, nil
}
func (s *memoryStore) ReleaseRefreshLock(_ context.Context, id, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[id] == owner {
		delete(s.locks, id)
	}
	return nil
}
func (s *memoryStore) SaveRefreshedSession(_ context.Context, previous, updated Session, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[previous.ID]
	if !exists {
		return ErrSessionNotFound
	}
	if current != previous || s.locks[previous.ID] != owner {
		return ErrRefreshConflict
	}
	s.sessions[previous.ID] = updated
	return nil
}
func (s *memoryStore) DeleteRefreshedSession(_ context.Context, previous Session, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.sessions[previous.ID]
	if !exists {
		return ErrSessionNotFound
	}
	if current != previous || s.locks[previous.ID] != owner {
		return ErrRefreshConflict
	}
	delete(s.sessions, previous.ID)
	return nil
}
func (*memoryStore) Ping(context.Context) error { return nil }
func (*memoryStore) Close() error               { return nil }

func (s *memoryStore) CommitLoginSession(ctx context.Context, previous string, value Session) error {
	if err := s.SaveSession(ctx, value); err != nil {
		return err
	}
	if previous != "" {
		return s.DeleteSession(ctx, previous)
	}
	return nil
}
func (s *memoryStore) SaveSessionHandoff(_ context.Context, key string, value SessionHandoff) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handoffs[key] = value
	return nil
}
func (s *memoryStore) TakeSessionHandoff(_ context.Context, key string) (SessionHandoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.handoffs[key]
	delete(s.handoffs, key)
	if !ok {
		return v, ErrSessionNotFound
	}
	return v, nil
}
func (s *memoryStore) ResolveSessionLineageRoot(_ context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.roots[id]
	if !ok {
		return "", ErrSessionNotFound
	}
	return v, nil
}
func (s *memoryStore) LogoutSession(_ context.Context, id, root string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mapped := s.roots[id]; mapped != "" {
		root = mapped
	}
	if root == "" {
		root = id
	}
	s.markers[root] = until
	for member, mapped := range s.roots {
		if mapped == root {
			delete(s.sessions, member)
		}
	}
	return nil
}
func (s *memoryStore) ReplaceSessionFromHandoff(ctx context.Context, previous string, value Session, until time.Time) error {
	if previous != "" {
		if err := s.LogoutSession(ctx, previous, "", until); err != nil {
			return err
		}
	}
	return s.SaveSession(ctx, value)
}

func testService(t *testing.T, store Store) *Service {
	t.Helper()
	parse := func(value string) *url.URL {
		parsed, err := url.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	service, err := NewService(Config{Environment: "test", IssuerURL: parse("https://identity.example"), JWKSURL: parse("https://identity.example/keys"), PublicURL: parse("https://portal.example"), AuthorizationURL: parse("https://identity.example/auth"), TokenURL: parse("https://identity.example/token"), ClientID: "portal", AccountURL: parse("https://account.example"), TokenGatewayURL: parse("https://token.example"), BillingURL: parse("https://billing.example"), SecureCookies: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
