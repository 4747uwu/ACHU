package httpx

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestLiveFailoverToRealFallback is the §12 dead-primary check, as a test so
// it runs with the real code rather than a copy of it: the primary is a
// guaranteed-dead TEST-NET-1 address, the fallback is the real backend host.
//
// A 400 from the fallback is the PROOF — it can only come from a live server,
// so the request genuinely moved hosts. Skipped when offline.
func TestLiveFailoverToRealFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	f := NewFailover("http://192.0.2.1:9/api", "https://achyu.xcentic.com/api")
	t.Logf("bases: %v", f.Bases())

	client := &http.Client{Timeout: 30 * time.Second}
	res, err := f.PostJSON(context.Background(), client,
		"http://192.0.2.1:9/api/auth/lab-login", []byte(`{}`), nil)

	if res.Status == 0 {
		t.Skipf("no host answered (offline?): %v", err)
	}
	t.Logf("answered by : %s", res.URL)
	t.Logf("status      : %d", res.Status)
	t.Logf("body        : %.90s", string(res.Body))
	t.Logf("active base : %s", f.Active())

	if res.URL != "https://achyu.xcentic.com/api/auth/lab-login" {
		t.Fatalf("answered by %q — the request did not move to the fallback", res.URL)
	}
	if f.Active() != "https://achyu.xcentic.com/api" {
		t.Fatalf("Active() = %q, want the fallback base", f.Active())
	}
}
