package executor

import (
	"bytes"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"

	"go.uber.org/zap"
)

// Executor handles command execution as a specific Linux user
type Executor struct {
	username   string
	uid        int
	gid        int
	homeDir    string
	shell      string
	allowSudo  bool
	logger     *zap.Logger
	userExists bool
}

// New creates a new executor for the given username
func New(username string, allowSudo bool, logger *zap.Logger) (*Executor, error) {
	e := &Executor{
		username:  username,
		allowSudo: allowSudo,
		logger:    logger,
	}

	// Check if user exists
	u, err := user.Lookup(username)
	if err != nil {
		// User doesn't exist, we'll need to create it
		e.userExists = false
		e.logger.Info("User does not exist, will be created", zap.String("username", username))

		// Create the user
		if err := e.createUser(); err != nil {
			return nil, fmt.Errorf("failed to create user: %w", err)
		}

		// Lookup again after creation
		u, err = user.Lookup(username)
		if err != nil {
			return nil, fmt.Errorf("failed to lookup user after creation: %w", err)
		}
	} else {
		e.userExists = true
		e.logger.Info("User already exists", zap.String("username", username))
	}

	// Parse UID and GID
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	e.uid = uid
	e.gid = gid
	e.homeDir = u.HomeDir
	e.shell = "/bin/bash" // Default shell

	return e, nil
}

// createUser creates a new Linux user
func (e *Executor) createUser() error {
	e.logger.Info("Creating user", zap.String("username", e.username))

	// Create user with home directory
	// useradd -m -s /bin/bash <username>
	cmd := exec.Command("useradd", "-m", "-s", "/bin/bash", e.username) // #nosec G204 -- argv-slice form, no shell involved
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd failed: %w, output: %s", err, string(output))
	}

	e.logger.Info("User created successfully", zap.String("username", e.username))

	// Add to sudo group if requested
	if e.allowSudo {
		if err := e.grantSudoAccess(); err != nil {
			e.logger.Warn("Failed to grant sudo access", zap.Error(err))
			// Don't fail, just log warning
		}
	}

	return nil
}

// grantSudoAccess adds the user to the sudo group
func (e *Executor) grantSudoAccess() error {
	e.logger.Info("Granting sudo access", zap.String("username", e.username))

	// Try different sudo group names depending on the distro
	sudoGroups := []string{"sudo", "wheel"}

	for _, group := range sudoGroups {
		// Check if group exists
		if _, err := user.LookupGroup(group); err != nil {
			continue
		}

		// usermod -aG <group> <username>
		cmd := exec.Command("usermod", "-aG", group, e.username) // #nosec G204 -- argv-slice form, no shell involved
		output, err := cmd.CombinedOutput()
		if err != nil {
			e.logger.Warn("Failed to add to group",
				zap.String("group", group),
				zap.Error(err),
				zap.String("output", string(output)),
			)
			continue
		}

		e.logger.Info("User added to sudo group",
			zap.String("username", e.username),
			zap.String("group", group),
		)
		return nil
	}

	return fmt.Errorf("no sudo group found (tried: %v)", sudoGroups)
}

// Execute runs a command as the configured user
func (e *Executor) Execute(command string, env map[string]string) (*ExecutionResult, error) {
	e.logger.Info("Executing command",
		zap.String("command", command),
		zap.String("user", e.username),
		zap.String("workdir", e.homeDir),
	)

	// Create command. This IS the agent's core purpose — running a command the
	// authenticated (mTLS) backend sent — not an injection bug; e.shell is
	// always the fixed literal "/bin/bash", never derived from `command`.
	cmd := exec.Command(e.shell, "-c", command) // #nosec G204

	// Set working directory to user's home
	cmd.Dir = e.homeDir

	// Set up environment
	cmd.Env = e.buildEnvironment(env)

	// Set user credentials (platform-specific)
	e.setUserCredentials(cmd)

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Execute
	err := cmd.Run()

	// Determine exit code
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	result := &ExecutionResult{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: int32(exitCode),
	}

	e.logger.Info("Command executed",
		zap.String("command", command),
		zap.Int32("exit_code", result.ExitCode),
		zap.Int("stdout_len", len(result.Stdout)),
		zap.Int("stderr_len", len(result.Stderr)),
	)

	return result, nil
}

// buildEnvironment builds environment variables for command execution
func (e *Executor) buildEnvironment(extraEnv map[string]string) []string {
	env := []string{
		"HOME=" + e.homeDir,
		"USER=" + e.username,
		"LOGNAME=" + e.username,
		"SHELL=" + e.shell,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}

	// Add extra environment variables
	for k, v := range extraEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	return env
}

// GetUserInfo returns information about the executor's user
func (e *Executor) GetUserInfo() UserInfo {
	return UserInfo{
		Username:  e.username,
		UID:       e.uid,
		GID:       e.gid,
		HomeDir:   e.homeDir,
		Shell:     e.shell,
		AllowSudo: e.allowSudo,
	}
}

// UserInfo contains information about a user
type UserInfo struct {
	Username  string
	UID       int
	GID       int
	HomeDir   string
	Shell     string
	AllowSudo bool
}

// ExecutionResult contains the result of a command execution
type ExecutionResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int32
}

// CheckSudoAccess checks if the user has sudo access
func CheckSudoAccess(username string) bool {
	u, err := user.Lookup(username)
	if err != nil {
		return false
	}

	gids, err := u.GroupIds()
	if err != nil {
		return false
	}

	// Check if user is in sudo or wheel group
	for _, gid := range gids {
		g, err := user.LookupGroupId(gid)
		if err != nil {
			continue
		}
		if g.Name == "sudo" || g.Name == "wheel" {
			return true
		}
	}

	return false
}

// ValidateSudoAccess validates that the user has sudo access if required
func ValidateSudoAccess(username string) error {
	if !CheckSudoAccess(username) {
		return fmt.Errorf("user '%s' is not in sudo group. Add user to sudo group: sudo usermod -aG sudo %s. User must be in sudo group", username, username)
	}
	return nil
}
