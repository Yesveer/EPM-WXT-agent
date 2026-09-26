package clipboard

import (
	"runtime"
	"strings"
	"testing"
)

func TestTruncateKeepsUTF8Intact(t *testing.T) {
	// A multi-byte rune straddling the limit must not be cut in half — a
	// severed sequence would travel as invalid UTF-8 and render as garbage on
	// the other side.
	long := strings.Repeat("é", maxTextBytes) // 2 bytes each
	got := truncate(long)

	if len(got) > maxTextBytes {
		t.Fatalf("truncated length %d exceeds the %d byte limit", len(got), maxTextBytes)
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("truncation produced an invalid rune at byte %d", i)
		}
	}
	if !strings.HasSuffix(got, "é") {
		t.Error("truncation cut a rune in half")
	}
}

func TestTruncateLeavesShortTextAlone(t *testing.T) {
	const s = "hello"
	if got := truncate(s); got != s {
		t.Errorf("truncate(%q) = %q", s, got)
	}
}

// TestRoundTrip exercises the real system clipboard. It is skipped where there
// is no implementation, and deliberately restores whatever was there first so
// running the tests does not eat the developer's clipboard.
func TestRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no clipboard implementation on " + runtime.GOOS)
	}

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	original, err := c.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	t.Cleanup(func() {
		if original != "" {
			_ = c.Write(original)
		}
	})

	const want = "wxt-agent clipboard round trip ✓ Ünïcödé"
	if err := c.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := c.Read()
	if err != nil {
		t.Fatalf("Read back: %v", err)
	}
	if got != want {
		t.Errorf("read back %q, want %q", got, want)
	}

	// Our own write must not register as a local change, or the poller would
	// echo the viewer's paste straight back at it.
	if c.Changed() {
		t.Error("Changed() reported true immediately after our own Write")
	}
}

func TestChangedDetectsAnExternalWrite(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no clipboard implementation on " + runtime.GOOS)
	}

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	original, _ := c.Read()
	t.Cleanup(func() {
		if original != "" {
			_ = c.Write(original)
		}
	})

	// A second handle stands in for another application copying something.
	other, err := New()
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	defer other.Close()

	if err := other.Write("changed by someone else"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !c.Changed() {
		t.Error("Changed() missed a write made outside this handle")
	}
}
