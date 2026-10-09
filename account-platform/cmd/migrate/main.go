package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/simulatorschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/jackc/pgx/v5"
)

var evidenceHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

type migrationEvidence struct {
	Event             string `json:"event"`
	Domain            string `json:"domain"`
	BackupEvidenceID  string `json:"backup_evidence_id,omitempty"`
	BackupCompletedAt string `json:"backup_completed_at,omitempty"`
	BackupSHA256      string `json:"backup_sha256,omitempty"`
	migrations.Verification
}

func productionBackupEvidence(now time.Time) (string, string, string, error) {
	if os.Getenv("PINVOU_DEPLOYMENT_ENVIRONMENT") != "production-new" {
		return "", "", "", nil
	}
	id := strings.TrimSpace(os.Getenv("MIGRATION_BACKUP_EVIDENCE_ID"))
	completedAt := strings.TrimSpace(os.Getenv("MIGRATION_BACKUP_COMPLETED_AT"))
	hash := strings.TrimSpace(os.Getenv("MIGRATION_BACKUP_SHA256"))
	if len(id) < 8 || len(id) > 128 || strings.ContainsAny(id, "\r\n\t ") {
		return "", "", "", fmt.Errorf("production migration backup evidence ID is missing or invalid")
	}
	completed, err := time.Parse(time.RFC3339, completedAt)
	if err != nil || completed.Before(now.Add(-24*time.Hour)) || completed.After(now.Add(5*time.Minute)) {
		return "", "", "", fmt.Errorf("production migration backup must be completed within the previous 24 hours")
	}
	if !evidenceHash.MatchString(hash) {
		return "", "", "", fmt.Errorf("production migration backup checksum is missing or invalid")
	}
	return id, completed.UTC().Format(time.RFC3339), hash, nil
}

func main() {
	directory := flag.String("dir", os.Getenv("MIGRATIONS_DIR"), "directory containing SQL migrations")
	direction := flag.String("direction", "up", "migration direction: up or down")
	flag.Parse()
	if *directory == "" {
		log.Fatal("migration directory is required through --dir or MIGRATIONS_DIR")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	backupID, backupCompletedAt, backupSHA256, err := productionBackupEvidence(time.Now().UTC())
	if err != nil {
		log.Fatal("MIGRATION_BACKUP_EVIDENCE_INVALID")
	}

	loaded, err := migrations.Load(*directory)
	if err != nil {
		log.Fatal("MIGRATION_SOURCE_INVALID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatal("MIGRATION_DATABASE_CONNECTION_FAILED")
	}
	defer connection.Close(context.Background())
	defer migrations.Unlock(context.Background(), connection)
	if migrationRole := strings.TrimSpace(os.Getenv("MIGRATION_ROLE")); migrationRole != "" {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(migrationRole) {
			log.Fatal("MIGRATION_ROLE is invalid")
		}
		if _, err = connection.Exec(ctx, "SET ROLE "+pgx.Identifier{migrationRole}.Sanitize()); err != nil {
			log.Fatal("MIGRATION_ROLE_SELECTION_FAILED")
		}
	}

	domain, err := migrationDomain(ctx, connection, loaded, *directory)
	if err != nil {
		log.Fatal("MIGRATION_DOMAIN_IDENTITY_INVALID")
	}

	switch *direction {
	case "up":
		if domain == "account" {
			err = accountschema.Up(ctx, connection, loaded)
		} else if domain == "billing" {
			err = billingschema.Up(ctx, connection, loaded)
		} else if domain == "token" {
			err = tokenschema.Up(ctx, connection, loaded)
		} else if domain == "payment-simulator" {
			err = simulatorschema.Up(ctx, connection, loaded)
		} else {
			err = migrations.Up(ctx, connection, loaded)
		}
		if err == nil {
			var verification migrations.Verification
			verification, err = migrations.VerifyApplied(ctx, connection, loaded)
			if err == nil {
				evidence := migrationEvidence{
					Event: "migration.post_verification", Domain: domain,
					BackupEvidenceID: backupID, BackupCompletedAt: backupCompletedAt, BackupSHA256: backupSHA256,
					Verification: verification,
				}
				err = json.NewEncoder(os.Stdout).Encode(evidence)
			}
		}
	case "down":
		if domain == "account" {
			loaded, err = accountschema.Ordered(loaded)
		} else if domain == "billing" {
			loaded, err = billingschema.Ordered(loaded)
		} else if domain == "token" {
			loaded, err = tokenschema.Ordered(loaded)
		} else if domain == "payment-simulator" {
			loaded, err = simulatorschema.Ordered(loaded)
		}
		if err != nil {
			break
		}
		err = migrations.DownOne(ctx, connection, loaded)
	default:
		err = fmt.Errorf("unsupported migration direction %q", *direction)
	}
	if err != nil {
		log.Fatal("MIGRATION_EXECUTION_FAILED")
	}
}

// Protected identities follow both their SQL package and installed ledger,
// even when a deployment mounts them under a different directory name. The
// same migration lock remains held through Up/Down; an unrelated custom
// migration history still uses the original generic runner.
func migrationDomain(ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration, directory string) (string, error) {
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
		return "", err
	}
	domain := ""
	observe := func(version int64, name, checksum string) error {
		candidate := protectedMigrationDomain(version, name, checksum)
		if candidate == "" {
			return nil
		}
		if domain != "" && domain != candidate {
			return fmt.Errorf("mixed protected migration domains")
		}
		domain = candidate
		return nil
	}
	for _, migration := range loaded {
		if err := observe(migration.Version, migration.Name, migration.Checksum); err != nil {
			return "", err
		}
	}
	var exists bool
	if err := connection.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		rows, err := connection.Query(ctx, `SELECT version,name,checksum FROM schema_migrations`)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		for rows.Next() {
			var version int64
			var name, checksum string
			if err := rows.Scan(&version, &name, &checksum); err != nil {
				return "", err
			}
			if err := observe(version, name, checksum); err != nil {
				return "", err
			}
		}
		if err := rows.Err(); err != nil {
			return "", err
		}
	}
	base := filepathBase(directory)
	if base == "account" || base == "billing" || base == "payment-simulator" || base == "token" {
		if domain != "" && domain != base {
			return "", fmt.Errorf("migration directory disagrees with protected identity")
		}
		return base, nil
	}
	if domain != "" {
		return domain, nil
	}
	return base, nil
}

func protectedMigrationDomain(version int64, name, checksum string) string {
	for _, identity := range []struct {
		domain         string
		version        int64
		name, checksum string
	}{
		{"token", 31, "token_command_governance_activation", "d957f8cac9de3566b1e30ab286458f80e78acd2cc9d61917e3367a0c8280ee16"},
		{"account", 900001, "first_delivery_permission_metadata", "b14cb9c2e538d20ec869458940d60867090549c027f439ed199d703ef3e7f45d"},
		{"account", 900002, "approval_execution_frozen_facts", "019fd548cf79e833fd53ef60591a4f714a40268dceb8d1237c53e69b0162215a"},
		{"account", 87, "registration_bonus_fingerprint", "1d76a6e9aa6b5fcee84e2eabe8912cbfb8fa53c79ba29ce2bb4c29852be070e1"},
		{"account", 900004, "registration_bonus_required_facts", "ddfdb70370c3900c113efc918fb5586ab1a2cb977337b8df35e565e1a250772f"},
		{"billing", 900001, "outbox_publication_generation", "7e36e103ad25620dd7a882792fcd0d942f6208038aeaa966eb44b367cea27f05"},
		{"billing", 900002, "payment_anomaly_resolution_intents", "68df09f526088b981c992d0c9912cab8f4ac2c6f5567fdb6351cfdc2b905d2a2"},
		{"billing", 79, "registration_bonus", "27802b2d3e76e6a42c809d620d3d9833cfaa529d7d5517982318236a56719deb"},
		{"billing", 900011, "member_lifecycle_event_identity", "af4a63ed0592a0e60d5f4744176e15cc5013c3633c6f0f414f7580b7132b2aa8"},
		{"payment-simulator", 5, "mapped_order_facts", "c10369f3c1201aa37752d686645fac25c435b6e68daba401158f54090ae4e0bb"},
		{"payment-simulator", 6, "merchant_customer_key", "a5629107cf1ba62324fb0fb9ea548037a291a327274ef35cdeafa69f4dc1e9dc"},
	} {
		if version == identity.version && (name == identity.name || checksum == identity.checksum) {
			return identity.domain
		}
	}
	return ""
}

func filepathBase(value string) string {
	value = strings.TrimRight(strings.ReplaceAll(value, "\\", "/"), "/")
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		return value[index+1:]
	}
	return value
}
