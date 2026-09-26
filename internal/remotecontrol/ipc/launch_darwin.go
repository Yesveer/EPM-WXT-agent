//go:build darwin

package ipc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// launchHelper starts the session helper inside the logged-in user's GUI
// session.
//
// The daemon runs as root with no window server connection, so simply forking
// would produce a process that can neither capture the screen nor draw the
// consent prompt. `launchctl asuser` moves the new process into the target
// user's GUI bootstrap namespace, which is what gives it a window server
// connection and makes its TCC grants — Screen Recording, Accessibility —
// apply to the right user.
func launchHelper(helperPath, addr, token string) (helperProcess, error) {
	uid, err := consoleUID()
	if err != nil {
		return nil, err
	}

	// The helper is launched detached, so its stderr goes nowhere. Without a
	// log file everything it reports — including whether viewer input is
	// arriving — is invisible, which makes "remote control does nothing"
	// impossible to diagnose.
	logPath := helperLogPath()

	cmd := exec.Command("/bin/launchctl", "asuser", strconv.Itoa(uid),
		helperPath, "--connect", addr, "--token", token, "--log", logPath)

	// The token is passed as an argument rather than through the environment
	// or a file: it is single-use, and keeping it off disk means a stale copy
	// cannot be replayed later.
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launchctl asuser %d: %w", uid, err)
	}
	return &execProcess{cmd: cmd}, nil
}

// execProcess adapts an *exec.Cmd to helperProcess.
type execProcess struct{ cmd *exec.Cmd }

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	// Reap it, or the helper lingers as a zombie for the daemon's lifetime.
	go func() { _ = p.cmd.Wait() }()
	return err
}

// consoleUID returns the uid of whoever owns the graphical console.
//
// /dev/console is owned by the user currently logged in at the screen, which
// makes stat'ing it the simplest reliable way to find them — no SystemConfiguration
// framework, no parsing `who`. When nobody is logged in it belongs to root,
// and launching a helper there would be pointless, so that is rejected.
func consoleUID() (int, error) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return 0, fmt.Errorf("stat /dev/console: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type for /dev/console")
	}
	uid := int(st.Uid)
	if uid == 0 {
		return 0, fmt.Errorf("no user is logged in at the console")
	}
	return uid, nil
}

// helperLogPath is where the session helper writes its log, alongside the
// agent's own.
func helperLogPath() string {
	dir := "/var/log/vsay"
	_ = os.MkdirAll(dir, 0o755) // #nosec G301 -- matches the agent's own log dir
	return filepath.Join(dir, "session-helper.log")
}
