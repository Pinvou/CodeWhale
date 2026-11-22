package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConfiguredSystemProbeRequiresExplicitTrue(t *testing.T) {
	for _, c := range []struct {
		name              string
		code              int
		body, field, want string
	}{
		{"true", 200, `{"success":true}`, "success", "HEALTHY"},
		{"false", 200, `{"success":false}`, "success", "UNHEALTHY"},
		{"missing", 200, `{}`, "success", "UNHEALTHY"},
		{"empty200", 200, "", "success", "UNHEALTHY"},
		{"empty204", 204, "", "success", "UNHEALTHY"},
		{"malformed", 200, `{"success":`, "success", "UNHEALTHY"},
		{"over_limit", 200, `{"success":true}` + strings.Repeat(" ", 64<<10), "success", "UNHEALTHY"},
		{"ordinary204", 204, "", "", "HEALTHY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(c.code); io.WriteString(w, c.body) }))
			defer backend.Close()
			target, _ := url.Parse(backend.URL)
			s := testService(t, newMemoryStore())
			got := s.probe(context.Background(), SystemProbe{Target: target, JSONBooleanField: c.field})
			if got.Status != c.want {
				t.Fatalf("status=%s want=%s", got.Status, c.want)
			}
		})
	}
}

func TestSystemProbeDoesNotFollowRedirect(t *testing.T) {
	var effects atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			effects.Add(1)
			io.WriteString(w, `{"success":true}`)
			return
		}
		w.Header().Set("Location", "/next")
		w.WriteHeader(302)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s := testService(t, newMemoryStore())
	got := s.probe(context.Background(), SystemProbe{Target: target, JSONBooleanField: "success"})
	if got.Status != "UNHEALTHY" || effects.Load() != 0 {
		t.Fatalf("redirect status=%s effects=%d", got.Status, effects.Load())
	}
}
