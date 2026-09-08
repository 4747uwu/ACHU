// Package netdns gives the sender its own DNS resolution, independent of
// whatever nameserver the site's router hands out over DHCP.
//
// # Why
//
// A site's router was answering DHCP with a DNS server that had stopped
// resolving. Every outbound call — the study notifier, the activation gate,
// the peer probe, the STOW-RS upload — stalled at name resolution, and the
// only fix was to visit the site and change the router's DHCP settings. One
// change here ships to every site and touches no client network.
//
// net.Resolver.PreferGo is what makes this possible: it selects Go's own DNS
// client instead of the Windows resolver, so the router's nameserver is out of
// the picture entirely. Verified working on go1.20.14/386 — the toolchain this
// binary ships with — not assumed.
//
// # Why the fallback is at the query level
//
// The obvious shape is one resolver whose Dial tries 8.8.8.8, then 1.1.1.1,
// then the system server. It does not work: a UDP "dial" creates a socket
// without sending anything, so it succeeds even against a completely
// unreachable address. Every request would pin to the first nameserver in the
// list and the rest of the chain would be unreachable code.
//
// That matters at sites where DNS is fine but public resolvers are blocked or
// intercepted, which is common behind clinic CPE: those sites would go from
// working to timing out. So each nameserver gets its own resolver here and the
// fallback happens when a QUERY fails, which is a thing we can actually
// observe. A site with working DNS and blocked public resolvers falls through
// to the system resolver and behaves exactly as it did before.
package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
)

const (
	// queryTimeout bounds a whole lookup, not one nameserver, because every
	// nameserver is now asked at the same time.
	//
	// It used to bound each one in turn, and a site that blackholed both public
	// resolvers paid the sum: 8 seconds on EVERY lookup, which was more than
	// the notifier's entire request budget. Asking in parallel makes a dead
	// nameserver cost nothing at all -- the answer arrives when the FASTEST
	// working resolver replies -- so this can be generous again. It is only
	// ever reached when every resolver fails.
	queryTimeout = 5 * time.Second

	// nsDialTimeout is how long we wait to open the socket to a nameserver.
	nsDialTimeout = 3 * time.Second

	// connectTimeout bounds one TCP connection attempt to a resolved address.
	connectTimeout = 10 * time.Second

	// warnInterval rate-limits resolver-fallback warnings. A site with broken
	// DNS makes this happen on every request, and the log file is itself
	// shipped to the backend.
	warnInterval = 5 * time.Minute
)

// publicNameservers are tried before the system resolver. Two independent
// operators, so one provider's outage is not ours.
var publicNameservers = []string{
	"8.8.8.8:53", // Google
	"1.1.1.1:53", // Cloudflare
}

// resolverFor builds a resolver that sends every query to exactly one
// nameserver, bypassing the Windows resolver.
func resolverFor(ns string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: nsDialTimeout}
			// Honour the network Go asked for. It retries over TCP when a UDP
			// answer comes back truncated, and forcing UDP there would drop
			// large responses.
			return d.DialContext(ctx, network, ns)
		},
	}
}

type namedResolver struct {
	name string
	r    *net.Resolver
}

var (
	chainOnce sync.Once
	chain     []namedResolver

	warnMu   sync.Mutex
	lastWarn time.Time
)

func resolvers() []namedResolver {
	chainOnce.Do(func() {
		for _, ns := range publicNameservers {
			chain = append(chain, namedResolver{name: ns, r: resolverFor(ns)})
		}
		// Last resort: whatever the machine is configured to use. This is what
		// keeps sites with working DNS and blocked public resolvers working.
		chain = append(chain, namedResolver{name: "system", r: net.DefaultResolver})
	})
	return chain
}

// LookupHost resolves host by asking every nameserver AT ONCE and taking the
// first usable answer, along with the name of the resolver that produced it.
//
// # Why all at once
//
// Asking in turn made a blocked nameserver cost real time on every single
// lookup — 4s each, 8s total at a site that blackholes both public resolvers,
// which is more than some callers' entire request budget. In parallel a dead
// nameserver costs nothing: the answer arrives when the fastest WORKING one
// replies, and the rest are simply ignored.
//
// # Why NXDOMAIN no longer wins
//
// This used to stop the moment any nameserver said "no such host", on the
// reasoning that a definitive answer cannot be improved on. That is true of an
// honest resolver and false of the ones these sites actually have: an ISP that
// filters by DNS answers NXDOMAIN for a name that exists perfectly well, and
// believing it took a live site's uploads down while a public resolver was
// sitting there with the right address. So NXDOMAIN is now only believed when
// EVERY resolver agrees on it, and any real answer beats it.
func LookupHost(ctx context.Context, host string) ([]string, string, error) {
	all := resolvers()

	type answer struct {
		addrs []string
		name  string
		err   error
	}

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	// Cancelling on the way out stops the losers mid-query; they have nowhere
	// to report to once an answer has been returned.
	defer cancel()

	ch := make(chan answer, len(all))
	for _, nr := range all {
		go func(nr namedResolver) {
			addrs, err := nr.r.LookupHost(qctx, host)
			if err == nil && len(addrs) == 0 {
				err = fmt.Errorf("no addresses returned")
			}
			ch <- answer{addrs: addrs, name: nr.name, err: err}
		}(nr)
	}

	var (
		firstErr    error
		notFoundAll = true
		seen        int
	)
	for seen = 0; seen < len(all); seen++ {
		a := <-ch
		if a.err == nil {
			// A real answer, and the earliest one to arrive.
			//
			// Only worth a warning if something actually FAILED first. Which
			// resolver wins a race carries no fault information — 8.8.8.8
			// answering before the site's own nameserver is the normal, healthy
			// case, and logging that as "a nameserver did not answer" sent a
			// field engineer hunting a DNS problem that did not exist.
			if firstErr != nil {
				warnFallback(host, a.name, firstErr)
			}
			return a.addrs, a.name, nil
		}
		if firstErr == nil {
			firstErr = a.err
		}
		var dnsErr *net.DNSError
		if !errors.As(a.err, &dnsErr) || !dnsErr.IsNotFound {
			notFoundAll = false
		}
	}

	// Everyone failed. When they all agree the name does not exist, say that;
	// it is the one case where NXDOMAIN is trustworthy.
	if notFoundAll && firstErr != nil {
		return nil, "", fmt.Errorf("resolve %s: %w", host, firstErr)
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no nameserver answered")
	}
	return nil, "", fmt.Errorf("resolve %s: %w", host, firstErr)
}

// warnFallback reports that the site's own DNS is not answering, rate-limited.
func warnFallback(host, used string, prevErr error) {
	warnMu.Lock()
	quiet := time.Since(lastWarn) < warnInterval
	if !quiet {
		lastWarn = time.Now()
	}
	warnMu.Unlock()
	if quiet {
		return
	}

	log.L().Warn("a nameserver failed; another answered",
		"host", host,
		"answered_by", used,
		"failure", prevErr,
	)
}

// DialContext resolves and connects, using our own resolver chain.
//
// Plug this into an http.Transport to make every request from that client
// independent of the site's nameserver.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	// Already an IP — nothing to resolve. The peer is often configured by
	// address, and those sites should not pay for a lookup at all.
	if net.ParseIP(host) != nil {
		d := net.Dialer{Timeout: connectTimeout}
		return d.DialContext(ctx, network, address)
	}

	addrs, _, err := LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, ip := range preferIPv4(addrs) {
		d := net.Dialer{Timeout: connectTimeout}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses for %s", host)
	}
	return nil, lastErr
}

// preferIPv4 orders IPv4 addresses ahead of IPv6, keeping the relative order
// within each family.
//
// IPv6 can stay switched on: we still try those addresses, just second. Doing
// it the other way round is what hurts at these sites — a clinic LAN often
// advertises IPv6 with no working route to the internet, and a AAAA-first
// dial then burns the full connect timeout before falling back to the address
// that was always going to work.
func preferIPv4(addrs []string) []string {
	out := make([]string, len(addrs))
	copy(out, addrs)
	sort.SliceStable(out, func(i, j int) bool {
		return isIPv4(out[i]) && !isIPv4(out[j])
	})
	return out
}

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// Resolver queries the public nameservers directly, bypassing the OS resolver
// and therefore the site's router.
//
// Exposed for callers that need a *net.Resolver specifically. Note that a
// single net.Resolver cannot express the fall-through chain — LookupHost and
// DialContext do that — so this one sends every query to the first public
// nameserver and lets Go's own retry handle the rest.
var Resolver = resolverFor(publicNameservers[0])

// Dialer returns a net.Dialer whose name lookups bypass the OS resolver.
func Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Resolver: Resolver}
}

// isrgRootX2 is the root our backend's certificate chains to.
//
//go:embed isrg-root-x2.pem
var isrgRootX2 []byte

var (
	pinnedOnce sync.Once
	pinnedPool *x509.CertPool
)

// pinnedRoots returns the embedded root pool, or nil if it will not parse.
func pinnedRoots() *x509.CertPool {
	pinnedOnce.Do(func() {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(isrgRootX2) {
			pinnedPool = pool
		} else {
			log.L().Warn("embedded CA root failed to parse — falling back to the platform verifier")
		}
	})
	return pinnedPool
}

// verifyConnection checks the server's chain against the embedded root first
// and only then asks the platform.
//
// # Why this exists
//
// Go hands certificate verification to the OS. On a Windows 7 site that call
// took 15.3 SECONDS, measured on the machine — CryptoAPI goes to the network
// mid-handshake for revocation and root-list updates, and on that clinic's
// line those fetches hang until they time out internally. Verification then
// SUCCEEDS, which is what made this so hard to see: nothing was untrusted and
// nothing errored. It was simply slower than the caller's timeout, so every
// backend call died at 10s while the app's own login — which goes through
// Chromium, with its own verifier — worked perfectly on the same machine, to
// the same URL, at the same moment.
//
// The identical handshake verified in pure Go against the pinned root took
// 202ms. Same chain, same server, same result: 75x faster because Windows is
// not in the path.
//
// # Why this is not a relaxation
//
// The chain is still fully verified, including hostname and expiry — just by
// Go rather than by the OS. What is skipped is only the OS's own network
// round trip. And the platform verifier remains as a fallback, so a backend
// chaining to some other CA keeps working exactly as before; it merely pays
// the slow path this exists to avoid.
func verifyConnection(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: server presented no certificate")
	}

	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	opts := x509.VerifyOptions{
		DNSName:       cs.ServerName,
		Intermediates: inter,
	}

	if roots := pinnedRoots(); roots != nil {
		opts.Roots = roots
		if _, err := cs.PeerCertificates[0].Verify(opts); err == nil {
			return nil
		}
	}

	// Nil Roots means the platform verifier. Slow on an affected Windows box,
	// but correct, and the only thing that can vouch for a CA we do not ship.
	opts.Roots = nil
	_, err := cs.PeerCertificates[0].Verify(opts)
	return err
}

// pinnedTLS returns the TLS config every client here uses.
//
// InsecureSkipVerify disables only the BUILT-IN check, which is then replaced
// by verifyConnection above — Go still calls VerifyConnection and still honours
// its error. Skipping verification outright would be a one-word change from
// this and is exactly what must never happen, so the two are kept together.
func pinnedTLS() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // replaced by VerifyConnection, not removed
		VerifyConnection:   verifyConnection,
	}
}

// NewTransport builds an *http.Transport wired to the resilient resolver.
// Use this when you need to customise the client (e.g. add TLS roots) but
// still want DNS to bypass the OS.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// DialContext, not Dialer().DialContext: this is the path that gets
		// the full nameserver fall-through and the IPv4-first ordering.
		DialContext:           DialContext,
		TLSClientConfig:       pinnedTLS(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewClient returns an *http.Client whose name lookups bypass the OS resolver.
// timeout bounds the whole request (connect + TLS + response).
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: NewTransport(),
	}
}

// DescribeError turns a resolution failure into something an engineer reading
// the log can act on, instead of a generic dial error that gets guessed at.
// It returns "" when err is not a DNS failure.
func DescribeError(err error) string {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return ""
	}
	switch {
	case dnsErr.IsNotFound:
		return fmt.Sprintf("hostname %q does not exist (NXDOMAIN)", dnsErr.Name)
	case dnsErr.IsTimeout:
		return fmt.Sprintf("DNS resolution for %q timed out — no nameserver answered", dnsErr.Name)
	default:
		return fmt.Sprintf("DNS resolution for %q failed: %s", dnsErr.Name, dnsErr.Err)
	}
}

// LogIfDNS writes an explicit warning when err is a name-resolution failure.
// Callers pass the operation for context.
func LogIfDNS(op string, err error) bool {
	msg := DescribeError(err)
	if msg == "" {
		return false
	}
	log.L().Warn("DNS resolution failed — site DNS server not responding", "op", op, "detail", msg)
	return true
}
