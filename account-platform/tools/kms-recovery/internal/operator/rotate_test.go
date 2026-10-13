package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type rotationFixture struct {
	*bootstrapFixture
	rolesDirectory                           string
	version, rotations, logins, rootRequests int
	mode                                     string
	serverNow                                int64
	invalidResponse, loseLogin, failRevoke   bool
}

func newRotationFixture(t *testing.T) *rotationFixture {
	t.Helper()
	b := newBootstrapFixture(t)
	roles := filepath.Join(filepath.Dir(b.cfg.CAFile), "bootstrap")
	if PrepareBootstrap(context.Background(), b.cfg, b.bundle(t), b.id, roles) != nil || FinalizeBootstrap(context.Background(), b.cfg, b.bundle(t), b.id, roles) != nil {
		t.Fatal("rotation bootstrap fixture failed")
	}
	f := &rotationFixture{bootstrapFixture: b, rolesDirectory: roles, version: 1, serverNow: time.Now().Unix()}
	old := b.server.Config.Handler
	b.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := strings.TrimPrefix(r.URL.Path, "/v1/")
		token := r.Header.Get("X-Vault-Token")
		if api == "sys/health" {
			json.NewEncoder(w).Encode(map[string]any{"initialized": true, "sealed": false, "server_time_utc": f.serverNow})
			return
		}
		if strings.Contains(token, "root-canary") {
			f.rootRequests++
			t.Error("rotation used root")
		}
		if api == "auth/approle/login" {
			f.logins++
			if f.invalidResponse || f.loseLogin {
				recorder := httptest.NewRecorder()
				old.ServeHTTP(recorder, r)
				if f.loseLogin {
					connection, _, _ := w.(http.Hijacker).Hijack()
					connection.Close()
					return
				}
				var response map[string]any
				json.Unmarshal(recorder.Body.Bytes(), &response)
				response["auth"].(map[string]any)["lease_duration"] = 10000
				json.NewEncoder(w).Encode(response)
				return
			}
		}
		if b.tokens[token] != roleNames[3] {
			old.ServeHTTP(w, r)
			return
		}
		send := func(value any) { json.NewEncoder(w).Encode(map[string]any{"data": value}) }
		switch api {
		case "auth/token/lookup-self":
			d := map[string]any{"policies": []string{roleNames[3]}, "ttl": 200, "explicit_max_ttl": 900, "type": "service", "path": "auth/approle/login", "renewable": true, "orphan": true, "meta": map[string]string{"role_name": roleNames[3]}}
			d["creation_time"] = f.serverNow
			if f.mode == "extra-policy" {
				d["identity_policies"] = []string{"fixture-extra-policy"}
			}
			send(d)
		case keyPath:
			d := map[string]any{"type": "aes256-gcm96", "latest_version": f.version, "exportable": false, "allow_plaintext_backup": false, "deletion_allowed": false, "derived": false, "min_decryption_version": 1}
			if f.mode == "unsafe-key" {
				d["exportable"] = true
			}
			if f.mode == "missing-key-field" {
				delete(d, "min_decryption_version")
			}
			send(d)
		case keyPath + "/rotate":
			if r.Method != "POST" {
				t.Error("rotation wrong method")
			}
			f.rotations++
			if f.mode != "unchanged" {
				f.version++
			}
			if f.mode == "concurrent" {
				f.version++
			}
			if f.mode == "lost-response" {
				connection, _, _ := w.(http.Hijacker).Hijack()
				connection.Close()
				return
			}
			send(map[string]any{})
		case "auth/token/revoke-self":
			if f.mode == "revoke-failed" || f.failRevoke {
				w.WriteHeader(500)
				return
			}
			old.ServeHTTP(w, r)
		default:
			old.ServeHTTP(w, r)
		}
	})
	return f
}

func (f *rotationFixture) rotate(output string, expected int) error {
	return RotateTransit(context.Background(), f.cfg, f.rolesDirectory, f.id, output, expected)
}

func requireSupportedTransitRotation(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("transit rotation requires the Linux/Windows process lock; unsupported platforms fail closed")
	}
}

func TestRotationOneMutationAndReadOnlyResume(t *testing.T) {
	requireSupportedTransitRotation(t)
	for _, mode := range []string{"normal", "lost-response", "unchanged", "concurrent", "revoke-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRotationFixture(t)
			f.mode = mode
			parent := t.TempDir()
			os.Chmod(parent, 0700)
			output := filepath.Join(parent, "attempt")
			err := f.rotate(output, 1)
			if (mode == "normal") != (err == nil) || f.rotations != 1 || f.rootRequests != 0 {
				t.Fatal("initial rotation contract failed")
			}
			if mode != "revoke-failed" && len(f.tokens) != 0 {
				t.Fatal("operation token not retired")
			}
			attempt, readErr := os.ReadFile(filepath.Join(output, "ROTATION_ATTEMPT"))
			if readErr != nil {
				t.Fatal("attempt not persisted")
			}
			for _, credential := range f.credentials {
				if bytes.Contains(attempt, []byte(credential.SecretID)) {
					t.Fatal("attempt leaks identity")
				}
			}
			if mode != "normal" {
				if _, err := os.Stat(filepath.Join(output, "ROTATION_VERIFIED")); !os.IsNotExist(err) {
					t.Fatal("unknown attempt marked complete")
				}
			}
			f.mode = "normal"
			err = f.rotate(output, 1)
			canVerify := mode != "unchanged" && mode != "concurrent" && mode != "revoke-failed"
			if canVerify != (err == nil) || f.rotations != 1 {
				t.Fatal("resume repeated rotation or accepted wrong version")
			}
			stored, _ := os.ReadFile(filepath.Join(output, "ROTATION_ATTEMPT"))
			if !bytes.Equal(stored, attempt) {
				t.Fatal("attempt overwritten")
			}
			if mode == "revoke-failed" {
				// Simulate the hard token lifetime passing, not a second rotate.
				f.serverNow += 1000
				clear(f.tokens)
				if f.rotate(output, 1) != nil || f.rotations != 1 {
					t.Fatal("expired original identity prevented query-only recovery")
				}
			}
		})
	}
}

func TestRotationRejectsPreflightAndDamagedAttempts(t *testing.T) {
	requireSupportedTransitRotation(t)
	for _, mode := range []string{"extra-policy", "unsafe-key", "missing-key-field", "version", "unfinalized", "partial", "expired", "wrong-binding"} {
		t.Run(mode, func(t *testing.T) {
			f := newRotationFixture(t)
			parent := t.TempDir()
			os.Chmod(parent, 0700)
			output := filepath.Join(parent, "attempt")
			f.mode = mode
			expected := 1
			if mode == "version" {
				expected = 2
			}
			if mode == "unfinalized" {
				os.Remove(filepath.Join(f.rolesDirectory, "BOOTSTRAP_FINALIZED"))
			}
			if mode == "partial" {
				os.Mkdir(output, 0700)
			}
			if mode == "expired" || mode == "wrong-binding" {
				f.mode = "normal"
				if f.rotate(output, 1) != nil {
					t.Fatal("seed rotation failed")
				}
				raw, _ := os.ReadFile(filepath.Join(output, "ROTATION_ATTEMPT"))
				var record rotationAttempt
				json.Unmarshal(raw, &record)
				if mode == "expired" {
					record.CreatedAt = time.Now().Add(-25 * time.Hour).Format(time.RFC3339Nano)
				} else {
					record.CA = strings.Repeat("0", 64)
				}
				raw, _ = json.Marshal(record)
				os.WriteFile(filepath.Join(output, "ROTATION_ATTEMPT"), raw, 0600)
			}
			before := f.rotations
			if f.rotate(output, expected) == nil || f.rotations != before || len(f.tokens) != 0 || f.rootRequests != 0 {
				t.Fatal("unsafe rotation preflight accepted")
			}
			if before == 0 && mode != "partial" {
				if _, err := os.Stat(filepath.Join(output, "ROTATION_ATTEMPT")); !os.IsNotExist(err) {
					t.Fatal("rejected preflight recorded a rotation attempt")
				}
			}
		})
	}
}

func TestRotationReplayRetirementCannotBorrowPreviousSuccess(t *testing.T) {
	requireSupportedTransitRotation(t)
	f := newRotationFixture(t)
	parent := t.TempDir()
	os.Chmod(parent, 0700)
	output := filepath.Join(parent, "attempt")
	if f.rotate(output, 1) != nil {
		t.Fatal("initial rotation failed")
	}
	f.failRevoke = true
	if f.rotate(output, 1) != ErrRotateCleanup || len(f.tokens) != 1 {
		t.Fatal("replay cleanup failure hidden")
	}
	before := f.logins
	f.failRevoke = false
	if f.rotate(output, 1) != ErrRotateCleanup || f.logins != before || f.rotations != 1 {
		t.Fatal("pending replay admitted another token")
	}
	// Only the remote clock establishes expiry; no local record timestamp edit.
	f.serverNow += 1000
	clear(f.tokens)
	if f.rotate(output, 1) != nil || f.rotations != 1 || len(f.tokens) != 0 {
		t.Fatal("verified expiry did not permit read-only recovery")
	}
}

func TestRotationInvalidAndUnknownLoginRemainOwned(t *testing.T) {
	requireSupportedTransitRotation(t)
	for _, mode := range []string{"invalid-retired", "invalid-unretired", "lost"} {
		t.Run(mode, func(t *testing.T) {
			f := newRotationFixture(t)
			parent := t.TempDir()
			os.Chmod(parent, 0700)
			output := filepath.Join(parent, "attempt")
			f.invalidResponse = mode != "lost"
			f.loseLogin = mode == "lost"
			f.failRevoke = mode == "invalid-unretired"
			if f.rotate(output, 1) == nil || f.rotations != 0 {
				t.Fatal("invalid login accepted")
			}
			if (mode == "invalid-retired") != (len(f.tokens) == 0) {
				t.Fatal("received invalid token not accounted for")
			}
			f.invalidResponse, f.loseLogin, f.failRevoke = false, false, false
			before := f.logins
			f.serverNow += 1000
			if f.rotate(output, 1) == nil || f.logins != before || f.rotations != 0 {
				t.Fatal("unverified lease or absent rotation attempt incorrectly resumed")
			}
		})
	}
}

func TestRotationLockAndPendingRecordIntegrity(t *testing.T) {
	requireSupportedTransitRotation(t)
	for _, mode := range []string{"locked", "missing-lease", "bad-binding", "bad-closed", "archive-collision", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			f := newRotationFixture(t)
			parent := t.TempDir()
			os.Chmod(parent, 0700)
			output := filepath.Join(parent, "attempt")
			f.failRevoke = true
			if f.rotate(output, 1) != ErrRotateCleanup {
				t.Fatal("pending seed failed")
			}
			f.failRevoke = false
			before := f.logins
			current := filepath.Join(output, "CURRENT_INVOCATION")
			var invocation rotationInvocation
			raw, err := readRotationJSON(filepath.Join(current, "STARTED"), &invocation)
			if err != nil {
				t.Fatal("pending record missing")
			}
			switch mode {
			case "locked":
				lock, err := rotationLock(output)
				if err != nil {
					t.Fatal("test lock failed")
				}
				defer func() { unlockRotationFile(lock); lock.Close() }()
			case "missing-lease":
				os.Remove(filepath.Join(current, "LEASE"))
				f.serverNow += 1000
			case "bad-binding":
				invocation.Context = strings.Repeat("0", 64)
				b, _ := json.Marshal(invocation)
				os.WriteFile(filepath.Join(current, "STARTED"), b, 0600)
			case "bad-closed":
				os.WriteFile(filepath.Join(current, "CLOSED"), []byte("damaged"), 0600)
			case "archive-collision":
				os.WriteFile(filepath.Join(current, "CLOSED"), []byte("KMS_INVOCATION_RETIRED_V1\n"+hash(raw)+"\n"), 0600)
				os.Mkdir(filepath.Join(output, "invocation-"+invocation.ID), 0700)
			case "rollback":
				f.serverNow--
			}
			if f.rotate(output, 1) == nil || f.logins != before || f.rotations != 1 {
				t.Fatal("unsafe pending state accepted")
			}
		})
	}
}

func TestRotationMalformedAttributesPreserveKnownToken(t *testing.T) {
	f := newRotationFixture(t)
	c, caHash, err := client(f.cfg)
	if err != nil {
		t.Fatal("client setup failed")
	}
	defer c.CloseIdleConnections()
	original := f.server.Config.Handler
	for _, field := range []string{"policies", "token_policies", "lease_duration", "renewable", "orphan", "token_type"} {
		for _, first := range []bool{true, false} {
			for _, fail := range []bool{false, true} {
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/auth/approle/login" {
						original.ServeHTTP(w, r)
						return
					}
					recorder := httptest.NewRecorder()
					original.ServeHTTP(recorder, r)
					var response map[string]any
					json.Unmarshal(recorder.Body.Bytes(), &response)
					auth := response["auth"].(map[string]any)
					auth[field] = map[string]any{"wrong": "type"}
					tokenJSON, _ := json.Marshal(auth["client_token"])
					delete(auth, "client_token")
					attributes, _ := json.Marshal(auth)
					if first {
						w.Write([]byte(`{"auth":{"client_token":` + string(tokenJSON) + `,` + string(attributes[1:]) + `}`))
					} else {
						w.Write([]byte(`{"auth":` + string(attributes[:len(attributes)-1]) + `,"client_token":` + string(tokenJSON) + `}}`))
					}
				})
				ctx, cancel := context.WithCancel(context.Background())
				s := &bootstrapSession{ctx: ctx, client: c, cfg: f.cfg, caHash: caHash}
				token, valid := s.rotationLogin(f.credentials[roleNames[3]])
				cancel()
				if token == "" || valid {
					t.Fatalf("known identity lost for %s", field)
				}
				f.failRevoke = fail
				err := s.retireRotationToken(token)
				if fail != (err == ErrRotateCleanup) || (!fail && len(f.tokens) != 0) {
					t.Fatal("retirement result incorrect")
				}
				f.failRevoke = false
				if fail && s.retireRotationToken(token) != nil {
					t.Fatal("fixture cleanup failed")
				}
			}
		}
	}
}

func TestRotationServerClockRequiresExplicitHealthyState(t *testing.T) {
	f := newRotationFixture(t)
	c, caHash, err := client(f.cfg)
	if err != nil {
		t.Fatal("client setup failed")
	}
	defer c.CloseIdleConnections()
	s := &bootstrapSession{ctx: context.Background(), client: c, cfg: f.cfg, caHash: caHash}
	for _, response := range []string{
		`{"initialized":true,"server_time_utc":1000}`,
		`{"initialized":true,"sealed":null,"server_time_utc":1000}`,
		`{"initialized":true,"sealed":"false","server_time_utc":1000}`,
		`{"initialized":true,"sealed":true,"server_time_utc":1000}`,
		`{"initialized":true,"sealed":false,"server_time_utc":null}`,
		`{"initialized":true,"sealed":false,"server_time_utc":"1000"}`,
		`{"initialized":true,"sealed":false,"server_time_utc":1000.5}`,
		`{"initialized":true,"sealed":false,"server_time_utc":9223372036854775807}`,
	} {
		f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(response)) })
		if _, err := s.rotationServerTime(); err == nil {
			t.Fatal("malformed clock accepted")
		}
	}
}

func TestRotationRetirementSurvivesCancelledContext(t *testing.T) {
	f := newRotationFixture(t)
	c, caHash, err := client(f.cfg)
	if err != nil {
		t.Fatal("client setup failed")
	}
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	s := &bootstrapSession{ctx: ctx, client: c, cfg: f.cfg, caHash: caHash}
	f.invalidResponse = true
	token, valid := s.rotationLogin(f.credentials[roleNames[3]])
	if token == "" || valid {
		t.Fatal("invalid received token ownership lost")
	}
	cancel()
	if s.retireRotationToken(token) != nil || len(f.tokens) != 0 {
		t.Fatal("cancelled operation prevented independent retirement")
	}
}
