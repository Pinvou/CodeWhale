package operator

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
)

func readSnapshotFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrSnapshot
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, ErrSnapshot
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrSnapshot
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) || info.Size() > MaxSnapshot+(1<<20) {
		return nil, ErrSnapshot
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSnapshot+(1<<20)+1))
	if err != nil || len(b) > MaxSnapshot+(1<<20) {
		return nil, ErrSnapshot
	}
	return b, nil
}

func writeSnapshotFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrSnapshot
	}
	n, writeErr := f.Write(b)
	syncErr, closeErr := f.Sync(), f.Close()
	if writeErr != nil || n != len(b) || syncErr != nil || closeErr != nil || syncDirectory(filepath.Dir(path)) != nil {
		return ErrSnapshot
	}
	stored, err := readSnapshotFile(path)
	if err != nil || len(stored) != len(b) || hash(stored) != hash(b) {
		return ErrSnapshot
	}
	return nil
}
