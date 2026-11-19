package portal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const portalFixtureOwnerKey = "pinvou:portal-fixture-owner"

var portalFixtureOnce sync.Once
var portalFixtureError error
var portalFixtureRuntimeURL string

func validatePortalFixtureSpec(dsn string, get func(string) string) error {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "redis" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/0" {
		return errors.New("invalid owned Redis endpoint")
	}
	host, port, err := net.SplitHostPort(u.Host)
	number, numberErr := strconv.Atoi(port)
	if err != nil || numberErr != nil || host != "127.0.0.1" || number < 1024 || number > 65535 || strconv.Itoa(number) != port || get("PINVOU_PORTAL_TEST_REDIS_PORT") != port {
		return errors.New("Redis fixture loopback binding mismatch")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(get("PINVOU_PORTAL_TEST_REDIS_RUN_ID")) || !regexp.MustCompile(`^[A-Za-z0-9-]{10,100}$`).MatchString(get("PINVOU_PORTAL_TEST_REDIS_OWNER_ID")) {
		return errors.New("Redis fixture identity missing")
	}
	switch get("PINVOU_PORTAL_TEST_REDIS_PROFILE") {
	case "local-owned":
	case "ci-owned":
		if get("GITHUB_ACTIONS") != "true" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(get("PINVOU_PORTAL_TEST_REDIS_CONTAINER_ID")) {
			return errors.New("Redis CI service identity missing")
		}
	default:
		return errors.New("owned Redis profile required")
	}
	return nil
}

func redisServerFields(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			out[key] = value
		}
	}
	return out
}

func checkedPortalFixture(t *testing.T) string {
	t.Helper()
	if os.Getenv("PINVOU_PORTAL_TEST_REDIS_URL") == "" {
		t.Skip("PINVOU_PORTAL_TEST_REDIS_URL is required for real Redis tests")
	}
	portalFixtureOnce.Do(func() { portalFixtureError = initializePortalFixture() })
	if portalFixtureError != nil {
		t.Fatal("owned Redis fixture rejected:", portalFixtureError)
	}
	return portalFixtureRuntimeURL
}

func initializePortalFixture() error {
	dsn := os.Getenv("PINVOU_PORTAL_TEST_REDIS_URL")
	if err := validatePortalFixtureSpec(dsn, os.Getenv); err != nil {
		return err
	}
	options, err := redis.ParseURL(dsn)
	if err != nil {
		return errors.New("invalid owned Redis endpoint")
	}
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	options.PoolSize = 8
	client := redis.NewClient(options)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = checkPortalFixtureIdentity(ctx, client); err != nil {
		return err
	}
	users, err := client.Do(ctx, "ACL", "USERS").StringSlice()
	if err != nil || len(users) != 1 || users[0] != "default" {
		return errors.New("unexpected Redis actors; no fixture writes allowed")
	}
	count, err := client.DBSize(ctx).Result()
	if err != nil || count != 1 {
		return errors.New("Redis fixture contains outside keys; no fixture writes allowed")
	}
	policy, err := client.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil || policy["maxmemory-policy"] != "noeviction" {
		return errors.New("Redis fixture must preserve accepted objects")
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return err
	}
	password := hex.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(password))
	data, err := os.ReadFile("../../deploy/production/redis-accounts.acl.template")
	if err != nil {
		return err
	}
	var profile []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "user portal ") {
			profile = strings.Fields(strings.ReplaceAll(line, "PORTAL_PASSWORD_SHA256", hex.EncodeToString(digest[:])))[2:]
		}
	}
	if len(profile) == 0 {
		return errors.New("published Portal ACL missing")
	}
	args := []any{"ACL", "SETUSER", "portal"}
	for _, rule := range profile {
		args = append(args, rule)
	}
	if err = client.Do(ctx, args...).Err(); err != nil {
		return errors.New("published Portal ACL could not be installed")
	}
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword("portal", password)
	portalFixtureRuntimeURL = u.String()
	ops, err := NewRedisStore(dsn)
	if err != nil {
		return err
	}
	defer ops.Close()
	return ops.InventoryTransientCapacity(ctx)
}

func checkPortalFixtureIdentity(ctx context.Context, client *redis.Client) error {
	info, err := client.Info(ctx, "server").Result()
	if err != nil {
		return errors.New("owned Redis identity read failed")
	}
	facts := redisServerFields(info)
	if facts["run_id"] != os.Getenv("PINVOU_PORTAL_TEST_REDIS_RUN_ID") || !strings.HasPrefix(facts["redis_version"], "8.2.") || facts["redis_mode"] != "standalone" || facts["tcp_port"] != "6379" {
		return errors.New("owned Redis server identity mismatch")
	}
	owner, err := client.Get(ctx, portalFixtureOwnerKey).Result()
	if err != nil || owner != os.Getenv("PINVOU_PORTAL_TEST_REDIS_OWNER_ID") {
		return errors.New("Redis fixture ownership marker mismatch")
	}
	return nil
}

func ownedPortalRedisAdmin(t *testing.T) *redis.Client {
	t.Helper()
	checkedPortalFixture(t)
	options, err := redis.ParseURL(os.Getenv("PINVOU_PORTAL_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal("invalid owned Redis endpoint")
	}
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	options.PoolSize = 8
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = checkPortalFixtureIdentity(ctx, client); err != nil {
		client.Close()
		t.Fatal(err)
	}
	return client
}

// Scenario isolation is allowed only inside the controller-created whole Redis
// instance, after rechecking its immutable run ID and out-of-band ownership ID.
// These are fake test objects, never a production cleanup or capacity algorithm.
func resetOwnedPortalNamespace(t *testing.T) {
	t.Helper()
	client := ownedPortalRedisAdmin(t)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, "portal:*", 128).Result()
		if err != nil {
			t.Fatal("owned Redis scenario cleanup failed")
		}
		if len(keys) > 0 {
			if err = client.Del(ctx, keys...).Err(); err != nil {
				t.Fatal("owned Redis scenario cleanup failed")
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	ops, err := NewRedisStore(os.Getenv("PINVOU_PORTAL_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	if err = ops.InventoryTransientCapacity(ctx); err != nil {
		t.Fatal(err)
	}

}

func TestPortalOwnedRedisFixtureRejectsUnboundInputs(t *testing.T) {
	env := map[string]string{"PINVOU_PORTAL_TEST_REDIS_PORT": "15432", "PINVOU_PORTAL_TEST_REDIS_PROFILE": "local-owned", "PINVOU_PORTAL_TEST_REDIS_RUN_ID": strings.Repeat("a", 40), "PINVOU_PORTAL_TEST_REDIS_OWNER_ID": "controlled-fixture-owner"}
	get := func(k string) string { return env[k] }
	if err := validatePortalFixtureSpec("redis://127.0.0.1:15432/0", get); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"redis://outside.invalid:15432/0", "redis://127.0.0.1:15432/1", "redis://user:controlled-private-value@127.0.0.1:15432/0", "redis://127.0.0.1:15433/0", "redis://127.0.0.1:15432/0?db=1"} {
		if err := validatePortalFixtureSpec(bad, get); err == nil || strings.Contains(err.Error(), "controlled-private") {
			t.Fatal("unbound fixture input accepted or exposed", err)
		}
	}
	for _, key := range []string{"PINVOU_PORTAL_TEST_REDIS_PROFILE", "PINVOU_PORTAL_TEST_REDIS_RUN_ID", "PINVOU_PORTAL_TEST_REDIS_OWNER_ID", "PINVOU_PORTAL_TEST_REDIS_PORT"} {
		old := env[key]
		env[key] = ""
		if validatePortalFixtureSpec("redis://127.0.0.1:15432/0", get) == nil {
			t.Fatal("missing fixture authority accepted", key)
		}
		env[key] = old
	}
}

// Reuse the running actor's ephemeral password without logging it, while loading
// the exact published profile. Only the proven owned server can be reconfigured.
func installOwnedPortalFixtureACL(t *testing.T, admin *redis.Client, template string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkPortalFixtureIdentity(ctx, admin); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(checkedPortalFixture(t))
	if err != nil {
		t.Fatal("invalid runtime fixture endpoint")
	}
	password, ok := u.User.Password()
	if !ok {
		t.Fatal("fixture runtime password missing")
	}
	digest := sha256.Sum256([]byte(password))
	data, err := os.ReadFile("../../deploy/production/" + template)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "user portal ") {
			continue
		}
		rules := strings.Fields(strings.ReplaceAll(line, "PORTAL_PASSWORD_SHA256", hex.EncodeToString(digest[:])))[2:]
		args := []any{"ACL", "SETUSER", "portal"}
		for _, rule := range rules {
			args = append(args, rule)
		}
		if err = admin.Do(ctx, args...).Err(); err != nil {
			t.Fatal("published fixture ACL rejected")
		}
		return
	}
	t.Fatal("published Portal actor missing")
}
