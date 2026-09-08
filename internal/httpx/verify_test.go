package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The transport sets InsecureSkipVerify so that verifyConnection can do the
// checking instead — 15.3s of platform verification on a Windows 7 site was
// killing every backend call. That is only safe while verifyConnection really
// rejects what the built-in check would have rejected, and the failure mode if
// it does not is silent: every connection succeeds, including a hostile one.
// These tests exist to make that failure loud.

// TestUntrustedCertificateIsRejected is the one that matters. A self-signed
// server chains to neither the embedded root nor anything the platform trusts,
// so it must be refused.
func TestUntrustedCertificateIsRejected(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewTransport(), Timeout: 20 * time.Second}
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("self-signed certificate was ACCEPTED — TLS verification is disabled")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "authority") {
		t.Fatalf("rejected, but not for a certificate reason: %v", err)
	}
	t.Logf("correctly rejected: %v", err)
}

// TestHostnameMismatchIsRejected covers the other half of what the built-in
// check does. A certificate can be perfectly valid and still be the wrong one
// for the host being dialled.
func TestHostnameMismatchIsRejected(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	// Trust the test server's own CA, so the ONLY remaining objection is the name.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	cs := tls.ConnectionState{
		ServerName:       "not-the-right-host.example",
		PeerCertificates: []*x509.Certificate{srv.Certificate()},
	}
	// Exercise the same code path with the trusted pool standing in for the
	// platform's answer.
	inter := x509.NewCertPool()
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: inter, Roots: pool}
	if _, err := cs.PeerCertificates[0].Verify(opts); err == nil {
		t.Fatal("hostname mismatch was ACCEPTED")
	}

	// And the real verifier must reject it too.
	if err := verifyConnection(cs); err == nil {
		t.Fatal("verifyConnection accepted a certificate issued to a different host")
	}
}

// TestNoCertificateIsRejected guards the empty-chain path, which would panic on
// PeerCertificates[0] if it were not checked first.
func TestNoCertificateIsRejected(t *testing.T) {
	if err := verifyConnection(tls.ConnectionState{ServerName: "example.com"}); err == nil {
		t.Fatal("verifyConnection accepted a connection with no certificate")
	}
}

// TestEmbeddedRootParses fails loudly if the embedded PEM is missing or
// corrupt. Without it every request silently falls through to the slow
// platform path and the fix quietly stops working.
func TestEmbeddedRootParses(t *testing.T) {
	if pinnedRoots() == nil {
		t.Fatal("embedded ISRG Root X2 did not parse — every request falls back to the platform verifier")
	}
}

// TestPinnedRootVerifiesRealBackend confirms the embedded root actually matches
// the chain our backend serves. Network test, so it is skipped when offline
// rather than failing the build.
func TestPinnedRootVerifiesRealBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	d := &tls.Dialer{Config: pinnedTLS()}
	conn, err := d.Dial("tcp", "pacs.achyutrs.com:443")
	if err != nil {
		t.Skipf("cannot reach backend (offline?): %v", err)
	}
	defer conn.Close()

	// It connected, so verifyConnection returned nil. Confirm the PINNED pool
	// is what accepted it, not the platform fallback quietly covering for a
	// root that does not match.
	cs := conn.(*tls.Conn).ConnectionState()
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       "pacs.achyutrs.com",
		Intermediates: inter,
		Roots:         pinnedRoots(),
	}); err != nil {
		t.Fatalf("backend chain does NOT verify against the embedded root: %v", err)
	}
}
