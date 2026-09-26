package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These identifiers previously existed twice — once where the service is
// registered, once where self-update restarts it — and drifted apart. Both
// failures were silent: the update installed, the restart targeted a name that
// did not exist, and the machine kept running the old binary.
//
// These tests read the configure sources and fail if either stops referencing
// the shared constants, which is the only way that drift can reappear.

func readSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

func TestDarwinConfigureUsesTheSharedLaunchdLabel(t *testing.T) {
	src := readSource(t, "cmd/agent/configure_darwin.go")

	if !strings.Contains(src, "agent.LaunchDaemonLabel") {
		t.Error("configure_darwin.go no longer uses agent.LaunchDaemonLabel — " +
			"a hardcoded label here will silently break self-update restarts")
	}
	// A literal label alongside the constant means one of them is unused and
	// the two can drift again.
	if strings.Contains(src, `"io.wxt.agent"`) || strings.Contains(src, `"com.vsay.agent"`) {
		t.Error("configure_darwin.go contains a hardcoded launchd label")
	}
}

func TestWindowsConfigureUsesTheSharedTaskName(t *testing.T) {
	src := readSource(t, "cmd/agent/configure_windows.go")

	if !strings.Contains(src, "agent.WindowsTaskName") {
		t.Error("configure_windows.go no longer uses agent.WindowsTaskName — " +
			"a hardcoded task name here will silently break self-update restarts")
	}
}

// The agent is a scheduled task on Windows, not a service. Restarting it with
// `net stop` targets a service that does not exist and fails silently.
func TestWindowsRestartDoesNotUseNetStop(t *testing.T) {
	src := readSource(t, "internal/agent/agent.go")

	start := strings.Index(src, "func (a *Agent) restartService()")
	if start == -1 {
		t.Fatal("restartService not found")
	}
	body := src[start : start+2500]

	if strings.Contains(body, "net stop") || strings.Contains(body, "net start") {
		t.Error("restartService uses `net stop/start`, but the agent is registered " +
			"as a scheduled task — use schtasks")
	}
	if !strings.Contains(body, "schtasks") {
		t.Error("restartService does not use schtasks for Windows")
	}
	if !strings.Contains(body, "LaunchDaemonLabel") {
		t.Error("restartService does not use the shared launchd label")
	}
	if !strings.Contains(body, "WindowsTaskName") {
		t.Error("restartService does not use the shared Windows task name")
	}
}

func TestServiceIdentifiersAreNotEmpty(t *testing.T) {
	for name, v := range map[string]string{
		"LaunchDaemonLabel": LaunchDaemonLabel,
		"WindowsTaskName":   WindowsTaskName,
		"SystemdUnit":       SystemdUnit,
	} {
		if strings.TrimSpace(v) == "" {
			t.Errorf("%s is empty", name)
		}
	}
}
