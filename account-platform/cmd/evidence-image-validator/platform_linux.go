//go:build linux

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func allowedPeer(c net.Conn, allowedUID uint32) bool {
	u, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var opErr error
	if raw.Control(func(fd uintptr) { cred, opErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }) != nil || opErr != nil || cred == nil {
		return false
	}
	return cred.Uid == allowedUID && cred.Uid != 0
}
func secureSocketPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." {
		return false
	}
	dir := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != dir {
		return false
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0027 != 0 {
		return false
	}
	info, ok := st.Sys().(*syscall.Stat_t)
	if !ok || info.Uid != uint32(os.Geteuid()) || info.Uid == 0 {
		return false
	}
	// Ancestors must not be replaceable by an unrelated uid or writable by a
	// group/other user. The service UID itself still must be deployment-isolated.
	for ancestor := filepath.Dir(dir); ; ancestor = filepath.Dir(ancestor) {
		entry, e := os.Lstat(ancestor)
		if e != nil || !entry.IsDir() || entry.Mode().Perm()&0022 != 0 {
			return false
		}
		owner, ok := entry.Sys().(*syscall.Stat_t)
		if !ok || (owner.Uid != 0 && owner.Uid != uint32(os.Geteuid())) {
			return false
		}
		if ancestor == filepath.Dir(ancestor) {
			break
		}
	}
	_, err = os.Lstat(path)
	return os.IsNotExist(err)
}
func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
