package bridge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Under mobile whitelists ("белые списки") the operator lets through only
// approved sites. A phone with Private DNS (dns.google, 1.1.1.1) then loses
// DNS altogether, although the subscription host and the servers themselves
// are reachable. The app's own sockets bypass the VPN, so these helpers talk
// to the phone's network directly and fall back to Yandex DNS, which such
// lists always allow, when the system resolver gives no answer.

var fallbackDNSServer = "77.88.8.8:53"

var fallbackResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 3 * time.Second}
		return dialer.DialContext(ctx, "udp", fallbackDNSServer)
	},
}

// systemLookup is replaced in tests.
var systemLookup = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// lookupIPWithFallback resolves host with the system resolver and, when it
// fails, with Yandex DNS.
func lookupIPWithFallback(ctx context.Context, host string) ([]net.IPAddr, error) {
	addresses, err := systemLookup(ctx, host)
	if err == nil && len(addresses) > 0 {
		return addresses, nil
	}
	fallbackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if fallback, fallbackErr := fallbackResolver.LookupIPAddr(fallbackCtx, host); fallbackErr == nil && len(fallback) > 0 {
		return fallback, nil
	}
	if err == nil {
		err = errors.New("DNS не вернул адрес")
	}
	return nil, err
}

// dialDirect connects over the phone's own network, resolving with the
// fallback above and trying IPv4 addresses first.
func dialDirect(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 8 * time.Second}
	if net.ParseIP(host) != nil {
		return dialer.DialContext(ctx, network, address)
	}
	addresses, err := lookupIPWithFallback(ctx, host)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(addresses, func(a, b int) bool {
		return addresses[a].IP.To4() != nil && addresses[b].IP.To4() == nil
	})
	var lastErr error
	for _, ip := range addresses {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// directTLSConfig is nil (the system roots) outside tests.
var directTLSConfig *tls.Config

func directTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig:     directTLSConfig,
		Proxy:               nil,
		DialContext:         dialDirect,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	}
}

// Sites that whitelists keep open, and sites they do not. If only the first
// answer, the network is very likely on a whitelist.
var (
	whitelistAllowedSites = []string{"https://vk.com/", "https://ya.ru/", "https://max.ru/"}
	whitelistControlSites = []string{"https://www.google.com/generate_204", "https://cp.cloudflare.com/generate_204", "https://github.com/"}
)

type whitelistResult struct {
	Whitelist bool `json:"whitelist"`
	Allowed   int  `json:"allowed"`
	Control   int  `json:"control"`
	// Offline: nothing answered at all, so there is no telling.
	Offline bool `json:"offline"`
}

// CheckWhitelist looks for a mobile whitelist: approved Russian sites
// answer while ordinary ones do not. Requests go straight to the phone's
// network, even while the VPN is connected.
func CheckWhitelist() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result := probeWhitelist(ctx)
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

func probeWhitelist(ctx context.Context) whitelistResult {
	transport := directTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   6 * time.Second,
		// Any answer proves the site is reachable; a redirect is one.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	reachable := func(sites []string) int {
		var count int
		var mu sync.Mutex
		var wait sync.WaitGroup
		for _, site := range sites {
			wait.Add(1)
			go func(site string) {
				defer wait.Done()
				request, err := http.NewRequestWithContext(ctx, http.MethodHead, site, nil)
				if err != nil {
					return
				}
				request.Header.Set("User-Agent", appUserAgent)
				response, err := client.Do(request)
				if err != nil {
					return
				}
				response.Body.Close()
				mu.Lock()
				count++
				mu.Unlock()
			}(site)
		}
		wait.Wait()
		return count
	}
	var result whitelistResult
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); result.Allowed = reachable(whitelistAllowedSites) }()
	go func() { defer wait.Done(); result.Control = reachable(whitelistControlSites) }()
	wait.Wait()
	result.Whitelist = result.Allowed > 0 && result.Control == 0
	result.Offline = result.Allowed == 0 && result.Control == 0
	return result
}

// directTCPLatency knocks on the server's port over the phone's network.
func directTCPLatency(ctx context.Context, profile Profile) (int64, error) {
	resolved, _, err := resolveProfileEndpoint(ctx, profile)
	if err != nil {
		return 0, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	started := time.Now()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(resolved.address, strconv.Itoa(resolved.port)))
	if err != nil {
		return 0, err
	}
	latency := time.Since(started).Milliseconds()
	_ = conn.Close()
	if latency < 1 {
		latency = 1
	}
	return latency, nil
}

func init() {
	// Servers are resolved the same way, so connecting and checking them
	// also survive a dead system resolver.
	lookupEndpointIPs = lookupIPWithFallback
}
