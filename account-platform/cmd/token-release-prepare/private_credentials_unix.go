//go:build unix

package main

import (
	"os"
	"syscall"
)

const privateCredentialPlatformSupported = true

func privateCredentialMetadata(info os.FileInfo) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && owner.Uid == uint32(os.Geteuid()) && info.Mode().Perm() == 0600
}
