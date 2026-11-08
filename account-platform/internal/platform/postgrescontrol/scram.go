package postgrescontrol

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

var errSCRAM = errors.New("PostgreSQL SCRAM authentication was not verified")

// SCRAMVerifier constructs the PostgreSQL verifier locally so a managed server's
// legacy password_encryption setting cannot silently install an MD5 credential.
// ASCII generated credentials avoid a separate SASLprep normalization contract.
func SCRAMVerifier(password string) (string, error) {
	if len(password) < 32 || len(password) > 128 {
		return "", errSCRAM
	}
	for _, c := range password {
		if c < 33 || c > 126 {
			return "", errSCRAM
		}
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", errSCRAM
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, 4096, 32)
	if err != nil {
		return "", errSCRAM
	}
	defer clear(salted)
	mac := func(key []byte, text string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(text))
		return h.Sum(nil)
	}
	client := mac(salted, "Client Key")
	defer clear(client)
	stored := sha256.Sum256(client)
	server := mac(salted, "Server Key")
	defer clear(server)
	encode := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$4096:" + encode(salt) + "$" + encode(stored[:]) + ":" + encode(server), nil
}

// ProbeSCRAM requires verified TLS and the complete SASL exchange, then verifies
// the authenticated role and database in a forced read-only connection. A trust,
// cleartext or MD5 authentication request is refused before pgx can answer it.
func ProbeSCRAM(ctx context.Context, configuration *pgx.ConnConfig, user, password string) error {
	config := configuration.Copy()
	if len(config.Fallbacks) != 0 || config.TLSConfig == nil || config.TLSConfig.InsecureSkipVerify || config.TLSConfig.ServerName != config.Host {
		return errSCRAM
	}
	config.User, config.Password = user, password
	config.Fallbacks = nil
	config.RuntimeParams = map[string]string{"default_transaction_read_only": "on", "statement_timeout": "5000", "application_name": "pinvou-release-credential-probe"}
	var authentication *scramReader
	config.BuildFrontend = func(reader io.Reader, writer io.Writer) *pgproto3.Frontend {
		authentication = &scramReader{reader: reader}
		return pgproto3.NewFrontend(authentication, writer)
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return errSCRAM
	}
	defer connection.Close(context.Background())
	var session, current, database, readonly string
	if authentication == nil || authentication.stage != 4 || connection.QueryRow(ctx,
		`SELECT session_user,current_user,current_database(),current_setting('transaction_read_only')`).Scan(&session, &current, &database, &readonly) != nil ||
		session != user || current != user || database != config.Database || readonly != "on" {
		return errSCRAM
	}
	return nil
}

// scramReader preserves wire bytes. It examines only backend authentication
// method codes; it never parses, copies to logs or exposes credential material.
type scramReader struct {
	reader io.Reader
	buffer []byte
	stage  int
}

func (r *scramReader) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	if len(r.buffer) == 0 {
		if r.stage == 4 {
			return r.reader.Read(output)
		}
		header := make([]byte, 5)
		if _, err := io.ReadFull(r.reader, header); err != nil {
			return 0, err
		}
		length := int(binary.BigEndian.Uint32(header[1:]))
		if length < 4 || length > 1<<20 {
			return 0, errSCRAM
		}
		frame := make([]byte, length+1)
		copy(frame, header)
		if _, err := io.ReadFull(r.reader, frame[5:]); err != nil {
			return 0, err
		}
		if frame[0] == 'R' {
			if length < 8 {
				return 0, errSCRAM
			}
			method := binary.BigEndian.Uint32(frame[5:9])
			expected := []uint32{10, 11, 12, 0}
			if r.stage >= len(expected) || method != expected[r.stage] {
				return 0, errSCRAM
			}
			r.stage++
		}
		r.buffer = frame
	}
	n := copy(output, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}
