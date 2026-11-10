package config

import "strings"

// Receiver selection.
//
// A sender can be pointed at more than one Orthanc receiver. In "auto" it
// picks a reachable one per study, preferring the first; in "manual" an
// operator has pinned one and nothing moves off it.
const (
	PeerSelectionAuto   = "auto"
	PeerSelectionManual = "manual"
)

// IsManual reports whether an operator has pinned the receiver.
//
// An empty Selection means auto: configs written before multi-receiver
// support have no such field, and those installs should keep working with the
// fallback list rather than silently pinning themselves to one receiver.
func (p PeerConfig) IsManual() bool {
	return strings.EqualFold(strings.TrimSpace(p.Selection), PeerSelectionManual)
}

// Candidates returns the receivers this config may use, most preferred first.
//
// Manual returns URL alone — FallbackURLs is ignored rather than merely
// deprioritised, because the entire point of a pin is that nothing moves off
// it, including a failure that would otherwise trigger failover.
//
// Duplicates are dropped case-insensitively and ignoring one trailing slash,
// because the transfer client trims a trailing slash before appending its
// paths: "https://r/" and "https://r" are the same server, and letting both
// through would make a single receiver look like two healthy ones.
func (p PeerConfig) Candidates() []string {
	var out []string
	seen := map[string]bool{}

	add := func(u string) {
		u = strings.TrimSpace(u)
		key := strings.ToLower(strings.TrimRight(u, "/"))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, u)
	}

	add(p.URL)
	if !p.IsManual() {
		for _, u := range p.FallbackURLs {
			add(u)
		}
	}
	return out
}
