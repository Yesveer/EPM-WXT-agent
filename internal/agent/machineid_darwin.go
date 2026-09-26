//go:build darwin

package agent

import (
	"fmt"
	"os/exec"
	"strings"
)

// readMachineID returns this host's stable machine identifier.
//
// macOS has no /etc/machine-id. Its equivalent is IOPlatformUUID, burned into
// the hardware and stable for the life of the machine — which is exactly the
// property the config- and key-encryption derivation needs. It is read through
// ioreg rather than IOKit so this stays a cgo-free package.
func readMachineID() (string, error) {
	out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return "", fmt.Errorf("could not read IOPlatformUUID via ioreg: %w", err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		// The line looks like:  "IOPlatformUUID" = "2C936D30-...-26BD87FC1B91"
		parts := strings.Split(line, "\"")
		if len(parts) < 4 {
			continue
		}
		id := strings.TrimSpace(parts[len(parts)-2])
		if len(id) < 16 {
			return "", fmt.Errorf("IOPlatformUUID is too short (%d chars)", len(id))
		}
		return id, nil
	}
	return "", fmt.Errorf("IOPlatformUUID not present in ioreg output")
}
