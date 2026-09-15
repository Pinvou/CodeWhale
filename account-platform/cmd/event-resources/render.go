package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

var errRender = errors.New("NATS_PUBLIC_AUTHORIZATION_RENDER_FAILED_PRESERVE_PARTIAL_OUTPUT")

func renderAuthorization(current, expected, previous, previousExpected, output string) error {
	primary, err := readPublicArtifact(current, expected)
	if err != nil {
		return errRender
	}
	var old []byte
	if previous != "" {
		old, err = readPublicArtifact(previous, previousExpected)
		if err != nil {
			return errRender
		}
	}
	policy, err := events.RenderNATSAuthorization(primary, old)
	if err != nil {
		return errRender
	}
	return writeRenderedPolicy(output, policy)
}

func writeRenderedPolicy(output string, policy []byte) error {
	parent := filepath.Dir(output)
	resolved, err := filepath.EvalSymlinks(parent)
	info, statErr := os.Stat(parent)
	if !filepath.IsAbs(output) || filepath.Clean(output) != output || err != nil || resolved != parent || statErr != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errRender
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errRender
	}
	n, writeErr := f.Write(policy)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || n != len(policy) || syncErr != nil || closeErr != nil {
		return errRender
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(parent)
		if err != nil {
			return errRender
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return errRender
		}
	}
	actual, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(actual, policy) {
		return errRender
	}
	return nil
}

func renderConsumerAuthorization(current, expected, previous, previousExpected, manifest, manifestExpected, output string) (string, error) {
	primary, err := readPublicArtifact(current, expected)
	if err != nil {
		return "", errRender
	}
	var old []byte
	if previous != "" {
		old, err = readPublicArtifact(previous, previousExpected)
		if err != nil {
			return "", errRender
		}
	}
	resources, err := readPublicArtifact(manifest, manifestExpected)
	if err != nil {
		return "", errRender
	}
	result, err := events.RenderNATSAuthorizationWithConsumers(primary, old, resources, expected, manifestExpected)
	if err != nil || writeRenderedPolicy(output, result.Configuration) != nil {
		return "", errRender
	}
	return "NATS_CONSUMER_AUTHORIZATION_RENDERED identity_sha256=" + result.IdentitySHA256 + " manifest_sha256=" + result.ManifestSHA256 + " configuration_sha256=" + result.ConfigurationSHA256, nil
}

func renderConsumerManifest(current, expected, output string) error {
	primary, err := readPublicArtifact(current, expected)
	if err != nil {
		return errRender
	}
	manifest, err := events.ApprovalConsumerManifest(expected)
	if err != nil {
		return errRender
	}
	digest := sha256.Sum256(manifest)
	// Reuse the v2-only validator without writing or activating its ACL output.
	if _, err := events.RenderNATSAuthorizationWithConsumers(primary, nil, manifest, expected, hex.EncodeToString(digest[:])); err != nil {
		return errRender
	}
	return writeRenderedPolicy(output, manifest)
}

func readPublicArtifact(path, expected string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	info, statErr := os.Lstat(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || err != nil || resolved != path || statErr != nil || !info.Mode().IsRegular() || info.Size() > 32<<10 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, errRender
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errRender
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || (runtime.GOOS != "windows" && opened.Mode().Perm()&0022 != 0) {
		return nil, errRender
	}
	b, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	if err != nil || len(b) > 32<<10 {
		return nil, errRender
	}
	digest := sha256.Sum256(b)
	if hex.EncodeToString(digest[:]) != expected {
		return nil, errRender
	}
	return b, nil
}
