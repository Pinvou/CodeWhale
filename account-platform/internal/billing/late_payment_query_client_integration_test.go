package billing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestLatePaymentQueryClientHTTPPostgres(t *testing.T) {
	t.Run("actual_client_original_bytes_runtime_role_and_handler", func(t *testing.T) {
		r, ctx, s, provider, item, actor, input := lateQueryFixture(t)
		key := "query-" + uuid.NewString()
		allowed := true
		handler := lateQueryAuthorizedHandler(t, s, actor, &allowed)
		var mutex sync.Mutex
		var bodies, paths []string
		// This local adapter only strips the Portal prefix and supplies the
		// controlled bearer. Actual Portal bytes/session/CSRF have a separate
		// contract; no real IdP or external payment channel is claimed here.
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			raw, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			mutex.Lock()
			bodies = append(bodies, string(raw))
			paths = append(paths, request.URL.Path)
			mutex.Unlock()
			if request.Header.Get("Idempotency-Key") != key {
				t.Error("client changed its original key")
			}
			request.Body = io.NopCloser(bytes.NewReader(raw))
			request.URL.Path = strings.TrimPrefix(request.URL.Path, "/api/admin/billing")
			request.Header.Set("Authorization", "Bearer PUBLIC_SYNTHETIC_QUERY")
			handler.ServeHTTP(w, request)
		}))
		defer receiver.Close()
		configuration := map[string]any{"url": receiver.URL, "item": item, "actor_id": actor, "key": key}
		raw, _ := json.Marshal(configuration)
		path := filepath.Join(t.TempDir(), "query-client.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		node := os.Getenv("PINVOU_QUERY_NODE")
		if node == "" {
			node = "node"
		}
		helper, err := filepath.Abs("../../../pinvou3-app/tests/helpers/late-payment-query-http-client.mjs")
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(ctx, node, helper, path)
		for _, value := range os.Environ() {
			name := strings.SplitN(value, "=", 2)[0]
			if !strings.Contains(name, "DATABASE_URL") && !strings.HasPrefix(name, "PG") {
				command.Env = append(command.Env, value)
			}
		}
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Fatal("actual Node client HTTP contract", err, stderr.String())
		}
		var result struct {
			CaseID uuid.UUID `json:"case_id"`
		}
		if json.Unmarshal(output, &result) != nil || result.CaseID != item.ID {
			t.Fatal("missing original result", string(output))
		}
		expected, _ := json.Marshal(input)
		mutex.Lock()
		defer mutex.Unlock()
		if len(bodies) != 2 || bodies[0] != string(expected) || bodies[1] != bodies[0] || !strings.HasSuffix(paths[0], "/query-commands") || !strings.HasSuffix(paths[1], "/query-command-results") {
			t.Fatal("original client bytes/routes differ", bodies, paths)
		}
		var role string
		if err := r.pool.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "billing_service" {
			t.Fatal("financial action did not use reviewed runtime role", role, err)
		}
		if provider.gets.Load() != 1 || provider.closes.Load() != 0 || lateQueryCount(t, r, ctx, `SELECT count(*) FROM late_payment_query_commands WHERE completed_at IS NOT NULL`) != 1 {
			t.Fatal("HTTP recovery dispatched again or omitted atomic receipt")
		}
	})
}
