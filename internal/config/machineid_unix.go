//go:build !windows && !darwin

package config

import (
	"fmt"
	"os"
	"strings"
)

// readMachineID returns this host's stable machine identifier. On Unix it is
// /etc/machine-id — set at OS install, unique per machine, stable for the install
// lifetime. Used to derive the config-encryption key.
func readMachineID() (string, error) {
	raw, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", fmt.Errorf("/etc/machine-id not found (%w). "+
			"Ensure the host has a unique /etc/machine-id before running wxt-agent", err)
	}
	machineID := strings.TrimSpace(string(raw))
	if len(machineID) < 16 {
		return "", fmt.Errorf("/etc/machine-id too short (%d chars), file may be corrupt", len(machineID))
	}
	return machineID, nil
}
