//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"strings"
)

// readMachineID returns this host's stable machine identifier — the registry
// MachineGuid on Windows (HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid), the
// Windows analogue of /etc/machine-id. Used to derive the private-key encryption key.
func readMachineID() (string, error) {
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
	if err != nil {
		return "", fmt.Errorf("cannot read MachineGuid from registry: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("MachineGuid registry value not found")
	}
	machineID := strings.TrimSpace(fields[len(fields)-1])
	if len(machineID) < 16 {
		return "", fmt.Errorf("MachineGuid too short (%d chars)", len(machineID))
	}
	return machineID, nil
}
