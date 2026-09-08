// Package connectivity provides a background watchdog that monitors whether
// the configured DICOM receiver (Orthanc) is reachable over HTTP.
//
// The queue worker calls WaitUntilOnline before processing each upload job.
// While the peer is unreachable the worker blocks here rather than burning
// retry budget on entries that will immediately fail again.
package connectivity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/httpx"
)

// Watcher polls a peer URL at a fixed interval and reports its reachability.
type Watcher struct {
	PeerURL  string
	Username string
	Password string

	online    atomic.Bool
	fails     atomic.Int32 // consecutive failed probes; see setOnline
	lastError atomic.Value // string — why the last probe failed, "" when healthy
	lastOK    atomic.Value // time.Time — last successful probe
	client    *http.Client
	wakeCh    chan struct{} // closed when peer comes back online
}

// Start launches the background polling goroutine. It returns when ctx is
// cancelled.
func (w *Watcher) Start(ctx context.Context) {
	// Own resolver: the peer probe is what decides whether uploads run at
	// all, so it must not be the first thing a broken site nameserver takes
	// down. See internal/httpx.
	// 15s, not 8s: a site whose DNS is failing now spends a few seconds
	// falling through the nameserver chain before the request even starts,
	// and the probe result is what decides whether uploads run at all.
	w.client = httpx.NewClient(15 * time.Second)
	w.wakeCh = make(chan struct{})

	// Probe immediately so the first upload attempt doesn't wait 30s.
	w.probe()

	go w.loop(ctx)
}

// IsOnline returns true if the last probe succeeded.
func (w *Watcher) IsOnline() bool { return w.online.Load() }

// WaitUntilOnline blocks until the peer is reachable or ctx is cancelled.
// Returns ctx.Err() if cancelled while still offline.
func (w *Watcher) WaitUntilOnline(ctx context.Context) error {
	if w.online.Load() {
		return nil
	}
	log.L().Warn("peer unreachable — waiting for connectivity before uploading")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.wakeCh:
			return nil
		case <-time.After(5 * time.Second):
			if w.online.Load() {
				return nil
			}
		}
	}
}

func (w *Watcher) loop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.probe()
		}
	}
}

func (w *Watcher) probe() {
	url := strings.TrimRight(w.PeerURL, "/") + "/system"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		w.setOnline(false)
		return
	}
	if w.Username != "" || w.Password != "" {
		req.SetBasicAuth(w.Username, w.Password)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.setOnline(false)
		w.lastError.Store(err.Error())
		// Name resolution failing looks identical to "the peer is down" in a
		// generic dial error, and the two need completely different fixes.
		// Say which one it was rather than leaving it to be guessed at later.
		if !httpx.LogIfDNS("peer probe", err) {
			log.L().Warn("connectivity probe failed", "peer", w.PeerURL, "err", err)
		}
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 500 {
		w.lastError.Store("")
	} else {
		w.lastError.Store(fmt.Sprintf("peer returned HTTP %d", resp.StatusCode))
	}
	w.setOnline(resp.StatusCode < 500)
}

// LastError reports why the most recent probe failed, or "" when the peer is
// answering. It feeds /api/status so the UI can show the real reason.
func (w *Watcher) LastError() string {
	v, _ := w.lastError.Load().(string)
	return v
}

// LastProbe reports when the peer was last successfully contacted.
func (w *Watcher) LastProbe() time.Time {
	v, _ := w.lastOK.Load().(time.Time)
	return v
}

// unreachableAfter is how many probes must fail IN A ROW before uploads are
// paused. Recovery stays immediate — one good probe resumes them.
//
// It is not 1, and the reason is that a single probe used to be enough. The
// probe shares the clinic's uplink with the uploads, and on a small connection
// the uploads saturate it: a 145MB study at 3.6Mbps holds the link for minutes.
// The probe's TCP connect then times out, uploads pause, the link drains, the
// next probe succeeds, uploads resume and saturate it again. A live site
// oscillated on roughly a 90-second cycle doing exactly this, reporting the
// peer unreachable while that same peer was accepting series and marking them
// delivered.
//
// Three consecutive failures at a 30s interval means a genuine outage still
// pauses uploads inside 90 seconds, while a transient stall no longer halts a
// pipeline that is demonstrably working.
const unreachableAfter = 3

func (w *Watcher) setOnline(up bool) {
	if up {
		w.lastOK.Store(time.Now())
		w.fails.Store(0)
		if !w.online.Swap(true) {
			log.L().Info("peer reachable — resuming uploads", "peer", w.PeerURL)
			// Replace the wake channel so waiting goroutines unblock.
			old := w.wakeCh
			w.wakeCh = make(chan struct{})
			close(old)
		}
		return
	}

	// A failure on its own proves very little; several in a row do.
	n := w.fails.Add(1)
	if int(n) < unreachableAfter {
		return
	}
	if w.online.Swap(false) {
		log.L().Warn("peer became unreachable — uploads will pause",
			"peer", w.PeerURL, "consecutive_failures", n)
	}
}
