// Package transfer implements the push client that ships studies from this
// sender to the receiver-side Orthanc.
//
// Two protocols are supported (selected by Client.Protocol):
//
//  1. STOW-RS (default) — DICOMweb (PS3.18) multipart upload via the
//     dicom-web plugin's POST /dicom-web/studies endpoint. ONE HTTP
//     request per series carrying every instance as a multipart/related
//     part. ~20-60× fewer round trips than per-instance POST under load.
//     Standard, well-supported, broadly tooled.
//
//  2. POST /instances (legacy) — Orthanc's native REST: one HTTP request
//     per instance. Universal — works against any Orthanc with no
//     plugins. Useful as a fallback when dicom-web is unavailable.
//
// We deliberately do NOT use the Transfers Accelerator plugin's push
// protocol. That protocol is fundamentally Orthanc-to-Orthanc — its
// /transfers/send endpoint lives on the SOURCE Orthanc and orchestrates
// internal HTTP calls between the two instances. Since we're not Orthanc
// on the source side, the only path would be reverse-engineering its
// internal target-side endpoints, which are explicitly undocumented and
// versioned with the plugin. STOW-RS is a documented standard with
// equivalent (or better) throughput for our 50k-studies/month workload.
package transfer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/httpx"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// PushProtocol selects how the client uploads data to the peer.
type PushProtocol string

const (
	// ProtocolSTOWRS uses DICOMweb STOW-RS (PS3.18). Default.
	ProtocolSTOWRS PushProtocol = "stow-rs"

	// ProtocolInstances uses Orthanc's native REST (POST /instances).
	ProtocolInstances PushProtocol = "instances"
)

// instanceFile pairs a file path with its DICOM SOP Instance UID.
// Both are needed for checksum/retry: the path to stream, the UID to
// verify against the STOW-RS response's ReferencedSOPSequence.
type instanceFile struct {
	path   string
	sopUID string
}

// Client pushes DICOM files to an Orthanc peer.
type Client struct {
	// PeerURL is the receiver Orthanc base URL, e.g. "http://206.189.133.52:8042".
	PeerURL string

	// Username and Password authenticate with the peer. Optional if the
	// peer doesn't require auth.
	Username string
	Password string

	// CACertPath is an optional PEM file containing CAs to trust when
	// PeerURL is HTTPS.
	CACertPath string

	// Timeout per HTTP request.
	Timeout time.Duration

	// Concurrency caps parallel STOW-RS chunk uploads per series.
	Concurrency int

	// BucketSizeMB controls how many MB per STOW-RS chunk. Chunks are
	// uploaded in parallel up to Concurrency. 0 → 32 MB default.
	BucketSizeMB int

	// Protocol selects STOW-RS (default) or per-instance POST.
	Protocol PushProtocol

	// OnSeriesProgress is called as transfer state changes.
	// Safe to leave nil.
	OnSeriesProgress func(SeriesProgress)

	// OnStudyDelivered, if set, is called every time a study transitions to
	// Delivered (all of its series pushed). Used to fire the backend
	// "delivered" notification. Safe to leave nil.
	OnStudyDelivered func(studyUID string)

	// CheckpointEnabled, if set, is called to check whether instance
	// checkpointing is active. If nil, checkpointing is disabled.
	CheckpointEnabled func() bool

	// Internal — initialized lazily.
	httpClient *http.Client
}

// SeriesProgress represents live transfer progress for a series upload.
type SeriesProgress struct {
	SeriesUID      string
	StudyUID       string
	Status         string // sending, complete, failed
	BytesSent      int64
	TotalBytes     int64
	InstancesSent  int
	TotalInstances int
	Error          string
}

// PushSeries pushes every instance of a series to the peer, then marks the
// series Delivered in the store. If the parent study has all its series
// delivered, the study is also marked Delivered.
//
// On any per-instance error, returns immediately (the worker's retry loop
// will re-enqueue with backoff).
func (c *Client) PushSeries(ctx context.Context, st *store.Store, seriesUID string) error {
	if err := c.ensureClient(); err != nil {
		return err
	}

	ser, err := st.GetSeries(seriesUID)
	if err != nil {
		return fmt.Errorf("get series: %w", err)
	}

	files, err := c.findInstanceFiles(st, seriesUID)
	if err != nil {
		return fmt.Errorf("locate instance files: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no instance files found for series %s", seriesUID)
	}

	// Instance checkpointing: skip SOPs already confirmed delivered in a
	// previous attempt that was interrupted by a network failure.
	checkpointOn := c.CheckpointEnabled != nil && c.CheckpointEnabled()
	if checkpointOn && len(ser.DeliveredSOPs) > 0 {
		delivered := make(map[string]bool, len(ser.DeliveredSOPs))
		for _, uid := range ser.DeliveredSOPs {
			delivered[uid] = true
		}
		filtered := files[:0]
		for _, f := range files {
			if !delivered[f.sopUID] {
				filtered = append(filtered, f)
			}
		}
		skipped := len(files) - len(filtered)
		if skipped > 0 {
			log.L().Info("checkpoint: skipping already-delivered instances",
				"series_uid", seriesUID,
				"skipped", skipped,
				"remaining", len(filtered),
			)
		}
		files = filtered
	}
	if len(files) == 0 {
		// All instances already delivered — complete the series.
		log.L().Info("all instances already delivered (checkpoint), completing series", "series_uid", seriesUID)
		_ = st.ClearDeliveredSOPs(seriesUID)
		if err := st.SetSeriesStatus(seriesUID, store.StatusDelivered); err != nil {
			log.L().Warn("mark series delivered", "uid", seriesUID, "err", err)
		}
		if ser.StudyInstanceUID != "" {
			c.maybeMarkStudyDelivered(st, ser.StudyInstanceUID)
		}
		return nil
	}

	totalBytes := int64(0)
	for _, f := range files {
		fi, err := os.Stat(f.path)
		if err == nil {
			totalBytes += fi.Size()
		}
	}
	c.emit(SeriesProgress{
		SeriesUID:      seriesUID,
		StudyUID:       ser.StudyInstanceUID,
		Status:         "sending",
		BytesSent:      0,
		TotalBytes:     totalBytes,
		InstancesSent:  0,
		TotalInstances: len(files),
	})

	log.L().Info("pushing series",
		"series_uid", seriesUID,
		"study_uid", ser.StudyInstanceUID,
		"instances", len(files),
		"modality", ser.Modality,
		"protocol", c.protocol(),
	)

	progressFn := func(bytesSent int64, instancesSent int) {
		c.emit(SeriesProgress{
			SeriesUID:      seriesUID,
			StudyUID:       ser.StudyInstanceUID,
			Status:         "sending",
			BytesSent:      bytesSent,
			TotalBytes:     totalBytes,
			InstancesSent:  instancesSent,
			TotalInstances: len(files),
		})
	}

	// onChunkDelivered is called after each successful chunk upload.
	// It persists the delivered SOP UIDs so a retry can skip them.
	onChunkDelivered := func(sopUIDs []string) {
		if c.CheckpointEnabled == nil || !c.CheckpointEnabled() {
			return
		}
		if err := st.MarkSOPsDelivered(seriesUID, sopUIDs); err != nil {
			log.L().Warn("checkpoint sops", "series_uid", seriesUID, "err", err)
		}
	}

	if err := c.pushFiles(ctx, files, progressFn, onChunkDelivered); err != nil {
		c.emit(SeriesProgress{
			SeriesUID:      seriesUID,
			StudyUID:       ser.StudyInstanceUID,
			Status:         "failed",
			BytesSent:      0,
			TotalBytes:     totalBytes,
			InstancesSent:  0,
			TotalInstances: len(files),
			Error:          err.Error(),
		})
		return err
	}

	// All instances of this series delivered — clear the checkpoint.
	_ = st.ClearDeliveredSOPs(seriesUID)
	if err := st.SetSeriesStatus(seriesUID, store.StatusDelivered); err != nil {
		log.L().Warn("mark series delivered", "uid", seriesUID, "err", err)
	}

	// Walk the parent study: if all its series are Delivered, mark the
	// study Delivered too.
	if ser.StudyInstanceUID != "" {
		c.maybeMarkStudyDelivered(st, ser.StudyInstanceUID)
	}

	log.L().Info("series delivered", "series_uid", seriesUID, "instances", len(files))
	c.emit(SeriesProgress{
		SeriesUID:      seriesUID,
		StudyUID:       ser.StudyInstanceUID,
		Status:         "complete",
		BytesSent:      totalBytes,
		TotalBytes:     totalBytes,
		InstancesSent:  len(files),
		TotalInstances: len(files),
	})
	return nil
}

// pushFiles dispatches to the configured protocol.
func (c *Client) pushFiles(ctx context.Context, files []instanceFile, onProgress func(bytesSent int64, instancesSent int), onChunkDelivered func(sopUIDs []string)) error {
	switch c.protocol() {
	case ProtocolSTOWRS:
		return c.pushFilesSTOWRS(ctx, files, onProgress, onChunkDelivered)
	case ProtocolInstances:
		return c.pushFilesInstances(ctx, files, onProgress, onChunkDelivered)
	default:
		return fmt.Errorf("unknown push protocol %q", c.Protocol)
	}
}

// protocol returns the effective protocol, defaulting to STOW-RS.
func (c *Client) protocol() PushProtocol {
	if c.Protocol == "" {
		return ProtocolSTOWRS
	}
	return c.Protocol
}

// pushFilesInstances does the per-instance POST /instances dance. Universal
// (works with any Orthanc) but ~1 HTTP round-trip per instance.
func (c *Client) pushFilesInstances(ctx context.Context, files []instanceFile, onProgress func(bytesSent int64, instancesSent int), onChunkDelivered func(sopUIDs []string)) error {
	bytesSent := int64(0)
	for i, f := range files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := c.postInstance(ctx, f.path); err != nil {
			return fmt.Errorf("push instance %d/%d (%s): %w", i+1, len(files), f.path, err)
		}
		if fi, err := os.Stat(f.path); err == nil {
			bytesSent += fi.Size()
		}
		if onProgress != nil {
			onProgress(bytesSent, i+1)
		}
		if onChunkDelivered != nil && f.sopUID != "" {
			onChunkDelivered([]string{f.sopUID})
		}
	}
	return nil
}

// postInstance reads one DICOM file and POSTs it to the peer's /instances.
func (c *Client) postInstance(ctx context.Context, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	url := strings.TrimRight(c.PeerURL, "/") + "/instances"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/dicom")
	req.Header.Set("Expect", "")
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 1KB of error body for the log/error message.
		buf := make([]byte, 1024)
		n, _ := io.ReadFull(resp.Body, buf)
		return fmt.Errorf("peer returned %d: %s", resp.StatusCode, strings.TrimSpace(string(buf[:n])))
	}

	// Orthanc replies with JSON describing the stored instance.
	// We don't strictly need it but logging helps when debugging.
	var stored struct {
		ID     string `json:"ID"`
		Status string `json:"Status"`
	}
	dec := json.NewDecoder(resp.Body)
	_ = dec.Decode(&stored)

	return nil
}

// maybeMarkStudyDelivered checks every series under the study; if all are
// Delivered, marks the study Delivered.
func (c *Client) maybeMarkStudyDelivered(st *store.Store, studyUID string) {
	allSeries, err := st.ListSeriesByStudy(studyUID)
	if err != nil {
		log.L().Warn("list series for study", "study_uid", studyUID, "err", err)
		return
	}
	if len(allSeries) == 0 {
		return
	}
	for _, s := range allSeries {
		if s.Status != store.StatusDelivered {
			return
		}
	}
	if err := st.SetStudyStatus(studyUID, store.StatusDelivered); err != nil {
		log.L().Warn("mark study delivered", "study_uid", studyUID, "err", err)
		return
	}
	log.L().Info("study delivered", "study_uid", studyUID, "series", len(allSeries))

	// Fire the backend "delivered" notification (the hook owns any delay and
	// always notifies — even on a re-delivery).
	if c.OnStudyDelivered != nil {
		c.OnStudyDelivered(studyUID)
	}
}

// findInstanceFiles returns every .dcm file path on disk belonging to the
// given series.
//
// File paths are stored on each InstanceRecord by the SCP handler (M1)
// using the convention <data_dir>/instances/<study>/<series>/<sop>.dcm.
func (c *Client) findInstanceFiles(st *store.Store, seriesUID string) ([]instanceFile, error) {
	insts, err := st.ListInstancesBySeries(seriesUID)
	if err != nil {
		return nil, err
	}
	out := make([]instanceFile, 0, len(insts))
	for _, i := range insts {
		if i.FilePath == "" {
			continue
		}
		out = append(out, instanceFile{path: i.FilePath, sopUID: i.SOPInstanceUID})
	}
	return out, nil
}

// ensureClient lazily configures the HTTP client (mostly for TLS).
func (c *Client) ensureClient() error {
	if c.httpClient != nil {
		return nil
	}
	tr := &http.Transport{
		// Resolve through our own nameservers rather than the one the site's
		// router hands out over DHCP — see internal/httpx. Uploads are the
		// thing that must not stall behind a broken clinic router.
		//
		// Proxy is deliberately left nil, as it was before: this transport
		// never used a proxy, and quietly starting to honour HTTP_PROXY would
		// reroute every study upload at any site that happens to have it set.
		DialContext:         httpx.DialContext,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // DICOM files are already compressed; gzip wastes CPU
	}

	if strings.HasPrefix(strings.ToLower(c.PeerURL), "https://") && c.CACertPath != "" {
		pem, err := os.ReadFile(c.CACertPath)
		if err != nil {
			return fmt.Errorf("read ca cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("ca cert %s contains no valid certificates", c.CACertPath)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	c.httpClient = &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
	return nil
}

// ------------------------------------------------------------------------
// STOW-RS (DICOMweb) — POST /dicom-web/studies multipart/related
// ------------------------------------------------------------------------

// pushFilesSTOWRS splits files into chunks and uploads them with dynamic
// concurrency. It starts with 2 parallel connections and ramps up by one
// after each successful chunk — so fast networks quickly reach maxConc
// while slow ones ramp gradually without overwhelming the server.
func (c *Client) pushFilesSTOWRS(ctx context.Context, files []instanceFile, onProgress func(bytesSent int64, instancesSent int), onChunkDelivered func(sopUIDs []string)) error {
	bucketBytes := int64(c.BucketSizeMB) * 1024 * 1024
	if bucketBytes <= 0 {
		bucketBytes = 32 * 1024 * 1024
	}
	chunks := splitIntoChunks(files, bucketBytes)

	maxConc := c.Concurrency
	if maxConc <= 0 {
		maxConc = 8
	}

	// Pre-compute each chunk's disk size for throughput logging.
	type job struct {
		idx   int
		files []instanceFile
		size  int64
	}
	jobCh := make(chan job, len(chunks))
	for i, ch := range chunks {
		var sz int64
		for _, f := range ch {
			if fi, e := os.Stat(f.path); e == nil {
				sz += fi.Size()
			}
		}
		jobCh <- job{i, ch, sz}
	}
	close(jobCh)

	log.L().Info("stow-rs upload plan",
		"chunks", len(chunks),
		"max_concurrency", maxConc,
		"bucket_mb", bucketBytes>>20,
	)

	// Per-chunk progress counters (atomics, indexed by chunk position).
	chunkBytes := make([]int64, len(chunks))
	chunkInst := make([]int64, len(chunks))
	report := func() {
		if onProgress == nil {
			return
		}
		var tb, ti int64
		for i := range chunks {
			tb += atomic.LoadInt64(&chunkBytes[i])
			ti += atomic.LoadInt64(&chunkInst[i])
		}
		onProgress(tb, int(ti))
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(chunks))
	var (
		mu      sync.Mutex
		workers int
		wg      sync.WaitGroup
	)

	// spawnWorker launches one more worker goroutine if below maxConc.
	// Workers pull jobs from jobCh; when it's empty, range exits and
	// the goroutine returns.  Each successful chunk calls spawnWorker
	// once, growing concurrency organically as throughput allows.
	var spawnWorker func()
	spawnWorker = func() {
		mu.Lock()
		if workers >= maxConc {
			mu.Unlock()
			return
		}
		workers++
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				start := time.Now()
				prog := func(bs int64, is int) {
					atomic.StoreInt64(&chunkBytes[j.idx], bs)
					atomic.StoreInt64(&chunkInst[j.idx], int64(is))
					report()
				}
				if err := c.stowChunk(ctx, j.files, prog, 2); err != nil {
					cancel()
					errCh <- err
					return
				}
				// Checkpoint: record every SOP in this chunk as delivered.
				if onChunkDelivered != nil {
					sopUIDs := make([]string, 0, len(j.files))
					for _, f := range j.files {
						if f.sopUID != "" {
							sopUIDs = append(sopUIDs, f.sopUID)
						}
					}
					if len(sopUIDs) > 0 {
						onChunkDelivered(sopUIDs)
					}
				}
				elapsed := time.Since(start)
				if elapsed > 0 && j.size > 0 {
					mbps := float64(j.size) / elapsed.Seconds() / (1 << 20)
					log.L().Info("chunk uploaded",
						"idx", j.idx+1, "of", len(chunks),
						"size_mb", fmt.Sprintf("%.1f", float64(j.size)/(1<<20)),
						"elapsed", elapsed.Round(time.Millisecond),
						"mbps", fmt.Sprintf("%.1f", mbps),
						"workers", workers,
					)
				}
				// Each completed chunk earns one more concurrent slot.
				spawnWorker()
			}
		}()
	}

	// Seed with 2 workers so we're parallel from the first chunk.
	seed := 2
	if seed > len(chunks) {
		seed = len(chunks)
	}
	if seed > maxConc {
		seed = maxConc
	}
	for i := 0; i < seed; i++ {
		spawnWorker()
	}

	wg.Wait()
	close(errCh)
	return <-errCh
}

// splitIntoChunks groups files into slices where each slice's total size is
// at most maxBytes. A single file larger than maxBytes gets its own chunk.
func splitIntoChunks(files []instanceFile, maxBytes int64) [][]instanceFile {
	var chunks [][]instanceFile
	var cur []instanceFile
	var curSize int64
	for _, f := range files {
		fi, err := os.Stat(f.path)
		sz := int64(0)
		if err == nil {
			sz = fi.Size()
		}
		if curSize+sz > maxBytes && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
			curSize = 0
		}
		cur = append(cur, f)
		curSize += sz
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// stowChunk uploads a slice of files as one multipart/related STOW-RS POST.
// Streams via io.Pipe — never materialises the full payload in memory.
func (c *Client) stowChunk(ctx context.Context, files []instanceFile, onProgress func(bytesSent int64, instancesSent int), retriesLeft int) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	boundary := mw.Boundary()

	go func() {
		var streamErr error
		bytesSent := int64(0)
		instancesSent := 0
		for _, f := range files {
			select {
			case <-ctx.Done():
				streamErr = ctx.Err()
				goto done
			default:
			}
			partHeader := textproto.MIMEHeader{}
			partHeader.Set("Content-Type", "application/dicom")
			part, err := mw.CreatePart(partHeader)
			if err != nil {
				streamErr = fmt.Errorf("create multipart part: %w", err)
				goto done
			}
			{
				file, err := os.Open(f.path)
				if err != nil {
					streamErr = fmt.Errorf("open %s: %w", f.path, err)
					goto done
				}
				n, err := io.Copy(part, file)
				_ = file.Close()
				if err != nil {
					streamErr = fmt.Errorf("stream %s: %w", f.path, err)
					goto done
				}
				bytesSent += n
			}
			instancesSent++
			if onProgress != nil {
				onProgress(bytesSent, instancesSent)
			}
		}
	done:
		if cerr := mw.Close(); cerr != nil && streamErr == nil {
			streamErr = cerr
		}
		pw.CloseWithError(streamErr)
	}()

	url := strings.TrimRight(c.PeerURL, "/") + "/dicom-web/studies"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", fmt.Sprintf(`multipart/related; type="application/dicom"; boundary=%s`, boundary))
	req.Header.Set("Accept", "application/dicom+json")
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("stow-rs http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("stow-rs: peer returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Parse the manifest regardless of status code — some servers return 200
	// with a FailedSOPSequence for instances they could not store.
	failedUIDs := decodeSTOWFailedUIDs(resp.Body)
	if len(failedUIDs) == 0 {
		return nil
	}
	if retriesLeft <= 0 {
		log.L().Error("stow-rs: instances rejected after retries", "count", len(failedUIDs))
		return fmt.Errorf("stow-rs: %d instance(s) rejected by peer after retries", len(failedUIDs))
	}
	// Build retry slice from the failed UIDs.
	sopMap := make(map[string]instanceFile, len(files))
	for _, f := range files {
		sopMap[f.sopUID] = f
	}
	var retryFiles []instanceFile
	for _, uid := range failedUIDs {
		if f, ok := sopMap[uid]; ok {
			retryFiles = append(retryFiles, f)
		}
	}
	if len(retryFiles) == 0 {
		// UIDs not in our map (server returned unexpected UIDs); log and succeed.
		log.L().Warn("stow-rs: failed UIDs not in sent set, treating as success", "failed", len(failedUIDs))
		return nil
	}
	log.L().Warn("stow-rs: retrying rejected instances",
		"failed", len(failedUIDs), "retries_left", retriesLeft-1)
	time.Sleep(500 * time.Millisecond)
	return c.stowChunk(ctx, retryFiles, nil, retriesLeft-1)
}

func (c *Client) emit(p SeriesProgress) {
	if c.OnSeriesProgress != nil {
		c.OnSeriesProgress(p)
	}
}

// decodeSTOWFailedUIDs parses a STOW-RS manifest and returns the SOP Instance
// UIDs of any instances the peer rejected. Returns nil on parse failure or
// when all instances were accepted.
func decodeSTOWFailedUIDs(r io.Reader) []string {
	body, err := io.ReadAll(io.LimitReader(r, 256*1024))
	if err != nil || len(body) == 0 {
		return nil
	}
	// DICOMweb JSON: FailedSOPSequence = tag 00081198. Each item is a sequence
	// element with 00081155 (ReferencedSOPInstanceUID).
	var manifest map[string]struct {
		Value []map[string]struct {
			Value []string `json:"Value"`
		} `json:"Value"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil
	}
	seq, ok := manifest["00081198"]
	if !ok || len(seq.Value) == 0 {
		return nil
	}
	var uids []string
	for _, item := range seq.Value {
		if ref, ok := item["00081155"]; ok {
			uids = append(uids, ref.Value...)
		}
	}
	return uids
}
