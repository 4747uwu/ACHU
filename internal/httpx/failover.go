package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
)

// apiCooldown is how long a backend host stays demoted after it fails, and
// also how often a failover may be logged.
//
// Short enough that a host coming back is picked up again within one study's
// worth of activity, long enough that a host which is genuinely down is not
// re-dialled — and re-waited-on — by every single request.
const apiCooldown = 2 * time.Minute

// Failover picks which backend API host to talk to.
//
// The backend runs the same application on more than one host. When the
// preferred host stops answering — DNS poisoned at the site, the host itself
// down, the site's ISP blackholing it — requests should go to another host
// instead of failing, and should go back to the preferred one once it recovers.
//
// # Why every method is nil-safe
//
// Most sites configure no fallback at all, and a *Failover is threaded through
// call sites that already have plenty of error paths. Making nil behave as
// "no failover configured, just use the URL I gave you" means callers never
// need a nil check, so a forgotten one cannot turn a working single-host site
// into a panic.
type Failover struct {
	primary   string
	fallbacks []string

	mu          sync.Mutex
	failedUntil map[string]time.Time
	active      string
	lastWarn    time.Time
}

// Result is what a host answered with.
type Result struct {
	URL    string // the URL that answered
	Status int
	Body   []byte
}

// NewFailover returns a Failover preferring primary, then fallbacks in order.
//
// It returns nil when primary is empty or blank: nil is the fully supported
// "nothing configured" value, so callers can pass a config field straight in
// without validating it first.
func NewFailover(primary string, fallbacks ...string) *Failover {
	primary = cleanBase(primary)
	if primary == "" {
		return nil
	}

	f := &Failover{
		primary:     primary,
		failedUntil: make(map[string]time.Time),
		active:      primary,
	}

	// Duplicates are dropped case-insensitively: a config that lists the same
	// host twice — often with different capitalisation, or with and without a
	// trailing slash — would otherwise make every failover dial the dead host
	// twice and double the time before a working host is reached.
	seen := map[string]bool{strings.ToLower(primary): true}
	for _, b := range fallbacks {
		b = cleanBase(b)
		if b == "" || seen[strings.ToLower(b)] {
			continue
		}
		seen[strings.ToLower(b)] = true
		f.fallbacks = append(f.fallbacks, b)
	}
	return f
}

// cleanBase trims whitespace and any trailing slashes, so that base + rest
// never produces a doubled slash.
func cleanBase(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// Bases returns the configured bases, primary first.
func (f *Failover) Bases() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, 1+len(f.fallbacks))
	out = append(out, f.primary)
	out = append(out, f.fallbacks...)
	return out
}

// Active reports the base that last answered — what the UI and the logs should
// say we are currently talking to. Before anything has answered it is the
// primary.
func (f *Failover) Active() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

// cutPrefixFold reports whether s starts with prefix, ignoring case, and
// returns the remainder.
//
// Go 1.20 — the toolchain this binary ships with, for Windows 7 — has no
// strings.CutPrefix, hence the hand-rolled version.
//
// The match must end on a path boundary: '/', '?', or the end of the string.
// A plain prefix test would let base "https://host/api" swallow
// "https://host/apiv2/thing" and rewrite it onto a fallback host, where that
// path may mean something entirely different, or nothing at all.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	rest := s[len(prefix):]
	if rest == "" || rest[0] == '/' || rest[0] == '?' {
		return rest, true
	}
	return "", false
}

// Candidates returns the URLs to try for target, best first.
//
// If target does not sit under the primary base it is returned unchanged and
// alone. That is deliberate: callers pass URLs that are not ours at all — a
// site-local PACS, a customer-supplied webhook, an absolute URL handed back by
// the backend — and silently re-pointing one of those at our own hosts would
// send data somewhere the caller never asked for.
func (f *Failover) Candidates(target string) []string {
	if f == nil {
		return []string{target}
	}

	rest, ok := cutPrefixFold(target, f.primary)
	if !ok {
		return []string{target}
	}

	bases := f.Bases()

	f.mu.Lock()
	now := time.Now()
	// Hosts in cooldown move to the BACK, they are never removed. If the whole
	// backend is unreachable for a while every host ends up cooling, and an
	// implementation that filtered them out would have nothing left to try and
	// would fail requests that a plain retry would have served.
	ready := make([]string, 0, len(bases))
	cooling := make([]string, 0, len(bases))
	for _, b := range bases {
		if until, bad := f.failedUntil[b]; bad && now.Before(until) {
			cooling = append(cooling, b+rest)
		} else {
			ready = append(ready, b+rest)
		}
	}
	f.mu.Unlock()

	return append(ready, cooling...)
}

// baseOf finds which configured base url belongs to, or "" if none does.
func (f *Failover) baseOf(url string) string {
	if f == nil {
		return ""
	}
	for _, b := range f.Bases() {
		if _, ok := cutPrefixFold(url, b); ok {
			return b
		}
	}
	return ""
}

// MarkFailed puts the host behind url into cooldown. url may be a base or a
// full request URL; anything not under a configured base is ignored, because a
// foreign URL failing says nothing about our hosts.
func (f *Failover) MarkFailed(url string) {
	if f == nil {
		return
	}
	base := f.baseOf(url)
	if base == "" {
		return
	}
	f.mu.Lock()
	f.failedUntil[base] = time.Now().Add(apiCooldown)
	f.mu.Unlock()
}

// MarkOK clears any cooldown on the host behind url and records it as active.
// A host that answers is trusted again immediately — waiting out the remaining
// cooldown would keep a recovered primary demoted for no reason.
func (f *Failover) MarkOK(url string) {
	if f == nil {
		return
	}
	base := f.baseOf(url)
	if base == "" {
		return
	}
	f.mu.Lock()
	delete(f.failedUntil, base)
	f.active = base
	f.mu.Unlock()
}

// noteFailover logs, at most once per cooldown, that the preferred host was not
// the one that answered.
//
// Rate-limited because a down primary makes this true for EVERY request, and
// the log file is itself shipped to the backend.
func (f *Failover) noteFailover(wanted, used string) {
	if f == nil || wanted == used {
		return
	}

	f.mu.Lock()
	quiet := !f.lastWarn.IsZero() && time.Since(f.lastWarn) < apiCooldown
	if !quiet {
		f.lastWarn = time.Now()
	}
	f.mu.Unlock()
	if quiet {
		return
	}

	log.L().Warn("backend API did not answer — using the fallback host",
		"wanted", wanted,
		"used", used,
	)
}

// preferred returns the URL target would have on the primary base, which is
// what we WANTED to reach regardless of the order Candidates produced.
func (f *Failover) preferred(target string) string {
	if f == nil {
		return target
	}
	if rest, ok := cutPrefixFold(target, f.primary); ok {
		return f.primary + rest
	}
	return target
}

// PostJSON POSTs body as JSON to targetURL, trying each candidate host until
// one answers.
//
// decorate, if non-nil, is called on every request before it is sent — that is
// where auth headers, idempotency keys and tracing go. It runs on EVERY
// attempt, not just the first: a retry that lost the Authorization header would
// come back 401 and look like a credentials problem rather than a failover.
//
// A non-2xx status is returned WITH a non-nil error, so callers that only check
// err stay correct while callers that need the status and body can use them.
func (f *Failover) PostJSON(ctx context.Context, client *http.Client, targetURL string, body []byte, decorate func(*http.Request)) (Result, error) {
	if client == nil {
		client = http.DefaultClient
	}

	cands := f.Candidates(targetURL)
	wanted := f.preferred(targetURL)

	var lastErr error
	for _, u := range cands {
		// A fresh request per attempt. An http.Request's body is a one-shot
		// reader, so reusing the request object would send an EMPTY body to the
		// second host; bytes.NewReader over the same slice re-reads it from the
		// start and costs nothing.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			lastErr = fmt.Errorf("build request for %s: %w", u, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if decorate != nil {
			decorate(req)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			f.MarkFailed(u)
			// The caller gave up — shutdown, or its own deadline. Trying the next
			// host would fail identically and only delay the exit.
			if ctx.Err() != nil {
				return Result{}, err
			}
			continue
		}

		payload, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 500 {
			// The host is up but the app behind it is not. Another host may have a
			// working database or disk, so this is worth failing over.
			lastErr = fmt.Errorf("POST %s: %s", u, resp.Status)
			f.MarkFailed(u)
			continue
		}

		// Anything else — 2xx or 4xx — is this host's final answer.
		//
		// A 4xx is NOT retried. Both hosts run the same application against the
		// same data, so a 401 or a 422 would come back identical from the next
		// host; all a retry could achieve is registering the same study twice on
		// a request that in fact succeeded server-side.
		f.MarkOK(u)
		f.noteFailover(wanted, u)

		res := Result{URL: u, Status: resp.StatusCode, Body: payload}
		switch {
		case readErr != nil:
			return res, fmt.Errorf("POST %s: reading response: %w", u, readErr)
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			return res, fmt.Errorf("POST %s: %s", u, resp.Status)
		}
		return res, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no candidate URL to try")
	}
	// Status stays 0: no host produced one, and a caller must be able to tell
	// "nobody answered" from "answered with something".
	return Result{}, fmt.Errorf("no backend host answered: %w", lastErr)
}
