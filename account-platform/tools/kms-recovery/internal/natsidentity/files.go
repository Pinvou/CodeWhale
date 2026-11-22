package natsidentity

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

func readPublic(path string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	info, statErr := os.Lstat(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || err != nil || resolved != path || statErr != nil || !info.Mode().IsRegular() || info.Size() > 32<<10 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, ErrIdentity
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrIdentity
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || (runtime.GOOS != "windows" && opened.Mode().Perm()&0022 != 0) {
		return nil, ErrIdentity
	}
	b, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	if err != nil || len(b) > 32<<10 {
		return nil, ErrIdentity
	}
	return b, nil
}

func readSeed(path string) ([]byte, error) {
	if privateDirectory(filepath.Dir(path)) != nil {
		return nil, ErrIdentity
	}
	info, err := os.Lstat(path)
	resolved, resolveErr := filepath.EvalSymlinks(path)
	if err != nil || resolveErr != nil || resolved != path || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4096 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, ErrIdentity
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrIdentity
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() != info.Size() || (runtime.GOOS != "windows" && opened.Mode().Perm()&0077 != 0) {
		return nil, ErrIdentity
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 || !bytes.Equal(b, bytes.TrimSpace(b)) {
		clear(b)
		return nil, ErrIdentity
	}
	return b, nil
}
func privateDirectory(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	info, statErr := os.Stat(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || err != nil || resolved != path || statErr != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return ErrIdentity
	}
	return nil
}
func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ErrIdentity
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrIdentity
	}
	return nil
}
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrIdentity
	}
	n, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || n != len(b) || syncErr != nil || closeErr != nil || syncDirectory(filepath.Dir(path)) != nil {
		return ErrIdentity
	}
	actual, err := os.ReadFile(path)
	defer clear(actual)
	if err != nil || !bytes.Equal(actual, b) {
		return ErrIdentity
	}
	return nil
}
