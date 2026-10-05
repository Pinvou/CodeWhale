package account

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostgresWalletWorkerRequiresCompleteBoundAuthorityResponse(t *testing.T) {
	for _, scenario := range []string{"valid", "html", "empty", "null", "truncated", "tail", "oversized", "wrong_owner", "organization", "missing_id", "disabled", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx := context.Background()
			userID := provisioningUser(t, repo)
			var followed atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1); w.WriteHeader(200) }))
			defer target.Close()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wallet := map[string]any{"wallet_id": uuid.New(), "owner_type": "PERSONAL", "owner_id": userID, "status": "ACTIVE"}
				switch scenario {
				case "html":
					io.WriteString(w, "<html>unavailable</html>")
					return
				case "empty":
					return
				case "null":
					io.WriteString(w, "null")
					return
				case "truncated":
					io.WriteString(w, "{")
					return
				case "wrong_owner":
					wallet["owner_id"] = uuid.New()
				case "organization":
					wallet["owner_type"] = "ORG"
				case "missing_id":
					delete(wallet, "wallet_id")
				case "disabled":
					wallet["status"] = "DISABLED"
				case "redirect":
					http.Redirect(w, r, target.URL, 307)
					return
				}
				data, _ := json.Marshal(wallet)
				w.Write(data)
				if scenario == "tail" {
					io.WriteString(w, "{}")
				} else if scenario == "oversized" {
					io.WriteString(w, strings.Repeat(" ", 64<<10))
				}
			}))
			defer provider.Close()
			client, err := NewHTTPWalletProvisioner(provider.URL, strings.Repeat("s", 32))
			if err != nil {
				t.Fatal(err)
			}
			if err := NewWalletProvisioningWorker(repo, client, time.Second).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			var userStatus, jobStatus string
			if err := repo.pool.QueryRow(ctx, `SELECT u.status,j.status FROM users u JOIN wallet_provisioning_jobs j ON j.user_id=u.id WHERE u.id=$1`, userID).Scan(&userStatus, &jobStatus); err != nil {
				t.Fatal(err)
			}
			if scenario == "valid" {
				if userStatus != "ACTIVE" || jobStatus != "COMPLETED" {
					t.Fatal("valid wallet authority did not activate")
				}
			} else if userStatus != "PROVISIONING" || jobStatus != "RETRY" {
				t.Fatal("unproven wallet authority activated the user")
			}
			if followed.Load() != 0 {
				t.Fatal("wallet provisioning followed a redirect")
			}
		})
	}
}
