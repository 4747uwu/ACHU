package config

import (
	"strings"
	"testing"
)

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPeerCandidates(t *testing.T) {
	cases := []struct {
		name string
		peer PeerConfig
		want []string
	}{
		{
			name: "auto keeps preference order",
			peer: PeerConfig{URL: "https://r1", FallbackURLs: []string{"https://r2"}},
			want: []string{"https://r1", "https://r2"},
		},
		{
			name: "empty selection means auto",
			peer: PeerConfig{URL: "https://r1", FallbackURLs: []string{"https://r2"}, Selection: ""},
			want: []string{"https://r1", "https://r2"},
		},
		{
			// A pin must not merely deprioritise the others: the whole point is
			// that no failure can move off it.
			name: "manual drops the fallbacks entirely",
			peer: PeerConfig{URL: "https://r2", FallbackURLs: []string{"https://r1"}, Selection: "manual"},
			want: []string{"https://r2"},
		},
		{
			// "https://r/" and "https://r" are one server — the transfer client
			// trims the slash before appending its path. Two entries would look
			// like two healthy receivers.
			name: "dedupes on slash and case",
			peer: PeerConfig{URL: "https://R1/", FallbackURLs: []string{"https://r1", "HTTPS://R1/"}},
			want: []string{"https://R1/"},
		},
		{
			name: "blanks are dropped",
			peer: PeerConfig{URL: "https://r1", FallbackURLs: []string{"", "   ", "https://r2"}},
			want: []string{"https://r1", "https://r2"},
		},
		{
			name: "single receiver config is unchanged",
			peer: PeerConfig{URL: "https://only"},
			want: []string{"https://only"},
		},
		{
			name: "no url at all yields nothing",
			peer: PeerConfig{},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.peer.Candidates(); !eq(got, c.want) {
				t.Fatalf("Candidates() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPeerIsManual(t *testing.T) {
	for _, c := range []struct {
		sel  string
		want bool
	}{
		{"", false}, {"auto", false}, {"AUTO", false},
		{"manual", true}, {"Manual", true}, {" manual ", true},
	} {
		if got := (PeerConfig{Selection: c.sel}).IsManual(); got != c.want {
			t.Fatalf("IsManual(%q) = %v, want %v", c.sel, got, c.want)
		}
	}
}

// A typo must fail loudly. Guessing would silently give a site behaviour its
// operator did not choose.
func TestPeerSelectionValidated(t *testing.T) {
	base := func(sel string) Config {
		c := Default()
		c.LabID = "LAB"
		c.OrgID = "ORG"
		c.Peer.URL = "https://r1"
		c.Peer.Name = "R"
		c.Peer.Protocol = "stow-rs"
		c.Peer.Selection = sel
		return c
	}
	for _, sel := range []string{"", "auto", "manual"} {
		if err := base(sel).Validate(); err != nil {
			t.Fatalf("selection %q rejected: %v", sel, err)
		}
	}
	err := base("automatic").Validate()
	if err == nil {
		t.Fatal("selection \"automatic\" was accepted")
	}
	if !strings.Contains(err.Error(), "peer.selection") {
		t.Fatalf("error %q does not name the field", err)
	}
}
