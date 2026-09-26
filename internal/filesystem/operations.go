package filesystem

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap"
)

// Operations handles filesystem operations over WebSocket
type Operations struct {
	logger *zap.Logger
}

// NewOperations creates a new filesystem operations handler
func NewOperations(logger *zap.Logger) *Operations {
	return &Operations{
		logger: logger,
	}
}

// StatResult represents file stat information
type StatResult struct {
	IsDir bool  `json:"is_dir"`
	Size  int64 `json:"size"`
	MTime int64 `json:"mtime"`
	CTime int64 `json:"ctime"`
}

// ReadDirEntry represents a directory entry
type ReadDirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

// ReadDirResult represents directory listing result
type ReadDirResult struct {
	Entries []ReadDirEntry `json:"entries"`
}

// Stat returns file/directory information
func (o *Operations) Stat(path string) (*StatResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	return &StatResult{
		IsDir: info.IsDir(),
		Size:  info.Size(),
		MTime: info.ModTime().Unix(),
		CTime: info.ModTime().Unix(), // Go doesn't expose creation time easily
	}, nil
}

// ReadDir lists directory contents
func (o *Operations) ReadDir(path string) (*ReadDirResult, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	result := &ReadDirResult{
		Entries: make([]ReadDirEntry, 0, len(entries)),
	}

	for _, entry := range entries {
		result.Entries = append(result.Entries, ReadDirEntry{
			Name:  entry.Name(),
			IsDir: entry.IsDir(),
		})
	}

	return result, nil
}

// Operations is a remote file-management API exposed to the authenticated
// (mTLS) backend on behalf of an authorized operator — the same trust model
// as the agent's remote terminal/exec. Arbitrary path access here is the
// intended feature (an admin managing the machine's filesystem), not a
// traversal bug, so paths below are deliberately not restricted to a
// sub-root — doing so would break legitimate remote file management.

// ReadFile reads file content as base64
func (o *Operations) ReadFile(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(data), nil
}

// WriteFile writes file content from base64
func (o *Operations) WriteFile(path string, contentBase64 string, create bool, overwrite bool) error {
	// Check if file exists
	_, err := os.Stat(path)
	fileExists := err == nil

	if fileExists && !overwrite {
		return fmt.Errorf("file exists and overwrite is false")
	}

	if !fileExists && !create {
		return fmt.Errorf("file does not exist and create is false")
	}

	// Decode base64 content
	data, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		return fmt.Errorf("failed to decode content: %w", err)
	}

	// Create parent directory if needed
	if create {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil { // #nosec G301
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	// Write file — standard umask-style default permissions; the operator
	// manages arbitrary files here, so we don't second-guess intended perms.
	if err := os.WriteFile(path, data, 0644); err != nil { // #nosec G306,G304
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// MkDir creates a directory
func (o *Operations) MkDir(path string) error {
	return os.MkdirAll(path, 0755) // #nosec G301
}

// Delete removes a file or directory
func (o *Operations) Delete(path string, recursive bool) error {
	if recursive {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// Rename renames/moves a file or directory
func (o *Operations) Rename(oldPath string, newPath string, overwrite bool) error {
	// Check if destination exists
	_, err := os.Stat(newPath)
	destExists := err == nil

	if destExists && !overwrite {
		return fmt.Errorf("destination exists and overwrite is false")
	}

	// Create parent directory of destination
	dir := filepath.Dir(newPath)
	if err := os.MkdirAll(dir, 0755); err != nil { // #nosec G301
		return fmt.Errorf("failed to create directory: %w", err)
	}

	return os.Rename(oldPath, newPath)
}

// Copy copies a file or directory
func (o *Operations) Copy(srcPath string, destPath string, overwrite bool) error {
	// Check if destination exists
	_, err := os.Stat(destPath)
	destExists := err == nil

	if destExists && !overwrite {
		return fmt.Errorf("destination exists and overwrite is false")
	}

	// Get source info
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return err
	}

	if srcInfo.IsDir() {
		return o.copyDir(srcPath, destPath)
	}

	return o.copyFile(srcPath, destPath)
}

func (o *Operations) copyFile(src, dst string) error {
	// Create parent directory
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil { // #nosec G301
		return err
	}

	// Open source file
	srcFile, err := os.Open(src) // #nosec G304
	if err != nil {
		return err
	}
	defer func() { _ = srcFile.Close() }()

	// Create destination file
	dstFile, err := os.Create(dst) // #nosec G304
	if err != nil {
		return err
	}
	defer func() { _ = dstFile.Close() }()

	// Copy content
	_, err = io.Copy(dstFile, srcFile)
	return err
}

func (o *Operations) copyDir(src, dst string) error {
	// Create destination directory
	if err := os.MkdirAll(dst, 0755); err != nil { // #nosec G301
		return err
	}

	// Read source directory
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	// Copy each entry
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := o.copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := o.copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}
