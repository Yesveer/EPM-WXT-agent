package agent

import (
	"testing"
	"time"
)

// now is fixed so the year-rollover logic is deterministic.
var whoNow = time.Date(2026, time.September, 26, 19, 0, 0, 0, time.Local)

func TestParseWhoLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantUser string
		wantTTY  string
		wantProt string
		wantIP   string
		wantTime time.Time
	}{
		{
			name:     "macOS console",
			line:     "yesveer          console      Sep 16 12:54",
			wantUser: "yesveer",
			wantTTY:  "console",
			wantProt: "console",
			wantTime: time.Date(2026, time.September, 16, 12, 54, 0, 0, time.Local),
		},
		{
			name:     "macOS local terminal",
			line:     "yesveer          ttys000      Sep 26 18:51",
			wantUser: "yesveer",
			wantTTY:  "ttys000",
			wantProt: "console",
			wantTime: time.Date(2026, time.September, 26, 18, 51, 0, 0, time.Local),
		},
		{
			name:     "macOS ssh with source",
			line:     "kaal             ttys001      Sep 26 18:52 (203.0.113.5)",
			wantUser: "kaal",
			wantTTY:  "ttys001",
			wantProt: "ssh",
			wantIP:   "203.0.113.5",
			wantTime: time.Date(2026, time.September, 26, 18, 52, 0, 0, time.Local),
		},
		{
			name:     "linux ssh with source",
			line:     "kaal  pts/3    2026-08-12 15:46 (203.0.113.5)",
			wantUser: "kaal",
			wantTTY:  "pts/3",
			wantProt: "ssh",
			wantIP:   "203.0.113.5",
			wantTime: time.Date(2026, time.August, 12, 15, 46, 0, 0, time.Local),
		},
		{
			name:     "linux local tty",
			line:     "kaal  tty1     2026-08-12 09:00",
			wantUser: "kaal",
			wantTTY:  "tty1",
			wantProt: "console",
			wantTime: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.Local),
		},
		{
			// An X display is the local screen, not a network address — it
			// must not be reported as a remote source.
			name:     "linux X display is not a remote host",
			line:     "kaal  tty2     2026-08-12 09:00 (:0)",
			wantUser: "kaal",
			wantTTY:  "tty2",
			wantProt: "console",
			wantIP:   "",
			wantTime: time.Date(2026, time.August, 12, 9, 0, 0, 0, time.Local),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseWhoLine(tc.line, whoNow)
			if !ok {
				t.Fatal("line was not parsed")
			}
			if got.OSUser != tc.wantUser {
				t.Errorf("user = %q, want %q", got.OSUser, tc.wantUser)
			}
			if got.Line != tc.wantTTY {
				t.Errorf("tty = %q, want %q", got.Line, tc.wantTTY)
			}
			if got.Protocol != tc.wantProt {
				t.Errorf("protocol = %q, want %q", got.Protocol, tc.wantProt)
			}
			if got.SourceIP != tc.wantIP {
				t.Errorf("source = %q, want %q", got.SourceIP, tc.wantIP)
			}
			// The timestamp is the part that silently degraded before: a failed
			// parse falls back to "now", so every login looked like it had just
			// happened.
			if !got.LoginTime.Equal(tc.wantTime) {
				t.Errorf("login time = %s, want %s", got.LoginTime, tc.wantTime)
			}
		})
	}
}

func TestParseWhoLineRejectsJunk(t *testing.T) {
	for _, line := range []string{"", "   ", "onlyonefield"} {
		if _, ok := parseWhoLine(line, whoNow); ok {
			t.Errorf("parsed %q as a session", line)
		}
	}
}

// macOS omits the year. A December login read in January would otherwise be
// dated eleven months into the future.
func TestParseWhoTimeHandlesYearRollover(t *testing.T) {
	jan := time.Date(2027, time.January, 5, 10, 0, 0, 0, time.Local)

	got, ok := parseWhoTime([]string{"Dec", "28", "22:15"}, jan)
	if !ok {
		t.Fatal("December timestamp was not parsed")
	}
	want := time.Date(2026, time.December, 28, 22, 15, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("time = %s, want %s (the previous year)", got, want)
	}
}

func TestParseWhoTimeRejectsUnknownFormats(t *testing.T) {
	for _, fields := range [][]string{
		{},
		{"only"},
		{"not", "a", "time"},
	} {
		if _, ok := parseWhoTime(fields, whoNow); ok {
			t.Errorf("accepted %v as a timestamp", fields)
		}
	}
}
