package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// deadHost is in TEST-NET-1 (RFC 5737): reserved for documentation, guaranteed
// never to be a real host, so a connection to it can only fail. Port 9 is the
// discard port, which nothing on a developer machine listens on either.
const deadHost = "http://192.0.2.1:9/api"
const deadHost2 = "http://192.0.2.2:9/api"

// quickClient keeps an unreachable-host test from waiting out the OS connect
// timeout.
func quickClient() *http.Client {
	return &http.Client{Timeout: 750 * time.Millisecond}
}

// countingServer answers with status/body and counts the requests it saw.
func countingServer(t *testing.T, status int, body string, hits *int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCandidatesRewritesTheBase(t *testing.T) {
	f := NewFailover("https://a.example/api", "https://b.example/api")

	got := f.Candidates("https://a.example/api/studies?x=1&y=2")
	want := []string{
		"https://a.example/api/studies?x=1&y=2",
		"https://b.example/api/studies?x=1&y=2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}

	// The base itself is under the base: the prefix ends at end-of-string.
	got = f.Candidates("https://a.example/api")
	want = []string{"https://a.example/api", "https://b.example/api"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bare base: candidates = %q, want %q", got, want)
	}

	// A query string directly after the base is a boundary too.
	got = f.Candidates("https://a.example/api?ping=1")
	want = []string{"https://a.example/api?ping=1", "https://b.example/api?ping=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query at boundary: candidates = %q, want %q", got, want)
	}

	// Scheme and host are case-insensitive in URLs, so a differently cased
	// target must still be recognised as ours.
	got = f.Candidates("HTTPS://A.EXAMPLE/api/studies")
	want = []string{"https://a.example/api/studies", "https://b.example/api/studies"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed case: candidates = %q, want %q", got, want)
	}
}

func TestCandidatesLeavesForeignURLsAlone(t *testing.T) {
	f := NewFailover("https://a.example/api", "https://b.example/api")

	for _, target := range []string{
		"https://customer-pacs.local:8042/dicom-web/studies", // not ours at all
		"https://a.example/apiv2/thing",                      // same host, NOT under /api
		"https://a.example/apix",                             // prefix match with no boundary
		"https://other.example/api/studies",                  // right path, wrong host
	} {
		got := f.Candidates(target)
		if !reflect.DeepEqual(got, []string{target}) {
			t.Errorf("Candidates(%q) = %q, want the URL unchanged and alone", target, got)
		}
	}
}

func TestNewFailoverCleansInput(t *testing.T) {
	f := NewFailover("https://a.example/api/",
		"https://a.example/api",    // duplicate of the primary
		"   ",                      // blank
		"HTTPS://A.EXAMPLE/API",    // duplicate, different case
		"https://b.example/api/",   // trailing slash
		"https://b.example/api",    // duplicate of the previous fallback
		"https://c.example/api///", // several trailing slashes
	)

	got := f.Bases()
	want := []string{"https://a.example/api", "https://b.example/api", "https://c.example/api"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Bases() = %q, want %q", got, want)
	}

	if f.Active() != "https://a.example/api" {
		t.Errorf("Active() = %q, want the primary before anything has answered", f.Active())
	}

	for _, blank := range []string{"", "   ", "\t\n"} {
		if got := NewFailover(blank, "https://b.example/api"); got != nil {
			t.Errorf("NewFailover(%q) = %v, want nil", blank, got)
		}
	}
}

func TestNilFailoverStillPosts(t *testing.T) {
	var f *Failover // the "nothing configured" value callers pass straight through

	if f.Bases() != nil {
		t.Errorf("nil Bases() = %v, want nil", f.Bases())
	}
	if f.Active() != "" {
		t.Errorf("nil Active() = %q, want empty", f.Active())
	}
	if got := f.Candidates("https://x.example/api/thing"); !reflect.DeepEqual(got, []string{"https://x.example/api/thing"}) {
		t.Errorf("nil Candidates() = %q, want the target alone", got)
	}
	// Must not panic.
	f.MarkFailed("https://x.example/api")
	f.MarkOK("https://x.example/api")

	var hits int32
	s := countingServer(t, http.StatusOK, `{"ok":true}`, &hits)

	res, err := f.PostJSON(context.Background(), quickClient(), s.URL+"/studies", []byte(`{"a":1}`), nil)
	if err != nil {
		t.Fatalf("nil PostJSON: %v", err)
	}
	if res.Status != http.StatusOK || string(res.Body) != `{"ok":true}` || res.URL != s.URL+"/studies" {
		t.Fatalf("nil PostJSON = %+v", res)
	}
	if hits != 1 {
		t.Fatalf("server saw %d requests, want 1", hits)
	}
}

func TestUnreachablePrimaryFallsOver(t *testing.T) {
	var hits int32
	good := countingServer(t, http.StatusOK, "served", &hits)

	f := NewFailover(deadHost, good.URL+"/api")

	res, err := f.PostJSON(context.Background(), quickClient(), deadHost+"/studies", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if want := good.URL + "/api/studies"; res.URL != want {
		t.Fatalf("answered by %q, want %q", res.URL, want)
	}
	if res.Status != http.StatusOK || string(res.Body) != "served" {
		t.Fatalf("result = %+v", res)
	}
	if hits != 1 {
		t.Fatalf("fallback saw %d requests, want 1", hits)
	}
	if want := good.URL + "/api"; f.Active() != want {
		t.Fatalf("Active() = %q, want %q", f.Active(), want)
	}
}

func TestServerErrorFallsOverButClientErrorDoesNot(t *testing.T) {
	t.Run("500 fails over", func(t *testing.T) {
		var badHits, goodHits int32
		bad := countingServer(t, http.StatusInternalServerError, "boom", &badHits)
		good := countingServer(t, http.StatusOK, "served", &goodHits)

		f := NewFailover(bad.URL+"/api", good.URL+"/api")
		res, err := f.PostJSON(context.Background(), quickClient(), bad.URL+"/api/studies", []byte(`{}`), nil)
		if err != nil {
			t.Fatalf("PostJSON: %v", err)
		}
		if want := good.URL + "/api/studies"; res.URL != want {
			t.Fatalf("answered by %q, want %q", res.URL, want)
		}
		if badHits != 1 || goodHits != 1 {
			t.Fatalf("hits: bad=%d good=%d, want 1 and 1", badHits, goodHits)
		}
	})

	t.Run("401 is passed through", func(t *testing.T) {
		var badHits, goodHits int32
		bad := countingServer(t, http.StatusUnauthorized, `{"error":"bad token"}`, &badHits)
		good := countingServer(t, http.StatusOK, "served", &goodHits)

		f := NewFailover(bad.URL+"/api", good.URL+"/api")
		res, err := f.PostJSON(context.Background(), quickClient(), bad.URL+"/api/studies", []byte(`{}`), nil)
		if err == nil {
			t.Fatal("PostJSON: want an error for 401")
		}
		if res.Status != http.StatusUnauthorized {
			t.Fatalf("Status = %d, want 401", res.Status)
		}
		if string(res.Body) != `{"error":"bad token"}` {
			t.Fatalf("Body = %q, want the server's body passed through", res.Body)
		}
		if goodHits != 0 {
			t.Fatalf("fallback saw %d requests, want 0 — a 4xx must not be retried", goodHits)
		}
		if badHits != 1 {
			t.Fatalf("primary saw %d requests, want 1", badHits)
		}
	})
}

func TestFailedBaseIsTriedLastUntilCooldownEnds(t *testing.T) {
	var aHits, bHits int32
	a := countingServer(t, http.StatusOK, "a", &aHits)
	b := countingServer(t, http.StatusOK, "b", &bHits)

	f := NewFailover(a.URL+"/api", b.URL+"/api")
	target := a.URL + "/api/studies"

	f.MarkFailed(a.URL + "/api")

	// Demoted, not dropped: still present, just last.
	got := f.Candidates(target)
	want := []string{b.URL + "/api/studies", a.URL + "/api/studies"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after MarkFailed: candidates = %q, want %q", got, want)
	}

	res, err := f.PostJSON(context.Background(), quickClient(), target, []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if string(res.Body) != "b" {
		t.Fatalf("answered by %q (%s), want the fallback", res.URL, res.Body)
	}
	if aHits != 0 {
		t.Fatalf("cooling host saw %d requests, want 0", aHits)
	}

	// Both cooling: everything is still tried rather than nothing, and with
	// nothing left to promote the list is back in plain preference order.
	f.MarkFailed(a.URL + "/api")
	f.MarkFailed(b.URL + "/api")
	allCooling := []string{a.URL + "/api/studies", b.URL + "/api/studies"}
	if got := f.Candidates(target); !reflect.DeepEqual(got, allCooling) {
		t.Fatalf("all cooling: candidates = %q, want all of them in preference order %q", got, allCooling)
	}

	// A host that answers is trusted again at once, so the order is restored.
	f.MarkOK(a.URL + "/api/studies")
	got = f.Candidates(target)
	want = []string{a.URL + "/api/studies", b.URL + "/api/studies"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after MarkOK: candidates = %q, want %q", got, want)
	}
	if f.Active() != a.URL+"/api" {
		t.Fatalf("Active() = %q, want %q", f.Active(), a.URL+"/api")
	}
}

func TestNoHostAnswers(t *testing.T) {
	f := NewFailover(deadHost, deadHost2)

	res, err := f.PostJSON(context.Background(), quickClient(), deadHost+"/studies", []byte(`{}`), nil)
	if err == nil {
		t.Fatal("want an error when no host answers")
	}
	if res.Status != 0 {
		t.Fatalf("Status = %d, want 0 so callers can tell nobody answered", res.Status)
	}
	if res.URL != "" || res.Body != nil {
		t.Fatalf("result = %+v, want the zero Result", res)
	}
	if !strings.Contains(err.Error(), "no backend host answered") {
		t.Fatalf("error = %v, want it to say no host answered", err)
	}
}

func TestDecorateIsAppliedToEveryAttempt(t *testing.T) {
	var (
		attempts int32
		withAuth int32
		jsonCT   int32
	)
	record := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		if r.Header.Get("Authorization") == "Bearer t0ken" {
			atomic.AddInt32(&withAuth, 1)
		}
		if r.Header.Get("Content-Type") == "application/json" {
			atomic.AddInt32(&jsonCT, 1)
		}
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(w, r)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(w, r)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	f := NewFailover(bad.URL+"/api", good.URL+"/api")
	decorate := func(req *http.Request) { req.Header.Set("Authorization", "Bearer t0ken") }

	if _, err := f.PostJSON(context.Background(), quickClient(), bad.URL+"/api/studies", []byte(`{}`), decorate); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if withAuth != 2 {
		t.Fatalf("%d of %d attempts carried the auth header, want all of them", withAuth, attempts)
	}
	if jsonCT != 2 {
		t.Fatalf("%d of %d attempts carried the JSON content type, want all of them", jsonCT, attempts)
	}
}

func TestBodyIsResentIntact(t *testing.T) {
	const payload = `{"study":"1.2.840.113619.2.55","images":417}`

	var (
		mu     sync.Mutex
		bodies []string
	)
	handler := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
			w.WriteHeader(status)
		}
	}
	bad := httptest.NewServer(handler(http.StatusServiceUnavailable))
	defer bad.Close()
	good := httptest.NewServer(handler(http.StatusOK))
	defer good.Close()

	f := NewFailover(bad.URL+"/api", good.URL+"/api")
	if _, err := f.PostJSON(context.Background(), quickClient(), bad.URL+"/api/studies", []byte(payload), nil); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("saw %d bodies, want 2", len(bodies))
	}
	for i, b := range bodies {
		if b != payload {
			// The retry getting an empty body is the classic reused-request bug.
			t.Errorf("attempt %d body = %q, want %q", i+1, b, payload)
		}
	}
}
