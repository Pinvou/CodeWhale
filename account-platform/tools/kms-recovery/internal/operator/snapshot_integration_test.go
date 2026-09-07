package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/initialize"
)

// This is only reachable from the explicitly enabled two-instance fixture.
// No snapshot-force capability is exposed by the shipped operator command.
func snapshotRestoreProof(t *testing.T, source Config, rootToken string, id *age.X25519Identity, sourceBundle []byte, bundlePath, targetEndpoint string) {
	t.Helper()
	u, err := url.Parse(targetEndpoint)
	if err != nil || u.Scheme != "https" || u.Port() != "8200" || !net.ParseIP(u.Hostname()).IsPrivate() || targetEndpoint == source.Endpoint {
		t.Fatal("unsafe restore target")
	}
	api := func(cfg Config) func(string, string, string, []byte) (int, []byte) {
		c, _, err := client(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.CloseIdleConnections)
		return func(path, method, token string, body []byte) (int, []byte) {
			r, err := http.NewRequest(method, cfg.Endpoint+"/v1/"+path, bytes.NewReader(body))
			if err != nil {
				t.Fatal("snapshot request setup failed")
			}
			r.GetBody = nil
			r.Header.Set("X-Vault-Token", token)
			r.Header.Set("Content-Type", "application/octet-stream")
			response, err := c.Do(r)
			if err != nil {
				t.Fatal("snapshot transport failed")
			}
			defer response.Body.Close()
			b, err := io.ReadAll(io.LimitReader(response.Body, custody.MaxMaterial+1))
			if err != nil || len(b) > custody.MaxMaterial {
				t.Fatal("snapshot fixture response exceeded bound")
			}
			return response.StatusCode, b
		}
	}
	src := api(source)
	jsonCall := func(call func(string, string, string, []byte) (int, []byte), path, token string, payload any) map[string]any {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal("snapshot fixture encoding failed")
		}
		defer clear(b)
		status, response := call(path, "POST", token, b)
		defer clear(response)
		if status != 200 && status != 204 {
			t.Fatalf("snapshot fixture stage %s status %d", path, status)
		}
		var value map[string]any
		if len(response) > 0 && json.Unmarshal(response, &value) != nil {
			t.Fatal("snapshot fixture JSON invalid")
		}
		return value
	}
	jsonCall(src, "sys/mounts/transit", rootToken, map[string]string{"type": "transit"})
	jsonCall(src, "transit/keys/pinvou-token-data", rootToken, map[string]any{"type": "aes256-gcm96", "exportable": false, "allow_plaintext_backup": false})
	plaintext := base64.StdEncoding.EncodeToString([]byte("snapshot-fixture-plaintext-canary"))
	aad := base64.StdEncoding.EncodeToString([]byte("snapshot-fixture-context"))
	ciphertext := jsonCall(src, "transit/encrypt/pinvou-token-data", rootToken, map[string]string{"plaintext": plaintext, "associated_data": aad})["data"].(map[string]any)["ciphertext"].(string)
	jsonCall(src, "transit/keys/pinvou-token-data/rotate", rootToken, map[string]any{})
	backupPolicy := `path "sys/storage/raft/snapshot" { capabilities = ["read", "sudo"] }`
	readPolicy := `path "transit/decrypt/pinvou-token-data" { capabilities = ["update"] }
path "transit/keys/pinvou-token-data" { capabilities = ["read"] }`
	jsonCall(src, "sys/policies/acl/fixture-backup", rootToken, map[string]string{"policy": backupPolicy})
	jsonCall(src, "sys/policies/acl/fixture-readback", rootToken, map[string]string{"policy": readPolicy})
	token := func(policy string) string {
		return jsonCall(src, "auth/token/create-orphan", rootToken, map[string]any{"policies": []string{policy}, "no_default_policy": true, "ttl": "180s", "explicit_max_ttl": "180s", "renewable": false})["auth"].(map[string]any)["client_token"].(string)
	}
	backupToken, readToken := token("fixture-backup"), token("fixture-readback")
	if status, _ := src("sys/storage/raft/snapshot", "GET", readToken, nil); status != 403 {
		t.Fatal("readback identity can take snapshot")
	}
	jsonCall(src, "auth/token/revoke-self", rootToken, map[string]any{})
	if status, _ := src("auth/token/lookup-self", "GET", rootToken, nil); status != 403 {
		t.Fatal("source root not revoked before backup")
	}
	status, snapshot := src("sys/storage/raft/snapshot", "GET", backupToken, nil)
	if status != 200 || len(snapshot) == 0 {
		t.Fatal("actual Raft snapshot failed")
	}
	encrypted, err := custody.Encrypt(snapshot, id.Recipient())
	clear(snapshot)
	if err != nil {
		t.Fatal("snapshot outer encryption failed")
	}
	snapshotPath := filepath.Join(filepath.Dir(bundlePath), "raft-snapshot.age")
	if os.WriteFile(snapshotPath, encrypted, 0600) != nil {
		t.Fatal("encrypted snapshot write failed")
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := custody.Decrypt(encrypted, other); err == nil {
		t.Fatal("wrong snapshot identity accepted")
	}
	broken := append([]byte(nil), encrypted...)
	broken[len(broken)-1] ^= 1
	if _, err := custody.Decrypt(broken, id); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
	encrypted, err = os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal("snapshot readback failed")
	}
	verified, err := custody.Decrypt(encrypted, id)
	if err != nil {
		t.Fatal("snapshot integrity readback failed")
	}
	defer clear(verified)
	parent := t.TempDir()
	os.Chmod(parent, 0700)
	pubPath := filepath.Join(parent, "recipient.txt")
	os.WriteFile(pubPath, []byte(id.Recipient().String()), 0600)
	initCfg := initialize.Config{Endpoint: targetEndpoint, CAFile: source.CAFile, RecipientFile: pubPath, RecipientSHA256: source.RecipientSHA256, OutputDirectory: filepath.Join(parent, "capture"), InstanceID: "isolated-new-restore-target"}
	if initialize.Run(context.Background(), initCfg, true, true) != nil {
		t.Fatal("fresh restore target initialization failed")
	}
	target := source
	target.Endpoint = targetEndpoint
	target.InstanceID = initCfg.InstanceID
	target.ConnectAddress = ""
	targetBundle, err := os.ReadFile(filepath.Join(initCfg.OutputDirectory, "initialization.age"))
	if err != nil {
		t.Fatal("restore target capture failed")
	}
	if changed, err := Unseal(context.Background(), target, targetBundle, id); err != nil || !changed {
		t.Fatal("fresh restore target unseal failed")
	}
	plain, err := custody.Decrypt(targetBundle, id)
	if err != nil {
		t.Fatal("target test envelope failed")
	}
	defer clear(plain)
	var envelope initialize.Envelope
	var targetMaterial struct {
		Root string `json:"root_token"`
	}
	if json.Unmarshal(plain, &envelope) != nil || json.Unmarshal(envelope.Response, &targetMaterial) != nil {
		t.Fatal("target test material failed")
	}
	defer clear(envelope.Response)
	dst := api(target)
	ready := false
	for range 40 {
		status, _ = dst("sys/health", "GET", "", nil)
		if status == 200 {
			ready = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		t.Fatal("target not ready before restore")
	}
	status, _ = dst("sys/storage/raft/snapshot-force", "POST", targetMaterial.Root, verified)
	if status != 200 && status != 204 {
		t.Fatalf("snapshot restore status %d", status)
	}
	// Force restore installs the source Shamir barrier and seals the target.
	sealed := false
	for range 40 {
		status, b := dst("sys/seal-status", "GET", "", nil)
		var s sealState
		if status == 200 && json.Unmarshal(b, &s) == nil && s.Sealed != nil && *s.Sealed {
			sealed = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !sealed {
		t.Fatal("restored target did not reseal")
	}
	// Route the original logical identity to the owned replacement. The two
	// fixture endpoints share a test certificate; no production cert is reused.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("restore loopback failed")
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				up, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
				if err != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{})
				go func() {
					io.Copy(up, conn)
					if tcp, ok := up.(*net.TCPConn); ok {
						tcp.CloseWrite()
					}
					close(done)
				}()
				io.Copy(conn, up)
				conn.Close()
				<-done
			}()
		}
	}()
	restoreCfg := source
	restoreCfg.ConnectAddress = listener.Addr().String()
	if changed, err := Unseal(context.Background(), restoreCfg, sourceBundle, id); err != nil || !changed {
		t.Fatal("source recovery package did not unseal replacement")
	}
	ready = false
	for range 40 {
		status, _ = dst("sys/health", "GET", "", nil)
		if status == 200 {
			ready = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		t.Fatal("restored target not ready")
	}
	for _, oldRoot := range []string{rootToken, targetMaterial.Root} {
		if status, _ := dst("auth/token/lookup-self", "GET", oldRoot, nil); status != 403 {
			t.Fatal("restore revived an obsolete root")
		}
	}
	result := jsonCall(dst, "transit/decrypt/pinvou-token-data", readToken, map[string]string{"ciphertext": ciphertext, "associated_data": aad})
	if result["data"].(map[string]any)["plaintext"] != plaintext {
		t.Fatal("independent restore lost historical ciphertext")
	}
	status, b := dst("transit/keys/pinvou-token-data", "GET", readToken, nil)
	var metadata struct {
		Data struct {
			LatestVersion int `json:"latest_version"`
		} `json:"data"`
	}
	if status != 200 || json.Unmarshal(b, &metadata) != nil || metadata.Data.LatestVersion != 2 {
		t.Fatal("independent restore lost key rotation")
	}
	t.Log("ACTUAL_OPENBAO_SNAPSHOT_RESTORE_PASS")
}
