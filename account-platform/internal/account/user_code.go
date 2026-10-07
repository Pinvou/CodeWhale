package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	userCodePrefix        = "USR"
	userCodePayloadLength = 16
	userCodeStorageLength = 21
	userCodeAlphabet      = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var ErrInvalidUserCode = errors.New("invalid user code")

func GenerateUserCode() (string, error) {
	return generateUserCode(rand.Reader)
}

func generateUserCode(source io.Reader) (string, error) {
	random := make([]byte, userCodePayloadLength)
	if _, err := io.ReadFull(source, random); err != nil {
		return "", err
	}
	payload := make([]byte, userCodePayloadLength)
	for index, value := range random {
		payload[index] = userCodeAlphabet[int(value)&31]
	}
	base := userCodePrefix + string(payload)
	checksum := sha256.Sum256([]byte(base))
	firstTenBits := uint16(checksum[0])<<2 | uint16(checksum[1])>>6
	return base + string([]byte{
		userCodeAlphabet[(firstTenBits>>5)&31],
		userCodeAlphabet[firstTenBits&31],
	}), nil
}

func NormalizeUserCode(value string) (string, error) {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
	if len(normalized) != userCodeStorageLength || !strings.HasPrefix(normalized, userCodePrefix) {
		return "", ErrInvalidUserCode
	}
	for _, value := range normalized[len(userCodePrefix):] {
		if !strings.ContainsRune(userCodeAlphabet, value) {
			return "", ErrInvalidUserCode
		}
	}
	base := normalized[:userCodeStorageLength-2]
	checksum := sha256.Sum256([]byte(base))
	firstTenBits := uint16(checksum[0])<<2 | uint16(checksum[1])>>6
	want := string([]byte{userCodeAlphabet[(firstTenBits>>5)&31], userCodeAlphabet[firstTenBits&31]})
	if normalized[userCodeStorageLength-2:] != want {
		return "", ErrInvalidUserCode
	}
	return normalized, nil
}

func FormatUserCode(value string) string {
	normalized, err := NormalizeUserCode(value)
	if err != nil {
		return ""
	}
	return strings.Join([]string{
		normalized[:3], normalized[3:7], normalized[7:11], normalized[11:15], normalized[15:19], normalized[19:],
	}, "-")
}

func insertUserWithGeneratedCode(ctx context.Context, tx pgx.Tx, insert func(pgx.Tx, string) error) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		candidate, err := GenerateUserCode()
		if err != nil {
			return "", err
		}
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return "", err
		}
		err = insert(savepoint, candidate)
		if err == nil {
			err = savepoint.Commit(ctx)
		}
		if err == nil {
			return candidate, nil
		}
		_ = savepoint.Rollback(ctx)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || (databaseError.ConstraintName != "users_user_code_unique" && databaseError.ConstraintName != "user_code_registry_pkey") {
			return "", err
		}
	}
	return "", errors.New("generate a unique user code after 8 attempts")
}
