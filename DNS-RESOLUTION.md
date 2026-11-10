# DNS Resolution — Failures at Filtered Sites & How They Were Fixed

Single-file context for the DNS work in `internal/httpx`. Written after two
live clinic sites lost uploads to DNS faults that looked like everything except
DNS — TLS, certificates, MTU, SNI filtering and the ISP all got blamed first.

Read top-to-bottom; it is ordered so another agent can replicate the reasoning
without re-deriving it.

---

## 0. TL;DR

**Two independent bugs**, both in `internal/httpx.LookupHost`, both fixed by the
same rewrite: ask every nameserver **at once** instead of one after another.

| Bug | Symptom in the field | Cost | Fix |
|-----|----------------------|------|-----|
| Serial fallback | every backend call timed out; uploads fine | **8s per lookup** | query all resolvers in parallel |
| NXDOMAIN believed | peer "unreachable", uploads paused | total outage | NXDOMAIN only wins if **unanimous** |

Measured: **8s → 7.4ms** with two blackholed nameservers in the chain.

**Key gotcha:** the peer kept working throughout both failures, which is what
made this so hard to see. `DialContext` short-circuits on an IP literal —
a peer configured as `http://134.209.157.68:8043` never resolves anything, so
uploads sailed through while every hostname-based call died.

---

## 1. Why this code exists at all

`internal/httpx` gives the sender its own DNS, bypassing whatever nameserver the
site's router hands out over DHCP. That was itself a field fix: a clinic router
was answering DHCP with a dead resolver, and every outbound call stalled at name
resolution until someone physically visited the site.

The chain is:

```go
var publicNameservers = []string{
    "8.8.8.8:53", // Google
    "1.1.1.1:53", // Cloudflare
}
// ...then the system resolver, last.
```

The system resolver stays in the chain deliberately: sites where DNS works fine
but public resolvers are blocked must keep working.

---

## 2. Bug 1 — serial fallback cost 8 seconds per lookup

### What it looked like

Every call to the backend failed, forever:

```
level=ERROR msg="NOTIFIER POST failed" url=https://pacs.achyutrs.com/api/orthanc2/instance-exe-received
  err="... context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
```

while study uploads to the peer completed normally, on the same machine, in the
same second.

### The arithmetic

The affected sites' ISPs blackhole **both** public resolvers. The old code walked
the chain from the top on **every single lookup**:

```
8.8.8.8   → 4s timeout
1.1.1.1   → 4s timeout
system    → answers
            = 8 seconds burned before the request even started
```

The notifier's whole budget is **10 seconds** (`internal/notifier`). That left
two seconds for TCP + TLS + POST + response headers. It never finished.

The log said so plainly once we knew to read it:

```
msg="DNS fallback in use" resolved_by=1.1.1.1:53  err="...dial udp 8.8.8.8:53: i/o timeout"
msg="DNS fallback in use" resolved_by=system      err="...dial udp 1.1.1.1:53: i/o timeout"
```

First line: 4s. Second: 8s.

### Why uploads were unaffected

```go
// Already an IP — nothing to resolve.
if net.ParseIP(host) != nil { ... }
```

The peer at that site had been moved to `http://134.209.157.68:8043` for an
unrelated reason. IP literal → no DNS → no penalty. That asymmetry sent the
investigation into TLS and certificates for a long time.

---

## 3. Bug 2 — one resolver's NXDOMAIN killed the lookup

### What it looked like

```
err="... resolve router.achyutrs.com: lookup router.achyutrs.com: no such host"
level=WARN msg="peer became unreachable — uploads will pause"
```

`no such host` is a **definitive** NXDOMAIN. But the name exists —
`pacs.achyutrs.com` resolved fine via 1.1.1.1 in the same log. The site's ISP
answers NXDOMAIN for names it filters.

### The code that believed it

```go
// A definitive "this name does not exist" is an answer, not a failure
// to answer. Asking another nameserver cannot improve it, and trying
// would just add latency to every typo'd hostname.
if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
    break
}
```

True of an honest resolver. **False of the ones these sites actually have.** A
public resolver was sitting there with the correct address and was never asked.

---

## 4. The fix

`LookupHost` now queries every resolver **concurrently** and takes the first
usable answer. NXDOMAIN is only believed when **all** resolvers agree.

```go
qctx, cancel := context.WithTimeout(ctx, queryTimeout)
defer cancel()                       // cancels the losers

ch := make(chan answer, len(all))
for _, nr := range all {
    go func(nr namedResolver) { ... ch <- answer{...} }(nr)
}

for seen := 0; seen < len(all); seen++ {
    a := <-ch
    if a.err == nil { return a.addrs, a.name, nil }   // first real answer wins
    if !isNotFound(a.err) { notFoundAll = false }
}
// only here, with everyone failed, is NXDOMAIN reported
```

Consequences:

- **A blocked nameserver costs nothing.** The answer arrives when the fastest
  *working* resolver replies; the rest are ignored.
- **A lying resolver cannot win.** Its NXDOMAIN is outvoted by any real answer.
- **A genuinely bogus name still fails fast**, because all resolvers agree
  quickly, and still reports not-found rather than a vague timeout.

### Current constants (`internal/httpx/httpx.go`)

| Constant | Value | Meaning |
|----------|-------|---------|
| `queryTimeout` | **5s** | bounds a whole lookup, not one nameserver |
| `nsDialTimeout` | 3s | opening the socket to a nameserver |
| `connectTimeout` | 10s | one TCP connection to a resolved address |

`queryTimeout` went **up** (4s → 5s) while lookups got faster. It used to be paid
per nameserver and summed; it is now paid once, in parallel, and only reached
when every resolver fails.

---

## 5. Measurements

```
TestBlockedNameserversDoNotDelayTheAnswer
  answered in 7.4248ms with two blackholed nameservers in the chain
```

Serial would have been 8s before the working resolver was even asked.

```
TestNXDOMAINFromOneResolverDoesNotWin
  ignored NXDOMAIN from 8.8.8.8:53, resolved via system -> [::1 127.0.0.1]
```

That test uses a **real** liar, no mocking: `8.8.8.8` genuinely NXDOMAINs
`localhost` (it is not in public DNS) while the system resolver answers it from
the hosts file — a truthful and a lying resolver for the same name.

---

## 6. Tests that pin this

`internal/httpx/dnsparallel_test.go`:

| Test | Guards against |
|------|----------------|
| `TestBlockedNameserversDoNotDelayTheAnswer` | returning to serial fallback |
| `TestNXDOMAINFromOneResolverDoesNotWin` | believing a filtering ISP |
| `TestUnanimousNXDOMAINIsReported` | losing fast failure on genuinely bad names |
| `TestAllResolversDeadFailsWithinBudget` | hanging a caller during a total DNS outage |

`192.0.2.0/24` is TEST-NET-1 (RFC 5737), guaranteed unroutable — a blackholed
nameserver that needs no network to simulate.

`swapChain()` saves and restores the package-global resolver chain. Without it a
test that replaces the chain leaves every later test looking at fake resolvers —
which is exactly how this file first broke `TestSystemResolverIsLastInTheChain`.

---

## 7. A self-inflicted follow-up worth knowing about

The parallel rewrite introduced a **misleading log line**: it reported healthy
lookups as failures, because the winning resolver arrived before any error had
been recorded, and the warning printed `err=<nil>`:

```
level=WARN msg="DNS fallback in use — a nameserver did not answer"
  host=router.achyutrs.com resolved_by=8.8.8.8:53 err=<nil>
```

Harmless to behaviour, actively harmful to diagnosis — it makes a working site
look broken. Fixed so the warning only fires on a real fallback.

Useful side effect while it lasted: `err=<nil>` was a reliable **fingerprint**
for "this machine is running the parallel-DNS build", since the engine's
`version=` never changes between builds. If you need to tell builds apart in the
field, compare the engine's file size — there is no build stamp in the log.

---

## 8. Reading a log after this fix

**Healthy at a filtered site** — this is expected and fine:

```
msg="DNS fallback in use — a nameserver did not answer" resolved_by=1.1.1.1:53
```

It means 8.8.8.8 is still blocked at that site, which is true and outside our
control. The lookup succeeded. No pause, no failure.

**Not fixed** — either of these means something is still wrong:

```
lookup <host>: no such host          ← should now require ALL resolvers to agree
peer became unreachable              ← see also the connectivity watcher hysteresis
```

---

## 9. Related, not the same

Two other faults at the same sites produced near-identical symptoms and are
documented where they live:

- **Windows certificate verification taking 15.3s** (`internal/httpx`,
  `verifyConnection`) — CryptoAPI going to the network mid-handshake on Windows 7.
  Also blew the notifier's 10s budget. Pinned-root verification: 15,268ms → 202ms.
- **The connectivity watcher pausing uploads on a single failed probe**
  (`internal/connectivity`) — uploads saturated the uplink, the probe's own TCP
  connect timed out, and the pipeline halted itself on a ~90s cycle. Now requires
  three consecutive failures.

All three were live at once at different sites, which is why each one looked
like the others.

---

## 10. Still open

The **notifier has a 10s budget and no retries at all**, against 120s and three
retries for uploads (`MaxHTTPRetries: 3`). That asymmetry is why it was the
canary for every fault above: a single dropped request loses that notification
permanently — the study arrives, the portal never hears about it.

Nothing here fixed that. Anything that makes a request exceed 10s at any site
will produce the same class of symptom again.
