package scp

import (
	"net"
	"testing"
)

// TestAETitleIsFullyUnrestricted pins the policy that the called AE title
// never gates an association.
//
// The title is caller-supplied and unauthenticated, so checking it buys no
// security. What it did buy, repeatedly, was a site that could not send
// because an engineer typed the destination name with a different case or an
// extra character. Every case below must associate, and the title must come
// back echoed exactly as sent.
func TestAETitleIsFullyUnrestricted(t *testing.T) {
	_, addr := startTestServer(t, TLSOptions{Enabled: false})

	cases := []struct {
		name      string
		calledAE  string
		callingAE string
	}{
		{"exact match", "ACHYU", "CT01"},
		{"a completely different name", "SOMETHING-ELSE", "CT01"},
		{"lower case", "achyu", "CT01"},
		{"mixed case", "Achyu", "CT01"},
		{"empty called title", "", "CT01"},
		{"empty calling title", "ACHYU", ""},
		{"both empty", "", ""},
		{"full 16 characters", "ABCDEFGHIJKLMNOP", "CT01"},
		{"punctuation", "ACH-YU_1.2", "CT01"},
		{"hash and dash", "X-RAY_#2", "CT01"},
		{"digits only", "12345678", "CT01"},
		{"internal spaces", "AR IH ANT", "CT01"},
		{"leading space", " ACHYU", "CT01"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			echoed := associate(t, conn, tc.calledAE, tc.callingAE)

			// The wire field is space-padded, so trailing spaces are not
			// recoverable — compare against the same trimming the parser does.
			want := trimAE(padAE(tc.calledAE))
			if echoed != want {
				t.Fatalf("A-ASSOCIATE-AC echoed called AE %q, want %q", echoed, want)
			}
		})
	}
}

// TestPadAETruncatesAtFieldWidth: an over-long title must be cut to the PS3.8
// field width rather than overflowing into the next field, which would
// corrupt every field after it in the PDU.
func TestPadAETruncatesAtFieldWidth(t *testing.T) {
	out := padAE("THIS-TITLE-IS-FAR-TOO-LONG-FOR-DICOM")
	if len(out) != 16 {
		t.Fatalf("padAE produced %d bytes, want exactly 16", len(out))
	}
	if got := string(out); got != "THIS-TITLE-IS-FA" {
		t.Fatalf("padAE truncated to %q, want %q", got, "THIS-TITLE-IS-FA")
	}
}

// TestPadAEPadsShortTitles: short titles occupy the full field, space-padded.
func TestPadAEPadsShortTitles(t *testing.T) {
	out := padAE("CT01")
	if len(out) != 16 {
		t.Fatalf("padAE produced %d bytes, want 16", len(out))
	}
	if string(out) != "CT01            " {
		t.Fatalf("padAE = %q, want %q", string(out), "CT01            ")
	}
	if trimAE(out) != "CT01" {
		t.Fatalf("trimAE(padAE(%q)) = %q", "CT01", trimAE(out))
	}
}
