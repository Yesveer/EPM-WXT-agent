//go:build !windows && !darwin

package input

import "errors"

// newInjector refuses on platforms EPM does not target. See the note in
// capture_other.go: remote control is a Windows and macOS feature.
func newInjector() (Injector, error) {
	return nil, errors.New("input injection is only implemented for Windows and macOS")
}
