// Package connectivity decides which DICOM receiver (Orthanc) to upload to,
// and whether any of them can be reached at all.
//
// The queue worker calls WaitUntilOnline before processing each upload job.
// While no receiver is reachable the worker blocks here rather than burning
// retry budget on entries that will immediately fail again.
//
// With more than one receiver configured the watcher also chooses between
// them, per study, preferring the first that answers. See ChoosePeer.
package connectivity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/httpx"
	"github.com/bharatpacs/tarang-sender/internal/log"
)

const (
	// defaultFailureCooldown is how long a receiver is passed over after it
	// fails a study. Long enough that a receiver with a real problem is not
	// retried every few seconds; short enough that a brief outage does not
	// strand every study on the fallback for the rest of the day.
	defaultFailureCooldown = 5 * time.Minute

	// probeFreshness lets a burst of studies share one probe per receiver.
	// Without it, an outage backlog releasing all at once turns into a probe
	// storm against receivers that are already struggling.
	probeFreshness = 10 * time.Second

	// chooseProbeTimeout bounds the per-study check. Deliberately shorter than
	// the background probe: a study is waiting on this answer.
	chooseProbeTimeout = 8 * time.Second

	probeInterval = 30 * time.Second

	// unreachableAfter is how many probes must fail IN A ROW before a receiver
	// is treated as down. Recovery stays immediate — one good probe restores it.
	//
	// It is not 1, and the reason is that a single probe used to be enough. The
	// probe shares the clinic's uplink with the uploads, and on a small
	// connection the uploads saturate it: a 145MB study at 3.6Mbps holds the
	// link for minutes. The probe's TCP connect then times out, uploads pause,
	// the link drains, the next probe succeeds, uploads resume and saturate it
	// again. A live site oscillated on roughly a 90-second cycle doing exactly
	// this, reporting the peer unreachable while that same peer was accepting
	// series and marking them delivered.
	//
	// Three consecutive failures at a 30s interval means a genuine outage still
	// pauses uploads inside 90 seconds, while a transient stall no longer halts
	// a pipeline that is demonstrably working.
	unreachableAfter = 3
)

// peerState is everything known about one receiver.
type peerState struct {
	reachable   bool
	fails       int       // consecutive failed probes; see unreachableAfter
	lastErr     string    // why it is unusable, "" when healthy
	lastProbe   time.Time // last time it was checked at all
	lastOK      time.Time // last time it answered
	failedUntil time.Time // cooling down after failing a study
}

// Watcher probes the configured receivers and picks one per study.
type Watcher struct {
	// PeerURL is the single-receiver fallback, used when URLs is empty so
	// configs written before multi-receiver support keep working unchanged.
	PeerURL string

	// URLs are the candidates, most preferred first.
	URLs []string

	// Manual pins URLs[0]: no failure and no outage moves off it.
	Manual bool

	Username string
	Password string

	// FailureCooldown overrides defaultFailureCooldown. Zero uses the default.
	FailureCooldown time.Duration

	mu     sync.Mutex
	state  map[string]*peerState
	active string
	wakeCh chan struct{}

	online atomic.Bool
	client *http.Client

	// probeFn replaces the network probe in tests.
	probeFn func(ctx context.Context, url string) (bool, string)
}

// init prepares the fields Start would normally set.
//
// Called from every entry point rather than only from Start, because the
// connectivity watchdog can be switched off in config — and ChoosePeer must
// still answer when it is. Caller must hold w.mu.
func (w *Watcher) init() {
	if w.state == nil {
		w.state = map[string]*peerState{}
	}
	if w.wakeCh == nil {
		w.wakeCh = make(chan struct{})
	}
	if w.client == nil {
		w.client = httpx.NewClient(90 * time.Second)
	}
}

// candidates returns the receivers in preference order.
func (w *Watcher) candidates() []string {
	if len(w.URLs) == 0 {
		if strings.TrimSpace(w.PeerURL) == "" {
			return nil
		}
		return []string{w.PeerURL}
	}
	if w.Manual {
		return w.URLs[:1]
	}
	return w.URLs
}

func (w *Watcher) cooldown() time.Duration {
	if w.FailureCooldown > 0 {
		return w.FailureCooldown
	}
	return defaultFailureCooldown
}

// Start launches the background polling goroutine. It returns when ctx is
// cancelled.
func (w *Watcher) Start(ctx context.Context) {
	w.mu.Lock()
	w.init()
	w.mu.Unlock()

	// Probe immediately so the first upload attempt doesn't wait 30s.
	w.probeAll(ctx)
	go w.loop(ctx)
}

func (w *Watcher) loop(ctx context.Context) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.probeAll(ctx)
		}
	}
}

func (w *Watcher) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, u := range w.candidates() {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			ok, msg := w.probe(ctx, u)
			w.record(u, ok, msg)
		}(u)
	}
	wg.Wait()
}

// probe reports whether a receiver can actually take an upload.
//
// "Answers at all" is not the bar. The old probe accepted any status below
// 500, so a receiver rejecting the credentials showed healthy while every
// upload failed with an empty-bodied 401 — the least informative failure in
// the system. Anything that is not a plain success is unusable here.
func (w *Watcher) probe(ctx context.Context, peer string) (bool, string) {
	if w.probeFn != nil {
		return w.probeFn(ctx, peer)
	}

	w.mu.Lock()
	w.init()
	client := w.client
	w.mu.Unlock()

	target := strings.TrimRight(peer, "/") + "/system"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err.Error()
	}
	if w.Username != "" || w.Password != "" {
		req.SetBasicAuth(w.Username, w.Password)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Name resolution failing looks identical to "the receiver is down" in
		// a generic dial error, and the two need completely different fixes.
		if !httpx.LogIfDNS("peer probe", err) {
			log.L().Warn("connectivity probe failed", "peer", peer, "err", err)
		}
		return false, err.Error()
	}
	defer resp.Body.Close()

	// A scheme-changing redirect is fatal for uploads even though the probe
	// itself sails through it. The probe is a GET and follows redirects
	// happily; an upload is a streamed POST with no replayable body, so Go
	// cannot re-send it to the new location and the request hangs until the
	// client timeout. Report the working URL rather than a healthy receiver.
	if resp.Request != nil && resp.Request.URL != nil &&
		!strings.EqualFold(resp.Request.URL.Scheme, req.URL.Scheme) {
		return false, fmt.Sprintf(
			"redirects to %s://%s — configure that URL instead; uploads cannot follow a redirect",
			resp.Request.URL.Scheme, resp.Request.URL.Host)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("receiver rejected the credentials (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 500:
		return false, fmt.Sprintf("receiver returned HTTP %d", resp.StatusCode)
	case resp.StatusCode >= 400:
		return false, fmt.Sprintf("receiver returned HTTP %d for /system", resp.StatusCode)
	}
	return true, ""
}

// record stores one probe result and recomputes whether anything is reachable.
func (w *Watcher) record(peer string, ok bool, msg string) {
	w.mu.Lock()
	w.init()
	st := w.state[peer]
	if st == nil {
		st = &peerState{}
		w.state[peer] = st
	}

	now := time.Now()
	st.lastProbe = now
	st.lastErr = msg

	was := st.reachable
	if ok {
		st.fails = 0
		st.reachable = true
		st.lastOK = now
	} else {
		// One failure proves very little; several in a row do. A receiver that
		// has never answered is down from the first failure — there is no
		// working state to protect.
		st.fails++
		if st.fails >= unreachableAfter || st.lastOK.IsZero() {
			st.reachable = false
		}
	}

	anyUp := false
	for _, u := range w.candidates() {
		if s := w.state[u]; s != nil && s.reachable {
			anyUp = true
			break
		}
	}

	wasOnline := w.online.Swap(anyUp)
	var wake chan struct{}
	if anyUp && !wasOnline {
		wake = w.wakeCh
		w.wakeCh = make(chan struct{})
	}
	changed := was != st.reachable
	w.mu.Unlock()

	if changed {
		if st.reachable {
			log.L().Info("receiver reachable", "peer", peer)
		} else {
			log.L().Warn("receiver unreachable", "peer", peer, "err", msg)
		}
	}
	if wake != nil {
		close(wake)
	}
	if wasOnline && !anyUp {
		log.L().Warn("no receiver reachable — uploads will pause")
	}
}

// freshCheck returns a receiver's usability, reusing a recent probe rather
// than starting another one.
func (w *Watcher) freshCheck(ctx context.Context, peer string) bool {
	w.mu.Lock()
	w.init()
	if st := w.state[peer]; st != nil && time.Since(st.lastProbe) < probeFreshness {
		reachable := st.reachable
		w.mu.Unlock()
		return reachable
	}
	w.mu.Unlock()

	pctx, cancel := context.WithTimeout(ctx, chooseProbeTimeout)
	defer cancel()
	ok, msg := w.probe(pctx, peer)
	w.record(peer, ok, msg)
	return ok
}

// ChoosePeer picks the receiver for one study.
//
// Candidates are probed CONCURRENTLY but consumed in preference order, so the
// preferred receiver wins whenever it answers, while a dead one costs at most
// chooseProbeTimeout instead of a sum of timeouts.
//
// ok=false means nothing answered. The returned URL is still the most
// preferred one, so a caller with no better information has somewhere to go;
// callers holding an assigned study should keep that assignment instead.
func (w *Watcher) ChoosePeer(ctx context.Context) (string, bool) {
	cands := w.candidates()
	if len(cands) == 0 {
		return "", false
	}

	if w.Manual || len(cands) == 1 {
		ok := w.freshCheck(ctx, cands[0])
		w.setActive(cands[0])
		return cands[0], ok
	}

	results := make([]chan bool, len(cands))
	for i, u := range cands {
		results[i] = make(chan bool, 1)
		go func(i int, u string) { results[i] <- w.freshCheck(ctx, u) }(i, u)
	}

	cooling := ""
	for i, u := range cands {
		if !<-results[i] {
			continue
		}
		if w.IsCoolingDown(u) {
			// Reachable but recently failed a study. Remember it: better than
			// nothing if no healthier receiver turns up.
			if cooling == "" {
				cooling = u
			}
			continue
		}
		w.setActive(u)
		return u, true
	}
	if cooling != "" {
		w.setActive(cooling)
		return cooling, true
	}
	return cands[0], false
}

func (w *Watcher) setActive(peer string) {
	w.mu.Lock()
	w.active = peer
	w.mu.Unlock()
}

// MarkFailed passes a receiver over for a cooldown after it failed a study.
//
// A manual pin is the operator's decision and outranks this: pinning a
// receiver and then silently moving off it when it struggles is precisely the
// behaviour manual mode exists to prevent.
func (w *Watcher) MarkFailed(peer string, cause error) {
	if w.Manual || peer == "" {
		return
	}
	w.mu.Lock()
	w.init()
	st := w.state[peer]
	if st == nil {
		st = &peerState{}
		w.state[peer] = st
	}
	st.failedUntil = time.Now().Add(w.cooldown())
	w.mu.Unlock()

	log.L().Warn("receiver failed a study — trying another receiver for the next attempt",
		"peer", peer, "cooldown", w.cooldown(), "err", cause)
}

// IsCoolingDown reports whether a receiver is inside its post-failure window.
func (w *Watcher) IsCoolingDown(peer string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.state[peer]
	return st != nil && time.Now().Before(st.failedUntil)
}

// IsCandidate reports whether a URL is still one of the configured receivers.
//
// Compared case-insensitively and ignoring a trailing slash, matching
// config.Candidates: a study assigned before a config edit must not be
// stranded on a URL that is really still configured, only spelled differently.
func (w *Watcher) IsCandidate(peer string) bool {
	key := strings.ToLower(strings.TrimRight(strings.TrimSpace(peer), "/"))
	if key == "" {
		return false
	}
	for _, u := range w.candidates() {
		if strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/")) == key {
			return true
		}
	}
	return false
}

// IsManual reports whether a receiver is pinned.
func (w *Watcher) IsManual() bool { return w.Manual }

// IsOnline reports whether ANY candidate is reachable.
func (w *Watcher) IsOnline() bool { return w.online.Load() }

// WaitUntilOnline blocks until some receiver is reachable or ctx is cancelled.
func (w *Watcher) WaitUntilOnline(ctx context.Context) error {
	if w.online.Load() {
		return nil
	}
	log.L().Warn("no receiver reachable — waiting for connectivity before uploading")
	for {
		w.mu.Lock()
		w.init()
		wake := w.wakeCh
		w.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
			return nil
		case <-time.After(5 * time.Second):
			if w.online.Load() {
				return nil
			}
		}
	}
}

// PeerStatus is one receiver's state, for /api/status and the Settings list.
type PeerStatus struct {
	URL         string    `json:"url"`
	Reachable   bool      `json:"reachable"`
	Error       string    `json:"error,omitempty"`
	LastProbe   time.Time `json:"last_probe,omitempty"`
	LastOK      time.Time `json:"last_ok,omitempty"`
	CoolingDown bool      `json:"cooling_down"`
	Active      bool      `json:"active"`
}

// Peers reports every candidate in preference order.
func (w *Watcher) Peers() []PeerStatus {
	cands := w.candidates()
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	out := make([]PeerStatus, 0, len(cands))
	for _, u := range cands {
		ps := PeerStatus{URL: u, Active: u == w.active}
		if st := w.state[u]; st != nil {
			ps.Reachable = st.reachable
			ps.Error = st.lastErr
			ps.LastProbe = st.lastProbe
			ps.LastOK = st.lastOK
			ps.CoolingDown = now.Before(st.failedUntil)
		}
		out = append(out, ps)
	}
	return out
}

// ActivePeer is the receiver most recently chosen, or the preferred one.
func (w *Watcher) ActivePeer() string {
	w.mu.Lock()
	active := w.active
	w.mu.Unlock()
	if active != "" {
		return active
	}
	if c := w.candidates(); len(c) > 0 {
		return c[0]
	}
	return ""
}

// LastError is "" when some receiver is usable, else every receiver's reason.
func (w *Watcher) LastError() string {
	if w.online.Load() {
		return ""
	}
	cands := w.candidates()
	w.mu.Lock()
	defer w.mu.Unlock()

	var parts []string
	for _, u := range cands {
		if st := w.state[u]; st != nil && st.lastErr != "" {
			parts = append(parts, u+": "+st.lastErr)
		}
	}
	return strings.Join(parts, "; ")
}

// LastProbe is the most recent successful contact with any receiver.
func (w *Watcher) LastProbe() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	var latest time.Time
	for _, st := range w.state {
		if st.lastOK.After(latest) {
			latest = st.lastOK
		}
	}
	return latest
}
