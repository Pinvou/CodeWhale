// Package custody is purely offline. It never contacts a server or displays
// secrets. Callers must obtain passphrases through a real local terminal.
package custody

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

const MaxMaterial = 1 << 20

var ErrRecovery = errors.New("RECOVERY_OPERATION_FAILED")

func Encrypt(data []byte, recipient age.Recipient) ([]byte, error) {
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipient)
	if err != nil {
		return nil, ErrRecovery
	}
	if _, err = w.Write(data); err != nil {
		return nil, ErrRecovery
	}
	if w.Close() != nil {
		return nil, ErrRecovery
	}
	return out.Bytes(), nil
}

func Decrypt(data []byte, identity age.Identity) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(data), identity)
	if err != nil {
		return nil, ErrRecovery
	}
	out, err := io.ReadAll(io.LimitReader(r, MaxMaterial+1))
	if err != nil || len(out) == 0 || len(out) > MaxMaterial {
		clear(out)
		return nil, ErrRecovery
	}
	return out, nil
}

// ReadBounded preserves the offline command's existing file semantics.
// Online callers additionally require canonical absolute non-symlink paths.
func ReadBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrRecovery
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2*MaxMaterial {
		return nil, ErrRecovery
	}
	b, err := io.ReadAll(io.LimitReader(f, 2*MaxMaterial+1))
	if err != nil || len(b) > 2*MaxMaterial {
		return nil, ErrRecovery
	}
	return b, nil
}

func Unlock(dir string, passphrase []byte) (*age.X25519Identity, error) {
	cipher, err := ReadBounded(filepath.Join(dir, "identity.age"))
	if err != nil {
		return nil, err
	}
	password, err := age.NewScryptIdentity(string(passphrase))
	if err != nil {
		return nil, ErrRecovery
	}
	password.SetMaxWorkFactor(18)
	plain, err := Decrypt(cipher, password)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	id, err := age.ParseX25519Identity(string(plain))
	if err != nil {
		return nil, ErrRecovery
	}
	public, err := ReadBounded(filepath.Join(dir, "recipient.txt"))
	if err != nil || strings.TrimSpace(string(public)) != id.Recipient().String() {
		return nil, ErrRecovery
	}
	return id, nil
}
