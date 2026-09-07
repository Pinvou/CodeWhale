package serviceauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestWorkloadValidityInterval(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, test := range []struct {
		name           string
		issued, expiry time.Time
		notBefore      int64
		want           bool
	}{
		{"valid", now, now.Add(5 * time.Minute), 0, true},
		{"maximum", now, now.Add(15 * time.Minute), now.Unix(), true},
		{"missing-issued", time.Time{}, now.Add(time.Minute), 0, false},
		{"future-issued", now.Add(time.Second), now.Add(time.Minute), 0, false},
		{"future-not-before", now, now.Add(time.Minute), now.Add(time.Second).Unix(), false},
		{"expired", now.Add(-time.Minute), now, 0, false},
		{"negative-life", now, now.Add(-time.Minute), 0, false},
		{"long-life", now, now.Add(15*time.Minute + time.Second), 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validTokenTimes(test.issued, test.expiry, test.notBefore, now); got != test.want {
				t.Fatalf("validity = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMiddlewareRequiresExactToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, test := range []struct {
		header string
		want   int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Bearer correct-token", http.StatusNoContent},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", test.header)
		response := httptest.NewRecorder()
		Middleware("correct-token", next).ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("header %q: status = %d, want %d", test.header, response.Code, test.want)
		}
	}
}

func TestProjectedCredentialIsReloadedForEveryRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := SetAuthorization(request, "file:"+path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetAuthorization(request, "file:"+path); err != nil {
		t.Fatal(err)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer second" {
		t.Fatalf("rotated token was not reloaded: %q", got)
	}
}

func TestWorkloadRulesExposeOnlyExactPermissions(t *testing.T) {
	auth := authenticator{verify: func(context.Context, string) (string, error) {
		return "system:serviceaccount:pinvou:billing-service", nil
	}, rules: []rule{{Subject: "system:serviceaccount:pinvou:billing-service", Service: "billing-service", Permissions: []string{"internal.authorization.check"}}}}
	allowed := httptest.NewRequest(http.MethodPost, "/internal/v1/authorization/check", nil)
	allowed.Header.Set("Authorization", "Bearer signed")
	identity, status := auth.authenticate(allowed)
	if status != http.StatusOK || identity.ActorType != "SERVICE" || identity.Service != "billing-service" || len(identity.permissions) != 1 || identity.permissions[0] != "internal.authorization.check" {
		t.Fatalf("unexpected decision: %#v %d", identity, status)
	}
}

func TestRequireRejectsUnassignedInternalPermission(t *testing.T) {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		workload := Identity{ActorType: "SERVICE", Service: "billing-service", Subject: "billing", permissions: []string{"internal.authorization.check"}}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), identityContextKey{}, workload))
	})
	router.POST("/allowed", Require("internal.authorization.check"), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.POST("/denied", Require("internal.authorization.delegate"), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for path, expected := range map[string]int{"/allowed": http.StatusNoContent, "/denied": http.StatusForbidden} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != expected {
			t.Fatalf("%s returned %d, want %d", path, response.Code, expected)
		}
	}
}

func TestStaticServiceTokenRejectedOutsideDevelopment(t *testing.T) {
	t.Setenv("APP_ENV", "staging")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/check", nil)
	request.Header.Set("Authorization", "Bearer legacy")
	if _, status := newAuthenticator("legacy").authenticate(request); status != http.StatusUnauthorized {
		t.Fatalf("expected fail closed, got %d", status)
	}
}

func TestBackgroundIdentityCannotMasqueradeAsHuman(t *testing.T) {
	ctx, err := ContextForJob(context.Background(), "account-event-relay", "OUTBOX_RELAY", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := FromContext(ctx)
	if !ok || identity.ActorType != "SERVICE" || identity.JobRunID != "run-1" || identity.Permission != "internal.job.outbox-relay" {
		t.Fatalf("unexpected background identity: %#v", identity)
	}
}
