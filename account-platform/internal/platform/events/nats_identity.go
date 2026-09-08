package events

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// File authentication is explicit and never silently overrides another identity.
// Legacy password-only URLs and JWT credentials remain supported.
func natsAuthentication(rawURL, seedFile, credentialsFile string) ([]nats.Option, error) {
	if seedFile != "" && credentialsFile != "" {
		return nil, ErrNATSResources
	}
	if seedFile == "" && credentialsFile == "" {
		return nil, nil
	}
	for _, endpoint := range strings.Split(rawURL, ",") {
		endpoint = strings.TrimSpace(endpoint)
		// Match nats.go's support for URLs without a scheme before checking
		// userinfo, including token-only and comma-separated server URLs.
		if !strings.Contains(endpoint, "://") {
			endpoint = "nats://" + endpoint
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.User != nil {
			return nil, ErrNATSResources
		}
	}
	if credentialsFile != "" {
		return []nats.Option{nats.UserCredentials(credentialsFile)}, nil
	}
	pair, err := readNATSUserSeed(seedFile)
	if err != nil {
		return nil, err
	}
	public, err := pair.PublicKey()
	pair.Wipe()
	if err != nil {
		return nil, ErrNATSResources
	}
	return []nats.Option{nats.Nkey(public, func(nonce []byte) ([]byte, error) {
		// Every reconnect repeats the file checks. A different identity requires
		// a new client; never sign for a stale public-key connection option.
		current, err := readNATSUserSeed(seedFile)
		if err != nil {
			return nil, ErrNATSResources
		}
		defer current.Wipe()
		actual, err := current.PublicKey()
		if err != nil || actual != public {
			return nil, ErrNATSResources
		}
		signature, err := current.Sign(nonce)
		if err != nil {
			return nil, ErrNATSResources
		}
		return signature, nil
	})}, nil
}

func readNATSUserSeed(filename string) (nkeys.KeyPair, error) {
	resolved, err := filepath.EvalSymlinks(filename)
	info, statErr := os.Lstat(filename)
	parent, parentErr := os.Stat(filepath.Dir(filename))
	if !filepath.IsAbs(filename) || filepath.Clean(filename) != filename || err != nil || resolved != filename || statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4096 || parentErr != nil || !parent.IsDir() || (runtime.GOOS != "windows" && (info.Mode().Perm()&0077 != 0 || parent.Mode().Perm()&0077 != 0)) {
		return nil, ErrNATSResources
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, ErrNATSResources
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() <= 0 || opened.Size() > 4096 || (runtime.GOOS != "windows" && opened.Mode().Perm()&0077 != 0) {
		return nil, ErrNATSResources
	}
	seed, err := io.ReadAll(io.LimitReader(f, 4097))
	defer clear(seed)
	if err != nil || len(seed) > 4096 {
		return nil, ErrNATSResources
	}
	pair, err := nkeys.FromSeed(bytes.TrimSpace(seed))
	if err != nil {
		return nil, ErrNATSResources
	}
	public, err := pair.PublicKey()
	if err != nil || !nkeys.IsValidPublicUserKey(public) {
		pair.Wipe()
		return nil, ErrNATSResources
	}
	return pair, nil
}
