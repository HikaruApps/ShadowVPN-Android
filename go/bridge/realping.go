package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/xtls/xray-core/core"
)

// A TCP ping only shows that the server's port is open. A server whose key
// expired, whose config is broken or that answers with a decoy still
// "pings". The real check builds the same outbound a connection would use
// (dialer chains and fragmentation included), sends HTTPS requests through
// it and counts the server as working only when an answer comes back.

const (
	realPingWorkers = 12
	realPingTimeout = 8 * time.Second
)

// realPingURLs are tried in order: one blocked endpoint must not make a
// working server look dead.
var realPingURLs = []string{"https://www.gstatic.com/generate_204", "https://cp.cloudflare.com/generate_204"}

func realPingProfile(parent context.Context, profile Profile, fragmentation bool) (int64, error) {
	if profile.Protocol == "naive" {
		// Cronet runs only for the selected server; check the endpoint instead.
		latency, _, err := tcpPingEndpoint(parent, profile.address, profile.port)
		return latency, err
	}
	ctx, cancel := context.WithTimeout(parent, realPingTimeout)
	defer cancel()
	resolved, _, err := resolveProfileEndpoint(ctx, profile)
	if err != nil {
		return 0, err
	}
	outbound, err := configuredOutbound(resolved, "proxy", fragmentation && len(resolved.chain) == 0)
	if err != nil {
		return 0, err
	}
	outbounds := []any{outbound}
	for _, hop := range resolved.chain {
		clone, err := cloneObject(hop)
		if err != nil {
			return 0, err
		}
		outbounds = append(outbounds, clone)
	}
	config, err := json.Marshal(map[string]any{"log": map[string]any{"loglevel": "none"}, "outbounds": outbounds})
	if err != nil {
		return 0, err
	}
	parsed, err := core.LoadConfig("json", bytes.NewReader(config))
	if err != nil {
		return 0, err
	}
	chained := len(resolved.chain) > 0
	if chained {
		// The chain resolves its hops through the process-wide outbound
		// manager, which must be this instance for the whole probe.
		xrayDialerMu.Lock()
		defer xrayDialerMu.Unlock()
	} else {
		xrayDialerMu.RLock()
	}
	instance, err := newXrayInstance(parsed)
	if !chained {
		// Nothing here dials through the manager; hand it straight back.
		restoreActiveDialer()
		xrayDialerMu.RUnlock()
	}
	if err != nil {
		return 0, err
	}
	defer instance.Close()
	if chained {
		defer restoreActiveDialer()
	}
	if err := instance.Start(); err != nil {
		return 0, err
	}
	return measureThroughInstance(ctx, instance)
}

// measureThroughInstance proves that traffic passes and returns the latency
// of a request over the already established connection, which is what
// browsing feels like; the first request also pays for the handshakes.
func measureThroughInstance(ctx context.Context, instance *core.Instance) (int64, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dialThroughInstance(ctx, instance, address)
		},
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     realPingTimeout,
		TLSHandshakeTimeout: realPingTimeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request := func(ctx context.Context, address string) (time.Duration, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, address, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("User-Agent", "shadowvpn-probe/1")
		started := time.Now()
		response, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return 0, fmt.Errorf("HTTP %d", response.StatusCode)
		}
		return time.Since(started), nil
	}
	var lastErr error
	for index, address := range realPingURLs {
		// A dead endpoint may hang until the overall timeout; leave the
		// others time to answer.
		attemptCtx, cancel := ctx, context.CancelFunc(func() {})
		if index < len(realPingURLs)-1 {
			attemptCtx, cancel = context.WithTimeout(ctx, realPingTimeout/2)
		}
		first, err := request(attemptCtx, address)
		if err != nil {
			cancel()
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		latency := first
		if second, err := request(attemptCtx, address); err == nil {
			latency = second
		}
		cancel()
		if latency < time.Millisecond {
			latency = time.Millisecond
		}
		return latency.Milliseconds(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("сервер не ответил")
	}
	return 0, lastErr
}

func realPingProfiles(parent context.Context, profiles []Profile, fragmentation bool) []PingResult {
	results := make([]PingResult, len(profiles))
	jobs := make(chan int)
	var group sync.WaitGroup
	for range min(realPingWorkers, len(profiles)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				latency, err := realPingProfile(parent, profiles[index], fragmentation)
				results[index] = PingResult{ID: profiles[index].ID, LatencyMS: latency, Available: err == nil}
			}
		}()
	}
	for index := range profiles {
		select {
		case jobs <- index:
		case <-parent.Done():
			results[index] = PingResult{ID: profiles[index].ID}
		}
	}
	close(jobs)
	group.Wait()
	return results
}

// realPingFinalists bounds how many servers Auto checks for real before it
// connects, so connecting stays fast with large subscriptions.
const realPingFinalists = 6

// verifiedPingProfiles is what Auto and balancers choose from: a quick TCP
// ping of every server, then a real check of the fastest ones. Servers that
// fail it are reported unavailable, so a server that only "pings" is never
// picked.
func verifiedPingProfiles(ctx context.Context, profiles []Profile, fragmentation bool) []PingResult {
	results := pingProfiles(ctx, profiles)
	order := make([]int, 0, len(results))
	for index, result := range results {
		if result.Available {
			order = append(order, index)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return results[order[a]].LatencyMS < results[order[b]].LatencyMS })
	if len(order) > realPingFinalists {
		order = order[:realPingFinalists]
	}
	finalists := make([]Profile, len(order))
	for i, index := range order {
		finalists[i] = profiles[index]
	}
	checked := realPingProfiles(ctx, finalists, fragmentation)
	verified := make([]PingResult, len(results))
	for index, result := range results {
		verified[index] = PingResult{ID: result.ID, ResolvedAddress: result.ResolvedAddress}
	}
	for i, index := range order {
		verified[index].Available = checked[i].Available
		verified[index].LatencyMS = checked[i].LatencyMS
	}
	return verified
}

// withAutoPingResults adds the Auto entries, as fast as their fastest
// working server, in front of the per-server results.
func withAutoPingResults(profiles []Profile, results []PingResult) []PingResult {
	auto := PingResult{ID: autoProfileID}
	if _, best, ok := fastestProfile(profilesWithoutProtocol(profiles, "naive"), results); ok {
		auto.Available, auto.LatencyMS = true, best.LatencyMS
	}
	autoNoRU := PingResult{ID: autoNoRUProfileID}
	if _, best, ok := fastestProfile(profilesWithoutProtocol(withoutRussianProfiles(profiles), "naive"), results); ok {
		autoNoRU.Available, autoNoRU.LatencyMS = true, best.LatencyMS
	}
	return append([]PingResult{auto, autoNoRU}, results...)
}
