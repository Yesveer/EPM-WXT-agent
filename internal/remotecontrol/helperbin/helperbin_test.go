package helperbin

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests run against a plain `go test` build, which embeds the committed
// placeholder rather than a real helper — so the not-embedded path is what the
// package-level functions exercise, and the write logic is driven through
// extractTo with fake payloads.

func TestPlaceholderIsNotTreatedAsAHelper(t *testing.T) {
	// This checks placeholder DETECTION, so it only applies to a build that
	// carries the placeholder — a Makefile build embeds a real helper and has
	// nothing to detect.
	if len(helper) > 0 && !bytes.HasPrefix(helper, []byte(placeholderMarker)) {
		t.Skip("this build embeds a real helper")
	}

	// The whole point of the marker: an agent built without the Makefile's
	// inject step must NOT extract 600 bytes of prose and call it an
	// executable. Getting this wrong produces a helper that launchd or
	// CreateProcess refuses, with no clue why.
	if Available() {
		t.Fatal("the committed placeholder was mistaken for a real helper")
	}
	if got := Size(); got != 0 {
		t.Errorf("Size() = %d with no helper embedded, want 0", got)
	}
	if got := SHA256(); got != "" {
		t.Errorf("SHA256() = %q with no helper embedded, want empty", got)
	}
}

func TestExtractRefusesWhenNothingIsEmbedded(t *testing.T) {
	if Available() {
		t.Skip("this build embeds a real helper")
	}
	_, err := Extract(filepath.Join(t.TempDir(), "helper"))
	if !errors.Is(err, ErrNotEmbedded) {
		t.Fatalf("Extract error = %v, want ErrNotEmbedded", err)
	}
}

func TestExtractWritesAnExecutable(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nested", "wxt-agent-session")
	payload := []byte("pretend this is a binary")

	got, err := extractTo(dst, payload)
	if err != nil {
		t.Fatalf("extractTo: %v", err)
	}
	if got != dst {
		t.Errorf("returned path = %q, want %q", got, dst)
	}

	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Error("written contents do not match the payload")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// A helper that is not executable fails at launch with a permission error
	// that looks nothing like "the file mode is wrong".
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the executable bits set", info.Mode().Perm())
	}
}

func TestExtractLeavesAnIdenticalFileAlone(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "helper")
	payload := []byte("same bytes")

	if _, err := extractTo(dst, payload); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	first, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := extractTo(dst, payload); err != nil {
		t.Fatalf("second extract: %v", err)
	}
	second, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	// Rewriting an unchanged helper on every agent start would churn the file
	// and, on macOS, risks invalidating the Screen Recording grant the user
	// already gave it.
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("an unchanged helper was rewritten")
	}
}

func TestExtractReplacesAStaleFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "helper")

	if _, err := extractTo(dst, []byte("old version")); err != nil {
		t.Fatalf("writing the old version: %v", err)
	}

	// An agent upgrade carries a new helper; the old one must not survive.
	newPayload := []byte("new version, different length")
	if _, err := extractTo(dst, newPayload); err != nil {
		t.Fatalf("writing the new version: %v", err)
	}

	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, newPayload) {
		t.Errorf("stale helper survived the upgrade: %q", onDisk)
	}
}

func TestExtractLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "helper")

	if _, err := extractTo(dst, []byte("payload")); err != nil {
		t.Fatalf("extractTo: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "helper" {
			t.Errorf("left %q behind in the install directory", e.Name())
		}
	}
}

func TestSameContentsDistinguishesBySize(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(dst, []byte("12345"), 0o755); err != nil {
		t.Fatal(err)
	}

	if same, err := sameContents(dst, []byte("12345")); err != nil || !same {
		t.Errorf("identical contents reported as different (err=%v)", err)
	}
	if same, err := sameContents(dst, []byte("1234")); err != nil || same {
		t.Errorf("shorter payload reported as identical (err=%v)", err)
	}
	if same, err := sameContents(dst, []byte("54321")); err != nil || same {
		t.Errorf("same-length different payload reported as identical (err=%v)", err)
	}
}
