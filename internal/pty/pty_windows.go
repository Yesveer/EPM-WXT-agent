//go:build windows

package pty

import (
	"os/exec"
)

// setUserCredentials is a no-op on Windows (user impersonation not supported)
func setUserCredentials(cmd *exec.Cmd, uid, gid int) {
	// Windows doesn't support syscall.Credential
	// PTY with user impersonation would require different approach on Windows
}
