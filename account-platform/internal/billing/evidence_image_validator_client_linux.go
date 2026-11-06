//go:build linux

package billing

import (
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func validImageValidatorUID(uid uint32) bool {
	return uid != 0 && uid != uint32(os.Geteuid()) && os.Geteuid() != 0
}
func verifyImageValidatorPeer(conn net.Conn, uid uint32) bool {
	u, ok := conn.(*net.UnixConn)
	if !ok || !validImageValidatorUID(uid) {
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
	return cred.Uid == uid && cred.Uid != 0
}
