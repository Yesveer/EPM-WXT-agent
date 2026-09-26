package remotecontrol

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Yesveer/wxt-agent/internal/remotecontrol/helperbin"
)

// write creates an empty file so FindHelper's os.Stat succeeds.
func write(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	return p
}

func TestFindHelperAcceptsTheCanonicalName(t *testing.T) {
	dir := t.TempDir()
	want := write(t, dir, CanonicalHelperName())

	got, err := FindHelper(dir)
	if err != nil {
		t.Fatalf("FindHelper: %v", err)
	}
	if got != want {
		t.Errorf("FindHelper = %q, want %q", got, want)
	}
}

// TestFindHelperAcceptsTheDownloadedName is the regression test for the bug
// this lookup exists to fix: the portal serves the helper with an architecture
// suffix, so somebody who downloads both binaries into one folder has
// "wxt-agent-session-<arch>" sitting next to the agent, not the canonical name.
// Rejecting that is rejecting a working install over a filename.
func TestFindHelperAcceptsTheDownloadedName(t *testing.T) {
	dir := t.TempDir()

	var downloaded string
	switch runtime.GOOS {
	case "windows":
		downloaded = "wxt-agent-session-" + runtime.GOARCH + ".exe"
	case "darwin":
		downloaded = "wxt-agent-session-macos-" + runtime.GOARCH
	default:
		downloaded = "wxt-agent-session-" + runtime.GOOS + "-" + runtime.GOARCH
	}
	want := write(t, dir, downloaded)

	got, err := FindHelper(dir)
	if err != nil {
		t.Fatalf("FindHelper did not accept the published download name %q: %v", downloaded, err)
	}
	if got != want {
		t.Errorf("FindHelper = %q, want %q", got, want)
	}
}

func TestFindHelperPrefersTheCanonicalName(t *testing.T) {
	dir := t.TempDir()
	// Both present — an install that ran once and a stray download. The
	// installed copy is the one the installer manages, so it wins.
	canonical := write(t, dir, CanonicalHelperName())
	if runtime.GOOS == "windows" {
		write(t, dir, "wxt-agent-session-"+runtime.GOARCH+".exe")
	} else {
		write(t, dir, "wxt-agent-session-macos-"+runtime.GOARCH)
	}

	got, err := FindHelper(dir)
	if err != nil {
		t.Fatalf("FindHelper: %v", err)
	}
	if got != canonical {
		t.Errorf("FindHelper = %q, want the canonical %q", got, canonical)
	}
}

// TestFindHelperErrorNamesEveryPathTried keeps the failure actionable. The
// original error said only "not found at <one path>", which sent an operator
// hunting for a file that was present under a different name a directory away.
func TestFindHelperErrorNamesEveryPathTried(t *testing.T) {
	dir := t.TempDir()

	_, err := FindHelper(dir)
	if err == nil {
		t.Fatal("expected an error for an empty directory")
	}
	msg := err.Error()

	if !strings.Contains(msg, dir) {
		t.Errorf("error does not mention the directory searched:\n%s", msg)
	}
	for _, name := range helperNames() {
		if !strings.Contains(msg, name) {
			t.Errorf("error does not mention candidate %q:\n%s", name, msg)
		}
	}
	if !strings.Contains(msg, "Packages page") {
		t.Errorf("error does not say where to get the helper:\n%s", msg)
	}
}

func TestCanonicalHelperNameMatchesPlatform(t *testing.T) {
	got := CanonicalHelperName()
	if runtime.GOOS == "windows" {
		if got != "wxt-agent-session.exe" {
			t.Errorf("canonical name = %q, want wxt-agent-session.exe", got)
		}
		return
	}
	if got != "wxt-agent-session" {
		t.Errorf("canonical name = %q, want wxt-agent-session", got)
	}
}

// TestCanonicalNameIsAccepted guards the two lists against drifting apart: an
// installer writing a name the lookup does not accept would produce an install
// that looks complete and fails at connect time.
func TestCanonicalNameIsAccepted(t *testing.T) {
	for _, n := range helperNames() {
		if n == CanonicalHelperName() {
			return
		}
	}
	t.Fatalf("CanonicalHelperName() = %q is not in helperNames() %v",
		CanonicalHelperName(), helperNames())
}

// TestEnsureHelperRefreshesAStaleHelper is the regression test for a bug that
// only shows up after a self-update: the agent replaces its own binary, but a
// helper already on disk was returned as-is, leaving a NEW agent driving the
// PREVIOUS version's helper. Nothing about that looks wrong until the two
// disagree about the IPC protocol.
//
// It runs only where a helper is really embedded, which is a Makefile build;
// a plain `go test` carries the placeholder and skips.
func TestEnsureHelperRefreshesAStaleHelper(t *testing.T) {
	if !helperbin.Available() {
		t.Skip("no helper embedded in this build (plain go build)")
	}

	dir := t.TempDir()
	stale := filepath.Join(dir, CanonicalHelperName())
	if err := os.WriteFile(stale, []byte("an older helper"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := EnsureHelper(dir)
	if err != nil {
		t.Fatalf("EnsureHelper: %v", err)
	}
	if got != stale {
		t.Errorf("path = %q, want %q", got, stale)
	}

	onDisk, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == "an older helper" {
		t.Error("the stale helper survived — an upgraded agent would run the old one")
	}
	if len(onDisk) != helperbin.Size() {
		t.Errorf("helper on disk is %d bytes, embedded is %d", len(onDisk), helperbin.Size())
	}
}

// Without an embedded helper the agent must still use whatever is installed
// beside it, rather than failing outright.
func TestEnsureHelperFallsBackToDiskWithoutAnEmbeddedCopy(t *testing.T) {
	if helperbin.Available() {
		t.Skip("this build embeds a helper, which always wins")
	}

	dir := t.TempDir()
	onDisk := filepath.Join(dir, CanonicalHelperName())
	if err := os.WriteFile(onDisk, []byte("installed helper"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := EnsureHelper(dir)
	if err != nil {
		t.Fatalf("EnsureHelper: %v", err)
	}
	if got != onDisk {
		t.Errorf("path = %q, want %q", got, onDisk)
	}
}
