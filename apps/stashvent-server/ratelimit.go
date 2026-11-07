package main

import (
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/stashvent/stashvent/internal/api"
)

const (
	// limiterSweepInterval is how often a limiter forgets clients whose
	// bucket has refilled.
	limiterSweepInterval = time.Minute
	// limiterMaxClients caps the clients one limiter remembers, so a flood
	// from many addresses can't grow memory without bound. Beyond it, a client
	// the limiter doesn't know yet is refused until others are forgotten.
	limiterMaxClients = 100_000
)

// rateLimit is a token bucket: perMinute requests sustained, burst at once.
type rateLimit struct {
	perMinute int
	burst     int
}

// rateLimitConfig holds one limit per class of route, and the limit per API
// key on creating shares.
type rateLimitConfig struct {
	create    rateLimit // POST /v1/shares
	lookup    rateLimit // GET /v1/shares/{id}
	other     rateLimit // every other /v1 route
	keyCreate rateLimit // POST /v1/shares per API key, when keys are required
}

// rateLimits holds the limiters, one per class. A client has a separate
// bucket in each, so using one up doesn't block the others.
type rateLimits struct {
	create *ipLimiter
	lookup *ipLimiter
	other  *ipLimiter
	// keyCreate counts creates per API key (its internal ID), on top of the
	// per-address create limit: a key used from many addresses is still one
	// sender.
	keyCreate *limiter[int64]
}

func newRateLimits(cfg rateLimitConfig, trusted []netip.Prefix, now func() time.Time) *rateLimits {
	return &rateLimits{
		create:    newIPLimiter(cfg.create, trusted, now),
		lookup:    newIPLimiter(cfg.lookup, trusted, now),
		other:     newIPLimiter(cfg.other, trusted, now),
		keyCreate: newLimiter[int64](cfg.keyCreate, now),
	}
}

// limiter is an in-memory token bucket per client, K being what tells
// clients apart: an address, or an API key. It is per process: with several
// replicas, each one enforces the limit on its own.
type limiter[K comparable] struct {
	limit rate.Limit
	burst int
	// refill is how long an empty bucket takes to fill up. A client not seen
	// for that long has a full bucket, the same as a new client, so it can be
	// forgotten without giving it anything. Keeping it longer would only
	// make the cap on clients cheaper to hold against everyone else.
	refill     time.Duration
	maxClients int
	now        func() time.Time

	mu        sync.Mutex
	clients   map[K]*limitedClient
	lastSweep time.Time
}

type limitedClient struct {
	bucket *rate.Limiter
	seen   time.Time
}

func newLimiter[K comparable](l rateLimit, now func() time.Time) *limiter[K] {
	limit := rate.Limit(float64(l.perMinute) / 60)
	return &limiter[K]{
		limit:      limit,
		burst:      l.burst,
		refill:     time.Duration(float64(l.burst) / float64(limit) * float64(time.Second)),
		maxClients: limiterMaxClients,
		now:        now,
		clients:    map[K]*limitedClient{},
	}
}

// ipLimiter is a limiter per client address, applied to a whole route.
type ipLimiter struct {
	*limiter[netip.Addr]
	trusted []netip.Prefix
}

func newIPLimiter(l rateLimit, trusted []netip.Prefix, now func() time.Time) *ipLimiter {
	return &ipLimiter{limiter: newLimiter[netip.Addr](l, now), trusted: trusted}
}

// wrap rate-limits next per client: a request over the limit gets 429 with a
// Retry-After header and never reaches next.
func (l *ipLimiter) wrap(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wait, ok := l.allow(limitKey(clientIP(r, l.trusted))); !ok {
			writeRateLimited(w, wait)
			return
		}
		next(w, r)
	})
}

// writeRateLimited answers a request over a limit: 429, and Retry-After.
func writeRateLimited(w http.ResponseWriter, wait time.Duration) {
	setRetryAfter(w, wait)
	writeError(w, http.StatusTooManyRequests, api.CodeRateLimited, "Too many requests")
}

// setRetryAfter sets Retry-After to wait in whole seconds, at least one.
func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
}

// allow takes one token from key's bucket. If there is none, it reports how
// long until there is.
func (l *limiter[K]) allow(key K) (wait time.Duration, ok bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= limiterSweepInterval {
		l.dropIdle(now)
	}

	c := l.clients[key]
	if c == nil {
		if len(l.clients) >= l.maxClients {
			// Room is made at the next sweep.
			return limiterSweepInterval - now.Sub(l.lastSweep), false
		}
		c = &limitedClient{bucket: rate.NewLimiter(l.limit, l.burst)}
		l.clients[key] = c
	}
	c.seen = now

	res := c.bucket.ReserveN(now, 1)
	if wait := res.DelayFrom(now); wait > 0 {
		// Give the token back: a refused request must not push the next
		// allowed one further away.
		res.CancelAt(now)
		return wait, false
	}
	return 0, true
}

// dropIdle forgets clients whose bucket has refilled since they were last
// seen. It runs inside allow, at most once per limiterSweepInterval, so no
// goroutine is needed.
func (l *limiter[K]) dropIdle(now time.Time) {
	for key, c := range l.clients {
		if now.Sub(c.seen) >= l.refill {
			delete(l.clients, key)
		}
	}
	l.lastSweep = now
}

// limitKey is the address a client is counted under. An IPv6 client is
// counted by its /64: one host usually owns the whole /64, and could
// otherwise use a new address for every request.
func limitKey(addr netip.Addr) netip.Addr {
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.Addr()
		}
	}
	return addr
}

// clientIP returns the address a request came from: the connection's remote
// address, unless that is a trusted proxy. Then it is the rightmost
// X-Forwarded-For entry that isn't a trusted proxy too. Entries further left
// are whatever the client chose to send, so they are never used. Requests
// whose address can't be read share the zero address, and with it one bucket.
func clientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	var client netip.Addr
	if remote, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		client = remote.Addr().Unmap()
	}
	if !isTrusted(client, trusted) {
		return client
	}

	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseHop(hops[i])
		if !ok {
			break // keep the last address we could read
		}
		client = hop
		if !isTrusted(client, trusted) {
			break
		}
	}
	return client
}

// parseHop reads one X-Forwarded-For entry: an address, which some proxies
// follow with a port.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.Unmap(), true
	}
	if addrPort, err := netip.ParseAddrPort(s); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	// A prefix never contains an address that carries a zone (fe80::1%eth0).
	addr = addr.WithZone("")
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// parseTrustedProxies reads a comma-separated list of addresses and CIDR
// ranges. An empty list means X-Forwarded-For is never believed.
//
// Request addresses are compared in their plain form: IPv4 even when written
// as IPv4-mapped IPv6 (::ffff:10.0.0.1), and without a zone. Entries are
// brought to the same form here, so none of them is accepted and then never
// matches.
func parseTrustedProxies(list string) ([]netip.Prefix, error) {
	var trusted []netip.Prefix
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("%q is not a CIDR range", entry)
			}
			if addr := prefix.Addr(); addr.Is4In6() {
				// The first 96 bits are the ::ffff: marker, not address.
				if prefix.Bits() < 96 {
					return nil, fmt.Errorf("%q is an IPv4-mapped range wider than IPv4; write it as an IPv4 range", entry)
				}
				prefix = netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96)
			}
			trusted = append(trusted, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address", entry)
		}
		addr = addr.Unmap().WithZone("")
		trusted = append(trusted, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return trusted, nil
}
