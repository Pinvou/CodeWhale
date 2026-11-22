//go:build !unix

package main

import "os"

// No permission fallback: an implementation that verifies an equivalent
// private ACL is required before accepting production credentials here.
const privateCredentialPlatformSupported = false

func privateCredentialMetadata(os.FileInfo) bool { return false }
