package labstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestInterpretResponseMatrix is the executable form of the backend contract.
// Every row of the interpretation ladder documented in ACHYU_BACKEND_API.md
// is pinned here; if a backend wants another response shape accepted, it gets
// added to this table first.
func TestInterpretResponseMatrix(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		body       string
		transport  error
		wantKnown  bool
		wantActive bool // only asserted when wantKnown
		wantMsg    string
	}{
		// ---- rule 1: deliberate refusal --------------------------------
		{name: "402 is a refusal", httpStatus: 402, body: `{}`,
			wantKnown: true, wantActive: false, wantMsg: DefaultInactiveMessage},
		{name: "403 is a refusal", httpStatus: 403, body: `{}`,
			wantKnown: true, wantActive: false, wantMsg: DefaultInactiveMessage},
		{name: "423 is a refusal", httpStatus: 423, body: `{}`,
			wantKnown: true, wantActive: false},
		{name: "451 is a refusal", httpStatus: 451, body: `{}`,
			wantKnown: true, wantActive: false},
		{name: "refusal keeps the body message", httpStatus: 403,
			body:      `{"message":"Subscription lapsed."}`,
			wantKnown: true, wantActive: false, wantMsg: "Subscription lapsed."},
		{name: "refusal with no body still blocks", httpStatus: 403, body: ``,
			wantKnown: true, wantActive: false, wantMsg: DefaultInactiveMessage},

		// ---- rule 2: could-not-answer is unknown, never inactive -------
		{name: "500 is unknown", httpStatus: 500, body: `{}`, wantKnown: false},
		{name: "502 is unknown", httpStatus: 502, body: ``, wantKnown: false},
		{name: "404 is unknown, not a block", httpStatus: 404, body: `{}`, wantKnown: false},
		{name: "429 is unknown", httpStatus: 429, body: `{}`, wantKnown: false},
		{name: "401 is unknown", httpStatus: 401, body: `{}`, wantKnown: false},
		{name: "400 is unknown", httpStatus: 400, body: `{}`, wantKnown: false},
		{name: "transport failure is unknown", httpStatus: 0,
			transport: errors.New("dial tcp: no such host"), wantKnown: false},
		{name: "200 with HTML is unknown", httpStatus: 200,
			body: `<html>captive portal</html>`, wantKnown: false},
		{name: "200 with an empty body is unknown", httpStatus: 200, body: ``, wantKnown: false},

		// ---- rule 3: an explicit boolean wins --------------------------
		{name: "active true", httpStatus: 200, body: `{"success":true,"active":true}`,
			wantKnown: true, wantActive: true},
		{name: "active false", httpStatus: 200, body: `{"success":true,"active":false}`,
			wantKnown: true, wantActive: false, wantMsg: DefaultInactiveMessage},
		{name: "is_active alias", httpStatus: 200, body: `{"is_active":false}`,
			wantKnown: true, wantActive: false},
		{name: "lab_active alias", httpStatus: 200, body: `{"lab_active":true}`,
			wantKnown: true, wantActive: true},
		{name: "data envelope is unwrapped", httpStatus: 200,
			body:      `{"success":true,"data":{"active":true}}`,
			wantKnown: true, wantActive: true},
		{name: "boolean beats a contradicting status", httpStatus: 200,
			body:      `{"active":true,"status":"suspended"}`,
			wantKnown: true, wantActive: true},
		{name: "boolean beats a contradicting success", httpStatus: 200,
			body:      `{"success":false,"active":true}`,
			wantKnown: true, wantActive: true},
		{name: "message rides along with active false", httpStatus: 200,
			body:      `{"active":false,"message":"Please pay your invoice."}`,
			wantKnown: true, wantActive: false, wantMsg: "Please pay your invoice."},

		// ---- rule 4: status labels -------------------------------------
		{name: "status active", httpStatus: 200, body: `{"status":"active"}`,
			wantKnown: true, wantActive: true},
		{name: "status enabled", httpStatus: 200, body: `{"status":"enabled"}`,
			wantKnown: true, wantActive: true},
		{name: "status approved", httpStatus: 200, body: `{"status":"approved"}`,
			wantKnown: true, wantActive: true},
		{name: "status suspended", httpStatus: 200, body: `{"status":"suspended"}`,
			wantKnown: true, wantActive: false},
		{name: "status expired", httpStatus: 200, body: `{"status":"expired"}`,
			wantKnown: true, wantActive: false},
		{name: "status not_found", httpStatus: 200, body: `{"status":"not_found"}`,
			wantKnown: true, wantActive: false},
		{name: "status is case-insensitive", httpStatus: 200, body: `{"status":"ACTIVE"}`,
			wantKnown: true, wantActive: true},
		{name: "status in a data envelope", httpStatus: 200,
			body:      `{"data":{"status":"suspended"}}`,
			wantKnown: true, wantActive: false},
		{name: "an unrecognised status does not block", httpStatus: 200,
			body: `{"status":"quantum"}`, wantKnown: false},

		// ---- rule 5: success only --------------------------------------
		{name: "success true alone", httpStatus: 200, body: `{"success":true}`,
			wantKnown: true, wantActive: true},
		{name: "success false alone", httpStatus: 200, body: `{"success":false}`,
			wantKnown: true, wantActive: false, wantMsg: DefaultInactiveMessage},

		// ---- rule 6: nothing recognisable ------------------------------
		{name: "200 with an unrelated body is unknown", httpStatus: 200,
			body: `{"hello":"world"}`, wantKnown: false},
		{name: "200 with a JSON array is unknown", httpStatus: 200,
			body: `[1,2,3]`, wantKnown: false},

		// ---- the canonical shapes from the contract --------------------
		{name: "canonical active", httpStatus: 200,
			body:      `{"success":true,"active":true,"status":"active","message":""}`,
			wantKnown: true, wantActive: true, wantMsg: ""},
		{name: "canonical inactive", httpStatus: 200,
			body:      `{"success":true,"active":false,"status":"suspended","message":"Your subscription has expired. Please contact Achyu."}`,
			wantKnown: true, wantActive: false,
			wantMsg: "Your subscription has expired. Please contact Achyu."},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := interpretResponse(tc.httpStatus, []byte(tc.body), tc.transport)

			if got.Known != tc.wantKnown {
				t.Fatalf("Known = %v, want %v (status=%q err=%q)",
					got.Known, tc.wantKnown, got.Status, got.LastError)
			}
			if tc.wantKnown && got.Active != tc.wantActive {
				t.Fatalf("Active = %v, want %v", got.Active, tc.wantActive)
			}
			if tc.wantMsg != "" && got.Message != tc.wantMsg {
				t.Fatalf("Message = %q, want %q", got.Message, tc.wantMsg)
			}
			// An unknown verdict must never claim to have decided anything.
			if !got.Known && got.LastError == "" {
				t.Fatalf("unknown verdict carries no LastError to explain itself")
			}
		})
	}
}

// TestActiveVerdictCarriesNoMessage guards the banner: a healthy lab must not
// produce operator-facing text, or the UI would show a message with nothing
// wrong.
func TestActiveVerdictCarriesNoMessage(t *testing.T) {
	v := interpretResponse(200, []byte(`{"success":true,"active":true,"status":"active"}`), nil)
	if v.Message != "" {
		t.Fatalf("an active lab produced a message: %q", v.Message)
	}
}

// newTestChecker points a Checker at a URL with timings compressed so the
// tests don't sleep for real.
func newTestChecker(url string) *Checker {
	c := New(url, "", "ARX1", "ARX", "TEST-PC", "0.0.0-test")
	c.StaleAfter = time.Millisecond
	c.PollInterval = time.Hour
	c.InactiveHold = 20 * time.Millisecond
	c.RecheckInterval = 10 * time.Millisecond
	return c
}

// decodeJSON reads a request body as JSON into v.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// TestUnreachableBackendFailsOpen is the outage policy in its default posture:
// a workstation that has never reached the backend keeps transmitting, because
// stranding studies on a box whose sweeper deletes them in 24h is the worse
// failure.
func TestUnreachableBackendFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := newTestChecker(url)
	c.FailOpen = true

	ok, _ := c.Allowed(context.Background(), "notify")
	if !ok {
		t.Fatal("fail-open checker blocked transmission on an unreachable backend")
	}
	if v := c.Snapshot(); v.Known {
		t.Fatalf("an unreachable backend produced a Known verdict: %+v", v)
	}
	if err := c.GateUpload(context.Background()); err != nil {
		t.Fatalf("fail-open GateUpload returned %v, want nil", err)
	}
}

// TestUnreachableBackendFailsClosed is the deny-by-default posture: nothing
// transmits until the backend has confirmed the lab.
func TestUnreachableBackendFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := newTestChecker(url)
	c.FailOpen = false

	if ok, _ := c.Allowed(context.Background(), "notify"); ok {
		t.Fatal("fail-closed checker allowed transmission with no verdict")
	}
	err := c.GateUpload(context.Background())
	if !errors.Is(err, ErrInactive) {
		t.Fatalf("GateUpload returned %v, want ErrInactive", err)
	}
}

// TestKnownVerdictSurvivesOutage is the load-bearing half of the failure
// policy: once the backend has actually answered, a later outage must not be
// able to overturn that answer in either direction.
func TestKnownVerdictSurvivesOutage(t *testing.T) {
	var reply atomic.Value // func(http.ResponseWriter)
	reply.Store(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"active":false,"status":"suspended","message":"Contact Achyu."}`)
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply.Load().(func(http.ResponseWriter))(w)
	}))
	defer srv.Close()

	c := newTestChecker(srv.URL)
	c.FailOpen = true // would say "active" if the outage were allowed to decide

	v := c.Refresh(context.Background(), "manual")
	if !v.Known || v.Active {
		t.Fatalf("first verdict = %+v, want known inactive", v)
	}

	// The backend now falls over. Fail-open must NOT resurrect this lab.
	reply.Store(func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })

	v = c.Refresh(context.Background(), "poll")
	if v.Active {
		t.Fatal("a 5xx upgraded a known-inactive lab to active")
	}
	if v.Message != "Contact Achyu." {
		t.Fatalf("outage discarded the operator message: %q", v.Message)
	}

	// And the same in reverse: a known-active lab is not blocked by an outage.
	reply.Store(func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"success":true,"active":true,"status":"active"}`)
	})
	if v = c.Refresh(context.Background(), "manual"); !v.Active {
		t.Fatalf("reactivation not picked up: %+v", v)
	}

	reply.Store(func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) })
	if v = c.Refresh(context.Background(), "poll"); !v.Active {
		t.Fatal("an outage blocked a known-active lab")
	}
}

// TestRefreshForcesACheck pins the distinction the "Check again" button
// depends on: Allowed may answer from cache, Refresh may not. On Windows the
// monotonic clock is coarse enough that two calls in the same tick see zero
// age, which is how this surfaced in the field.
func TestRefreshForcesACheck(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"success":true,"active":true}`)
	}))
	defer srv.Close()

	c := newTestChecker(srv.URL)
	c.StaleAfter = time.Hour // cache everything

	c.Refresh(context.Background(), "startup")
	after := atomic.LoadInt32(&hits)

	// Allowed must be served from cache.
	c.Allowed(context.Background(), "notify")
	if got := atomic.LoadInt32(&hits); got != after {
		t.Fatalf("Allowed hit the backend %d extra times despite a fresh verdict", got-after)
	}

	// Refresh must not be.
	c.Refresh(context.Background(), "manual")
	if got := atomic.LoadInt32(&hits); got != after+1 {
		t.Fatalf("Refresh made %d requests, want exactly 1", got-after)
	}
}

// TestConcurrentRefreshesCollapse keeps a busy ingest path from turning into a
// request storm: whichever caller wins the race makes the one request and the
// rest wait on it.
func TestConcurrentRefreshesCollapse(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release // hold the request open so the others pile up behind it
		fmt.Fprint(w, `{"success":true,"active":true}`)
	}))
	defer srv.Close()

	c := newTestChecker(srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Refresh(context.Background(), "poll")
		}()
	}

	// Give the goroutines a moment to queue up behind the in-flight request.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("8 concurrent refreshes made %d requests, want 1", got)
	}
	if v := c.Snapshot(); !v.Active {
		t.Fatalf("collapsed refresh lost the verdict: %+v", v)
	}
}

// TestDisabledCheckerMakesNoRequests: an empty backend URL must cost nothing at
// all, not merely always answer yes.
func TestDisabledCheckerMakesNoRequests(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	c := New("", "", "ARX1", "ARX", "TEST-PC", "0.0.0-test")

	if c.Enabled() {
		t.Fatal("a checker with no backend URL reports itself enabled")
	}
	if ok, _ := c.Allowed(context.Background(), "notify"); !ok {
		t.Fatal("disabled checker blocked a notification")
	}
	if err := c.GateUpload(context.Background()); err != nil {
		t.Fatalf("disabled GateUpload returned %v, want nil", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("disabled checker made %d requests", got)
	}
	if v := c.Snapshot(); v.Enabled || !v.Active {
		t.Fatalf("disabled snapshot = %+v, want disabled+active", v)
	}
}

// TestGateUploadReleasesOnReactivation: the upload path must resume on its own
// once the lab comes back, without waiting for the next poll tick.
func TestGateUploadReleasesOnReactivation(t *testing.T) {
	var active atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"success":true,"active":%v}`, active.Load())
	}))
	defer srv.Close()

	c := newTestChecker(srv.URL)
	c.InactiveHold = 2 * time.Second

	// Seed a known-inactive verdict, then reactivate while GateUpload holds.
	c.Refresh(context.Background(), "startup")
	go func() {
		time.Sleep(50 * time.Millisecond)
		active.Store(true)
	}()

	start := time.Now()
	if err := c.GateUpload(context.Background()); err != nil {
		t.Fatalf("GateUpload returned %v after reactivation, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > c.InactiveHold {
		t.Fatalf("GateUpload took %v, longer than the hold — it did not re-check", elapsed)
	}
}

// TestGateUploadRespectsContextCancellation: shutdown must not be stalled by a
// worker parked in the hold loop.
func TestGateUploadRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"active":false}`)
	}))
	defer srv.Close()

	c := newTestChecker(srv.URL)
	c.InactiveHold = time.Hour // would hang forever without cancellation

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := c.GateUpload(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GateUpload returned %v, want context.Canceled", err)
	}
}

// TestRequestCarriesIdentity: the backend looks the lab up by these fields, so
// a rename on either side must fail here rather than in the field.
func TestRequestCarriesIdentity(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSON(r, &body)
		got <- body
		fmt.Fprint(w, `{"success":true,"active":true}`)
	}))
	defer srv.Close()

	c := newTestChecker(srv.URL)
	c.Refresh(context.Background(), "notify")

	body := <-got
	for key, want := range map[string]string{
		"lab_id":        "ARX1",
		"org_id":        "ARX",
		"hostname":      "TEST-PC",
		"agent_version": "0.0.0-test",
		"reason":        "notify",
	} {
		if body[key] != want {
			t.Errorf("request %s = %v, want %q", key, body[key], want)
		}
	}
}
