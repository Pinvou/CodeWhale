//go:build !linux

package billing

import "net"

func validImageValidatorUID(uint32) bool             { return false }
func verifyImageValidatorPeer(net.Conn, uint32) bool { return false }
