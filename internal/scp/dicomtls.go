package scp

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
)

// Secure DICOM on the same port as plaintext.
//
// The problem this solves came from a live site: an X-ray unit was configured
// for "Secure DICOM" and could not associate at all, while a CT on the same
// switch sent plaintext. Giving the X-ray its own port would have meant
// sending an engineer to reconfigure the CT as well.
//
// A DICOM A-ASSOCIATE-RQ PDU starts with 0x01 and a TLS ClientHello starts
// with 0x16 (TLS record type "handshake"), so one peeked byte tells the two
// apart with no ambiguity and no configuration.

const (
	// tlsHandshakeByte is the TLS record type for "handshake", the first byte
	// of every ClientHello.
	tlsHandshakeByte = 0x16

	// sniffTimeout bounds how long a connection may stay silent before we give
	// up on identifying it. A modality that connects and says nothing must not
	// hold a goroutine indefinitely.
	sniffTimeout = 15 * time.Second

	certFileName = "dicom-tls-cert.pem"
	keyFileName  = "dicom-tls-key.pem"
)

// TLSOptions configures Secure DICOM.
type TLSOptions struct {
	Enabled bool
	// CertFile and KeyFile point at an operator-supplied certificate. Leave
	// both empty to generate and cache a self-signed one.
	CertFile string
	KeyFile  string
	// CacheDir is where the self-signed certificate is kept between runs.
	CacheDir string
	// AllowLegacyRC4 additionally enables RC4 cipher suites. Off by default;
	// only worth setting for a modality that implements nothing else.
	AllowLegacyRC4 bool
}

// peekedConn re-serves the byte we peeked at, so whichever reader comes next —
// the TLS handshake or the DICOM PDU parser — sees the stream from its start.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// maybeWrapTLS identifies a freshly accepted connection and wraps it in TLS if
// the peer opened with a ClientHello. It reports whether TLS was applied.
//
// The returned net.Conn is ALWAYS non-nil, including on error. It used to be
// nil on a failed sniff, and the caller assigned that over its own conn
// variable before a deferred Close() ran on it — one abandoned connection then
// panicked the process. Returning the original socket keeps the contract
// simple: whatever comes back is still the caller's to close.
func (s *Server) maybeWrapTLS(conn net.Conn) (net.Conn, bool, error) {
	if s.cfg.TLSConfig == nil {
		return conn, false, nil
	}

	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(sniffTimeout))
	first, err := br.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return conn, false, fmt.Errorf("sniff first byte: %w", err)
	}

	buffered := &peekedConn{Conn: conn, r: br}
	if first[0] != tlsHandshakeByte {
		return buffered, false, nil
	}
	return tls.Server(buffered, s.cfg.TLSConfig), true, nil
}

// BuildServerTLSConfig produces the TLS configuration for the DICOM listener,
// or nil when TLS is switched off.
//
// The settings below are deliberately permissive and must NOT be copied to an
// internet-facing listener. They are defensible here because this listener
// lives on the clinic LAN and receives from hardware on the same switch; the
// onward hop to the central PACS is a separate connection that uses modern TLS
// with certificate verification.
func BuildServerTLSConfig(opts TLSOptions) (*tls.Config, error) {
	if !opts.Enabled {
		return nil, nil
	}

	cert, err := loadOrCreateCert(opts)
	if err != nil {
		return nil, err
	}

	suites := []uint16{
		// Modern first — a capable modality should still get a good suite.
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
		// TLS 1.0-era suites, for imaging hardware that shipped a decade ago
		// and will never be updated.
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
	}
	if opts.AllowLegacyRC4 {
		suites = append(suites, tls.TLS_RSA_WITH_RC4_128_SHA, tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Set explicitly, not left to the default: Go 1.22+ servers reject TLS
		// 1.0/1.1 unless MinVersion says otherwise, and the field X-ray that
		// prompted this work opens with a TLS 1.0 ClientHello.
		MinVersion:   tls.VersionTLS10,
		CipherSuites: suites,
		// Let the client choose. Old stacks frequently implement exactly one
		// suite correctly, and imposing a server preference is how you end up
		// negotiating the one they get wrong.
		PreferServerCipherSuites: false,
		// Modalities rarely have a client certificate, and asking for one
		// confuses several older stacks into aborting the handshake.
		ClientAuth: tls.NoClientCert,
	}, nil
}

// loadOrCreateCert returns the operator-supplied certificate when configured,
// otherwise a self-signed one cached under CacheDir.
func loadOrCreateCert(opts TLSOptions) (tls.Certificate, error) {
	if opts.CertFile != "" && opts.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("load dicom tls keypair: %w", err)
		}
		log.L().Info("dicom tls using configured certificate", "cert", opts.CertFile)
		return cert, nil
	}

	dir := filepath.Join(opts.CacheDir, "tls")
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	// Reuse the cached certificate if it is still there. This matters more
	// than it looks: a modality that pinned the server certificate keeps
	// working across an upgrade only because we don't regenerate on restart.
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		log.L().Info("dicom tls using cached self-signed certificate", "path", certPath)
		return cert, nil
	}

	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tls.Certificate{}, fmt.Errorf("create tls dir: %w", err)
	}
	// Key first and 0600: if the process dies between the two writes, a
	// readable key next to no certificate is the worse of the two states.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write tls key: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("write tls cert: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	log.L().Info("dicom tls generated self-signed certificate", "path", certPath)
	return cert, nil
}

// generateSelfSigned builds a certificate for this workstation.
func generateSelfSigned() (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate rsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   host,
			Organization: []string{"Achyu PACS"},
		},
		// Backdated an hour: workstation clocks drift, and a certificate that
		// is not yet valid fails in a way nobody at the site can diagnose.
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// Some older stacks refuse a self-signed leaf that isn't also a CA.
		IsCA:     true,
		DNSNames: []string{host, "localhost"},
	}
	tmpl.IPAddresses = localIPs()

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// tlsVersionName renders a negotiated TLS version for the log. Knowing a site
// negotiated 1.0 rather than 1.2 is the first thing you want when a modality
// starts failing after a Windows update.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS1.0"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	}
	return fmt.Sprintf("0x%04x", v)
}

// localIPs collects every address this host answers on, so a modality
// configured by IP rather than by name still validates the certificate.
func localIPs() []net.IP {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			ips = append(ips, ipnet.IP)
		}
	}
	return ips
}
