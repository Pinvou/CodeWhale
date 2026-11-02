package transit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSelectedKeyBoundToRequestAndHistoricalDecryption(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sink")
	if err := os.WriteFile(file, []byte("first-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path+"/"+r.Header.Get("X-Vault-Token"))
		mu.Unlock()
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["associated_data"] != base64.StdEncoding.EncodeToString([]byte("case-bound-aad")) {
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/v1/transit/encrypt/previous-key":
			if body["plaintext"] != base64.StdEncoding.EncodeToString([]byte("dek-material")) {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"ciphertext":"vault:v2:YWJjZA=="}}`))
		case "/v1/transit/decrypt/previous-key":
			if body["ciphertext"] != "vault:v2:YWJjZA==" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"plaintext":"ZGVrLW1hdGVyaWFs"}}`))
		default:
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	p, err := NewTransitProvider(Config{Environment: "test", KeyID: "current-key", RemoteURL: server.URL, RemoteTokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	if p.KeyID() != "current-key" {
		t.Fatal("configured current key changed")
	}
	ciphertext, err := p.EncryptWithKeyIDContext(context.Background(), "previous-key", []byte("dek-material"), []byte("case-bound-aad"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("second-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	plain, err := p.DecryptWithKeyIDContext(context.Background(), "previous-key", ciphertext, []byte("case-bound-aad"))
	if err != nil || string(plain) != "dek-material" {
		t.Fatal("selected historical key did not round trip", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "/v1/transit/encrypt/previous-key/first-test-credential" || calls[1] != "/v1/transit/decrypt/previous-key/second-test-credential" {
		t.Fatal("request did not bind the selected key or rotate the sink")
	}
}

func TestSelectedKeyRejectsInvalidBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	p, err := NewTransitProvider(Config{Environment: "test", KeyID: "active-key", RemoteURL: server.URL, RemoteToken: "test-only-credential"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "../other", "key/other", "other?x", "bad.name", string([]byte{0xff})} {
		if _, err = p.EncryptWithKeyID(key, []byte("dek"), []byte("aad")); !errors.Is(err, ErrInvalidSecret) {
			t.Fatal("invalid selected key accepted")
		}
	}
	if _, err = p.EncryptWithKeyID("active-key", nil, []byte("aad")); !errors.Is(err, ErrInvalidSecret) {
		t.Fatal("empty plaintext accepted")
	}
	if _, err = p.EncryptWithKeyID("active-key", []byte("dek"), nil); !errors.Is(err, ErrInvalidSecret) {
		t.Fatal("empty associated data accepted")
	}
	if calls != 0 {
		t.Fatal("invalid input reached KMS")
	}
}

func TestContextCancellationReachesTLSRequest(t *testing.T) {
	for _, operation := range []string{"encrypt", "decrypt"} {
		for _, phase := range []string{"headers", "body"} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
				arrived := make(chan string, 1)
				released := make(chan struct{})
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if phase == "body" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(`{"data":`))
						w.(http.Flusher).Flush()
					}
					arrived <- r.URL.Path
					select {
					case <-r.Context().Done():
					case <-released:
					}
				}))
				defer server.Close()
				defer close(released)
				ca := filepath.Join(t.TempDir(), "ca.pem")
				if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
					t.Fatal(err)
				}
				p, err := NewTransitProvider(Config{Environment: "production", KeyID: "current-key", RemoteURL: server.URL, RemoteTokenFile: writeTestSink(t), TransitCAFile: ca})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					var err error
					if operation == "encrypt" {
						_, err = p.EncryptWithKeyIDContext(ctx, "previous-key", []byte("private-data"), []byte("aad"))
					} else {
						_, err = p.DecryptWithKeyIDContext(ctx, "previous-key", []byte("vault:v1:YWJjZA=="), []byte("aad"))
					}
					done <- err
				}()
				select {
				case path := <-arrived:
					if path != "/v1/transit/"+operation+"/previous-key" {
						t.Fatal("request did not use selected key")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("request did not reach server")
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatal("request did not return cancellation")
					}
					if err.Error() != context.Canceled.Error() {
						t.Fatal("cancellation disclosed transport details")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not interrupt request")
				}
			})
		}
	}
}

func writeTestSink(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sink")
	if err := os.WriteFile(path, []byte("test-only-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cancelBlockedBody struct {
	ctx     context.Context
	started chan struct{}
	closed  chan struct{}
}

func (b *cancelBlockedBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelBlockedBody) Close() error { close(b.closed); return nil }

func TestContextCancellationDuringBodyReadClosesBody(t *testing.T) {
	for _, operation := range []string{"encrypt", "decrypt"} {
		t.Run(operation, func(t *testing.T) {
			p, err := NewTransitProvider(Config{Environment: "test", KeyID: "current", RemoteURL: "http://localhost", RemoteToken: "test-only-credential"})
			if err != nil {
				t.Fatal(err)
			}
			started, closed := make(chan struct{}), make(chan struct{})
			p.client.Transport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &cancelBlockedBody{ctx: r.Context(), started: started, closed: closed}}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if operation == "encrypt" {
					_, err = p.EncryptWithKeyIDContext(ctx, "current", []byte("private-data"), []byte("aad"))
				} else {
					_, err = p.DecryptWithKeyIDContext(ctx, "current", []byte("vault:v1:YWJjZA=="), []byte("aad"))
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("response body read did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
					t.Fatal("body cancellation did not return safe cancellation error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("body read did not cancel")
			}
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestAlreadyCanceledSkipsSinkRead(t *testing.T) {
	sink := writeTestSink(t)
	p, err := NewTransitProvider(Config{Environment: "test", KeyID: "current", RemoteURL: "http://localhost", RemoteTokenFile: sink})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sink); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.EncryptWithKeyIDContext(ctx, "current", []byte("private-data"), []byte("aad")); !errors.Is(err, context.Canceled) {
		t.Fatal("encrypt touched sink after cancellation")
	}
	if _, err := p.DecryptWithKeyIDContext(ctx, "current", []byte("vault:v1:YWJjZA=="), []byte("aad")); !errors.Is(err, context.Canceled) {
		t.Fatal("decrypt touched sink after cancellation")
	}
}
