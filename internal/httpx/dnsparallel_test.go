package httpx

import (
	"context"
	"net"
	"testing"
	"time"
)

// swapChain installs a test resolver chain and restores the real one after.
// The chain is a package global, so a test that walks away from it leaves
// every later test looking at fake resolvers.
func swapChain(t *testing.T, c []namedResolver) {
	t.Helper()
	saved := append([]namedResolver(nil), resolvers()...)
	chain = c
	t.Cleanup(func() { chain = saved })
}

// deadNS is a resolver pointed at TEST-NET-1 (RFC 5737), which is guaranteed
// unroutable — a blackholed nameserver, exactly like the public resolvers at
// the affected sites.
func deadNS(addr string) namedResolver {
	return namedResolver{name: addr, r: resolverFor(addr)}
}

// TestBlockedNameserversDoNotDelayTheAnswer is the whole point of asking in
// parallel. Two blackholed resolvers used to cost 4s each, in series, on every
// lookup — more than the notifier's entire 10s budget. Now they cost nothing.
func TestBlockedNameserversDoNotDelayTheAnswer(t *testing.T) {
	swapChain(t, []namedResolver{
		deadNS("192.0.2.1:53"),
		deadNS("192.0.2.2:53"),
		{name: "system", r: net.DefaultResolver},
	})

	start := time.Now()
	addrs, by, err := LookupHost(context.Background(), "localhost")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("lookup failed with a working resolver in the chain: %v", err)
	}
	if by != "system" {
		t.Fatalf("answered by %q, want system", by)
	}
	if len(addrs) == 0 {
		t.Fatal("no addresses returned")
	}
	t.Logf("answered in %v with two blackholed nameservers in the chain", elapsed)

	// Serial would have been 2 x queryTimeout before the working resolver was
	// even asked. One second is far above a loopback lookup and far below that.
	if elapsed > time.Second {
		t.Fatalf("took %v — a dead nameserver is still being waited on", elapsed)
	}
}

// TestNXDOMAINFromOneResolverDoesNotWin reproduces the failure that took a
// Windows 10 site's uploads down.
//
// Its ISP resolver answered "no such host" for router.achyutrs.com, a name
// that exists perfectly well — DNS-level filtering. The old code treated the
// first NXDOMAIN as final and stopped, so the peer went unreachable and
// uploads paused, while a public resolver was sitting there with the correct
// address the whole time.
//
// 8.8.8.8 genuinely NXDOMAINs "localhost" (it is not in public DNS) while the
// system resolver answers it from the hosts file, which gives a real liar and
// a real truth-teller for the same name — no mocking required.
func TestNXDOMAINFromOneResolverDoesNotWin(t *testing.T) {
	if testing.Short() {
		t.Skip("needs network")
	}
	liar := namedResolver{name: "8.8.8.8:53", r: resolverFor("8.8.8.8:53")}
	if _, err := liar.r.LookupHost(context.Background(), "localhost"); err == nil {
		t.Skip("8.8.8.8 resolved localhost — no NXDOMAIN available to test with")
	}

	// Liar first, exactly as the site's own resolver would be consulted.
	swapChain(t, []namedResolver{
		liar,
		{name: "system", r: net.DefaultResolver},
	})

	addrs, by, err := LookupHost(context.Background(), "localhost")
	if err != nil {
		t.Fatalf("NXDOMAIN from one resolver killed the lookup: %v", err)
	}
	if by != "system" {
		t.Fatalf("answered by %q, want system (the honest one)", by)
	}
	if len(addrs) == 0 {
		t.Fatal("no addresses returned")
	}
	t.Logf("ignored NXDOMAIN from %s, resolved via %s -> %v", liar.name, by, addrs)
}

// TestUnanimousNXDOMAINIsReported keeps the useful half of the old behaviour:
// when every resolver agrees a name does not exist, that is the answer, and it
// must still surface as a not-found rather than a vague timeout.
func TestUnanimousNXDOMAINIsReported(t *testing.T) {
	swapChain(t, []namedResolver{
		{name: "system", r: net.DefaultResolver},
	})

	_, _, err := LookupHost(context.Background(),
		"this-name-does-not-exist-anywhere-achyu-test.invalid")
	if err == nil {
		t.Fatal("a nonexistent name resolved")
	}
	t.Logf("correctly failed: %v", err)
}

// TestAllResolversDeadFailsWithinBudget makes sure a total DNS outage ends in
// bounded time instead of hanging a caller.
func TestAllResolversDeadFailsWithinBudget(t *testing.T) {
	swapChain(t, []namedResolver{
		deadNS("192.0.2.1:53"),
		deadNS("192.0.2.2:53"),
	})

	start := time.Now()
	_, _, err := LookupHost(context.Background(), "example.com")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("lookup succeeded with no working nameserver")
	}
	if elapsed > queryTimeout+2*time.Second {
		t.Fatalf("took %v, well past the %v budget", elapsed, queryTimeout)
	}
	t.Logf("failed in %v (budget %v): %v", elapsed, queryTimeout, err)
}
