//go:build windows

package ipc

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createUnicodeEnvironment = 0x00000400
	createNoWindow           = 0x08000000
	createNewConsole         = 0x00000010
)

// launchHelper starts the session helper inside the interactive console
// session.
//
// The agent daemon runs as a service in session 0, which has its own isolated
// window station. A process started there can neither capture the user's
// screen nor show them a dialog — session 0 isolation has been enforced since
// Vista specifically to stop services doing that. The only supported way
// across is to take the token of whoever is logged in at the console and start
// the process with it, targeting that session's default desktop.
func launchHelper(helperPath, addr, token string) (helperProcess, error) {
	sessionID := windows.WTSGetActiveConsoleSessionId()
	if sessionID == 0xFFFFFFFF {
		return nil, fmt.Errorf("no active console session — nobody is logged in")
	}

	// The token of the user attached to that session. This is what carries
	// their desktop, their profile and their privileges.
	var userToken windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &userToken); err != nil {
		return nil, fmt.Errorf("WTSQueryUserToken(session %d): %w", sessionID, err)
	}
	defer userToken.Close()

	// Prefer the user's ELEVATED token when one exists.
	//
	// With UAC on, WTSQueryUserToken hands back the filtered, medium-integrity
	// token. UIPI then silently refuses synthetic input from that process to
	// any window running at a higher integrity level — so an admin console, an
	// elevated editor or a UAC prompt simply ignores every click and keystroke,
	// with SendInput reporting success. That looks exactly like "remote control
	// is view-only", which is the bug this avoids.
	//
	// A standard user has no linked token; there the filtered one is all there
	// is, and elevated windows stay out of reach.
	launchToken := userToken
	if elevated, err := linkedElevatedToken(userToken); err == nil {
		defer elevated.Close()
		launchToken = elevated
	}

	// WTSQueryUserToken hands back an impersonation token; CreateProcessAsUser
	// requires a primary one, so it has to be duplicated.
	var primary windows.Token
	err := windows.DuplicateTokenEx(
		launchToken,
		windows.MAXIMUM_ALLOWED,
		nil,
		windows.SecurityIdentification,
		windows.TokenPrimary,
		&primary,
	)
	if err != nil {
		return nil, fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primary.Close()

	// Without the user's environment block the helper inherits the service's,
	// so %APPDATA% and friends would point at the system profile.
	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, primary, false); err != nil {
		return nil, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer func() { _ = windows.DestroyEnvironmentBlock(envBlock) }()

	// Launched with CREATE_NO_WINDOW and no console, so the helper's stderr is
	// discarded. A log file is the only way to see what it is doing — without
	// it, "remote control does nothing" has no evidence to work from.
	logPath := helperLogPath()

	cmdLine := windows.EscapeArg(helperPath) +
		" --connect " + windows.EscapeArg(addr) +
		" --token " + windows.EscapeArg(token) +
		" --log " + windows.EscapeArg(logPath)

	argv, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return nil, err
	}
	appName, err := windows.UTF16PtrFromString(helperPath)
	if err != nil {
		return nil, err
	}
	// winsta0\default is the interactive desktop. Omitting it lands the
	// process on the service's invisible desktop, where its consent dialog
	// would be drawn where nobody can see or click it.
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return nil, err
	}

	si := windows.StartupInfo{
		Cb:      uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Desktop: desktop,
	}
	var pi windows.ProcessInformation

	err = windows.CreateProcessAsUser(
		primary,
		appName,
		argv,
		nil, nil,
		false,
		createUnicodeEnvironment|createNoWindow,
		envBlock,
		nil,
		&si,
		&pi,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateProcessAsUser: %w", err)
	}

	// The thread handle is of no further use; leaking it would hold the
	// thread object alive for the daemon's lifetime.
	_ = windows.CloseHandle(pi.Thread)

	return &winProcess{handle: pi.Process, pid: pi.ProcessId}, nil
}

// winProcess adapts a raw process handle to helperProcess.
type winProcess struct {
	handle windows.Handle
	pid    uint32
}

func (p *winProcess) Kill() error {
	if p.handle == 0 {
		return nil
	}
	err := windows.TerminateProcess(p.handle, 1)
	_ = windows.CloseHandle(p.handle)
	p.handle = 0
	return err
}

// helperLogPath is where the session helper writes its log, alongside the
// agent's own under ProgramData.
func helperLogPath() string {
	dir := `C:\ProgramData\vsay\logs`
	if pd := os.Getenv("ProgramData"); pd != "" {
		dir = filepath.Join(pd, "vsay", "logs")
	}
	_ = os.MkdirAll(dir, 0o755) // #nosec G301 -- matches the agent's own log dir
	return filepath.Join(dir, "session-helper.log")
}

// tokenLinkedToken is TOKEN_INFORMATION_CLASS.TokenLinkedToken.
const tokenLinkedToken = 19

// linkedElevatedToken returns the elevated counterpart of a UAC-filtered token.
//
// Windows keeps the two halves linked: the filtered token a normal process runs
// with, and the full-privilege one UAC would grant on elevation. Asking for the
// link is the supported way for a SYSTEM service to start a process that can
// actually drive the whole desktop.
func linkedElevatedToken(t windows.Token) (windows.Token, error) {
	var linked windows.Token
	var returned uint32

	err := windows.GetTokenInformation(
		t,
		tokenLinkedToken,
		(*byte)(unsafe.Pointer(&linked)),
		uint32(unsafe.Sizeof(linked)),
		&returned,
	)
	if err != nil {
		return 0, fmt.Errorf("no linked elevated token: %w", err)
	}
	if linked == 0 {
		return 0, fmt.Errorf("linked token handle was null")
	}
	return linked, nil
}
