//go:build !windows && !darwin

package clipboard

// noopClipboard stands in on platforms without remote control. It reports no
// changes and swallows writes so that callers need no build tags of their own.
type noopClipboard struct{}

func newClipboard() (Clipboard, error) { return noopClipboard{}, nil }

func (noopClipboard) Read() (string, error) { return "", nil }
func (noopClipboard) Write(string) error    { return nil }
func (noopClipboard) Changed() bool         { return false }
func (noopClipboard) Close() error          { return nil }
