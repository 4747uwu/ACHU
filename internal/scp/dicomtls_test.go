package scp

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startTestServer brings up an SCP on a random port with the supplied TLS
// options and returns its address.
func startTestServer(t *testing.T, opts TLSOptions) (*Server, string) {
	t.Helper()

	tlsCfg, err := BuildServerTLSConfig(opts)
	if err != nil {
		t.Fatalf("build tls config: %v", err)
	}

	srv := New(Config{
		Address:     "127.0.0.1:0",
		AETitle:     "ACHYU",
		IdleTimeout: 5 * time.Second,
		TLSConfig:   tlsCfg,
	}, &recordingHandler{})

	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		shutdown, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Stop(shutdown)
		cancel()
	})

	addr := srv.listenerAddr()
	if addr == "" {
		t.Fatal("listener didn't bind")
	}
	return srv, addr
}

// associate runs an A-ASSOCIATE-RQ/AC exchange over an already-connected
// stream and returns the echoed called AE title from the AC.
func associate(t *testing.T, conn net.Conn, calledAE, callingAE string) string {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	rq := buildClientAssociateRQ(calledAE, callingAE, []proposedContext{
		{ID: 1, AbstractSyntax: UIDVerificationSOPClass, TransferSyntaxes: []string{UIDImplicitVRLittleEndian}},
	})
	if err := writePDU(conn, pduTypeAssociateRQ, rq); err != nil {
		t.Fatalf("send associate-rq: %v", err)
	}

	ptype, body, err := readPDU(conn)
	if err != nil {
		t.Fatalf("read associate-ac: %v", err)
	}
	if ptype != pduTypeAssociateAC {
		t.Fatalf("called AE %q was rejected: got PDU 0x%02x, want associate-ac", calledAE, ptype)
	}
	if len(body) < 68 {
		t.Fatalf("associate-ac body too short: %d", len(body))
	}
	// Bytes 4..20 of the AC body are the called AE title.
	return trimAE(body[4:20])
}

// TestDualTransportOnOnePort is the whole point of the sniffing listener: a
// site with one modality on Secure DICOM and another on plaintext must work
// without splitting them across two ports or reconfiguring the plaintext one.
func TestDualTransportOnOnePort(t *testing.T) {
	_, addr := startTestServer(t, TLSOptions{Enabled: true, CacheDir: t.TempDir()})

	t.Run("plaintext still associates", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		associate(t, conn, "ACHYU", "CT-SCANNER")
	})

	t.Run("tls associates on the same port", func(t *testing.T) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("tls dial: %v", err)
		}
		defer conn.Close()
		associate(t, conn, "ACHYU", "XRAY-SECURE")
	})
}

// TestLegacyTLS10ClientIsAccepted pins the reason MinVersion is set by hand:
// Go's own default would refuse the field X-ray that prompted this work.
func TestLegacyTLS10ClientIsAccepted(t *testing.T) {
	_, addr := startTestServer(t, TLSOptions{Enabled: true, CacheDir: t.TempDir()})

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS10,
		CipherSuites:       []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA},
	})
	if err != nil {
		t.Fatalf("a TLS 1.0 client could not connect: %v", err)
	}
	defer conn.Close()

	if v := conn.ConnectionState().Version; v != tls.VersionTLS10 {
		t.Fatalf("negotiated version = %s, want TLS1.0", tlsVersionName(v))
	}
	associate(t, conn, "ACHYU", "OLD-XRAY")
}

// TestSelfSignedCertIsCachedAndReused: a modality that pinned the server
// certificate must keep working across a restart, so the certificate cannot be
// regenerated on every boot.
func TestSelfSignedCertIsCachedAndReused(t *testing.T) {
	dir := t.TempDir()
	opts := TLSOptions{Enabled: true, CacheDir: dir}

	first, err := BuildServerTLSConfig(opts)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := BuildServerTLSConfig(opts)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if string(first.Certificates[0].Certificate[0]) != string(second.Certificates[0].Certificate[0]) {
		t.Fatal("certificate was regenerated instead of reused from the cache")
	}

	certPath := filepath.Join(dir, "tls", certFileName)
	keyPath := filepath.Join(dir, "tls", keyFileName)
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("certificate not cached: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("key not cached: %v", err)
	}
	// Permissions are advisory on Windows, so only assert where they mean
	// something.
	if os.PathSeparator == '/' && info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestTLSMinVersionAndLegacyCiphers pins the negotiation envelope: old suites
// present because modalities need them, RC4 absent unless explicitly opted in.
func TestTLSMinVersionAndLegacyCiphers(t *testing.T) {
	cfg, err := BuildServerTLSConfig(TLSOptions{Enabled: true, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if cfg.MinVersion != tls.VersionTLS10 {
		t.Fatalf("MinVersion = %s, want TLS1.0", tlsVersionName(cfg.MinVersion))
	}
	if cfg.PreferServerCipherSuites {
		t.Fatal("server cipher preference is on; old stacks must be allowed to choose")
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatal("client certificates are being requested; modalities rarely have one")
	}

	has := func(suite uint16) bool {
		for _, s := range cfg.CipherSuites {
			if s == suite {
				return true
			}
		}
		return false
	}

	for _, want := range []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
	} {
		if !has(want) {
			t.Errorf("legacy suite %s missing", tls.CipherSuiteName(want))
		}
	}
	if has(tls.TLS_RSA_WITH_RC4_128_SHA) {
		t.Error("RC4 is enabled without allow_legacy_rc4")
	}

	optIn, err := BuildServerTLSConfig(TLSOptions{Enabled: true, CacheDir: t.TempDir(), AllowLegacyRC4: true})
	if err != nil {
		t.Fatalf("build with rc4: %v", err)
	}
	found := false
	for _, s := range optIn.CipherSuites {
		if s == tls.TLS_RSA_WITH_RC4_128_SHA {
			found = true
		}
	}
	if !found {
		t.Error("allow_legacy_rc4 did not enable RC4")
	}
}

// TestTLSDisabledYieldsNoConfig: switching TLS off must produce a nil config,
// which is what makes the sniff a no-op rather than a per-connection cost.
func TestTLSDisabledYieldsNoConfig(t *testing.T) {
	cfg, err := BuildServerTLSConfig(TLSOptions{Enabled: false, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if cfg != nil {
		t.Fatal("disabled TLS still produced a config")
	}
}

// TestPlaintextUnaffectedWhenTLSDisabled guards the fallback path used when a
// certificate cannot be built: plaintext modalities must be untouched.
func TestPlaintextUnaffectedWhenTLSDisabled(t *testing.T) {
	_, addr := startTestServer(t, TLSOptions{Enabled: false})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	associate(t, conn, "ACHYU", "CT-SCANNER")
}
