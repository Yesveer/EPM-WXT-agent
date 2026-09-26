//go:build !windows && !darwin

package capture

import "errors"

// newCapturer refuses on platforms EPM does not target. Remote control ships
// for Windows and macOS only; Linux machines are managed through the terminal.
// Failing loudly here keeps a Linux build from advertising a desktop it cannot
// actually produce.
func newCapturer() (Capturer, error) {
	return nil, errors.New("screen capture is only implemented for Windows and macOS")
}
