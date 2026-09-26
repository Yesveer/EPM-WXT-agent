//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// describeElevation reports whether this process runs with an elevated token.
//
// It matters because UIPI refuses synthetic input from a lower-integrity
// process to a higher-integrity window: a helper running unelevated can drive
// ordinary applications but is silently ignored by anything running as
// administrator. That failure is invisible — SendInput reports success — so
// the state is worth recording at startup rather than inferring later.
func describeElevation() string {
	token := windows.GetCurrentProcessToken()

	var elevated uint32
	var returned uint32
	err := windows.GetTokenInformation(
		token,
		windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevated)),
		uint32(unsafe.Sizeof(elevated)),
		&returned,
	)
	if err != nil {
		return "unknown"
	}
	if elevated != 0 {
		return "elevated"
	}
	return "not elevated — input to administrator windows will be ignored"
}
