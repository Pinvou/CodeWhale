package token

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoteEnvelopeRequiresCompleteBoundedResponse(t *testing.T) {
	const wireLimit = 1 << 20 // Original Remote wire capacity, including JSON whitespace.
	const object = `{"value_base64":"c2VjcmV0","provider_extension":true}`
	for _, tc := range []struct {
		name, body string
		incomplete bool
		want       bool
	}{
		{name: "ordinary-extension-and-whitespace", body: object + " \r\n", want: true},
		{name: "exact-wire-limit", body: object + strings.Repeat(" ", wireLimit-len(object)), want: true},
		{name: "one-byte-over-limit", body: object + strings.Repeat(" ", wireLimit-len(object)+1)},
		{name: "second-json", body: object + ` {"extra":true}`},
		{name: "garbage-tail", body: object + ` invalid`},
		{name: "incomplete-http-body", body: object, incomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/decrypt" || r.Header.Get("Authorization") != "Bearer controlled-remote-envelope-fixture-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.incomplete {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+1))
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			provider, err := NewRemoteEnvelopeProvider("development", "active/opaque:key", server.URL, "controlled-remote-envelope-fixture-token")
			if err != nil {
				t.Fatal(err)
			}
			value, err := provider.DecryptWithKeyID("historical/opaque:key", []byte("controlled ciphertext"), []byte("controlled aad"))
			if tc.want {
				if err != nil || !bytes.Equal(value, []byte("secret")) {
					t.Fatal("valid original Remote response rejected", err)
				}
			} else if !errors.Is(err, ErrInvalidSecret) || len(value) != 0 {
				t.Fatalf("incomplete or oversized response was accepted: returned=%t, invalid_error=%t", len(value) != 0, errors.Is(err, ErrInvalidSecret))
			}
		})
	}
}
