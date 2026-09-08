// tools/tlsprobe — why does Go fail where Chrome succeeds, on THIS machine?
//
// Built with the same toolchain and GOARCH as the engine, so it exercises the
// identical TLS and certificate-verification path. The app's login goes through
// Chromium and works; the notifier goes through Go and times out, against the
// same URL at the same moment. That narrows the fault to something Go does and
// Chromium does not, and the biggest such difference on Windows is certificate
// verification: Go hands the chain to CryptoAPI, Chromium verifies it itself.
//
// So each phase is timed separately, and the handshake is then repeated with an
// explicit root pool, which makes Go verify in pure Go and never call Windows.
// If the first fails and the second succeeds, the fault is Windows verification
// and pinning the root is the fix.
//
//	GOOS=windows GOARCH=386 go1.20.14 build -o tlsprobe.exe ./tools/tlsprobe/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed isrg-root-x2.pem
var isrgRootX2 []byte

const (
	host    = "pacs.achyutrs.com"
	apiPath = "/api/orthanc2/instance-exe-received"
)

func step(name string) func(error, ...string) {
	fmt.Printf("  %-34s ", name)
	start := time.Now()
	return func(err error, extra ...string) {
		ms := time.Since(start).Milliseconds()
		if err != nil {
			fmt.Printf("FAILED  %5dms\n      %v\n", ms, err)
			return
		}
		fmt.Printf("ok      %5dms   %s\n", ms, strings.Join(extra, " "))
	}
}

func main() {
	fmt.Printf("tlsprobe - %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	// ---- DNS ---------------------------------------------------------------
	done := step("1. resolve " + host)
	addrs, err := net.LookupHost(host)
	done(err, strings.Join(addrs, " "))
	if err != nil {
		os.Exit(1)
	}
	ip := addrs[0]

	// ---- TCP ---------------------------------------------------------------
	done = step("2. TCP " + ip + ":443")
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "443"), 15*time.Second)
	done(err)
	if err != nil {
		os.Exit(1)
	}
	conn.Close()

	// ---- TLS, verified the way the engine does it today --------------------
	// Nil RootCAs on Windows means CryptoAPI builds and checks the chain.
	done = step("3. TLS (Windows verification)")
	winErr := handshake(ip, nil)
	done(winErr)

	// ---- TLS, verified in pure Go against a pinned root ---------------------
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(isrgRootX2) {
		fmt.Println("  embedded root failed to parse - build is broken")
		os.Exit(2)
	}
	done = step("4. TLS (pinned root, pure Go)")
	pinErr := handshake(ip, pool)
	done(pinErr)

	// ---- The actual POST, pinned if that is what worked --------------------
	var rootsForPost *x509.CertPool
	if winErr != nil && pinErr == nil {
		rootsForPost = pool
	}
	done = step("5. POST " + apiPath)
	status, err := post(rootsForPost)
	done(err, status)

	// ---- Verdict -----------------------------------------------------------
	fmt.Println()
	switch {
	case winErr != nil && pinErr == nil:
		fmt.Println("  VERDICT: Windows certificate verification is the fault.")
		fmt.Println("           Pinning the root fixes it. This is fixable in the app.")
	case winErr == nil && pinErr == nil:
		fmt.Println("  VERDICT: TLS is fine both ways. The fault is not the handshake -")
		fmt.Println("           look at step 5 and the request timeout.")
	case winErr != nil && pinErr != nil:
		fmt.Println("  VERDICT: TLS fails both ways, so it is not certificate verification.")
		fmt.Println("           Something is dropping the handshake on the wire.")
	default:
		fmt.Println("  VERDICT: pinned verification failed where Windows succeeded -")
		fmt.Println("           the embedded root does not match this server's chain.")
	}
}

func handshake(ip string, roots *x509.CertPool) error {
	d := &net.Dialer{Timeout: 15 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "443"))
	if err != nil {
		return err
	}
	defer raw.Close()

	c := tls.Client(raw, &tls.Config{ServerName: host, RootCAs: roots})
	defer c.Close()
	if err := c.HandshakeContext(ctx); err != nil {
		return err
	}
	st := c.ConnectionState()
	fmt.Printf("\n      negotiated %s, %d certs from server, cn=%q",
		tlsVersion(st.Version), len(st.PeerCertificates), st.PeerCertificates[0].Subject.CommonName)
	fmt.Printf("\n     ")
	return nil
}

func post(roots *x509.CertPool) (string, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	// Deliberately an empty body: this is a reachability probe, and the point
	// is which HTTP status comes back, not what the backend does with it.
	resp, err := client.Post("https://"+host+apiPath, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return "HTTP " + resp.Status, nil
}

func tlsVersion(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS10:
		return "TLS1.0"
	}
	return fmt.Sprintf("0x%04x", v)
}
