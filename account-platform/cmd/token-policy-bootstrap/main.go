package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	mode := flag.String("mode", "bootstrap", "bootstrap, readiness, or quiescence")
	capValue := flag.Int("max-per-owner", envInt("TOKEN_POLICY_MAX_PER_OWNER"), "explicit production Token Account cap")
	expected := flag.Int64("expected-version", envInt64("TOKEN_POLICY_EXPECTED_VERSION"), "expected active policy version; zero means no active policy")
	key := flag.String("idempotency-key", strings.TrimSpace(os.Getenv("TOKEN_POLICY_IDEMPOTENCY_KEY")), "16-128 byte deployment idempotency key")
	reason := flag.String("reason", strings.TrimSpace(os.Getenv("TOKEN_POLICY_REASON")), "audited policy change reason")
	flag.Parse()
	databaseURL := strings.TrimSpace(os.Getenv("TOKEN_POLICY_DATABASE_URL"))
	if databaseURL == "" {
		log.Fatal("TOKEN_POLICY_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	switch *mode {
	case "bootstrap":
		if *capValue < 1 || *capValue > 1000 || len(*key) < 16 || len(*key) > 128 || strings.TrimSpace(*reason) != *reason || *reason == "" {
			log.Fatal("explicit cap, idempotency key, and normalized reason are required")
		}
		var expectedValue any
		if *expected > 0 {
			expectedValue = *expected
		}
		var version int64
		var digest string
		var event uuid.UUID
		err = pool.QueryRow(ctx, `SELECT policy_version,config_digest,outbox_event_id FROM public.apply_token_account_owner_cap_policy($1,$2,$3,$4,$5)`, *capValue, expectedValue, *key, *reason, uuid.Must(uuid.NewV7())).Scan(&version, &digest, &event)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("policy_version=%d config_digest=%s outbox_event_id=%s\n", version, digest, event)
	case "readiness":
		var ok bool
		err = pool.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(r.config_digest=encode(digest(convert_to('{"max_per_owner":'||r.max_per_owner::text||',"policy_version":'||r.policy_version::text||'}','UTF8'),'sha256'),'hex')) FROM token_account_owner_cap_policy_active p JOIN token_account_owner_cap_policy_revisions r ON r.policy_version=p.active_policy_version WHERE p.singleton_key`).Scan(&ok)
		if err != nil || !ok {
			log.Fatal("TOKEN_ACCOUNT_POLICY_UNAVAILABLE")
		}
		fmt.Println("token_account_create_policy=ready")
	case "quiescence":
		if err = checkWriterQuiescence(ctx, pool); err != nil {
			log.Fatal(err)
		}
		fmt.Println("token_account_create_writers=quiesced database_write_fence=verified")
	default:
		log.Fatal("unsupported mode")
	}
}

func envInt(name string) int {
	value, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	return value
}

func envInt64(name string) int64 {
	value, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	return value
}
