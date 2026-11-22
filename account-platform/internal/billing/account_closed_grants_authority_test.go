package billing

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClosedGrantAuthorityRequiresWholeExactAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		ok         bool
	}{
		{"anonymized", `{"stage":"ANONYMIZED","subject_lifecycle_version":2}`, 200, true},
		{"completed", `{"stage":"COMPLETED","subject_lifecycle_version":2,"extension":true}`, 200, true},
		{"wrong-stage", `{"stage":"IDENTITY_CLEANUP","subject_lifecycle_version":2}`, 200, false},
		{"wrong-version", `{"stage":"ANONYMIZED","subject_lifecycle_version":3}`, 200, false},
		{"trailing-object", `{"stage":"ANONYMIZED","subject_lifecycle_version":2}{}`, 200, false},
		{"trailing-garbage", `{"stage":"ANONYMIZED","subject_lifecycle_version":2}junk`, 200, false},
		{"oversize", `{"stage":"ANONYMIZED","subject_lifecycle_version":2}` + strings.Repeat(" ", 64<<10), 200, false},
		{"unavailable", "{}", 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := uuid.New()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/users/"+user.String()+"/account-closure-state" || r.Header.Get("Authorization") != "Bearer controlled-authority" {
					t.Error("authority tuple lost")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			v, e := NewHTTPAccountClosureStateVerifier(server.URL, "controlled-authority")
			if e != nil {
				t.Fatal(e)
			}
			if e = v.RequireAnonymized(context.Background(), user, 2); (e == nil) != tc.ok {
				t.Fatalf("accepted=%v want=%v error=%v", e == nil, tc.ok, e)
			}
		})
	}
}

func TestClosedGrantAuthorityReloadsProjectedCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if e := os.WriteFile(path, []byte("first"), 0600); e != nil {
		t.Fatal(e)
	}
	seen := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"stage":"ANONYMIZED","subject_lifecycle_version":2}`)
	}))
	defer server.Close()
	v, e := NewHTTPAccountClosureStateVerifier(server.URL, "file:"+path)
	if e != nil {
		t.Fatal(e)
	}
	user := uuid.New()
	if e = v.RequireAnonymized(context.Background(), user, 2); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte("second"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = v.RequireAnonymized(context.Background(), user, 2); e != nil {
		t.Fatal(e)
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Fatal("projected credentials were cached")
	}
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if e = v.RequireAnonymized(context.Background(), user, 2); e == nil || len(seen) != 2 {
		t.Fatal("missing projected credential reached authority")
	}
}
