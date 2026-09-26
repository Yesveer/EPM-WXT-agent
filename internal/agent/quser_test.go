package agent

import "testing"

// quser's columns are fixed-width and shift when a column is blank, which is
// why the parser locates the numeric session ID instead of counting fields.
// These cases are the layouts that actually appear in the wild.
func TestParseQuserLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantUser string
		wantProt string
		wantLine string
		wantOK   bool
	}{
		{
			name:     "current console session",
			line:     `>kaal                 console             1  Active      none   9/26/2026 1:03 PM`,
			wantUser: "kaal",
			wantProt: "console",
			wantLine: "console#1",
			wantOK:   true,
		},
		{
			name:     "remote desktop login",
			line:     ` admin                rdp-tcp#2           3  Active          .  9/26/2026 2:11 PM`,
			wantUser: "admin",
			wantProt: "rdp",
			wantLine: "rdp-tcp#2#3",
			wantOK:   true,
		},
		{
			// A disconnected session has no SESSIONNAME, so every later column
			// shifts one to the left. Counting fields would misread the user.
			name:     "disconnected session has no session name",
			line:     ` kaal                                      4  Disc         1:20  9/26/2026 9:15 AM`,
			wantUser: "kaal",
			wantProt: "console",
			wantLine: "#4",
			wantOK:   true,
		},
		{
			name:   "header row is not a session",
			line:   ` USERNAME              SESSIONNAME        ID  STATE   IDLE TIME  LOGON TIME`,
			wantOK: false,
		},
		{
			name:   "blank line",
			line:   `   `,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseQuserLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (parsed %+v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.OSUser != tc.wantUser {
				t.Errorf("user = %q, want %q", got.OSUser, tc.wantUser)
			}
			if got.Protocol != tc.wantProt {
				t.Errorf("protocol = %q, want %q", got.Protocol, tc.wantProt)
			}
			if got.Line != tc.wantLine {
				t.Errorf("line = %q, want %q", got.Line, tc.wantLine)
			}
			if got.LoginTime.IsZero() {
				t.Error("login time is zero")
			}
		})
	}
}

// The header row must not parse as a session, or every machine would show a
// permanent "USERNAME" intruder.
func TestParseQuserLineRejectsHeader(t *testing.T) {
	if _, ok := parseQuserLine(` USERNAME  SESSIONNAME  ID  STATE  IDLE TIME  LOGON TIME`); ok {
		t.Fatal("the header row was parsed as a login session")
	}
}

func TestParseQuserTimeAcceptsCommonLocales(t *testing.T) {
	for _, s := range []string{
		"9/26/2026 1:03 PM",
		"9/26/2026 13:03",
		"26/09/2026 13:03",
		"2026-09-26 13:03",
	} {
		if _, err := parseQuserTime(s); err != nil {
			t.Errorf("could not parse %q: %v", s, err)
		}
	}
}
