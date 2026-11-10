package httpx

import "testing"

// The notifier, delivered, error-log and lab-status endpoints must all sit
// under the configured API base, or prefix rewriting silently does nothing and
// those calls never fail over — the failure mode being that everything looks
// wired up while only login actually moves hosts.
func TestRealBackendURLsRewriteToTheFallback(t *testing.T) {
	const (
		base = "https://pacs.achyutrs.com/api"
		alt  = "https://achyu.xcentic.com/api"
	)
	f := NewFailover(base, alt)

	for _, tc := range []struct{ name, url, want string }{
		{"notifier", base + "/orthanc2/instance-exe-received", alt + "/orthanc2/instance-exe-received"},
		{"delivered", base + "/orthanc2/delivered", alt + "/orthanc2/delivered"},
		{"error logs", base + "/sender/error-logs", alt + "/sender/error-logs"},
		{"lab status", base + "/sender/lab-status", alt + "/sender/lab-status"},
		{"login", base + "/auth/lab-login", alt + "/auth/lab-login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.Candidates(tc.url)
			if len(got) != 2 {
				t.Fatalf("got %d candidates %v, want 2", len(got), got)
			}
			if got[0] != tc.url {
				t.Fatalf("primary candidate = %q, want %q", got[0], tc.url)
			}
			if got[1] != tc.want {
				t.Fatalf("fallback candidate = %q, want %q", got[1], tc.want)
			}
			t.Logf("%s -> %s", got[0], got[1])
		})
	}
}
