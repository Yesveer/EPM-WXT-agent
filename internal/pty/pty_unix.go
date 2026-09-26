//go:build !windows

package pty

import (
	"os/exec"
	"syscall"
)

// setUserCredentials sets the user credentials for PTY session (Unix only)
func setUserCredentials(cmd *exec.Cmd, uid, gid int) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// uid/gid come from os/user lookups against the local passwd/group
		// database, never negative in any real system configuration.
		Credential: &syscall.Credential{
			Uid: uint32(uid), // #nosec G115
			Gid: uint32(gid), // #nosec G115
		},
	}
}
