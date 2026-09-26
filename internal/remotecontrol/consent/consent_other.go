//go:build !windows && !darwin

package consent

import "errors"

// ask has nowhere to draw a prompt on platforms EPM does not target. It denies
// rather than defaulting to approval: no prompt means no consent.
func ask(Request) (Decision, error) {
	return Denied, errors.New("consent prompts are only implemented for Windows and macOS")
}
