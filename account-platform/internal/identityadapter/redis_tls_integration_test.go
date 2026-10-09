package identityadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRedisAdmissionProductionTLS(t *testing.T) {
	filename := os.Getenv("PINVOU_TEST_REDIS_FIXTURE_CONFIG")
	if filename == "" {
		t.Skip("opt-in isolated production Redis fixture")
	}
	var cfg struct {
		IdentityURL string `json:"identity_url"`
		CAFile      string `json:"ca_file"`
	}
	b, err := os.ReadFile(filename)
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		t.Fatal("fixture config unavailable")
	}
	a, err := NewRedisTokenAdmissionWithCA(cfg.IdentityURL, base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))), cfg.CAFile)
	if err != nil {
		t.Fatal("admission construction failed")
	}
	t.Cleanup(func() { _ = a.client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d := tokenRiskDimensions{action: "password", source: "fixture-source", device: "fixture-device", identifier: "fixture-user", session: "fixture-session"}
	if retry, err := a.Admit(ctx, d); err != nil || retry != 0 {
		t.Fatal("initial Lua admission failed")
	}
	if retry, err := a.Complete(ctx, d, true, "stable-user"); err != nil || retry != 0 {
		t.Fatal("stable alias write failed")
	}
	for i := 0; i < 5; i++ {
		if retry, err := a.Complete(ctx, d, false, ""); err != nil || retry <= 0 {
			t.Fatal("failure increment/expiry failed")
		}
	}
	if retry, err := a.Admit(ctx, d); err != nil || retry <= 0 {
		t.Fatal("failure block not enforced")
	}
	if n, err := a.client.Get(ctx, "pinvou:auth-failure:user:"+a.digest("stable-user")).Int(); err != nil || n != 5 {
		t.Fatal("stable-user aggregation failed")
	}
	for _, cmd := range [][]any{{"GET", "portal:session:fixture"}, {"SET", "portal:flow:fixture", "1"}, {"SCAN", "0"}, {"CONFIG", "GET", "*"}, {"ACL", "LIST"}, {"FLUSHDB"}, {"EVAL", "return redis.call('GET', KEYS[1])", "1", "portal:session:fixture"}, {"EVAL", "return redis.call('GET', 'portal:session:fixture')", "0"}, {"EVAL", "return redis.call('SCAN','0')", "0"}} {
		err := a.client.Do(ctx, cmd...).Err()
		if err == nil || !(strings.HasPrefix(err.Error(), "NOPERM ") || strings.HasPrefix(err.Error(), "ERR ACL failure in script: No permissions to access a key script: ") || strings.HasPrefix(err.Error(), "ERR ACL failure in script: User identity has no permissions to run the 'scan' command script: ")) {
			t.Fatal("forbidden Identity command/script not explicitly denied")
		}
	}
	if err := a.client.Ping(ctx).Err(); err != nil {
		t.Fatal("positive check after denials failed")
	}
	t.Log("PRODUCTION_REDIS_IDENTITY_CLIENT_PASS")
}
