package events

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// PrepareNATSCredentials handles only the init-container command. Paths are fixed
// here and clients are allowlisted by each binary; argv never selects a Secret
// path. The application startup path does not read the projected Secret.
func PrepareNATSCredentials(args []string, clients ...string) (bool, error) {
	if len(args) == 0 || args[0] != "prepare-nats-credentials" {
		return false, nil
	}
	if len(args) != 2 || !slices.Contains(clients, args[1]) {
		return true, ErrNATSResources
	}
	return true, StageNATSCredentials(
		"/var/run/bootstrap/pinvou/nats/"+args[1]+".creds",
		"/run/pinvou/nats/private/"+args[1]+".creds")
}

// StageNATSCredentials copies a Kubernetes projected input into a private real
// file. Only this init operation accepts source symlinks. The unchanged runtime
// reader validates the complete multi-line JWT/NKey document before publication.
func StageNATSCredentials(source, destination string) error {
	if source == destination || !filepath.IsAbs(source) || filepath.Clean(source) != source ||
		!filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return ErrNATSResources
	}
	parent := filepath.Dir(destination)
	root := filepath.Dir(parent)
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return ErrNATSResources
	}
	if err := os.Mkdir(parent, 0700); err != nil && !os.IsExist(err) {
		return ErrNATSResources
	}
	canonical, err = filepath.EvalSymlinks(parent)
	private, statErr := os.Lstat(parent)
	if err != nil || canonical != parent || statErr != nil || !private.IsDir() ||
		(runtime.GOOS != "windows" && private.Mode().Perm()&0077 != 0) {
		return ErrNATSResources
	}
	before, err := os.Stat(source)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > 64<<10 {
		return ErrNATSResources
	}
	input, err := os.Open(source)
	if err != nil {
		return ErrNATSResources
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return ErrNATSResources
	}
	body, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	defer clear(body)
	if err != nil || len(body) == 0 || len(body) > 64<<10 {
		return ErrNATSResources
	}
	output, err := os.CreateTemp(parent, ".nats-credentials-*")
	if err != nil {
		return ErrNATSResources
	}
	defer func() {
		output.Close()
		os.Remove(output.Name())
	}()
	if _, err := output.Write(body); err != nil {
		return ErrNATSResources
	}
	if err := output.Chmod(0400); err != nil {
		return ErrNATSResources
	}
	if err := output.Close(); err != nil {
		return ErrNATSResources
	}
	_, pair, err := readNATSUserCredentials(output.Name())
	if err != nil {
		return ErrNATSResources
	}
	pair.Wipe()
	// Link publishes only a complete checked file and never overwrites a name.
	if err := os.Link(output.Name(), destination); err == nil {
		return nil
	} else if !os.IsExist(err) {
		return ErrNATSResources
	}
	// A killed init may have published before reporting success. Accept only the
	// same complete private file on retry; rotation needs a fresh Pod/emptyDir.
	_, current, err := readNATSUserCredentials(destination)
	if err != nil {
		return ErrNATSResources
	}
	current.Wipe()
	existing, err := os.ReadFile(destination)
	defer clear(existing)
	if err != nil || !bytes.Equal(existing, body) {
		return ErrNATSResources
	}
	return nil
}
