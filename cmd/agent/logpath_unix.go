//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// defaultLogFile is the fallback log path used when the config doesn't specify one.
func defaultLogFile() string {
	return "/var/log/vsay/agent.log"
}

// writeBootError appends an early startup error to the default log file, for failures
// that happen before the zap logger is initialized (e.g. config load). Best-effort.
func writeBootError(msg string) { writeBootLine("ERROR", msg) }

// writeBootLog writes an early INFO line to the default log file. Called at the very
// top of `start` so the log file is ALWAYS created immediately on a real run.
func writeBootLog(msg string) { writeBootLine("INFO", msg) }

func writeBootLine(level, msg string) {
	// path is the fixed literal from defaultLogFile(), never request/user
	// input. Left world-readable (0755/0644), matching every other agent log
	// path, since ops/monitoring tooling may reasonably expect to read it.
	path := defaultLogFile()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)                              // #nosec G301,G304
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) // #nosec G302,G304
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	fmt.Fprintf(f, "%s | %s | %s\n", time.Now().Format("2006-01-02 15:04:05"), level, msg)
}
