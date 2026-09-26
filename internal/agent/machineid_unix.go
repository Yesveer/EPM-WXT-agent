//go:build !windows && !darwin

package agent

import (
	"fmt"
	"os"
	"strings"
)

// readMachineID returns this host's stable machine identifier (/etc/machine-id on
// Unix). Used to derive the private-key encryption key.
func readMachineID() (string, error) {
	raw, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", fmt.Errorf("/etc/machine-id not found (%w). "+
			"Ensure the host has a unique /etc/machine-id before running wxt-agent", err)
	}
	machineID := strings.TrimSpace(string(raw))
	if len(machineID) < 16 {
		return "", fmt.Errorf("/etc/machine-id is too short (%d chars), file may be corrupt", len(machineID))
	}
	return machineID, nil
}
