// Package errorreport captures error-level log records and ships them to the
// backend PACS, tagged with the device and lab they came from.
//
// Flow:
//
//   1. log.SetErrorSink hands every error-or-higher record to Reporter.Report.
//   2. Report throttles duplicate floods, then spools the record to disk as a
//      JSON file under <dataDir>/error-reports/ and signals the worker.
//   3. The worker POSTs each spooled file to BackendURL. On success the file
//      is deleted; on failure it stays and is retried (on a ticker and on the
//      next app start), so a field device that is briefly offline never loses
//      an error report.
//
// Persisting first ("save it") and sending second means reports survive
// network outages and restarts. The send is fully decoupled from the logging
// goroutine, so logging never blocks on the network.
package errorreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/httpx"
)

// DeviceInfo identifies the machine the sender runs on.
type DeviceInfo struct {
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`   // runtime.GOOS, e.g. "windows"
	Arch     string   `json:"arch"` // runtime.GOARCH, e.g. "amd64"
	IPs      []string `json:"ips"`  // non-loopback IPv4 addresses
	MACs     []string `json:"macs"` // hardware addresses of up, non-loopback ifaces
}

// LabInfo identifies which lab/org/peer the device is bound to.
type LabInfo struct {
	LabID    string `json:"labId"`
	OrgID    string `json:"orgId"`
	PeerName string `json:"peerName"`
	PeerURL  string `json:"peerUrl"`
}

// Report is the JSON document POSTed to the backend per captured error.
// The shape here is the contract the server-side model/controller implements.
type Report struct {
	ID         string         `json:"id"` // client-generated; lets the server dedupe on retry
	Source     string         `json:"source"`
	AppVersion string         `json:"appVersion"`
	Level      string         `json:"level"`
	Message    string         `json:"message"`
	Attrs      map[string]any `json:"attrs,omitempty"`
	OccurredAt time.Time      `json:"occurredAt"`
	Device     DeviceInfo     `json:"device"`
	Lab        LabInfo        `json:"lab"`
}

// Reporter spools and ships error reports. Construct with New, install with
// log.SetErrorSink(r.Report), and run with Start.
type Reporter struct {
	backendURL string
	apiKey     string
	appVersion string
	device     DeviceInfo
	lab        LabInfo
	spoolDir   string

	client *http.Client
	signal chan struct{} // buffered(1): nudges the worker to flush now

	seq uint64 // atomic; disambiguates IDs/filenames generated in the same ms

	mu       sync.Mutex
	lastSent map[string]time.Time // fingerprint -> last spool time (flood control)
	cooldown time.Duration
}

// New builds a Reporter. dataDir is the sender's data directory; reports are
// spooled under dataDir/error-reports. A Reporter with an empty backendURL
// still spools to disk (so nothing is lost) but never sends.
func New(backendURL, apiKey, appVersion, dataDir string, device DeviceInfo, lab LabInfo) *Reporter {
	return &Reporter{
		backendURL: backendURL,
		apiKey:     apiKey,
		appVersion: appVersion,
		device:     device,
		lab:        lab,
		spoolDir:   filepath.Join(dataDir, "error-reports"),
		client:     httpx.NewClient(15 * time.Second),
		signal:     make(chan struct{}, 1),
		lastSent:   make(map[string]time.Time),
		cooldown:   30 * time.Second,
	}
}

// Report is the log sink. It runs inline on the logging goroutine, so it only
// does cheap work: throttle, spool to disk, and nudge the worker. Never blocks.
func (r *Reporter) Report(ev log.ErrorEvent) {
	if r == nil {
		return
	}
	// Flood control: collapse identical messages within the cooldown window.
	fp := ev.Level + "|" + ev.Message
	now := time.Now()
	r.mu.Lock()
	if last, ok := r.lastSent[fp]; ok && now.Sub(last) < r.cooldown {
		r.mu.Unlock()
		return
	}
	r.lastSent[fp] = now
	if len(r.lastSent) > 512 { // bound the dedupe map
		r.pruneLocked(now)
	}
	r.mu.Unlock()

	occurred := ev.Time
	if occurred.IsZero() {
		occurred = now
	}
	rep := Report{
		ID:         r.newID(occurred),
		Source:     "tarang-sender",
		AppVersion: r.appVersion,
		Level:      ev.Level,
		Message:    ev.Message,
		Attrs:      sanitizeAttrs(ev.Attrs),
		OccurredAt: occurred.UTC(),
		Device:     r.device,
		Lab:        r.lab,
	}
	if err := r.spool(rep); err != nil {
		// Can't use the error logger here — that would recurse into this sink.
		fmt.Fprintf(os.Stderr, "errorreport: spool failed: %v\n", err)
		return
	}
	// Non-blocking nudge; the worker also flushes on its ticker regardless.
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// Start launches the background worker. It flushes spooled reports on startup,
// whenever Report nudges it, and every 30s as a safety net. Returns after the
// goroutine is launched; stops when ctx is cancelled.
func (r *Reporter) Start(ctx context.Context) {
	if r == nil {
		return
	}
	if err := os.MkdirAll(r.spoolDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "errorreport: cannot create spool dir: %v\n", err)
	}
	go r.loop(ctx)
}

func (r *Reporter) loop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	r.flush(ctx) // ship anything left over from a previous run
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.signal:
			r.flush(ctx)
		case <-ticker.C:
			r.flush(ctx)
		}
	}
}

// flush attempts to POST every spooled report, oldest first. Files that send
// successfully are deleted; the rest stay for the next attempt.
func (r *Reporter) flush(ctx context.Context) {
	if r.backendURL == "" {
		return // no destination; reports stay spooled on disk
	}
	entries, err := os.ReadDir(r.spoolDir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // filenames are time-ordered, so this is oldest-first
	for _, name := range names {
		select {
		case <-ctx.Done():
			return
		default:
		}
		path := filepath.Join(r.spoolDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if err := r.send(ctx, body); err != nil {
			// Network/server problem — keep the file and stop; retry later.
			return
		}
		_ = os.Remove(path)
	}
}

// send POSTs one report body. Returns nil only on a 2xx response.
func (r *Reporter) send(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.backendURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("backend returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// spool writes one report to disk as <id>.json (atomic rename).
func (r *Reporter) spool(rep Report) error {
	if err := os.MkdirAll(r.spoolDir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	path := filepath.Join(r.spoolDir, rep.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newID builds a sortable, unique id: <unixNanos>-<seq>. Time-ordered so the
// flush loop ships oldest first; seq avoids collisions within the same instant.
func (r *Reporter) newID(t time.Time) string {
	n := atomic.AddUint64(&r.seq, 1)
	return fmt.Sprintf("%020d-%06d", t.UTC().UnixNano(), n)
}

// pruneLocked drops dedupe entries older than the cooldown. Caller holds r.mu.
func (r *Reporter) pruneLocked(now time.Time) {
	for k, v := range r.lastSent {
		if now.Sub(v) >= r.cooldown {
			delete(r.lastSent, k)
		}
	}
}

// CollectDeviceInfo gathers identifying details about the host machine.
func CollectDeviceInfo() DeviceInfo {
	host, _ := os.Hostname()
	d := DeviceInfo{Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH}

	ifaces, err := net.Interfaces()
	if err != nil {
		return d
	}
	seenMAC := map[string]struct{}{}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if mac := ifc.HardwareAddr.String(); mac != "" {
			if _, dup := seenMAC[mac]; !dup {
				seenMAC[mac] = struct{}{}
				d.MACs = append(d.MACs, mac)
			}
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				d.IPs = append(d.IPs, ip4.String())
			}
		}
	}
	return d
}

// sanitizeAttrs renders attribute values JSON-safe. slog values such as errors
// marshal to "{}" otherwise, so coerce anything non-trivial to its string form.
func sanitizeAttrs(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch v.(type) {
		case nil, bool, string,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64:
			out[k] = v
		case error:
			out[k] = v.(error).Error()
		case fmt.Stringer:
			out[k] = v.(fmt.Stringer).String()
		default:
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
}
