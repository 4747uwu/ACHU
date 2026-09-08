package httpx

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPreferGoBypassesTheSystemResolver is the load-bearing assumption of this
// whole package: that PreferGo actually takes effect on this platform and
// toolchain. Go ignored it on Windows for years, so it is verified here rather
// than assumed — if a toolchain bump ever regresses it, this fails instead of
// the sender silently going back to the site's broken nameserver.
func TestPreferGoBypassesTheSystemResolver(t *testing.T) {
	var called int32

	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			atomic.AddInt32(&called, 1)
			// Nothing needs to succeed; being asked at all is the proof.
			return nil, errors.New("test resolver")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = r.LookupHost(ctx, "example.invalid")

	if atomic.LoadInt32(&called) == 0 {
		t.Fatal("PreferGo did not take effect: the OS resolver was used and our " +
			"nameservers would be bypassed entirely")
	}
}

// TestUDPDialSucceedsEvenWhenUnreachable documents WHY the fallback is at the
// query level rather than inside a single resolver's Dial.
//
// A UDP dial only creates a socket; it sends nothing. So the obvious
// "try 8.8.8.8, else 1.1.1.1, else system" written inside Dial can never
// advance past the first entry, and a site that blocks outbound UDP/53 would
// break instead of falling back.
func TestUDPDialSucceedsEvenWhenUnreachable(t *testing.T) {
	d := net.Dialer{Timeout: 2 * time.Second}
	// TEST-NET-1, reserved by RFC 5737 and guaranteed not routable.
	c, err := d.DialContext(context.Background(), "udp", "192.0.2.1:53")
	if err != nil {
		t.Skipf("this platform fails UDP dial to an unroutable address (%v); "+
			"the query-level fallback is still correct, just not necessary here", err)
	}
	c.Close()
	// The dial succeeded against an unroutable address, which is exactly the
	// trap this package avoids.
}

// TestLookupFallsThroughToAWorkingNameserver: a dead nameserver first in the
// chain must cost one timeout, not the resolution.
func TestLookupFallsThroughToAWorkingNameserver(t *testing.T) {
	if testing.Short() {
		t.Skip("needs network")
	}

	dead := namedResolver{name: "192.0.2.1:53", r: resolverFor("192.0.2.1:53")}
	live := namedResolver{name: "8.8.8.8:53", r: resolverFor("8.8.8.8:53")}

	// Exercise the same ordering logic LookupHost uses.
	ctx := context.Background()
	var lastErr error
	var got []string
	var usedName string
	for _, nr := range []namedResolver{dead, live} {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		addrs, err := nr.r.LookupHost(qctx, "one.one.one.one")
		cancel()
		if err == nil && len(addrs) > 0 {
			got, usedName = addrs, nr.name
			break
		}
		lastErr = err
	}

	if len(got) == 0 {
		t.Skipf("no network access in this environment (last error: %v)", lastErr)
	}
	if usedName != "8.8.8.8:53" {
		t.Fatalf("resolved via %q, expected the fallback nameserver", usedName)
	}
	if lastErr == nil {
		t.Fatal("the dead nameserver appeared to succeed — the chain is not being exercised")
	}
}

// TestLookupHostResolvesARealName end-to-end through the public chain.
func TestLookupHostResolvesARealName(t *testing.T) {
	if testing.Short() {
		t.Skip("needs network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	addrs, via, err := LookupHost(ctx, "one.one.one.one")
	if err != nil {
		t.Skipf("no network access in this environment: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("resolved with no addresses")
	}
	if via == "" {
		t.Fatal("no resolver name reported")
	}
	t.Logf("resolved via %s -> %v", via, addrs)
}

// TestNXDOMAINStopsTheChain: a definitive "no such name" is an answer. Asking
// three more nameservers cannot improve it and just adds latency to every
// mistyped hostname.
func TestNXDOMAINStopsTheChain(t *testing.T) {
	if testing.Short() {
		t.Skip("needs network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := LookupHost(ctx, "this-name-should-never-exist-achyu-test.invalid")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a bogus hostname resolved")
	}
	// One query, not the whole chain plus timeouts.
	if elapsed > queryTimeout*2 {
		t.Errorf("NXDOMAIN took %v — the chain did not stop early", elapsed)
	}
}

// TestPreferIPv4 answers the practical question at these sites: IPv6 may stay
// switched on, we simply try the address family that works there first.
func TestPreferIPv4(t *testing.T) {
	in := []string{"2606:4700::1111", "1.1.1.1", "2606:4700::1001", "1.0.0.1"}
	got := preferIPv4(in)

	if got[0] != "1.1.1.1" || got[1] != "1.0.0.1" {
		t.Fatalf("IPv4 not ordered first: %v", got)
	}
	if got[2] != "2606:4700::1111" || got[3] != "2606:4700::1001" {
		t.Fatalf("IPv6 order not preserved: %v", got)
	}
	if len(got) != len(in) {
		t.Fatalf("addresses lost: %v", got)
	}

	// The input must not be mutated — callers may reuse it.
	if in[0] != "2606:4700::1111" {
		t.Fatal("preferIPv4 mutated its input")
	}
}

// TestDialContextWithLiteralIPSkipsResolution: peers are often configured by
// address, and those sites should not pay for a lookup at all.
func TestDialContextWithLiteralIPSkipsResolution(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial to a literal IP failed: %v", err)
	}
	conn.Close()
}

// TestDescribeErrorIsActionable: the point of the message is that the next
// person reads the cause instead of guessing at it.
func TestDescribeErrorIsActionable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not a DNS error", errors.New("connection refused"), ""},
		{"nxdomain", &net.DNSError{Name: "pacs.achyutrs.com", IsNotFound: true}, "does not exist"},
		{"timeout", &net.DNSError{Name: "pacs.achyutrs.com", IsTimeout: true}, "timed out"},
		{"other", &net.DNSError{Name: "pacs.achyutrs.com", Err: "server misbehaving"}, "server misbehaving"},
	}

	for _, tc := range cases {
		got := DescribeError(tc.err)
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s: expected no message, got %q", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not mention %q", tc.name, got, tc.want)
		}
		if !strings.Contains(got, "pacs.achyutrs.com") {
			t.Errorf("%s: message omits the hostname: %q", tc.name, got)
		}
	}
}

// TestSystemResolverIsLastInTheChain: this is what keeps a site with working
// DNS but blocked public resolvers behaving exactly as it did before.
func TestSystemResolverIsLastInTheChain(t *testing.T) {
	chain := resolvers()
	if len(chain) < 2 {
		t.Fatalf("chain has only %d entries", len(chain))
	}
	last := chain[len(chain)-1]
	if last.name != "system" {
		t.Fatalf("last resolver is %q, want the system resolver", last.name)
	}
	if last.r != net.DefaultResolver {
		t.Fatal("the final entry is not the actual system resolver")
	}
	if chain[0].name != "8.8.8.8:53" {
		t.Fatalf("first resolver is %q", chain[0].name)
	}
}
