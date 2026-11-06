package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	tokendomain "github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ownerCombinedTrace struct {
	queries atomic.Int64
	owners  atomic.Int64
}

func (v *ownerCombinedTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	v.queries.Add(1)
	if strings.Contains(q.SQL, "FROM organizations") || strings.Contains(q.SQL, "FROM organization_units") || strings.Contains(q.SQL, "FROM users WHERE id=") {
		v.owners.Add(1)
	}
	return ctx
}
func (*ownerCombinedTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func ownerCombinedFacts(t *testing.T, r *PostgresRepository) string {
	t.Helper()
	var raw string
	err := r.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),
 'identities',(SELECT jsonb_agg(to_jsonb(x) ORDER BY issuer,subject) FROM identities x),
 'organizations',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM organizations x),
 'units',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM organization_units x),
 'roles',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),
 'memberships',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),
 'bindings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),
 'contexts',(SELECT jsonb_agg(to_jsonb(x) ORDER BY user_id) FROM user_active_contexts x),
 'sessions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY issuer,session_id) FROM user_sessions x),
 'auth',(SELECT jsonb_agg(to_jsonb(x) ORDER BY user_id,scope,scope_id) FROM authorization_versions x),
 'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_logs x),
 'outbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM outbox_events x)
 )::text`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ownerCombinedRequest(actor entitlementFixtureActor, kind string, owner uuid.UUID) tokenauthority.Request {
	issuer, subject := "https://entitlement-owned.invalid", actor.Subject
	sha := sha256.Sum256([]byte(subject))
	hash := hex.EncodeToString(sha[:])
	digest := strings.Repeat("a", 64)
	id := owner.String()
	q := tokenauthority.Request{AuthoritySchemaVersion: "1", Mode: tokenauthority.ModeT1Snapshot, IdentityIssuer: &issuer, IdentitySubject: &subject, IdentitySubjectSHA256: &hash, OwnerType: &kind, OwnerID: &id, RequiredPermissions: append([]string(nil), tokenauthority.RequiredPermissions...), CommandReference: uuid.NewString(), CommandRequestDigest: &digest}
	q.AuthorityRequestSHA256 = q.Digest()
	return q
}

func ownerCombinedT3(q tokenauthority.Request, prior tokenauthority.Response, mode string) tokenauthority.Request {
	q.Mode = mode
	q.IdentityIssuer = nil
	q.IdentitySubject = nil
	q.IdentitySubjectSHA256 = nil
	q.ActorUserID = &prior.ActorUserID
	q.ExpectedPlatformAuthorizationVersion = &prior.ObservedPlatformAuthorizationVersion
	q.ExpectedActorVersion = &prior.ObservedActorVersion
	q.ExpectedActorIdentityDigest = &prior.ActorIdentityDigest
	q.ExpectedOwnerVersion = prior.ObservedOwnerVersion
	q.ExpectedOwnerIdentityDigest = prior.OwnerIdentityDigest
	q.AuthorityRequestSHA256 = q.Digest()
	return q
}

func ownerCombinedJWT(t *testing.T) func(subject, variant string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "owner-controlled", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(jwks.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "owner-controlled")
	rules := []map[string]any{}
	for _, p := range []struct{ sub, service, scope string }{{"pinvou:service:token-gateway", "token-gateway", "internal.account.token-owner-authority.check"}, {"system:serviceaccount:isolated:token-gateway", "token-gateway", "internal.account.token-owner-authority.check"}, {"wrong-service", "billing-service", "internal.account.token-owner-authority.check"}, {"retired", "token-gateway", "internal.token-owner-authority.read"}, {"wrong-scope", "token-gateway", "internal.authorization.check"}} {
		permissions := []string{p.scope}
		if p.sub == "pinvou:service:token-gateway" || strings.HasPrefix(p.sub, "system:") {
			permissions = append(permissions, "internal.token-owner-candidates.read", "internal.authorization.check")
		}
		rules = append(rules, map[string]any{"subject": p.sub, "service": p.service, "permissions": permissions})
	}
	raw, _ := json.Marshal(rules)
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(raw))
	return func(subject, variant string) string {
		if subject == "" {
			return ""
		}
		if variant == "garbage" {
			return "invalid"
		}
		claims := map[string]any{"iss": issuer, "aud": "owner-controlled", "sub": subject, "iat": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()}
		if variant == "issuer" {
			claims["iss"] = "https://wrong.invalid"
		}
		if variant == "audience" {
			claims["aud"] = "wrong"
		}
		if variant == "expired" {
			claims["iat"] = time.Now().Add(-10 * time.Minute).Unix()
			claims["exp"] = time.Now().Add(-time.Minute).Unix()
		}
		signKey := key
		if variant == "signature" {
			signKey, err = rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
		}
		signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signKey}, (&jose.SignerOptions{}).WithHeader("kid", "owner-controlled"))
		if e != nil {
			t.Fatal(e)
		}
		body, _ := json.Marshal(claims)
		signed, e := signer.Sign(body)
		if e != nil {
			t.Fatal(e)
		}
		result, e := signed.CompactSerialize()
		if e != nil {
			t.Fatal(e)
		}
		return result
	}
}

func TestOwnerCombinedActualHTTPPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL") == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	f := retainedWalletFenceActors(t)
	deadline, bounded := t.Deadline()
	if !bounded {
		deadline = time.Now().Add(20 * time.Minute)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-15*time.Second))
	t.Cleanup(cancel)
	// The fixture's ordinary 120-second context is for a local DB test. This
	// test also compiles a child consumer, so retain the enclosing test deadline
	// and cleanup margin without weakening the CI phase's existing 20-minute cap.
	f.ctx = ctx
	r := f.repository(t)
	var current, session string
	var super bool
	if err := r.pool.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&current, &session, &super); err != nil || current != "account_service" || session != current || super {
		t.Fatal("runtime identity", err)
	}
	actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "owner-combined-creator")
	if _, err := r.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://controlled.invalid','operator',$1,clock_timestamp(),clock_timestamp())`, actor.ID); err != nil {
		t.Fatal(err)
	}
	seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "owner-combined-guard")
	operator := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_MODEL_ADMIN", "owner-combined-operator")
	if _, err := r.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://controlled.invalid','recovery-operator',$1,clock_timestamp(),clock_timestamp())`, operator.ID); err != nil {
		t.Fatal(err)
	}
	user := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "owner-combined-user")
	org, err := r.AdminCreateOrganization(ctx, entitlementApprovalPrincipal(actor), actor.ID, "Combined owner organization", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	unit, err := r.AdminCreateUnit(ctx, entitlementApprovalPrincipal(actor), org.ID, nil, "Combined owner unit", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	trace := &ownerCombinedTrace{}
	cfg := r.pool.Config()
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	observed := *r
	observed.pool = pool
	sign := ownerCombinedJWT(t)
	s := NewService(&observed, nil).WithTokenOwnerCandidateCursorKey(bytes.Repeat([]byte{7}, 32))
	server := httptest.NewServer(s.Handler(func(_ context.Context, bearer string) (identity.Principal, error) {
		if bearer != "owned-parent-status-administrator" {
			return identity.Principal{}, errors.New("unknown controlled human identity")
		}
		return entitlementApprovalPrincipal(actor), nil
	}))
	t.Cleanup(server.Close)
	credential := sign("pinvou:service:token-gateway", "")
	client, err := tokendomain.NewHTTPOwnerAuthorityClient(server.URL, credential)
	if err != nil {
		t.Fatal(err)
	}
	call := func(t *testing.T, q tokenauthority.Request) tokenauthority.Response {
		t.Helper()
		before := ownerCombinedFacts(t, r)
		res, e := client.CheckTokenOwnerAuthority(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		if ownerCombinedFacts(t, r) != before {
			t.Fatal("authority wrote domain facts")
		}
		return res
	}
	requests := map[string]tokenauthority.Request{}
	for _, owner := range []struct {
		kind string
		id   uuid.UUID
	}{{"USER", user.ID}, {"ORGANIZATION", org.ID}, {"ORG_UNIT", unit.Unit.ID}} {
		requests[owner.kind] = ownerCombinedRequest(actor, owner.kind, owner.id)
		t.Run(owner.kind+"/four-mode-contract", func(t *testing.T) {
			q := requests[owner.kind]
			res := call(t, q)
			if res.Decision != "ALLOW" || res.ActorUserID != actor.ID.String() || res.ObservedOwnerVersion == nil {
				t.Fatalf("T1 %s %+v", owner.kind, res)
			}
			if owner.kind == "USER" && res.OwnerOrganizationID != nil || owner.kind != "USER" && (res.OwnerOrganizationID == nil || *res.OwnerOrganizationID != org.ID.String()) {
				t.Fatal("owner organization binding")
			}
			if res.ActorIdentityDigest != tokenauthority.ActorIdentityDigest(actor.ID, "ACTIVE", 1) {
				t.Fatal("actor version/digest")
			}
			for _, mode := range []string{tokenauthority.ModeT3CreatorRecheck, tokenauthority.ModeT3OperatorRecheck} {
				check := ownerCombinedT3(q, res, mode)
				got := call(t, check)
				if got.Decision != "ALLOW" || got.Mode != mode || *got.ExpectedActorVersion != res.ObservedActorVersion || *got.ExpectedOwnerIdentityDigest != *res.OwnerIdentityDigest {
					t.Fatal("T3 binding", mode)
				}
			}
			requests[owner.kind] = q
		})
	}
	t.Run("operator-no-owner-query", func(t *testing.T) {
		q := ownerCombinedRequest(operator, "USER", user.ID)
		q.Mode = tokenauthority.ModeOperatorAuthorize
		q.OwnerType = nil
		q.OwnerID = nil
		q.CommandRequestDigest = nil
		q.AuthorityRequestSHA256 = q.Digest()
		owners := trace.owners.Load()
		res := call(t, q)
		if res.ActorUserID != operator.ID.String() || res.ObservedOwnerVersion != nil || res.OwnerStatus != nil || res.OwnerIdentityDigest != nil || res.OwnerOrganizationID != nil || trace.owners.Load() != owners {
			t.Fatal("operator read or returned owner facts")
		}
		for _, reference := range []string{uuid.NewString(), uuid.NewString()} {
			q.CommandReference = reference
			res = call(t, q)
			if res.Decision != "ALLOW" {
				t.Fatal("reference existence probe")
			}
		}
	})
	t.Run("actual-candidates-unit-parent", func(t *testing.T) {
		directory, e := tokendomain.NewHTTPOwnerCandidateDirectory(server.URL, credential)
		if e != nil {
			t.Fatal(e)
		}
		p := identity.Principal{Issuer: "https://entitlement-owned.invalid", Subject: actor.Subject}
		for _, kind := range []string{"USER", "ORGANIZATION", "UNIT"} {
			var parent *uuid.UUID
			if kind == "UNIT" {
				parent = &org.ID
			}
			found := false
			e = directory.Scan(ctx, p, kind, parent, func(items []tokendomain.AdminOwnerCandidate) error {
				for _, item := range items {
					if item.Type != kind {
						t.Fatal("wrong owner type")
					}
					if item.ID == user.ID || item.ID == org.ID || item.ID == unit.Unit.ID {
						found = true
					}
				}
				return nil
			})
			if e != nil || !found {
				t.Fatal("actual candidate scan", kind, e, found)
			}
		}
		if e = directory.Scan(ctx, p, "USER", &org.ID, func([]tokendomain.AdminOwnerCandidate) error { t.Fatal("membership discovery invoked"); return nil }); e == nil {
			t.Fatal("USER+org membership discovery accepted")
		}
		missing := uuid.New()
		count := 0
		e = directory.Scan(ctx, p, "UNIT", &missing, func(items []tokendomain.AdminOwnerCandidate) error { count += len(items); return nil })
		if e != nil || count != 0 {
			t.Fatal("wrong parent leaked units", e, count)
		}
		if _, e := r.AdminCreateUnit(ctx, entitlementApprovalPrincipal(actor), org.ID, nil, "Combined second unit", uuid.NewString()); e != nil {
			t.Fatal(e)
		}
		cursor, seen := "", map[uuid.UUID]bool{}
		for {
			query := url.Values{"actor_issuer": {p.Issuer}, "actor_subject": {p.Subject}, "owner_type": {"UNIT"}, "organization_id": {org.ID.String()}, "limit": {"1"}}
			if cursor != "" {
				query.Set("cursor", cursor)
			}
			request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/internal/v1/token-owner-candidates?"+query.Encode(), nil)
			request.Header.Set("Authorization", "Bearer "+credential)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			var page TokenOwnerCandidatePage
			err = json.NewDecoder(response.Body).Decode(&page)
			response.Body.Close()
			if response.StatusCode != 200 || err != nil || len(page.Items) != 1 || seen[page.Items[0].ID] {
				t.Fatal("unit pagination", response.StatusCode, err)
			}
			item := page.Items[0]
			var fields map[string]any
			raw, _ := json.Marshal(item)
			_ = json.Unmarshal(raw, &fields)
			if len(fields) != 4 || item.Type != "UNIT" || page.OwnerVersions[item.ID.String()] < 1 {
				t.Fatal("candidate projection expanded")
			}
			seen[item.ID] = true
			if !page.HasMore {
				break
			}
			if page.NextCursor == nil || *page.NextCursor == cursor || len(seen) > 2 {
				t.Fatal("unit continuation")
			}
			cursor = *page.NextCursor
		}
		if len(seen) != 2 || !seen[unit.Unit.ID] {
			t.Fatal("unit scan lost an existing owner")
		}
	})
	t.Run("JWT-and-workload-before-SQL", func(t *testing.T) {
		for _, sample := range []struct {
			sub, variant string
			code         int
		}{{"", "", 401}, {"unknown", "", 403}, {"wrong-service", "", 403}, {"retired", "", 403}, {"wrong-scope", "", 403}, {"pinvou:service:token-gateway", "issuer", 401}, {"pinvou:service:token-gateway", "audience", 401}, {"pinvou:service:token-gateway", "expired", 401}, {"pinvou:service:token-gateway", "signature", 401}, {"pinvou:service:token-gateway", "garbage", 401}, {"system:serviceaccount:isolated:token-gateway", "", 200}} {
			q := requests["USER"]
			raw, _ := json.Marshal(q)
			request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/internal/v1/token-account-authority/snapshot", bytes.NewReader(raw))
			if sample.sub != "" {
				request.Header.Set("Authorization", "Bearer "+sign(sample.sub, sample.variant))
			}
			before := trace.queries.Load()
			response, e := http.DefaultClient.Do(request)
			if e != nil {
				t.Fatal(e)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != sample.code {
				t.Fatalf("%s/%s => %d", sample.sub, sample.variant, response.StatusCode)
			}
			if sample.code != 200 && trace.queries.Load() != before {
				t.Fatal("untrusted request reached SQL")
			}
		}
	})
	t.Run("closed-request-before-SQL", func(t *testing.T) {
		q := requests["USER"]
		raw, _ := json.Marshal(q)
		bad := []string{`null`, `{}`, `[]`, string(raw) + ` {}`, strings.TrimSuffix(string(raw), "}") + `,"unknown":true}`, strings.TrimSuffix(string(raw), "}") + `,"mode":"T1_SNAPSHOT"}`, strings.Replace(string(raw), `"authority_schema_version":"1"`, `"authority_schema_version":"2"`, 1), strings.Replace(string(raw), `"expected_actor_version":null,`, "", 1), strings.Replace(string(raw), `"owner_type":`, `"OWNER_TYPE":`, 1)}
		for _, body := range bad {
			before := trace.queries.Load()
			request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/internal/v1/token-account-authority/snapshot", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+credential)
			response, e := http.DefaultClient.Do(request)
			if e != nil {
				t.Fatal(e)
			}
			result, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 400 || !strings.Contains(string(result), "AUTHORITY_REQUEST_INVALID") {
				t.Fatalf("closed body => %d %s", response.StatusCode, result)
			}
			if trace.queries.Load() != before {
				t.Fatal("invalid body reached SQL")
			}
		}
	})
	t.Run("four-mode-null-and-permission-rejection", func(t *testing.T) {
		base := requests["USER"]
		initial := call(t, base)
		var bad []tokenauthority.Request
		q := base
		actorID := actor.ID.String()
		q.ActorUserID = &actorID
		bad = append(bad, q)
		q = base
		q.Mode = tokenauthority.ModeOperatorAuthorize
		bad = append(bad, q)
		for _, mode := range []string{tokenauthority.ModeT3CreatorRecheck, tokenauthority.ModeT3OperatorRecheck} {
			q = ownerCombinedT3(base, initial, mode)
			q.IdentityIssuer = base.IdentityIssuer
			bad = append(bad, q)
			q = ownerCombinedT3(base, initial, mode)
			q.ExpectedActorVersion = nil
			bad = append(bad, q)
		}
		q = base
		q.RequiredPermissions = []string{"platform.tokens.manage"}
		bad = append(bad, q)
		q = base
		q.RequiredPermissions = []string{"platform.tokens.owner-candidates.read", "platform.tokens.manage"}
		bad = append(bad, q)
		q = base
		q.RequiredPermissions = append(append([]string(nil), base.RequiredPermissions...), "platform.users.read")
		bad = append(bad, q)
		for _, q = range bad {
			q.AuthorityRequestSHA256 = q.Digest()
			raw, _ := json.Marshal(q)
			request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/internal/v1/token-account-authority/snapshot", bytes.NewReader(raw))
			request.Header.Set("Authorization", "Bearer "+credential)
			before := trace.queries.Load()
			response, e := http.DefaultClient.Do(request)
			if e != nil {
				t.Fatal(e)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 400 || trace.queries.Load() != before {
				t.Fatalf("invalid mode/null/permissions reached database: %d", response.StatusCode)
			}
		}
	})
	t.Run("actual-response-client-echo-null-and-digest-guards", func(t *testing.T) {
		for _, change := range []string{"reference", "mode", "owner-null", "parent", "actor-digest"} {
			tampered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				rec := httptest.NewRecorder()
				s.Handler(nil).ServeHTTP(rec, q)
				if rec.Code != 200 {
					t.Errorf("expected real authority result, got %d", rec.Code)
				}
				var body map[string]any
				if json.Unmarshal(rec.Body.Bytes(), &body) != nil {
					t.Error("real response decode")
				}
				switch change {
				case "reference":
					body["command_reference"] = uuid.NewString()
				case "mode":
					body["mode"] = tokenauthority.ModeOperatorAuthorize
				case "owner-null":
					body["observed_owner_version"] = nil
				case "parent":
					body["owner_organization_id"] = uuid.NewString()
				case "actor-digest":
					body["actor_identity_digest"] = strings.Repeat("0", 64)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			probe, e := tokendomain.NewHTTPOwnerAuthorityClient(tampered.URL, credential)
			if e != nil {
				t.Fatal(e)
			}
			before := ownerCombinedFacts(t, r)
			if _, e = probe.CheckTokenOwnerAuthority(ctx, requests["ORG_UNIT"]); e == nil {
				t.Fatalf("tampered %s accepted", change)
			}
			tampered.Close()
			if ownerCombinedFacts(t, r) != before {
				t.Fatal("transport negative wrote Account facts")
			}
		}
	})
	t.Run("domain-parent-status-persists-unit-version-and-CAS", func(t *testing.T) {
		principal := entitlementApprovalPrincipal(actor)
		parent, err := r.AdminCreateOrganization(ctx, principal, actor.ID, "Unit authority lifecycle", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		var units []uuid.UUID
		for _, label := range []string{"active-one", "active-two", "disabled", "tombstone"} {
			created, err := r.AdminCreateUnit(ctx, principal, parent.ID, nil, label, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			units = append(units, created.Unit.ID)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE organization_units SET status='DISABLED',version=version+1,updated_at=clock_timestamp() WHERE id=$1`, units[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := r.AdminDeleteUnit(ctx, principal, parent.ID, units[3], uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		other, err := r.AdminCreateOrganization(ctx, principal, actor.ID, "Other unit parent", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		otherUnit, err := r.AdminCreateUnit(ctx, principal, other.ID, nil, "unrelated", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, otherUnit.Unit.ID)
		type observation struct {
			status  string
			version int64
			updated time.Time
		}
		observe := func() map[uuid.UUID]observation {
			t.Helper()
			rows, err := r.pool.Query(ctx, `SELECT id,status,version,updated_at FROM organization_units WHERE id=ANY($1)`, units)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			result := map[uuid.UUID]observation{}
			for rows.Next() {
				var id uuid.UUID
				var item observation
				if err := rows.Scan(&id, &item.status, &item.version, &item.updated); err != nil {
					t.Fatal(err)
				}
				result[id] = item
			}
			if rows.Err() != nil || len(result) != len(units) {
				t.Fatal("unit observation incomplete", rows.Err())
			}
			return result
		}
		baseline := observe()
		var structure int64
		if err := r.pool.QueryRow(ctx, `SELECT structure_version FROM organizations WHERE id=$1`, parent.ID).Scan(&structure); err != nil {
			t.Fatal(err)
		}
		checkAdvance := func(previous map[uuid.UUID]observation) map[uuid.UUID]observation {
			t.Helper()
			got := observe()
			var currentStructure int64
			var parentUpdated time.Time
			if err := r.pool.QueryRow(ctx, `SELECT structure_version,updated_at FROM organizations WHERE id=$1`, parent.ID).Scan(&currentStructure, &parentUpdated); err != nil {
				t.Fatal(err)
			}
			if currentStructure != structure {
				t.Fatal("parent status changed organization structure version")
			}
			for index, id := range units {
				before, after := previous[id], got[id]
				if index < 2 {
					if after.status != "ACTIVE" || after.version != before.version+1 || !after.updated.Equal(parentUpdated) || !after.updated.After(before.updated) {
						t.Fatalf("ACTIVE unit lifecycle version did not advance exactly once: id=%s before=%+v after=%+v", id, before, after)
					}
				} else if after != before {
					t.Fatalf("non-ACTIVE or unrelated unit changed: id=%s before=%+v after=%+v", id, before, after)
				}
			}
			return got
		}
		q := ownerCombinedRequest(actor, "ORG_UNIT", units[0])
		initial := call(t, q)
		if err := r.AdminSetOrganizationStatus(ctx, principal, parent.ID, parent.Version, "DISABLED"); err != nil {
			t.Fatal(err)
		}
		disabled := checkAdvance(baseline)
		negative := call(t, ownerCombinedT3(q, initial, tokenauthority.ModeT3CreatorRecheck))
		if negative.Decision != "DENY" || negative.ReasonCode == nil || *negative.ReasonCode != "OWNER_INACTIVE" || negative.ObservedOwnerVersion == nil || *negative.ObservedOwnerVersion != strconv.FormatInt(disabled[units[0]].version, 10) {
			t.Fatal("domain parent disable lacked newer stored owner evidence")
		}
		facts := ownerCombinedFacts(t, r)
		if err := r.AdminSetOrganizationStatus(ctx, principal, parent.ID, parent.Version, "DISABLED"); err != nil {
			t.Fatal("exact status replay", err)
		}
		if ownerCombinedFacts(t, r) != facts {
			t.Fatal("exact parent status replay changed domain facts")
		}
		for _, input := range []struct {
			version int64
			status  string
		}{{0, "DISABLED"}, {parent.Version + 1, "DISABLED"}, {parent.Version, "ACTIVE"}} {
			if err := r.AdminSetOrganizationStatus(ctx, principal, parent.ID, input.version, input.status); !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid CAS accepted: %+v %v", input, err)
			}
			if ownerCombinedFacts(t, r) != facts {
				t.Fatal("rejected parent status CAS wrote facts")
			}
		}
		if err := r.AdminRenameOrganization(ctx, principal, parent.ID, "Later parent revision"); err != nil {
			t.Fatal(err)
		}
		facts = ownerCombinedFacts(t, r)
		if err := r.AdminSetOrganizationStatus(ctx, principal, parent.ID, parent.Version, "DISABLED"); !errors.Is(err, ErrConflict) {
			t.Fatal("stale exact replay after rename accepted", err)
		}
		if ownerCombinedFacts(t, r) != facts {
			t.Fatal("stale parent projection changed facts")
		}
		current, err := r.AdminOrganizationState(ctx, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.AdminSetOrganizationStatus(ctx, principal, parent.ID, current.Version, "ACTIVE"); err != nil {
			t.Fatal(err)
		}
		enabled := checkAdvance(disabled)
		positive := call(t, ownerCombinedT3(q, initial, tokenauthority.ModeT3OperatorRecheck))
		if positive.Decision != "ALLOW" || positive.ObservedOwnerVersion == nil || *positive.ObservedOwnerVersion != strconv.FormatInt(enabled[units[0]].version, 10) {
			t.Fatal("domain parent enable lacked newer stored owner evidence")
		}
		facts = ownerCombinedFacts(t, r)
		if err := r.AdminSetOrganizationStatus(ctx, principal, parent.ID, current.Version, "ACTIVE"); err != nil || ownerCombinedFacts(t, r) != facts {
			t.Fatal("exact enable replay changed facts", err)
		}
	})
	t.Run("actual-Token-persistent-worker-parent-lifecycle-and-legacy", func(t *testing.T) {
		goExecutable := os.Getenv("OWNER_COMBINED_TEST_GO")
		if goExecutable == "" {
			var err error
			goExecutable, err = exec.LookPath("go")
			if err != nil {
				t.Fatal(err)
			}
		}
		args := []string{"test", "-p=1", "-tags=nomsgpack", "-mod=readonly", "-timeout=20m"}
		if overlay := os.Getenv("OWNER_COMBINED_TEST_OVERLAY"); overlay != "" {
			args = append(args, "-overlay", overlay)
		}
		args = append(args, "./internal/token", "-run", "^TestOwnerCombinedSameVersionPersistentWorker$", "-count=1", "-v")
		command := exec.CommandContext(ctx, goExecutable, args...)
		command.Dir = "../.."
		command.Env = append(os.Environ(), "PINVOU_TEST_TOKEN_DATABASE_URL="+os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"), "OWNER_COMBINED_ACCOUNT_URL="+server.URL, "OWNER_COMBINED_WORKLOAD_JWT="+credential, "OWNER_COMBINED_ACCOUNT_DATABASE_URL="+os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"), "OWNER_COMBINED_ACCOUNT_DATABASE_NAME="+r.pool.Config().ConnConfig.Database, "OWNER_COMBINED_ACTOR="+actor.ID.String(), "OWNER_COMBINED_ORGANIZATION="+org.ID.String(), "OWNER_COMBINED_UNIT="+unit.Unit.ID.String())
		output, err := command.CombinedOutput()
		if path := os.Getenv("OWNER_COMBINED_CHILD_LOG"); path != "" {
			if writeErr := os.WriteFile(path, output, 0600); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
		if err != nil || !bytes.Contains(output, []byte("--- PASS: TestOwnerCombinedSameVersionPersistentWorker")) || bytes.Contains(output, []byte("--- SKIP:")) {
			t.Fatalf("actual Token worker child: %v\n%s", err, output)
		}
		t.Logf("%s", output)
	})
	t.Run("legacy-raw-parent-same-version-RR-fails-closed", func(t *testing.T) {
		q := requests["ORG_UNIT"]
		initial := call(t, q)
		t3 := ownerCombinedT3(q, initial, tokenauthority.ModeT3CreatorRecheck)
		reached, release := make(chan struct{}), make(chan struct{})
		var block atomic.Bool
		block.Store(true)
		observed.now = func() time.Time {
			if block.CompareAndSwap(true, false) {
				close(reached)
				<-release
			}
			return time.Now().UTC()
		}
		result := make(chan tokenauthority.Response, 1)
		failure := make(chan error, 1)
		go func() {
			res, e := client.CheckTokenOwnerAuthority(ctx, t3)
			if e != nil {
				failure <- e
				return
			}
			result <- res
		}()
		<-reached
		if _, e := r.pool.Exec(ctx, `UPDATE organizations SET status='DISABLED',version=version+1,updated_at=clock_timestamp() WHERE id=$1`, org.ID); e != nil {
			close(release)
			t.Fatal(e)
		}
		close(release)
		select {
		case e := <-failure:
			t.Fatal(e)
		case res := <-result:
			if res.Decision != "ALLOW" || *res.ObservedOwnerVersion != *initial.ObservedOwnerVersion {
				t.Fatal("mixed RR snapshot")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		observed.now = time.Now
		next := call(t, t3)
		if next.Decision != "DENY" || next.ReasonCode == nil || *next.ReasonCode != "OWNER_INACTIVE" || *next.ObservedOwnerVersion != *initial.ObservedOwnerVersion || *next.OwnerIdentityDigest == *initial.OwnerIdentityDigest {
			t.Fatal("parent state fabricated version or allowed unit")
		}
		directory, e := tokendomain.NewHTTPOwnerCandidateDirectory(server.URL, credential)
		if e != nil {
			t.Fatal(e)
		}
		if e := directory.Scan(ctx, identity.Principal{Issuer: "https://entitlement-owned.invalid", Subject: actor.Subject}, "UNIT", &org.ID, func(items []tokendomain.AdminOwnerCandidate) error {
			for _, item := range items {
				if item.Status != "DISABLED" {
					t.Fatal("disabled parent exposed active candidate")
				}
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
		// This raw update simulates old/abnormal stored facts, bypassing the real
		// lifecycle writer. It must remain uncertain rather than invent a version.
		t.Log("legacy raw parent corruption: same-version DENY remains PENDING; normal domain parent lifecycle advances unit.version")
		if _, e := r.pool.Exec(ctx, `UPDATE organizations SET status='ACTIVE',version=version+1,updated_at=clock_timestamp() WHERE id=$1`, org.ID); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("actor-current-revoked-and-versions", func(t *testing.T) {
		q := requests["USER"]
		first := call(t, q)
		if _, e := r.pool.Exec(ctx, `UPDATE users SET status='DISABLED',version=version+1 WHERE id=$1`, actor.ID); e != nil {
			t.Fatal(e)
		}
		got := call(t, ownerCombinedT3(q, first, tokenauthority.ModeT3CreatorRecheck))
		if got.Decision != "DENY" || *got.ReasonCode != "ACTOR_INACTIVE" || got.ObservedActorVersion == first.ObservedActorVersion {
			t.Fatal("actor lifecycle not current")
		}
		replacement := ownerCombinedRequest(operator, "USER", user.ID)
		op := call(t, replacement)
		got = call(t, ownerCombinedT3(replacement, op, tokenauthority.ModeT3OperatorRecheck))
		if got.Decision != "ALLOW" || got.ActorUserID != operator.ID.String() {
			t.Fatal("disabled creator blocked current operator")
		}
		if _, e := r.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1`, operator.ID); e != nil {
			t.Fatal(e)
		}
		got = call(t, replacement)
		if got.Decision != "DENY" || *got.ReasonCode != "AUTHORIZATION_REVOKED" {
			t.Fatal("operator revocation ignored")
		}
	})
	t.Run("owner-versions-tombstone-and-missing", func(t *testing.T) {
		// Use a fresh actor, independently of the disabled/revoked actors above.
		fresh := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "owner-fresh")
		q := ownerCombinedRequest(fresh, "USER", user.ID)
		first := call(t, q)
		for _, status := range []string{"CLOSING", "DISABLED", "ACTIVE", "DELETED"} {
			if _, e := r.pool.Exec(ctx, `UPDATE users SET status=$2,version=version+1 WHERE id=$1`, user.ID, status); e != nil {
				t.Fatal(e)
			}
			got := call(t, q)
			expected := "INACTIVE"
			if status == "ACTIVE" {
				expected = "ACTIVE"
			}
			if status == "DELETED" {
				expected = "NOT_FOUND"
			}
			if *got.OwnerStatus != expected || *got.ObservedOwnerVersion == *first.ObservedOwnerVersion {
				t.Fatal("owner status/version", status)
			}
		}
		q = ownerCombinedRequest(fresh, "USER", uuid.New())
		before := ownerCombinedFacts(t, r)
		if _, e := client.CheckTokenOwnerAuthority(ctx, q); e == nil {
			t.Fatal("physical miss became authority")
		}
		if ownerCombinedFacts(t, r) != before {
			t.Fatal("missing owner wrote facts")
		}
	})
	t.Logf("public migrations + actual HTTP/JWT runtime=%s session=%s non-superuser; queries=%d", current, session, trace.queries.Load())
}

func TestOwnerCombinedNullModeBoundaries(t *testing.T) {
	actor := entitlementFixtureActor{ID: uuid.New(), Subject: "synthetic"}
	q := ownerCombinedRequest(actor, "USER", uuid.New())
	raw, _ := json.Marshal(q)
	for _, body := range []string{string(raw), strings.TrimSuffix(string(raw), "}") + `,"unexpected":1}`, strings.TrimSuffix(string(raw), "}") + `,"owner_type":"USER"}`} {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		var got tokenauthority.Request
		e := decodeTokenOwnerAuthorityRequest(w, req, &got)
		if body == string(raw) && e != nil || body != string(raw) && e == nil {
			t.Fatal("closed decoder", e)
		}
	}
}
