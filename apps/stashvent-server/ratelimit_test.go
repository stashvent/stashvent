package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
)

// limitedHandler returns a handler with the given limits and a clock the test
// moves by hand.
func limitedHandler(cfg rateLimitConfig, trusted []netip.Prefix) (*handler, *time.Time) {
	h, _ := newTestHandler()
	now := time.Now()
	h.now = func() time.Time { return now }
	h.limits = newRateLimits(cfg, trusted, h.now)
	return h, &now
}

// lookupFrom sends a lookup of an unknown share from remoteAddr.
func lookupFrom(t *testing.T, mux http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/shares/"+unknownID, nil)
	req.RemoteAddr = remoteAddr
	return serve(mux, req)
}

func TestRateLimit(t *testing.T) {
	cfg := testLimits
	cfg.lookup = rateLimit{perMinute: 6, burst: 3} // one token every 10 seconds
	h, now := limitedHandler(cfg, nil)
	mux := newMux(h)
	const client, other = "203.0.113.7:50000", "203.0.113.8:50000"

	steps := []struct {
		name       string
		advance    time.Duration
		from       string
		wantStatus int
		wantRetry  string
	}{
		{"burst 1", 0, client, http.StatusNotFound, ""},
		{"burst 2", 0, client, http.StatusNotFound, ""},
		{"burst 3", 0, client, http.StatusNotFound, ""},
		{"over the burst", 0, client, http.StatusTooManyRequests, "10"},
		// A refused request doesn't use a token, or the wait would grow.
		{"still refused, same wait", 0, client, http.StatusTooManyRequests, "10"},
		{"another client is unaffected", 0, other, http.StatusNotFound, ""},
		{"the same client from another port", 0, "203.0.113.7:50001", http.StatusTooManyRequests, "10"},
		{"part of the wait is over", 4 * time.Second, client, http.StatusTooManyRequests, "6"},
		{"Retry-After rounds up", 5500 * time.Millisecond, client, http.StatusTooManyRequests, "1"},
		{"one token is back", 500 * time.Millisecond, client, http.StatusNotFound, ""},
		{"and only one", 0, client, http.StatusTooManyRequests, "10"},
		{"a long pause refills the burst, 1", time.Hour, client, http.StatusNotFound, ""},
		{"a long pause refills the burst, 2", 0, client, http.StatusNotFound, ""},
		{"a long pause refills the burst, 3", 0, client, http.StatusNotFound, ""},
		{"but no more than the burst", 0, client, http.StatusTooManyRequests, "10"},
	}
	for _, s := range steps {
		*now = now.Add(s.advance)
		rec := lookupFrom(t, mux, s.from)
		if rec.Code != s.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
		if got := rec.Header().Get("Retry-After"); got != s.wantRetry {
			t.Fatalf("%s: Retry-After = %q, want %q", s.name, got, s.wantRetry)
		}
		if s.wantStatus == http.StatusTooManyRequests {
			if code := errorCode(t, rec); code != api.CodeRateLimited {
				t.Fatalf("%s: code = %q, want %q", s.name, code, api.CodeRateLimited)
			}
		}
	}
}

// Each class of route has its own bucket, and the health endpoints have none.
func TestRateLimitClasses(t *testing.T) {
	one := rateLimit{perMinute: 1, burst: 1}
	h, _ := limitedHandler(rateLimitConfig{create: one, lookup: one, other: one}, nil)
	mux := newMux(h)
	path := "/v1/shares/" + unknownID

	steps := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
	}{
		{"create", http.MethodPost, "/v1/shares", validCreate(), http.StatusCreated},
		{"create again", http.MethodPost, "/v1/shares", validCreate(), http.StatusTooManyRequests},
		// An invalid request is counted too: the limit comes before validation.
		{"invalid create", http.MethodPost, "/v1/shares", "{", http.StatusTooManyRequests},
		{"lookup has its own bucket", http.MethodGet, path, nil, http.StatusNotFound},
		{"lookup again", http.MethodGet, path, nil, http.StatusTooManyRequests},
		{"complete has its own bucket", http.MethodPost, path + "/complete", nil, http.StatusNotFound},
		{"delete shares it with complete", http.MethodDelete, path, nil, http.StatusTooManyRequests},
		{"complete again", http.MethodPost, path + "/complete", nil, http.StatusTooManyRequests},
	}
	for _, s := range steps {
		rec := do(t, mux, s.method, s.path, "token", s.body)
		if rec.Code != s.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		for i := range 50 {
			if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusOK {
				t.Fatalf("%s request %d: status = %d, want 200: health endpoints are not rate-limited", path, i+1, rec.Code)
			}
		}
	}
}

// With a trusted proxy in front, clients are told apart by X-Forwarded-For;
// without one, the header is ignored and can't be used to dodge the limit.
func TestRateLimitBehindProxy(t *testing.T) {
	cfg := testLimits
	cfg.lookup = rateLimit{perMinute: 1, burst: 1}
	const proxy = "10.0.0.1:40000"

	tests := []struct {
		name    string
		trusted string
		// Each request comes from the proxy's address with this header.
		forwarded  []string
		wantStatus []int
	}{
		{"untrusted: one bucket for the address, whatever the header says", "",
			[]string{"198.51.100.1", "198.51.100.2", ""}, []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusTooManyRequests}},
		{"trusted: one bucket per forwarded client", "10.0.0.0/8",
			[]string{"198.51.100.1", "198.51.100.2", "198.51.100.1"}, []int{http.StatusNotFound, http.StatusNotFound, http.StatusTooManyRequests}},
		{"trusted: a made-up first entry doesn't give a new bucket", "10.0.0.0/8",
			[]string{"1.1.1.1, 198.51.100.1", "2.2.2.2, 198.51.100.1"}, []int{http.StatusNotFound, http.StatusTooManyRequests}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trusted, err := parseTrustedProxies(tt.trusted)
			if err != nil {
				t.Fatal(err)
			}
			h, _ := limitedHandler(cfg, trusted)
			mux := newMux(h)
			for i, forwarded := range tt.forwarded {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/shares/"+unknownID, nil)
				req.RemoteAddr = proxy
				if forwarded != "" {
					req.Header.Set("X-Forwarded-For", forwarded)
				}
				if rec := serve(mux, req); rec.Code != tt.wantStatus[i] {
					t.Fatalf("request %d (X-Forwarded-For %q): status = %d, want %d", i+1, forwarded, rec.Code, tt.wantStatus[i])
				}
			}
		})
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name      string
		trusted   string
		remote    string
		forwarded []string // one entry per X-Forwarded-For header line
		want      string
	}{
		{"no proxies trusted: the header is ignored", "", "203.0.113.7:1234", []string{"198.51.100.1"}, "203.0.113.7"},
		{"remote is not a trusted proxy: the header is ignored", "10.0.0.1", "203.0.113.7:1234", []string{"198.51.100.1"}, "203.0.113.7"},
		{"trusted proxy, one hop", "10.0.0.1", "10.0.0.1:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"trusted proxy, no header", "10.0.0.1", "10.0.0.1:1234", nil, "10.0.0.1"},
		{"trusted proxy, empty header", "10.0.0.1", "10.0.0.1:1234", []string{""}, "10.0.0.1"},
		{"the leftmost entry is the client's own claim", "10.0.0.1", "10.0.0.1:1234", []string{"1.1.1.1, 198.51.100.1"}, "198.51.100.1"},
		{"two trusted proxies in a row", "10.0.0.0/8", "10.0.0.1:1234", []string{"1.1.1.1, 198.51.100.1, 10.0.0.2"}, "198.51.100.1"},
		{"an untrusted proxy in the chain ends the walk", "10.0.0.1", "10.0.0.1:1234", []string{"198.51.100.1, 10.0.0.2"}, "10.0.0.2"},
		{"several header lines are one list", "10.0.0.0/8", "10.0.0.1:1234", []string{"1.1.1.1", "198.51.100.1", "10.0.0.2"}, "198.51.100.1"},
		{"only trusted proxies in the header", "10.0.0.0/8", "10.0.0.1:1234", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.3"},
		{"garbage keeps the proxy's address", "10.0.0.1", "10.0.0.1:1234", []string{"not-an-address"}, "10.0.0.1"},
		{"garbage left of a trusted hop keeps that hop", "10.0.0.0/8", "10.0.0.1:1234", []string{"unknown, 10.0.0.2"}, "10.0.0.2"},
		{"an entry with a port", "10.0.0.1", "10.0.0.1:1234", []string{"198.51.100.1:5555"}, "198.51.100.1"},
		{"an IPv6 entry with a port", "10.0.0.1", "10.0.0.1:1234", []string{"[2001:db8::1]:5555"}, "2001:db8::1"},
		{"IPv6 remote", "", "[2001:db8::7]:1234", nil, "2001:db8::7"},
		{"IPv6 trusted proxy", "fd00::/8", "[fd00::1]:1234", []string{"2001:db8::9"}, "2001:db8::9"},
		{"IPv4-mapped remote matches an IPv4 proxy entry", "10.0.0.1", "[::ffff:10.0.0.1]:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"IPv4-mapped entry is read as IPv4", "10.0.0.1", "10.0.0.1:1234", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"proxy range written as IPv4-mapped IPv6", "::ffff:10.0.0.0/104", "10.0.0.1:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"proxy on a link-local address with a zone", "fe80::/10", "[fe80::1%eth0]:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"proxy entry written with a zone", "fe80::1%eth0", "[fe80::1%eth0]:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"unreadable remote address", "", "somewhere", []string{"198.51.100.1"}, "invalid IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trusted, err := parseTrustedProxies(tt.trusted)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			for _, line := range tt.forwarded {
				req.Header.Add("X-Forwarded-For", line)
			}
			if got := clientIP(req, trusted).String(); got != tt.want {
				t.Errorf("clientIP = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestLimitKey(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{"203.0.113.7", "203.0.113.7"},
		{"::ffff:203.0.113.7", "203.0.113.7"},
		// One /64 is one client, however many addresses it uses.
		{"2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::"},
		{"2001:db8:1:2::1", "2001:db8:1:2::"},
		{"2001:db8:1:3::1", "2001:db8:1:3::"},
		{"fe80::1%eth0", "fe80::"},
	}
	for _, tt := range tests {
		if got := limitKey(netip.MustParseAddr(tt.addr)).String(); got != tt.want {
			t.Errorf("limitKey(%s) = %s, want %s", tt.addr, got, tt.want)
		}
	}
	if got := limitKey(netip.Addr{}); got.IsValid() {
		t.Errorf("limitKey of no address = %s, want no address", got)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	tests := []struct {
		list    string
		want    string // fmt of the result
		wantErr bool
	}{
		{"", "[]", false},
		{" , ", "[]", false},
		{"10.0.0.1", "[10.0.0.1/32]", false},
		{"10.0.0.1, 172.16.5.9/12 ,fd00::1", "[10.0.0.1/32 172.16.0.0/12 fd00::1/128]", false},
		{"::ffff:10.0.0.1", "[10.0.0.1/32]", false},
		// Stored as the IPv4 range it stands for, or it would never match.
		{"::ffff:10.0.0.0/104", "[10.0.0.0/8]", false},
		{"::ffff:10.1.2.3/128", "[10.1.2.3/32]", false},
		{"::ffff:0.0.0.0/64", "", true},
		{"fe80::1%eth0", "[fe80::1/128]", false},
		{"10.0.0.1,proxy.internal", "", true},
		{"10.0.0.0/33", "", true},
		{"10.0.0.1:8080", "", true},
	}
	for _, tt := range tests {
		got, err := parseTrustedProxies(tt.list)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseTrustedProxies(%q): err = %v, wantErr %v", tt.list, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && fmt.Sprint(got) != tt.want {
			t.Errorf("parseTrustedProxies(%q) = %v, want %s", tt.list, got, tt.want)
		}
	}
}

// A client whose bucket has refilled is no different from a new one, so it is
// forgotten at the next sweep. The map doesn't grow for as long as the server
// runs, and a slot under the cap can't be held by one request every now and then.
func TestLimiterForgetsRefilledClients(t *testing.T) {
	now := time.Now()
	// 3 tokens at one per 10 seconds: an empty bucket is full after 30 seconds.
	l := newLimiter[netip.Addr](rateLimit{perMinute: 6, burst: 3}, func() time.Time { return now })
	addr := func(i int) netip.Addr { return netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", i)) }

	for i := range 10 {
		l.allow(addr(i))
	}
	now = now.Add(limiterSweepInterval - 10*time.Second)
	l.allow(addr(0)) // 10 seconds before the sweep: its bucket won't be full by then
	if got := len(l.clients); got != 10 {
		t.Fatalf("%d clients before the sweep, want 10", got)
	}

	now = now.Add(10 * time.Second)
	l.allow(addr(1)) // runs the sweep, and is a new client again after it
	if got := len(l.clients); got != 2 {
		t.Errorf("%d clients after the sweep, want 2: the one seen 10 seconds ago and the one asking now", got)
	}
	if l.clients[addr(0)] == nil {
		t.Error("a client was forgotten before its bucket had refilled")
	}
}

// A bucket that refills slowly is kept until it is full again. Forgetting it
// sooner would hand the client a full burst early.
func TestLimiterKeepsBucketsUntilRefilled(t *testing.T) {
	now := time.Now()
	// 30 tokens at one per minute: 30 minutes to refill.
	l := newLimiter[netip.Addr](rateLimit{perMinute: 1, burst: 30}, func() time.Time { return now })
	client := netip.MustParseAddr("198.51.100.1")
	other := netip.MustParseAddr("198.51.100.2")

	for range 30 {
		if _, ok := l.allow(client); !ok {
			t.Fatal("refused within the burst")
		}
	}
	now = now.Add(15 * time.Minute) // 15 tokens are back
	l.allow(other)                  // runs the sweep

	allowed := 0
	for range 30 {
		if _, ok := l.allow(client); ok {
			allowed++
		}
	}
	if allowed != 15 {
		t.Errorf("%d requests allowed after 15 minutes, want the 15 that were refilled", allowed)
	}
}

// At the cap, clients the limiter already knows keep working and new ones are
// refused until the next sweep, which forgets every client whose bucket has
// refilled (after 5 seconds here).
func TestLimiterCap(t *testing.T) {
	now := time.Now()
	l := newLimiter[netip.Addr](rateLimit{perMinute: 60, burst: 5}, func() time.Time { return now })
	l.maxClients = 3
	addr := func(i int) netip.Addr { return netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", i)) }

	for i := range 3 {
		if _, ok := l.allow(addr(i)); !ok {
			t.Fatalf("client %d refused below the cap", i)
		}
	}
	wait, ok := l.allow(addr(3))
	if ok || wait != limiterSweepInterval {
		t.Errorf("new client at the cap: allowed = %v, wait = %v; want refused for %v", ok, wait, limiterSweepInterval)
	}
	if _, ok := l.allow(addr(0)); !ok {
		t.Error("a known client was refused at the cap")
	}

	// The wait is the time left until the sweep.
	now = now.Add(20 * time.Second)
	if wait, ok := l.allow(addr(3)); ok || wait != limiterSweepInterval-20*time.Second {
		t.Errorf("20 seconds later: allowed = %v, wait = %v; want refused for %v", ok, wait, limiterSweepInterval-20*time.Second)
	}

	now = now.Add(40 * time.Second)
	if _, ok := l.allow(addr(3)); !ok {
		t.Error("new client refused after the sweep")
	}
}

// The limiter is shared by every request goroutine.
func TestLimiterConcurrentUse(t *testing.T) {
	l := newLimiter[netip.Addr](rateLimit{perMinute: 60, burst: 100}, time.Now)
	client := netip.MustParseAddr("198.51.100.1")
	results := make(chan bool)
	const goroutines, each = 8, 50
	for range goroutines {
		go func() {
			for range each {
				_, ok := l.allow(client)
				results <- ok
			}
		}()
	}
	allowed := 0
	for range goroutines * each {
		if <-results {
			allowed++
		}
	}
	// 100 from the burst, plus the few that refill while the test runs.
	if allowed < 100 || allowed > 110 {
		t.Errorf("%d of %d requests allowed, want about 100: "+strconv.Itoa(goroutines)+" goroutines share one bucket", allowed, goroutines*each)
	}
}
