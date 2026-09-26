//go:build !windows && !darwin

package ipc

import "errors"

// launchHelper has no interactive session to launch into on platforms EPM does
// not target.
func launchHelper(helperPath, addr, token string) (helperProcess, error) {
	return nil, errors.New("the session helper is only supported on Windows and macOS")
}
