package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"go.uber.org/zap"
)

// Handler manages PTY sessions for interactive terminal access
type Handler struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	logger   *zap.Logger
}

// Session represents a PTY session
type Session struct {
	ID         string
	PTY        *os.File
	Cmd        *exec.Cmd
	Username   string
	UID        int
	GID        int
	HomeDir    string
	Shell      string
	OutputChan chan []byte
	mu         sync.Mutex
	logger     *zap.Logger
	running    bool
}

// NewHandler creates a new PTY handler
func NewHandler(logger *zap.Logger) *Handler {
	return &Handler{
		sessions: make(map[string]*Session),
		logger:   logger,
	}
}

// CreateSession creates a new PTY session
func (h *Handler) CreateSession(sessionID, username string, uid, gid int, homeDir, shell string) (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Check if session already exists
	if _, exists := h.sessions[sessionID]; exists {
		return nil, fmt.Errorf("session already exists: %s", sessionID)
	}

	h.logger.Info("Creating PTY session",
		zap.String("session_id", sessionID),
		zap.String("username", username),
		zap.String("home_dir", homeDir),
	)

	// Create shell command
	cmd := exec.Command(shell)
	cmd.Dir = homeDir
	cmd.Env = []string{
		"HOME=" + homeDir,
		"USER=" + username,
		"LOGNAME=" + username,
		"SHELL=" + shell,
		"TERM=xterm-256color",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}

	// Set user credentials (platform-specific)
	setUserCredentials(cmd, uid, gid)

	// Start PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY: %w", err)
	}

	// Create session
	session := &Session{
		ID:         sessionID,
		PTY:        ptmx,
		Cmd:        cmd,
		Username:   username,
		UID:        uid,
		GID:        gid,
		HomeDir:    homeDir,
		Shell:      shell,
		OutputChan: make(chan []byte, 100),
		logger:     h.logger,
		running:    true,
	}

	// Start reading output
	go session.readOutput()

	// Store session
	h.sessions[sessionID] = session

	h.logger.Info("PTY session created", zap.String("session_id", sessionID))

	return session, nil
}

// GetSession retrieves a session by ID
func (h *Handler) GetSession(sessionID string) (*Session, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	session, exists := h.sessions[sessionID]
	if !exists {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	return session, nil
}

// CloseSession closes a PTY session
func (h *Handler) CloseSession(sessionID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, exists := h.sessions[sessionID]
	if !exists {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	h.logger.Info("Closing PTY session", zap.String("session_id", sessionID))

	// Close session
	session.Close()

	// Remove from map
	delete(h.sessions, sessionID)

	return nil
}

// ListSessions returns all active session IDs
func (h *Handler) ListSessions() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	ids := make([]string, 0, len(h.sessions))
	for id := range h.sessions {
		ids = append(ids, id)
	}

	return ids
}

// Session methods

// Write writes data to the PTY (user input)
func (s *Session) Write(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return fmt.Errorf("session is not running")
	}

	_, err := s.PTY.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write to PTY: %w", err)
	}

	return nil
}

// Resize resizes the PTY
func (s *Session) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return fmt.Errorf("session is not running")
	}

	size := &pty.Winsize{
		Rows: rows,
		Cols: cols,
	}

	if err := pty.Setsize(s.PTY, size); err != nil {
		return fmt.Errorf("failed to resize PTY: %w", err)
	}

	s.logger.Debug("PTY resized",
		zap.String("session_id", s.ID),
		zap.Uint16("rows", rows),
		zap.Uint16("cols", cols),
	)

	return nil
}

// Close closes the PTY session
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	s.running = false

	// Close PTY
	if s.PTY != nil {
		if err := s.PTY.Close(); err != nil {
			s.logger.Warn("Failed to close PTY", zap.String("session_id", s.ID), zap.Error(err))
		}
	}

	// Kill process if still running
	if s.Cmd != nil && s.Cmd.Process != nil {
		if err := s.Cmd.Process.Kill(); err != nil {
			s.logger.Warn("Failed to kill PTY process", zap.String("session_id", s.ID), zap.Error(err))
		}
	}

	// Close output channel
	close(s.OutputChan)

	s.logger.Info("PTY session closed", zap.String("session_id", s.ID))
}

// readOutput reads output from PTY and sends to channel
func (s *Session) readOutput() {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("Panic in readOutput", zap.Any("recover", r))
		}
	}()

	buffer := make([]byte, 4096)

	for {
		n, err := s.PTY.Read(buffer)
		if err != nil {
			if err != io.EOF {
				s.logger.Error("Error reading from PTY",
					zap.String("session_id", s.ID),
					zap.Error(err),
				)
			}
			break
		}

		if n > 0 {
			// Copy data to avoid buffer reuse issues
			data := make([]byte, n)
			copy(data, buffer[:n])

			// Send to output channel (non-blocking)
			select {
			case s.OutputChan <- data:
			default:
				s.logger.Warn("Output channel full, dropping data",
					zap.String("session_id", s.ID),
					zap.Int("bytes", n),
				)
			}
		}
	}

	// Wait for command to finish
	if err := s.Cmd.Wait(); err != nil {
		s.logger.Debug("PTY command exited", zap.String("session_id", s.ID), zap.Error(err))
	}
	s.running = false
}

// IsRunning returns whether the session is still running
func (s *Session) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Count returns the number of active PTY (terminal) sessions.
func (h *Handler) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}
