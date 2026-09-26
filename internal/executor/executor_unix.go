//go:build !windows

package executor

import (
	"os/exec"
	"syscall"
)

// setUserCredentials sets the user credentials for command execution (Unix only)
func (e *Executor) setUserCredentials(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// e.uid/e.gid come from os/user lookups against the local passwd/group
		// database, never negative in any real system configuration.
		Credential: &syscall.Credential{
			Uid: uint32(e.uid), // #nosec G115
			Gid: uint32(e.gid), // #nosec G115
		},
	}
}
