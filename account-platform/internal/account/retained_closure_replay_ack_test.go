package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetainedClosureReplayHTTPACK(t *testing.T) {
	user := uuid.New()
	for _, status := range []int{http.StatusNoContent, http.StatusOK, http.StatusAccepted, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodDelete || r.URL.Path != "/internal/v1/users/"+user.String()+"/replay-payloads" || r.Header.Get("Authorization") != "Bearer closure-fixture" {
					t.Error("cleanup request facts changed")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			client, err := NewHTTPAccountClosureClient(server.URL, "closure-fixture", "TOKEN")
			if err != nil {
				t.Fatal(err)
			}
			err = client.ClearReplay(context.Background(), user)
			if (err == nil) != (status == http.StatusNoContent) || calls != 1 {
				t.Fatalf("status=%d error=%v calls=%d", status, err, calls)
			}
		})
	}
	t.Run("invalid-tuple-does-not-dispatch", func(t *testing.T) {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
		defer server.Close()
		client, _ := NewHTTPAccountClosureClient(server.URL, "closure-fixture", "TOKEN")
		if client.ClearReplay(context.Background(), uuid.Nil) == nil {
			t.Fatal("nil user accepted")
		}
		client, _ = NewHTTPAccountClosureClient(server.URL, "closure-fixture", "BILLING")
		if client.ClearReplay(context.Background(), user) == nil {
			t.Fatal("wrong domain accepted")
		}
		if calls != 0 {
			t.Fatal("invalid authority tuple dispatched")
		}
	})
	t.Run("workload-file-refreshes-on-cleanup-retry", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "workload-token")
		expected := "first-fixture"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+expected {
				t.Error("stale workload credential")
			}
			w.WriteHeader(204)
		}))
		defer server.Close()
		client, _ := NewHTTPAccountClosureClient(server.URL, "file:"+path, "TOKEN")
		for _, value := range []string{"first-fixture", "second-fixture"} {
			expected = value
			if err := os.WriteFile(path, []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			if err := client.ClearReplay(context.Background(), user); err != nil {
				t.Fatal(err)
			}
		}
	})
}

type closureReplayRoundTrip func(*http.Request) (*http.Response, error)

func (f closureReplayRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closureReplayBody struct {
	io.Reader
	closed bool
}

func (b *closureReplayBody) Close() error { b.closed = true; return nil }

type closureReplayReadError struct{}

func (closureReplayReadError) Read([]byte) (int, error) {
	return 0, errors.New("controlled incomplete body")
}
func TestRetainedClosureReplayMalformedACK(t *testing.T) {
	for _, c := range []struct {
		name   string
		reader io.Reader
		status int
		accept bool
	}{
		{"empty-204", strings.NewReader(""), 204, true},
		{"nonempty-204", strings.NewReader("unexpected"), 204, false},
		{"oversize-204", strings.NewReader(strings.Repeat("x", (64<<10)+1)), 204, false},
		{"incomplete-204", closureReplayReadError{}, 204, false},
		{"empty-200", strings.NewReader(""), 200, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := &closureReplayBody{Reader: c.reader}
			client, _ := NewHTTPAccountClosureClient("https://closure.invalid", "closure-fixture", "TOKEN")
			client.client = &http.Client{Transport: closureReplayRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: c.status, Body: body, Header: make(http.Header)}, nil
			})}
			err := client.ClearReplay(context.Background(), uuid.New())
			if (err == nil) != c.accept || !body.closed {
				t.Fatalf("accept=%v closed=%v error=%v", c.accept, body.closed, err)
			}
		})
	}
}
