package cifixture

// Only test setup may use these process-owned snapshots. Migration acceptance
// must still execute the migration under test in each private clone.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Measure emits no SQL, connection string, database name or credentials.
func Measure(operation string) func() {
	start := time.Now()
	return func() {
		if os.Getenv("PINVOU_CI_FIXTURE_TIMING") == "1" {
			data, _ := json.Marshal(struct {
				Operation string  `json:"operation"`
				Seconds   float64 `json:"seconds"`
			}{operation, time.Since(start).Seconds()})
			fmt.Fprintf(os.Stderr, "[ci-fixture-timing] %s\n", data)
		}
	}
}

// TemplateKey binds the complete immutable setup input, including SQL and
// checksums, to a versioned fixture policy. It is never a cached test result.
func TemplateKey(policy string, input any) (string, error) {
	data, err := json.Marshal([]any{policy, input})
	if err != nil || policy == "" {
		return "", errors.New("invalid fixture template input")
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

type templateEntry struct {
	lease  *Database
	config *pgx.ConnConfig
	ready  bool
}

// TemplatePool never discovers/adopts an existing template. Close must run
// before the fixture's deployment actors are retired. No state crosses a Go
// test process, CI phase or Job. A pool used by absent-actor tests must be
// scoped inside that same actor-owning root, never kept until TestMain.
type TemplatePool struct {
	mu      sync.Mutex
	enabled bool
	actors  []string
	entries map[string]*templateEntry
	closed  bool
	hits    int
	misses  int
}

func NewTemplatePool(enabled bool, actors ...string) *TemplatePool {
	return &TemplatePool{enabled: enabled, actors: append([]string(nil), actors...), entries: make(map[string]*templateEntry)}
}

func (p *TemplatePool) actorPolicy(ctx context.Context, c *pgx.Conn, owner string) (string, error) {
	var identity string
	var count int
	var owns bool
	err := c.QueryRow(ctx, `SELECT md5(jsonb_build_array(
	 (SELECT jsonb_agg(to_jsonb(r) - 'rolpassword' ORDER BY oid) FROM pg_roles r WHERE rolname=ANY($1)),
	 (SELECT jsonb_agg(to_jsonb(m) ORDER BY roleid,member,grantor) FROM pg_auth_members m
	 WHERE roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))::text),
	 (SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)), $2=ANY($1::text[])`, p.actors, owner).Scan(&identity, &count, &owns)
	if err != nil || !owns || len(p.actors) == 0 || count != len(p.actors) {
		return "", errors.New("fixture template actor policy is incomplete")
	}
	return identity, nil
}

func templateActors(ctx context.Context, c *pgx.Conn) (string, error) {
	// OIDs matter: a dropped/recreated same-name actor is a different identity.
	// Membership flags and role attributes also separate preparation policies.
	var identity string
	err := c.QueryRow(ctx, `SELECT md5(jsonb_build_array(
	 (SELECT jsonb_agg(to_jsonb(r) - 'rolpassword' ORDER BY oid) FROM pg_roles r WHERE rolname !~ '^pg_'),
	 (SELECT jsonb_agg(to_jsonb(m) ORDER BY roleid,member,grantor) FROM pg_auth_members m))::text)`).Scan(&identity)
	return identity, err
}

// Create either performs the original empty CREATE, or prepares one frozen
// source with the caller's real migration actor and creates a private clone.
// Errors fail closed; a failed preparation is not replaced with an empty DB.
func (p *TemplatePool) Create(ctx context.Context, c *pgx.Conn, name, owner, input string, prepare func(context.Context, string) error) (*Database, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, errors.New("fixture template pool already retired")
	}
	statement := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize() + " OWNER " + pgx.Identifier{owner}.Sanitize()
	if !p.enabled || os.Getenv("PINVOU_CI_FIXTURE_TEMPLATES") != "1" {
		lease, err := CreateDatabase(ctx, c, name, statement)
		return lease, false, err
	}
	if owner == "" || len(input) != 64 || prepare == nil {
		return nil, false, errors.New("explicit fixture snapshot policy required")
	}
	if _, err := hex.DecodeString(input); err != nil {
		return nil, false, errors.New("invalid fixture snapshot fingerprint")
	}
	cluster, err := databaseCluster(ctx, c, name)
	if err != nil {
		return nil, false, err
	}
	actors, err := p.actorPolicy(ctx, c, owner)
	if err != nil {
		return nil, false, err
	}
	key, err := TemplateKey("owned-template-v1", []string{cluster.system, cluster.directory, c.Config().User, owner, actors, input})
	if err != nil {
		return nil, false, err
	}
	entry := p.entries[key]
	wasHit := entry != nil
	if entry == nil {
		if len(p.entries) >= 32 {
			return nil, false, errors.New("fixture template pool limit reached")
		}
		p.misses++
		source := "ci_template_" + uuid.NewString()
		lease, createErr := CreateDatabase(ctx, c, source, "CREATE DATABASE "+pgx.Identifier{source}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize())
		// Retain even ambiguous creation evidence for Close; never adopt it.
		entry = &templateEntry{lease: lease, config: c.Config().Copy()}
		p.entries[key] = entry
		if createErr != nil {
			return nil, false, createErr
		}
		before, err := templateActors(ctx, c)
		if err != nil {
			return nil, false, err
		}
		err = func() error {
			defer Measure("template-prepare")()
			return prepare(ctx, source)
		}()
		if err != nil {
			return nil, false, err
		}
		after, err := templateActors(ctx, c)
		if err != nil || after != before {
			return nil, false, errors.New("fixture preparation changed global actors")
		}
		if err = waitDatabase(ctx, c, source, lease, owner); err != nil {
			return nil, false, err
		}
		if _, err = c.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{source}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
			return nil, false, err
		}
		entry.ready = true
	}
	if !entry.ready || entry.lease == nil {
		return nil, false, errors.New("unconfirmed fixture template")
	}
	if err = waitDatabase(ctx, c, entry.lease.key.name, entry.lease, owner); err != nil {
		return nil, false, err
	}
	var connectionsAllowed bool
	if err = c.QueryRow(ctx, "SELECT datallowconn FROM pg_database WHERE datname=$1", entry.lease.key.name).Scan(&connectionsAllowed); err != nil || connectionsAllowed {
		return nil, false, errors.New("fixture template is not frozen")
	}
	defer Measure("template-clone")()
	lease, err := CreateDatabase(ctx, c, name, statement+" TEMPLATE "+pgx.Identifier{entry.lease.key.name}.Sanitize()+" STRATEGY WAL_LOG")
	if err == nil && wasHit {
		p.hits++
	}
	return lease, true, err
}

// Close uses each source's opaque creation lease and ordinary zero-client DROP.
// It never forces clients out, adopts a replacement, or drops global roles.
func (p *TemplatePool) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	var failures []error
	for key, entry := range p.entries {
		if entry.lease == nil {
			failures = append(failures, ErrDatabaseUnregistered)
			continue
		}
		c, err := ConnectConfig(ctx, entry.config.Copy())
		if err == nil {
			_, err = DropDatabase(ctx, c, entry.lease)
			c.Close(ctx)
		}
		if err != nil {
			failures = append(failures, err)
		} else {
			delete(p.entries, key)
		}
	}
	if os.Getenv("PINVOU_CI_FIXTURE_TIMING") == "1" {
		data, _ := json.Marshal(map[string]int{"prepared": p.misses, "reused": p.hits, "remaining": len(p.entries)})
		fmt.Fprintf(os.Stderr, "[ci-fixture-template] %s\n", data)
	}
	return errors.Join(failures...)
}
