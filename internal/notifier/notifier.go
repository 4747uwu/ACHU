// Package notifier pre-registers studies on the backend PACS the moment the
// first DICOM instance arrives, so the study appears immediately in the
// worklist with an "upload_pending" status. The backend is expected to remove
// that tag via an OnStableStudy Orthanc webhook once the real STOW-RS upload
// completes.
package notifier

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/httpx"
)

// StudyPayload is the JSON body posted to BackendURL.
// Field names match the Lua OnStoredInstance payload so the server handler
// needs no changes — it already knows how to resolve org/lab from these fields.
type StudyPayload struct {
	StudyInstanceUID string `json:"StudyInstanceUID"`
	PatientID        string `json:"PatientID"`
	PatientName      string `json:"PatientName"`
	StudyDate        string `json:"StudyDate"`
	StudyTime        string `json:"StudyTime"`
	StudyDescription string `json:"StudyDescription"`
	AccessionNumber  string `json:"AccessionNumber"`
	Modality         string `json:"Modality"`
	Status           string `json:"status"` // always "upload_pending"

	// Private tag fields — server uses these to resolve org and lab.
	// We populate them with OrgID and LabID from config (same identifiers
	// that the Lua script writes into the private DICOM tags).
	PrivateCreator0013      string `json:"PrivateCreator_0013"`
	PrivateCreator0015      string `json:"PrivateCreator_0015"`
	PrivateOrganisation0021 string `json:"PrivateOrganisation_0021"`
	PrivateOrganisation0043 string `json:"PrivateOrganisation_0043"`
}

// LabGate reports whether this lab is currently allowed to transmit, plus the
// operator-facing reason when it is not. A nil gate is always open.
type LabGate func() (bool, string)

// maxPending caps the parked-notification backlog. A workstation left running
// for weeks behind a suspended account must not grow without bound; past this
// point the oldest notification is dropped to make room for the newest.
const maxPending = 500

// pendingPost is a notification that was refused by the lab gate and is being
// held until the account is active again.
//
// Refused notifications are parked rather than dropped because dropping them
// loses studies permanently: the lab keeps receiving and storing scans while
// suspended, the retention sweeper eventually deletes them, and the central
// PACS never hears the study existed.
type pendingPost struct {
	kind    string // "study" or "delivered", for logging only
	label   string // study UID
	url     string
	payload any
}

// Notifier posts study metadata to a backend URL once per study.
type Notifier struct {
	BackendURL string
	// DeliveredURL receives the per-study "delivered" event. It is derived
	// from BackendURL by replacing the final path segment with "delivered"
	// (e.g. .../api/orthanc2/instance-exe-received -> .../api/orthanc2/delivered).
	DeliveredURL string
	APIKey       string
	LabID        string
	OrgID        string

	// Gate is consulted before every POST. When it refuses, the notification
	// is parked and retried by StartPendingFlusher instead of being sent.
	Gate LabGate

	mu       sync.Mutex
	notified map[string]struct{}
	pending  []pendingPost
	dropped  int
	client   *http.Client
}

// New creates a Notifier. If BackendURL is empty, all calls are no-ops.
func New(backendURL, apiKey, labID, orgID string) *Notifier {
	n := &Notifier{
		BackendURL:   backendURL,
		DeliveredURL: deriveDeliveredURL(backendURL),
		APIKey:       apiKey,
		LabID:        labID,
		OrgID:        orgID,
		notified:     make(map[string]struct{}),
		client:       httpx.NewClient(10 * time.Second),
	}
	if backendURL == "" {
		log.L().Warn("NOTIFIER disabled — backend_url is empty in config")
	} else {
		log.L().Info("NOTIFIER ready", "url", backendURL, "delivered_url", n.DeliveredURL, "lab_id", labID, "org_id", orgID)
	}
	return n
}

// deriveDeliveredURL builds the "delivered" endpoint from the notifier's
// BackendURL by swapping the final path segment for "delivered". Returns ""
// when backendURL is empty or unparseable (delivered notifications disabled).
func deriveDeliveredURL(backendURL string) string {
	if backendURL == "" {
		return ""
	}
	u, err := url.Parse(backendURL)
	if err != nil {
		return ""
	}
	p := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[:i] // drop the last segment (e.g. "instance-exe-received")
	}
	u.Path = p + "/delivered"
	u.RawQuery = ""
	return u.String()
}

// MaybeNotify fires at most once per studyUID. It is safe for concurrent use
// and runs the HTTP POST in the background so it never blocks the ingest path.
func (n *Notifier) MaybeNotify(studyUID string, payload StudyPayload) {
	if n.BackendURL == "" {
		return
	}

	n.mu.Lock()
	if _, already := n.notified[studyUID]; already {
		n.mu.Unlock()
		log.L().Info("NOTIFIER skip — already notified", "study_uid", studyUID)
		return
	}
	n.notified[studyUID] = struct{}{}
	n.mu.Unlock()

	payload.Status = "upload_pending"
	// Populate private tag fields so the server can resolve org and lab.
	payload.PrivateCreator0013      = n.LabID
	payload.PrivateCreator0015      = n.LabID
	payload.PrivateOrganisation0021 = n.OrgID
	payload.PrivateOrganisation0043 = n.OrgID

	// The gate may need a network round trip, so evaluate it off the ingest
	// path along with the POST itself.
	go func() {
		if ok, reason := n.gateAllows(); !ok {
			n.park(pendingPost{
				kind:    "study",
				label:   studyUID,
				url:     n.BackendURL,
				payload: payload,
			})
			log.L().Warn("NOTIFIER held — lab is not active",
				"study_uid", studyUID,
				"patient", payload.PatientName,
				"reason", reason,
				"pending", n.PendingCount(),
			)
			return
		}

		log.L().Info("NOTIFIER firing",
			"study_uid", studyUID,
			"patient", payload.PatientName,
			"accession", payload.AccessionNumber,
			"modality", payload.Modality,
			"lab_id", n.LabID,
			"org_id", n.OrgID,
			"url", n.BackendURL,
			"lab_gate", "passed",
		)

		status, body, err := n.postJSON(n.BackendURL, payload)
		if err != nil {
			log.L().Error("NOTIFIER POST failed",
				"study_uid", studyUID,
				"patient", payload.PatientName,
				"url", n.BackendURL,
				"http_status", status,
				"response_body", body,
				"err", err,
			)
		} else {
			log.L().Info("NOTIFIER POST ok",
				"study_uid", studyUID,
				"patient", payload.PatientName,
				"http_status", status,
				"response_body", body,
			)
		}
	}()
}

// ---------------------------------------------------------------------------
// Lab gate + parking
// ---------------------------------------------------------------------------

// gateAllows consults the lab gate. A nil gate is always open.
func (n *Notifier) gateAllows() (bool, string) {
	if n.Gate == nil {
		return true, ""
	}
	return n.Gate()
}

// park holds a refused notification for later. The newest is always kept: when
// the backlog is full the oldest is dropped, which keeps memory flat while
// preserving the studies most likely to still matter.
func (n *Notifier) park(p pendingPost) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.pending) >= maxPending {
		evicted := n.pending[0]
		n.pending = n.pending[1:]
		n.dropped++
		log.L().Warn("NOTIFIER backlog full — dropped the oldest held notification",
			"study_uid", evicted.label,
			"kind", evicted.kind,
			"max_pending", maxPending,
			"dropped_total", n.dropped,
		)
	}
	n.pending = append(n.pending, p)
}

// repark puts a notification back at the FRONT of the queue, so a failure
// mid-flush cannot reorder notifications relative to each other.
func (n *Notifier) repark(p pendingPost) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pending = append([]pendingPost{p}, n.pending...)
}

// PendingCount reports how many notifications are held. It feeds /api/status,
// which is what the app's inactive banner counts.
func (n *Notifier) PendingCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.pending)
}

// StartPendingFlusher retries held notifications on an interval until ctx is
// cancelled. It returns immediately.
func (n *Notifier) StartPendingFlusher(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.flushPending()
			}
		}
	}()
}

// flushPending drains the backlog in arrival order, stopping at the first
// refusal or transport failure so ordering is preserved and nothing is lost.
func (n *Notifier) flushPending() {
	if n.PendingCount() == 0 {
		return
	}

	for {
		n.mu.Lock()
		if len(n.pending) == 0 {
			n.mu.Unlock()
			return
		}
		p := n.pending[0]
		n.pending = n.pending[1:]
		remaining := len(n.pending)
		n.mu.Unlock()

		if ok, _ := n.gateAllows(); !ok {
			n.repark(p)
			return
		}

		status, body, err := n.postJSON(p.url, p.payload)
		if err != nil {
			// The account is active but the POST failed — put it back and try
			// again on the next tick rather than losing the notification.
			n.repark(p)
			log.L().Warn("NOTIFIER flush failed — notification stays held",
				"study_uid", p.label,
				"kind", p.kind,
				"http_status", status,
				"err", err,
			)
			return
		}

		log.L().Info("NOTIFIER flushed held notification",
			"study_uid", p.label,
			"kind", p.kind,
			"http_status", status,
			"response_body", body,
			"remaining", remaining,
		)
	}
}

// postJSON marshals v and POSTs it to targetURL, returning
// (httpStatus, responseBody, error).
func (n *Notifier) postJSON(targetURL string, v any) (int, string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, "", fmt.Errorf("marshal: %w", err)
	}

	log.L().Info("NOTIFIER sending payload", "url", targetURL, "json", string(b))

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, targetURL, bytes.NewReader(b))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+n.APIKey)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	respBody := string(rawBody)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, respBody, fmt.Errorf("backend returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, respBody, nil
}

// ---------------------------------------------------------------------------
// Delivered notification
// ---------------------------------------------------------------------------

// DeliveredPayload is the JSON body POSTed to DeliveredURL once a study has
// been fully pushed to the peer (every series delivered). Unlike the
// upload_pending notification, this fires on EVERY delivery — there is no
// de-duplication — so the server sees a delivery event each time, including
// re-deliveries after a retry or resend.
type DeliveredPayload struct {
	// OrthancID is the receiver-side Orthanc public ID for the study,
	// computed deterministically from PatientID + StudyInstanceUID the exact
	// same way Orthanc computes it. This lets the server correlate the study
	// with the resource that now exists on the PACS without a lookup.
	OrthancID        string `json:"OrthancID"`
	StudyInstanceUID string `json:"StudyInstanceUID"`
	PatientID        string `json:"PatientID"`
	Status           string `json:"status"` // always "delivered"
	LabID            string `json:"lab_id"`
	OrgID            string `json:"org_id"`
	DeliveredAt      string `json:"delivered_at"` // RFC3339 timestamp
}

// OrthancStudyID reproduces Orthanc's deterministic study public ID:
// the SHA-1 of "PatientID|StudyInstanceUID", lower-case hex, split into five
// dash-separated 8-character groups (e.g. "b9c08539-26456a25-...-9d3e0d0e").
// Identical inputs always yield the identical ID Orthanc itself assigns.
func OrthancStudyID(patientID, studyInstanceUID string) string {
	sum := sha1.Sum([]byte(patientID + "|" + studyInstanceUID))
	h := hex.EncodeToString(sum[:]) // 40 lower-case hex chars
	return strings.Join([]string{h[0:8], h[8:16], h[16:24], h[24:32], h[32:40]}, "-")
}

// NotifyDelivered POSTs a delivered event to DeliveredURL. It ALWAYS fires
// (no de-dup) and runs the HTTP POST in the background so it never blocks the
// transfer path. OrthancID, LabID, OrgID, Status and DeliveredAt are filled
// in automatically when left empty.
func (n *Notifier) NotifyDelivered(p DeliveredPayload) {
	if n.DeliveredURL == "" {
		log.L().Warn("NOTIFIER delivered skipped — no delivered_url", "study_uid", p.StudyInstanceUID)
		return
	}

	p.Status = "delivered"
	if p.OrthancID == "" {
		p.OrthancID = OrthancStudyID(p.PatientID, p.StudyInstanceUID)
	}
	if p.LabID == "" {
		p.LabID = n.LabID
	}
	if p.OrgID == "" {
		p.OrgID = n.OrgID
	}
	if p.DeliveredAt == "" {
		p.DeliveredAt = time.Now().UTC().Format(time.RFC3339)
	}

	go func() {
		if ok, reason := n.gateAllows(); !ok {
			n.park(pendingPost{
				kind:    "delivered",
				label:   p.StudyInstanceUID,
				url:     n.DeliveredURL,
				payload: p,
			})
			log.L().Warn("NOTIFIER delivered held — lab is not active",
				"study_uid", p.StudyInstanceUID,
				"orthanc_id", p.OrthancID,
				"reason", reason,
				"pending", n.PendingCount(),
			)
			return
		}

		log.L().Info("NOTIFIER delivered firing",
			"study_uid", p.StudyInstanceUID,
			"orthanc_id", p.OrthancID,
			"lab_id", p.LabID,
			"org_id", p.OrgID,
			"url", n.DeliveredURL,
			"lab_gate", "passed",
		)

		status, body, err := n.postJSON(n.DeliveredURL, p)
		if err != nil {
			log.L().Error("NOTIFIER delivered POST failed",
				"study_uid", p.StudyInstanceUID,
				"orthanc_id", p.OrthancID,
				"url", n.DeliveredURL,
				"http_status", status,
				"response_body", body,
				"err", err,
			)
		} else {
			log.L().Info("NOTIFIER delivered POST ok",
				"study_uid", p.StudyInstanceUID,
				"orthanc_id", p.OrthancID,
				"http_status", status,
				"response_body", body,
			)
		}
	}()
}
