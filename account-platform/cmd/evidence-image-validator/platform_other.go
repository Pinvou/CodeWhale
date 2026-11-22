//go:build !linux

package main

import (
	"net"
	"os/exec"
)

// The candidate service has no verified peer or process isolation here.
func allowedPeer(net.Conn, uint32) bool { return false }
func secureSocketPath(string) bool      { return false }
func configureChild(*exec.Cmd)          {}
