package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A transport/state fixture complements, but never replaces, the opt-in real
// OpenBao proof. It can force one-time response loss and revoke uncertainty.
type bootstrapFixture struct {
	*fixture
	mounted, authMounted, rootRevoked bool
	writes                            int
	failPath, failMode                string
	revokeDisconnect                  bool
	invalidLogin, extraGrant          bool
	policies                          map[string]string
	settings                          map[string]map[string]any
	credentials                       map[string]roleCredential
	tokens                            map[string]string
	count                             int
}

func newBootstrapFixture(t *testing.T) *bootstrapFixture {
	t.Helper()
	b := &bootstrapFixture{fixture: newFixture(t), policies: map[string]string{}, settings: map[string]map[string]any{}, credentials: map[string]roleCredential{}, tokens: map[string]string{}}
	b.sealed.Store(false)
	b.server.Config.Handler = http.HandlerFunc(b.serve)
	return b
}

func (b *bootstrapFixture) serve(w http.ResponseWriter, r *http.Request) {
	b.requests.Add(1)
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	token := r.Header.Get("X-Vault-Token")
	root := token == "root-canary-must-not-be-sent" && !b.rootRevoked
	role := b.tokens[token]
	if r.Method == "POST" {
		b.writes++
	}
	if path == b.failPath {
		switch b.failMode {
		case "redirect":
			w.Header().Set("Location", r.URL.Path)
			w.WriteHeader(307)
		case "large":
			io.WriteString(w, strings.Repeat("s", 300<<10))
		case "truncated":
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, "one-time-response-canary")
		default:
			w.WriteHeader(500)
			io.WriteString(w, "one-time-response-canary")
		}
		return
	}
	send := func(data any) { json.NewEncoder(w).Encode(map[string]any{"data": data}) }
	if path == "sys/seal-status" {
		json.NewEncoder(w).Encode(map[string]any{"initialized": true, "sealed": b.sealed.Load(), "type": "shamir", "n": 1, "t": 1})
		return
	}
	if path == "auth/approle/login" {
		var c roleCredential
		json.NewDecoder(r.Body).Decode(&c)
		for name, credential := range b.credentials {
			if c.RoleID == credential.RoleID && c.SecretID == credential.SecretID && !b.invalidLogin {
				b.count++
				newToken := fmt.Sprintf("fixture-bounded-token-canary-%d", b.count)
				b.tokens[newToken] = name
				json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": newToken, "policies": []string{name}, "token_policies": []string{name}, "lease_duration": 300, "renewable": true, "orphan": true, "token_type": "service"}})
				return
			}
		}
		w.WriteHeader(400)
		return
	}
	if !root && role == "" {
		w.WriteHeader(403)
		return
	}
	if path == "auth/token/lookup-self" {
		policies := []string{role}
		if root {
			policies = []string{"root"}
		}
		send(map[string]any{"policies": policies})
		return
	}
	if path == "auth/token/revoke-self" {
		if root {
			b.rootRevoked = true
		} else {
			delete(b.tokens, token)
		}
		if root && b.revokeDisconnect {
			connection, _, _ := w.(http.Hijacker).Hijack()
			connection.Close()
			return
		}
		w.WriteHeader(204)
		return
	}
	if path == "sys/mounts" || path == "sys/auth" {
		if !root {
			w.WriteHeader(403)
			return
		}
		data := map[string]any{"sys/": map[string]any{}}
		if path == "sys/mounts" && b.mounted {
			data["transit/"] = map[string]any{}
		}
		if path == "sys/auth" && b.authMounted {
			data["approle/"] = map[string]any{}
		}
		send(data)
		return
	}
	if path == "sys/policies/acl" {
		keys := []string{"default", "root"}
		for p := range b.policies {
			keys = append(keys, p)
		}
		send(map[string]any{"keys": keys})
		return
	}
	if root && (path == "sys/mounts/transit" || path == "sys/auth/approle") {
		if path == "sys/mounts/transit" {
			b.mounted = true
		} else {
			b.authMounted = true
		}
		w.WriteHeader(204)
		return
	}
	if path == keyPath {
		if r.Method == "POST" && root {
			w.WriteHeader(200)
			return
		}
		if root || role == roleNames[3] {
			send(map[string]any{"type": "aes256-gcm96", "latest_version": 1, "exportable": false, "allow_plaintext_backup": false, "deletion_allowed": false, "derived": false})
			return
		}
	}
	if root && strings.HasPrefix(path, "sys/policies/acl/") {
		name := strings.TrimPrefix(path, "sys/policies/acl/")
		if r.Method == "POST" {
			var data map[string]any
			json.NewDecoder(r.Body).Decode(&data)
			b.policies[name], _ = data["policy"].(string)
			w.WriteHeader(204)
		} else {
			if _, exists := b.policies[name]; !exists {
				w.WriteHeader(404)
				return
			}
			send(map[string]any{"policy": b.policies[name]})
		}
		return
	}
	if root && strings.HasPrefix(path, "auth/approle/role/") {
		parts := strings.Split(strings.TrimPrefix(path, "auth/approle/role/"), "/")
		name := parts[0]
		if len(parts) == 1 {
			if r.Method == "POST" {
				var data map[string]any
				json.NewDecoder(r.Body).Decode(&data)
				b.settings[name] = data
				b.count++
				offset := b.count * 3
				uuid := func(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-000000000000", i) }
				b.credentials[name] = roleCredential{RoleID: uuid(offset), SecretID: uuid(offset + 1), Accessor: uuid(offset + 2)}
				w.WriteHeader(204)
			} else {
				send(b.settings[name])
			}
			return
		}
		c := b.credentials[name]
		if parts[1] == "role-id" {
			send(map[string]string{"role_id": c.RoleID})
		} else {
			send(c)
		}
		return
	}
	if root && path == "sys/capabilities" {
		var q struct {
			Paths []string `json:"paths"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		data := map[string]any{}
		for _, p := range q.Paths {
			data[p] = []string{"deny"}
		}
		if b.extraGrant {
			data[q.Paths[0]] = []string{"update"}
		}
		send(data)
		return
	}
	if (role == roleNames[0] || role == roleNames[1]) && strings.HasPrefix(path, "transit/") {
		var q map[string]string
		json.NewDecoder(r.Body).Decode(&q)
		if path == "transit/encrypt/pinvou-token-data" {
			if q["associated_data"] == "" {
				w.WriteHeader(403)
				return
			}
			send(map[string]string{"ciphertext": "vault:v1:" + q["associated_data"] + ":" + q["plaintext"]})
			return
		}
		if path == "transit/decrypt/pinvou-token-data" {
			parts := strings.Split(q["ciphertext"], ":")
			if len(parts) != 4 || q["associated_data"] != parts[2] {
				w.WriteHeader(400)
				return
			}
			send(map[string]string{"plaintext": parts[3]})
			return
		}
	}
	if role == roleNames[2] && path == "sys/storage/raft/snapshot" {
		w.Write(append([]byte{0x1f, 0x8b}, make([]byte, 100)...))
		return
	}
	w.WriteHeader(403)
}

func TestBootstrapPrepareFinalizeAndNoPlaintext(t *testing.T) {
	f := newBootstrapFixture(t)
	output := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
	encrypted := f.bundle(t)
	ctx := context.Background()
	if err := PrepareBootstrap(ctx, f.cfg, encrypted, f.id, output); err != nil {
		t.Fatal(err)
	}
	if f.rootRevoked {
		t.Fatal("prepare revoked root")
	}
	if err := FinalizeBootstrap(ctx, f.cfg, encrypted, f.id, output); err != nil {
		t.Fatal(err)
	}
	if !f.rootRevoked {
		t.Fatal("root not revoked")
	}
	if err := FinalizeBootstrap(ctx, f.cfg, encrypted, f.id, output); err != nil {
		t.Fatal(err)
	}
	if err := PrepareBootstrap(ctx, f.cfg, encrypted, f.id, output); err != ErrBootstrapPreflight {
		t.Fatal("overwrite accepted")
	}
	if len(f.tokens) != 0 {
		t.Fatal("probe token left active")
	}
	entries, _ := os.ReadDir(output)
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(output, entry.Name()))
		if bytes.Contains(data, []byte("root-canary")) || bytes.Contains(data, []byte("one-time-response-canary")) {
			t.Fatal("plaintext persisted")
		}
		for _, c := range f.credentials {
			if bytes.Contains(data, []byte(c.SecretID)) {
				t.Fatal("SecretID persisted plaintext")
			}
		}
	}
}

func TestBootstrapPartialCapturesAndNeverRevokes(t *testing.T) {
	for _, mode := range []string{"error", "redirect", "large", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			f := newBootstrapFixture(t)
			f.failPath = "auth/approle/role/" + roleNames[0] + "/secret-id"
			f.failMode = mode
			output := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
			err := PrepareBootstrap(context.Background(), f.cfg, f.bundle(t), f.id, output)
			if err != ErrBootstrapPartial || f.rootRevoked {
				t.Fatal("uncertain prepare accepted or root revoked")
			}
			if _, err := os.Stat(filepath.Join(output, "roles.age")); !os.IsNotExist(err) {
				t.Fatal("partial package published")
			}
			if mode != "redirect" {
				if _, err := os.Stat(filepath.Join(output, roleNames[0]+"-response.age")); err != nil {
					t.Fatal("partial response not captured")
				}
			}
			if err := PrepareBootstrap(context.Background(), f.cfg, f.bundle(t), f.id, filepath.Join(filepath.Dir(output), "retry")); err != ErrBootstrapPreflight {
				t.Fatal("partial mount overwritten")
			}
		})
	}
}

func TestBootstrapRejectsBindingAndExistingState(t *testing.T) {
	for _, kind := range []string{"binding", "sealed", "mount", "auth", "policy", "output"} {
		t.Run(kind, func(t *testing.T) {
			f := newBootstrapFixture(t)
			output := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
			switch kind {
			case "binding":
				f.envelope.InstanceID = "wrong-instance-binding"
			case "sealed":
				f.sealed.Store(true)
			case "mount":
				f.mounted = true
			case "auth":
				f.authMounted = true
			case "policy":
				f.policies[roleNames[0]] = "existing"
			case "output":
				os.Mkdir(output, 0700)
			}
			if err := PrepareBootstrap(context.Background(), f.cfg, f.bundle(t), f.id, output); err == nil {
				t.Fatal("preflight accepted")
			}
			if f.writes != 0 {
				t.Fatal("preflight performed write")
			}
			if kind == "binding" && f.requests.Load() != 0 {
				t.Fatal("binding error reached network")
			}
		})
	}
}

func TestBootstrapFinalizeFailureAndUncertainResume(t *testing.T) {
	for _, kind := range []string{"login", "policy", "capability", "corrupt", "disconnect"} {
		t.Run(kind, func(t *testing.T) {
			f := newBootstrapFixture(t)
			output := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
			ctx := context.Background()
			encrypted := f.bundle(t)
			if err := PrepareBootstrap(ctx, f.cfg, encrypted, f.id, output); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "login":
				f.invalidLogin = true
			case "policy":
				f.policies[roleNames[0]] += "extra"
			case "capability":
				f.extraGrant = true
			case "corrupt":
				os.WriteFile(filepath.Join(output, "roles.age"), []byte("damaged"), 0600)
			case "disconnect":
				f.revokeDisconnect = true
			}
			if err := FinalizeBootstrap(ctx, f.cfg, encrypted, f.id, output); err == nil {
				t.Fatal("failure accepted")
			}
			if kind == "disconnect" {
				if !f.rootRevoked {
					t.Fatal("disconnect fixture did not revoke root")
				}
				if err := FinalizeBootstrap(ctx, f.cfg, encrypted, f.id, output); err != nil {
					t.Fatal("same-package finalization cannot resume", err)
				}
			} else if f.rootRevoked {
				t.Fatal("root revoked before verification")
			}
		})
	}
}

func TestBootstrapCompletePackageCanResumeFailedProbe(t *testing.T) {
	f := newBootstrapFixture(t)
	f.invalidLogin = true
	output := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
	ctx := context.Background()
	b := f.bundle(t)
	if err := PrepareBootstrap(ctx, f.cfg, b, f.id, output); err != ErrBootstrapVerify {
		t.Fatal("fixture probe did not fail")
	}
	if _, err := os.Stat(filepath.Join(output, "BOOTSTRAP_PREPARED")); !os.IsNotExist(err) {
		t.Fatal("prepared published too soon")
	}
	f.invalidLogin = false
	if err := FinalizeBootstrap(ctx, f.cfg, b, f.id, output); err != nil {
		t.Fatal("verified complete package cannot resume", err)
	}
	if !f.rootRevoked {
		t.Fatal("root not retired after successful resumed validation")
	}
}

func TestBootstrapRolesBindingRejectsBeforeNetwork(t *testing.T) {
	f := newBootstrapFixture(t)
	output := filepath.Join(filepath.Dir(f.cfg.CAFile), "bootstrap")
	ctx := context.Background()
	init := f.bundle(t)
	if err := PrepareBootstrap(ctx, f.cfg, init, f.id, output); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(output, "roles.age"))
	plaintext, err := custody.Decrypt(original, f.id)
	if err != nil {
		t.Fatal("fixture decrypt failed")
	}
	defer clear(plaintext)
	for _, kind := range []string{"version", "instance", "origin", "ca", "recipient", "time", "missing", "duplicate", "within-role"} {
		t.Run(kind, func(t *testing.T) {
			var envelope bootstrapEnvelope
			if json.Unmarshal(plaintext, &envelope) != nil {
				t.Fatal("fixture decode failed")
			}
			switch kind {
			case "version":
				envelope.Version = 2
			case "instance":
				envelope.InstanceID = "different-instance-id"
			case "origin":
				envelope.Endpoint = "https://elsewhere.invalid"
			case "ca":
				envelope.CASHA256 = strings.Repeat("0", 64)
			case "recipient":
				envelope.RecipientSHA256 = strings.Repeat("0", 64)
			case "time":
				envelope.CreatedAt = "2999-01-01T00:00:00Z"
			case "missing":
				delete(envelope.Roles, roleNames[0])
			case "duplicate":
				envelope.Roles[roleNames[0]] = envelope.Roles[roleNames[1]]
			case "within-role":
				c := envelope.Roles[roleNames[0]]
				c.SecretID = c.RoleID
				envelope.Roles[roleNames[0]] = c
			}
			plain, _ := json.Marshal(envelope)
			b, err := custody.Encrypt(plain, f.id.Recipient())
			clear(plain)
			if err != nil {
				t.Fatal("fixture encrypt failed")
			}
			if os.WriteFile(filepath.Join(output, "roles.age"), b, 0600) != nil {
				t.Fatal("fixture update failed")
			}
			before := f.requests.Load()
			if err := FinalizeBootstrap(ctx, f.cfg, init, f.id, output); err != ErrBinding {
				t.Fatal("bad bound artifact accepted")
			}
			if f.requests.Load() != before || f.rootRevoked {
				t.Fatal("bad bound artifact reached network")
			}
		})
	}
}
