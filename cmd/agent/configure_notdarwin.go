//go:build !darwin

package main

import "fmt"

// runConfigureDarwin is only ever invoked on macOS (guarded by runtime.GOOS in
// runConfigure). This stub exists so the other builds link; it is never called.
func runConfigureDarwin(configureParams) error {
	return fmt.Errorf("macOS configuration is not supported on this platform")
}
