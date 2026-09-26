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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenmigrations"
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
		log.Fatal(err)
	}

	loaded, err := migrations.Load(*directory)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer connection.Close(context.Background())
	defer migrations.Unlock(context.Background(), connection)
	if migrationRole := strings.TrimSpace(os.Getenv("MIGRATION_ROLE")); migrationRole != "" {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(migrationRole) {
			log.Fatal("MIGRATION_ROLE is invalid")
		}
		if _, err = connection.Exec(ctx, "SET ROLE "+pgx.Identifier{migrationRole}.Sanitize()); err != nil {
			log.Fatal(err)
		}
	}

	switch *direction {
	case "up":
		if filepathBase(*directory) == "token" {
			err = tokenmigrations.Up(ctx, connection, loaded)
		} else {
			err = migrations.Up(ctx, connection, loaded)
		}
		if err == nil {
			var verification migrations.Verification
			verification, err = migrations.VerifyApplied(ctx, connection, loaded)
			if err == nil {
				evidence := migrationEvidence{
					Event: "migration.post_verification", Domain: filepathBase(*directory),
					BackupEvidenceID: backupID, BackupCompletedAt: backupCompletedAt, BackupSHA256: backupSHA256,
					Verification: verification,
				}
				err = json.NewEncoder(os.Stdout).Encode(evidence)
			}
		}
	case "down":
		err = migrations.DownOne(ctx, connection, loaded)
	default:
		err = fmt.Errorf("unsupported migration direction %q", *direction)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func filepathBase(value string) string {
	value = strings.TrimRight(strings.ReplaceAll(value, "\\", "/"), "/")
	if index := strings.LastIndexByte(value, '/'); index >= 0 {
		return value[index+1:]
	}
	return value
}
