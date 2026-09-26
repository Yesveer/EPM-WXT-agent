//go:build windows

package executor

import (
	"os/exec"
)

// setUserCredentials is a no-op on Windows (user impersonation not supported)
func (e *Executor) setUserCredentials(cmd *exec.Cmd) {
	// Windows doesn't support syscall.Credential
	// User impersonation would require different approach on Windows
}
