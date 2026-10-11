// kms-recovery is an offline custody tool. It never contacts a server or emits
// decrypted material. Only a real local terminal may supply a passphrase.
package main

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"filippo.io/age"
	"github.com/Pinvou/pinvou3/account-platform/tools/kms-recovery/internal/custody"
	"golang.org/x/term"
)

const maxMaterial = custody.MaxMaterial

var errRecovery = custody.ErrRecovery

func encrypt(data []byte, recipient age.Recipient) ([]byte, error) {
	return custody.Encrypt(data, recipient)
}

func decrypt(data []byte, identity age.Identity) ([]byte, error) {
	return custody.Decrypt(data, identity)
}

func wrapIdentity(passphrase []byte) ([]byte, string, error) {
	if !utf8.Valid(passphrase) || utf8.RuneCountInString(strings.TrimSpace(string(passphrase))) < 16 || len(passphrase) > 1024 {
		return nil, "", errors.New("PASSPHRASE_REQUIRES_16_TO_1024_CHARACTERS")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, "", errRecovery
	}
	r, err := age.NewScryptRecipient(string(passphrase))
	if err != nil {
		return nil, "", errRecovery
	}
	plain := []byte(id.String())
	defer clear(plain)
	cipher, err := encrypt(plain, r)
	if err != nil {
		return nil, "", err
	}
	return cipher, id.Recipient().String(), nil
}

func readBounded(path string) ([]byte, error) {
	return custody.ReadBounded(path)
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("OUTPUT_EXISTS_OR_UNAVAILABLE")
	}
	success := false
	defer func() {
		_ = f.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return errRecovery
	}
	if err = f.Sync(); err != nil {
		return errRecovery
	}
	if err = f.Close(); err != nil {
		return errRecovery
	}
	success = true
	return nil
}

func unlock(dir string, passphrase []byte) (*age.X25519Identity, error) {
	return custody.Unlock(dir, passphrase)
}

func prepare(dir string, passphrase []byte) error {
	if !filepath.IsAbs(dir) {
		return errors.New("ABSOLUTE_DIRECTORY_REQUIRED")
	}
	// A new custody directory is mandatory: never overwrite a previous identity.
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return errors.New("NEW_DIRECTORY_REQUIRED")
	}
	cipher, recipient, err := wrapIdentity(passphrase)
	if err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return errRecovery
	}
	if err = writeNew(filepath.Join(dir, "identity.age"), cipher); err != nil {
		return err
	}
	if err = writeNew(filepath.Join(dir, "recipient.txt"), []byte(recipient+"\n")); err != nil {
		return err
	}
	identity, err := unlock(dir, passphrase)
	if err != nil {
		return err
	}
	challenge := []byte("pinvou-offline-recovery-custody-v1")
	sealed, err := encrypt(challenge, identity.Recipient())
	if err != nil {
		return err
	}
	opened, err := decrypt(sealed, identity)
	defer clear(opened)
	if err != nil || !bytes.Equal(opened, challenge) {
		return errRecovery
	}
	return writeNew(filepath.Join(dir, "CUSTODY_READY"), []byte("Identity encrypted and read-back verified. No KMS initialized.\n"))
}

func readPassphrase(confirm bool) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("LOCAL_INTERACTIVE_TERMINAL_REQUIRED")
	}
	fmt.Fprint(os.Stderr, "设置/输入恢复口令（至少16个字符；输入不回显）： ")
	p, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, errRecovery
	}
	if confirm {
		fmt.Fprint(os.Stderr, "再次输入以确认： ")
		second, e := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		match := e == nil && subtle.ConstantTimeCompare(p, second) == 1
		clear(second)
		if !match {
			clear(p)
			return nil, errors.New("PASSPHRASE_CONFIRMATION_FAILED")
		}
	}
	return p, nil
}

func run(action, dir, bundle string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("ABSOLUTE_DIRECTORY_REQUIRED")
	}
	switch action {
	case "prepare":
		p, err := readPassphrase(true)
		if err != nil {
			return err
		}
		defer clear(p)
		if err = prepare(dir, p); err != nil {
			return err
		}
		fmt.Println("CUSTODY_READY：加密身份已建立并验证。请离线备份整个目录，口令分开保存。尚未初始化KMS。")
	case "verify":
		p, err := readPassphrase(false)
		if err != nil {
			return err
		}
		defer clear(p)
		identity, err := unlock(dir, p)
		if err != nil {
			return err
		}
		if bundle != "" {
			b, err := readBounded(bundle)
			if err != nil {
				return err
			}
			plain, err := decrypt(b, identity)
			defer clear(plain)
			if err != nil {
				return err
			}
		}
		fmt.Println("RECOVERY_VERIFIED：解密及完整性验证通过，未输出任何明文。")
	case "seal":
		if !filepath.IsAbs(bundle) {
			return errors.New("ABSOLUTE_OUTPUT_REQUIRED")
		}
		if _, err := readBounded(filepath.Join(dir, "CUSTODY_READY")); err != nil {
			return errors.New("CUSTODY_NOT_PREPARED")
		}
		pub, err := readBounded(filepath.Join(dir, "recipient.txt"))
		if err != nil {
			return err
		}
		r, err := age.ParseX25519Recipient(strings.TrimSpace(string(pub)))
		if err != nil {
			return errRecovery
		}
		plain, err := io.ReadAll(io.LimitReader(os.Stdin, maxMaterial+1))
		defer clear(plain)
		if err != nil || len(plain) == 0 || len(plain) > maxMaterial {
			return errRecovery
		}
		cipher, err := encrypt(plain, r)
		if err != nil {
			return err
		}
		if err = writeNew(bundle, cipher); err != nil {
			return err
		}
		fmt.Println("RECOVERY_ENCRYPTED")
	default:
		return errors.New("UNKNOWN_ACTION")
	}
	return nil
}

func main() {
	action := flag.String("action", "prepare", "prepare, seal, or verify")
	dir := flag.String("directory", "", "absolute custody directory")
	bundle := flag.String("bundle", "", "absolute encrypted bundle path")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "UNEXPECTED_ARGUMENTS")
		os.Exit(1)
	}
	if err := run(*action, *dir, *bundle); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
