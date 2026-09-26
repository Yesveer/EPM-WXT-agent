//go:build windows

package config

import (
	"fmt"
	"os/exec"
	"strings"
)

// readMachineID returns this host's stable machine identifier. On Windows it is the
// registry MachineGuid (HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid) — created
// at OS install, unique per machine, stable for the install lifetime. This is the
// Windows analogue of /etc/machine-id and is used to derive the config-encryption key.
func readMachineID() (string, error) {
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
	if err != nil {
		return "", fmt.Errorf("cannot read MachineGuid from registry: %w", err)
	}
	// Output form: "    MachineGuid    REG_SZ    <guid>"
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
