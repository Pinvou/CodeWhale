package token

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SecretRotationResult struct {
	ActiveKeyID   string
	Rotated       int
	Remaining     int64
	Scanned       int
	Unconfirmed   int
	SweepComplete bool
}

const secretRotationRemainingSQL = `SELECT
  (SELECT count(*) FROM provider_credentials WHERE key_id<>$1) +
  (SELECT count(*) FROM usage_dispatch_preparations WHERE key_id<>$1) +
  (SELECT count(*) FROM usage_dispatch_intents WHERE encrypted_envelope IS NOT NULL AND key_id IS NOT NULL AND key_id<>$1) +
  (SELECT count(*) FROM usage_stream_events WHERE octet_length(encrypted_payload)>0 AND key_id<>$1) +
  (SELECT count(*) FROM usage_replay_results WHERE octet_length(encrypted_payload)>0 AND key_id<>$1)`

type secretRotationRepository interface {
	RotateSecretKey(context.Context, int) (SecretRotationResult, error)
}

type SecretRotationWorker struct {
	repository secretRotationRepository
	interval   time.Duration
	batchSize  int
}

func NewSecretRotationWorker(repository secretRotationRepository, interval time.Duration, batchSize int) (*SecretRotationWorker, error) {
	if repository == nil || interval < time.Minute || interval > 24*time.Hour || batchSize < 1 || batchSize > 1000 {
		return nil, ErrForbidden
	}
	return &SecretRotationWorker{repository: repository, interval: interval, batchSize: batchSize}, nil
}

func (w *SecretRotationWorker) Run(ctx context.Context) {
	w.rotate(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.rotate(ctx)
		}
	}
}

func (w *SecretRotationWorker) rotate(ctx context.Context) {
	for {
		result, err := w.repository.RotateSecretKey(ctx, w.batchSize)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Error("secret key rotation failed", "error_code", "SECRET_ROTATION_FAILED")
			}
			return
		}
		if result.Unconfirmed > 0 {
			// Decryption failure may be a provider outage, not corrupt data.
			// Preserve the original row and retry it in a later bounded sweep.
			slog.Error("secret key rotation incomplete", "error_code", "SECRET_ROTATION_UNCONFIRMED", "unconfirmed", result.Unconfirmed, "scanned", result.Scanned, "rotated", result.Rotated, "remaining", result.Remaining)
			return
		}
		slog.Info("secret key rotation batch completed", "rotated", result.Rotated, "remaining", result.Remaining)
		if result.SweepComplete || result.Rotated == 0 || result.Remaining == 0 {
			return
		}
	}
}

// Rotation cursors are process-local, constant-sized scan progress, not a
// quarantine or a list of bad records. A completed sweep retries from the start
// on the next original worker tick. Changed active keys start a new sweep.
type secretRotationRuntime struct {
	gate chan struct{}
	scan secretRotationScan
}

var secretRotationInitialization sync.Mutex

type secretRotationPosition struct {
	id       uuid.UUID
	sequence int64
	set      bool
}

type secretRotationScan struct {
	activeKeyID string
	initialized bool
	table       int
	positions   [5]secretRotationPosition
	upper       [5]secretRotationPosition
}

type secretRotationItem struct {
	table         int
	id, accountID uuid.UUID
	sequence      int64
	kind, keyID   string
	ciphertext    []byte
	integrity     string
	replacement   []byte
}

var secretRotationTables = [5]string{"provider_credentials", "usage_dispatch_preparations", "usage_dispatch_intents", "usage_stream_events", "usage_replay_results"}

func (r *PostgresRepository) RotateSecretKey(ctx context.Context, batchSize int) (SecretRotationResult, error) {
	if batchSize < 1 || batchSize > 1000 {
		return SecretRotationResult{}, ErrForbidden
	}
	secretRotationInitialization.Lock()
	if r.secretRotation == nil {
		r.secretRotation = &secretRotationRuntime{gate: make(chan struct{}, 1)}
	}
	rotation := r.secretRotation
	secretRotationInitialization.Unlock()
	select {
	case <-ctx.Done():
		return SecretRotationResult{}, ctx.Err()
	case rotation.gate <- struct{}{}:
	}
	defer func() { <-rotation.gate }()
	if err := ctx.Err(); err != nil {
		return SecretRotationResult{}, err
	}
	active := r.secrets.KeyID()
	scan := rotation.scan
	if !scan.initialized || scan.activeKeyID != active {
		scan = secretRotationScan{activeKeyID: active, initialized: true}
		for table := range secretRotationTables {
			upper, err := r.secretRotationUpper(ctx, table, active)
			if err != nil {
				return SecretRotationResult{}, err
			}
			scan.upper[table] = upper
		}
	}
	// Read bounded snapshots and close all database rows before KMS calls.
	// No SQL transaction or row lock is held across external cryptography.
	items := make([]secretRotationItem, 0, batchSize)
	for scan.table < len(secretRotationTables) && len(items) < batchSize {
		table := scan.table
		if !scan.upper[table].set {
			scan.table++
			continue
		}
		limit := batchSize - len(items)
		selected, err := r.secretRotationItems(ctx, table, active, scan.positions[table], scan.upper[table], limit)
		if err != nil {
			return SecretRotationResult{}, err
		}
		items = append(items, selected...)
		if len(selected) > 0 {
			last := selected[len(selected)-1]
			scan.positions[table] = secretRotationPosition{id: last.id, sequence: last.sequence, set: true}
		}
		if len(selected) < limit {
			scan.table++
		}
	}
	result := SecretRotationResult{ActiveKeyID: active, Scanned: len(items), SweepComplete: scan.table == len(secretRotationTables)}
	for i := range items {
		if err := ctx.Err(); err != nil {
			return SecretRotationResult{}, err
		}
		item := &items[i]
		if item.table != 0 && replayDigest(item.ciphertext) != item.integrity {
			result.Unconfirmed++
			continue
		}
		plaintext, err := r.secrets.DecryptWithKeyID(item.keyID, item.ciphertext, item.associatedData())
		if err != nil {
			clear(plaintext)
			if contextErr := ctx.Err(); contextErr != nil {
				return SecretRotationResult{}, contextErr
			}
			// Never infer corrupt ciphertext from an uncertain KMS response.
			result.Unconfirmed++
			continue
		}
		item.replacement, err = r.secrets.Encrypt(plaintext, item.associatedData())
		clear(plaintext)
		if err != nil {
			return SecretRotationResult{}, err
		}
		if len(item.replacement) == 0 {
			return SecretRotationResult{}, ErrInvalidSecret
		}
		if err := ctx.Err(); err != nil {
			return SecretRotationResult{}, err
		}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SecretRotationResult{}, err
	}
	defer tx.Rollback(ctx)
	for _, item := range items {
		if item.replacement == nil {
			continue
		}
		changed, err := rotateSecretSnapshot(ctx, tx, item, active)
		if err != nil {
			return SecretRotationResult{}, err
		}
		if changed {
			result.Rotated++
		} else {
			result.Unconfirmed++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SecretRotationResult{}, err
	}
	// Only committed progress advances the private cursor. Unknown commit,
	// SQL/encrypt/context failures retain the previous cursor for safe retry.
	if result.SweepComplete {
		rotation.scan = secretRotationScan{}
	} else {
		rotation.scan = scan
	}
	remaining, err := r.SecretRotationRemaining(ctx)
	result.Remaining = remaining
	return result, err
}

func (r *PostgresRepository) SecretRotationRemaining(ctx context.Context) (int64, error) {
	var remaining int64
	err := r.pool.QueryRow(ctx, secretRotationRemainingSQL, r.secrets.KeyID()).Scan(&remaining)
	return remaining, err
}

func (r *PostgresRepository) secretRotationUpper(ctx context.Context, table int, active string) (secretRotationPosition, error) {
	var upper secretRotationPosition
	name := secretRotationTables[table]
	id := "usage_request_id"
	if table == 0 {
		id = "id"
	}
	columns, order := id, id+" DESC"
	predicate := "key_id<>$1"
	if table == 1 || table == 2 {
		predicate += " AND encrypted_envelope IS NOT NULL"
	}
	if table == 3 || table == 4 {
		// F05 retention keeps first-content/key/integrity metadata after erasing
		// the public body. These empty stubs are not ciphertext to re-encrypt.
		predicate += " AND octet_length(encrypted_payload)>0"
	}
	if table == 3 {
		columns += ",sequence"
		order += ",sequence DESC"
	}
	row := r.pool.QueryRow(ctx, "SELECT "+columns+" FROM "+name+" WHERE "+predicate+" ORDER BY "+order+" LIMIT 1", active)
	var err error
	if table == 3 {
		err = row.Scan(&upper.id, &upper.sequence)
	} else {
		err = row.Scan(&upper.id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return upper, nil
	}
	if err != nil {
		return upper, err
	}
	upper.set = true
	return upper, nil
}

func (r *PostgresRepository) secretRotationItems(ctx context.Context, table int, active string, cursor, upper secretRotationPosition, limit int) ([]secretRotationItem, error) {
	var after any
	if cursor.set {
		after = cursor.id
	}
	var query string
	args := []any{active, after, upper.id, limit}
	switch table {
	case 0:
		query = `SELECT id,token_account_id,ciphertext,key_id FROM provider_credentials WHERE key_id<>$1 AND ($2::uuid IS NULL OR id>$2) AND id<=$3 ORDER BY id LIMIT $4`
	case 1, 2:
		query = `SELECT usage_request_id,encrypted_envelope,key_id,COALESCE(envelope_integrity,'') FROM ` + secretRotationTables[table] + ` WHERE encrypted_envelope IS NOT NULL AND key_id<>$1 AND ($2::uuid IS NULL OR usage_request_id>$2) AND usage_request_id<=$3 ORDER BY usage_request_id LIMIT $4`
	case 3:
		query = `SELECT usage_request_id,sequence,event_type,encrypted_payload,key_id,payload_integrity FROM usage_stream_events WHERE octet_length(encrypted_payload)>0 AND key_id<>$1 AND ($2::uuid IS NULL OR (usage_request_id,sequence)>($2,$5)) AND (usage_request_id,sequence)<=($3,$6) ORDER BY usage_request_id,sequence LIMIT $4`
		args = append(args, cursor.sequence, upper.sequence)
	case 4:
		query = `SELECT usage_request_id,response_kind,last_sequence,encrypted_payload,key_id,payload_integrity FROM usage_replay_results WHERE octet_length(encrypted_payload)>0 AND key_id<>$1 AND ($2::uuid IS NULL OR usage_request_id>$2) AND usage_request_id<=$3 ORDER BY usage_request_id LIMIT $4`
	default:
		return nil, ErrForbidden
	}
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]secretRotationItem, 0, limit)
	for rows.Next() {
		item := secretRotationItem{table: table}
		switch table {
		case 0:
			err = rows.Scan(&item.id, &item.accountID, &item.ciphertext, &item.keyID)
		case 1, 2:
			err = rows.Scan(&item.id, &item.ciphertext, &item.keyID, &item.integrity)
		case 3:
			err = rows.Scan(&item.id, &item.sequence, &item.kind, &item.ciphertext, &item.keyID, &item.integrity)
		case 4:
			err = rows.Scan(&item.id, &item.kind, &item.sequence, &item.ciphertext, &item.keyID, &item.integrity)
		}
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (i secretRotationItem) associatedData() []byte {
	switch i.table {
	case 0:
		return []byte(i.accountID.String() + ":" + i.id.String())
	case 1, 2:
		return replayAAD(i.id, "dispatch", 0)
	case 3:
		return replayAAD(i.id, "event:"+i.kind, i.sequence)
	default:
		return replayAAD(i.id, "result:"+i.kind, i.sequence)
	}
}

func rotateSecretSnapshot(ctx context.Context, tx pgx.Tx, item secretRotationItem, active string) (bool, error) {
	if item.table < 0 || item.table >= len(secretRotationTables) {
		return false, ErrForbidden
	}
	// Preserve the original skip-locked behavior for in-flight writers. KMS
	// has already finished; only the short write transaction acquires this lock.
	idColumn := "usage_request_id"
	if item.table == 0 {
		idColumn = "id"
	}
	lockQuery := "SELECT " + idColumn + " FROM " + secretRotationTables[item.table] + " WHERE " + idColumn + "=$1"
	lockArgs := []any{item.id}
	if item.table == 3 {
		lockQuery += " AND sequence=$2"
		lockArgs = append(lockArgs, item.sequence)
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, lockQuery+" FOR UPDATE SKIP LOCKED", lockArgs...).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var query string
	var args []any
	switch item.table {
	case 0:
		query = `UPDATE provider_credentials SET ciphertext=$2,key_id=$3 WHERE id=$1 AND key_id=$4 AND ciphertext=$5 AND token_account_id=$6`
		args = []any{item.id, item.replacement, active, item.keyID, item.ciphertext, item.accountID}
	case 1, 2:
		query = `UPDATE ` + secretRotationTables[item.table] + ` SET encrypted_envelope=$2,key_id=$3,envelope_integrity=$4 WHERE usage_request_id=$1 AND key_id=$5 AND encrypted_envelope=$6 AND envelope_integrity=$7`
		args = []any{item.id, item.replacement, active, replayDigest(item.replacement), item.keyID, item.ciphertext, item.integrity}
	case 3:
		query = `UPDATE usage_stream_events SET encrypted_payload=$3,key_id=$4,payload_integrity=$5 WHERE usage_request_id=$1 AND sequence=$2 AND key_id=$6 AND encrypted_payload=$7 AND payload_integrity=$8 AND event_type=$9`
		args = []any{item.id, item.sequence, item.replacement, active, replayDigest(item.replacement), item.keyID, item.ciphertext, item.integrity, item.kind}
	case 4:
		query = `UPDATE usage_replay_results SET encrypted_payload=$2,key_id=$3,payload_integrity=$4 WHERE usage_request_id=$1 AND key_id=$5 AND encrypted_payload=$6 AND payload_integrity=$7 AND response_kind=$8 AND last_sequence=$9`
		args = []any{item.id, item.replacement, active, replayDigest(item.replacement), item.keyID, item.ciphertext, item.integrity, item.kind, item.sequence}
	default:
		return false, ErrForbidden
	}
	command, err := tx.Exec(ctx, query, args...)
	return command.RowsAffected() == 1, err
}
