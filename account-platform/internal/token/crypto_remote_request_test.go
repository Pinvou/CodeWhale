package token

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRemoteEnvelopeRequestCountsEncodedFields(t *testing.T) {
	const key = "historical/opaque:<&>"
	value, aad := []byte("value"), []byte("scope")
	want := []byte(`{"associated_data_b64":"c2NvcGU=","key_id":"historical/opaque:\u003c\u0026\u003e","value_base64":"dmFsdWU="}`)
	got, err := encodeRemoteEnvelopeRequest(key, value, aad, len(want))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("exact encoded boundary or original wire format changed", err)
	}
	if got, err = encodeRemoteEnvelopeRequest(key, value, aad, len(want)-1); !errors.Is(err, ErrInvalidSecret) || got != nil {
		t.Fatal("encoded metadata overflow accepted", err)
	}
}

func TestRemoteEnvelopeRequestBoundsPreserveLargeCaller(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		var request map[string]string
		if err != nil || json.Unmarshal(raw, &request) != nil || r.URL.Path != "/v1/encrypt" || request["key_id"] != "remote/opaque:key" || request["associated_data_b64"] != "c2NvcGU=" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		value, err := base64.StdEncoding.DecodeString(request["value_base64"])
		if err != nil || len(value) != (1<<20)+1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		_, _ = io.WriteString(w, `{"value_base64":"Y2lwaGVy"}`)
	}))
	t.Cleanup(server.Close)
	provider, err := NewRemoteEnvelopeProvider("development", "remote/opaque:key", server.URL, "controlled-remote-envelope-fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	// Remote has callers above the 1 MiB Transit plaintext ceiling.
	if got, err := provider.Encrypt(make([]byte, (1<<20)+1), []byte("scope")); err != nil || string(got) != "cipher" || calls.Load() != 1 {
		t.Fatal("compatible large request did not reach Remote", err)
	}
	// Both fields individually fit the wire budget, but their two Base64
	// encodings do not fit together. Reuse one bounded allocation for the probe.
	large := make([]byte, 25<<20)
	if got, err := provider.Encrypt(large, large); !errors.Is(err, ErrInvalidSecret) || got != nil || calls.Load() != 1 {
		t.Fatal("oversized combined envelope reached Remote", err)
	}
	oversizedKey := string(make([]byte, (1<<20)+1))
	if got, err := provider.DecryptWithKeyID(oversizedKey, []byte("cipher"), []byte("scope")); !errors.Is(err, ErrInvalidSecret) || got != nil || calls.Load() != 1 {
		t.Fatal("oversized historical key metadata reached Remote", err)
	}
	if got, err := NewRemoteEnvelopeProvider("development", oversizedKey, server.URL, "controlled-remote-envelope-fixture-token"); !errors.Is(err, ErrInvalidSecret) || got != nil || calls.Load() != 1 {
		t.Fatal("oversized configured key metadata accepted", err)
	}
}
