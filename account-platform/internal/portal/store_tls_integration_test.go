package portal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Only the owned production-Redis fixture supplies this private, temporary file.
func TestRedisStoreProductionTLS(t *testing.T) {
	filename := os.Getenv("PINVOU_TEST_REDIS_FIXTURE_CONFIG")
	if filename == "" {
		t.Skip("opt-in isolated production Redis fixture")
	}
	var cfg struct {
		PortalURL string `json:"portal_url"`
		CAFile    string `json:"ca_file"`
	}
	b, err := os.ReadFile(filename)
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatal("fixture config unavailable")
	}
	s, err := NewRedisStoreWithCA(cfg.PortalURL, cfg.CAFile)
	if err != nil {
		t.Fatal("store construction failed")
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Fatal("authenticated TLS PING failed")
	}
	flow := LoginFlow{Verifier: "temporary-verifier", Nonce: "test-nonce", ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.SaveFlow(ctx, "fixture", flow); err != nil {
		t.Fatal("save flow failed")
	}
	got, err := s.TakeFlow(ctx, "fixture")
	if err != nil || got.Verifier != flow.Verifier {
		t.Fatal("take flow failed")
	}
	if _, err := s.TakeFlow(ctx, "fixture"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("flow replay allowed")
	}
	session := Session{ID: "fixture", AccessToken: "temporary-access", RefreshExpiresAt: time.Now().Add(time.Minute)}
	if err := s.SaveSession(ctx, session); err != nil {
		t.Fatal("save session failed")
	}
	if got, err := s.GetSession(ctx, session.ID); err != nil || got.AccessToken != session.AccessToken {
		t.Fatal("get session failed")
	}
	if wire, err := s.LockApprovalWireVersion(ctx, session, 2); err != nil || wire != 1 {
		t.Fatal("pre-upgrade session did not lock to v1")
	}
	session.ApprovalV2Eligible = true
	if wire, err := s.LockApprovalWireVersion(ctx, session, 2); err != nil || wire != 1 {
		t.Fatal("pre-upgrade session changed protocol after lock")
	}
	newSession := Session{ID: "fixture-new", ApprovalV2Eligible: true, RefreshExpiresAt: time.Now().Add(time.Minute)}
	if err := s.SaveSession(ctx, newSession); err != nil {
		t.Fatal("save new session failed")
	}
	if wire, err := s.LockApprovalWireVersion(ctx, newSession, 2); err != nil || wire != 2 {
		t.Fatal("new session did not lock to v2")
	}
	peer, err := NewRedisStoreWithCA(cfg.PortalURL, cfg.CAFile)
	if err != nil {
		t.Fatal("second portal replica store construction failed")
	}
	defer peer.Close()
	if wire, err := peer.LockApprovalWireVersion(ctx, newSession, 1); err != nil || wire != 2 {
		t.Fatal("new session changed protocol after gate downgrade")
	}
	if err := s.DeleteSession(ctx, newSession.ID); err != nil {
		t.Fatal("delete new session failed")
	}
	proof := ApprovalClassification{SessionID: session.ID, ApprovalRequestID: "approval", CommandType: "fixture", ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.SaveApprovalClassification(ctx, proof); err != nil {
		t.Fatal("save classification failed")
	}
	if got, err := s.GetApprovalClassification(ctx, session.ID, "approval"); err != nil || got.CommandType != proof.CommandType {
		t.Fatal("get classification failed")
	}
	if locked, err := s.TryRefreshLock(ctx, session.ID); err != nil || !locked {
		t.Fatal("lock failed")
	}
	if locked, err := s.TryRefreshLock(ctx, session.ID); err != nil || locked {
		t.Fatal("duplicate lock accepted")
	}
	if err := s.ReleaseRefreshLock(ctx, session.ID); err != nil {
		t.Fatal("unlock failed")
	}
	if locked, err := s.TryRefreshLock(ctx, session.ID); err != nil || !locked {
		t.Fatal("relock failed")
	}
	if err := s.ReleaseRefreshLock(ctx, session.ID); err != nil {
		t.Fatal("final unlock failed")
	}
	if err := s.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal("delete failed")
	}
	if _, err := s.GetSession(ctx, session.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("deleted session retained")
	}
	for _, cmd := range [][]any{{"GET", "pinvou:auth-alias:fixture"}, {"SET", "pinvou:auth-failure:fixture", "1"}, {"SCAN", "0"}, {"KEYS", "*"}, {"CONFIG", "GET", "*"}, {"ACL", "LIST"}, {"FLUSHALL"}, {"EVAL", "return 1", "0"}} {
		if err := s.client.Do(ctx, cmd...).Err(); err == nil || !strings.HasPrefix(err.Error(), "NOPERM") {
			t.Fatal("forbidden Portal command not explicitly denied")
		}
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal("positive check after denials failed")
	}
	t.Log("PRODUCTION_REDIS_PORTAL_CLIENT_PASS")
}
