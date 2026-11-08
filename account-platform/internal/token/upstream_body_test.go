package token

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBoundedUpstreamBodyUsesCompleteHTTPBody(t *testing.T) {
	for _, limit := range []int64{2 << 20, 32 << 20} {
		for _, delta := range []int64{-1, 0, 1} {
			size := limit + delta
			t.Run(strings.Join([]string{formatBodySize(limit), formatBodySize(size)}, "/"), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(w, io.LimitReader(repeatingBodyReader{}, size))
				}))
				defer server.Close()
				client := &http.Client{Timeout: 10 * time.Second}
				response, err := client.Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				payload, err := readBoundedUpstreamBody(response.Body, limit)
				if delta > 0 {
					if err == nil || len(payload) != 0 {
						t.Fatalf("incomplete HTTP body accepted: bytes=%d error=%v", len(payload), err)
					}
					return
				}
				if err != nil || int64(len(payload)) != size {
					t.Fatalf("complete HTTP body rejected: bytes=%d error=%v", len(payload), err)
				}
			})
		}
	}
}

type repeatingBodyReader struct{}

func (repeatingBodyReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
func formatBodySize(n int64) string { return fmt.Sprintf("%d", n) }

type interruptedBodyReader struct{ sent bool }

func (r *interruptedBodyReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("controlled body interruption")
	}
	r.sent = true
	return copy(p, []byte("partial body")), errors.New("controlled body interruption")
}
func TestBoundedUpstreamBodyDiscardsInterruptedPayload(t *testing.T) {
	payload, err := readBoundedUpstreamBody(&interruptedBodyReader{}, 32<<20)
	if err == nil || len(payload) != 0 {
		t.Fatalf("interrupted body exposed: bytes=%d error=%v", len(payload), err)
	}
}
