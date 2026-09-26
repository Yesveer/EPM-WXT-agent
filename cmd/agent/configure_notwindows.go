//go:build !windows

package main

import "fmt"

// runConfigureWindows is only ever invoked on Windows (guarded by runtime.GOOS in
// runConfigure). This stub exists so the non-Windows build links; it is never called.
func runConfigureWindows(configureParams) error {
	return fmt.Errorf("Windows configuration is not supported on this platform")
}
